//! SPEC-W47 DB-01: payments money-table RLS hardening — live Postgres proof.
//!
//! Covers the DB-01 audit finding (A-DB-register): payout_beneficiaries,
//! deposit_provenance, payout_attempts and rail_attempts previously had NO
//! RLS and were accessed over the bootstrap superuser DSN with id-only
//! filters, so any holder of the DSN could read/update every tenant's rows.
//!
//! These tests run against a REAL Postgres (self-skip — billing's
//! pg_hardening.rs idiom — when `PAYMENTS_TEST_DATABASE_URL` is unset) and
//! prove:
//!   1. migrations/0001_rls.sql ENABLEs+FORCEs RLS on all four tables and
//!      creates the app_payments / app_payments_internal roles;
//!   2. under the least-privilege `app_payments` role (SET LOCAL ROLE), a
//!      transaction WITHOUT the `app.tenant_id` GUC sees 0 rows (fail-closed)
//!      and a transaction pinned to tenant B cannot see tenant A's
//!      rail_attempts (and vice versa) — cross-tenant isolation;
//!   3. the policy WITH CHECK rejects an insert whose row tenant differs
//!      from the pinned GUC;
//!   4. the internal role escape (app_payments_internal) sees all tenants
//!      WITHOUT the GUC (reconciler sweep path), and role membership — not
//!      the GUC — gates it;
//!   5. the transaction-local GUC does not leak across pooled connections;
//!   6. the store code itself (PgTransferAttemptStore::get_scoped /
//!      mark_scoped over a pool whose connections run `SET ROLE
//!      app_payments`) pins the GUC: record+get in the owning tenant works,
//!      a foreign-tenant scoped read is invisible, and the internal pool
//!      read (get_internal) sees the row.
//!
//! The module/AppState mirror below is the route_smoke.rs idiom (the crate
//! is binary-only): `#[path]`-include the store modules and mirror
//! `src/main.rs::AppState` verbatim so any drift breaks compilation here.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

use serde::Serialize;
use tracing::warn;

#[path = "../src/auth.rs"]
mod auth;
#[path = "../src/config.rs"]
mod config;
#[path = "../src/consumer.rs"]
mod consumer;
#[path = "../src/dapr.rs"]
mod dapr;
#[path = "../src/events.rs"]
mod events;
#[path = "../src/flutterwave.rs"]
mod flutterwave;
#[path = "../src/ledger/mod.rs"]
mod ledger;
#[path = "../src/mojaloop.rs"]
mod mojaloop;
#[path = "../src/payouts.rs"]
mod payouts;
#[path = "../src/registry.rs"]
mod registry;
#[path = "../src/routes.rs"]
mod routes;
#[path = "../src/tenant.rs"]
mod tenant;
#[path = "../src/transfers.rs"]
mod transfers;

use payouts::PayoutAttemptStore as _;
use transfers::TransferAttemptStore as _;

/// Verbatim mirror of `src/main.rs::http_client` (referenced as
/// `crate::http_client` by mojaloop.rs / flutterwave.rs).
pub fn http_client() -> reqwest::Client {
    reqwest::Client::builder()
        .connect_timeout(std::time::Duration::from_secs(5))
        .timeout(std::time::Duration::from_secs(30))
        .build()
        .expect("reqwest client with static timeout configuration must build")
}

