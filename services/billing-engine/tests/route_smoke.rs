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
//! that requests with concrete path segments REACH THE HANDLER. Since
//! SPEC-W44 K1 the tenant check is a BINDING of the caller-supplied tenant
//! to the gateway-injected `X-Tenant-Slugs` claim: a request whose slugs do
//! NOT list the target tenant is rejected 403 BEFORE any DB access (routes
//! with a tenant path/query/body param). Invoice-ID-addressed routes look up
//! the invoice to learn its tenant first, so on the lazy never-dialing pool
//! they deterministically answer 500 (query error) — which equally proves
//! the match layer routed to the handler. A match-layer miss answers 404
//! with an empty body instead.
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

/// Mirror of `src/main.rs::http_client` (referenced as `crate::http_client`
/// by the included consumer module's tests).
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
        dlq_topic: "opendesk.dlq".to_string(),
        billing_ledger_impl: "sim".to_string(),
        internal_token: INTERNAL_TOKEN.to_string(),
        paystack_secret_key: None,
        paystack_default_email: "smoke@example.com".to_string(),
        paystack_callback_url: "http://127.0.0.1/callback".to_string(),
        billing_static_account: "OpenDesk Smoke/0000000000".to_string(),
        billing_merchant_name: "OpenDesk Smoke".to_string(),
        money_roles: vec!["owner".to_string(), "admin".to_string()],
        trust_direct_tenant: false,
        // Identity resolution DISABLED by default in this harness: the
        // uuid-form K1 binding then fails closed with a deterministic 403
        // (no network), keeping the route-match assertions hermetic. The
        // resolution tests below spawn a mock identity and override this.
        identity_base_url: String::new(),
        identity_internal_token: None,
        tenant_cache_ttl_s: 60,
        dunning_interval_s: 3600,
        invoice_due_days: 14,
    }
}

/// Boot the real router on an ephemeral port; return its base URL.
async fn spawn_server() -> String {
    spawn_server_with(test_config()).await
}

/// Boot the real router with an explicit config (the K1 identity-resolution
/// tests point `identity_base_url` at a mock identity-service).
async fn spawn_server_with(cfg: config::Config) -> String {
    // Lazy pools dial 127.0.0.1:1 (connection refused) with a SHORT acquire
    // budget: routes that reach a query (id-addressed invoice lookups,
    // dependency-aware /healthz) fail fast with a deterministic error instead
    // of hanging on sqlx's 30s default acquire timeout.
    use std::str::FromStr;
    let opts = sqlx::postgres::PgConnectOptions::from_str(
        "postgres://127.0.0.1:1/billing_route_smoke",
    )
    .expect("parse smoke DSN");
    let pool = sqlx::postgres::PgPoolOptions::new()
        .acquire_timeout(std::time::Duration::from_secs(1))
        .connect_lazy_with(opts);
    let state = AppState {
        pool: pool.clone(),
        internal_pool: pool,
        ledger: Arc::new(ledger::SimLedgerClient::new()),
        producer: None,
        http: reqwest::Client::new(),
        identity: Arc::new(identity::SlugResolver::new(
            reqwest::Client::new(),
            &cfg.identity_base_url,
            cfg.identity_internal_token.clone(),
            std::time::Duration::from_secs(cfg.tenant_cache_ttl_s),
        )),
        config: Arc::new(cfg),
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
    format!("http://{addr}")
}

fn client() -> reqwest::Client {
    reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(5))
        .build()
        .unwrap()
}

/// A matched /v1/* route carrying a tenant path/query/body param answers 403
/// from the K1 binding (X-Tenant-Slugs does not list the tenant) BEFORE any
/// DB access. Anything else (404 at the match layer, extractor failures)
/// fails the assertion.
async fn assert_reaches_tenant_check(resp: reqwest::Response, desc: &str) {
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    assert_eq!(
        status,
        reqwest::StatusCode::FORBIDDEN,
        "{desc}: route must match and reach the handler (expected 403 from the \
         K1 X-Tenant-Slugs binding); got {status} with body {body:?}. A 404 here \
         means the path template did not match (axum 0.7 `:param` syntax defect)."
    );
    assert!(
        body.contains("X-Tenant-Slugs"),
        "{desc}: expected the handler's K1 binding error, got {body:?}"
    );
}

