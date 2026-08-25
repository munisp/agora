//! SPEC-W43 (B-01..B-07) integration tests against a REAL Postgres.
//!
//! The crate is binary-only, so the production modules are compiled into
//! this test crate via `#[path]` (same idiom as route_smoke.rs), the REAL
//! router is served over TCP, the REAL durable PgLedgerClient does the
//! ledger postings, and the Paystack webhook is exercised with a correctly
//! computed HMAC-SHA512 signature.
//!
//! Requires env `BILLING_TEST_DATABASE_URL` pointing at a Postgres database
//! the test owns (the pgserver-backed driver sets it; see the wave W43
//! evidence). When the variable is unset every test skips itself so a plain
//! `cargo test` stays hermetic — the migration/RLS/DELETE-privilege checks
//! below MUST be run against a live database to mean anything.

use std::sync::atomic::AtomicU64;
use std::sync::Arc;

use sqlx::{PgPool, Row};
use uuid::Uuid;

#[path = "../src/config.rs"]
mod config;
#[path = "../src/consumer.rs"]
mod consumer;
#[path = "../src/identity.rs"]
mod identity;
#[path = "../src/invoices.rs"]
mod invoices;
#[path = "../src/ledger.rs"]
mod ledger;
#[path = "../src/metering.rs"]
mod metering;
#[path = "../src/models.rs"]
mod models;
#[path = "../src/outbox.rs"]
mod outbox;
#[path = "../src/payments_qr.rs"]
mod payments_qr;
#[path = "../src/routes.rs"]
mod routes;
#[path = "../src/tenant.rs"]
mod tenant;

use ledger::BillingLedger;

/// Mirror of `src/main.rs::http_client`.
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
    pub pool: sqlx::PgPool,
    pub internal_pool: sqlx::PgPool,
    pub ledger: Arc<dyn ledger::BillingLedger>,
    pub producer: Option<rdkafka::producer::FutureProducer>,
    pub http: reqwest::Client,
    pub config: Arc<config::Config>,
    pub identity: Arc<identity::SlugResolver>,
    pub outbox_notify: Arc<tokio::sync::Notify>,
    pub events_published: Arc<AtomicU64>,
    pub events_failed: Arc<AtomicU64>,
    pub usage_dead_lettered: Arc<AtomicU64>,
    pub usage_processed: Arc<AtomicU64>,
    pub dlq: Arc<dyn consumer::DlqSink>,
}

const INTERNAL_TOKEN: &str = "pg-hardening-internal-token";
const PAYSTACK_SECRET: &str = "sk_test_pg_hardening_secret";

fn test_config(database_url: &str) -> config::Config {
    config::Config {
        port: 0,
        database_url: database_url.to_string(),
        internal_database_url: None,
        kafka_brokers: "127.0.0.1:1".to_string(),
        kafka_group_id: "pg-hardening".to_string(),
        usage_events_topic: "opendesk.usage.events".to_string(),
        kafka_consumer_enabled: false,
        billing_events_topic: "opendesk.billing.events".to_string(),
        dlq_topic: "opendesk.dlq".to_string(),
        billing_ledger_impl: "postgres".to_string(),
        internal_token: INTERNAL_TOKEN.to_string(),
        paystack_secret_key: Some(PAYSTACK_SECRET.to_string()),
        paystack_default_email: "pg@example.com".to_string(),
        paystack_callback_url: "http://127.0.0.1/callback".to_string(),
        billing_static_account: "PG/0000000000".to_string(),
        billing_merchant_name: "PG Hardening".to_string(),
        money_roles: vec!["owner".to_string(), "admin".to_string()],
        trust_direct_tenant: false,
        // Identity slug->uuid resolution disabled here: this harness's K1
        // tests bind via uuid-string slug claims (direct match, no network).
        // Slug-resolution against a mock identity + real PG is covered by
        // tests/pg_k1_binding.rs.
        identity_base_url: String::new(),
        identity_internal_token: None,
        tenant_cache_ttl_s: 60,
        dunning_interval_s: 3600,
        invoice_due_days: 14,
    }
}

struct Harness {
    base: String,
    pool: PgPool,
    ledger: Arc<ledger::PgLedgerClient>,
    http: reqwest::Client,
}

/// Connect, apply ALL migrations (0001..0005, the boot path incl. B-07), and
/// serve the real router behind a TCP listener. Returns None (test skips)
/// when BILLING_TEST_DATABASE_URL is unset.
/// Build connect options from BILLING_TEST_DATABASE_URL. The pgserver-backed
/// Postgres is socket-only, so the URL may carry the socket dir as a query
/// parameter (`postgresql://user@/db?host=/path/to/socketdir`), which sqlx
/// 0.7's URL parser rejects — handle that form explicitly via `.socket()`.
fn connect_options(url: &str) -> sqlx::postgres::PgConnectOptions {
    use sqlx::postgres::PgConnectOptions;
    use std::str::FromStr;
    if let Some((base, socket_dir)) = url.split_once("?host=") {
        let rest = base
            .trim_start_matches("postgresql://")
            .trim_start_matches("postgres://");
        let (creds, db) = rest.split_once('@').expect("user@ in DSN");
        let user = creds.split(':').next().unwrap_or("postgres");
        return PgConnectOptions::new()
            .username(user)
            .socket(socket_dir)
            .database(db.trim_start_matches('/'));
    }
    PgConnectOptions::from_str(url).expect("parse BILLING_TEST_DATABASE_URL")
}

