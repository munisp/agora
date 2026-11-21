//! Durable sim ledger (2026-11-21). LEDGER_IMPL=sim keeps balances in memory,
//! so every payments restart wiped revenue while payout_attempts (Postgres)
//! survived. When a DSN is configured, this wrapper snapshots the whole sim
//! state to `sim_ledger_state` after every successful write and loads it at
//! boot. Writes are serialised (one gate) so snapshots are saved in order.
//! Dev/demo posture only — the real ledger is TigerBeetle.
use async_trait::async_trait;
use sqlx::PgPool;
use tokio::sync::Mutex;
use tracing::{error, info};
use uuid::Uuid;

use super::sim::SimLedgerClient;
use super::*;

const DDL: &str = "CREATE TABLE IF NOT EXISTS sim_ledger_state (
    id         INT PRIMARY KEY CHECK (id = 1),
    state      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)";

pub struct DurableSimLedger {
    inner: SimLedgerClient,
    pool: PgPool,
    gate: Mutex<()>,
}

impl DurableSimLedger {
    pub async fn open(inner: SimLedgerClient, pool: PgPool) -> Result<Self, String> {
        sqlx::query(DDL)
            .execute(&pool)
            .await
            .map_err(|e| format!("sim_ledger_state bootstrap failed: {e}"))?;
        let row: Option<(String,)> = sqlx::query_as("SELECT state FROM sim_ledger_state WHERE id = 1")
            .fetch_optional(&pool)
            .await
            .map_err(|e| format!("sim_ledger_state read failed: {e}"))?;
        match row {
            Some((text,)) => {
                let (accounts, transfers) = inner.import_snapshot(&text).await?;
                info!(accounts, transfers, "durable sim ledger: state restored from Postgres");
            }
            None => info!("durable sim ledger: no saved state yet (fresh ledger)"),
        }
        Ok(Self { inner, pool, gate: Mutex::new(()) })
    }

    async fn save(&self) {
        let text = match self.inner.export_snapshot().await {
            Ok(t) => t,
            Err(e) => return error!(error = %e, "durable sim ledger: snapshot encode failed"),
        };
        if let Err(e) = sqlx::query(
            "INSERT INTO sim_ledger_state (id, state, updated_at) VALUES (1, $1, now())
             ON CONFLICT (id) DO UPDATE SET state = EXCLUDED.state, updated_at = now()",
        )
        .bind(text)
        .execute(&self.pool)
        .await
        {
            error!(error = %e, "durable sim ledger: snapshot save failed (state kept in memory)");
        }
    }
}

macro_rules! write_op {
    ($self:ident, $call:expr) => {{
        let _g = $self.gate.lock().await;
        let out = $call.await;
        if out.is_ok() {
            $self.save().await;
        }
        out
    }};
}

#[async_trait]
impl LedgerClient for DurableSimLedger {
    async fn create_accounts(&self, tenant_id: &str) -> Result<Vec<Account>, LedgerError> {
        write_op!(self, self.inner.create_accounts(tenant_id))
    }
    async fn hold_deposit(&self, tenant_id: &str, transfer_id: Uuid, amount: u64) -> Result<Transfer, LedgerError> {
        write_op!(self, self.inner.hold_deposit(tenant_id, transfer_id, amount))
    }
    async fn capture(&self, tenant_id: &str, hold_id: Uuid, transfer_id: Uuid, amount: Option<u64>) -> Result<CaptureResult, LedgerError> {
        write_op!(self, self.inner.capture(tenant_id, hold_id, transfer_id, amount))
    }
    async fn refund(&self, tenant_id: &str, transfer_id: Uuid, hold_id: Option<Uuid>, amount: u64) -> Result<Transfer, LedgerError> {
        write_op!(self, self.inner.refund(tenant_id, transfer_id, hold_id, amount))
    }
    async fn no_show_fee(&self, tenant_id: &str, hold_id: Uuid, transfer_id: Uuid, amount: u64) -> Result<CaptureResult, LedgerError> {
        write_op!(self, self.inner.no_show_fee(tenant_id, hold_id, transfer_id, amount))
    }
    async fn payout(&self, tenant_id: &str, transfer_id: Uuid, amount: u64) -> Result<Transfer, LedgerError> {
        write_op!(self, self.inner.payout(tenant_id, transfer_id, amount))
    }
    async fn payout_hold(&self, tenant_id: &str, transfer_id: Uuid, amount: u64) -> Result<Transfer, LedgerError> {
        write_op!(self, self.inner.payout_hold(tenant_id, transfer_id, amount))
    }
    async fn payout_post(&self, tenant_id: &str, hold_id: Uuid, transfer_id: Uuid) -> Result<Transfer, LedgerError> {
        write_op!(self, self.inner.payout_post(tenant_id, hold_id, transfer_id))
    }
    async fn payout_void(&self, tenant_id: &str, hold_id: Uuid, transfer_id: Uuid) -> Result<Transfer, LedgerError> {
        write_op!(self, self.inner.payout_void(tenant_id, hold_id, transfer_id))
    }
    async fn get_transfer(&self, transfer_id: Uuid) -> Result<Transfer, LedgerError> {
        self.inner.get_transfer(transfer_id).await
    }
    async fn balance(&self, tenant_id: &str) -> Result<TenantBalance, LedgerError> {
        self.inner.balance(tenant_id).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // The snapshot round-trips every account and transfer (u128 ids intact).
    #[tokio::test]
    async fn snapshot_round_trip_keeps_balances() {
        let a = SimLedgerClient::new(250);
        a.create_accounts("t1").await.unwrap();
        let hold = Uuid::new_v4();
        a.hold_deposit("t1", hold, 10_000).await.unwrap();
        a.capture("t1", hold, Uuid::new_v4(), None).await.unwrap();
        let before = serde_json::to_string(&a.balance("t1").await.unwrap()).unwrap();
        let text = a.export_snapshot().await.unwrap();
        let b = SimLedgerClient::new(250);
        b.import_snapshot(&text).await.unwrap();
        let after = serde_json::to_string(&b.balance("t1").await.unwrap()).unwrap();
        assert_eq!(before, after);
        assert!(b.get_transfer(hold).await.is_ok());
    }
}
