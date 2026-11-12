//! SPEC-W54 Scope A HTTP-surface tests. Network posture: LOOPBACK ONLY —
//! the graph-service and MinIO stubs bind 127.0.0.1:0 (geo-service permify
//! stub idiom); nothing leaves the host.
//!
//! The crate is binary-only, so the modules are compiled into this test
//! crate via `#[path]` and `AppState`/`http_client` are mirrored verbatim
//! from src/main.rs (the geo-service tests/w52_http.rs idiom — drift breaks
//! compilation, so the mirror cannot silently go stale).

#[path = "../src/auth.rs"]
mod auth;
#[path = "../src/client.rs"]
mod client;
#[path = "../src/config.rs"]
mod config;
#[path = "../src/mapping.rs"]
mod mapping;
#[path = "../src/metrics.rs"]
mod metrics;
#[path = "../src/routes.rs"]
mod routes;
#[path = "../src/serialize.rs"]
mod serialize;
#[path = "../src/storage.rs"]
mod storage;

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use axum::body::Body;
use axum::http::{Request, StatusCode};
use tower::ServiceExt;

const TENANT: &str = "11111111-2222-4333-8444-555555555555";
const TOKEN: &str = "test-rdf-export-internal-token";
const GRAPH_TOKEN: &str = "test-graph-internal-token";
const NODES_NDJSON: &str = include_str!("fixtures/graph_nodes.ndjson");
const EDGES_NDJSON: &str = include_str!("fixtures/graph_edges.ndjson");

// ---- mirror of src/main.rs ------------------------------------------------

pub fn http_client() -> reqwest::Client {
    reqwest::Client::builder()
        .connect_timeout(std::time::Duration::from_secs(5))
        .build()
        .expect("reqwest client with static configuration must build")
}

#[derive(Clone)]
pub struct AppState {
    pub config: Arc<config::Config>,
    pub http: reqwest::Client,
    pub metrics: Arc<metrics::Metrics>,
}

// ---- helpers --------------------------------------------------------------

fn test_config(graph_url: &str, minio_url: &str, token: Option<&str>) -> config::Config {
    config::Config {
        bind: "127.0.0.1:0".parse().unwrap(),
        graph_service_url: graph_url.into(),
        graph_internal_token: GRAPH_TOKEN.into(),
        internal_token: token.map(str::to_string),
        minio_endpoint: minio_url.into(),
        minio_bucket: "lake".into(),
        minio_region: "us-east-1".into(),
        minio_access_key: "minioadmin".into(),
        minio_secret_key: "minioadmin".into(),
        export_timeout_s: 30,
    }
}

fn app(cfg: config::Config) -> axum::Router {
    routes::router(AppState {
        config: Arc::new(cfg),
        http: http_client(),
        metrics: Arc::new(metrics::Metrics::default()),
    })
}

fn run_request(token: Option<&str>) -> Request<Body> {
    let mut b = Request::builder()
        .method("POST")
        .uri("/v1/rdf-export/run")
        .header("content-type", "application/json");
    if let Some(t) = token {
        b = b.header("x-internal-token", t);
    }
    b.body(Body::from(format!(
        r#"{{"tenant_id":"{TENANT}","snapshot_date":"2026-01-15"}}"#
    )))
    .unwrap()
}

