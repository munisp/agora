//! SPEC-W63 geo-service gazetteer HTTP surface tests (no database required;
//! the w52_http.rs idiom).
//!
//! The crate is binary-only, so the modules are compiled into this test
//! crate via `#[path]` and `AppState`/`http_client` are mirrored verbatim
//! from src/main.rs (extended with the pinned W63 fields `av` /
//! `av_required` — Scope B's main.rs adds them; the mirror cannot silently
//! go stale because any drift breaks compilation). The pool is
//! `connect_lazy`: handlers that need the DB answer 500, which the
//! validation/AV tests use to prove the request PASSED all pre-DB gates.
//!
//! The AV tests stand up a fake clamd on loopback (the W59 conftest.py
//! socket-fixture idiom) speaking real zINSTREAM framing, and drive the
//! pinned `AvClient` API through it.

#[path = "../src/auth.rs"]
mod auth;
#[path = "../src/av.rs"]
mod av;
#[path = "../src/config.rs"]
mod config;
#[path = "../src/enrich.rs"]
mod enrich;
#[allow(dead_code)] // mirror idiom: this crate exercises the HTTP surface only
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
use tower::ServiceExt;

/// Verbatim mirror of src/main.rs::http_client.
pub fn http_client() -> reqwest::Client {
    reqwest::Client::builder()
        .connect_timeout(std::time::Duration::from_secs(5))
        .timeout(std::time::Duration::from_secs(30))
        .build()
        .expect("reqwest client with static timeout configuration must build")
}

/// Verbatim mirror of src/main.rs::AppState (the router's state type),
/// including the pinned W63 fields (`av`, `av_required`).
#[derive(Clone)]
pub struct AppState {
    pub pool: sqlx::PgPool,
    pub config: Arc<config::Config>,
    pub permify: auth::PermifyClient,
    pub http: reqwest::Client,
    pub av: Option<av::AvClient>,
    pub av_required: bool,
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

const TENANT: &str = "b3b3f6f0-6f6b-4f6b-8f6b-6f6b6f6b6f6b";
const CSV: &[u8] = b"kind,code,name,parent_code,lat,lng,source\nstate,NG-TEST,Test State,,,,\nlga,,Test Lga,NG-TEST,6.5,3.3,unit-test\n";

fn csv_req(body: &[u8]) -> Request<Body> {
    Request::post("/v1/geo/gazetteer/import")
        .header("content-type", "text/csv")
        .header("x-tenant-slug", TENANT)
        .header("x-user-id", "user-1")
        .body(Body::from(body.to_vec()))
        .unwrap()
}

// ---------------------------------------------------------------------------
// Search / snapshot validation (pre-DB paths)
// ---------------------------------------------------------------------------

#[tokio::test]
async fn gazetteer_search_validates_kind() {
    let app = routes::router(test_state(true, None));
    let (status, body, _) = call(
        app,
        Request::get("/v1/geo/gazetteer?kind=ward")
            .body(Body::empty())
            .unwrap(),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
    assert!(String::from_utf8_lossy(&body).contains("state"), "kind error lists valid kinds");
}

#[tokio::test]
async fn gazetteer_search_reaches_store_layer() {
    // Valid params pass validation; the lazy pool then 500s — proving the
    // route is wired and the query was attempted.
    let app = routes::router(test_state(true, None));
    let (status, _, _) = call(
        app,
        Request::get("/v1/geo/gazetteer?kind=lga&state=Lagos&q=ikeja")
            .body(Body::empty())
            .unwrap(),
    )
    .await;
    assert_eq!(status, StatusCode::INTERNAL_SERVER_ERROR);
}

#[tokio::test]
async fn gazetteer_snapshot_validates_levels() {
    let app = routes::router(test_state(true, None));
    let (status, body, _) = call(
        app,
        Request::get("/v1/geo/gazetteer/snapshot?levels=state,ward")
            .body(Body::empty())
            .unwrap(),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
    assert!(String::from_utf8_lossy(&body).contains("levels"));
}

#[tokio::test]
async fn gazetteer_snapshot_reaches_store_layer() {
    let app = routes::router(test_state(true, None));
    let (status, _, _) = call(
        app,
        Request::get("/v1/geo/gazetteer/snapshot?levels=state,lga")
            .body(Body::empty())
            .unwrap(),
    )
    .await;
    assert_eq!(status, StatusCode::INTERNAL_SERVER_ERROR);
}

// ---------------------------------------------------------------------------
// Import validation + authz (pre-DB paths)
// ---------------------------------------------------------------------------

#[tokio::test]
async fn gazetteer_import_requires_tenant_header() {
    let app = routes::router(test_state(true, None));
    let req = Request::post("/v1/geo/gazetteer/import")
        .header("content-type", "text/csv")
        .body(Body::from(CSV))
        .unwrap();
    let (status, body, _) = call(app, req).await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
    assert!(String::from_utf8_lossy(&body).contains("X-Tenant-Slug"));
}

#[tokio::test]
async fn gazetteer_import_rejects_unsupported_content_type() {
    let app = routes::router(test_state(true, None));
    let req = Request::post("/v1/geo/gazetteer/import")
        .header("content-type", "application/octet-stream")
        .header("x-tenant-slug", TENANT)
        .body(Body::from(CSV))
        .unwrap();
    let (status, _, _) = call(app, req).await;
    assert_eq!(status, StatusCode::UNSUPPORTED_MEDIA_TYPE);
}

#[tokio::test]
async fn gazetteer_import_all_rows_invalid_is_422() {
    // Every row invalid (bad kind) => 422 with no importable places; the
    // DB is never touched (lazy pool would 500).
    let app = routes::router(test_state(true, None));
    let bad = b"kind,name\nward,Nope\n";
    let (status, body, _) = call(app, csv_req(bad)).await;
    assert_eq!(
        status,
        StatusCode::UNPROCESSABLE_ENTITY,
        "{}",
        String::from_utf8_lossy(&body)
    );
}

#[tokio::test]
async fn gazetteer_import_reaches_store_layer_when_clean() {
    // authz disabled (dev bypass), no AV client: a valid CSV passes all
    // pre-DB gates and 500s at the lazy pool.
    let app = routes::router(test_state(true, None));
    let (status, body, _) = call(app, csv_req(CSV)).await;
    assert_eq!(
        status,
        StatusCode::INTERNAL_SERVER_ERROR,
        "valid import must reach the store layer: {}",
        String::from_utf8_lossy(&body)
    );
}

/// Live Permify gate on the gazetteer import route (w52_http.rs idiom):
/// denial -> 403, outage -> 502.
#[tokio::test]
async fn gazetteer_import_permify_gate_live() {
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

    let deny = spawn_stub("RESULT_DENIED", false).await;
    let app = routes::router(test_state(false, Some(deny)));
    let (status, _, _) = call(app, csv_req(CSV)).await;
    assert_eq!(status, StatusCode::FORBIDDEN);

    let down = spawn_stub("RESULT_ALLOWED", true).await;
    let app = routes::router(test_state(false, Some(down)));
    let (status, _, _) = call(app, csv_req(CSV)).await;
    assert_eq!(status, StatusCode::BAD_GATEWAY);
}

// ---------------------------------------------------------------------------
// AV mapping (pinned W63 handler-side contract) via a fake clamd
// ---------------------------------------------------------------------------

/// Fake clamd (W59 conftest socket-fixture idiom): speaks zINSTREAM framing
/// (command line, 4-byte BE length chunks, zero terminator) and replies with
/// the canned verdict line. Returns the bound port.
async fn spawn_fake_clamd(reply: &'static str) -> u16 {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    tokio::spawn(async move {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        loop {
            let Ok((mut conn, _)) = listener.accept().await else {
                return;
            };
            tokio::spawn(async move {
                // Command line up to \0.
                let mut byte = [0u8; 1];
                loop {
                    if conn.read_exact(&mut byte).await.is_err() {
                        return;
                    }
                    if byte[0] == 0 {
                        break;
                    }
                }
                // Chunks until zero length.
                loop {
                    let mut len = [0u8; 4];
                    if conn.read_exact(&mut len).await.is_err() {
                        return;
                    }
                    let n = u32::from_be_bytes(len) as usize;
                    if n == 0 {
                        break;
                    }
                    let mut buf = vec![0u8; n];
                    if conn.read_exact(&mut buf).await.is_err() {
                        return;
                    }
                }
                let _ = conn.write_all(reply.as_bytes()).await;
                let _ = conn.shutdown().await;
            });
        }
    });
    port
}

