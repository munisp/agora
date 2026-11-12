//! SPEC-W52 geo-service DB tests: import -> enrich round-trip, upsert
//! idempotency and RLS isolation against a REAL Postgres+PostGIS.
//!
//! Gating (billing pg_hardening.rs idiom): the tests SKIP CLEANLY when
//! `GEO_TEST_DATABASE_URL` is unset, and also skip when the target database
//! cannot provide PostGIS (an embedded plain-pg pgserver). Mocking the
//! database is intentionally NOT done.
//!
//! `GEO_TEST_DATABASE_URL` is the ADMIN (migration-owner) DSN. The runtime
//! pool the API runs against is the same database with the userinfo swapped
//! to `app_geo_login:app_geo_dev_password` (the least-privilege role the
//! migration creates) so FORCE ROW LEVEL SECURITY actually binds — the
//! bootstrap superuser would bypass RLS and make the isolation assertions
//! meaningless. No network beyond the configured test database.

#[path = "../src/auth.rs"]
mod auth;
#[path = "../src/config.rs"]
mod config;
#[path = "../src/enrich.rs"]
mod enrich;
#[path = "../src/import.rs"]
mod import;
#[path = "../src/routes.rs"]
mod routes;
#[allow(dead_code)] // mirror idiom: not every test crate uses every helper
#[path = "../src/store.rs"]
mod store;
#[path = "../src/tiles.rs"]
mod tiles;

use std::sync::Arc;

use axum::body::{Body, Bytes};
use axum::http::{Request, StatusCode};
use sqlx::postgres::PgPoolOptions;
use sqlx::PgPool;
use tower::ServiceExt;

/// Verbatim mirror of src/main.rs::http_client.
pub fn http_client() -> reqwest::Client {
    reqwest::Client::builder()
        .connect_timeout(std::time::Duration::from_secs(5))
        .timeout(std::time::Duration::from_secs(30))
        .build()
        .expect("reqwest client with static timeout configuration must build")
}

/// Verbatim mirror of src/main.rs::AppState.
#[derive(Clone)]
pub struct AppState {
    pub pool: sqlx::PgPool,
    pub config: Arc<config::Config>,
    pub permify: auth::PermifyClient,
    pub http: reqwest::Client,
}

const FIXTURE: &[u8] = include_bytes!("fixtures/boundaries_4326.geojson");
const TENANT_A: &str = "aaaaaaaa-0000-0000-0000-00000000000a";
const TENANT_B: &str = "bbbbbbbb-0000-0000-0000-00000000000b";

/// Admin pool + runtime (app_geo_login) pool, or None when the test must
/// skip (no DSN configured / PostGIS unavailable).
async fn gated_pools() -> Option<(PgPool, PgPool)> {
    let admin_dsn = match std::env::var("GEO_TEST_DATABASE_URL") {
        Ok(d) if !d.trim().is_empty() => d,
        _ => {
            eprintln!("SKIP: GEO_TEST_DATABASE_URL unset (no test database configured)");
            return None;
        }
    };
    let admin = match PgPoolOptions::new()
        .max_connections(2)
        .connect(&admin_dsn)
        .await
    {
        Ok(p) => p,
        Err(e) => {
            eprintln!("SKIP: cannot connect to GEO_TEST_DATABASE_URL: {e}");
            return None;
        }
    };
    if let Err(e) = sqlx::query("CREATE EXTENSION IF NOT EXISTS postgis")
        .execute(&admin)
        .await
    {
        eprintln!("SKIP: PostGIS unavailable on the test database: {e}");
        return None;
    }
    // Same database, least-privilege runtime role (created by the migration).
    let opts: sqlx::postgres::PgConnectOptions = match admin_dsn.parse() {
        Ok(o) => o,
        Err(e) => {
            eprintln!("SKIP: GEO_TEST_DATABASE_URL unparsable: {e}");
            return None;
        }
    };
    let runtime_dsn = opts
        .username("app_geo_login")
        .password("app_geo_dev_password");
    let runtime = match PgPoolOptions::new()
        .max_connections(4)
        .connect_with(runtime_dsn)
        .await
    {
        Ok(p) => p,
        Err(e) => {
            eprintln!("SKIP: app_geo_login pool unusable: {e}");
            return None;
        }
    };
    Some((admin, runtime))
}