/// Loopback graph-service stub serving the golden fixtures with the real
/// export contract headers; enforces its X-Internal-Token (fail-closed).
async fn spawn_graph_stub() -> String {
    use axum::extract::Query;
    use axum::http::HeaderMap;
    use axum::response::IntoResponse;

    async fn nodes(
        headers: HeaderMap,
        Query(q): Query<HashMap<String, String>>,
    ) -> axum::response::Response {
        if headers
            .get("x-internal-token")
            .and_then(|v| v.to_str().ok())
            != Some(GRAPH_TOKEN)
        {
            return (StatusCode::UNAUTHORIZED, "bad token").into_response();
        }
        assert_eq!(q.get("tenant_id").map(String::as_str), Some(TENANT));
        axum::response::Response::builder()
            .status(StatusCode::OK)
            .header("content-type", "application/x-ndjson")
            .header("x-row-count", "12")
            .header("x-snapshot-date", "2026-01-15")
            .body(Body::from(NODES_NDJSON))
            .unwrap()
    }
    async fn edges(
        headers: HeaderMap,
        Query(q): Query<HashMap<String, String>>,
    ) -> axum::response::Response {
        if headers
            .get("x-internal-token")
            .and_then(|v| v.to_str().ok())
            != Some(GRAPH_TOKEN)
        {
            return (StatusCode::UNAUTHORIZED, "bad token").into_response();
        }
        assert_eq!(q.get("tenant_id").map(String::as_str), Some(TENANT));
        axum::response::Response::builder()
            .status(StatusCode::OK)
            .header("content-type", "application/x-ndjson")
            .header("x-row-count", "14")
            .header("x-snapshot-date", "2026-01-15")
            .body(Body::from(EDGES_NDJSON))
            .unwrap()
    }
    let app = axum::Router::new()
        .route("/v1/graph/internal/export/nodes", axum::routing::get(nodes))
        .route("/v1/graph/internal/export/edges", axum::routing::get(edges));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    format!("http://{addr}")
}

/// Loopback MinIO stub: captures PUT bodies per key, asserts the SigV4
/// Authorization header shape (full signature verification is MinIO's job in
/// the live stack; here we pin that the client SIGNS correctly-formed
/// requests).
type Captured = Arc<Mutex<HashMap<String, (String, Vec<u8>)>>>;

async fn spawn_minio_stub() -> (String, Captured) {
    use axum::extract::{Path, State};

    let captured: Captured = Arc::new(Mutex::new(HashMap::new()));
    async fn put(
        State(c): State<Captured>,
        Path((bucket, key)): Path<(String, String)>,
        headers: axum::http::HeaderMap,
        body: axum::body::Bytes,
    ) -> StatusCode {
        assert_eq!(bucket, "lake");
        let auth = headers
            .get("authorization")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("");
        assert!(
            auth.starts_with("AWS4-HMAC-SHA256 Credential=minioadmin/"),
            "SigV4 authorization header present and well-formed: {auth}"
        );
        assert!(headers.get("x-amz-content-sha256").is_some());
        let ctype = headers
            .get("content-type")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            .to_string();
        c.lock().unwrap().insert(key, (ctype, body.to_vec()));
        StatusCode::OK
    }
    let app = axum::Router::new()
        .route("/:bucket/*key", axum::routing::put(put))
        .with_state(captured.clone());
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (format!("http://{addr}"), captured)
}

// ---- auth fail-closed matrix ----------------------------------------------

#[tokio::test]
async fn token_unset_fails_closed_503() {
    let app = app(test_config(
        "http://127.0.0.1:1",
        "http://127.0.0.1:1",
        None,
    ));
    let resp = app.oneshot(run_request(Some(TOKEN))).await.unwrap();
    assert_eq!(resp.status(), StatusCode::SERVICE_UNAVAILABLE);
}

