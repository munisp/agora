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
#[path = "../src/routes.rs"]
mod routes;

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
    pub events_published: Arc<AtomicU64>,
    pub events_failed: Arc<AtomicU64>,
    pub commands_dead_lettered: Arc<AtomicU64>,
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
    }
}

/// Boot the real router on an ephemeral port; return its base URL.
async fn spawn_server() -> String {
    let state = AppState {
        ledger: Arc::new(ledger::sim::SimLedgerClient::new(0)),
        outbox: dapr::DaprOutbox::new(
            "http://127.0.0.1:1".to_string(),
            "pubsub".to_string(),
            "opendesk.payments.events".to_string(),
        ),
        mojaloop: mojaloop::MojaloopAdapter::new("http://127.0.0.1:1".to_string()),
        flutterwave: flutterwave::FlutterwaveAdapter::from_env(),
        config: Arc::new(test_config()),
        dlq: Arc::new(consumer::UnavailableDlqSink),
        events_published: Arc::new(AtomicU64::new(0)),
        events_failed: Arc::new(AtomicU64::new(0)),
        commands_dead_lettered: Arc::new(AtomicU64::new(0)),
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

/// `POST /v1/deposits/:id/capture`: a deposit held through the real API
/// must be capturable through the parameterized route (end-to-end 200).
/// Pre-fix this 404'd at the match layer.
#[tokio::test]
async fn capture_route_matches_concrete_deposit_id() {
    let base = spawn_server().await;
    let http = client();

    // Hold a deposit via the static route (this one always worked).
    let held = http
        .post(format!("{base}/v1/deposits"))
        .json(&serde_json::json!({
            "tenant_id": "t-smoke",
            "amount_cents": 500,
            "currency": "USD",
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
