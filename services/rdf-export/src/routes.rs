//! Route table (SPEC-W54 Scope A item 6). MESH-INTERNAL ONLY — there is
//! deliberately NO APISIX route for this service (kyc-service precedent for
//! internal-only services; the gateway already hard-blocks /api/graph/
//! internal* per GW-07).
//!
//!   GET  /healthz                 — liveness (no auth; never leaks config)
//!   GET  /metrics                 — Prometheus text (no auth, mesh-only)
//!   POST /v1/rdf-export/run       — one synchronous batch export:
//!                                   {tenant_id, snapshot_date?} ->
//!                                   {tenant_id, snapshot_date, nodes, edges,
//!                                    triples, skipped_unknown, outputs[]}
//!
//! Batch semantics (documented in README): the run is SYNCHRONOUS within
//! RDF_EXPORT_TIMEOUT_S (default 300s) — this is an ops/compliance batch
//! tool, not an interactive API. 504 on timeout.

use std::sync::atomic::Ordering;
use std::time::Instant;

use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::{Deserialize, Serialize};

use crate::client::GraphClient;
use crate::mapping::RdfMapper;
use crate::storage::MinioClient;
use crate::{auth, serialize, AppState};

async fn healthz() -> Json<serde_json::Value> {
    Json(serde_json::json!({"status": "ok"}))
}

async fn metrics(State(state): State<AppState>) -> impl IntoResponse {
    (
        [(
            axum::http::header::CONTENT_TYPE,
            "text/plain; version=0.0.4",
        )],
        state.metrics.render(),
    )
}

pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/healthz", get(healthz))
        .route("/metrics", get(metrics))
        .route("/v1/rdf-export/run", post(run))
        .with_state(state)
}

#[derive(Debug, Deserialize)]
pub struct RunRequest {
    pub tenant_id: String,
    /// Optional YYYY-MM-DD pin (graph-service defaults to today, UTC, when
    /// omitted — the EFFECTIVE date comes back in X-Snapshot-Date).
    #[serde(default)]
    pub snapshot_date: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct RunResponse {
    pub tenant_id: String,
    pub snapshot_date: String,
    pub nodes: u64,
    pub edges: u64,
    pub triples: u64,
    pub skipped_unknown: u64,
    pub outputs: Vec<String>,
}

/// Internal run summary (metrics observation).
pub struct RunOutcome {
    pub nodes: u64,
    pub edges: u64,
    pub triples: u64,
    pub skipped_unknown: u64,
    pub duration_ms: u64,
}

#[derive(Debug, Serialize)]
struct ErrorBody {
    error: &'static str,
    detail: String,
}

fn err(status: StatusCode, error: &'static str, detail: String) -> (StatusCode, Json<ErrorBody>) {
    (status, Json(ErrorBody { error, detail }))
}

/// tenant_id must be a single IRI-path/S3-key-safe segment (it is embedded
/// in the named-graph IRI, node IRIs, and the MinIO key).
fn tenant_id_valid(t: &str) -> bool {
    !t.is_empty()
        && t.len() <= 100
        && t.chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '.' | '_' | '~'))
}

fn snapshot_date_valid(s: &str) -> bool {
    chrono::NaiveDate::parse_from_str(s, "%Y-%m-%d").is_ok()
}

async fn run(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<RunRequest>,
) -> axum::response::Response {
    // Auth FIRST (fail-closed): 503 when the server token is unset, 401 on
    // missing/mismatched X-Internal-Token.
    match auth::check_internal_token(state.config.internal_token.as_deref(), &headers) {
        auth::Auth::Ok => {}
        auth::Auth::Unconfigured => {
            return err(
                StatusCode::SERVICE_UNAVAILABLE,
                "internal_token_unconfigured",
                "RDF_EXPORT_INTERNAL_TOKEN is unset; the export API is fail-closed".into(),
            )
            .into_response();
        }
        auth::Auth::Unauthenticated => {
            return err(
                StatusCode::UNAUTHORIZED,
                "unauthorized",
                "missing or invalid X-Internal-Token".into(),
            )
            .into_response();
        }
    }

    if !tenant_id_valid(&body.tenant_id) {
        return err(
            StatusCode::UNPROCESSABLE_ENTITY,
            "invalid_tenant_id",
            "tenant_id must be 1-100 chars of [A-Za-z0-9._~-]".into(),
        )
        .into_response();
    }
    if let Some(snap) = body.snapshot_date.as_deref() {
        if !snapshot_date_valid(snap) {
            return err(
                StatusCode::UNPROCESSABLE_ENTITY,
                "invalid_snapshot_date",
                format!("snapshot_date {snap:?} is not YYYY-MM-DD"),
            )
            .into_response();
        }
    }

    let started = Instant::now();
    match tokio::time::timeout(
        std::time::Duration::from_secs(state.config.export_timeout_s),
        run_export(&state, &body),
    )
    .await
    {
        Ok(Ok(resp)) => {
            state.metrics.runs_ok.fetch_add(1, Ordering::Relaxed);
            state.metrics.observe_run(&RunOutcome {
                nodes: resp.nodes,
                edges: resp.edges,
                triples: resp.triples,
                skipped_unknown: resp.skipped_unknown,
                duration_ms: started.elapsed().as_millis() as u64,
            });
            (StatusCode::OK, Json(resp)).into_response()
        }
        Ok(Err(e)) => {
            state.metrics.runs_error.fetch_add(1, Ordering::Relaxed);
            e.into_response()
        }
        Err(_) => {
            state.metrics.runs_error.fetch_add(1, Ordering::Relaxed);
            err(
                StatusCode::GATEWAY_TIMEOUT,
                "export_timeout",
                format!(
                    "export exceeded RDF_EXPORT_TIMEOUT_S={}s",
                    state.config.export_timeout_s
                ),
            )
            .into_response()
        }
    }
}

