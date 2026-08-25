//! Durable payout-attempt records + reconciler (SPEC-W43 P-01, contract C3).
//!
//! The payout flow is LEDGER-FIRST: a pending payout transfer reserves the
//! funds before the rail is called; the rail's explicit COMMITTED posts it;
//! failure/unknown voids the pending transfer and records a durable
//! `payout_attempts` row. The reconciler sweeps `unknown` rows, re-queries
//! the rail, and settles or fails them.
//!
//! Two store implementations behind [`PayoutAttemptStore`]:
//! - [`PgPayoutAttemptStore`] — Postgres (production; bootstrap DDL at boot,
//!   fail-closed when configured but unreachable);
//! - [`MemPayoutAttemptStore`] — in-memory dev fallback when no DSN is
//!   configured (records are lost on restart; main.rs logs a loud warning).

use std::collections::BTreeMap;

use async_trait::async_trait;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use tokio::sync::watch;
use tracing::{error, info, warn};
use uuid::Uuid;

use crate::ledger::{transfer_id_from_key, TransferState};
use crate::mojaloop::RailQueryState;
use crate::AppState;

/// Lifecycle of a payout attempt row.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AttemptState {
    /// Rail outcome unknown (transport error / decode failure / ambiguous
    /// state after the transfer was sent) — the reconciler sweeps these.
    Unknown,
    /// Rail explicitly rejected the payout; the pending hold was voided.
    Failed,
    /// Rail COMMITTED and the ledger payout was posted.
    Committed,
    /// Reconciler: the rail later confirmed COMMITTED; ledger settled.
    ResolvedCommitted,
    /// Reconciler: the rail confirmed the transfer never happened / aborted.
    ResolvedFailed,
}

