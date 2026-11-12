//! SPEC-W52 geo-service HTTP surface tests (no database required).
//!
//! The crate is binary-only, so the modules are compiled into this test
//! crate via `#[path]` and `AppState`/`http_client` are mirrored verbatim
//! from src/main.rs (the payments route_smoke.rs idiom — any drift breaks
//! compilation of this test, so the mirror cannot silently go stale). The
//! pool is `connect_lazy` (no connection attempt): handlers that need the DB
//! answer 500, which the authz-gate tests use to prove the request PASSED
//! the gate and reached the store layer.

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
use tower::ServiceExt;

/// Verbatim mirror of src/main.rs::http_client.
pub fn http_client() -> reqwest::Client {
    reqwest::Client::builder()
        .connect_timeout(std::time::Duration::from_secs(5))
        .timeout(std::time::Duration::from_secs(30))
        .build()
        .expect("reqwest client with static timeout configuration must build")
}

/// Verbatim mirror of src/main.rs::AppState (the router's state type).
#[derive(Clone)]
pub struct AppState {
    pub pool: sqlx::PgPool,
    pub config: Arc<config::Config>,
    pub permify: auth::PermifyClient,
    pub http: reqwest::Client,
}

fn lazy_pool() -> sqlx::PgPool {
    // No connection attempt: only used so handlers that reach the DB fail
    // with 500 instead of needing a live Postgres in these surface tests.
    sqlx::postgres::PgPoolOptions::new()
        .connect_lazy("postgres://postgres:postgres@127.0.0.1:1/postgres")
        .expect("lazy pool")
}