/// Invoice-ID-addressed routes learn the tenant from the invoice row, so the
/// handler's first act is an (internal-pool) lookup: on this lazy,
/// never-dialing pool a matched route deterministically answers 500 (query
/// error) — which proves the match layer reached the handler.
async fn assert_reaches_db_bound_handler(resp: reqwest::Response, desc: &str) {
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    assert_eq!(
        status,
        reqwest::StatusCode::INTERNAL_SERVER_ERROR,
        "{desc}: route must match and reach the handler (expected 500 from the \
         lazy pool lookup); got {status} with body {body:?}. A 404 here means \
         the path template did not match (axum 0.7 `:param` syntax defect)."
    );
    assert!(
        !body.contains("Wrong number of path arguments"),
        "{desc}: the pre-fix literal-segment signature must be gone, got {body:?}"
    );
}

/// K1 gateway headers whose slugs do NOT bind the target tenant (the
/// deterministic pre-DB rejection signal for tenant-param routes).
const FOREIGN_SLUGS: &str = "some-other-tenant";

/// Every parameterized route in the billing route table: a request carrying
/// a concrete path segment must match and reach the handler.
#[tokio::test]
async fn parameterized_routes_match_concrete_segments() {
    let base = spawn_server().await;
    let http = client();
    let id = uuid::Uuid::new_v4();
    let tenant = uuid::Uuid::new_v4();

    // PUT /v1/rate-cards/:tenant_id (tenant path param => K1 403 pre-DB)
    let resp = http
        .put(format!("{base}/v1/rate-cards/{tenant}"))
        .header("x-tenant-slugs", FOREIGN_SLUGS)
        .json(&serde_json::json!({
            "metric": "seat",
            "unit_price_cents": 100,
        }))
        .send()
        .await
        .unwrap();
    assert_reaches_tenant_check(resp, "PUT /v1/rate-cards/:tenant_id").await;

    // GET /v1/invoices/:id (id-addressed: handler reaches the DB lookup)
    let resp = http
        .get(format!("{base}/v1/invoices/{id}"))
        .header("x-tenant-slugs", FOREIGN_SLUGS)
        .send()
        .await
        .unwrap();
    assert_reaches_db_bound_handler(resp, "GET /v1/invoices/:id").await;

    // POST /v1/invoices/:id/issue
    let resp = http
        .post(format!("{base}/v1/invoices/{id}/issue"))
        .header("x-tenant-slugs", FOREIGN_SLUGS)
        .send()
        .await
        .unwrap();
    assert_reaches_db_bound_handler(resp, "POST /v1/invoices/:id/issue").await;

    // POST /v1/invoices/:id/void
    let resp = http
        .post(format!("{base}/v1/invoices/{id}/void"))
        .header("x-tenant-slugs", FOREIGN_SLUGS)
        .send()
        .await
        .unwrap();
    assert_reaches_db_bound_handler(resp, "POST /v1/invoices/:id/void").await;

    // POST /v1/invoices/:id/payment-link
    let resp = http
        .post(format!("{base}/v1/invoices/{id}/payment-link"))
        .header("x-tenant-slugs", FOREIGN_SLUGS)
        .send()
        .await
        .unwrap();
    assert_reaches_db_bound_handler(resp, "POST /v1/invoices/:id/payment-link").await;

    // GET /v1/invoices/:id/qr
    let resp = http
        .get(format!("{base}/v1/invoices/{id}/qr"))
        .header("x-tenant-slugs", FOREIGN_SLUGS)
        .send()
        .await
        .unwrap();
    assert_reaches_db_bound_handler(resp, "GET /v1/invoices/:id/qr").await;
}

