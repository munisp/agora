//! SPEC-W63 gazetteer DB tests: migration + RLS, boot-seed idempotency,
//! search filters, snapshot ETag/304, and import upsert idempotency against
//! a REAL Postgres (w52_geo_pg.rs idiom).
//!
//! Gating: the tests SKIP CLEANLY when `GEO_TEST_DATABASE_URL` is unset or
//! unreachable. Unlike w52_geo_pg.rs, no PostGIS is required — geo_places
//! carries no geometry (0002_gazetteer.sql applies on plain Postgres).
//!
//! `GEO_TEST_DATABASE_URL` is the ADMIN (migration-owner) DSN. The runtime
//! pool the API runs against is the same database with the userinfo swapped
//! to `app_geo_login:app_geo_dev_password` (the least-privilege role the
//! migration creates) so FORCE ROW LEVEL SECURITY actually binds — the
//! bootstrap superuser would bypass RLS and make the isolation assertions
//! meaningless.

#[path = "../src/auth.rs"]
mod auth;
#[path = "../src/av.rs"]
mod av;
#[path = "../src/config.rs"]
mod config;
#[path = "../src/enrich.rs"]
mod enrich;
#[path = "../src/gazetteer.rs"]
mod gazetteer;
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

/// Verbatim mirror of src/main.rs::AppState (incl. the pinned W63 fields).
#[derive(Clone)]
pub struct AppState {
    pub pool: sqlx::PgPool,
    pub config: Arc<config::Config>,
    pub permify: auth::PermifyClient,
    pub http: reqwest::Client,
    pub av: Option<av::AvClient>,
    pub av_required: bool,
}

/// Admin pool + runtime (app_geo_login) pool, or None when the test must
/// skip (no DSN configured / unreachable).
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
    let opts: sqlx::postgres::PgConnectOptions = match admin_dsn.parse() {
        Ok(o) => o,
        Err(e) => {
            eprintln!("SKIP: GEO_TEST_DATABASE_URL unparsable: {e}");
            return None;
        }
    };
    // The runtime role is created by the migration, but the pool below
    // connects EAGERLY (sqlx validates with a first connection), so ensure
    // the roles exist first — the same pg_roles-guarded statements the
    // migration runs (INCLUDING the IN ROLE app_geo membership that carries
    // the table grants); idempotent.
    sqlx::query(
        "DO $$ BEGIN \
             IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_geo') THEN \
                 CREATE ROLE app_geo NOLOGIN NOINHERIT; \
             END IF; \
             IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_geo_login') THEN \
                 CREATE ROLE app_geo_login LOGIN PASSWORD 'app_geo_dev_password' IN ROLE app_geo; \
             END IF; \
         END $$;",
    )
    .execute(&admin)
    .await
    .expect("ensure app_geo roles");
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
            // (the live gate is covered in w63_gazetteer_http.rs).
            authz_disabled: true,
        }),
        permify: auth::PermifyClient::new(None),
        http: http_client(),
        av: None,
        av_required: false,
    }
}

async fn call(app: axum::Router, req: Request<Body>) -> (StatusCode, Bytes, axum::http::HeaderMap) {
    let resp = app.oneshot(req).await.expect("response");
    let status = resp.status();
    let headers = resp.headers().clone();
    let body = axum::body::to_bytes(resp.into_body(), 4 << 20)
        .await
        .expect("body");
    (status, body, headers)
}

async fn count_where(pool: &PgPool, cond: &str) -> i64 {
    let (n,): (i64,) = sqlx::query_as(&format!("SELECT count(*) FROM geo_places WHERE {cond}"))
        .fetch_one(pool)
        .await
        .expect("count");
    n
}