/// Verbatim mirror of `src/main.rs::AppState` (the router's state type).
#[derive(Clone)]
pub struct AppState {
    pub ledger: Arc<dyn ledger::LedgerClient>,
    pub outbox: dapr::DaprOutbox,
    pub mojaloop: mojaloop::MojaloopAdapter,
    pub flutterwave: flutterwave::FlutterwaveAdapter,
    pub config: Arc<config::Config>,
    pub dlq: Arc<dyn consumer::DlqSink>,
    pub auth: auth::AuthConfig,
    pub payout_attempts: Arc<dyn payouts::PayoutAttemptStore>,
    pub registry: Arc<dyn registry::Registry>,
    pub transfer_attempts: Arc<dyn transfers::TransferAttemptStore>,
    pub ensured_tenants: Arc<std::sync::Mutex<std::collections::HashSet<String>>>,
    pub events_published: Arc<AtomicU64>,
    pub events_failed: Arc<AtomicU64>,
    pub commands_dead_lettered: Arc<AtomicU64>,
    pub commands_processed: Arc<AtomicU64>,
    pub payouts_attempted: Arc<AtomicU64>,
    pub payouts_committed: Arc<AtomicU64>,
    pub payouts_failed: Arc<AtomicU64>,
    pub payouts_unknown: Arc<AtomicU64>,
}

/// Verbatim mirror of `src/main.rs::impl AppState` (called by the handlers).
impl AppState {
    /// Mirror of `src/main.rs::ensure_accounts` (SPEC-W46 R3).
    pub async fn ensure_accounts(&self, tenant_id: &str) -> Result<(), ledger::LedgerError> {
        {
            let ensured = self
                .ensured_tenants
                .lock()
                .unwrap_or_else(|p| p.into_inner());
            if ensured.contains(tenant_id) {
                return Ok(());
            }
        }
        self.ledger.create_accounts(tenant_id).await?;
        self.ensured_tenants
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .insert(tenant_id.to_string());
        Ok(())
    }

    /// Mirror of `src/main.rs::publish_event` (SPEC-W46 R5).
    pub async fn publish_event<T: Serialize + Send + Sync + 'static>(
        &self,
        type_name: &str,
        subject: &str,
        tenant_id: &str,
        data: T,
    ) {
        let event = events::CloudEvent::new(
            "payments-service",
            &format!("com.opendesk.payments.{type_name}"),
            subject,
            tenant_id,
            data,
        );
        let outbox = self.outbox.clone();
        let events_published = self.events_published.clone();
        let events_failed = self.events_failed.clone();
        tokio::spawn(async move {
            match outbox.publish(&event).await {
                Ok(()) => {
                    events_published.fetch_add(1, Ordering::Relaxed);
                }
                Err(e) => {
                    events_failed.fetch_add(1, Ordering::Relaxed);
                    warn!(
                        error = %e,
                        type_ = %event.type_,
                        "dapr pubsub publish failed (best-effort outbox)"
                    );
                }
            }
        });
    }
}

// ---------------------------------------------------------------------------
// Harness (billing pg_hardening.rs idiom: self-skip without a DSN).
// ---------------------------------------------------------------------------

const MIGRATION: &str = include_str!("../migrations/0001_rls.sql");

fn test_dsn() -> Option<String> {
    std::env::var("PAYMENTS_TEST_DATABASE_URL")
        .ok()
        .filter(|s| !s.trim().is_empty())
}

async fn pg_pool(dsn: &str) -> sqlx::PgPool {
    let options = payouts::pg_connect_options(dsn).expect("valid test DSN");
    sqlx::postgres::PgPoolOptions::new()
        .max_connections(4)
        .connect_with(options)
        .await
        .expect("connect to test Postgres")
}

/// Apply the three stores' bootstrap DDL + the RLS migration, once per
/// process (the migration takes DDL locks; applying it concurrently from
/// parallel tests would deadlock). Returns a fresh pool per call.
async fn provision(dsn: &str) -> sqlx::PgPool {
    use tokio::sync::OnceCell;
    static READY: OnceCell<()> = OnceCell::const_new();
    let pool = pg_pool(dsn).await;
    READY
        .get_or_init(|| async {
            for ddl in [
                payouts::BOOTSTRAP_DDL,
                registry::BOOTSTRAP_DDL,
                transfers::BOOTSTRAP_DDL,
            ] {
                for stmt in ddl.split(';').map(str::trim).filter(|s| !s.is_empty()) {
                    sqlx::query(stmt)
                        .execute(&pool)
                        .await
                        .expect("bootstrap DDL applies");
                }
            }
            // The exact production boot path (src/tenant.rs).
            tenant::apply_rls_migration(&pool)
                .await
                .expect("RLS migration applies");
            // Idempotent: a second application (restart) must be a no-op.
            tenant::apply_rls_migration(&pool)
                .await
                .expect("RLS migration is idempotent");
        })
        .await;
    pool
}

