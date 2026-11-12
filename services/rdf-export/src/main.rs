//! OpenDesk rdf-export (SPEC-W54 Scope A): batch linked-data exporter.
//! Consumes graph-service's tenant-scoped internal NDJSON export
//! (/v1/graph/internal/export/{nodes,edges}, X-Internal-Token, mandatory
//! tenant_id, snapshot_date partitioning), projects property-graph rows to
//! RDF against ontology/agora.ttl (labels->classes, edge types->predicates,
//! tenant->named graph, hashed ids pass-through), serializes Turtle +
//! JSON-LD 1.1 via oxirs, and lands both at
//! s3://lake/extracts/graph_rdf/{tenant_id}/{snapshot_date}/ (the parquet
//! extracts' sibling; infra/lakehouse/spark/jobs/graph_export.py contract).
//!
//! Mesh-internal only (NO APISIX route). Boot posture: config parse is
//! fail-closed (GRAPH_SERVICE_URL / GRAPH_INTERNAL_TOKEN / MINIO creds are
//! required); RDF_EXPORT_INTERNAL_TOKEN unset is NOT a boot error — the API
//! then answers fail-closed 503 (geo-service disabled-config idiom).

mod auth;
mod client;
mod config;
mod mapping;
mod metrics;
mod routes;
mod serialize;
mod storage;

use std::sync::Arc;

use tracing::{info, warn};
use tracing_subscriber::EnvFilter;

/// Shared outbound HTTP client: 5s connect timeout, NO overall timeout —
/// export streams are long-lived by design; the run-level budget is
/// RDF_EXPORT_TIMEOUT_S (routes.rs), not a per-request ceiling.
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

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    tracing_subscriber::fmt()
        .with_env_filter(
            EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info")),
        )
        .json()
        .init();

    let cfg = match config::Config::from_env() {
        Ok(c) => c,
        Err(e) => {
            tracing::error!(error = %e, "invalid configuration; refusing to start");
            return Err(e.into());
        }
    };
    info!(bind = %cfg.bind, graph_service = %cfg.graph_service_url, bucket = %cfg.minio_bucket, "starting rdf-export");
    if cfg.internal_token.is_none() {
        warn!("RDF_EXPORT_INTERNAL_TOKEN unset: /v1/rdf-export/* answers fail-closed 503 (disabled-config semantics)");
    }

    let state = AppState {
        config: Arc::new(cfg.clone()),
        http: http_client(),
        metrics: Arc::new(metrics::Metrics::default()),
    };

    let app = routes::router(state);
    let listener = tokio::net::TcpListener::bind(cfg.bind).await?;
    info!(addr = %cfg.bind, "rdf-export listening");
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await?;
    info!("rdf-export stopped");
    Ok(())
}

async fn shutdown_signal() {
    let ctrl_c = async {
        let _ = tokio::signal::ctrl_c().await;
    };
    #[cfg(unix)]
    let terminate = async {
        if let Ok(mut sig) =
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
        {
            sig.recv().await;
        }
    };
    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => {},
        _ = terminate => {},
    }
    info!("shutdown signal received");
}
