//! SPEC-W44 K7 payee registry + deposit provenance (closes S1-F7-01).
//!
//! The mint-and-drain chain required a registry of vetted payout
//! destinations: a raw per-call `payee` on POST /v1/payouts let any tenant
//! member drain tenant revenue to an arbitrary Mojaloop party. Payouts now
//! reference a [`Beneficiary`] row owned by the tenant (never disabled), and
//! human-created deposits record WHO declared them (`declared_by` from the
//! gateway-injected `X-User-Id`) plus an optional PSP reference.
//!
//! Storage follows the payout_attempts idiom (payments has no migrations
//! dir): bootstrap DDL applied idempotently at boot by [`PgRegistry`]
//! (fail-closed when a DSN is configured but unreachable), with a
//! [`MemRegistry`] dev fallback when no DSN is set.

use std::collections::BTreeMap;

use async_trait::async_trait;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use tracing::{info, warn};
use uuid::Uuid;

/// A vetted payout destination owned by one tenant.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Beneficiary {
    pub id: Uuid,
    pub tenant_id: String,
    pub label: String,
    /// Mojaloop `PartyIdInfo` shape ({partyIdType, partyIdentifier}).
    pub party_id_info: serde_json::Value,
    /// Gateway `X-User-Id` of the creator ("unknown" under the dev escape).
    pub created_by: String,
    pub created_at: DateTime<Utc>,
    pub disabled_at: Option<DateTime<Utc>>,
}

/// K7 deposit provenance: who declared a human deposit and via which PSP
/// reference. Internal/verified-payment deposit creation does NOT write
/// provenance (that path is unchanged).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DepositProvenance {
    pub deposit_id: String,
    pub tenant_id: String,
    pub declared_by: String,
    pub psp_reference: Option<String>,
    pub created_at: DateTime<Utc>,
}

#[async_trait]
pub trait Registry: Send + Sync {
    /// Cheap liveness probe for /healthz (Pg: SELECT 1; Mem: always ok).
    async fn ping(&self) -> Result<(), String>;
    async fn create_beneficiary(&self, b: &Beneficiary) -> Result<(), String>;
    /// All beneficiaries of the tenant (disabled included; callers filter).
    async fn list_beneficiaries(&self, tenant_id: &str) -> Result<Vec<Beneficiary>, String>;
    async fn get_beneficiary(&self, id: Uuid) -> Result<Option<Beneficiary>, String>;
    /// Idempotent disable scoped to the owning tenant. Returns the updated
    /// row, or None when the id is unknown / foreign (defense in depth —
    /// routes re-check tenancy before calling).
    async fn disable_beneficiary(
        &self,
        id: Uuid,
        tenant_id: &str,
    ) -> Result<Option<Beneficiary>, String>;
    /// First write wins (deposit holds are idempotent by transfer id).
    async fn record_deposit_provenance(&self, p: &DepositProvenance) -> Result<(), String>;
    async fn deposit_provenance(
        &self,
        deposit_id: &str,
    ) -> Result<Option<DepositProvenance>, String>;
}

// ---------------------------------------------------------------------------
// Postgres implementation (production; bootstrap DDL at boot, fail-closed)
// ---------------------------------------------------------------------------