/// Count rail_attempts rows for a specific id under `role` with the tenant
/// GUC set to `guc_tenant` (None = GUC unset → must be fail-closed).
async fn count_rail_as(
    pool: &sqlx::PgPool,
    role: &str,
    guc_tenant: Option<&str>,
    transfer_id: &str,
) -> i64 {
    let mut tx = pool.begin().await.expect("begin tx");
    sqlx::query(&format!("SET LOCAL ROLE {role}"))
        .execute(&mut *tx)
        .await
        .expect("SET LOCAL ROLE");
    if let Some(t) = guc_tenant {
        sqlx::query("SELECT set_config('app.tenant_id', $1, true)")
            .bind(t)
            .execute(&mut *tx)
            .await
            .expect("set GUC");
    }
    let (n,): (i64,) = sqlx::query_as("SELECT count(*) FROM rail_attempts WHERE transfer_id = $1")
        .bind(transfer_id)
        .fetch_one(&mut *tx)
        .await
        .expect("count query");
    tx.commit().await.expect("commit");
    n
}

/// DB-01 core: tenant A cannot read tenant B's rail_attempts (and the GUC
/// gates every access under the least-privilege role).
#[tokio::test]
async fn db01_rls_cross_tenant_rail_attempts_isolation() {
    let Some(dsn) = test_dsn() else {
        eprintln!("skipping: PAYMENTS_TEST_DATABASE_URL not set");
        return;
    };
    let pool = provision(&dsn).await;
    let suffix = uuid::Uuid::new_v4().simple().to_string();
    let tenant_a = format!("t-rls-a-{suffix}");
    let tenant_b = format!("t-rls-b-{suffix}");
    let id_a = format!("rls-a-{suffix}");
    let id_b = format!("rls-b-{suffix}");

    // Seed one rail attempt per tenant as the bootstrap (superuser) role.
    for (id, t) in [(&id_a, &tenant_a), (&id_b, &tenant_b)] {
        sqlx::query(
            "INSERT INTO rail_attempts
                (transfer_id, kind, tenant_id, amount_cents, currency, destination, state)
             VALUES ($1, 'transfer', $2, 100, 'NGN', 'loan:l-x', 'queued_manual')
             ON CONFLICT (transfer_id) DO NOTHING",
        )
        .bind(id)
        .bind(t)
        .execute(&pool)
        .await
        .expect("seed rail attempt");
    }

    // Migration actually enabled + forced RLS on all four money tables.
    let rows: Vec<(String, bool, bool)> = sqlx::query_as(
        "SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity
         FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public' AND c.relname IN
             ('payout_beneficiaries','deposit_provenance','payout_attempts','rail_attempts')
         ORDER BY c.relname",
    )
    .fetch_all(&pool)
    .await
    .expect("pg_class query");
    assert_eq!(rows.len(), 4, "all four money tables exist");
    for (name, rls, force) in &rows {
        assert!(rls, "{name}: RLS must be enabled");
        assert!(force, "{name}: FORCE RLS must be enabled (binds the owner)");
    }

    // Roles exist (guarded creation in the migration).
    let (roles_ok,): (bool,) = sqlx::query_as(
        "SELECT count(*) = 4 FROM pg_roles WHERE rolname IN
             ('app_payments','app_payments_login','app_payments_internal','app_payments_internal_login')",
    )
    .fetch_one(&pool)
    .await
    .expect("pg_roles probe");
    assert!(roles_ok, "migration created the payments roles");

    // Fail-closed: under the app role WITHOUT the GUC, neither tenant's row
    // is visible.
    assert_eq!(count_rail_as(&pool, "app_payments", None, &id_a).await, 0);
    // Cross-tenant: pinned to tenant B, tenant A's row is invisible (and
    // vice versa).
    assert_eq!(
        count_rail_as(&pool, "app_payments", Some(&tenant_b), &id_a).await,
        0,
        "tenant B must not read tenant A's rail_attempts"
    );
    assert_eq!(
        count_rail_as(&pool, "app_payments", Some(&tenant_a), &id_b).await,
        0,
        "tenant A must not read tenant B's rail_attempts"
    );
    // Own-tenant: pinned correctly, the row is visible.
    assert_eq!(count_rail_as(&pool, "app_payments", Some(&tenant_a), &id_a).await, 1);
    assert_eq!(count_rail_as(&pool, "app_payments", Some(&tenant_b), &id_b).await, 1);

    // WITH CHECK: pinned to tenant B, inserting a row claiming tenant A is
    // rejected by the policy.
    let mut tx = pool.begin().await.expect("begin tx");
    sqlx::query("SET LOCAL ROLE app_payments")
        .execute(&mut *tx)
        .await
        .unwrap();
    sqlx::query("SELECT set_config('app.tenant_id', $1, true)")
        .bind(&tenant_b)
        .execute(&mut *tx)
        .await
        .unwrap();
    let err = sqlx::query(
        "INSERT INTO rail_attempts
            (transfer_id, kind, tenant_id, amount_cents, currency, destination, state)
         VALUES ($1, 'transfer', $2, 100, 'NGN', 'loan:l-y', 'queued_manual')",
    )
    .bind(format!("rls-evil-{suffix}"))
    .bind(&tenant_a)
    .execute(&mut *tx)
    .await
    .expect_err("cross-tenant insert must be rejected by the WITH CHECK");
    assert!(
        err.to_string().contains("row-level security"),
        "expected RLS violation, got: {err}"
    );
    tx.rollback().await.ok();

    // The internal role escape (reconciler sweep) is gated by ROLE
    // MEMBERSHIP, not the GUC: app_payments_internal sees all tenants with
    // the GUC unset, and the app role cannot escalate by setting a magic GUC.
    assert_eq!(count_rail_as(&pool, "app_payments_internal", None, &id_a).await, 1);
    assert_eq!(count_rail_as(&pool, "app_payments_internal", None, &id_b).await, 1);
}