fn state_with(pool: PgPool) -> AppState {
    AppState {
        pool,
        config: Arc::new(config::Config {
            database_url: "postgres://unused".to_string(),
            bind: "127.0.0.1:0".parse().unwrap(),
            tiles_path: None,
            permify_url: None,
            // Dev bypass: these tests exercise the DB/RLS path, not Permify
            // (the live gate is covered in w52_http.rs).
            authz_disabled: true,
        }),
        permify: auth::PermifyClient::new(None),
        http: http_client(),
    }
}

async fn call(app: axum::Router, req: Request<Body>) -> (StatusCode, Bytes) {
    let resp = app.oneshot(req).await.expect("response");
    let status = resp.status();
    let body = axum::body::to_bytes(resp.into_body(), 4 << 20)
        .await
        .expect("body");
    (status, body)
}

fn import_req(fixture: &[u8], scope: &str, tenant: &str) -> Request<Body> {
    let boundary = "w52pgboundary";
    let mut buf: Vec<u8> = Vec::new();
    for (k, v) in [
        ("kind", "custom".to_string()),
        ("name_field", "name".to_string()),
        ("code_field", "code".to_string()),
        ("source", "test-only-fixture".to_string()),
        ("scope", scope.to_string()),
    ] {
        buf.extend_from_slice(
            format!("--{boundary}\r\nContent-Disposition: form-data; name=\"{k}\"\r\n\r\n{v}\r\n")
                .as_bytes(),
        );
    }
    buf.extend_from_slice(
        format!(
            "--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"boundaries_4326.geojson\"\r\nContent-Type: application/geo+json\r\n\r\n"
        )
        .as_bytes(),
    );
    buf.extend_from_slice(fixture);
    buf.extend_from_slice(format!("\r\n--{boundary}--\r\n").as_bytes());
    Request::post("/v1/geo/boundaries/import")
        .header(
            "content-type",
            format!("multipart/form-data; boundary={boundary}"),
        )
        .header("x-tenant-slug", tenant)
        .header("x-user-id", "w52-test")
        .body(Body::from(buf))
        .unwrap()
}

fn enrich_req(lat: f64, lng: f64, tenant: Option<&str>) -> Request<Body> {
    let mut b = Request::get(format!("/v1/geo/enrich?lat={lat}&lng={lng}"));
    if let Some(t) = tenant {
        b = b.header("x-tenant-slug", t);
    }
    b.body(Body::empty()).unwrap()
}

