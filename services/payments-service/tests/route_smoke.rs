//! Route smoke tests (W41 repair R1): pin the axum 0.7 `:param` path
//! parameter syntax for every route registered by `routes::router`.
//!
//! Defect under test: this crate pins axum 0.7 (Cargo.lock: axum 0.7.9),
//! where path parameters are written `:param`. The route table had been
//! written with the axum 0.8 `{param}` syntax, which axum 0.7 accepts as a
//! LITERAL path segment — so e.g. `POST /v1/deposits/<uuid>/capture` never
//! matched `/v1/deposits/{id}/capture` and every parameterized route 404'd
//! at the match layer.
//!
//! These tests boot the REAL route table (`src/routes.rs::router`, compiled
//! into this test crate via `#[path]` — the crate is binary-only, mirroring
//! the proptest_ledger.rs idiom) behind a real TCP listener, backed by the
//! in-memory `SimLedgerClient` (ADR-0007), and assert that requests with
//! concrete path segments REACH THE HANDLER:
//!   * `POST /v1/deposits/:id/capture` must capture a deposit held through
//!     the real API (end-to-end 200, not a match-layer 404);
//!   * `GET /v1/accounts/:tenant_id/balance` must return the sim ledger's
//!     balance (200), not 404.
//!
//! Limitation: `AppState` (and its `publish_event` method / the
//! `http_client` helper) is defined in `src/main.rs`, whose `mod routes;`
//! is private and therefore not reachable from an integration test, so this
//! file mirrors them verbatim (any drift breaks compilation of this test —
//! the mirror cannot silently go stale) and compiles the real `routes.rs`
//! (route table and handlers) against it. The Dapr outbox points at an
//! unreachable port on purpose: publication is best-effort (ADR-0007), so
//! handler results do not depend on it.

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

// Trait in scope for the MemRegistry handle assertions (K7 provenance test).
use registry::Registry as _;

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
    /// Best-effort outbox (ADR-0007 note): ledger ops commit first; event
    /// publication failures are logged + counted, not rolled back. A
    /// reconciler can republish from the ledger.
    pub async fn publish_event<T: Serialize>(
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
        match self.outbox.publish(&event).await {
            Ok(()) => {
                self.events_published.fetch_add(1, Ordering::Relaxed);
            }
            Err(e) => {
                self.events_failed.fetch_add(1, Ordering::Relaxed);
                warn!(
                    error = %e,
                    type_ = %event.type_,
                    "dapr pubsub publish failed (best-effort outbox)"
                );
            }
        }
    }
}

fn test_config() -> config::Config {
    config::Config {
        port: 0,
        ledger_impl: "sim".to_string(),
        tb_addresses: String::new(),
        tb_cluster_id: 0,
        kafka_brokers: "127.0.0.1:1".to_string(),
        kafka_group_id: "route-smoke".to_string(),
        kafka_commands_topic: "opendesk.payments.commands".to_string(),
        kafka_consumer_enabled: false,
        dlq_topic: "opendesk.dlq".to_string(),
        dapr_host: "127.0.0.1".to_string(),
        dapr_http_port: 1, // unreachable; the outbox is best-effort
        dapr_pubsub: "pubsub".to_string(),
        events_topic: "opendesk.payments.events".to_string(),
        mojaloop_endpoint: "http://127.0.0.1:1".to_string(),
        mojaloop_allow_sim: false,
        platform_fee_bps: 0,
        internal_token: None,
        trust_direct_tenant: true,
        database_url: None,
        payout_reconciler_interval_secs: 30,
        money_roles: vec!["owner".to_string(), "admin".to_string()],
    }
}

/// Handles into the spawned server's state (K7/F15-03 tests inspect the
/// registry + counters directly).
pub struct ServerHandles {
    pub base: String,
    pub registry: Arc<registry::MemRegistry>,
    pub commands_dead_lettered: Arc<AtomicU64>,
}

/// Boot the real router on an ephemeral port; return its base URL.
/// Default posture: dev escape on, no internal token (OPENDESK_TRUST_DIRECT_TENANT=1).
async fn spawn_server() -> String {
    spawn_server_with_auth(None, true).await.base
}

/// Boot the real router with an explicit auth posture (P-09/K6/K7 tests).
async fn spawn_server_with_auth(internal_token: Option<&str>, trust_direct: bool) -> ServerHandles {
    spawn_server_full(internal_token, trust_direct, "http://127.0.0.1:1").await
}

/// Full control: explicit auth posture AND Mojaloop rail URL (a rail stub
/// gives the K7 happy path a committed payout).
async fn spawn_server_full(
    internal_token: Option<&str>,
    trust_direct: bool,
    rail_url: &str,
) -> ServerHandles {
    spawn_with_app(internal_token, trust_direct, rail_url, false).await
}

/// Same as `spawn_server_full`, plus the Flutterwave sub-router merged in —
/// mirroring `main.rs` (`routes::router(state).merge(flutterwave::router(state))`),
/// since the flutterwave routes are NOT part of `routes::router`.
async fn spawn_server_merged(
    internal_token: Option<&str>,
    trust_direct: bool,
) -> ServerHandles {
    spawn_with_app(internal_token, trust_direct, "http://127.0.0.1:1", true).await
}