async fn run_export(
    state: &AppState,
    body: &RunRequest,
) -> Result<RunResponse, (StatusCode, Json<ErrorBody>)> {
    let cfg = &state.config;
    let client = GraphClient::new(
        state.http.clone(),
        cfg.graph_service_url.clone(),
        cfg.graph_internal_token.clone(),
    );
    let storage = MinioClient::new(
        state.http.clone(),
        cfg.minio_endpoint.clone(),
        cfg.minio_region.clone(),
        cfg.minio_bucket.clone(),
        cfg.minio_access_key.clone(),
        cfg.minio_secret_key.clone(),
    );

    let mut mapper = RdfMapper::new(&body.tenant_id);
    let snap_req = body.snapshot_date.as_deref();

    let nodes_meta = client
        .stream_nodes(&body.tenant_id, snap_req, &mut |row| {
            mapper.add_node(&row);
            Ok(())
        })
        .await
        .map_err(|e| {
            err(
                StatusCode::BAD_GATEWAY,
                "graph_export_nodes_failed",
                e.to_string(),
            )
        })?;
    let edges_meta = client
        .stream_edges(&body.tenant_id, snap_req, &mut |row| {
            mapper.add_edge(&row);
            Ok(())
        })
        .await
        .map_err(|e| {
            err(
                StatusCode::BAD_GATEWAY,
                "graph_export_edges_failed",
                e.to_string(),
            )
        })?;

    // Effective snapshot date: the server's X-Snapshot-Date is authoritative
    // (it defaults the param when omitted). Both streams must agree.
    let snapshot_date = match (nodes_meta.snapshot_date, edges_meta.snapshot_date) {
        (Some(a), Some(b)) if a == b => a,
        (Some(a), Some(b)) => {
            return Err(err(
                StatusCode::BAD_GATEWAY,
                "snapshot_date_mismatch",
                format!("nodes export stamped {a}, edges export stamped {b}"),
            ));
        }
        (a, b) => a.or(b).unwrap_or_else(|| {
            body.snapshot_date
                .clone()
                .unwrap_or_else(|| chrono::Utc::now().format("%Y-%m-%d").to_string())
        }),
    };
    mapper.add_snapshot_metadata(&snapshot_date);

    let docs = serialize::render(&mapper.quads).map_err(|e| {
        err(
            StatusCode::INTERNAL_SERVER_ERROR,
            "serialize_failed",
            e.to_string(),
        )
    })?;
    let triples = mapper.quads.len() as u64;

    let prefix = format!("extracts/graph_rdf/{}/{snapshot_date}", body.tenant_id);
    let ttl_key = format!("{prefix}/graph.ttl");
    let jsonld_key = format!("{prefix}/graph.jsonld");
    let mut outputs = Vec::with_capacity(2);
    for (key, bytes, ctype) in [
        (&ttl_key, docs.turtle.as_bytes(), "text/turtle"),
        (&jsonld_key, docs.jsonld.as_bytes(), "application/ld+json"),
    ] {
        let uri = storage
            .put_object(key, bytes, ctype)
            .await
            .map_err(|e| err(StatusCode::BAD_GATEWAY, "minio_put_failed", e.to_string()))?;
        outputs.push(uri);
    }

    tracing::info!(
        tenant = %body.tenant_id,
        snapshot_date = %snapshot_date,
        nodes = mapper.nodes,
        edges = mapper.edges,
        triples,
        skipped_unknown = mapper.skipped_unknown,
        "rdf export run complete"
    );

    Ok(RunResponse {
        tenant_id: body.tenant_id.clone(),
        snapshot_date,
        nodes: mapper.nodes,
        edges: mapper.edges,
        triples,
        skipped_unknown: mapper.skipped_unknown,
        outputs,
    })
}