fn test_state(authz_disabled: bool, permify_url: Option<String>) -> AppState {
    AppState {
        pool: lazy_pool(),
        config: Arc::new(config::Config {
            database_url: "postgres://unused".to_string(),
            bind: "127.0.0.1:0".parse().unwrap(),
            tiles_path: None,
            permify_url: permify_url.clone(),
            authz_disabled,
        }),
        permify: auth::PermifyClient::new(permify_url),
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

fn multipart_body(
    field: &str,
    filename: &str,
    data: &[u8],
    extra: &[(&str, &str)],
) -> (String, Vec<u8>) {
    let boundary = "w52testboundary";
    let mut buf: Vec<u8> = Vec::new();
    for (k, v) in extra {
        buf.extend_from_slice(
            format!("--{boundary}\r\nContent-Disposition: form-data; name=\"{k}\"\r\n\r\n{v}\r\n")
                .as_bytes(),
        );
    }
    buf.extend_from_slice(
        format!(
            "--{boundary}\r\nContent-Disposition: form-data; name=\"{field}\"; filename=\"{filename}\"\r\nContent-Type: application/geo+json\r\n\r\n"
        )
        .as_bytes(),
    );
    buf.extend_from_slice(data);
    buf.extend_from_slice(format!("\r\n--{boundary}--\r\n").as_bytes());
    (format!("multipart/form-data; boundary={boundary}"), buf)
}

const FIXTURE: &[u8] = include_bytes!("fixtures/boundaries_4326.geojson");
const TENANT: &str = "b3b3f6f0-6f6b-4f6b-8f6b-6f6b6f6b6f6b";

#[tokio::test]
async fn healthz_ok() {
    let app = routes::router(test_state(true, None));
    let (status, body) = call(app, Request::get("/healthz").body(Body::empty()).unwrap()).await;
    assert_eq!(status, StatusCode::OK);
    assert!(String::from_utf8_lossy(&body).contains("\"ok\""));
}

/// Tiles fail-closed semantics: GEO_TILES_PATH unset -> 503 with a plain
/// error on BOTH the canonical /v1/geo/* paths and the /api/geo-strip
/// aliases (Scope C's default style URL shape). Real code, disabled config.
#[tokio::test]
async fn tiles_and_style_fail_closed_503_when_unset() {
    for path in [
        "/v1/geo/tiles/0/0/0.mvt",
        "/v1/geo/style.json",
        "/style.json",
        "/tiles/0/0/0.mvt",
    ] {
        let app = routes::router(test_state(true, None));
        let (status, body) = call(app, Request::get(path).body(Body::empty()).unwrap()).await;
        assert_eq!(status, StatusCode::SERVICE_UNAVAILABLE, "{path}");
        assert!(
            String::from_utf8_lossy(&body).contains("GEO_TILES_PATH"),
            "{path}: {}",
            String::from_utf8_lossy(&body)
        );
    }
}

#[tokio::test]
async fn enrich_validates_params() {
    let app = routes::router(test_state(true, None));
    let (status, _) = call(
        app,
        Request::get("/v1/geo/enrich").body(Body::empty()).unwrap(),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
    let app = routes::router(test_state(true, None));
    let (status, _) = call(
        app,
        Request::get("/v1/geo/enrich?lat=95&lng=3.4")
            .body(Body::empty())
            .unwrap(),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
}

#[tokio::test]
async fn import_validates_params_and_extension() {
    // Missing kind -> 400.
    let (ct, body) = multipart_body(
        "file",
        "b.geojson",
        FIXTURE,
        &[("name_field", "name"), ("code_field", "code")],
    );
    let app = routes::router(test_state(true, None));
    let req = Request::post("/v1/geo/boundaries/import")
        .header("content-type", ct)
        .header("x-tenant-slug", TENANT)
        .body(Body::from(body))
        .unwrap();
    let (status, resp) = call(app, req).await;
    assert_eq!(
        status,
        StatusCode::BAD_REQUEST,
        "{}",
        String::from_utf8_lossy(&resp)
    );

    // scope=tenant with a non-uuid tenant header -> 400.
    let (ct, body) = multipart_body(
        "file",
        "b.geojson",
        FIXTURE,
        &[
            ("kind", "lga"),
            ("name_field", "name"),
            ("code_field", "code"),
            ("scope", "tenant"),
        ],
    );
    let app = routes::router(test_state(true, None));
    let req = Request::post("/v1/geo/boundaries/import")
        .header("content-type", ct)
        .header("x-tenant-slug", "not-a-uuid")
        .body(Body::from(body))
        .unwrap();
    let (status, _) = call(app, req).await;
    assert_eq!(status, StatusCode::BAD_REQUEST);

    // Unsupported extension -> 422 (gate bypassed in dev mode).
    let (ct, body) = multipart_body(
        "file",
        "b.csv",
        b"a,b",
        &[
            ("kind", "lga"),
            ("name_field", "name"),
            ("code_field", "code"),
        ],
    );
    let app = routes::router(test_state(true, None));
    let req = Request::post("/v1/geo/boundaries/import")
        .header("content-type", ct)
        .header("x-tenant-slug", TENANT)
        .body(Body::from(body))
        .unwrap();
    let (status, _) = call(app, req).await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);
}

/// Permify gate on the live HTTP route: denial -> 403, outage -> 502, allow
/// -> the request reaches the store layer (500 on the lazy pool, proving the
/// gate passed); AUTHZ_DISABLED=true -> same reach-through.
#[tokio::test]
async fn import_permify_gate_live() {
    use axum::routing::post;
    use axum::Json as AxumJson;

    async fn spawn_stub(can: &'static str, fail: bool) -> String {
        #[derive(Clone)]
        struct Mock {
            can: &'static str,
            fail: bool,
        }
        async fn check(
            axum::extract::State(m): axum::extract::State<Mock>,
        ) -> (StatusCode, AxumJson<serde_json::Value>) {
            if m.fail {
                return (
                    StatusCode::INTERNAL_SERVER_ERROR,
                    AxumJson(serde_json::json!({"error": "outage"})),
                );
            }
            (StatusCode::OK, AxumJson(serde_json::json!({"can": m.can})))
        }
        let app = axum::Router::new()
            .route("/v1/tenants/:tenant/permissions/check", post(check))
            .with_state(Mock { can, fail });
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        format!("http://{addr}")
    }

    let import_req = || {
        let (ct, body) = multipart_body(
            "file",
            "b.geojson",
            FIXTURE,
            &[
                ("kind", "lga"),
                ("name_field", "name"),
                ("code_field", "code"),
            ],
        );
        Request::post("/v1/geo/boundaries/import")
            .header("content-type", ct)
            .header("x-tenant-slug", TENANT)
            .header("x-user-id", "user-1")
            .body(Body::from(body))
            .unwrap()
    };

    let deny = spawn_stub("RESULT_DENIED", false).await;
    let app = routes::router(test_state(false, Some(deny)));
    let (status, _) = call(app, import_req()).await;
    assert_eq!(status, StatusCode::FORBIDDEN);

    let down = spawn_stub("RESULT_ALLOWED", true).await;
    let app = routes::router(test_state(false, Some(down)));
    let (status, _) = call(app, import_req()).await;
    assert_eq!(status, StatusCode::BAD_GATEWAY);

    // Allow: passes the gate, parses the fixture with terrio, then fails at
    // the (deliberately unreachable) lazy pool -> 500. A 4xx here would mean
    // the gate or validation rejected the request.
    let allow = spawn_stub("RESULT_ALLOWED", false).await;
    let app = routes::router(test_state(false, Some(allow)));
    let (status, resp) = call(app, import_req()).await;
    assert_eq!(
        status,
        StatusCode::INTERNAL_SERVER_ERROR,
        "allowed request must reach the store layer: {}",
        String::from_utf8_lossy(&resp)
    );
}