async fn spawn_with_app(
    internal_token: Option<&str>,
    trust_direct: bool,
    rail_url: &str,
    with_flutterwave: bool,
) -> ServerHandles {
    let registry = Arc::new(registry::MemRegistry::default());
    let dead = Arc::new(AtomicU64::new(0));
    let state = AppState {
        ledger: Arc::new(ledger::sim::SimLedgerClient::new(0)),
        outbox: dapr::DaprOutbox::new(
            "http://127.0.0.1:1".to_string(),
            "pubsub".to_string(),
            "opendesk.payments.events".to_string(),
        ),
        mojaloop: mojaloop::MojaloopAdapter::new(rail_url.to_string()),
        flutterwave: flutterwave::FlutterwaveAdapter::from_env(),
        config: Arc::new(test_config()),
        dlq: Arc::new(consumer::UnavailableDlqSink),
        auth: auth::AuthConfig::new(
            internal_token.map(|s| s.to_string()),
            trust_direct,
            vec!["owner".to_string(), "admin".to_string()],
        ),
        payout_attempts: Arc::new(payouts::MemPayoutAttemptStore::default()),
        registry: registry.clone(),
        events_published: Arc::new(AtomicU64::new(0)),
        events_failed: Arc::new(AtomicU64::new(0)),
        commands_dead_lettered: dead.clone(),
        commands_processed: Arc::new(AtomicU64::new(0)),
        payouts_attempted: Arc::new(AtomicU64::new(0)),
        payouts_committed: Arc::new(AtomicU64::new(0)),
        payouts_failed: Arc::new(AtomicU64::new(0)),
        payouts_unknown: Arc::new(AtomicU64::new(0)),
    };
    let app = if with_flutterwave {
        routes::router(state.clone()).merge(flutterwave::router(state))
    } else {
        routes::router(state)
    };
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind ephemeral port");
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.expect("serve router");
    });
    ServerHandles {
        base: format!("http://{addr}"),
        registry,
        commands_dead_lettered: dead,
    }
}

fn client() -> reqwest::Client {
    reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(5))
        .build()
        .unwrap()
}

/// `POST /v1/deposits/:id/capture`: a deposit held through the real API
/// must be capturable through the parameterized route (end-to-end 200).
/// Pre-fix this 404'd at the match layer.
#[tokio::test]
async fn capture_route_matches_concrete_deposit_id() {
    let base = spawn_server().await;
    let http = client();

    // Hold a deposit via the static route (this one always worked).
    // P-12: idempotency key required; P-13: NGN only.
    let held = http
        .post(format!("{base}/v1/deposits"))
        .json(&serde_json::json!({
            "tenant_id": "t-smoke",
            "amount_cents": 500,
            "currency": "NGN",
            "idempotency_key": "smoke-hold-1",
        }))
        .send()
        .await
        .unwrap();
    assert_eq!(
        held.status(),
        reqwest::StatusCode::CREATED,
        "hold deposit via the static route must succeed"
    );
    let held_body: serde_json::Value = held.json().await.unwrap();
    let deposit_id = held_body["deposit_id"]
        .as_str()
        .expect("deposit_id in hold response")
        .to_string();

    // Capture it via the parameterized route.
    let resp = http
        .post(format!("{base}/v1/deposits/{deposit_id}/capture"))
        .json(&serde_json::json!({ "tenant_id": "t-smoke" }))
        .send()
        .await
        .unwrap();
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    assert_eq!(
        status,
        reqwest::StatusCode::OK,
        "POST /v1/deposits/:id/capture with a concrete id must reach the \
         handler and capture the held deposit; got {status} with body \
         {body:?}. A 404 here means the path template did not match (axum \
         0.7 `:param` syntax defect)."
    );
    let json: serde_json::Value = serde_json::from_str(&body).unwrap();
    // The ledger renders ids hyphenless (`id_string()`); compare as uuids.
    assert_eq!(
        uuid::Uuid::parse_str(json["deposit_id"].as_str().unwrap()).unwrap(),
        uuid::Uuid::parse_str(&deposit_id).unwrap(),
        "capture must target the deposit created via POST /v1/deposits"
    );
}

/// `GET /v1/accounts/:tenant_id/balance`: a concrete tenant segment must
/// reach the handler (200 from the sim ledger), not 404 at the match layer.
#[tokio::test]
async fn balance_route_matches_concrete_tenant() {
    let base = spawn_server().await;
    let http = client();

    let resp = http
        .get(format!("{base}/v1/accounts/t-smoke/balance"))
        .send()
        .await
        .unwrap();
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    assert_eq!(
        status,
        reqwest::StatusCode::OK,
        "GET /v1/accounts/:tenant_id/balance with a concrete tenant must \
         reach the handler; got {status} with body {body:?}. A 404 here \
         means the path template did not match (axum 0.7 `:param` syntax \
         defect)."
    );
}