/// Static routes remain anchors: dependency-aware healthz (F15-07: the lazy
/// pool fails its 2s PG ping => degraded 503), K1-bound handlers 403 on a
/// foreign slugs claim, and the credential gate stays 401 without any.
#[tokio::test]
async fn static_routes_still_behave() {
    let base = spawn_server().await;
    let http = client();
    let tenant = uuid::Uuid::new_v4();

    // /healthz is credential-exempt; with an unreachable Postgres it is
    // dependency-aware (F15-07): degraded + 503, check reported.
    let resp = http.get(format!("{base}/healthz")).send().await.unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::SERVICE_UNAVAILABLE);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["status"], "degraded");
    assert_eq!(body["checks"]["postgres"], "fail");

    // /metrics is credential-exempt and exposes the counters.
    let resp = http.get(format!("{base}/metrics")).send().await.unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK);
    let text = resp.text().await.unwrap();
    assert!(text.contains("billing_events_published_total"), "{text}");
    assert!(text.contains("billing_usage_dead_lettered"), "{text}");

    // GET /v1/invoices (static) still matches and reaches the handler.
    let resp = http
        .get(format!("{base}/v1/invoices?tenant_id={tenant}"))
        .header("x-tenant-slugs", FOREIGN_SLUGS)
        .send()
        .await
        .unwrap();
    assert_reaches_tenant_check(resp, "GET /v1/invoices").await;

    // POST /v1/invoices/generate (static) still matches and reaches the handler.
    let resp = http
        .post(format!("{base}/v1/invoices/generate"))
        .header("x-tenant-slugs", FOREIGN_SLUGS)
        .json(&serde_json::json!({
            "tenant_id": tenant,
            "period": "2026-01",
        }))
        .send()
        .await
        .unwrap();
    assert_reaches_tenant_check(resp, "POST /v1/invoices/generate").await;

    // The credential gate still fails closed (401 without internal token AND
    // without a gateway-injected slugs header).
    let resp = http
        .get(format!("{base}/v1/invoices?tenant_id={tenant}"))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::UNAUTHORIZED);

    // K6: a gateway caller bound to the tenant but WITHOUT a money role is
    // 403 on mutations (S1-R1 class: membership alone never mutates money).
    let resp = http
        .post(format!("{base}/v1/invoices/generate"))
        .header("x-tenant-slugs", tenant.to_string())
        .header("x-user-roles", "member")
        .json(&serde_json::json!({
            "tenant_id": tenant,
            "period": "2026-01",
        }))
        .send()
        .await
        .unwrap();
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    assert_eq!(
        status,
        reqwest::StatusCode::FORBIDDEN,
        "member without money role must 403 on generate; got {status} {body:?}"
    );
    assert!(body.contains("money role"), "{body:?}");
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

// ---------------------------------------------------------------------------
// SPEC-W44 F1: K1 uuid binding via identity slug->uuid resolution
// ---------------------------------------------------------------------------

/// Mock identity-service serving the GET /v1/tenants/{slug} contract
/// (identity-service/internal/httpapi/server.go getTenant): 200 `{"id":
/// <uuid>}` for mapped slugs, 404 for unknown slugs, 500 for every request
/// in `fail` mode (identity-outage simulation). Returns the base URL and a
/// hit counter so tests can assert caching behavior.
async fn spawn_mock_identity(
    map: std::collections::HashMap<String, uuid::Uuid>,
    fail: bool,
) -> (String, Arc<AtomicU64>) {
    use axum::extract::{Path, State};
    use axum::routing::get;
    use axum::Json;

    #[derive(Clone)]
    struct Mock {
        map: std::collections::HashMap<String, uuid::Uuid>,
        fail: bool,
        hits: Arc<AtomicU64>,
    }
    async fn get_tenant(
        State(m): State<Mock>,
        Path(slug): Path<String>,
    ) -> (reqwest::StatusCode, Json<serde_json::Value>) {
        m.hits.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        if m.fail {
            return (
                reqwest::StatusCode::INTERNAL_SERVER_ERROR,
                Json(serde_json::json!({"error": "mock identity outage"})),
            );
        }
        match m.map.get(&slug) {
            Some(id) => (
                reqwest::StatusCode::OK,
                Json(serde_json::json!({"id": id.to_string(), "slug": slug})),
            ),
            None => (
                reqwest::StatusCode::NOT_FOUND,
                Json(serde_json::json!({"error": "tenant not found"})),
            ),
        }
    }

    let hits = Arc::new(AtomicU64::new(0));
    let app = axum::Router::new()
        .route("/v1/tenants/:slug", get(get_tenant))
        .with_state(Mock {
            map,
            fail,
            hits: hits.clone(),
        });
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind mock identity");
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.expect("serve mock identity");
    });
    (format!("http://{addr}"), hits)
}

/// Config whose identity resolution points at the given mock base URL.
fn config_with_identity(base_url: &str) -> config::Config {
    let mut cfg = test_config();
    cfg.identity_base_url = base_url.to_string();
    cfg.identity_internal_token = Some("mock-identity-token".to_string());
    cfg
}

/// The pre-DB success signal for tenant-param routes on this harness: the
/// binding PASSED and the handler reached the (lazy, never-dialing) pool.
async fn assert_passed_binding_hits_db(resp: reqwest::Response, desc: &str) {
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    assert_eq!(
        status,
        reqwest::StatusCode::INTERNAL_SERVER_ERROR,
        "{desc}: the K1 binding must PASS (then fail at the lazy pool with 500); \
         got {status} with body {body:?}"
    );
}