/// All migrations (0001..0005, the boot path incl. B-07), in order.
pub const MIGRATIONS: [&str; 5] = [
    include_str!("../migrations/0001_init.sql"),
    include_str!("../migrations/0002_rls.sql"),
    include_str!("../migrations/0003_ledger.sql"),
    include_str!("../migrations/0004_outbox.sql"),
    include_str!("../migrations/0005_hardening.sql"),
];

/// Migrations are applied exactly once per test process (concurrent
/// application of the same DDL races in the system catalogs).
static MIGRATED: tokio::sync::OnceCell<()> = tokio::sync::OnceCell::const_new();

async fn apply_migrations(pool: &PgPool) {
    MIGRATED
        .get_or_init(|| async {
            for m in MIGRATIONS {
                sqlx::raw_sql(m)
                    .execute(pool)
                    .await
                    .expect("migration applies clean");
            }
        })
        .await;
}

async fn harness() -> Option<Harness> {
    let url = std::env::var("BILLING_TEST_DATABASE_URL").ok()?;
    let pool = PgPool::connect_with(connect_options(&url))
        .await
        .expect("connect test database");
    apply_migrations(&pool).await;
    let ledger = Arc::new(
        ledger::PgLedgerClient::new(pool.clone())
            .await
            .expect("pg ledger builds"),
    );
    let state = AppState {
        pool: pool.clone(),
        internal_pool: pool.clone(),
        ledger: ledger.clone(),
        producer: None,
        http: http_client(),
        identity: Arc::new(identity::SlugResolver::new(
            http_client(),
            "",
            None,
            std::time::Duration::from_secs(60),
        )),
        config: Arc::new(test_config(&url)),
        outbox_notify: Arc::new(tokio::sync::Notify::new()),
        events_published: Arc::new(AtomicU64::new(0)),
        events_failed: Arc::new(AtomicU64::new(0)),
        usage_dead_lettered: Arc::new(AtomicU64::new(0)),
        usage_processed: Arc::new(AtomicU64::new(0)),
        dlq: Arc::new(consumer::UnavailableDlqSink),
    };
    let app = routes::router(state);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind ephemeral port");
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.expect("serve router");
    });
    Some(Harness {
        base: format!("http://{addr}"),
        pool,
        ledger,
        http: http_client(),
    })
}

macro_rules! require_harness {
    () => {
        match harness().await {
            Some(h) => h,
            None => {
                eprintln!("BILLING_TEST_DATABASE_URL unset; skipping pg integration test");
                return;
            }
        }
    };
}

// ---------------------------------------------------------------------------
// Fixture helpers (real HTTP against the served router)
// ---------------------------------------------------------------------------

