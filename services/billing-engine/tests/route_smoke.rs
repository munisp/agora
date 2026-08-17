//! Route smoke tests (W41 repair R1): pin the axum 0.7 `:param` path
//! parameter syntax for every route registered by `routes::router`.
//!
//! Defect under test: this crate pins axum 0.7 (Cargo.lock: axum 0.7.9),
//! where path parameters are written `:param`. The route table had been
//! written with the axum 0.8 `{param}` syntax, which axum 0.7 accepts as a
//! LITERAL path segment — so e.g. `PUT /v1/rate-cards/<uuid>` never matched
//! `/v1/rate-cards/{tenant_id}` and every parameterized route 404'd at the
//! match layer (only the literal URL `/v1/rate-cards/{tenant_id}` routed to
//! the handler, where the Path extractor then failed with "Wrong number of
//! path arguments").
//!
//! These tests boot the REAL route table (`src/routes.rs::router`, compiled
//! into this test crate via `#[path]` — the crate is binary-only, mirroring
//! the proptest_signature.rs idiom) behind a real TCP listener and assert
//! that requests with concrete path segments REACH THE HANDLER. Every /v1/*
//! handler begins with the X-Tenant-ID header check, which runs before any
//! DB access, so a matched parameterized route deterministically answers
//! 403 ("missing X-Tenant-ID header") when only the internal token is
//! supplied. A match-layer miss answers 404 with an empty body instead.
//!
//! Limitation: `AppState` is defined in `src/main.rs`, whose `mod routes;`
//! is private and therefore not reachable from an integration test, so this
//! file mirrors the `AppState` field set verbatim (any drift breaks
//! compilation of this test — the mirror cannot silently go stale) and
//! compiles the real `routes.rs` (route table, middleware, handlers) against
//! it. The Postgres pools are `connect_lazy` handles that never dial: no
//! request below reaches a query.

use std::sync::atomic::AtomicU64;
use std::sync::Arc;

#[path = "../src/config.rs"]
mod config;
#[path = "../src/invoices.rs"]
mod invoices;
#[path = "../src/ledger.rs"]
mod ledger;
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

/// Verbatim mirror of `src/main.rs::AppState` (the router's state type).
#[derive(Clone)]
pub struct AppState {
    pub pool: sqlx::PgPool,
    pub internal_pool: sqlx::PgPool,
    pub ledger: Arc<dyn ledger::BillingLedger>,
    pub producer: Option<rdkafka::producer::FutureProducer>,
    pub http: reqwest::Client,
    pub config: Arc<config::Config>,
    pub outbox_notify: Arc<tokio::sync::Notify>,
    pub events_published: Arc<AtomicU64>,
    pub events_failed: Arc<AtomicU64>,
}

const INTERNAL_TOKEN: &str = "route-smoke-internal-token";

fn test_config() -> config::Config {
    config::Config {
        port: 0,
        database_url: "postgres://127.0.0.1:1/billing_route_smoke".to_string(),
        internal_database_url: None,
        kafka_brokers: "127.0.0.1:1".to_string(),
        kafka_group_id: "route-smoke".to_string(),
        usage_events_topic: "opendesk.usage".to_string(),
        kafka_consumer_enabled: false,
        billing_events_topic: "opendesk.billing".to_string(),
        billing_ledger_impl: "sim".to_string(),
        internal_token: INTERNAL_TOKEN.to_string(),
        paystack_secret_key: None,
        paystack_default_email: "smoke@example.com".to_string(),
        paystack_callback_url: "http://127.0.0.1/callback".to_string(),
        billing_static_account: "OpenDesk Smoke/0000000000".to_string(),
        billing_merchant_name: "OpenDesk Smoke".to_string(),
        dunning_interval_s: 3600,
        invoice_due_days: 14,
    }
}

/// Boot the real router on an ephemeral port; return its base URL.
async fn spawn_server() -> String {
    // Lazy pools never dial; no handler below reaches a query because the
    // tenant-header check rejects first.
    let pool = sqlx::PgPool::connect_lazy("postgres://127.0.0.1:1/billing_route_smoke")
        .expect("connect_lazy does not dial");
    let state = AppState {
        pool: pool.clone(),
        internal_pool: pool,
        ledger: Arc::new(ledger::SimLedgerClient::new()),
        producer: None,
        http: reqwest::Client::new(),
        config: Arc::new(test_config()),
        outbox_notify: Arc::new(tokio::sync::Notify::new()),
        events_published: Arc::new(AtomicU64::new(0)),
        events_failed: Arc::new(AtomicU64::new(0)),
    };
    let app = routes::router(state);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind ephemeral port");
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.expect("serve router");
    });
    format!("http://{addr}")
}

fn client() -> reqwest::Client {
    reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(5))
        .build()
        .unwrap()
}

/// A matched /v1/* route whose handler starts with the tenant-header check
/// answers 403 "missing X-Tenant-ID header" when only the internal token is
/// presented. Anything else (404 at the match layer, extractor failures)
/// fails the assertion.
async fn assert_reaches_tenant_check(resp: reqwest::Response, desc: &str) {
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    assert_eq!(
        status,
        reqwest::StatusCode::FORBIDDEN,
        "{desc}: route must match and reach the handler (expected 403 from the \
         tenant-header check); got {status} with body {body:?}. A 404 here means \
         the path template did not match (axum 0.7 `:param` syntax defect)."
    );
    assert!(
        body.contains("X-Tenant-ID"),
        "{desc}: expected the handler's tenant-check error, got {body:?}"
    );
}