/// SPEC-W44 F1: a gateway caller whose X-Tenant-Slugs claim lists a SLUG
/// that identity resolves to the requested tenant uuid is bound (the W39-era
/// code could never bind uuid params to slug claims — the human path only
/// "worked" through the gateway token injection). Direct slug match still
/// short-circuits WITHOUT an identity call; resolutions are TTL-cached.
#[tokio::test]
async fn k1_uuid_binding_resolves_slugs_via_identity() {
    let tenant = uuid::Uuid::new_v4();
    let (base, hits) = spawn_mock_identity(
        std::collections::HashMap::from([("acme".to_string(), tenant)]),
        false,
    )
    .await;
    let server = spawn_server_with(config_with_identity(&base)).await;
    let http = client();

    // 1. Direct slug match (the param string-equals a claimed slug): binding
    //    passes with NO identity call (slug-keyed tenant escape hatch).
    let resp = http
        .get(format!("{server}/v1/invoices?tenant_id={tenant}"))
        .header("x-tenant-slugs", tenant.to_string())
        .send()
        .await
        .unwrap();
    assert_passed_binding_hits_db(resp, "direct slug-as-uuid match").await;
    assert_eq!(
        hits.load(std::sync::atomic::Ordering::Relaxed),
        0,
        "direct match must not hit identity"
    );

    // 2. Slug claim resolved via identity: GET /v1/invoices?tenant_id=<uuid>
    //    with X-Tenant-Slugs: acme binds once identity maps acme -> tenant.
    let resp = http
        .get(format!("{server}/v1/invoices?tenant_id={tenant}"))
        .header("x-tenant-slugs", "acme")
        .send()
        .await
        .unwrap();
    assert_passed_binding_hits_db(resp, "uuid binding via identity resolution").await;
    assert_eq!(hits.load(std::sync::atomic::Ordering::Relaxed), 1);

    // 3. Second identical call is served from the TTL cache (no extra hit).
    let resp = http
        .get(format!("{server}/v1/invoices?tenant_id={tenant}"))
        .header("x-tenant-slugs", "acme")
        .send()
        .await
        .unwrap();
    assert_passed_binding_hits_db(resp, "cached uuid binding").await;
    assert_eq!(
        hits.load(std::sync::atomic::Ordering::Relaxed),
        1,
        "resolution must be TTL-cached"
    );

    // 4. Several slugs in the claim: ANY match binds.
    let resp = http
        .get(format!("{server}/v1/invoices?tenant_id={tenant}"))
        .header("x-tenant-slugs", "unknown-tenant,acme")
        .send()
        .await
        .unwrap();
    assert_passed_binding_hits_db(resp, "any-slug binding").await;
}

/// SPEC-W44 F1 (negative): identity resolves the claimed slug to a
/// DIFFERENT uuid than requested -> 403; unknown slug (404) -> 403.
#[tokio::test]
async fn k1_uuid_binding_rejects_foreign_and_unknown_slugs() {
    let tenant = uuid::Uuid::new_v4();
    let other = uuid::Uuid::new_v4();
    let (base, _hits) = spawn_mock_identity(
        std::collections::HashMap::from([("acme".to_string(), other)]),
        false,
    )
    .await;
    let server = spawn_server_with(config_with_identity(&base)).await;
    let http = client();

    // Claimed slug resolves to a different tenant than the param.
    let resp = http
        .get(format!("{server}/v1/invoices?tenant_id={tenant}"))
        .header("x-tenant-slugs", "acme")
        .send()
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        reqwest::StatusCode::FORBIDDEN,
        "slug resolving to a different uuid must 403"
    );

    // Claimed slug unknown to identity (404) can never bind.
    let resp = http
        .get(format!("{server}/v1/invoices?tenant_id={tenant}"))
        .header("x-tenant-slugs", "no-such-tenant")
        .send()
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        reqwest::StatusCode::FORBIDDEN,
        "unknown slug must 403"
    );
}

/// SPEC-W44 F1 (fail-closed): identity 5xx or unreachable -> 503, NEVER a
/// silent allow; resolution disabled (empty IDENTITY_BASE_URL) -> 403.
#[tokio::test]
async fn k1_uuid_binding_fails_closed_on_identity_errors() {
    let tenant = uuid::Uuid::new_v4();

    // Identity answers 500 (outage): 503, not 403/allow.
    let (base, _hits) = spawn_mock_identity(std::collections::HashMap::new(), true).await;
    let server = spawn_server_with(config_with_identity(&base)).await;
    let resp = client()
        .get(format!("{server}/v1/invoices?tenant_id={tenant}"))
        .header("x-tenant-slugs", "acme")
        .send()
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        reqwest::StatusCode::SERVICE_UNAVAILABLE,
        "identity outage must fail closed with 503"
    );

    // Identity unreachable (dead port): 503.
    let server = spawn_server_with(config_with_identity("http://127.0.0.1:1")).await;
    let resp = client()
        .get(format!("{server}/v1/invoices?tenant_id={tenant}"))
        .header("x-tenant-slugs", "acme")
        .send()
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        reqwest::StatusCode::SERVICE_UNAVAILABLE,
        "unreachable identity must fail closed with 503"
    );

    // Resolution disabled (empty IDENTITY_BASE_URL, the spawn_server
    // default): deterministic 403 without any network call.
    let server = spawn_server().await;
    let resp = client()
        .get(format!("{server}/v1/invoices?tenant_id={tenant}"))
        .header("x-tenant-slugs", "acme")
        .send()
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        reqwest::StatusCode::FORBIDDEN,
        "disabled resolution must fail closed with 403"
    );
}

