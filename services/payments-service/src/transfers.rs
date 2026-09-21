//! Durable rail-attempt records for `/v1/transfers` (SPEC-W45, lending
//! disbursement bridge) and the K12 Flutterwave refund rail.
//!
//! Mirrors the `payout_attempts` posture (payouts.rs, SPEC-W43 P-01/C3): the
//! money path is LEDGER-FIRST and every rail outcome is recorded durably so a
//! replay of the same idempotency key returns the ORIGINAL outcome instead of
//! re-executing the rail. Two store implementations behind
//! [`TransferAttemptStore`]:
//! - [`PgTransferAttemptStore`] — Postgres (production; bootstrap DDL at boot,
//!   fail-closed when configured but unreachable);
//! - [`MemTransferAttemptStore`] — in-memory dev fallback when no DSN is
//!   configured (records are lost on restart; main.rs logs a loud warning).
//!
//! One table serves both kinds (`kind` column): `transfer` rows are written by
//! the lending disbursement bridge, `refund` rows by the K12 refund rail. The
//! payout reconciler does NOT sweep this table: `unknown` is never written
//! here — an ambiguous rail outcome is recorded as `queued_manual` with an
//! explicit verify-before-manual-execution detail, and explicit rejections as
//! `failed`.

use std::collections::BTreeMap;

use async_trait::async_trait;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

/// Lifecycle of a rail attempt row (transfers + refunds).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TransferAttemptState {
    /// The rail could not execute (unconfigured, no provider reference, or an
    /// ambiguous outcome): NO provider money movement is claimed; the funds
    /// stay reserved in the ledger and the operation awaits manual
    /// execution/reconciliation. Honest fallback (SPEC-W45 K12 + /v1/transfers).
    QueuedManual,
    /// The rail explicitly COMMITTED; the ledger side was posted.
    Committed,
    /// The rail explicitly rejected the operation; the ledger hold was voided.
    Failed,
}

impl TransferAttemptState {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::QueuedManual => "queued_manual",
            Self::Committed => "committed",
            Self::Failed => "failed",
        }
    }

    pub fn from_str(s: &str) -> Option<Self> {
        match s {
            "queued_manual" => Some(Self::QueuedManual),
            "committed" => Some(Self::Committed),
            "failed" => Some(Self::Failed),
            _ => None,
        }
    }
}