/// Every parameterized route in the billing route table: a request carrying
/// a concrete path segment must match and reach the handler.
#[tokio::test]
async fn parameterized_routes_match_concrete_segments() {
    let base = spawn_server().await;
    let http = client();
    let id = uuid::Uuid::new_v4();
    let tenant = uuid::Uuid::new_v4();

    // PUT /v1/rate-cards/:tenant_id
    let resp = http
        .put(format!("{base}/v1/rate-cards/{tenant}"))
        .header("x-internal-token", INTERNAL_TOKEN)
        .json(&serde_json::json!({
            "metric": "seat",
            "unit_price_cents": 100,
        }))
        .send()
        .await
        .unwrap();
    assert_reaches_tenant_check(resp, "PUT /v1/rate-cards/:tenant_id").await;

    // GET /v1/invoices/:id
    let resp = http
        .get(format!("{base}/v1/invoices/{id}"))
        .header("x-internal-token", INTERNAL_TOKEN)
        .send()
        .await
        .unwrap();
    assert_reaches_tenant_check(resp, "GET /v1/invoices/:id").await;

    // POST /v1/invoices/:id/issue
    let resp = http
        .post(format!("{base}/v1/invoices/{id}/issue"))
        .header("x-internal-token", INTERNAL_TOKEN)
        .send()
        .await
        .unwrap();
    assert_reaches_tenant_check(resp, "POST /v1/invoices/:id/issue").await;

    // POST /v1/invoices/:id/void
    let resp = http
        .post(format!("{base}/v1/invoices/{id}/void"))
        .header("x-internal-token", INTERNAL_TOKEN)
        .send()
        .await
        .unwrap();
    assert_reaches_tenant_check(resp, "POST /v1/invoices/:id/void").await;

    // POST /v1/invoices/:id/payment-link
    let resp = http
        .post(format!("{base}/v1/invoices/{id}/payment-link"))
        .header("x-internal-token", INTERNAL_TOKEN)
        .send()
        .await
        .unwrap();
    assert_reaches_tenant_check(resp, "POST /v1/invoices/:id/payment-link").await;

    // GET /v1/invoices/:id/qr
    let resp = http
        .get(format!("{base}/v1/invoices/{id}/qr"))
        .header("x-internal-token", INTERNAL_TOKEN)
        .send()
        .await
        .unwrap();
    assert_reaches_tenant_check(resp, "GET /v1/invoices/:id/qr").await;
}

/// Static routes are unaffected anchors: they must behave exactly as before
/// (liveness 200; tenant-gated handlers 403 without the tenant header).
#[tokio::test]
async fn static_routes_still_behave() {
    let base = spawn_server().await;
    let http = client();
    let tenant = uuid::Uuid::new_v4();

    // /healthz is token-exempt.
    let resp = http.get(format!("{base}/healthz")).send().await.unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK);

    // GET /v1/invoices (static) still matches and reaches the handler.
    let resp = http
        .get(format!("{base}/v1/invoices?tenant_id={tenant}"))
        .header("x-internal-token", INTERNAL_TOKEN)
        .send()
        .await
        .unwrap();
    assert_reaches_tenant_check(resp, "GET /v1/invoices").await;

    // POST /v1/invoices/generate (static) still matches and reaches the handler.
    let resp = http
        .post(format!("{base}/v1/invoices/generate"))
        .header("x-internal-token", INTERNAL_TOKEN)
        .json(&serde_json::json!({
            "tenant_id": tenant,
            "period": "2026-01",
        }))
        .send()
        .await
        .unwrap();
    assert_reaches_tenant_check(resp, "POST /v1/invoices/generate").await;

    // The internal-token gate itself still fails closed (401 without it).
    let resp = http
        .get(format!("{base}/v1/invoices?tenant_id={tenant}"))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::UNAUTHORIZED);
}

/// Controls proving what 404 means here, and that the axum 0.8 `{param}`
/// literal-segment form is gone for good.
#[tokio::test]
async fn unmatched_and_literal_brace_paths_404() {
    let base = spawn_server().await;
    let http = client();

    // A path no route template matches 404s (this is what the parameterized
    // routes returned pre-fix).
    let resp = http
        .get(format!("{base}/v1/definitely-not-registered"))
        .header("x-internal-token", INTERNAL_TOKEN)
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::NOT_FOUND);

    // Sending the axum 0.8-style braces (percent-encoded, since HTTP clients
    // cannot put raw braces in a path) must NOT reach the handler logic:
    // post-fix the segment is captured by `:tenant_id` and the decoded
    // literal "{tenant_id}" is rejected by Path<Uuid> at extraction (400).
    // (With raw, unencoded braces — curl's behavior — the pre-fix literal
    // route was the ONLY match and the Path extractor 500'd with "Wrong
    // number of path arguments".)
    let resp = http
        .put(format!("{base}/v1/rate-cards/%7Btenant_id%7D"))
        .header("x-internal-token", INTERNAL_TOKEN)
        .json(&serde_json::json!({
            "metric": "seat",
            "unit_price_cents": 100,
        }))
        .send()
        .await
        .unwrap();
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    assert_eq!(
        status,
        reqwest::StatusCode::BAD_REQUEST,
        "braced segment must be captured by :tenant_id and rejected by          Path<Uuid> (400), got {status} with body {body:?}"
    );
    assert!(
        !body.contains("Wrong number of path arguments"),
        "the pre-fix literal-segment signature must be gone, got {body:?}"
    );

    // A malformed concrete segment still MATCHES :id (Path<Uuid> rejection
    // happens inside the handler's extractor -> 400, not a match-level 404).
    let resp = http
        .get(format!("{base}/v1/invoices/not-a-uuid"))
        .header("x-internal-token", INTERNAL_TOKEN)
        .send()
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        reqwest::StatusCode::BAD_REQUEST,
        "a matched :id route with a malformed uuid must 400 at extraction, not 404"
    );
}