/// Static-route anchors and the 404 control.
#[tokio::test]
async fn static_routes_and_404_control() {
    let base = spawn_server().await;
    let http = client();

    // Liveness anchor.
    let resp = http.get(format!("{base}/healthz")).send().await.unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK);

    // A path no route template matches 404s with an empty (non-API) body —
    // this is what the parameterized routes returned pre-fix.
    let resp = http
        .get(format!("{base}/v1/definitely-not-registered"))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::NOT_FOUND);

    // Sending the axum 0.8-style braces (percent-encoded, since HTTP clients
    // cannot put raw braces in a path) must NOT reach the handler logic:
    // post-fix the segment is captured by `:id` and the decoded literal
    // "{id}" is rejected by Path<Uuid> at extraction (400, plain-text axum
    // rejection — never the API JSON envelope).
    let resp = http
        .post(format!("{base}/v1/deposits/%7Bid%7D/capture"))
        .json(&serde_json::json!({ "tenant_id": "t-smoke" }))
        .send()
        .await
        .unwrap();
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    assert_eq!(
        status,
        reqwest::StatusCode::BAD_REQUEST,
        "braced segment must be captured by :id and rejected by Path<Uuid>          (400), got {status} with body {body:?}"
    );
    assert!(
        !body.contains("\"error\""),
        "literal {{id}} must not reach a handler (which would answer with \
         the API JSON envelope); got {body:?}"
    );

    // A malformed concrete segment still MATCHES :id (Path<Uuid> rejection
    // happens at extraction -> 400, not a match-level 404).
    let resp = http
        .post(format!("{base}/v1/deposits/not-a-uuid/capture"))
        .json(&serde_json::json!({ "tenant_id": "t-smoke" }))
        .send()
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        reqwest::StatusCode::BAD_REQUEST,
        "a matched :id route with a malformed uuid must 400 at extraction, not 404"
    );
}

// ===========================================================================
// SPEC-W43 API-level regression tests (P-01/P-06/P-09/P-10/P-11/P-12/P-13)
// ===========================================================================