async fn put_rate_card(h: &Harness, tenant: Uuid, metric: &str, price: i64, currency: &str) -> reqwest::Response {
    h.http
        .put(format!("{}/v1/rate-cards/{tenant}", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .header("x-tenant-id", tenant.to_string())
        .json(&serde_json::json!({
            "metric": metric,
            "unit_price_cents": price,
            "included_quota": 0,
            "currency": currency,
        }))
        .send()
        .await
        .unwrap()
}

async fn record_usage(h: &Harness, tenant: Uuid, event_id: &str, metric: &str, value: i64) {
    let event = models::RawCloudEvent {
        id: event_id.to_string(),
        type_: "com.opendesk.usage.UsageRecord".to_string(),
        data: serde_json::json!({
            "tenant_id": tenant,
            "metric": metric,
            "value": value,
            "ts": "2026-03-14T10:15:00Z",
        }),
    };
    let outcome = metering::record_usage(&h.pool, &event).await.unwrap();
    assert_eq!(outcome, metering::UsageOutcome::Recorded);
}

async fn generate(h: &Harness, tenant: Uuid, period: &str) -> (reqwest::StatusCode, serde_json::Value) {
    let resp = h
        .http
        .post(format!("{}/v1/invoices/generate", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .header("x-tenant-id", tenant.to_string())
        .header("x-user-roles", "owner")
        .json(&serde_json::json!({ "tenant_id": tenant, "period": period }))
        .send()
        .await
        .unwrap();
    let status = resp.status();
    let body = resp.json().await.unwrap_or(serde_json::Value::Null);
    (status, body)
}

async fn post_action(h: &Harness, tenant: Uuid, id: Uuid, action: &str) -> serde_json::Value {
    let resp = h
        .http
        .post(format!("{}/v1/invoices/{id}/{action}", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .header("x-tenant-id", tenant.to_string())
        .header("x-user-roles", "owner")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK, "{action} failed");
    resp.json().await.unwrap()
}

async fn get_invoice(h: &Harness, tenant: Uuid, id: Uuid) -> serde_json::Value {
    let resp = h
        .http
        .get(format!("{}/v1/invoices/{id}", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .header("x-tenant-id", tenant.to_string())
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK);
    resp.json().await.unwrap()
}

fn hex_lower(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// POST a charge.success webhook with a correctly computed HMAC signature.
async fn webhook(
    h: &Harness,
    invoice_id: Uuid,
    amount: i64,
    currency: &str,
) -> (reqwest::StatusCode, serde_json::Value) {
    let body = serde_json::json!({
        "event": "charge.success",
        "data": {
            "reference": invoice_id.to_string(),
            "amount": amount,
            "currency": currency,
            "status": "success",
        }
    });
    let raw = serde_json::to_vec(&body).unwrap();
    let sig = hex_lower(&payments_qr::hmac_sha512(PAYSTACK_SECRET.as_bytes(), &raw));
    let resp = h
        .http
        .post(format!("{}/webhooks/paystack", h.base))
        .header("x-paystack-signature", sig)
        .header("content-type", "application/json")
        .body(raw)
        .send()
        .await
        .unwrap();
    let status = resp.status();
    let json = resp.json().await.unwrap_or(serde_json::Value::Null);
    (status, json)
}

/// Outbox event types recorded for one tenant (event_key = tenant id).
async fn outbox_types(h: &Harness, tenant: Uuid) -> Vec<String> {
    let rows = sqlx::query(
        "SELECT payload->>'type' AS ty FROM billing_outbox WHERE event_key = $1 ORDER BY created_at",
    )
    .bind(tenant.to_string())
    .fetch_all(&h.pool)
    .await
    .unwrap();
    rows.iter().map(|r| r.try_get::<String, _>("ty").unwrap()).collect()
}

/// Per-tenant ledger balances as (ar_net, revenue_net) where
/// net = credits_posted - debits_posted (ledger.rs AccountBalance.posted_net):
/// an outstanding receivable reads AR = -X (DR balance), revenue = +X.
async fn ledger_nets(h: &Harness, tenant: Uuid) -> (i128, i128) {
    let bal = h.ledger.balance(&tenant.to_string()).await.unwrap();
    let ar = bal
        .accounts
        .iter()
        .find(|a| a.account == ledger::ar_account(&tenant.to_string()))
        .map(|a| a.posted_net)
        .unwrap_or(0);
    let rev = bal
        .accounts
        .iter()
        .find(|a| a.account == ledger::revenue_account(&tenant.to_string()))
        .map(|a| a.posted_net)
        .unwrap_or(0);
    (ar, rev)
}

async fn transfer_count(h: &Harness, tenant: Uuid) -> i64 {
    let ar = ledger::ar_account(&tenant.to_string());
    let rev = ledger::revenue_account(&tenant.to_string());
    sqlx::query_scalar(
        "SELECT COUNT(*) FROM ledger_transfers \
         WHERE debit_account IN ($1, $2) OR credit_account IN ($1, $2)",
    )
    .bind(&ar)
    .bind(&rev)
    .fetch_one(&h.pool)
    .await
    .unwrap()
}

/// Provision a tenant with one NGN rate card and usage, then generate +
/// issue the 2026-03 invoice; returns (invoice_id, subtotal_cents).
async fn issued_invoice(h: &Harness, period: &str) -> (Uuid, Uuid, i64) {
    let tenant = Uuid::new_v4();
    let resp = put_rate_card(h, tenant, "booking", 5_000, "NGN").await;
    assert_eq!(resp.status(), reqwest::StatusCode::OK);
    record_usage(h, tenant, &format!("usage-{tenant}"), "booking", 2).await;
    let (status, inv) = generate(h, tenant, period).await;
    assert_eq!(status, reqwest::StatusCode::CREATED, "generate failed: {inv}");
    let id = Uuid::parse_str(inv["id"].as_str().unwrap()).unwrap();
    let subtotal = inv["subtotal_cents"].as_i64().unwrap();
    assert_eq!(subtotal, 10_000);
    post_action(h, tenant, id, "issue").await;
    (tenant, id, subtotal)
}

// ---------------------------------------------------------------------------
// B-01: webhook amount/currency verification
// ---------------------------------------------------------------------------

#[tokio::test]
async fn b01_underpayment_never_settles_invoice() {
    let h = require_harness!();
    let (tenant, id, subtotal) = issued_invoice(&h, "2026-03").await;

    // Underpayment: 9999 of the 10000 owed.
    let (status, body) = webhook(&h, id, subtotal - 1, "NGN").await;
    assert_eq!(status, reqwest::StatusCode::ACCEPTED, "mismatch must be 202");
    assert_eq!(body["status"], "payment_mismatch");

    // No transition: the invoice is still issued.
    let inv = get_invoice(&h, tenant, id).await;
    assert_eq!(inv["status"], "issued", "underpayment must not settle");

    // The mismatch is recorded durably (not silently absorbed).
    let types = outbox_types(&h, tenant).await;
    assert!(
        types.iter().any(|t| t == "com.opendesk.billing.PaymentMismatch"),
        "payment_mismatch outbox event expected, got {types:?}"
    );
    assert!(
        !types.iter().any(|t| t == "com.opendesk.billing.InvoicePaid"),
        "no InvoicePaid event may be emitted for a mismatch"
    );

    // Ledger untouched by the mismatch: AR still carries the receivable.
    let (ar, rev) = ledger_nets(&h, tenant).await;
    assert_eq!((ar, rev), (-10_000, 10_000));
}

#[tokio::test]
async fn b01_wrong_currency_never_settles_invoice() {
    let h = require_harness!();
    let (tenant, id, subtotal) = issued_invoice(&h, "2026-03").await;

    let (status, body) = webhook(&h, id, subtotal, "USD").await;
    assert_eq!(status, reqwest::StatusCode::ACCEPTED);
    assert_eq!(body["status"], "payment_mismatch");

    let inv = get_invoice(&h, tenant, id).await;
    assert_eq!(inv["status"], "issued", "wrong currency must not settle");
    let types = outbox_types(&h, tenant).await;
    assert!(types.iter().any(|t| t == "com.opendesk.billing.PaymentMismatch"));
}

#[tokio::test]
async fn b01_correct_amount_and_currency_pays() {
    let h = require_harness!();
    let (tenant, id, subtotal) = issued_invoice(&h, "2026-03").await;

    let (status, body) = webhook(&h, id, subtotal, "NGN").await;
    assert_eq!(status, reqwest::StatusCode::OK, "correct payment must be 200");
    assert_eq!(body["status"], "paid");

    let inv = get_invoice(&h, tenant, id).await;
    assert_eq!(inv["status"], "paid");
    assert!(inv["paid_at"].is_string(), "paid_at set");

    // InvoicePaid event + ledger paid posting (B-03: committed atomically).
    let types = outbox_types(&h, tenant).await;
    assert!(types.iter().any(|t| t == "com.opendesk.billing.InvoicePaid"));
    let (ar, rev) = ledger_nets(&h, tenant).await;
    assert_eq!((ar, rev), (0, 10_000), "AR cleared, revenue kept");
    let paid_rows: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM ledger_transfers WHERE code = $1 AND credit_account = $2",
    )
    .bind(i32::from(ledger::CODE_INVOICE_PAID))
    .bind(ledger::ar_account(&tenant.to_string()))
    .fetch_one(&h.pool)
    .await
    .unwrap();
    assert_eq!(paid_rows, 1, "exactly one paid posting for the invoice");
}

#[tokio::test]
async fn b01_webhook_on_void_invoice_is_ignored_not_retried() {
    let h = require_harness!();
    let (tenant, id, subtotal) = issued_invoice(&h, "2026-03").await;
    post_action(&h, tenant, id, "void").await;

    let (status, body) = webhook(&h, id, subtotal, "NGN").await;
    assert_eq!(
        status,
        reqwest::StatusCode::OK,
        "void-invoice webhook must be acked 200 (stop the provider retry storm)"
    );
    assert_eq!(body["status"], "ignored");
    assert_eq!(body["reason"], "invoice_void");

    let inv = get_invoice(&h, tenant, id).await;
    assert_eq!(inv["status"], "void", "void is terminal");
    let types = outbox_types(&h, tenant).await;
    assert!(
        !types.iter().any(|t| t == "com.opendesk.billing.InvoicePaid"),
        "no paid event for a voided invoice"
    );
}

// ---------------------------------------------------------------------------
// B-06: duplicate charge.success on a paid invoice
// ---------------------------------------------------------------------------

#[tokio::test]
async fn b06_duplicate_charge_success_is_flagged_not_absorbed() {
    let h = require_harness!();
    let (tenant, id, subtotal) = issued_invoice(&h, "2026-03").await;

    let (status, body) = webhook(&h, id, subtotal, "NGN").await;
    assert_eq!((status, body["status"].as_str().unwrap()), (reqwest::StatusCode::OK, "paid"));

    // Provider redelivery of the same charge.success.
    let (status, body) = webhook(&h, id, subtotal, "NGN").await;
    assert_eq!(status, reqwest::StatusCode::OK, "duplicate stays a 200 replay");
    assert_eq!(body["status"], "already_paid");

    // ...but it is flagged via a durable DuplicatePaymentIgnored event that
    // carries the provider reference (no silent absorb).
    let rows = sqlx::query(
        "SELECT payload->>'type' AS ty, payload->'data'->>'paystackReference' AS pref \
         FROM billing_outbox WHERE event_key = $1 ORDER BY created_at",
    )
    .bind(tenant.to_string())
    .fetch_all(&h.pool)
    .await
    .unwrap();
    let types: Vec<String> = rows
        .iter()
        .map(|r| r.try_get::<String, _>("ty").unwrap())
        .collect();
    let dup = rows
        .iter()
        .find(|r| r.try_get::<String, _>("ty").unwrap() == "com.opendesk.billing.DuplicatePaymentIgnored")
        .expect("DuplicatePaymentIgnored event expected");
    assert_eq!(
        dup.try_get::<String, _>("pref").unwrap(),
        id.to_string(),
        "the duplicate event carries the provider reference"
    );
    assert_eq!(
        types.iter().filter(|t| *t == "com.opendesk.billing.InvoicePaid").count(),
        1,
        "exactly one InvoicePaid event despite the redelivery"
    );

    // Ledger: the paid posting is idempotent — AR nets to zero exactly once.
    let (ar, rev) = ledger_nets(&h, tenant).await;
    assert_eq!((ar, rev), (0, 10_000));
}

// ---------------------------------------------------------------------------
// B-02 (+B-03): void reversal
// ---------------------------------------------------------------------------

#[tokio::test]
async fn b02_void_from_issued_posts_reversal_and_event() {
    let h = require_harness!();
    let (tenant, id, _subtotal) = issued_invoice(&h, "2026-03").await;
    let (ar, rev) = ledger_nets(&h, tenant).await;
    assert_eq!((ar, rev), (-10_000, 10_000), "issued posts the receivable");

    let voided = post_action(&h, tenant, id, "void").await;
    assert_eq!(voided["status"], "void");

    // Reversing entry DR revenue / CR AR: both accounts net back to zero.
    let (ar, rev) = ledger_nets(&h, tenant).await;
    assert_eq!((ar, rev), (0, 0), "void reverses the issued posting");
    assert_eq!(transfer_count(&h, tenant).await, 2, "issued + reversal");

    let types = outbox_types(&h, tenant).await;
    assert!(
        types.iter().any(|t| t == "com.opendesk.billing.InvoiceVoided"),
        "InvoiceVoided outbox event expected, got {types:?}"
    );

    // Void replay via the ledger helper is idempotent (deterministic key).
    h.ledger
        .invoice_voided(&tenant.to_string(), id, 10_000)
        .await
        .unwrap();
    let (ar, rev) = ledger_nets(&h, tenant).await;
    assert_eq!((ar, rev), (0, 0), "replayed reversal must not double-post");
    assert_eq!(transfer_count(&h, tenant).await, 2);
}

#[tokio::test]
async fn b02_issue_void_regenerate_issue_does_not_double_count() {
    let h = require_harness!();
    let (tenant, id1, _) = issued_invoice(&h, "2026-03").await;
    post_action(&h, tenant, id1, "void").await;

    // Regenerate replaces the voided invoice with a NEW draft (same period).
    let (status, inv2) = generate(&h, tenant, "2026-03").await;
    assert_eq!(status, reqwest::StatusCode::CREATED, "regenerate: {inv2}");
    let id2 = Uuid::parse_str(inv2["id"].as_str().unwrap()).unwrap();
    assert_ne!(id1, id2, "regenerate after void creates a fresh invoice");
    post_action(&h, tenant, id2, "issue").await;

    // Exactly three postings: issued(1), void-reversal(1), re-issued(2).
    assert_eq!(transfer_count(&h, tenant).await, 3, "no double counting");
    // Net position: one live receivable of 10_000, revenue of 10_000 — not
    // 20_000 (double count) and not 0 (lost re-issue).
    let (ar, rev) = ledger_nets(&h, tenant).await;
    assert_eq!((ar, rev), (-10_000, 10_000));
}

#[tokio::test]
async fn b02_void_from_draft_stays_free() {
    let h = require_harness!();
    let tenant = Uuid::new_v4();
    let resp = put_rate_card(&h, tenant, "booking", 5_000, "NGN").await;
    assert_eq!(resp.status(), reqwest::StatusCode::OK);
    record_usage(&h, tenant, &format!("usage-{tenant}"), "booking", 2).await;
    let (status, inv) = generate(&h, tenant, "2026-03").await;
    assert_eq!(status, reqwest::StatusCode::CREATED);
    let id = Uuid::parse_str(inv["id"].as_str().unwrap()).unwrap();

    // Void directly from draft: no reversal, no event, no ledger rows.
    let voided = post_action(&h, tenant, id, "void").await;
    assert_eq!(voided["status"], "void");
    assert_eq!(transfer_count(&h, tenant).await, 0, "draft void stays free");
    let types = outbox_types(&h, tenant).await;
    assert!(
        !types.iter().any(|t| t == "com.opendesk.billing.InvoiceVoided"),
        "no InvoiceVoided event for a draft void"
    );
    let (ar, rev) = ledger_nets(&h, tenant).await;
    assert_eq!((ar, rev), (0, 0));
}

// ---------------------------------------------------------------------------
// B-05: single-currency rate cards
// ---------------------------------------------------------------------------

#[tokio::test]
async fn b05_rate_card_upsert_rejects_conflicting_currency() {
    let h = require_harness!();
    let tenant = Uuid::new_v4();

    let resp = put_rate_card(&h, tenant, "booking", 5_000, "NGN").await;
    assert_eq!(resp.status(), reqwest::StatusCode::OK);

    // A second metric in a DIFFERENT currency is rejected 409.
    let resp = put_rate_card(&h, tenant, "seat", 1_000, "USD").await;
    assert_eq!(
        resp.status(),
        reqwest::StatusCode::CONFLICT,
        "mixed-currency card must be rejected"
    );

    // Same currency for another metric is fine, and updating the existing
    // card in place (same currency) is fine.
    let resp = put_rate_card(&h, tenant, "seat", 1_000, "NGN").await;
    assert_eq!(resp.status(), reqwest::StatusCode::OK);
    let resp = put_rate_card(&h, tenant, "booking", 6_000, "NGN").await;
    assert_eq!(resp.status(), reqwest::StatusCode::OK);

    // Even re-pointing the EXISTING metric at another currency conflicts
    // while a second card in NGN exists.
    let resp = put_rate_card(&h, tenant, "booking", 6_000, "GHS").await;
    assert_eq!(resp.status(), reqwest::StatusCode::CONFLICT);
}

#[tokio::test]
async fn b05_generate_refuses_legacy_mixed_currency_cards() {
    let h = require_harness!();
    let tenant = Uuid::new_v4();

    // Simulate pre-gate rows: insert mixed-currency cards directly in SQL
    // (the HTTP gate added by B-05 makes this unreachable via the API).
    let mut tx = tenant::begin_tenant_tx(&h.pool, tenant).await.unwrap();
    for (metric, currency) in [("booking", "NGN"), ("seat", "USD")] {
        sqlx::query(
            "INSERT INTO rate_cards (tenant_id, metric, unit_price_cents, included_quota, currency) \
             VALUES ($1, $2, 100, 0, $3)",
        )
        .bind(tenant)
        .bind(metric)
        .bind(currency)
        .execute(&mut *tx)
        .await
        .unwrap();
    }
    tx.commit().await.unwrap();
    record_usage(&h, tenant, &format!("usage-{tenant}-a"), "booking", 1).await;
    record_usage(&h, tenant, &format!("usage-{tenant}-b"), "seat", 1).await;

    let (status, _body) = generate(&h, tenant, "2026-03").await;
    assert_eq!(
        status,
        reqwest::StatusCode::INTERNAL_SERVER_ERROR,
        "generate() must defensively refuse mixed currencies"
    );
    // Nothing persisted: the failed generate must not leave a draft behind
    // (the transaction rolled back).
    let drafts: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM invoices WHERE tenant_id = $1",
    )
    .bind(tenant)
    .fetch_one(&h.pool)
    .await
    .unwrap();
    assert_eq!(drafts, 0, "refused generate leaves no invoice row");
}

// ---------------------------------------------------------------------------
// B-04: consumer process_payload against a real database (success path;
// failure/DLQ paths are unit-tested in src/consumer.rs)
// ---------------------------------------------------------------------------

#[tokio::test]
async fn b04_consumer_process_payload_records_and_dedupes() {
    let h = require_harness!();
    let state = AppState {
        pool: h.pool.clone(),
        internal_pool: h.pool.clone(),
        ledger: h.ledger.clone(),
        producer: None,
        http: http_client(),
        config: Arc::new(test_config(&std::env::var("BILLING_TEST_DATABASE_URL").unwrap())),
        identity: Arc::new(identity::SlugResolver::new(
            http_client(),
            "",
            None,
            std::time::Duration::from_secs(60),
        )),
        outbox_notify: Arc::new(tokio::sync::Notify::new()),
        events_published: Arc::new(AtomicU64::new(0)),
        events_failed: Arc::new(AtomicU64::new(0)),
        usage_dead_lettered: Arc::new(AtomicU64::new(0)),
        usage_processed: Arc::new(AtomicU64::new(0)),
        dlq: Arc::new(consumer::UnavailableDlqSink),
    };
    let tenant = Uuid::new_v4();
    let event_id = format!("consumer-{tenant}");
    let payload = serde_json::to_vec(&serde_json::json!({
        "id": event_id,
        "type": "com.opendesk.usage.UsageRecord",
        "data": {
            "tenant_id": tenant,
            "metric": "booking",
            "value": 3,
            "ts": "2026-03-14T10:15:00Z",
        }
    }))
    .unwrap();

    let outcome = consumer::process_payload(&state, None, &payload, "opendesk.usage.events").await;
    assert_eq!(outcome, consumer::ProcessOutcome::Processed);
    // Redelivery is positively identified as a duplicate and also commits.
    let outcome = consumer::process_payload(&state, None, &payload, "opendesk.usage.events").await;
    assert_eq!(outcome, consumer::ProcessOutcome::Processed);

    let total: i64 = sqlx::query_scalar(
        "SELECT CAST(COALESCE(SUM(value), 0) AS bigint) FROM usage_records WHERE tenant_id = $1",
    )
    .bind(tenant)
    .fetch_one(&h.pool)
    .await
    .unwrap();
    assert_eq!(total, 3, "recorded exactly once despite redelivery");
    assert_eq!(
        state.usage_dead_lettered.load(std::sync::atomic::Ordering::Relaxed),
        0,
        "no DLQ traffic on the success path"
    );
}

// ---------------------------------------------------------------------------
// B-07: migration 0005 hardening assertions
// ---------------------------------------------------------------------------

#[tokio::test]
async fn b07_outbox_rls_internal_role_gated() {
    let h = require_harness!();
    // Seed an outbox row (any tenant) so the policy has something to hide.
    let (tenant, id, subtotal) = issued_invoice(&h, "2026-03").await;
    let (status, _) = webhook(&h, id, subtotal, "NGN").await;
    assert_eq!(status, reqwest::StatusCode::OK);

    // FORCE RLS is on for billing_outbox.
    let (rls, forced): (bool, bool) = sqlx::query(
        "SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'billing_outbox'",
    )
    .fetch_one(&h.pool)
    .await
    .map(|r| (r.try_get(0).unwrap(), r.try_get(1).unwrap()))
    .unwrap();
    assert!(rls && forced, "billing_outbox must carry FORCE RLS");

    // As the app role with NO tenant GUC: zero rows (fail-closed).
    let mut tx = h.pool.begin().await.unwrap();
    sqlx::query("SET LOCAL ROLE app_billing").execute(&mut *tx).await.unwrap();
    let hidden: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM billing_outbox")
        .fetch_one(&mut *tx)
        .await
        .unwrap();
    assert_eq!(hidden, 0, "app role without tenant GUC sees no outbox rows");
    // With the tenant GUC set, the app role can write+read (enqueue path).
    sqlx::query("SELECT set_config('app.tenant_id', $1, true)")
        .bind(tenant.to_string())
        .execute(&mut *tx)
        .await
        .unwrap();
    let visible: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM billing_outbox")
        .fetch_one(&mut *tx)
        .await
        .unwrap();
    assert!(visible >= 1, "tenant-scoped tx sees the outbox");
    tx.rollback().await.unwrap();

    // The internal role (relay / webhook lookup) bypasses the GUC gate.
    let mut tx = h.pool.begin().await.unwrap();
    sqlx::query("SET LOCAL ROLE app_billing_internal")
        .execute(&mut *tx)
        .await
        .unwrap();
    let visible: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM billing_outbox")
        .fetch_one(&mut *tx)
        .await
        .unwrap();
    assert!(visible >= 1, "internal role reads the outbox without a GUC");
    tx.rollback().await.unwrap();
}

#[tokio::test]
async fn b07_app_roles_cannot_delete_ledger_rows() {
    let h = require_harness!();
    let (tenant, id, _subtotal) = issued_invoice(&h, "2026-03").await;
    let _ = (tenant, id);

    for role in ["app_billing", "app_billing_internal"] {
        let has_delete: bool =
            sqlx::query_scalar("SELECT has_table_privilege($1, 'ledger_transfers', 'DELETE')")
                .bind(role)
                .fetch_one(&h.pool)
                .await
                .unwrap();
        assert!(!has_delete, "{role} must NOT hold DELETE on ledger_transfers");
        let has_delete: bool =
            sqlx::query_scalar("SELECT has_table_privilege($1, 'ledger_accounts', 'DELETE')")
                .bind(role)
                .fetch_one(&h.pool)
                .await
                .unwrap();
        assert!(!has_delete, "{role} must NOT hold DELETE on ledger_accounts");
    }

    // Live check: an actual DELETE as the app role is refused.
    let mut tx = h.pool.begin().await.unwrap();
    sqlx::query("SET LOCAL ROLE app_billing").execute(&mut *tx).await.unwrap();
    let err = sqlx::query("DELETE FROM ledger_transfers")
        .execute(&mut *tx)
        .await
        .expect_err("DELETE as app_billing must be refused");
    assert!(
        err.to_string().contains("permission denied"),
        "expected permission denied, got: {err}"
    );
    tx.rollback().await.unwrap();
}

#[tokio::test]
async fn b07_invoice_period_format_checked() {
    let h = require_harness!();
    let tenant = Uuid::new_v4();

    // Bad periods are rejected by the CHECK constraint (defense in depth
    // behind the API-layer parse_period validation).
    for bad in ["2026-13", "2026-00", "26-03", "2026-3", "2026/03"] {
        let err = sqlx::query(
            "INSERT INTO invoices (tenant_id, period, status, subtotal_cents, currency, line_items) \
             VALUES ($1, $2, 'draft', 0, 'NGN', '[]'::jsonb)",
        )
        .bind(tenant)
        .bind(bad)
        .execute(&h.pool)
        .await
        .expect_err("bad period must be rejected");
        assert!(
            err.to_string().contains("invoices_period_format"),
            "expected the period CHECK to fire for '{bad}', got: {err}"
        );
    }
    // A well-formed period inserts fine.
    sqlx::query(
        "INSERT INTO invoices (tenant_id, period, status, subtotal_cents, currency, line_items) \
         VALUES ($1, '2026-12', 'void', 0, 'NGN', '[]'::jsonb)",
    )
    .bind(tenant)
    .execute(&h.pool)
    .await
    .expect("well-formed period inserts");

    // The constraint exists and is validated (not merely NOT VALID).
    let validated: bool = sqlx::query_scalar(
        "SELECT convalidated FROM pg_constraint WHERE conname = 'invoices_period_format'",
    )
    .fetch_one(&h.pool)
    .await
    .unwrap();
    assert!(validated, "period CHECK must be VALIDATEd after NOT VALID add");
}

/// Boot-path idempotency: applying every migration twice (the service does
/// this on every restart) must succeed and keep the hardening intact. Runs
/// against its own scratch database so the re-applied DDL cannot deadlock
/// against the other tests' concurrent DML.
#[tokio::test]
async fn b07_migrations_are_idempotent_on_reapply() {
    let Some(url) = std::env::var("BILLING_TEST_DATABASE_URL").ok() else {
        eprintln!("BILLING_TEST_DATABASE_URL unset; skipping pg integration test");
        return;
    };
    let admin = PgPool::connect_with(connect_options(&url))
        .await
        .expect("connect admin database");
    let scratch = format!("billing_reapply_{}", Uuid::new_v4().simple());
    sqlx::query(&format!("CREATE DATABASE {scratch}"))
        .execute(&admin)
        .await
        .expect("create scratch database");
    let opts = connect_options(&url).database(&scratch);
    let pool = PgPool::connect_with(opts).await.expect("connect scratch db");
    for _round in 0..2 {
        for m in MIGRATIONS {
            sqlx::raw_sql(m)
                .execute(&pool)
                .await
                .expect("applying migrations twice must be a no-op");
        }
    }
    let (rls, forced): (bool, bool) = sqlx::query(
        "SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'billing_outbox'",
    )
    .fetch_one(&pool)
    .await
    .map(|r| (r.try_get(0).unwrap(), r.try_get(1).unwrap()))
    .unwrap();
    assert!(rls && forced);
    let has_delete: bool =
        sqlx::query_scalar("SELECT has_table_privilege('app_billing', 'ledger_transfers', 'DELETE')")
            .fetch_one(&pool)
            .await
            .unwrap();
    assert!(!has_delete, "REVOKE survives re-application ordering");
    let validated: bool = sqlx::query_scalar(
        "SELECT convalidated FROM pg_constraint WHERE conname = 'invoices_period_format'",
    )
    .fetch_one(&pool)
    .await
    .unwrap();
    assert!(validated, "period CHECK stays VALIDATEd after re-apply");
    pool.close().await;
    sqlx::query(&format!("DROP DATABASE {scratch}"))
        .execute(&admin)
        .await
        .expect("drop scratch database");
}

// ---------------------------------------------------------------------------
// SPEC-W44 K1/K6: gateway (human) auth path against real PG — no internal
// token; the tenant param must bind to X-Tenant-Slugs and mutations require
// a money role from X-User-Roles.
// ---------------------------------------------------------------------------

/// K1+K6 end-to-end: a gateway caller whose slugs claim lists the tenant and
/// who carries a money role generates + issues an invoice; a member without
/// a money role is 403 on mutations but may read; a caller whose slugs do
/// NOT list the tenant is 403 (and sees no cross-tenant data).
#[tokio::test]
async fn k1_gateway_binding_and_k6_roles_against_real_pg() {
    let h = require_harness!();
    let tenant = Uuid::new_v4();

    // Seed billable state via the internal-token service path (K2).
    let resp = put_rate_card(&h, tenant, "seat", 500, "NGN").await;
    assert_eq!(resp.status(), reqwest::StatusCode::OK);
    record_usage(&h, tenant, &format!("k1-{}", Uuid::new_v4()), "seat", 3).await;

    // 1. Gateway owner: slugs bind + money role => generate 201.
    let resp = h
        .http
        .post(format!("{}/v1/invoices/generate", h.base))
        .header("x-tenant-slugs", tenant.to_string())
        .header("x-user-roles", "owner")
        .json(&serde_json::json!({ "tenant_id": tenant, "period": "2026-05" }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::CREATED, "gateway owner generate");
    let invoice: serde_json::Value = resp.json().await.unwrap();
    let invoice_id = Uuid::parse_str(invoice["id"].as_str().unwrap()).unwrap();

    // 2. Gateway member (bound, no money role): generate 403, but READS are
    //    allowed (list is tenant-bound, not role-gated).
    let resp = h
        .http
        .post(format!("{}/v1/invoices/generate", h.base))
        .header("x-tenant-slugs", tenant.to_string())
        .header("x-user-roles", "member")
        .json(&serde_json::json!({ "tenant_id": tenant, "period": "2026-06" }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::FORBIDDEN, "member generate 403");
    let resp = h
        .http
        .get(format!("{}/v1/invoices?tenant_id={tenant}", h.base))
        .header("x-tenant-slugs", tenant.to_string())
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK, "member list read ok");

    // 3. Gateway caller bound to a DIFFERENT tenant: 403 on generate, and an
    //    id-addressed read of our invoice is 403 (invoice tenant not in the
    //    caller's claim) — never a cross-tenant leak.
    let resp = h
        .http
        .post(format!("{}/v1/invoices/generate", h.base))
        .header("x-tenant-slugs", Uuid::new_v4().to_string())
        .header("x-user-roles", "owner")
        .json(&serde_json::json!({ "tenant_id": tenant, "period": "2026-07" }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::FORBIDDEN, "unbound tenant 403");
    let resp = h
        .http
        .get(format!("{}/v1/invoices/{invoice_id}", h.base))
        .header("x-tenant-slugs", Uuid::new_v4().to_string())
        .send()
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        reqwest::StatusCode::FORBIDDEN,
        "cross-tenant invoice read via gateway must be 403"
    );

    // 4. Gateway owner mutates by id (issue) — the invoice's OWN tenant is
    //    bound from the verified claim after the scoped lookup.
    let resp = h
        .http
        .post(format!("{}/v1/invoices/{invoice_id}/issue", h.base))
        .header("x-tenant-slugs", tenant.to_string())
        .header("x-user-roles", "owner")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK, "gateway owner issue");
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["status"], "issued");

    // 5. Gateway member is 403 on issue too (K6 covers every mutation).
    let inv2: (reqwest::StatusCode, serde_json::Value) = generate(&h, tenant, "2026-08").await;
    assert_eq!(inv2.0, reqwest::StatusCode::CREATED);
    let inv2_id = Uuid::parse_str(inv2.1["id"].as_str().unwrap()).unwrap();
    let resp = h
        .http
        .post(format!("{}/v1/invoices/{inv2_id}/issue", h.base))
        .header("x-tenant-slugs", tenant.to_string())
        .header("x-user-roles", "member")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::FORBIDDEN, "member issue 403");
}