#[tokio::test]
async fn gazetteer_pg_migration_seed_rls_endpoints() {
    let Some((admin, runtime)) = gated_pools().await else {
        return;
    };
    // Clean slate (migration owner), then apply the startup migration —
    // exactly what the service does at boot (0001 not required: geo_places
    // has no geometry / no PostGIS dependency).
    sqlx::query("DROP TABLE IF EXISTS geo_places")
        .execute(&admin)
        .await
        .expect("drop");
    gazetteer::apply_gazetteer_migration(&admin)
        .await
        .expect("gazetteer migration applies");
    // Idempotent re-apply (restart no-op).
    gazetteer::apply_gazetteer_migration(&admin)
        .await
        .expect("gazetteer migration re-applies idempotently");

    // RLS really is ENABLEd+FORCEd.
    let (rls, force): (bool, bool) = sqlx::query_as(
        "SELECT relrowsecurity, relforcerowsecurity FROM pg_class \
         WHERE relname = 'geo_places' AND relnamespace = 'public'::regnamespace",
    )
    .fetch_one(&admin)
    .await
    .expect("pg_class");
    assert!(rls && force, "geo_places must have ENABLE+FORCE RLS");

    // kind CHECK constraint (SPEC-W63: kind IN state|lga|locality).
    let bad = sqlx::query(
        "INSERT INTO geo_places (kind, name, code, source) VALUES ('ward', 'X', 'X', 't')",
    )
    .execute(&admin)
    .await;
    assert!(bad.is_err(), "kind CHECK must reject 'ward'");
    // locality kind IS accepted (G3: schema + import support, seeded empty).
    sqlx::query(
        "INSERT INTO geo_places (kind, name, code, parent_code, source) \
         VALUES ('locality', 'Ckpt Locality', 'NG-LAGOS-IKEJA-CKPT-LOCALITY', 'NG-LAGOS-IKEJA', 't')",
    )
    .execute(&admin)
    .await
    .expect("locality kind allowed");

    // Boot seed: 811 reference rows; second run is a no-op (idempotent).
    gazetteer::seed_boot(&runtime).await.expect("seed_boot #1");
    assert_eq!(count_where(&runtime, "kind = 'state'").await, 37);
    assert_eq!(count_where(&runtime, "kind = 'lga'").await, 774);
    assert_eq!(count_where(&runtime, "TRUE").await, 812); // 811 seed + 1 locality
    gazetteer::seed_boot(&runtime).await.expect("seed_boot #2");
    assert_eq!(
        count_where(&runtime, "TRUE").await,
        812,
        "seed must be idempotent (ON CONFLICT DO NOTHING)"
    );

    // RLS reference-row visibility: the runtime role sees reference rows
    // WITHOUT the tenant GUC; a tenant-owned row is invisible without it and
    // visible with it.
    let tenant: uuid::Uuid = "aaaaaaaa-0000-0000-0000-00000000000a".parse().unwrap();
    sqlx::query(
        "INSERT INTO geo_places (tenant_id, kind, name, code, source) \
         VALUES ($1, 'locality', 'Tenant Secret', 'TENANT-SECRET', 't')",
    )
    .bind(tenant)
    .execute(&admin)
    .await
    .expect("tenant row");
    assert_eq!(
        count_where(&runtime, "code = 'TENANT-SECRET'").await,
        0,
        "tenant row must be invisible without the GUC"
    );
    {
        let mut tx = store::begin_tenant_tx(&runtime, tenant).await.expect("tx");
        let (n,): (i64,) = sqlx::query_as(
            "SELECT count(*) FROM geo_places WHERE code = 'TENANT-SECRET'",
        )
        .fetch_one(&mut *tx)
        .await
        .expect("count");
        assert_eq!(n, 1, "tenant row visible under its tenant GUC");
        // ...and reference rows stay world-visible inside the tenant tx.
        let (n,): (i64,) =
            sqlx::query_as("SELECT count(*) FROM geo_places WHERE tenant_id IS NULL")
                .fetch_one(&mut *tx)
                .await
                .expect("count");
        assert_eq!(n, 812);
        tx.commit().await.expect("commit");
    }

    // HTTP surface against the seeded runtime pool.
    let app = routes::router(state_with(runtime.clone()));

    // Search: kind filter.
    let (status, body, _) = call(
        app.clone(),
        Request::get("/v1/geo/gazetteer?kind=state&limit=100")
            .body(Body::empty())
            .unwrap(),
    )
    .await;
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(status, StatusCode::OK, "{v}");
    assert_eq!(v["count"], serde_json::json!(37), "{v}");

    // Search: state filter accepts a name and scopes LGAs (Lagos = 20 LGAs;
    // the state row itself has kind=state and is excluded by kind=lga).
    let (status, body, _) = call(
        app.clone(),
        Request::get("/v1/geo/gazetteer?kind=lga&state=Lagos&limit=100")
            .body(Body::empty())
            .unwrap(),
    )
    .await;
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(status, StatusCode::OK, "{v}");
    assert_eq!(v["count"], serde_json::json!(20), "{v}");
    // ...and by code.
    let (_, body, _) = call(
        app.clone(),
        Request::get("/v1/geo/gazetteer?kind=lga&state=NG-LAGOS&limit=100")
            .body(Body::empty())
            .unwrap(),
    )
    .await;
    let v2: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(v2["count"], v["count"]);

    // Search: q is a case-insensitive substring on name/code.
    let (status, body, _) = call(
        app.clone(),
        Request::get("/v1/geo/gazetteer?q=ikeja")
            .body(Body::empty())
            .unwrap(),
    )
    .await;
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(status, StatusCode::OK, "{v}");
    let codes: Vec<&str> = v["places"]
        .as_array()
        .unwrap()
        .iter()
        .map(|p| p["code"].as_str().unwrap())
        .collect();
    assert!(codes.contains(&"NG-LAGOS-IKEJA"), "{codes:?}");
    assert!(codes.contains(&"NG-LAGOS-IKEJA-CKPT-LOCALITY"), "{codes:?}");

    // Snapshot: full dump, ETag = sha256 of the exact body bytes.
    let (status, body, headers) = call(
        app.clone(),
        Request::get("/v1/geo/gazetteer/snapshot")
            .body(Body::empty())
            .unwrap(),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let etag = headers
        .get(axum::http::header::ETAG)
        .expect("ETag header")
        .to_str()
        .unwrap()
        .to_string();
    assert_eq!(
        etag,
        format!("\"{}\"", gazetteer::sha256_hex(&body)),
        "ETag must be the sha256 of the canonical body"
    );
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(v["levels"], serde_json::json!(["state", "lga", "locality"]));
    assert_eq!(v["places"].as_array().unwrap().len(), 812); // 811 seed + 1 locality

    // levels= is order-insensitive (canonical order => stable ETag).
    let (status, body2, headers2) = call(
        app.clone(),
        Request::get("/v1/geo/gazetteer/snapshot?levels=lga,state")
            .body(Body::empty())
            .unwrap(),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let etag2 = headers2.get(axum::http::header::ETAG).unwrap().to_str().unwrap();
    let v2: serde_json::Value = serde_json::from_slice(&body2).unwrap();
    assert_eq!(v2["levels"], serde_json::json!(["state", "lga"]));
    let (status, _, headers3) = call(
        app.clone(),
        Request::get("/v1/geo/gazetteer/snapshot?levels=state,lga")
            .body(Body::empty())
            .unwrap(),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(
        headers3.get(axum::http::header::ETAG).unwrap().to_str().unwrap(),
        etag2,
        "level order must not change the ETag"
    );

    // If-None-Match => 304 with empty body and the same ETag.
    let (status, body, headers304) = call(
        app.clone(),
        Request::get("/v1/geo/gazetteer/snapshot")
            .header("if-none-match", &etag)
            .body(Body::empty())
            .unwrap(),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_MODIFIED);
    assert!(body.is_empty(), "304 body must be empty");
    assert_eq!(
        headers304.get(axum::http::header::ETAG).unwrap().to_str().unwrap(),
        etag
    );
    // A stale ETag revalidates to 200.
    let (status, _, _) = call(
        app.clone(),
        Request::get("/v1/geo/gazetteer/snapshot")
            .header("if-none-match", "\"deadbeef\"")
            .body(Body::empty())
            .unwrap(),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    // Import: CSV upsert on (kind, code) — idempotent, updates in place.
    let csv1 = b"kind,code,name,parent_code,lat,lng,source\nstate,NG-TEST,Test State,,,,\nlga,,Test Lga,NG-TEST,6.5,3.3,unit-test\n";
    let import_req = |bytes: &[u8]| {
        Request::post("/v1/geo/gazetteer/import")
            .header("content-type", "text/csv")
            .header("x-tenant-slug", "aaaaaaaa-0000-0000-0000-00000000000a")
            .header("x-user-id", "user-1")
            .body(Body::from(bytes.to_vec()))
            .unwrap()
    };
    let before = count_where(&runtime, "TRUE").await;
    let (status, body, _) = call(app.clone(), import_req(csv1)).await;
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(status, StatusCode::OK, "{v}");
    assert_eq!(v["imported"], serde_json::json!(2), "{v}");
    assert_eq!(v["skipped"], serde_json::json!(0), "{v}");
    assert_eq!(count_where(&runtime, "TRUE").await, before + 2);
    // Derived code for the codeless LGA row.
    assert_eq!(count_where(&runtime, "code = 'NG-TEST-TEST-LGA'").await, 1);

    // Re-import with a changed name: same codes, updated in place (no dup).
    let csv2 = b"kind,code,name,parent_code,lat,lng,source\nstate,NG-TEST,Test State Renamed,,,,\nlga,NG-TEST-TEST-LGA,Test Lga Renamed,NG-TEST,6.5,3.3,unit-test\n";
    let (status, body, _) = call(app.clone(), import_req(csv2)).await;
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(status, StatusCode::OK, "{v}");
    assert_eq!(v["imported"], serde_json::json!(2), "{v}");
    assert_eq!(
        count_where(&runtime, "TRUE").await,
        before + 2,
        "upsert must not duplicate rows"
    );
    let (name,): (String,) =
        sqlx::query_as("SELECT name FROM geo_places WHERE kind = 'state' AND code = 'NG-TEST'")
            .fetch_one(&runtime)
            .await
            .expect("renamed row");
    assert_eq!(name, "Test State Renamed");
    // updated_at was refreshed by the upsert.
    let (stale,): (bool,) = sqlx::query_as(
        "SELECT updated_at < now() - interval '1 hour' FROM geo_places \
         WHERE kind = 'state' AND code = 'NG-TEST'",
    )
    .fetch_one(&runtime)
    .await
    .expect("updated_at");
    assert!(!stale, "upsert must refresh updated_at");

    // Import through the JSON body shape (array of places).
    let json = serde_json::json!([{"kind": "locality", "name": "Json Locality",
        "parent_code": "NG-TEST-TEST-LGA", "lat": 6.6, "lng": 3.4}]);
    let (status, body, _) = call(
        app.clone(),
        Request::post("/v1/geo/gazetteer/import")
            .header("content-type", "application/json")
            .header("x-tenant-slug", "aaaaaaaa-0000-0000-0000-00000000000a")
            .header("x-user-id", "user-1")
            .body(Body::from(serde_json::to_vec(&json).unwrap()))
            .unwrap(),
    )
    .await;
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(status, StatusCode::OK, "{v}");
    assert_eq!(v["imported"], serde_json::json!(1), "{v}");
    assert_eq!(
        count_where(&runtime, "code = 'NG-TEST-TEST-LGA-JSON-LOCALITY'").await,
        1
    );

    let _ = admin.close().await;
}
