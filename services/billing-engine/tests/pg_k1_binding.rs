//! SPEC-W44 F1 integration test: the HUMAN K1 path for uuid-keyed billing
//! tenants against REAL Postgres + a mock identity-service.
//!
//! Before this fix the human path only "worked" through the APISIX
//! x-internal-token injection (the deployment bypass): bind_tenant compared
//! the uuid tenant param string-exactly against the slug claim, which can
//! never match. Now bind_tenant resolves each claimed slug via identity
//! `GET /v1/tenants/{slug}` (X-Internal-Token service caller) and binds when
//! any resolved tenant id equals the uuid param.
//!
//! Covered here, end to end over HTTP with a live PG pool:
//! - owner with a slug claim (identity resolves slug -> tenant uuid):
//!   PUT rate-card 200, POST generate 201 (K1 binding + K6 role gate pass);
//! - member with the same valid binding: reads 200, mutations 403 (K6);
//! - caller whose claim resolves to a DIFFERENT tenant: 403;
//! - identity outage mid-path: 503 (fail-closed, never a silent allow);
//! - K2 service path unchanged: x-internal-token works with no slugs claim.
//!
//! Requires env `BILLING_TEST_DATABASE_URL` (pgserver-backed driver sets
//! it); when unset the test skips itself so plain `cargo test` stays
//! hermetic.

use std::sync::atomic::AtomicU64;
use std::sync::Arc;

use sqlx::PgPool;
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

const INTERNAL_TOKEN: &str = "pg-k1-binding-internal-token";
const IDENTITY_TOKEN: &str = "pg-k1-binding-identity-token";
/// The slug the mock identity maps to the test tenant.
const TENANT_SLUG: &str = "acme";

fn test_config(database_url: &str, identity_base_url: &str) -> config::Config {
    config::Config {
        port: 0,
        database_url: database_url.to_string(),
        internal_database_url: None,
        kafka_brokers: "127.0.0.1:1".to_string(),
        kafka_group_id: "pg-k1-binding".to_string(),
        usage_events_topic: "opendesk.usage".to_string(),
        kafka_consumer_enabled: false,
        billing_events_topic: "opendesk.billing".to_string(),
        dlq_topic: "opendesk.dlq".to_string(),
        billing_ledger_impl: "postgres".to_string(),
        internal_token: INTERNAL_TOKEN.to_string(),
        paystack_secret_key: None,
        paystack_default_email: "pg-k1@example.com".to_string(),
        paystack_callback_url: "http://127.0.0.1/callback".to_string(),
        billing_static_account: "PG/0000000000".to_string(),
        billing_merchant_name: "PG K1 Binding".to_string(),
        money_roles: vec!["owner".to_string(), "admin".to_string()],
        trust_direct_tenant: false,
        identity_base_url: identity_base_url.to_string(),
        identity_internal_token: Some(IDENTITY_TOKEN.to_string()),
        tenant_cache_ttl_s: 60,
        dunning_interval_s: 3600,
        invoice_due_days: 14,
    }
}