impl AttemptState {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Unknown => "unknown",
            Self::Failed => "failed",
            Self::Committed => "committed",
            Self::ResolvedCommitted => "resolved_committed",
            Self::ResolvedFailed => "resolved_failed",
        }
    }

    pub fn from_str(s: &str) -> Option<Self> {
        match s {
            "unknown" => Some(Self::Unknown),
            "failed" => Some(Self::Failed),
            "committed" => Some(Self::Committed),
            "resolved_committed" => Some(Self::ResolvedCommitted),
            "resolved_failed" => Some(Self::ResolvedFailed),
            _ => None,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PayoutAttempt {
    /// Deterministic payout id (uuid string, derived from the idempotency
    /// key); also the Mojaloop transferId and the ledger hold id.
    pub payout_id: String,
    pub tenant_id: String,
    pub amount_cents: u64,
    pub currency: String,
    pub payee: serde_json::Value,
    pub state: AttemptState,
    pub detail: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[async_trait]
pub trait PayoutAttemptStore: Send + Sync {
    /// Insert a new attempt row (idempotent: first record for a payout id
    /// wins; replays do not rewrite history).
    async fn record(&self, attempt: &PayoutAttempt) -> Result<(), String>;
    async fn get(&self, payout_id: &str) -> Result<Option<PayoutAttempt>, String>;
    async fn list_unknown(&self, limit: i64) -> Result<Vec<PayoutAttempt>, String>;
    async fn mark(
        &self,
        payout_id: &str,
        state: AttemptState,
        detail: Option<&str>,
    ) -> Result<(), String>;
}

// ---------------------------------------------------------------------------
// Postgres implementation (production)
// ---------------------------------------------------------------------------

/// Bootstrap DDL (C3: "new table via payments bootstrap migration"). Run
/// idempotently at boot when a DSN is configured.
pub const BOOTSTRAP_DDL: &str = r#"
CREATE TABLE IF NOT EXISTS payout_attempts (
    payout_id    TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    currency     TEXT NOT NULL,
    payee        JSONB NOT NULL,
    state        TEXT NOT NULL CHECK (state IN
        ('unknown','failed','committed','resolved_committed','resolved_failed')),
    detail       TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS payout_attempts_state_idx ON payout_attempts (state);
"#;

pub struct PgPayoutAttemptStore {
    pool: sqlx::PgPool,
}

/// Parse a Postgres DSN into connect options. Socket-only DSNs of the form
/// `postgresql://user@/db?host=/path/to/socketdir` (e.g. pgserver-backed test
/// databases) cannot be parsed by sqlx 0.7's URL parser ("empty host"), so
/// that form is handled explicitly via `.socket()`.
pub fn pg_connect_options(dsn: &str) -> Result<sqlx::postgres::PgConnectOptions, String> {
    use sqlx::postgres::PgConnectOptions;
    use std::str::FromStr;
    if let Some((base, socket_dir)) = dsn.split_once("?host=") {
        let rest = base
            .trim_start_matches("postgresql://")
            .trim_start_matches("postgres://");
        let (creds, db) = rest
            .split_once('@')
            .ok_or_else(|| "socket DSN must carry user@/db".to_string())?;
        let user = creds.split(':').next().unwrap_or("postgres");
        return Ok(PgConnectOptions::new()
            .username(user)
            .socket(socket_dir)
            .database(db.trim_start_matches('/')));
    }
    PgConnectOptions::from_str(dsn).map_err(|e| format!("invalid DSN: {e}"))
}

impl PgPayoutAttemptStore {
    /// Connect (with bounded retry) and bootstrap the table. Fail-closed:
    /// when a DSN is configured but unreachable/invalid, the service refuses
    /// to boot rather than silently losing reconciliation durability.
    pub async fn connect_with_retry(dsn: &str) -> Result<Self, String> {
        let options = pg_connect_options(dsn)?;
        let mut last_err = String::new();
        for attempt in 1..=10u32 {
            match sqlx::postgres::PgPoolOptions::new()
                .max_connections(4)
                .connect_with(options.clone())
                .await
            {
                Ok(pool) => {
                    let store = Self { pool };
                    store.bootstrap().await?;
                    info!("payout_attempts store: postgres (bootstrapped)");
                    return Ok(store);
                }
                Err(e) => {
                    last_err = e.to_string();
                    let backoff = std::time::Duration::from_millis(200 * attempt as u64);
                    warn!(attempt, error = %e, "payout_attempts postgres connect failed; retrying");
                    tokio::time::sleep(backoff).await;
                }
            }
        }
        Err(format!(
            "payout_attempts postgres unavailable after 10 attempts: {last_err}"
        ))
    }

    async fn bootstrap(&self) -> Result<(), String> {
        for stmt in BOOTSTRAP_DDL.split(';').map(str::trim).filter(|s| !s.is_empty()) {
            sqlx::query(stmt)
                .execute(&self.pool)
                .await
                .map_err(|e| format!("payout_attempts bootstrap failed: {e}"))?;
        }
        Ok(())
    }

    fn row_to_attempt(row: &sqlx::postgres::PgRow) -> Result<PayoutAttempt, String> {
        use sqlx::Row;
        let amount: i64 = row
            .try_get("amount_cents")
            .map_err(|e| format!("amount_cents decode: {e}"))?;
        let state: String = row
            .try_get("state")
            .map_err(|e| format!("state decode: {e}"))?;
        Ok(PayoutAttempt {
            payout_id: row.try_get("payout_id").map_err(|e| e.to_string())?,
            tenant_id: row.try_get("tenant_id").map_err(|e| e.to_string())?,
            amount_cents: u64::try_from(amount).map_err(|_| "negative amount_cents".to_string())?,
            currency: row.try_get("currency").map_err(|e| e.to_string())?,
            payee: row.try_get("payee").map_err(|e| e.to_string())?,
            state: AttemptState::from_str(&state)
                .ok_or_else(|| format!("unknown payout attempt state '{state}'"))?,
            detail: row.try_get("detail").map_err(|e| e.to_string())?,
            created_at: row.try_get("created_at").map_err(|e| e.to_string())?,
            updated_at: row.try_get("updated_at").map_err(|e| e.to_string())?,
        })
    }
}

#[async_trait]
impl PayoutAttemptStore for PgPayoutAttemptStore {
    async fn record(&self, attempt: &PayoutAttempt) -> Result<(), String> {
        sqlx::query(
            "INSERT INTO payout_attempts
                (payout_id, tenant_id, amount_cents, currency, payee, state, detail)
             VALUES ($1, $2, $3, $4, $5, $6, $7)
             ON CONFLICT (payout_id) DO NOTHING",
        )
        .bind(&attempt.payout_id)
        .bind(&attempt.tenant_id)
        .bind(i64::try_from(attempt.amount_cents).map_err(|_| "amount overflow".to_string())?)
        .bind(&attempt.currency)
        .bind(&attempt.payee)
        .bind(attempt.state.as_str())
        .bind(&attempt.detail)
        .execute(&self.pool)
        .await
        .map_err(|e| format!("payout_attempts insert failed: {e}"))?;
        Ok(())
    }

    async fn get(&self, payout_id: &str) -> Result<Option<PayoutAttempt>, String> {
        let row = sqlx::query(
            "SELECT payout_id, tenant_id, amount_cents, currency, payee, state, detail,
                    created_at, updated_at
             FROM payout_attempts WHERE payout_id = $1",
        )
        .bind(payout_id)
        .fetch_optional(&self.pool)
        .await
        .map_err(|e| format!("payout_attempts read failed: {e}"))?;
        row.as_ref().map(Self::row_to_attempt).transpose()
    }

    async fn list_unknown(&self, limit: i64) -> Result<Vec<PayoutAttempt>, String> {
        let rows = sqlx::query(
            "SELECT payout_id, tenant_id, amount_cents, currency, payee, state, detail,
                    created_at, updated_at
             FROM payout_attempts WHERE state = 'unknown'
             ORDER BY created_at LIMIT $1",
        )
        .bind(limit)
        .fetch_all(&self.pool)
        .await
        .map_err(|e| format!("payout_attempts sweep read failed: {e}"))?;
        rows.iter().map(Self::row_to_attempt).collect()
    }

    async fn mark(
        &self,
        payout_id: &str,
        state: AttemptState,
        detail: Option<&str>,
    ) -> Result<(), String> {
        sqlx::query(
            "UPDATE payout_attempts SET state = $2, detail = COALESCE($3, detail),
                    updated_at = now()
             WHERE payout_id = $1",
        )
        .bind(payout_id)
        .bind(state.as_str())
        .bind(detail)
        .execute(&self.pool)
        .await
        .map_err(|e| format!("payout_attempts update failed: {e}"))?;
        Ok(())
    }
}

// ---------------------------------------------------------------------------
// In-memory implementation (dev fallback when no DSN is configured)
// ---------------------------------------------------------------------------

#[derive(Default)]
pub struct MemPayoutAttemptStore {
    rows: tokio::sync::Mutex<BTreeMap<String, PayoutAttempt>>,
}

#[async_trait]
impl PayoutAttemptStore for MemPayoutAttemptStore {
    async fn record(&self, attempt: &PayoutAttempt) -> Result<(), String> {
        let mut rows = self.rows.lock().await;
        rows.entry(attempt.payout_id.clone())
            .or_insert_with(|| attempt.clone());
        Ok(())
    }

    async fn get(&self, payout_id: &str) -> Result<Option<PayoutAttempt>, String> {
        Ok(self.rows.lock().await.get(payout_id).cloned())
    }

    async fn list_unknown(&self, limit: i64) -> Result<Vec<PayoutAttempt>, String> {
        let rows = self.rows.lock().await;
        Ok(rows
            .values()
            .filter(|a| a.state == AttemptState::Unknown)
            .take(limit.max(0) as usize)
            .cloned()
            .collect())
    }

    async fn mark(
        &self,
        payout_id: &str,
        state: AttemptState,
        detail: Option<&str>,
    ) -> Result<(), String> {
        let mut rows = self.rows.lock().await;
        if let Some(a) = rows.get_mut(payout_id) {
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
// Reconciler (C3): sweep unknown attempts, re-query the rail, settle or fail.
// ---------------------------------------------------------------------------

/// Deterministic ledger ids shared with the payout route, so a reconciler
/// settlement is idempotent against a route that already posted/voided.
pub fn payout_post_id(payout_id: &str) -> Uuid {
    transfer_id_from_key(Some(&format!("payout-post:{payout_id}")))
}

pub fn payout_void_id(payout_id: &str) -> Uuid {
    transfer_id_from_key(Some(&format!("payout-void:{payout_id}")))
}

pub fn payout_reconcile_id(payout_id: &str) -> Uuid {
    transfer_id_from_key(Some(&format!("payout-reconcile:{payout_id}")))
}

/// One reconciler sweep over the unknown attempts (exported for tests).
pub async fn sweep_once(state: &AppState) -> Result<(), String> {
    let unknowns = state.payout_attempts.list_unknown(100).await?;
    for attempt in unknowns {
        let payout_uuid = match Uuid::parse_str(&attempt.payout_id) {
            Ok(u) => u,
            Err(e) => {
                error!(payout_id = %attempt.payout_id, error = %e,
                    "payout_attempts row with unparseable id; marking resolved_failed");
                state
                    .payout_attempts
                    .mark(
                        &attempt.payout_id,
                        AttemptState::ResolvedFailed,
                        Some("unparseable payout id"),
                    )
                    .await?;
                continue;
            }
        };
        match state.mojaloop.query_transfer_state(payout_uuid).await {
            RailQueryState::Committed => {
                // The rail DID move the money; settle the ledger side.
                // Which ledger step is still open depends on how far the
                // original request got before the outcome became unknown.
                let hold = state.ledger.get_transfer(payout_uuid).await;
                let settled = match hold {
                    Ok(h) if h.state == TransferState::Pending => state
                        .ledger
                        .payout_post(
                            &attempt.tenant_id,
                            payout_uuid,
                            payout_post_id(&attempt.payout_id),
                        )
                        .await
                        .map(|_| ()),
                    Ok(h) if h.state == TransferState::Posted => Ok(()),
                    Ok(_) => {
                        // Hold was voided (funds released): post a direct
                        // payout with a deterministic reconcile id.
                        state
                            .ledger
                            .payout(
                                &attempt.tenant_id,
                                payout_reconcile_id(&attempt.payout_id),
                                attempt.amount_cents,
                            )
                            .await
                            .map(|_| ())
                    }
                    Err(e) => {
                        error!(payout_id = %attempt.payout_id, error = %e,
                            "CRITICAL: rail committed but payout hold missing from ledger");
                        continue; // keep sweeping
                    }
                };
                match settled {
                    Ok(()) => {
                        state
                            .payout_attempts
                            .mark(
                                &attempt.payout_id,
                                AttemptState::ResolvedCommitted,
                                Some("reconciler: rail confirmed COMMITTED; ledger settled"),
                            )
                            .await?;
                        info!(payout_id = %attempt.payout_id, "payout reconciled: committed");
                    }
                    Err(e) => {
                        error!(payout_id = %attempt.payout_id, error = %e,
                            "CRITICAL: rail committed but ledger settlement failed; will retry");
                    }
                }
            }
            RailQueryState::Failed(reason) => {
                state
                    .payout_attempts
                    .mark(
                        &attempt.payout_id,
                        AttemptState::ResolvedFailed,
                        Some(&format!("reconciler: {reason}")),
                    )
                    .await?;
                info!(payout_id = %attempt.payout_id, "payout reconciled: failed on rail");
            }
            RailQueryState::Unknown(reason) => {
                warn!(payout_id = %attempt.payout_id, reason = %reason,
                    "payout rail outcome still unknown; keeping attempt for the next sweep");
            }
        }
    }
    Ok(())
}

/// Background reconciler loop (consumer.rs supervision idiom: shutdown via
/// the shared watch channel). `allow(dead_code)`: the route_smoke mirror
/// crate compiles this module without spawning the loop.
#[allow(dead_code)]
pub async fn run_reconciler(state: AppState, mut shutdown: watch::Receiver<bool>) {
    let interval =
        std::time::Duration::from_secs(state.config.payout_reconciler_interval_secs.max(1));
    info!(?interval, "payout reconciler started");
    loop {
        tokio::select! {
            changed = shutdown.changed() => {
                if changed.is_ok() {
                    info!("payout reconciler shutting down");
                }
                break;
            }
            _ = tokio::time::sleep(interval) => {
                if let Err(e) = sweep_once(&state).await {
                    warn!(error = %e, "payout reconciler sweep failed");
                }
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Tests: mem store semantics + reconciler sweep against a stub rail (axum)
// settling an unknown attempt on the sim ledger.
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;
    use crate::ledger::sim::SimLedgerClient;
    use crate::{auth, config, consumer, dapr, flutterwave, mojaloop};
    use std::sync::atomic::AtomicU64;
    use std::sync::Arc;

    fn attempt(id: &str, state: AttemptState) -> PayoutAttempt {
        PayoutAttempt {
            payout_id: id.to_string(),
            tenant_id: "t-rec".to_string(),
            amount_cents: 5_000,
            currency: "NGN".to_string(),
            payee: serde_json::json!({"partyIdType":"ALIAS","partyIdentifier":"payee-1"}),
            state,
            detail: None,
            created_at: Utc::now(),
            updated_at: Utc::now(),
        }
    }

    #[tokio::test]
    async fn mem_store_record_get_mark_list() {
        let store = MemPayoutAttemptStore::default();
        let id = Uuid::new_v4().to_string();
        store.record(&attempt(&id, AttemptState::Unknown)).await.unwrap();
        // First record wins on replay.
        store.record(&attempt(&id, AttemptState::Failed)).await.unwrap();
        let got = store.get(&id).await.unwrap().unwrap();
        assert_eq!(got.state, AttemptState::Unknown);
        assert_eq!(store.list_unknown(10).await.unwrap().len(), 1);
        store
            .mark(&id, AttemptState::ResolvedFailed, Some("rail 404"))
            .await
            .unwrap();
        let got = store.get(&id).await.unwrap().unwrap();
        assert_eq!(got.state, AttemptState::ResolvedFailed);
        assert_eq!(got.detail.as_deref(), Some("rail 404"));
        assert_eq!(store.list_unknown(10).await.unwrap().len(), 0);
    }

    fn test_state(rail_url: String) -> AppState {
        let cfg = config::Config {
            port: 0,
            ledger_impl: "sim".to_string(),
            tb_addresses: String::new(),
            tb_cluster_id: 0,
            kafka_brokers: String::new(),
            kafka_group_id: "test".to_string(),
            kafka_commands_topic: "opendesk.payments.commands".to_string(),
            kafka_consumer_enabled: false,
            dlq_topic: "opendesk.dlq".to_string(),
            dapr_host: "127.0.0.1".to_string(),
            dapr_http_port: 1,
            dapr_pubsub: "pubsub".to_string(),
            events_topic: "opendesk.payments.events".to_string(),
            mojaloop_endpoint: rail_url.clone(),
            mojaloop_allow_sim: true,
            platform_fee_bps: 0,
            internal_token: None,
            trust_direct_tenant: true,
            database_url: None,
            payout_reconciler_interval_secs: 30,
            money_roles: vec!["owner".to_string(), "admin".to_string()],
        };
        AppState {
            ledger: Arc::new(SimLedgerClient::new(0)),
            outbox: dapr::DaprOutbox::new(
                "http://127.0.0.1:1".to_string(),
                "pubsub".to_string(),
                "opendesk.payments.events".to_string(),
            ),
            mojaloop: mojaloop::MojaloopAdapter::new(rail_url),
            flutterwave: flutterwave::FlutterwaveAdapter::from_env(),
            config: Arc::new(cfg),
            dlq: Arc::new(consumer::UnavailableDlqSink),
            auth: auth::AuthConfig::new(
                None,
                true,
                vec!["owner".to_string(), "admin".to_string()],
            ),
            payout_attempts: Arc::new(MemPayoutAttemptStore::default()),
            registry: Arc::new(crate::registry::MemRegistry::default()),
            events_published: Arc::new(AtomicU64::new(0)),
            events_failed: Arc::new(AtomicU64::new(0)),
            commands_dead_lettered: Arc::new(AtomicU64::new(0)),
            commands_processed: Arc::new(AtomicU64::new(0)),
            payouts_attempted: Arc::new(AtomicU64::new(0)),
            payouts_committed: Arc::new(AtomicU64::new(0)),
            payouts_failed: Arc::new(AtomicU64::new(0)),
            payouts_unknown: Arc::new(AtomicU64::new(0)),
        }
    }

    /// Stub Mojaloop rail answering GET /transfers/:id with a fixed state.
    async fn spawn_rail_stub(state: &'static str) -> String {
        async fn handler(
            axum::extract::Path(_id): axum::extract::Path<String>,
            axum::extract::State(s): axum::extract::State<&'static str>,
        ) -> axum::Json<serde_json::Value> {
            axum::Json(serde_json::json!({
                "transferState": s,
                "completedTimestamp": "2026-08-22T00:00:00Z"
            }))
        }
        let app = axum::Router::new()
            .route("/transfers/:id", axum::routing::get(handler))
            .with_state(state);
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        format!("http://{addr}")
    }

    /// C3 reconciler: an UNKNOWN attempt whose rail transfer turns out
    /// COMMITTED is settled on the ledger (voided hold -> direct payout post)
    /// and marked resolved_committed.
    #[tokio::test]
    async fn reconciler_settles_committed_unknown() {
        let rail = spawn_rail_stub("COMMITTED").await;
        let st = test_state(rail);
        // Earn revenue first.
        let hold = st
            .ledger
            .hold_deposit("t-rec", Uuid::new_v4(), 8_000)
            .await
            .unwrap();
        st.ledger
            .capture("t-rec", Uuid::from_u128(hold.id), Uuid::new_v4(), None)
            .await
            .unwrap();
        // Simulate the unknown-outcome path: hold reserved then voided, row
        // recorded as unknown.
        let payout_id = Uuid::new_v4();
        st.ledger.payout_hold("t-rec", payout_id, 5_000).await.unwrap();
        st.ledger
            .payout_void("t-rec", payout_id, payout_void_id(&payout_id.to_string()))
            .await
            .unwrap();
        st.payout_attempts
            .record(&attempt(&payout_id.to_string(), AttemptState::Unknown))
            .await
            .unwrap();

        sweep_once(&st).await.unwrap();

        let got = st
            .payout_attempts
            .get(&payout_id.to_string())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(got.state, AttemptState::ResolvedCommitted);
        // Ledger settled: revenue decreased by the payout amount.
        let bal = st.ledger.balance("t-rec").await.unwrap();
        let revenue = bal
            .accounts
            .iter()
            .find(|a| a.account.ends_with(":revenue"))
            .unwrap();
        assert_eq!(revenue.posted_net, 3_000);
    }

    /// P-01 (C3): the Postgres store against a REAL database — bootstrap DDL
    /// is idempotent, record/get/mark/list round-trip, first record wins.
    /// Gated on PAYMENTS_TEST_DATABASE_URL (no PG in CI); run explicitly via
    /// `PAYMENTS_TEST_DATABASE_URL=... cargo test pg_store -- --ignored`.
    #[tokio::test]
    #[ignore = "requires PAYMENTS_TEST_DATABASE_URL pointing at a real Postgres"]
    async fn pg_store_bootstrap_and_roundtrip() {
        let dsn = std::env::var("PAYMENTS_TEST_DATABASE_URL")
            .expect("PAYMENTS_TEST_DATABASE_URL must point at a real Postgres");
        let store = PgPayoutAttemptStore::connect_with_retry(&dsn).await.unwrap();
        // Bootstrap is idempotent.
        let store2 = PgPayoutAttemptStore::connect_with_retry(&dsn).await.unwrap();
        drop(store2);

        let id = format!("pg-{}", Uuid::new_v4());
        store.record(&attempt(&id, AttemptState::Unknown)).await.unwrap();
        // First record wins.
        store.record(&attempt(&id, AttemptState::Failed)).await.unwrap();
        let got = store.get(&id).await.unwrap().unwrap();
        assert_eq!(got.state, AttemptState::Unknown);
        assert_eq!(got.amount_cents, 5_000);
        assert_eq!(got.currency, "NGN");
        assert_eq!(got.payee["partyIdentifier"], "payee-1");
        assert!(store
            .list_unknown(100)
            .await
            .unwrap()
            .iter()
            .any(|a| a.payout_id == id));
        store
            .mark(&id, AttemptState::ResolvedCommitted, Some("rail confirmed"))
            .await
            .unwrap();
        let got = store.get(&id).await.unwrap().unwrap();
        assert_eq!(got.state, AttemptState::ResolvedCommitted);
        assert_eq!(got.detail.as_deref(), Some("rail confirmed"));
        assert!(!store
            .list_unknown(100)
            .await
            .unwrap()
            .iter()
            .any(|a| a.payout_id == id));
    }

    /// Reconciler: a rail that reports the transfer never happened resolves
    /// the attempt as failed and moves NO money.
    #[tokio::test]
    async fn reconciler_fails_aborted_unknown() {
        let rail = spawn_rail_stub("ABORTED").await;
        let st = test_state(rail);
        let hold = st
            .ledger
            .hold_deposit("t-rec", Uuid::new_v4(), 8_000)
            .await
            .unwrap();
        st.ledger
            .capture("t-rec", Uuid::from_u128(hold.id), Uuid::new_v4(), None)
            .await
            .unwrap();
        let payout_id = Uuid::new_v4();
        st.ledger.payout_hold("t-rec", payout_id, 5_000).await.unwrap();
        st.ledger
            .payout_void("t-rec", payout_id, payout_void_id(&payout_id.to_string()))
            .await
            .unwrap();
        st.payout_attempts
            .record(&attempt(&payout_id.to_string(), AttemptState::Unknown))
            .await
            .unwrap();

        sweep_once(&st).await.unwrap();

        let got = st
            .payout_attempts
            .get(&payout_id.to_string())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(got.state, AttemptState::ResolvedFailed);
        let bal = st.ledger.balance("t-rec").await.unwrap();
        let revenue = bal
            .accounts
            .iter()
            .find(|a| a.account.ends_with(":revenue"))
            .unwrap();
        assert_eq!(revenue.posted_net, 8_000, "no money moved on failed rail");
    }
}
