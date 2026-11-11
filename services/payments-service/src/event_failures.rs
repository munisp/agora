//! SPEC-W49 A1: reliable capture events — durable failure log + replay.
//!
//! TIER DECISION (per SPEC-W49 A1): payments-service has NO database outbox
//! pattern — the ledger commits through the TigerBeetle-compatible
//! `LedgerClient` (ADR-0007), NOT through a Postgres transaction, so there
//! is no DB transaction a capture shares with an outbox row (unlike
//! booking-service's `outbox` table or billing's durable outbox). The
//! SPEC-mandated minimal honest alternative is therefore implemented here:
//!
//!   1. **Retry-with-backoff**: the `FlutterwavePaymentCaptured` publish
//!      (`AppState::publish_event_reliable` in main.rs) retries the Dapr
//!      pubsub publish up to [`PUBLISH_ATTEMPTS`] times with backoff
//!      ([`RETRY_BACKOFF_MS`]) inside the SPEC-W46 R5 fire-and-forget
//!      spawned task — the webhook request path still never awaits the
//!      sidecar/broker RTT and capture math is untouched.
//!   2. **Durable failure log**: after the final attempt the FULL
//!      CloudEvent payload is recorded in `payment_event_failures`
//!      (`id, tx_ref, tenant_id, payload jsonb, error, created_at`), so no
//!      capture event is silently lost. Postgres when a DSN is configured
//!      (fail-closed boot, same posture as `payout_attempts`); in-memory
//!      dev fallback otherwise.
//!   3. **Replay route**: `POST /internal/events/replay` (K2-guarded by
//!      `AuthConfig::require_internal` — `X-Internal-Token` vs
//!      `PAYMENTS_INTERNAL_TOKEN`, constant-time, 401 mismatch / 503
//!      unconfigured, mirroring `/v1/internal/accounts/provision`)
//!      re-publishes recorded failures verbatim (same CloudEvent id, so
//!      idempotent consumers dedupe) and deletes each row on success.
//!
//! The table carries a real `tenant_id` column (beyond the SPEC's minimum
//! column list) so the DB-01 `tenant_isolation` RLS policy applies to it
//! exactly like the other money tables (migrations/0001_rls.sql): writes
//! are pinned to the event's tenant GUC, cross-tenant reads/deletes go
//! through the internal pool (role-gated escape).

use std::collections::BTreeMap;