/// SPEC-W44 F1 + K6 end-to-end-ish policy pinning on the smoke harness:
/// with a valid uuid binding via identity, a MEMBER (no money role) is 403
/// on mutations BEFORE any DB access, while an OWNER passes both gates (the
/// lazy-pool 500 proves the handler ran). The real-PG 200/201 counterparts
/// live in tests/pg_k1_binding.rs.
#[tokio::test]
async fn k1_uuid_binding_plus_k6_role_gate() {
    let tenant = uuid::Uuid::new_v4();
    let (base, _hits) = spawn_mock_identity(
        std::collections::HashMap::from([("acme".to_string(), tenant)]),
        false,
    )
    .await;
    let server = spawn_server_with(config_with_identity(&base)).await;
    let http = client();

    // Member: bound via identity but no money role -> 403 on generate.
    let resp = http
        .post(format!("{server}/v1/invoices/generate"))
        .header("x-tenant-slugs", "acme")
        .header("x-user-roles", "member")
        .json(&serde_json::json!({ "tenant_id": tenant, "period": "2026-01" }))
        .send()
        .await
        .unwrap();
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    assert_eq!(
        status,
        reqwest::StatusCode::FORBIDDEN,
        "member without money role must 403 on generate; got {status} {body:?}"
    );
    assert!(body.contains("money role"), "{body:?}");

    // No X-User-Roles header at all: fail-closed 403 (K6).
    let resp = http
        .post(format!("{server}/v1/invoices/generate"))
        .header("x-tenant-slugs", "acme")
        .json(&serde_json::json!({ "tenant_id": tenant, "period": "2026-02" }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::FORBIDDEN);

    // Owner: binding + money role both pass -> the handler reaches the DB.
    let resp = http
        .post(format!("{server}/v1/invoices/generate"))
        .header("x-tenant-slugs", "acme")
        .header("x-user-roles", "owner")
        .json(&serde_json::json!({ "tenant_id": tenant, "period": "2026-03" }))
        .send()
        .await
        .unwrap();
    assert_passed_binding_hits_db(resp, "owner generate passes K1+K6").await;
}

// ---------------------------------------------------------------------------
// SPEC-W44 F1: the APISIX deployment bypass must stay removed — no OIDC
// route may inject x-internal-token (doing so authenticates every human
// gateway call as an internal service caller and no-ops billing's K1/K6
// gates).
// ---------------------------------------------------------------------------

#[test]
fn apisix_oidc_routes_never_inject_internal_token() {
    let yaml = std::fs::read_to_string(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../infra/apisix/apisix.yaml"
    ))
    .expect("read infra/apisix/apisix.yaml");

    // A `headers.set` injection is a yaml mapping entry
    // `x-internal-token: <value>`; comment mentions and the strip-list
    // entry `"x-internal-token",` are fine.
    for (lineno, line) in yaml.lines().enumerate() {
        let t = line.trim_start();
        if t.starts_with("x-internal-token:") {
            let value = t["x-internal-token:".len()..].trim();
            assert!(
                value.is_empty(),
                "apisix.yaml:{} injects x-internal-token ({line:?}) — the \
                 SPEC-W44 F1 deployment bypass must stay removed: service \
                 callers present the token in-cluster, the gateway only \
                 strips it",
                lineno + 1
            );
        }
    }

    // The api-billing route still exists and still rewrites the prefix
    // (everything else about the route is unchanged).
    let start = yaml
        .find("- id: api-billing")
        .expect("api-billing route present");
    let block = &yaml[start..];
    let end = block[1..]
        .find("\n  - id: ")
        .map(|i| i + 1)
        .unwrap_or(block.len());
    let block = &block[..end];
    assert!(
        block.contains("openid-connect"),
        "api-billing must stay OIDC-protected"
    );
    assert!(
        block.contains("regex_uri"),
        "api-billing must keep its prefix rewrite"
    );
    assert!(
        block.contains("serverless-pre-function"),
        "api-billing must keep the K1 header injection (X-Tenant-Slugs etc.)"
    );
}