/// Attempt kinds sharing the table.
pub const KIND_TRANSFER: &str = "transfer";
pub const KIND_REFUND: &str = "refund";

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TransferAttempt {
    /// Deterministic id (uuid string) derived from the idempotency key; also
    /// the ledger hold id and the rail transfer id.
    pub transfer_id: String,
    /// `transfer` (lending disbursement) or `refund` (K12 provider refund).
    pub kind: String,
    pub tenant_id: String,
    pub amount_cents: u64,
    pub currency: String,
    /// Rail destination / provider reference (payee JSON or provider tx id).
    pub destination: String,
    pub state: TransferAttemptState,
    pub detail: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[async_trait]
pub trait TransferAttemptStore: Send + Sync {
    /// Insert a new attempt row (idempotent: first record for an id wins;
    /// replays do not rewrite history).
    async fn record(&self, attempt: &TransferAttempt) -> Result<(), String>;
    async fn get(&self, transfer_id: &str) -> Result<Option<TransferAttempt>, String>;
    async fn mark(
        &self,
        transfer_id: &str,
        state: TransferAttemptState,
        detail: Option<&str>,
    ) -> Result<(), String>;
}

// ---------------------------------------------------------------------------
// Postgres implementation (production)
// ---------------------------------------------------------------------------

/// Bootstrap DDL. Run idempotently at boot when a DSN is configured (same
/// runtime-DDL idiom as payouts.rs `payout_attempts`).
pub const BOOTSTRAP_DDL: &str = r#"
CREATE TABLE IF NOT EXISTS rail_attempts (
    transfer_id  TEXT PRIMARY KEY,
    kind         TEXT NOT NULL CHECK (kind IN ('transfer','refund')),
    tenant_id    TEXT NOT NULL,
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    currency     TEXT NOT NULL,
    destination  TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL CHECK (state IN ('queued_manual','committed','failed')),
    detail       TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS rail_attempts_state_idx ON rail_attempts (state);
"#;

pub struct PgTransferAttemptStore {
    pool: sqlx::PgPool,
}

impl PgTransferAttemptStore {
    /// Connect (with bounded retry) and bootstrap the table. Fail-closed:
    /// when a DSN is configured but unreachable/invalid, the service refuses
    /// to boot rather than silently losing replay durability.
    pub async fn connect_with_retry(dsn: &str) -> Result<Self, String> {
        let options = crate::payouts::pg_connect_options(dsn)?;
        let mut last_err = String::new();
        for attempt in 1..=10u32 {
            match sqlx::postgres::PgPoolOptions::new()
                .max_connections(2)
                .connect_with(options.clone())
                .await
            {
                Ok(pool) => {
                    for stmt in BOOTSTRAP_DDL.split(';').map(str::trim).filter(|s| !s.is_empty()) {
                        sqlx::query(stmt)
                            .execute(&pool)
                            .await
                            .map_err(|e| format!("rail_attempts bootstrap failed: {e}"))?;
                    }
                    tracing::info!("rail_attempts store: postgres (bootstrapped)");
                    return Ok(Self { pool });
                }
                Err(e) => {
                    last_err = e.to_string();
                    let backoff = std::time::Duration::from_millis(200 * attempt as u64);
                    tracing::warn!(attempt, error = %e, "rail_attempts postgres connect failed; retrying");
                    tokio::time::sleep(backoff).await;
                }
            }
        }
        Err(format!(
            "rail_attempts postgres unavailable after 10 attempts: {last_err}"
        ))
    }

    fn row_to_attempt(row: &sqlx::postgres::PgRow) -> Result<TransferAttempt, String> {
        use sqlx::Row;
        let amount: i64 = row
            .try_get("amount_cents")
            .map_err(|e| format!("amount_cents decode: {e}"))?;
        let state: String = row
            .try_get("state")
            .map_err(|e| format!("state decode: {e}"))?;
        Ok(TransferAttempt {
            transfer_id: row.try_get("transfer_id").map_err(|e| e.to_string())?,
            kind: row.try_get("kind").map_err(|e| e.to_string())?,
            tenant_id: row.try_get("tenant_id").map_err(|e| e.to_string())?,
            amount_cents: u64::try_from(amount).map_err(|_| "negative amount_cents".to_string())?,
            currency: row.try_get("currency").map_err(|e| e.to_string())?,
            destination: row.try_get("destination").map_err(|e| e.to_string())?,
            state: TransferAttemptState::from_str(&state)
                .ok_or_else(|| format!("unknown rail attempt state '{state}'"))?,
            detail: row.try_get("detail").map_err(|e| e.to_string())?,
            created_at: row.try_get("created_at").map_err(|e| e.to_string())?,
            updated_at: row.try_get("updated_at").map_err(|e| e.to_string())?,
        })
    }
}

#[async_trait]
impl TransferAttemptStore for PgTransferAttemptStore {
    async fn record(&self, attempt: &TransferAttempt) -> Result<(), String> {
        sqlx::query(
            "INSERT INTO rail_attempts
                (transfer_id, kind, tenant_id, amount_cents, currency, destination, state, detail)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
             ON CONFLICT (transfer_id) DO NOTHING",
        )
        .bind(&attempt.transfer_id)
        .bind(&attempt.kind)
        .bind(&attempt.tenant_id)
        .bind(i64::try_from(attempt.amount_cents).map_err(|_| "amount overflow".to_string())?)
        .bind(&attempt.currency)
        .bind(&attempt.destination)
        .bind(attempt.state.as_str())
        .bind(&attempt.detail)
        .execute(&self.pool)
        .await
        .map_err(|e| format!("rail_attempts insert failed: {e}"))?;
        Ok(())
    }

    async fn get(&self, transfer_id: &str) -> Result<Option<TransferAttempt>, String> {
        let row = sqlx::query(
            "SELECT transfer_id, kind, tenant_id, amount_cents, currency, destination,
                    state, detail, created_at, updated_at
             FROM rail_attempts WHERE transfer_id = $1",
        )
        .bind(transfer_id)
        .fetch_optional(&self.pool)
        .await
        .map_err(|e| format!("rail_attempts read failed: {e}"))?;
        row.as_ref().map(Self::row_to_attempt).transpose()
    }

    async fn mark(
        &self,
        transfer_id: &str,
        state: TransferAttemptState,
        detail: Option<&str>,
    ) -> Result<(), String> {
        sqlx::query(
            "UPDATE rail_attempts SET state = $2, detail = COALESCE($3, detail),
                    updated_at = now()
             WHERE transfer_id = $1",
        )
        .bind(transfer_id)
        .bind(state.as_str())
        .bind(detail)
        .execute(&self.pool)
        .await
        .map_err(|e| format!("rail_attempts update failed: {e}"))?;
        Ok(())
    }
}

// ---------------------------------------------------------------------------
// In-memory implementation (dev fallback when no DSN is configured)
// ---------------------------------------------------------------------------

#[derive(Default)]
pub struct MemTransferAttemptStore {
    rows: tokio::sync::Mutex<BTreeMap<String, TransferAttempt>>,
}

#[async_trait]
impl TransferAttemptStore for MemTransferAttemptStore {
    async fn record(&self, attempt: &TransferAttempt) -> Result<(), String> {
        let mut rows = self.rows.lock().await;
        rows.entry(attempt.transfer_id.clone())
            .or_insert_with(|| attempt.clone());
        Ok(())
    }

    async fn get(&self, transfer_id: &str) -> Result<Option<TransferAttempt>, String> {
        Ok(self.rows.lock().await.get(transfer_id).cloned())
    }

    async fn mark(
        &self,
        transfer_id: &str,
        state: TransferAttemptState,
        detail: Option<&str>,
    ) -> Result<(), String> {
        let mut rows = self.rows.lock().await;
        if let Some(a) = rows.get_mut(transfer_id) {
            a.state = state;
            if let Some(d) = detail {
                a.detail = Some(d.to_string());
            }
            a.updated_at = Utc::now();
        }
        Ok(())
    }
}

// ---------------------------------------------------------------------------
// Tests: mem store semantics (first record wins; mark updates).
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;

    fn attempt(id: &str, state: TransferAttemptState) -> TransferAttempt {
        TransferAttempt {
            transfer_id: id.to_string(),
            kind: KIND_TRANSFER.to_string(),
            tenant_id: "t-1".to_string(),
            amount_cents: 5_000,
            currency: "NGN".to_string(),
            destination: "loan:l-1".to_string(),
            state,
            detail: None,
            created_at: Utc::now(),
            updated_at: Utc::now(),
        }
    }

    #[tokio::test]
    async fn mem_store_first_record_wins_and_mark_updates() {
        let store = MemTransferAttemptStore::default();
        let id = uuid::Uuid::new_v4().to_string();
        store
            .record(&attempt(&id, TransferAttemptState::QueuedManual))
            .await
            .unwrap();
        // Replay: the first record wins.
        store
            .record(&attempt(&id, TransferAttemptState::Committed))
            .await
            .unwrap();
        let got = store.get(&id).await.unwrap().unwrap();
        assert_eq!(got.state, TransferAttemptState::QueuedManual);
        store
            .mark(&id, TransferAttemptState::Committed, Some("rail committed"))
            .await
            .unwrap();
        let got = store.get(&id).await.unwrap().unwrap();
        assert_eq!(got.state, TransferAttemptState::Committed);
        assert_eq!(got.detail.as_deref(), Some("rail committed"));
    }

    /// Pg store against a REAL database — gated on PAYMENTS_TEST_DATABASE_URL
    /// (no PG in CI); run via `... cargo test pg_transfer -- --ignored`.
    #[tokio::test]
    #[ignore = "requires PAYMENTS_TEST_DATABASE_URL pointing at a real Postgres"]
    async fn pg_transfer_attempt_store_roundtrip() {
        let dsn = std::env::var("PAYMENTS_TEST_DATABASE_URL")
            .expect("PAYMENTS_TEST_DATABASE_URL must point at a real Postgres");
        let store = PgTransferAttemptStore::connect_with_retry(&dsn).await.unwrap();
        // Bootstrap is idempotent.
        let store2 = PgTransferAttemptStore::connect_with_retry(&dsn).await.unwrap();
        drop(store2);
        let id = format!("pg-{}", uuid::Uuid::new_v4());
        store
            .record(&attempt(&id, TransferAttemptState::QueuedManual))
            .await
            .unwrap();
        store
            .record(&attempt(&id, TransferAttemptState::Committed))
            .await
            .unwrap();
        let got = store.get(&id).await.unwrap().unwrap();
        assert_eq!(got.state, TransferAttemptState::QueuedManual);
        store
            .mark(&id, TransferAttemptState::Committed, Some("done"))
            .await
            .unwrap();
        let got = store.get(&id).await.unwrap().unwrap();
        assert_eq!(got.state, TransferAttemptState::Committed);
    }
}