async fn hold_via_api(http: &reqwest::Client, base: &str, tenant: &str, key: &str, cents: u64) -> String {
    let resp = http
        .post(format!("{base}/v1/deposits"))
        .json(&serde_json::json!({
            "tenant_id": tenant,
            "amount_cents": cents,
            "currency": "NGN",
            "idempotency_key": key,
        }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::CREATED);
    resp.json::<serde_json::Value>().await.unwrap()["deposit_id"]
        .as_str()
        .unwrap()
        .to_string()
}

/// P-12/C5: money-moving endpoints reject a missing idempotency key (400).
#[tokio::test]
async fn money_endpoints_require_idempotency_key() {
    let base = spawn_server().await;
    let http = client();

    // hold
    let r = http
        .post(format!("{base}/v1/deposits"))
        .json(&serde_json::json!({"tenant_id": "t-k", "amount_cents": 100, "currency": "NGN"}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::BAD_REQUEST, "hold without key");
    // refund
    let r = http
        .post(format!("{base}/v1/refunds"))
        .json(&serde_json::json!({"tenant_id": "t-k", "amount_cents": 100}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::BAD_REQUEST, "refund without key");
    // payout
    let r = http
        .post(format!("{base}/v1/payouts"))
        .json(&serde_json::json!({
            "tenant_id": "t-k", "amount_cents": 100, "currency": "NGN",
            "payee": {"partyIdType": "ALIAS", "partyIdentifier": "p1"},
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::BAD_REQUEST, "payout without key");
    // no-show fee
    let r = http
        .post(format!("{base}/v1/no-show-fee"))
        .json(&serde_json::json!({
            "tenant_id": "t-k", "deposit_id": uuid::Uuid::new_v4(), "amount_cents": 100,
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::BAD_REQUEST, "no-show-fee without key");
    // empty string key is also rejected
    let r = http
        .post(format!("{base}/v1/deposits"))
        .json(&serde_json::json!({"tenant_id": "t-k", "amount_cents": 100, "currency": "NGN", "idempotency_key": "  "}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::BAD_REQUEST, "blank key");
}

/// P-13: holds and payouts are NGN-only (400 otherwise).
#[tokio::test]
async fn non_ngn_currency_is_rejected() {
    let base = spawn_server().await;
    let http = client();

    let r = http
        .post(format!("{base}/v1/deposits"))
        .json(&serde_json::json!({
            "tenant_id": "t-c", "amount_cents": 100, "currency": "USD",
            "idempotency_key": "ccy-hold",
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::BAD_REQUEST, "USD hold rejected");
    let r = http
        .post(format!("{base}/v1/payouts"))
        .json(&serde_json::json!({
            "tenant_id": "t-c", "amount_cents": 100, "currency": "USD",
            "payee": {"partyIdType": "ALIAS", "partyIdentifier": "p1"},
            "idempotency_key": "ccy-payout",
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::BAD_REQUEST, "USD payout rejected");
}

/// P-09/C1 auth matrix: internal token, gateway tenant binding, fail-closed.
#[tokio::test]
async fn auth_matrix_token_gateway_and_fail_closed() {
    let http = client();
    let body = serde_json::json!({
        "tenant_id": "t-a", "amount_cents": 250, "currency": "NGN",
        "idempotency_key": "auth-hold",
    });

    // 1. Token configured, dev escape off.
    let base = spawn_server_with_auth(Some("s3cret"), false).await.base;
    // no credentials => 401
    let r = http.post(format!("{base}/v1/deposits")).json(&body).send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::UNAUTHORIZED);
    // wrong token => 401
    let r = http.post(format!("{base}/v1/deposits")).header("x-internal-token", "wrong")
        .json(&body).send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::UNAUTHORIZED);
    // valid token => 201
    let r = http.post(format!("{base}/v1/deposits")).header("x-internal-token", "s3cret")
        .json(&serde_json::json!({"tenant_id":"t-a","amount_cents":250,"currency":"NGN","idempotency_key":"auth-t1"}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::CREATED);
    // gateway-bound tenant match + money role (K6) => 201; mismatch => 403
    let r = http.post(format!("{base}/v1/deposits"))
        .header("x-tenant-slugs", "t-a, t-b").header("x-user-roles", "owner")
        .json(&serde_json::json!({"tenant_id":"t-b","amount_cents":250,"currency":"NGN","idempotency_key":"auth-t2"}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::CREATED);
    let r = http.post(format!("{base}/v1/deposits"))
        .header("x-tenant-slugs", "t-a").header("x-user-roles", "owner")
        .json(&serde_json::json!({"tenant_id":"t-other","amount_cents":250,"currency":"NGN","idempotency_key":"auth-t3"}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::FORBIDDEN);
    // balance read requires tenant binding (P-09)
    let r = http.get(format!("{base}/v1/accounts/t-a/balance")).send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::UNAUTHORIZED);
    let r = http.get(format!("{base}/v1/accounts/t-a/balance"))
        .header("x-tenant-slugs", "t-x").send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::FORBIDDEN);
    let r = http.get(format!("{base}/v1/accounts/t-a/balance"))
        .header("x-tenant-slugs", "t-a").send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::OK);
    // /activities/* require the internal token
    let r = http.post(format!("{base}/activities/hold-deposit"))
        .json(&serde_json::json!({"tenant_id":"t-a","booking_id":"b-1","amount_cents":100}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::UNAUTHORIZED, "activity without token");
    let r = http.post(format!("{base}/activities/hold-deposit"))
        .header("x-internal-token", "s3cret")
        .json(&serde_json::json!({"tenant_id":"t-a","booking_id":"b-1","amount_cents":100}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::CREATED, "activity with token");
    // P-10: provision endpoint requires the internal token
    let r = http.post(format!("{base}/v1/internal/accounts/provision"))
        .json(&serde_json::json!({"tenant_id": "t-new"})).send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::UNAUTHORIZED);
    let r = http.post(format!("{base}/v1/internal/accounts/provision"))
        .header("x-internal-token", "s3cret")
        .json(&serde_json::json!({"tenant_id": "t-new"})).send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::OK);

    // 2. No token configured, dev escape off => money routes fail closed (503)
    //    without a gateway header, and activities are 503.
    let base = spawn_server_with_auth(None, false).await.base;
    let r = http.post(format!("{base}/v1/deposits")).json(&body).send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::SERVICE_UNAVAILABLE, "fail-closed 503");
    let r = http.post(format!("{base}/activities/hold-deposit"))
        .json(&serde_json::json!({"tenant_id":"t-a","booking_id":"b-2","amount_cents":100}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::SERVICE_UNAVAILABLE, "activities 503 fail-closed");
    // ... but a gateway-injected binding + money role still authorizes (C1+K6).
    let r = http.post(format!("{base}/v1/deposits"))
        .header("x-tenant-slugs", "t-a").header("x-user-roles", "admin")
        .json(&serde_json::json!({"tenant_id":"t-a","amount_cents":250,"currency":"NGN","idempotency_key":"auth-t4"}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::CREATED);
}

/// P-06: cross-tenant capture is 403 and leaves the hold untouched.
#[tokio::test]
async fn cross_tenant_capture_is_403() {
    let base = spawn_server().await;
    let http = client();
    let deposit_id = hold_via_api(&http, &base, "t-a", "xt-hold", 700).await;
    let r = http
        .post(format!("{base}/v1/deposits/{deposit_id}/capture"))
        .json(&serde_json::json!({"tenant_id": "t-b"}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::FORBIDDEN);
    // Same-tenant capture still works.
    let r = http
        .post(format!("{base}/v1/deposits/{deposit_id}/capture"))
        .json(&serde_json::json!({"tenant_id": "t-a"}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::OK);
}

/// P-11: refund(amount < hold amount) of a pending hold is 400, no void;
/// refund(amount == hold amount) voids it.
#[tokio::test]
async fn refund_partial_amount_of_pending_hold_is_400() {
    let base = spawn_server().await;
    let http = client();
    let deposit_id = hold_via_api(&http, &base, "t-r", "rf-hold", 1_000).await;

    let r = http
        .post(format!("{base}/v1/refunds"))
        .json(&serde_json::json!({
            "tenant_id": "t-r", "deposit_id": deposit_id, "amount_cents": 400,
            "idempotency_key": "rf-partial",
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::BAD_REQUEST, "partial amount must 400");

    // Hold is still pending (not silently voided): a full-amount refund works.
    let r = http
        .post(format!("{base}/v1/refunds"))
        .json(&serde_json::json!({
            "tenant_id": "t-r", "deposit_id": deposit_id, "amount_cents": 1_000,
            "idempotency_key": "rf-full",
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::CREATED);
}

/// K7 helper: register a beneficiary through the real API; returns its id.
async fn create_beneficiary_via_api(http: &reqwest::Client, base: &str, tenant: &str, party: &str) -> String {
    let resp = http
        .post(format!("{base}/v1/beneficiaries"))
        .json(&serde_json::json!({
            "tenant_id": tenant,
            "label": "Main payout account",
            "party_id_info": {"partyIdType": "ALIAS", "partyIdentifier": party},
        }))
        .send().await.unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::CREATED, "beneficiary create");
    resp.json::<serde_json::Value>().await.unwrap()["id"]
        .as_str().unwrap().to_string()
}

/// P-01 (C3): payouts are ledger-first — an over-limit payout is rejected
/// BEFORE any rail side effect, and an unreachable rail leaves the revenue
/// untouched (pending voided) with the attempt recorded for the reconciler
/// (replay of the same key is a 409, not a second rail call).
/// SPEC-W44 K7: the destination is a registered beneficiary_id.
#[tokio::test]
async fn payout_ledger_first_overdraft_and_rail_failure() {
    let base = spawn_server().await;
    let http = client();
    let beneficiary_id = create_beneficiary_via_api(&http, &base, "t-p", "p1").await;

    // 1. Over-limit payout (no revenue at all) => 422, rail never called.
    let r = http
        .post(format!("{base}/v1/payouts"))
        .json(&serde_json::json!({
            "tenant_id": "t-p", "amount_cents": 500, "currency": "NGN",
            "beneficiary_id": beneficiary_id, "idempotency_key": "po-over",
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::UNPROCESSABLE_ENTITY, "overdraft payout => 422");

    // 2. Earn revenue, then payout against an unreachable rail => 502 and
    //    the reserved funds are released (hold voided).
    let deposit_id = hold_via_api(&http, &base, "t-p", "po-hold", 1_000).await;
    let r = http
        .post(format!("{base}/v1/deposits/{deposit_id}/capture"))
        .json(&serde_json::json!({"tenant_id": "t-p"}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::OK);

    let r = http
        .post(format!("{base}/v1/payouts"))
        .json(&serde_json::json!({
            "tenant_id": "t-p", "amount_cents": 500, "currency": "NGN",
            "beneficiary_id": beneficiary_id, "idempotency_key": "po-fail",
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::BAD_GATEWAY, "unreachable rail => 502");

    // Funds released: the full revenue is payable again (a fresh over-limit
    // attempt of 1_001 fails, 1_000 passes the hold phase and then 502s).
    let r = http
        .post(format!("{base}/v1/payouts"))
        .json(&serde_json::json!({
            "tenant_id": "t-p", "amount_cents": 1_001, "currency": "NGN",
            "beneficiary_id": beneficiary_id, "idempotency_key": "po-over2",
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::UNPROCESSABLE_ENTITY);
    let r = http
        .post(format!("{base}/v1/payouts"))
        .json(&serde_json::json!({
            "tenant_id": "t-p", "amount_cents": 1_000, "currency": "NGN",
            "beneficiary_id": beneficiary_id, "idempotency_key": "po-again",
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::BAD_GATEWAY, "rail still down but hold passed");

    // 3. Replay of the failed payout's key: recorded attempt short-circuits
    //    with 409 (no second rail call, no double hold).
    let r = http
        .post(format!("{base}/v1/payouts"))
        .json(&serde_json::json!({
            "tenant_id": "t-p", "amount_cents": 500, "currency": "NGN",
            "beneficiary_id": beneficiary_id, "idempotency_key": "po-fail",
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::CONFLICT, "failed payout replay => 409");
}

// ===========================================================================
// SPEC-W44 tests: K6 money-role gate, K7 payee registry + deposit provenance,
// F15-03 dependency-aware healthz + /metrics
// ===========================================================================

/// K6 (S1-F7-01): a tenant MEMBER without a money role can bind the tenant
/// but must NOT mutate money — 403 on every mutation endpoint.
#[tokio::test]
async fn k6_member_without_money_role_is_403_on_all_mutations() {
    let base = spawn_server_with_auth(Some("s3cret"), false).await.base;
    let http = client();
    let member = [("x-tenant-slugs", "t-m"), ("x-user-roles", "member")];
    let send = |req: reqwest::RequestBuilder| {
        req.header(member[0].0, member[0].1).header(member[1].0, member[1].1)
    };

    let r = send(http.post(format!("{base}/v1/deposits")))
        .json(&serde_json::json!({"tenant_id":"t-m","amount_cents":100,"currency":"NGN","idempotency_key":"k6-hold"}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::FORBIDDEN, "member hold => 403");

    let r = send(http.post(format!("{base}/v1/refunds")))
        .json(&serde_json::json!({"tenant_id":"t-m","amount_cents":100,"idempotency_key":"k6-rf"}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::FORBIDDEN, "member refund => 403");

    let r = send(http.post(format!("{base}/v1/payouts")))
        .json(&serde_json::json!({"tenant_id":"t-m","amount_cents":100,"currency":"NGN","beneficiary_id": uuid::Uuid::new_v4(),"idempotency_key":"k6-po"}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::FORBIDDEN, "member payout => 403");

    let r = send(http.post(format!("{base}/v1/beneficiaries")))
        .json(&serde_json::json!({"tenant_id":"t-m","label":"x","party_id_info":{"partyIdType":"ALIAS","partyIdentifier":"p"}}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::FORBIDDEN, "member beneficiary create => 403");

    let r = send(http.get(format!("{base}/v1/beneficiaries?tenant_id=t-m")))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::FORBIDDEN, "member beneficiary list => 403");

    // The same caller with an owner role passes the gate (and binds).
    let r = http.post(format!("{base}/v1/deposits"))
        .header("x-tenant-slugs", "t-m").header("x-user-roles", "owner")
        .json(&serde_json::json!({"tenant_id":"t-m","amount_cents":100,"currency":"NGN","idempotency_key":"k6-ok"}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::CREATED, "owner hold => 201");

    // A bound tenant member with NO roles header at all: fail-closed 403.
    let r = http.post(format!("{base}/v1/deposits"))
        .header("x-tenant-slugs", "t-m")
        .json(&serde_json::json!({"tenant_id":"t-m","amount_cents":100,"currency":"NGN","idempotency_key":"k6-nr"}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::FORBIDDEN, "no roles header => 403");
}

/// V1 (SPEC-W44 K6): `POST /v1/payments/flutterwave/initialize` posts a
/// deposit hold — a money mutation — so it is role-gated exactly like
/// deposits. A tenant-bound member WITHOUT a money role gets 403 with zero
/// ledger side effects; an owner proceeds past the gate.
#[tokio::test]
async fn flutterwave_initialize_requires_money_role() {
    // Dev-escape posture + gateway headers (roles header present defeats the
    // escape); the flutterwave sub-router is merged like main.rs does.
    let base = spawn_server_merged(None, true).await.base;
    let http = client();
    let url = format!("{base}/v1/payments/flutterwave/initialize");

    // Member without a money role, otherwise-valid body => 403 BEFORE any
    // validation or ledger side effect.
    let r = http.post(&url)
        .header("x-tenant-slugs", "t-flw").header("x-user-roles", "member")
        .json(&serde_json::json!({
            "tenant_id": "t-flw", "amount_cents": 100, "currency": "NGN",
            "email": "a@b.c", "idempotency_key": "flw-k6-member",
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::FORBIDDEN, "member initialize => 403");

    // Zero side effects: no hold was posted for t-flw.
    let bal: serde_json::Value = http
        .get(format!("{base}/v1/accounts/t-flw/balance"))
        .send().await.unwrap().json().await.unwrap();
    for a in bal["accounts"].as_array().unwrap() {
        assert_eq!(a["debits_pending"].as_u64().unwrap(), 0, "rejected initialize leaked a hold");
        assert_eq!(a["debits_posted"].as_u64().unwrap(), 0);
        assert_eq!(a["credits_pending"].as_u64().unwrap(), 0);
        assert_eq!(a["credits_posted"].as_u64().unwrap(), 0);
    }

    // Owner passes the gate and proceeds to request validation (amount 0 is
    // rejected 400 AFTER the role gate — deterministic regardless of whether
    // FLUTTERWAVE_SECRET_KEY is configured in the test env).
    let r = http.post(&url)
        .header("x-tenant-slugs", "t-flw").header("x-user-roles", "owner")
        .json(&serde_json::json!({
            "tenant_id": "t-flw", "amount_cents": 0, "currency": "NGN",
            "email": "a@b.c", "idempotency_key": "flw-k6-owner",
        }))
        .send().await.unwrap();
    assert_eq!(
        r.status(),
        reqwest::StatusCode::BAD_REQUEST,
        "owner proceeds past the role gate (fails later validation, not 403)"
    );
}

/// K7: raw per-call payee is rejected 422; foreign/unknown/disabled
/// beneficiaries are rejected 422; a registered beneficiary pays out
/// end-to-end against a committed rail stub.
#[tokio::test]
async fn k7_beneficiary_registry_flow() {
    let base = spawn_server().await;
    let http = client();

    // Raw payee (the pre-K7 exploit shape) is rejected 422 even with a key.
    let r = http.post(format!("{base}/v1/payouts"))
        .json(&serde_json::json!({
            "tenant_id": "t-b", "amount_cents": 100, "currency": "NGN",
            "payee": {"partyIdType": "ALIAS", "partyIdentifier": "attacker"},
            "idempotency_key": "k7-raw",
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::UNPROCESSABLE_ENTITY, "raw payee => 422");

    // Unknown beneficiary id => 422. V1: the body is the UNIFORM
    // "invalid beneficiary" contract — indistinguishable from the foreign
    // and disabled cases below (no cross-tenant existence oracle).
    let unknown_id = uuid::Uuid::new_v4();
    let r = http.post(format!("{base}/v1/payouts"))
        .json(&serde_json::json!({
            "tenant_id": "t-b", "amount_cents": 100, "currency": "NGN",
            "beneficiary_id": unknown_id, "idempotency_key": "k7-unknown",
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::UNPROCESSABLE_ENTITY, "unknown beneficiary => 422");
    let body: serde_json::Value = r.json().await.unwrap();
    assert_eq!(
        body["error"].as_str().unwrap(),
        format!("invalid beneficiary {unknown_id}"),
        "unknown beneficiary uses the uniform body"
    );

    // Foreign beneficiary (registered to another tenant) => 422, SAME body
    // shape as unknown (a distinct message would be an existence oracle).
    let foreign_id = create_beneficiary_via_api(&http, &base, "t-other", "p-foreign").await;
    let r = http.post(format!("{base}/v1/payouts"))
        .json(&serde_json::json!({
            "tenant_id": "t-b", "amount_cents": 100, "currency": "NGN",
            "beneficiary_id": foreign_id, "idempotency_key": "k7-foreign",
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::UNPROCESSABLE_ENTITY, "foreign beneficiary => 422");
    let body: serde_json::Value = r.json().await.unwrap();
    assert_eq!(
        body["error"].as_str().unwrap(),
        format!("invalid beneficiary {foreign_id}"),
        "foreign beneficiary is indistinguishable from unknown"
    );

    // Disabled beneficiary => 422, SAME uniform body. (List shows it flagged
    // as disabled.)
    let own_id = create_beneficiary_via_api(&http, &base, "t-b", "p-own").await;
    let r = http.post(format!("{base}/v1/beneficiaries/{own_id}/disable"))
        .json(&serde_json::json!({"tenant_id": "t-b"}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::OK, "disable");
    let r = http.post(format!("{base}/v1/payouts"))
        .json(&serde_json::json!({
            "tenant_id": "t-b", "amount_cents": 100, "currency": "NGN",
            "beneficiary_id": own_id, "idempotency_key": "k7-disabled",
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::UNPROCESSABLE_ENTITY, "disabled beneficiary => 422");
    let body: serde_json::Value = r.json().await.unwrap();
    assert_eq!(
        body["error"].as_str().unwrap(),
        format!("invalid beneficiary {own_id}"),
        "disabled beneficiary is indistinguishable from unknown/foreign"
    );
    let list: serde_json::Value = http
        .get(format!("{base}/v1/beneficiaries?tenant_id=t-b"))
        .send().await.unwrap().json().await.unwrap();
    let disabled = list.as_array().unwrap().iter()
        .find(|b| b["id"] == own_id).expect("listed");
    assert!(disabled["disabled_at"].is_string(), "list flags disabled");

    // Cross-tenant disable is not possible (indistinguishable 422).
    let r = http.post(format!("{base}/v1/beneficiaries/{foreign_id}/disable"))
        .json(&serde_json::json!({"tenant_id": "t-b"}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::UNPROCESSABLE_ENTITY);

    // Zero side effects: every rejected payout above (raw payee, unknown,
    // foreign, disabled) left the t-b ledger untouched — no holds, no posts.
    let bal: serde_json::Value = http
        .get(format!("{base}/v1/accounts/t-b/balance"))
        .send().await.unwrap().json().await.unwrap();
    for a in bal["accounts"].as_array().unwrap() {
        assert_eq!(a["debits_pending"].as_u64().unwrap(), 0, "rejected payout leaked a pending hold");
        assert_eq!(a["debits_posted"].as_u64().unwrap(), 0, "rejected payout leaked a post");
        assert_eq!(a["credits_pending"].as_u64().unwrap(), 0, "rejected payout leaked a pending credit");
        assert_eq!(a["credits_posted"].as_u64().unwrap(), 0, "rejected payout leaked a credit");
    }
}

/// K7 happy path: payout to a registered beneficiary COMMITS against a
/// stubbed Mojaloop rail (quote + transfer COMMITTED) and the ledger posts.
#[tokio::test]
async fn k7_payout_to_registered_beneficiary_commits() {
    // Stub rail: POST /quotes {} (terms accepted as requested),
    // POST /transfers COMMITTED.
    async fn quotes() -> axum::Json<serde_json::Value> {
        axum::Json(serde_json::json!({}))
    }
    async fn transfers() -> axum::Json<serde_json::Value> {
        axum::Json(serde_json::json!({
            "transferState": "COMMITTED",
            "completedTimestamp": "2026-08-22T00:00:00Z"
        }))
    }
    let rail_app = axum::Router::new()
        .route("/quotes", axum::routing::post(quotes))
        .route("/transfers", axum::routing::post(transfers));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let rail_addr = listener.local_addr().unwrap();
    tokio::spawn(async move { axum::serve(listener, rail_app).await.unwrap(); });

    let handles = spawn_server_full(None, true, &format!("http://{rail_addr}")).await;
    let base = &handles.base;
    let http = client();

    // Earn revenue.
    let deposit_id = hold_via_api(&http, base, "t-c", "k7-hold", 2_000).await;
    let r = http.post(format!("{base}/v1/deposits/{deposit_id}/capture"))
        .json(&serde_json::json!({"tenant_id": "t-c"})).send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::OK);

    let beneficiary_id = create_beneficiary_via_api(&http, base, "t-c", "payee-7").await;
    let r = http.post(format!("{base}/v1/payouts"))
        .json(&serde_json::json!({
            "tenant_id": "t-c", "amount_cents": 1_500, "currency": "NGN",
            "beneficiary_id": beneficiary_id, "idempotency_key": "k7-commit",
        }))
        .send().await.unwrap();
    let status = r.status();
    let body = r.text().await.unwrap_or_default();
    assert_eq!(status, reqwest::StatusCode::CREATED, "committed payout: {body}");
    let json: serde_json::Value = serde_json::from_str(&body).unwrap();
    assert_eq!(json["mojaloop"]["state"], "COMMITTED");

    // Idempotent replay returns the stored committed outcome.
    let r = http.post(format!("{base}/v1/payouts"))
        .json(&serde_json::json!({
            "tenant_id": "t-c", "amount_cents": 1_500, "currency": "NGN",
            "beneficiary_id": beneficiary_id, "idempotency_key": "k7-commit",
        }))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::CREATED, "replay");
}

/// K7: a human deposit records provenance (declared_by = X-User-Id,
/// psp_reference) in the registry store; write-once on replay.
#[tokio::test]
async fn k7_deposit_provenance_records_declared_by() {
    let handles = spawn_server_with_auth(Some("s3cret"), false).await;
    let base = &handles.base;
    let http = client();

    let resp = http.post(format!("{base}/v1/deposits"))
        .header("x-tenant-slugs", "t-d")
        .header("x-user-roles", "owner")
        .header("x-user-id", "user-42")
        .json(&serde_json::json!({
            "tenant_id": "t-d", "amount_cents": 900, "currency": "NGN",
            "idempotency_key": "prov-1", "psp_reference": "fw-tx-998",
        }))
        .send().await.unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::CREATED);
    let deposit_id = resp.json::<serde_json::Value>().await.unwrap()["deposit_id"]
        .as_str().unwrap().to_string();

    let prov = handles.registry.deposit_provenance(&deposit_id).await.unwrap()
        .expect("provenance recorded");
    assert_eq!(prov.declared_by, "user-42");
    assert_eq!(prov.psp_reference.as_deref(), Some("fw-tx-998"));
    assert_eq!(prov.tenant_id, "t-d");

    // Idempotent replay of the same key: provenance stays write-once even if
    // a different user header is presented.
    let resp = http.post(format!("{base}/v1/deposits"))
        .header("x-tenant-slugs", "t-d")
        .header("x-user-roles", "owner")
        .header("x-user-id", "user-99")
        .json(&serde_json::json!({
            "tenant_id": "t-d", "amount_cents": 900, "currency": "NGN",
            "idempotency_key": "prov-1",
        }))
        .send().await.unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::CREATED);
    let prov = handles.registry.deposit_provenance(&deposit_id).await.unwrap().unwrap();
    assert_eq!(prov.declared_by, "user-42", "first record wins");
}

/// K5: activity payloads accept tenant_slug (preferred); a uuid-only
/// tenant_id still works (legacy callers, WARN logged).
#[tokio::test]
async fn k5_activities_accept_tenant_slug() {
    let base = spawn_server_with_auth(Some("s3cret"), false).await.base;
    let http = client();

    let r = http.post(format!("{base}/activities/hold-deposit"))
        .header("x-internal-token", "s3cret")
        .json(&serde_json::json!({"tenant_slug":"acme","booking_id":"b-k5","amount_cents":100}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::CREATED, "tenant_slug activity");

    let r = http.post(format!("{base}/activities/hold-deposit"))
        .header("x-internal-token", "s3cret")
        .json(&serde_json::json!({"tenant_id": uuid::Uuid::new_v4(),"booking_id":"b-k5u","amount_cents":100}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::CREATED, "legacy uuid-only accepted (WARN)");

    let r = http.post(format!("{base}/activities/hold-deposit"))
        .header("x-internal-token", "s3cret")
        .json(&serde_json::json!({"booking_id":"b-k5n","amount_cents":100}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::BAD_REQUEST, "no tenant at all => 400");

    // Path-safety (K5 item 2): separators/traversal rejected.
    let r = http.post(format!("{base}/activities/hold-deposit"))
        .header("x-internal-token", "s3cret")
        .json(&serde_json::json!({"tenant_slug":"a/b","booking_id":"b-k5p","amount_cents":100}))
        .send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::BAD_REQUEST, "path-unsafe tenant => 400");
}

/// F15-03: healthz is dependency-aware — dead-lettered commands degrade the
/// service (503); /metrics exposes the command/payout counters.
#[tokio::test]
async fn f15_healthz_degrades_on_dead_letters_and_metrics_expose_counters() {
    let handles = spawn_server_with_auth(None, true).await;
    let base = &handles.base;
    let http = client();

    // Healthy: sim ledger ping ok, no PG configured, no dead letters.
    let r = http.get(format!("{base}/healthz")).send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::OK);
    let body: serde_json::Value = r.json().await.unwrap();
    assert_eq!(body["status"], "ok");
    assert_eq!(body["checks"]["postgres"], "not-configured");

    // Dead-lettered commands => degraded + 503.
    handles
        .commands_dead_lettered
        .fetch_add(1, Ordering::Relaxed);
    let r = http.get(format!("{base}/healthz")).send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::SERVICE_UNAVAILABLE);
    let body: serde_json::Value = r.json().await.unwrap();
    assert_eq!(body["status"], "degraded");
    assert_eq!(body["commands_dead_lettered"], 1);

    // /metrics: prometheus text with the counters.
    let r = http.get(format!("{base}/metrics")).send().await.unwrap();
    assert_eq!(r.status(), reqwest::StatusCode::OK);
    let text = r.text().await.unwrap();
    assert!(text.contains("payments_commands_dead_lettered 1"), "{text}");
    assert!(text.contains("payments_commands_processed_total"), "{text}");
    assert!(text.contains("payments_payout_attempts_total{outcome=\"committed\"}"), "{text}");
}