fn state_with_av(port: u16, required: bool) -> AppState {
    let mut s = test_state(true, None);
    s.av = Some(av::AvClient::new(
        "127.0.0.1".to_string(),
        port,
        std::time::Duration::from_secs(5),
    ));
    s.av_required = required;
    s
}

#[tokio::test]
async fn gazetteer_import_av_infected_is_415() {
    let port = spawn_fake_clamd("stream: Eicar-Test-Signature FOUND\0").await;
    let app = routes::router(state_with_av(port, true));
    let (status, body, _) = call(app, csv_req(CSV)).await;
    assert_eq!(status, StatusCode::UNSUPPORTED_MEDIA_TYPE);
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(v["error"], serde_json::json!("infected"));
    assert_eq!(v["signature"], serde_json::json!("Eicar-Test-Signature"));
}

#[tokio::test]
async fn gazetteer_import_av_error_required_is_503() {
    // clamd ERROR replies are never clean; required=true => 503 av_unavailable.
    let port = spawn_fake_clamd("INSTREAM size limit exceeded. ERROR\0").await;
    let app = routes::router(state_with_av(port, true));
    let (status, body, _) = call(app, csv_req(CSV)).await;
    assert_eq!(status, StatusCode::SERVICE_UNAVAILABLE);
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(v["error"], serde_json::json!("av_unavailable"));
}

#[tokio::test]
async fn gazetteer_import_av_error_not_required_proceeds() {
    // required=false => log + proceed: the import continues past the scan
    // and reaches the (lazy) store layer -> 500, proving proceed semantics.
    let port = spawn_fake_clamd("INSTREAM size limit exceeded. ERROR\0").await;
    let app = routes::router(state_with_av(port, false));
    let (status, body, _) = call(app, csv_req(CSV)).await;
    assert_eq!(
        status,
        StatusCode::INTERNAL_SERVER_ERROR,
        "scan error with CLAMAV_REQUIRED=false must proceed: {}",
        String::from_utf8_lossy(&body)
    );
}

#[tokio::test]
async fn gazetteer_import_av_clean_proceeds() {
    let port = spawn_fake_clamd("stream: OK\0").await;
    let app = routes::router(state_with_av(port, true));
    let (status, body, _) = call(app, csv_req(CSV)).await;
    assert_eq!(
        status,
        StatusCode::INTERNAL_SERVER_ERROR,
        "clean scan must proceed to the store layer: {}",
        String::from_utf8_lossy(&body)
    );
}