/// Bootstrap DDL (K7). Run idempotently at boot when a DSN is configured.
/// NOTE: payments-service has no migrations dir (runtime-DDL idiom, same as
/// `payout_attempts`); this constant is the canonical schema.
pub const BOOTSTRAP_DDL: &str = r#"
CREATE TABLE IF NOT EXISTS payout_beneficiaries (
    id            UUID PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    label         TEXT NOT NULL,
    party_id_info JSONB NOT NULL,
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS payout_beneficiaries_tenant_idx
    ON payout_beneficiaries (tenant_id);
CREATE TABLE IF NOT EXISTS deposit_provenance (
    deposit_id    TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    declared_by   TEXT NOT NULL,
    psp_reference TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
"#;

pub struct PgRegistry {
    pool: sqlx::PgPool,
}

impl PgRegistry {
    /// Connect (with bounded retry) and bootstrap the tables. Fail-closed,
    /// mirroring PgPayoutAttemptStore: a configured-but-unreachable database
    /// refuses the boot rather than silently dropping the payee registry.
    pub async fn connect_with_retry(dsn: &str) -> Result<Self, String> {
        // Socket-only DSNs (pgserver test databases) parse via the shared
        // helper (sqlx 0.7's URL parser rejects `?host=` socket forms).
        let options = crate::payouts::pg_connect_options(dsn)?;
        let mut last_err = String::new();
        for attempt in 1..=10u32 {
            match sqlx::postgres::PgPoolOptions::new()
                .max_connections(4)
                .connect_with(options.clone())
                .await
            {
                Ok(pool) => {
                    let registry = Self { pool };
                    registry.bootstrap().await?;
                    info!("payout_beneficiaries/deposit_provenance store: postgres (bootstrapped)");
                    return Ok(registry);
                }
                Err(e) => {
                    last_err = e.to_string();
                    let backoff = std::time::Duration::from_millis(200 * attempt as u64);
                    warn!(attempt, error = %e, "registry postgres connect failed; retrying");
                    tokio::time::sleep(backoff).await;
                }
            }
        }
        Err(format!(
            "registry postgres unavailable after 10 attempts: {last_err}"
        ))
    }

    async fn bootstrap(&self) -> Result<(), String> {
        for stmt in BOOTSTRAP_DDL.split(';').map(str::trim).filter(|s| !s.is_empty()) {
            sqlx::query(stmt)
                .execute(&self.pool)
                .await
                .map_err(|e| format!("registry bootstrap failed: {e}"))?;
        }
        Ok(())
    }

    fn row_to_beneficiary(row: &sqlx::postgres::PgRow) -> Result<Beneficiary, String> {
        use sqlx::Row;
        Ok(Beneficiary {
            id: row.try_get("id").map_err(|e| e.to_string())?,
            tenant_id: row.try_get("tenant_id").map_err(|e| e.to_string())?,
            label: row.try_get("label").map_err(|e| e.to_string())?,
            party_id_info: row.try_get("party_id_info").map_err(|e| e.to_string())?,
            created_by: row.try_get("created_by").map_err(|e| e.to_string())?,
            created_at: row.try_get("created_at").map_err(|e| e.to_string())?,
            disabled_at: row.try_get("disabled_at").map_err(|e| e.to_string())?,
        })
    }
}

#[async_trait]
impl Registry for PgRegistry {
    async fn ping(&self) -> Result<(), String> {
        sqlx::query("SELECT 1")
            .execute(&self.pool)
            .await
            .map(|_| ())
            .map_err(|e| format!("registry pg ping failed: {e}"))
    }

    async fn create_beneficiary(&self, b: &Beneficiary) -> Result<(), String> {
        sqlx::query(
            "INSERT INTO payout_beneficiaries
                (id, tenant_id, label, party_id_info, created_by)
             VALUES ($1, $2, $3, $4, $5)",
        )
        .bind(b.id)
        .bind(&b.tenant_id)
        .bind(&b.label)
        .bind(&b.party_id_info)
        .bind(&b.created_by)
        .execute(&self.pool)
        .await
        .map_err(|e| format!("payout_beneficiaries insert failed: {e}"))?;
        Ok(())
    }

    async fn list_beneficiaries(&self, tenant_id: &str) -> Result<Vec<Beneficiary>, String> {
        let rows = sqlx::query(
            "SELECT id, tenant_id, label, party_id_info, created_by, created_at, disabled_at
             FROM payout_beneficiaries WHERE tenant_id = $1 ORDER BY created_at",
        )
        .bind(tenant_id)
        .fetch_all(&self.pool)
        .await
        .map_err(|e| format!("payout_beneficiaries list failed: {e}"))?;
        rows.iter().map(Self::row_to_beneficiary).collect()
    }

    async fn get_beneficiary(&self, id: Uuid) -> Result<Option<Beneficiary>, String> {
        let row = sqlx::query(
            "SELECT id, tenant_id, label, party_id_info, created_by, created_at, disabled_at
             FROM payout_beneficiaries WHERE id = $1",
        )
        .bind(id)
        .fetch_optional(&self.pool)
        .await
        .map_err(|e| format!("payout_beneficiaries read failed: {e}"))?;
        row.as_ref().map(Self::row_to_beneficiary).transpose()
    }

    async fn disable_beneficiary(
        &self,
        id: Uuid,
        tenant_id: &str,
    ) -> Result<Option<Beneficiary>, String> {
        let res = sqlx::query(
            "UPDATE payout_beneficiaries SET disabled_at = COALESCE(disabled_at, now())
             WHERE id = $1 AND tenant_id = $2",
        )
        .bind(id)
        .bind(tenant_id)
        .execute(&self.pool)
        .await
        .map_err(|e| format!("payout_beneficiaries disable failed: {e}"))?;
        if res.rows_affected() == 0 {
            return Ok(None); // unknown or foreign id — no cross-tenant oracle
        }
        self.get_beneficiary(id).await
    }

    async fn record_deposit_provenance(&self, p: &DepositProvenance) -> Result<(), String> {
        sqlx::query(
            "INSERT INTO deposit_provenance
                (deposit_id, tenant_id, declared_by, psp_reference)
             VALUES ($1, $2, $3, $4)
             ON CONFLICT (deposit_id) DO NOTHING",
        )
        .bind(&p.deposit_id)
        .bind(&p.tenant_id)
        .bind(&p.declared_by)
        .bind(&p.psp_reference)
        .execute(&self.pool)
        .await
        .map_err(|e| format!("deposit_provenance insert failed: {e}"))?;
        Ok(())
    }

    async fn deposit_provenance(
        &self,
        deposit_id: &str,
    ) -> Result<Option<DepositProvenance>, String> {
        use sqlx::Row;
        let row = sqlx::query(
            "SELECT deposit_id, tenant_id, declared_by, psp_reference, created_at
             FROM deposit_provenance WHERE deposit_id = $1",
        )
        .bind(deposit_id)
        .fetch_optional(&self.pool)
        .await
        .map_err(|e| format!("deposit_provenance read failed: {e}"))?;
        row.as_ref()
            .map(|r| {
                Ok(DepositProvenance {
                    deposit_id: r.try_get("deposit_id").map_err(|e| e.to_string())?,
                    tenant_id: r.try_get("tenant_id").map_err(|e| e.to_string())?,
                    declared_by: r.try_get("declared_by").map_err(|e| e.to_string())?,
                    psp_reference: r.try_get("psp_reference").map_err(|e| e.to_string())?,
                    created_at: r.try_get("created_at").map_err(|e| e.to_string())?,
                })
            })
            .transpose()
    }
}

// ---------------------------------------------------------------------------
// In-memory implementation (dev fallback when no DSN is configured)
// ---------------------------------------------------------------------------

#[derive(Default)]
pub struct MemRegistry {
    beneficiaries: tokio::sync::Mutex<BTreeMap<Uuid, Beneficiary>>,
    provenance: tokio::sync::Mutex<BTreeMap<String, DepositProvenance>>,
}

#[async_trait]
impl Registry for MemRegistry {
    async fn ping(&self) -> Result<(), String> {
        Ok(())
    }

    async fn create_beneficiary(&self, b: &Beneficiary) -> Result<(), String> {
        self.beneficiaries.lock().await.insert(b.id, b.clone());
        Ok(())
    }

    async fn list_beneficiaries(&self, tenant_id: &str) -> Result<Vec<Beneficiary>, String> {
        Ok(self
            .beneficiaries
            .lock()
            .await
            .values()
            .filter(|b| b.tenant_id == tenant_id)
            .cloned()
            .collect())
    }

    async fn get_beneficiary(&self, id: Uuid) -> Result<Option<Beneficiary>, String> {
        Ok(self.beneficiaries.lock().await.get(&id).cloned())
    }

    async fn disable_beneficiary(
        &self,
        id: Uuid,
        tenant_id: &str,
    ) -> Result<Option<Beneficiary>, String> {
        let mut rows = self.beneficiaries.lock().await;
        let Some(b) = rows.get_mut(&id) else {
            return Ok(None);
        };
        if b.tenant_id != tenant_id {
            return Ok(None);
        }
        if b.disabled_at.is_none() {
            b.disabled_at = Some(Utc::now());
        }
        Ok(Some(b.clone()))
    }

    async fn record_deposit_provenance(&self, p: &DepositProvenance) -> Result<(), String> {
        self.provenance
            .lock()
            .await
            .entry(p.deposit_id.clone())
            .or_insert_with(|| p.clone());
        Ok(())
    }

    async fn deposit_provenance(
        &self,
        deposit_id: &str,
    ) -> Result<Option<DepositProvenance>, String> {
        Ok(self.provenance.lock().await.get(deposit_id).cloned())
    }
}

// ---------------------------------------------------------------------------
// Tests (mem store semantics; the Pg store is exercised by the same class of
// gated integration test as PgPayoutAttemptStore).
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;

    fn beneficiary(tenant: &str) -> Beneficiary {
        Beneficiary {
            id: Uuid::new_v4(),
            tenant_id: tenant.to_string(),
            label: "Main account".to_string(),
            party_id_info: serde_json::json!({
                "partyIdType": "ALIAS",
                "partyIdentifier": "payee-1",
            }),
            created_by: "user-1".to_string(),
            created_at: Utc::now(),
            disabled_at: None,
        }
    }

    #[tokio::test]
    async fn mem_beneficiary_create_list_get_disable() {
        let reg = MemRegistry::default();
        let a = beneficiary("t-a");
        let b = beneficiary("t-b");
        reg.create_beneficiary(&a).await.unwrap();
        reg.create_beneficiary(&b).await.unwrap();

        // List is tenant-scoped.
        let list = reg.list_beneficiaries("t-a").await.unwrap();
        assert_eq!(list.len(), 1);
        assert_eq!(list[0].id, a.id);

        // Disable is tenant-scoped: the owning tenant disables, a foreign
        // tenant gets None and the row stays enabled.
        assert!(reg.disable_beneficiary(a.id, "t-b").await.unwrap().is_none());
        assert!(reg.get_beneficiary(a.id).await.unwrap().unwrap().disabled_at.is_none());
        let disabled = reg.disable_beneficiary(a.id, "t-a").await.unwrap().unwrap();
        assert!(disabled.disabled_at.is_some());
        // Idempotent re-disable keeps the first timestamp.
        let again = reg.disable_beneficiary(a.id, "t-a").await.unwrap().unwrap();
        assert_eq!(again.disabled_at, disabled.disabled_at);
        assert!(reg.disable_beneficiary(Uuid::new_v4(), "t-a").await.unwrap().is_none());
    }

    #[tokio::test]
    async fn mem_provenance_first_write_wins() {
        let reg = MemRegistry::default();
        let p = DepositProvenance {
            deposit_id: "dep-1".to_string(),
            tenant_id: "t-a".to_string(),
            declared_by: "user-1".to_string(),
            psp_reference: Some("psp-abc".to_string()),
            created_at: Utc::now(),
        };
        reg.record_deposit_provenance(&p).await.unwrap();
        let mut replay = p.clone();
        replay.declared_by = "attacker".to_string();
        reg.record_deposit_provenance(&replay).await.unwrap();
        let got = reg.deposit_provenance("dep-1").await.unwrap().unwrap();
        assert_eq!(got.declared_by, "user-1", "provenance is write-once");
        assert_eq!(got.psp_reference.as_deref(), Some("psp-abc"));
        assert!(reg.deposit_provenance("nope").await.unwrap().is_none());
    }

    /// K7: the Postgres registry against a REAL database — bootstrap DDL is
    /// idempotent, beneficiary + provenance round-trip, tenancy enforced.
    /// Gated on PAYMENTS_TEST_DATABASE_URL; run explicitly via
    /// `PAYMENTS_TEST_DATABASE_URL=... cargo test pg_registry -- --ignored`.
    #[tokio::test]
    #[ignore = "requires PAYMENTS_TEST_DATABASE_URL pointing at a real Postgres"]
    async fn pg_registry_bootstrap_and_roundtrip() {
        let dsn = std::env::var("PAYMENTS_TEST_DATABASE_URL")
            .expect("PAYMENTS_TEST_DATABASE_URL must point at a real Postgres");
        let reg = PgRegistry::connect_with_retry(&dsn).await.unwrap();
        // Bootstrap is idempotent.
        let reg2 = PgRegistry::connect_with_retry(&dsn).await.unwrap();
        drop(reg2);

        let b = beneficiary(&format!("t-pg-{}", Uuid::new_v4()));
        reg.create_beneficiary(&b).await.unwrap();
        let got = reg.get_beneficiary(b.id).await.unwrap().unwrap();
        assert_eq!(got.tenant_id, b.tenant_id);
        assert_eq!(got.party_id_info["partyIdentifier"], "payee-1");
        assert!(got.disabled_at.is_none());
        assert!(reg
            .list_beneficiaries(&b.tenant_id)
            .await
            .unwrap()
            .iter()
            .any(|x| x.id == b.id));
        // Foreign disable is a no-op; owning tenant disables.
        assert!(reg.disable_beneficiary(b.id, "t-foreign").await.unwrap().is_none());
        let disabled = reg.disable_beneficiary(b.id, &b.tenant_id).await.unwrap().unwrap();
        assert!(disabled.disabled_at.is_some());

        let p = DepositProvenance {
            deposit_id: format!("dep-pg-{}", Uuid::new_v4()),
            tenant_id: b.tenant_id.clone(),
            declared_by: "user-9".to_string(),
            psp_reference: None,
            created_at: Utc::now(),
        };
        reg.record_deposit_provenance(&p).await.unwrap();
        let got = reg.deposit_provenance(&p.deposit_id).await.unwrap().unwrap();
        assert_eq!(got.declared_by, "user-9");
        assert!(got.psp_reference.is_none());
    }
}
