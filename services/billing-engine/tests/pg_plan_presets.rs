//! SPEC-W45 K18-billing-side integration tests against a REAL Postgres:
//! plan presets -> rate_cards copy, tax_bps flow-through, and the
//! billing_email roundtrip (generate body -> PATCH -> event payload).
//!
//! Same harness idiom as pg_hardening.rs: the production modules are
//! compiled in via `#[path]`, the REAL router is served over TCP, and all
//! six boot-path migrations (0001..0006) are applied.
//!
//! Requires env `BILLING_TEST_DATABASE_URL` (the pgserver-backed driver sets
//! it); when unset every test skips itself so plain `cargo test` stays
//! hermetic.

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

const INTERNAL_TOKEN: &str = "pg-plan-presets-internal-token";

fn test_config(database_url: &str) -> config::Config {
    config::Config {
        port: 0,
        database_url: database_url.to_string(),
        internal_database_url: None,
        kafka_brokers: "127.0.0.1:1".to_string(),
        kafka_group_id: "pg-plan-presets".to_string(),
        usage_events_topic: "opendesk.usage.events".to_string(),
        kafka_consumer_enabled: false,
        billing_events_topic: "opendesk.billing.events".to_string(),
        dlq_topic: "opendesk.dlq".to_string(),
        billing_ledger_impl: "postgres".to_string(),
        internal_token: INTERNAL_TOKEN.to_string(),
        paystack_secret_key: None,
        paystack_default_email: "pg-plan@example.com".to_string(),
        paystack_callback_url: "http://127.0.0.1/callback".to_string(),
        billing_static_account: "PG/0000000000".to_string(),
        billing_merchant_name: "PG Plan Presets".to_string(),
        money_roles: vec!["owner".to_string(), "admin".to_string()],
        trust_direct_tenant: false,
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
    http: reqwest::Client,
}

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

/// All migrations (0001..0006, the boot path incl. K18), in order.
pub const MIGRATIONS: [&str; 6] = [
    include_str!("../migrations/0001_init.sql"),
    include_str!("../migrations/0002_rls.sql"),
    include_str!("../migrations/0003_ledger.sql"),
    include_str!("../migrations/0004_outbox.sql"),
    include_str!("../migrations/0005_hardening.sql"),
    include_str!("../migrations/0006_plan_presets.sql"),
];

static MIGRATED: tokio::sync::OnceCell<()> = tokio::sync::OnceCell::const_new();

async fn harness() -> Option<Harness> {
    let url = std::env::var("BILLING_TEST_DATABASE_URL").ok()?;
    let pool = PgPool::connect_with(connect_options(&url))
        .await
        .expect("connect test database");
    MIGRATED
        .get_or_init(|| async {
            for m in MIGRATIONS {
                sqlx::raw_sql(m)
                    .execute(&pool)
                    .await
                    .expect("migration applies clean");
            }
        })
        .await;
    let ledger = Arc::new(
        ledger::PgLedgerClient::new(pool.clone())
            .await
            .expect("pg ledger builds"),
    );
    let state = AppState {
        pool: pool.clone(),
        internal_pool: pool.clone(),
        ledger,
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
// Fixture helpers (internal-token service path = K2)
// ---------------------------------------------------------------------------

async fn put_plan(h: &Harness, tenant: Uuid, plan: &str) -> reqwest::Response {
    h.http
        .put(format!("{}/v1/tenants/{tenant}/plan", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .json(&serde_json::json!({ "plan": plan }))
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

async fn generate(
    h: &Harness,
    tenant: Uuid,
    period: &str,
    billing_email: Option<&str>,
) -> (reqwest::StatusCode, serde_json::Value) {
    let mut body = serde_json::json!({ "tenant_id": tenant, "period": period });
    if let Some(email) = billing_email {
        body["billing_email"] = serde_json::Value::String(email.to_string());
    }
    let resp = h
        .http
        .post(format!("{}/v1/invoices/generate", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .header("x-user-roles", "owner")
        .json(&body)
        .send()
        .await
        .unwrap();
    let status = resp.status();
    let body = resp.json().await.unwrap_or(serde_json::Value::Null);
    (status, body)
}

async fn get_invoice(h: &Harness, id: Uuid) -> serde_json::Value {
    let resp = h
        .http
        .get(format!("{}/v1/invoices/{id}", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK);
    resp.json().await.unwrap()
}

async fn rate_card_rows(h: &Harness, tenant: Uuid) -> Vec<(String, i64, i64)> {
    sqlx::query(
        "SELECT metric, unit_price_cents, tax_bps FROM rate_cards \
         WHERE tenant_id = $1 ORDER BY metric",
    )
    .bind(tenant)
    .fetch_all(&h.pool)
    .await
    .unwrap()
    .iter()
    .map(|r| {
        (
            r.try_get::<String, _>("metric").unwrap(),
            r.try_get::<i64, _>("unit_price_cents").unwrap(),
            r.try_get::<i32, _>("tax_bps").unwrap() as i64,
        )
    })
    .collect()
}

// ---------------------------------------------------------------------------
// K18 (ORPH O5): plan presets -> rate_cards copy on first generation
// ---------------------------------------------------------------------------

/// A tenant with ZERO rate cards gets the presets for its tenant_plans plan
/// copied inside the generation transaction; generating TWICE copies at most
/// once (idempotent — no duplicate rows, no error on the second pass).
#[tokio::test]
async fn k18_plan_presets_copy_is_transactional_and_idempotent() {
    let h = require_harness!();
    let tenant = Uuid::new_v4();

    // identity pushes the plan (billing's plan source).
    let resp = put_plan(&h, tenant, "standard").await;
    assert_eq!(resp.status(), reqwest::StatusCode::OK, "plan push: {:?}", resp.text().await);

    record_usage(&h, tenant, &format!("usage-{tenant}"), "booking", 1200).await;

    // First generate: zero rate cards -> presets copied, invoice rated from
    // the copy (standard: booking 50c over a 1000 quota -> 200 * 50 = 10000).
    let (status, inv) = generate(&h, tenant, "2026-03", None).await;
    assert_eq!(status, reqwest::StatusCode::CREATED, "generate: {inv}");
    assert_eq!(inv["subtotal_cents"].as_i64().unwrap(), 10_000);
    let id = Uuid::parse_str(inv["id"].as_str().unwrap()).unwrap();

    let cards = rate_card_rows(&h, tenant).await;
    assert_eq!(
        cards.len(),
        3,
        "standard presets copied exactly once (booking, call_minutes, message): {cards:?}"
    );
    assert!(cards.iter().any(|(m, price, _)| m == "booking" && *price == 50));

    // Second generate (same period, still draft): regenerate path — the copy
    // is a no-op (ON CONFLICT DO NOTHING) and the row count is unchanged.
    let (status, inv2) = generate(&h, tenant, "2026-03", None).await;
    assert_eq!(status, reqwest::StatusCode::CREATED, "regenerate: {inv2}");
    assert_eq!(
        inv2["id"].as_str().unwrap(),
        id.to_string(),
        "draft regenerate keeps the invoice id stable"
    );
    let cards2 = rate_card_rows(&h, tenant).await;
    assert_eq!(cards2, cards, "double generate must not duplicate the copy");
}

/// No tenant_plans row at all => default 'free' (zero-priced); an UNKNOWN
/// plan falls back to the zero-priced 'free' presets (never bills garbage).
#[tokio::test]
async fn k18_default_free_and_unknown_plan_falls_back_to_free() {
    let h = require_harness!();

    // (a) No plan row: default 'free' presets — all zero-priced.
    let tenant = Uuid::new_v4();
    let (status, inv) = generate(&h, tenant, "2026-03", None).await;
    assert_eq!(status, reqwest::StatusCode::CREATED, "generate: {inv}");
    assert_eq!(inv["subtotal_cents"].as_i64().unwrap(), 0);
    let cards = rate_card_rows(&h, tenant).await;
    assert_eq!(cards.len(), 3, "free presets copied: {cards:?}");
    assert!(
        cards.iter().all(|(_, price, _)| *price == 0),
        "free presets are zero-priced: {cards:?}"
    );

    // (b) Unknown plan: WARN + zero-priced 'free' fallback.
    let tenant = Uuid::new_v4();
    let resp = put_plan(&h, tenant, "enterprise-deluxe").await;
    assert_eq!(resp.status(), reqwest::StatusCode::OK);
    record_usage(&h, tenant, &format!("usage-{tenant}"), "booking", 5000).await;
    let (status, inv) = generate(&h, tenant, "2026-03", None).await;
    assert_eq!(status, reqwest::StatusCode::CREATED, "generate: {inv}");
    assert_eq!(
        inv["subtotal_cents"].as_i64().unwrap(),
        0,
        "unknown plan must bill nothing (zero-priced free fallback)"
    );
    let cards = rate_card_rows(&h, tenant).await;
    assert_eq!(cards.len(), 3);
    assert!(cards.iter().all(|(_, price, _)| *price == 0));
}

/// A tenant that already HAS rate cards never gets presets copied over them.
#[tokio::test]
async fn k18_existing_rate_cards_are_never_overwritten_by_presets() {
    let h = require_harness!();
    let tenant = Uuid::new_v4();
    let resp = put_plan(&h, tenant, "pro").await;
    assert_eq!(resp.status(), reqwest::StatusCode::OK);
    // One explicit card (pro preset would price booking at 25; ours is 99).
    let resp = h
        .http
        .put(format!("{}/v1/rate-cards/{tenant}", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .json(&serde_json::json!({
            "metric": "booking",
            "unit_price_cents": 99,
            "included_quota": 0,
            "currency": "USD",
        }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK);

    record_usage(&h, tenant, &format!("usage-{tenant}"), "booking", 10).await;
    let (status, inv) = generate(&h, tenant, "2026-03", None).await;
    assert_eq!(status, reqwest::StatusCode::CREATED, "generate: {inv}");
    assert_eq!(inv["subtotal_cents"].as_i64().unwrap(), 990);
    let cards = rate_card_rows(&h, tenant).await;
    assert_eq!(
        cards.len(),
        1,
        "no preset copy when the tenant already has cards: {cards:?}"
    );
}

// ---------------------------------------------------------------------------
// K18: tax_bps (VAT-ready foundation)
// ---------------------------------------------------------------------------

/// tax_bps defaults to 0 through the whole flow: plan presets copied with
/// tax_bps 0 stamp tax_bps 0 onto generated line items; amounts unchanged.
#[tokio::test]
async fn k18_tax_bps_default_flows_through_generate() {
    let h = require_harness!();
    let tenant = Uuid::new_v4();
    record_usage(&h, tenant, &format!("usage-{tenant}"), "booking", 1200).await;
    let (status, inv) = generate(&h, tenant, "2026-03", None).await;
    assert_eq!(status, reqwest::StatusCode::CREATED, "generate: {inv}");
    let items = inv["line_items"].as_array().expect("line items array");
    assert_eq!(items.len(), 1, "one rated metric: {items:?}");
    assert_eq!(items[0]["metric"], "booking");
    assert_eq!(
        items[0]["tax_bps"].as_i64().unwrap(),
        0,
        "preset tax_bps defaults to 0 and stamps the line: {items:?}"
    );
    // Amounts unchanged: free presets are zero-priced even over quota.
    assert_eq!(items[0]["amount_cents"].as_i64().unwrap(), 0);
    assert_eq!(inv["subtotal_cents"].as_i64().unwrap(), 0);
}

/// PUT /v1/rate-cards accepts+validates tax_bps (0..=10000); a stamped card
/// flows onto the generated line item without changing amounts.
#[tokio::test]
async fn k18_rate_card_tax_bps_validated_and_stamped_on_lines() {
    let h = require_harness!();
    let tenant = Uuid::new_v4();

    // Out of range is rejected.
    for bad in [-1, 10001] {
        let resp = h
            .http
            .put(format!("{}/v1/rate-cards/{tenant}", h.base))
            .header("x-internal-token", INTERNAL_TOKEN)
            .json(&serde_json::json!({
                "metric": "booking",
                "unit_price_cents": 50,
                "tax_bps": bad,
            }))
            .send()
            .await
            .unwrap();
        assert_eq!(
            resp.status(),
            reqwest::StatusCode::BAD_REQUEST,
            "tax_bps {bad} must 400"
        );
    }

    // 750 bps (7.5%) accepted, echoed, and stamped onto the line.
    let resp = h
        .http
        .put(format!("{}/v1/rate-cards/{tenant}", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .json(&serde_json::json!({
            "metric": "booking",
            "unit_price_cents": 50,
            "included_quota": 1000,
            "currency": "USD",
            "tax_bps": 750,
        }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK);
    let card: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(card["tax_bps"].as_i64().unwrap(), 750);

    record_usage(&h, tenant, &format!("usage-{tenant}"), "booking", 1200).await;
    let (status, inv) = generate(&h, tenant, "2026-03", None).await;
    assert_eq!(status, reqwest::StatusCode::CREATED, "generate: {inv}");
    let items = inv["line_items"].as_array().unwrap();
    assert_eq!(items[0]["tax_bps"].as_i64().unwrap(), 750);
    assert_eq!(
        items[0]["amount_cents"].as_i64().unwrap(),
        10_000,
        "tax_bps never changes amounts (VAT-ready foundation only)"
    );
    assert_eq!(inv["subtotal_cents"].as_i64().unwrap(), 10_000);
}

// ---------------------------------------------------------------------------
// billing_email: generate body -> PATCH -> API + event payload reflection
// ---------------------------------------------------------------------------

/// Roundtrip: billing_email in the generate body is stored and returned by
/// the invoice API; PATCH replaces it (and validates lightly); the
/// InvoiceVoided event payload carries `billingEmail` when present; `null`
/// clears it; a regenerate with no billing_email COALESCE-keeps the stored
/// contact.
#[tokio::test]
async fn w45_billing_email_roundtrip_generate_patch_event_payload() {
    let h = require_harness!();
    let tenant = Uuid::new_v4();
    let resp = h
        .http
        .put(format!("{}/v1/rate-cards/{tenant}", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .json(&serde_json::json!({
            "metric": "booking",
            "unit_price_cents": 50,
            "currency": "USD",
        }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK);
    record_usage(&h, tenant, &format!("usage-{tenant}"), "booking", 2).await;

    // 1. Generate with billing_email in the body -> stored + reflected.
    let (status, inv) = generate(&h, tenant, "2026-03", Some("first@acme.ng")).await;
    assert_eq!(status, reqwest::StatusCode::CREATED, "generate: {inv}");
    assert_eq!(inv["billing_email"].as_str().unwrap(), "first@acme.ng");
    let id = Uuid::parse_str(inv["id"].as_str().unwrap()).unwrap();
    let fetched = get_invoice(&h, id).await;
    assert_eq!(fetched["billing_email"].as_str().unwrap(), "first@acme.ng");

    // 2. PATCH replaces the contact; the API JSON reflects it.
    let resp = h
        .http
        .patch(format!("{}/v1/invoices/{id}/billing-email", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .header("x-user-roles", "owner")
        .json(&serde_json::json!({ "billing_email": "billing@acme.ng" }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK);
    let patched: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(patched["billing_email"].as_str().unwrap(), "billing@acme.ng");

    // 2b. Light validation: malformed addresses 400.
    let resp = h
        .http
        .patch(format!("{}/v1/invoices/{id}/billing-email", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .header("x-user-roles", "owner")
        .json(&serde_json::json!({ "billing_email": "not-an-email" }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::BAD_REQUEST);

    // 3. K6: a gateway member (no money role) is 403 on the PATCH.
    let resp = h
        .http
        .patch(format!("{}/v1/invoices/{id}/billing-email", h.base))
        .header("x-tenant-slugs", tenant.to_string())
        .header("x-user-roles", "member")
        .json(&serde_json::json!({ "billing_email": "member@acme.ng" }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::FORBIDDEN, "member PATCH 403");
    let fetched = get_invoice(&h, id).await;
    assert_eq!(fetched["billing_email"].as_str().unwrap(), "billing@acme.ng");

    // 4. Regenerate WITHOUT billing_email COALESCE-keeps the stored contact.
    let (status, inv2) = generate(&h, tenant, "2026-03", None).await;
    assert_eq!(status, reqwest::StatusCode::CREATED, "regenerate: {inv2}");
    assert_eq!(inv2["billing_email"].as_str().unwrap(), "billing@acme.ng");

    // 5. Event payload reflection: issue + void -> the InvoiceVoided outbox
    //    event carries billingEmail.
    let resp = h
        .http
        .post(format!("{}/v1/invoices/{id}/issue", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .header("x-user-roles", "owner")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK);
    let resp = h
        .http
        .post(format!("{}/v1/invoices/{id}/void", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .header("x-user-roles", "owner")
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK);
    let row = sqlx::query(
        "SELECT payload->'data'->>'billingEmail' AS em FROM billing_outbox \
         WHERE event_key = $1 AND payload->>'type' = 'com.opendesk.billing.InvoiceVoided' \
         ORDER BY created_at DESC LIMIT 1",
    )
    .bind(tenant.to_string())
    .fetch_one(&h.pool)
    .await
    .expect("InvoiceVoided outbox row present");
    assert_eq!(
        row.try_get::<Option<String>, _>("em").unwrap().as_deref(),
        Some("billing@acme.ng"),
        "InvoiceVoided payload carries billingEmail when present"
    );

    // 6. `null` clears the contact on the replacement invoice.
    let (status, inv3) = generate(&h, tenant, "2026-04", None).await;
    assert_eq!(status, reqwest::StatusCode::CREATED, "generate: {inv3}");
    let id3 = Uuid::parse_str(inv3["id"].as_str().unwrap()).unwrap();
    assert!(
        inv3.get("billing_email").is_none() || inv3["billing_email"].is_null(),
        "fresh invoice starts without a billing contact: {inv3}"
    );
    let resp = h
        .http
        .patch(format!("{}/v1/invoices/{id3}/billing-email", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .header("x-user-roles", "owner")
        .json(&serde_json::json!({ "billing_email": "temp@acme.ng" }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK);
    let resp = h
        .http
        .patch(format!("{}/v1/invoices/{id3}/billing-email", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .header("x-user-roles", "owner")
        .json(&serde_json::json!({ "billing_email": null }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK, "null clears the contact");
    let cleared: serde_json::Value = resp.json().await.unwrap();
    assert!(
        cleared.get("billing_email").is_none() || cleared["billing_email"].is_null(),
        "cleared contact is absent from the API JSON: {cleared}"
    );
}