use async_trait::async_trait;
use axum::{
    extract::State,
    http::{HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    routing::post,
    Json, Router,
};
use chrono::{DateTime, Utc};
use serde::Deserialize;
use tracing::{error, warn};
use uuid::Uuid;

use crate::AppState;

/// SPEC-W49 A1: total publish attempts before the event is recorded as
/// durably failed ("retry-with-backoff (3 attempts)").
pub const PUBLISH_ATTEMPTS: u32 = 3;
/// Backoff slept before attempts 2 and 3 (attempt 1 is immediate). Short on
/// purpose: this runs in the spawned fire-and-forget task, not on the
/// request path, and the durable failure record is the real safety net.
pub const RETRY_BACKOFF_MS: [u64; PUBLISH_ATTEMPTS as usize - 1] = [200, 800];

/// A capture event whose Dapr pubsub publication exhausted every retry.
#[derive(Debug, Clone)]
pub struct PaymentEventFailure {
    pub id: Uuid,
    pub tx_ref: String,
    pub tenant_id: String,
    /// The full CloudEvent JSON, republished verbatim on replay (same event
    /// `id`, so idempotent consumers dedupe a replay vs a late original).
    pub payload: serde_json::Value,
    pub error: String,
    pub created_at: DateTime<Utc>,
}

#[async_trait]
pub trait EventFailureStore: Send + Sync {
    /// Record a failed event. DB-01: pinned to `failure.tenant_id` (the
    /// tenant GUC is set transaction-locally before the insert).
    async fn record(&self, failure: &PaymentEventFailure) -> Result<(), String>;
    /// Cross-tenant read of one row (replay route — K2-guarded). Pg store:
    /// internal pool, role-gated escape.
    async fn get_internal(&self, id: Uuid) -> Result<Option<PaymentEventFailure>, String>;
    /// Cross-tenant sweep, optionally filtered by tx_ref (replay route).
    /// Pg store: internal pool.
    async fn list_internal(
        &self,
        tx_ref: Option<&str>,
        limit: i64,
    ) -> Result<Vec<PaymentEventFailure>, String>;
    /// Delete after a successful replay. Pg store: internal pool.
    async fn delete_internal(&self, id: Uuid) -> Result<(), String>;
}

// ---------------------------------------------------------------------------
// Postgres implementation (production)
// ---------------------------------------------------------------------------

/// Bootstrap DDL (SPEC-W49 A1). Run idempotently at boot when a DSN is
/// configured; migrations/0001_rls.sql then ENABLEs+FORCEs RLS on the table.
/// `tenant_id` is carried as a real column (beyond the SPEC minimum) so the
/// DB-01 tenant_isolation policy applies.
pub const BOOTSTRAP_DDL: &str = r#"
CREATE TABLE IF NOT EXISTS payment_event_failures (
    id          UUID PRIMARY KEY,
    tx_ref      TEXT NOT NULL,
    tenant_id   TEXT NOT NULL,
    payload     JSONB NOT NULL,
    error       TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS payment_event_failures_tx_ref_idx
    ON payment_event_failures (tx_ref);
"#;

pub struct PgEventFailureStore {
    pool: sqlx::PgPool,
    /// SPEC-W47 DB-01: pool for the cross-tenant internal paths (the replay
    /// route reads/deletes across tenants). `PAYMENTS_INTERNAL_DATABASE_URL`
    /// when configured; falls back to the main pool when unset (dev default:
    /// bootstrap superuser bypasses RLS).
    internal_pool: sqlx::PgPool,
}

impl PgEventFailureStore {
    /// Connect (with bounded retry) and bootstrap the table. Fail-closed,
    /// mirroring `PgPayoutAttemptStore::connect_with_retry`.
    pub async fn connect_with_retry(dsn: &str, internal_dsn: Option<&str>) -> Result<Self, String> {
        let options = crate::payouts::pg_connect_options(dsn)?;
        let mut last_err = String::new();
        for attempt in 1..=10u32 {
            match sqlx::postgres::PgPoolOptions::new()
                .max_connections(2)
                .acquire_timeout(crate::payouts::DB_ACQUIRE_TIMEOUT)
                .connect_with(options.clone())
                .await
            {
                Ok(pool) => {
                    let internal_pool = match internal_dsn {
                        Some(idsn) => {
                            let iopts = crate::payouts::pg_connect_options(idsn)?;
                            sqlx::postgres::PgPoolOptions::new()
                                .max_connections(2)
                                .acquire_timeout(crate::payouts::DB_ACQUIRE_TIMEOUT)
                                .connect_with(iopts)
                                .await
                                .map_err(|e| {
                                    format!(
                                        "payment_event_failures internal pool connect failed: {e}"
                                    )
                                })?
                        }
                        None => pool.clone(),
                    };
                    let store = Self { pool, internal_pool };
                    store.bootstrap().await?;
                    tracing::info!("payment_event_failures store: postgres (bootstrapped)");
                    return Ok(store);
                }
                Err(e) => {
                    last_err = e.to_string();
                    let backoff = std::time::Duration::from_millis(200 * attempt as u64);
                    warn!(attempt, error = %e, "payment_event_failures postgres connect failed; retrying");
                    tokio::time::sleep(backoff).await;
                }
            }
        }
        Err(format!(
            "payment_event_failures postgres unavailable after 10 attempts: {last_err}"
        ))
    }

    async fn bootstrap(&self) -> Result<(), String> {
        for stmt in BOOTSTRAP_DDL.split(';').map(str::trim).filter(|s| !s.is_empty()) {
            sqlx::query(stmt)
                .execute(&self.pool)
                .await
                .map_err(|e| format!("payment_event_failures bootstrap failed: {e}"))?;
        }
        Ok(())
    }

    fn row_to_failure(row: &sqlx::postgres::PgRow) -> Result<PaymentEventFailure, String> {
        use sqlx::Row;
        Ok(PaymentEventFailure {
            id: row.try_get("id").map_err(|e| e.to_string())?,
            tx_ref: row.try_get("tx_ref").map_err(|e| e.to_string())?,
            tenant_id: row.try_get("tenant_id").map_err(|e| e.to_string())?,
            payload: row.try_get("payload").map_err(|e| e.to_string())?,
            error: row.try_get("error").map_err(|e| e.to_string())?,
            created_at: row.try_get("created_at").map_err(|e| e.to_string())?,
        })
    }

    const SELECT_COLS: &'static str = "id, tx_ref, tenant_id, payload, error, created_at";
}

#[async_trait]
impl EventFailureStore for PgEventFailureStore {
    async fn record(&self, failure: &PaymentEventFailure) -> Result<(), String> {
        // SPEC-W47 DB-01: pin the RLS tenant GUC for the insert.
        let mut tx = crate::tenant::begin_tenant_tx(&self.pool, &failure.tenant_id)
            .await
            .map_err(|e| format!("payment_event_failures tx failed: {e}"))?;
        sqlx::query(
            "INSERT INTO payment_event_failures (id, tx_ref, tenant_id, payload, error)
             VALUES ($1, $2, $3, $4, $5)
             ON CONFLICT (id) DO NOTHING",
        )
        .bind(failure.id)
        .bind(&failure.tx_ref)
        .bind(&failure.tenant_id)
        .bind(&failure.payload)
        .bind(&failure.error)
        .execute(&mut *tx)
        .await
        .map_err(|e| format!("payment_event_failures insert failed: {e}"))?;
        tx.commit()
            .await
            .map_err(|e| format!("payment_event_failures commit failed: {e}"))?;
        Ok(())
    }

    async fn get_internal(&self, id: Uuid) -> Result<Option<PaymentEventFailure>, String> {
        let mut tx = crate::tenant::begin_internal_tx(&self.internal_pool)
            .await
            .map_err(|e| format!("payment_event_failures internal tx failed: {e}"))?;
        let row = sqlx::query(&format!(
            "SELECT {} FROM payment_event_failures WHERE id = $1",
            Self::SELECT_COLS
        ))
        .bind(id)
        .fetch_optional(&mut *tx)
        .await
        .map_err(|e| format!("payment_event_failures internal read failed: {e}"))?;
        let out = row.as_ref().map(Self::row_to_failure).transpose();
        tx.commit()
            .await
            .map_err(|e| format!("payment_event_failures commit failed: {e}"))?;
        out
    }

    async fn list_internal(
        &self,
        tx_ref: Option<&str>,
        limit: i64,
    ) -> Result<Vec<PaymentEventFailure>, String> {
        let mut tx = crate::tenant::begin_internal_tx(&self.internal_pool)
            .await
            .map_err(|e| format!("payment_event_failures internal tx failed: {e}"))?;
        let rows = sqlx::query(&format!(
            "SELECT {} FROM payment_event_failures
             WHERE ($1::TEXT IS NULL OR tx_ref = $1)
             ORDER BY created_at LIMIT $2",
            Self::SELECT_COLS
        ))
        .bind(tx_ref)
        .bind(limit)
        .fetch_all(&mut *tx)
        .await
        .map_err(|e| format!("payment_event_failures sweep read failed: {e}"))?;
        let out = rows
            .iter()
            .map(Self::row_to_failure)
            .collect::<Result<Vec<_>, _>>();
        tx.commit()
            .await
            .map_err(|e| format!("payment_event_failures commit failed: {e}"))?;
        out
    }

    async fn delete_internal(&self, id: Uuid) -> Result<(), String> {
        let mut tx = crate::tenant::begin_internal_tx(&self.internal_pool)
            .await
            .map_err(|e| format!("payment_event_failures internal tx failed: {e}"))?;
        sqlx::query("DELETE FROM payment_event_failures WHERE id = $1")
            .bind(id)
            .execute(&mut *tx)
            .await
            .map_err(|e| format!("payment_event_failures delete failed: {e}"))?;
        tx.commit()
            .await
            .map_err(|e| format!("payment_event_failures commit failed: {e}"))?;
        Ok(())
    }
}

// ---------------------------------------------------------------------------
// In-memory implementation (dev fallback when no DSN is configured)
// ---------------------------------------------------------------------------

#[derive(Default)]
pub struct MemEventFailureStore {
    rows: tokio::sync::Mutex<BTreeMap<Uuid, PaymentEventFailure>>,
}

#[async_trait]
impl EventFailureStore for MemEventFailureStore {
    async fn record(&self, failure: &PaymentEventFailure) -> Result<(), String> {
        self.rows
            .lock()
            .await
            .entry(failure.id)
            .or_insert_with(|| failure.clone());
        Ok(())
    }

    async fn get_internal(&self, id: Uuid) -> Result<Option<PaymentEventFailure>, String> {
        Ok(self.rows.lock().await.get(&id).cloned())
    }

    async fn list_internal(
        &self,
        tx_ref: Option<&str>,
        limit: i64,
    ) -> Result<Vec<PaymentEventFailure>, String> {
        let rows = self.rows.lock().await;
        Ok(rows
            .values()
            .filter(|f| tx_ref.is_none() || Some(f.tx_ref.as_str()) == tx_ref)
            .take(limit.max(0) as usize)
            .cloned()
            .collect())
    }

    async fn delete_internal(&self, id: Uuid) -> Result<(), String> {
        self.rows.lock().await.remove(&id);
        Ok(())
    }
}

// ---------------------------------------------------------------------------
// POST /internal/events/replay (K2-guarded)
// ---------------------------------------------------------------------------

pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/internal/events/replay", post(replay))
        .with_state(state)
}

fn err_json(status: StatusCode, msg: impl Into<String>) -> Response {
    (status, Json(serde_json::json!({ "error": msg.into() }))).into_response()
}

#[derive(Debug, Deserialize)]
pub struct ReplayBody {
    /// Replay exactly one recorded failure.
    pub id: Option<Uuid>,
    /// Replay every recorded failure for this tx_ref (ignored when `id` set).
    pub tx_ref: Option<String>,
    /// Cap on rows replayed in one call (default 50, max 500).
    pub limit: Option<i64>,
}

/// Re-publish recorded failed events verbatim and delete each row whose
/// republication the broker accepted. Delivery stays AT-LEAST-ONCE (the row
/// is deleted only AFTER a successful publish; a crash between the two
/// re-delivers on the next replay, which idempotent consumers tolerate).
pub async fn replay(State(st): State<AppState>, headers: HeaderMap, Json(body): Json<ReplayBody>) -> Response {
    // K2: internal-token gate, identical posture to
    // /v1/internal/accounts/provision (401 wrong token, 503 unconfigured).
    if let Err(r) = st.auth.require_internal(&headers) {
        return err_json(r.status(), r.message());
    }
    let limit = body.limit.unwrap_or(50).clamp(1, 500);
    let rows = if let Some(id) = body.id {
        match st.event_failures.get_internal(id).await {
            Ok(Some(f)) => vec![f],
            Ok(None) => {
                return err_json(StatusCode::NOT_FOUND, "no recorded failure with that id")
            }
            Err(e) => return err_json(StatusCode::INTERNAL_SERVER_ERROR, e),
        }
    } else {
        match st
            .event_failures
            .list_internal(body.tx_ref.as_deref(), limit)
            .await
        {
            Ok(rs) => rs,
            Err(e) => return err_json(StatusCode::INTERNAL_SERVER_ERROR, e),
        }
    };

    let mut replayed = 0u64;
    let mut failed = 0u64;
    for row in rows {
        match st.outbox.publish_value(&row.payload).await {
            Ok(()) => {
                if let Err(e) = st.event_failures.delete_internal(row.id).await {
                    // The event IS published now; the row surviving means a
                    // later replay re-publishes it (at-least-once; consumers
                    // are idempotent). Loud log, counted as replayed.
                    error!(id = %row.id, error = %e,
                        "payment_event_failures: replayed but delete failed (will re-deliver)");
                }
                replayed += 1;
            }
            Err(e) => {
                failed += 1;
                warn!(id = %row.id, tx_ref = %row.tx_ref, error = %e,
                    "payment_event_failures: replay publish failed; row retained");
            }
        }
    }
    (
        StatusCode::OK,
        Json(serde_json::json!({ "replayed": replayed, "failed": failed })),
    )
        .into_response()
}