/// One sequential end-to-end test (a single test fn keeps the shared table
/// free of cross-test interference).
#[tokio::test]
async fn import_enrich_roundtrip_and_rls_isolation() {
    let Some((admin, runtime)) = gated_pools().await else {
        return;
    };
    // Clean slate (migration owner), then apply the startup migration —
    // exactly what the service does at boot.
    sqlx::query("DROP TABLE IF EXISTS geo_boundaries")
        .execute(&admin)
        .await
        .expect("drop");
    store::apply_geo_migration(&admin)
        .await
        .expect("migration applies");

    // RLS really is ENABLEd+FORCEd (pglast-clean policy check by shape).
    let (rls, force): (bool, bool) = sqlx::query_as(
        "SELECT relrowsecurity, relforcerowsecurity FROM pg_class \
                        WHERE relname = 'geo_boundaries' AND relnamespace = 'public'::regnamespace",
    )
    .fetch_one(&admin)
    .await
    .expect("pg_class");
    assert!(rls && force, "geo_boundaries must have ENABLE+FORCE RLS");

    let app = routes::router(state_with(runtime.clone()));

    // 1. Reference import: 3 polygon records imported, 2 features skipped.
    let (status, body) = call(app.clone(), import_req(FIXTURE, "reference", TENANT_A)).await;
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(status, StatusCode::OK, "{v}");
    assert_eq!(v["imported"], serde_json::json!(3), "{v}");
    assert_eq!(v["skipped"], serde_json::json!(2), "{v}");

    // 2. Enrich a point inside all three reference boundaries.
    let (status, body) = call(app.clone(), enrich_req(6.45, 3.345, None)).await;
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(status, StatusCode::OK, "{v}");
    assert_eq!(v["state"], serde_json::json!("Test State Alpha"), "{v}");
    assert_eq!(v["lga"], serde_json::json!("Test Lga Alpha"), "{v}");
    assert_eq!(v["ward"], serde_json::json!("Test Ward Alpha"), "{v}");
    assert_eq!(v["boundaries"].as_array().unwrap().len(), 3, "{v}");

    // 3. Documented no-hit behavior: 200 with nulls + empty boundaries.
    let (status, body) = call(app.clone(), enrich_req(10.0, 40.0, None)).await;
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(status, StatusCode::OK, "{v}");
    assert!(
        v["state"].is_null() && v["lga"].is_null() && v["ward"].is_null(),
        "{v}"
    );
    assert_eq!(v["boundaries"].as_array().unwrap().len(), 0, "{v}");

    // 4. Tenant-scope import for tenant A (kind=custom: state/lga/ward stay
    // reference-driven; the custom row only joins `boundaries`).
    let (status, body) = call(app.clone(), import_req(FIXTURE, "tenant", TENANT_A)).await;
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(status, StatusCode::OK, "{v}");
    assert_eq!(v["imported"], serde_json::json!(3), "{v}");

    // 5. RLS isolation: tenant B's enrich sees the reference rows but NOT
    //    tenant A's custom rows.
    let (status, body) = call(app.clone(), enrich_req(6.45, 3.345, Some(TENANT_B))).await;
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(status, StatusCode::OK, "{v}");
    assert_eq!(v["state"], serde_json::json!("Test State Alpha"), "{v}");
    assert_eq!(v["boundaries"].as_array().unwrap().len(), 3, "{v}");

    // 6. Tenant A sees its own custom rows in addition to the reference set.
    let (status, body) = call(app.clone(), enrich_req(6.45, 3.345, Some(TENANT_A))).await;
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(status, StatusCode::OK, "{v}");
    assert_eq!(v["boundaries"].as_array().unwrap().len(), 6, "{v}");

    // 7. Raw-policy proof: as app_geo_login WITHOUT any tenant GUC, tenant
    //    rows are invisible (reference-only), and with the GUC pinned to
    //    tenant B tenant A's rows stay invisible.
    let (n,): (i64,) =
        sqlx::query_as("SELECT count(*) FROM geo_boundaries WHERE tenant_id IS NOT NULL")
            .fetch_one(&runtime)
            .await
            .expect("count");
    assert_eq!(n, 0, "tenant rows must be invisible without the GUC");
    let mut tx = store::begin_tenant_tx(&runtime, TENANT_B.parse().unwrap())
        .await
        .expect("tx");
    let (n,): (i64,) =
        sqlx::query_as("SELECT count(*) FROM geo_boundaries WHERE tenant_id IS NOT NULL")
            .fetch_one(&mut *tx)
            .await
            .expect("count");
    assert_eq!(n, 0, "tenant B must not see tenant A rows");
    drop(tx);

    // 8. Upsert idempotency: re-importing the same fixture updates in place.
    let (status, body) = call(app.clone(), import_req(FIXTURE, "reference", TENANT_A)).await;
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(status, StatusCode::OK, "{v}");
    assert_eq!(v["imported"], serde_json::json!(3), "{v}");
    let (n,): (i64,) =
        sqlx::query_as("SELECT count(*) FROM geo_boundaries WHERE tenant_id IS NULL")
            .fetch_one(&runtime)
            .await
            .expect("count");
    assert_eq!(n, 3, "upsert must not duplicate reference rows");

    let _ = admin.close().await;
}