/// The transaction-local GUC must not leak across pooled connections: a new
/// transaction on a REUSED connection starts with the GUC unset.
#[tokio::test]
async fn db01_tenant_guc_is_transaction_local() {
    let Some(dsn) = test_dsn() else {
        eprintln!("skipping: PAYMENTS_TEST_DATABASE_URL not set");
        return;
    };
    // Single-connection pool: the second tx provably reuses the connection.
    let options = payouts::pg_connect_options(&dsn).expect("valid test DSN");
    let pool = sqlx::postgres::PgPoolOptions::new()
        .max_connections(1)
        .connect_with(options)
        .await
        .expect("connect");
    provision(&dsn).await;

    {
        let mut tx = pool.begin().await.unwrap();
        tenant::set_tenant_guc(&mut tx, "t-leak-check").await.unwrap();
        let (guc,): (String,) = sqlx::query_as("SELECT current_setting('app.tenant_id', true)")
            .fetch_one(&mut *tx)
            .await
            .unwrap();
        assert_eq!(guc, "t-leak-check");
        tx.commit().await.unwrap();
    }
    let mut tx2 = pool.begin().await.unwrap();
    let (guc,): (Option<String>,) =
        sqlx::query_as("SELECT NULLIF(current_setting('app.tenant_id', true), '')")
            .fetch_one(&mut *tx2)
            .await
            .unwrap();
    assert_eq!(guc, None, "transaction-local GUC must not leak into the next tx");
    tx2.rollback().await.ok();
}