#[tokio::test]
async fn token_missing_or_wrong_is_401() {
    let app = app(test_config(
        "http://127.0.0.1:1",
        "http://127.0.0.1:1",
        Some(TOKEN),
    ));
    let resp = app.clone().oneshot(run_request(None)).await.unwrap();
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    let resp = app.oneshot(run_request(Some("wrong"))).await.unwrap();
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn healthz_and_metrics_unauthenticated() {
    let app = app(test_config(
        "http://127.0.0.1:1",
        "http://127.0.0.1:1",
        None,
    ));
    let resp = app
        .clone()
        .oneshot(
            Request::builder()
                .uri("/healthz")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let resp = app
        .oneshot(
            Request::builder()
                .uri("/metrics")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let body = axum::body::to_bytes(resp.into_body(), 1 << 20)
        .await
        .unwrap();
    let text = String::from_utf8(body.to_vec()).unwrap();
    assert!(text.contains("rdf_export_runs_total"));
}

#[tokio::test]
async fn invalid_inputs_are_422_after_auth() {
    let app = app(test_config(
        "http://127.0.0.1:1",
        "http://127.0.0.1:1",
        Some(TOKEN),
    ));
    for body in [
        r#"{"tenant_id":"bad/tenant"}"#,
        r#"{"tenant_id":""}"#,
        r#"{"tenant_id":"ok-tenant","snapshot_date":"15-01-2026"}"#,
    ] {
        let req = Request::builder()
            .method("POST")
            .uri("/v1/rdf-export/run")
            .header("content-type", "application/json")
            .header("x-internal-token", TOKEN)
            .body(Body::from(body))
            .unwrap();
        let resp = app.clone().oneshot(req).await.unwrap();
        assert_eq!(resp.status(), StatusCode::UNPROCESSABLE_ENTITY, "{body}");
    }
}

#[tokio::test]
async fn upstream_unreachable_is_502_not_silent() {
    let app = app(test_config(
        "http://127.0.0.1:1",
        "http://127.0.0.1:1",
        Some(TOKEN),
    ));
    let resp = app.oneshot(run_request(Some(TOKEN))).await.unwrap();
    assert_eq!(resp.status(), StatusCode::BAD_GATEWAY);
}

// ---- full golden run against loopback stubs --------------------------------

#[tokio::test]
async fn golden_run_end_to_end() {
    let graph_url = spawn_graph_stub().await;
    let (minio_url, captured) = spawn_minio_stub().await;
    let app = app(test_config(&graph_url, &minio_url, Some(TOKEN)));

    let resp = app.oneshot(run_request(Some(TOKEN))).await.unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let body = axum::body::to_bytes(resp.into_body(), 1 << 20)
        .await
        .unwrap();
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(v["tenant_id"], TENANT);
    assert_eq!(v["snapshot_date"], "2026-01-15");
    assert_eq!(v["nodes"], 12);
    assert_eq!(v["edges"], 14);
    assert_eq!(v["skipped_unknown"], 3);
    assert_eq!(v["triples"], 69);
    let outputs = v["outputs"].as_array().unwrap();
    assert_eq!(outputs.len(), 2);
    let prefix = format!("s3://lake/extracts/graph_rdf/{TENANT}/2026-01-15");
    assert_eq!(outputs[0], format!("{prefix}/graph.ttl"));
    assert_eq!(outputs[1], format!("{prefix}/graph.jsonld"));

    // Captured MinIO objects round-trip and are PII-free.
    let captured = captured.lock().unwrap();
    let ttl_key = format!("extracts/graph_rdf/{TENANT}/2026-01-15/graph.ttl");
    let jsonld_key = format!("extracts/graph_rdf/{TENANT}/2026-01-15/graph.jsonld");
    let (ctype, turtle) = captured.get(&ttl_key).expect("graph.ttl PUT");
    assert_eq!(ctype, "text/turtle");
    let (ctype, jsonld) = captured.get(&jsonld_key).expect("graph.jsonld PUT");
    assert_eq!(ctype, "application/ld+json");
    let turtle = String::from_utf8(turtle.clone()).unwrap();
    let jsonld = String::from_utf8(jsonld.clone()).unwrap();

    let parsed = oxirs_ttl::turtle::TurtleParser::new()
        .parse_document(&turtle)
        .unwrap();
    assert_eq!(parsed.len(), 69);
    for marker in [
        "Adaeze Testperson",
        "+2348012345678",
        "UNKNOWN-LABEL-NAME-MARKER",
        "+2348099999999",
        "widget-0001",
    ] {
        assert!(!turtle.contains(marker), "turtle leaks {marker}");
        assert!(!jsonld.contains(marker), "jsonld leaks {marker}");
    }
}