/// Mock identity-service: GET /v1/tenants/{slug} -> 200 {"id": ...} for the
/// mapped slug, 404 otherwise; `fail` mode answers 500 to everything and the
/// token guard mode answers 401 without the X-Internal-Token header (pins
/// the K2 caller contract against identity).
async fn spawn_mock_identity(
    tenant: Uuid,
    fail: bool,
) -> (String, Arc<AtomicU64>) {
    use axum::extract::{Path, State};
    use axum::http::HeaderMap;
    use axum::routing::get;
    use axum::Json;

    #[derive(Clone)]
    struct Mock {
        tenant: Uuid,
        fail: bool,
        hits: Arc<AtomicU64>,
    }
    async fn get_tenant(
        State(m): State<Mock>,
        headers: HeaderMap,
        Path(slug): Path<String>,
    ) -> (reqwest::StatusCode, Json<serde_json::Value>) {
        m.hits.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        // Pin the K2 contract: identity's getTenant accepts internal-token
        // service callers; without the token a bare service call is 401.
        let presented = headers
            .get("x-internal-token")
            .and_then(|v| v.to_str().ok())
            .unwrap_or_default();
        if presented != IDENTITY_TOKEN {
            return (
                reqwest::StatusCode::UNAUTHORIZED,
                Json(serde_json::json!({"error": "invalid internal token"})),
            );
        }
        if m.fail {
            return (
                reqwest::StatusCode::INTERNAL_SERVER_ERROR,
                Json(serde_json::json!({"error": "mock identity outage"})),
            );
        }
        if slug == TENANT_SLUG {
            (
                reqwest::StatusCode::OK,
                Json(serde_json::json!({"id": m.tenant.to_string(), "slug": slug})),
            )
        } else {
            (
                reqwest::StatusCode::NOT_FOUND,
                Json(serde_json::json!({"error": "tenant not found"})),
            )
        }
    }

    let hits = Arc::new(AtomicU64::new(0));
    let app = axum::Router::new()
        .route("/v1/tenants/:slug", get(get_tenant))
        .with_state(Mock {
            tenant,
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

struct Harness {
    base: String,
    http: reqwest::Client,
    identity_hits: Arc<AtomicU64>,
}

/// All migrations (0001..0005, the boot path), in order.
pub const MIGRATIONS: [&str; 5] = [
    include_str!("../migrations/0001_init.sql"),
    include_str!("../migrations/0002_rls.sql"),
    include_str!("../migrations/0003_ledger.sql"),
    include_str!("../migrations/0004_outbox.sql"),
    include_str!("../migrations/0005_hardening.sql"),
];

static MIGRATED: tokio::sync::OnceCell<()> = tokio::sync::OnceCell::const_new();

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

async fn harness(tenant: Uuid, identity_fail: bool) -> Option<Harness> {
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
    let (identity_base, identity_hits) = spawn_mock_identity(tenant, identity_fail).await;
    let cfg = test_config(&url, &identity_base);
    let ledger = Arc::new(
        ledger::PgLedgerClient::new(pool.clone())
            .await
            .expect("pg ledger builds"),
    );
    let state = AppState {
        pool: pool.clone(),
        internal_pool: pool,
        ledger,
        producer: None,
        http: http_client(),
        identity: Arc::new(identity::SlugResolver::new(
            http_client(),
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
    Some(Harness {
        base: format!("http://{addr}"),
        http: http_client(),
        identity_hits,
    })
}

/// Human (gateway) request builder: the K1 headers APISIX injects from the
/// verified JWT — X-Tenant-Slugs + X-User-Roles — and NOTHING else.
fn human(h: &Harness, method: &str, url: String, slugs: &str, roles: Option<&str>) -> reqwest::RequestBuilder {
    let b = match method {
        "GET" => h.http.get(url),
        "POST" => h.http.post(url),
        "PUT" => h.http.put(url),
        other => panic!("unsupported method {other}"),
    };
    let b = b.header("x-tenant-slugs", slugs);
    match roles {
        Some(r) => b.header("x-user-roles", r),
        None => b,
    }
}

/// SPEC-W44 F1 end-to-end-ish: a human caller bound via identity slug->uuid
/// resolution; member vs owner role gating per the existing K6 policy.
#[tokio::test]
async fn k1_human_uuid_binding_with_identity_resolution_against_real_pg() {
    let tenant = Uuid::new_v4();
    let Some(h) = harness(tenant, false).await else {
        eprintln!("BILLING_TEST_DATABASE_URL unset; skipping pg integration test");
        return;
    };
    let period = format!("2026-{:02}", (tenant.as_bytes()[0] % 12) + 1);

    // 0. K2 service path unchanged: internal-token caller, no slugs header.
    let resp = h
        .http
        .put(format!("{}/v1/rate-cards/{tenant}", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .json(&serde_json::json!({
            "metric": "seat",
            "unit_price_cents": 500,
            "included_quota": 0,
            "currency": "NGN",
        }))
        .send()
        .await
        .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::OK, "K2 service path");

    // 1. Owner human: slug claim resolves via identity to the tenant uuid.
    //    Mutation allowed (K6): generate -> 201.
    let resp = human(
        &h,
        "POST",
        format!("{}/v1/invoices/generate", h.base),
        TENANT_SLUG,
        Some("owner"),
    )
    .json(&serde_json::json!({ "tenant_id": tenant, "period": period }))
    .send()
    .await
    .unwrap();
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    assert_eq!(
        status,
        reqwest::StatusCode::CREATED,
        "owner generate via identity-bound slug must be 201; got {status} {body:?}"
    );
    assert!(
        h.identity_hits.load(std::sync::atomic::Ordering::Relaxed) >= 1,
        "the uuid binding must have consulted identity"
    );

    // 2. Member human: same valid binding, no money role. Reads 200 ...
    let resp = human(
        &h,
        "GET",
        format!("{}/v1/invoices?tenant_id={tenant}", h.base),
        TENANT_SLUG,
        Some("member"),
    )
    .send()
    .await
    .unwrap();
    assert_eq!(
        resp.status(),
        reqwest::StatusCode::OK,
        "member read with valid binding must be 200"
    );
    // ... mutations 403 (generate and rate-card upsert).
    let resp = human(
        &h,
        "POST",
        format!("{}/v1/invoices/generate", h.base),
        TENANT_SLUG,
        Some("member"),
    )
    .json(&serde_json::json!({ "tenant_id": tenant, "period": "2027-01" }))
    .send()
    .await
    .unwrap();
    assert_eq!(
        resp.status(),
        reqwest::StatusCode::FORBIDDEN,
        "member generate must be 403 (K6)"
    );
    let resp = human(
        &h,
        "PUT",
        format!("{}/v1/rate-cards/{tenant}", h.base),
        TENANT_SLUG,
        Some("member"),
    )
    .json(&serde_json::json!({
        "metric": "seat",
        "unit_price_cents": 700,
        "currency": "NGN",
    }))
    .send()
    .await
    .unwrap();
    assert_eq!(
        resp.status(),
        reqwest::StatusCode::FORBIDDEN,
        "member rate-card upsert must be 403 (K6)"
    );

    // 3. Money-role header absent entirely: fail-closed 403 on mutations.
    let resp = human(
        &h,
        "POST",
        format!("{}/v1/invoices/generate", h.base),
        TENANT_SLUG,
        None,
    )
    .json(&serde_json::json!({ "tenant_id": tenant, "period": "2027-02" }))
    .send()
    .await
    .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::FORBIDDEN);

    // 4. Caller whose claim cannot bind: a uuid slug nobody resolves and a
    //    foreign tenant param both 403 (never a cross-tenant leak).
    let resp = human(
        &h,
        "GET",
        format!("{}/v1/invoices?tenant_id={tenant}", h.base),
        "no-such-tenant",
        Some("owner"),
    )
    .send()
    .await
    .unwrap();
    assert_eq!(resp.status(), reqwest::StatusCode::FORBIDDEN);
    let foreign = Uuid::new_v4();
    let resp = human(
        &h,
        "GET",
        format!("{}/v1/invoices?tenant_id={foreign}", h.base),
        TENANT_SLUG,
        Some("owner"),
    )
    .send()
    .await
    .unwrap();
    assert_eq!(
        resp.status(),
        reqwest::StatusCode::FORBIDDEN,
        "claim resolving to a different uuid must not bind"
    );
}

/// SPEC-W44 F1 fail-closed: identity outage -> 503 on the human path; the
/// K2 internal-token path is unaffected (no identity dependency).
#[tokio::test]
async fn k1_human_binding_fails_closed_when_identity_is_down() {
    let tenant = Uuid::new_v4();
    let Some(h) = harness(tenant, true).await else {
        eprintln!("BILLING_TEST_DATABASE_URL unset; skipping pg integration test");
        return;
    };

    // Human path: identity 500 -> 503, never a silent allow.
    let resp = human(
        &h,
        "GET",
        format!("{}/v1/invoices?tenant_id={tenant}", h.base),
        TENANT_SLUG,
        Some("owner"),
    )
    .send()
    .await
    .unwrap();
    assert_eq!(
        resp.status(),
        reqwest::StatusCode::SERVICE_UNAVAILABLE,
        "identity outage must fail closed with 503"
    );

    // K2 service path: unaffected by the identity outage.
    let resp = h
        .http
        .get(format!("{}/v1/invoices?tenant_id={tenant}", h.base))
        .header("x-internal-token", INTERNAL_TOKEN)
        .send()
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        reqwest::StatusCode::OK,
        "internal-token service path must not depend on identity"
    );
}