/// Store-level proof: the real Pg store code pins the GUC correctly when its
/// connections run under the least-privilege role — scoped ops work for the
/// owning tenant, a foreign tenant is invisible, and get_internal (internal
/// pool) is the cross-tenant path.
#[tokio::test]
async fn db01_store_pins_tenant_guc_under_app_role() {
    let Some(dsn) = test_dsn() else {
        eprintln!("skipping: PAYMENTS_TEST_DATABASE_URL not set");
        return;
    };
    let su_pool = provision(&dsn).await;

    // Pool whose connections impersonate the least-privilege app role.
    let options = payouts::pg_connect_options(&dsn).expect("valid test DSN");
    let app_pool = sqlx::postgres::PgPoolOptions::new()
        .max_connections(2)
        .after_connect(|conn, _meta| {
            Box::pin(async move {
                sqlx::Executor::execute(conn, "SET ROLE app_payments").await?;
                Ok(())
            })
        })
        .connect_with(options)
        .await
        .expect("connect app-role pool");

    let suffix = uuid::Uuid::new_v4().simple().to_string();
    let tenant_a = format!("t-store-a-{suffix}");
    let tenant_b = format!("t-store-b-{suffix}");
    let id = format!("store-{suffix}");
    let attempt = transfers::TransferAttempt {
        transfer_id: id.clone(),
        kind: transfers::KIND_TRANSFER.to_string(),
        tenant_id: tenant_a.clone(),
        amount_cents: 5_000,
        currency: "NGN".to_string(),
        destination: "loan:l-1".to_string(),
        state: transfers::TransferAttemptState::QueuedManual,
        detail: None,
        created_at: chrono::Utc::now(),
        updated_at: chrono::Utc::now(),
    };

    let store = transfers::PgTransferAttemptStore::from_pool(app_pool.clone());
    // Own-tenant write + read work under RLS.
    store.record(&attempt).await.expect("record under app role");
    let got = store
        .get_scoped(&id, &tenant_a)
        .await
        .expect("scoped read")
        .expect("own row visible");
    assert_eq!(got.tenant_id, tenant_a);
    // Cross-tenant: tenant B cannot read tenant A's rail_attempts through
    // the store code path.
    assert!(
        store.get_scoped(&id, &tenant_b).await.unwrap().is_none(),
        "tenant B must not read tenant A's rail_attempts (store path)"
    );
    // Cross-tenant mark is a no-op (RLS hides the row from the UPDATE).
    store
        .mark_scoped(&id, &tenant_b, transfers::TransferAttemptState::Failed, None)
        .await
        .unwrap();
    assert_eq!(
        store.get_scoped(&id, &tenant_a).await.unwrap().unwrap().state,
        transfers::TransferAttemptState::QueuedManual,
        "foreign-tenant mark must not touch the row"
    );

    // The payout store's internal path: scoped read invisible cross-tenant,
    // internal pool (here: the superuser pool, the dev fallback posture)
    // sees the row.
    let pay_store = payouts::PgPayoutAttemptStore::from_pools(app_pool.clone(), su_pool.clone());
    let payout_id = format!("store-pay-{suffix}");
    pay_store
        .record(&payouts::PayoutAttempt {
            payout_id: payout_id.clone(),
            tenant_id: tenant_a.clone(),
            amount_cents: 7_500,
            currency: "NGN".to_string(),
            payee: serde_json::json!({"partyIdType":"ALIAS","partyIdentifier":"p-1"}),
            state: payouts::AttemptState::PendingApproval,
            detail: None,
            created_at: chrono::Utc::now(),
            updated_at: chrono::Utc::now(),
        })
        .await
        .expect("record payout attempt under app role");
    assert!(pay_store.get_scoped(&payout_id, &tenant_b).await.unwrap().is_none());
    let internal = pay_store
        .get_internal(&payout_id)
        .await
        .expect("internal read")
        .expect("internal pool sees the row (approval learn-the-tenant path)");
    assert_eq!(internal.tenant_id, tenant_a);
}
