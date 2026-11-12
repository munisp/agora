//! OpenDesk geo-service (SPEC-W52 Scope A): terrio-based boundary ingest
//! (GeoJSON/Shapefile/GeoPackage/zip -> PostGIS geo_boundaries with RLS),
//! point-in-polygon enrichment (coords -> state/lga/ward) for
//! booking-service's civic/fieldcapture enrichment, and self-hosted
//! PMTiles tile/style serving for admin-web's map (no more OSM hotlink).
//!
//! Boot posture (payments-service idiom): config parse is fail-closed, the
//! Postgres pool connects with retry, migrations/0001_geo.sql is applied at
//! startup via sqlx::raw_sql and a failure ABORTS the boot (never serve
//! without the RLS policy). GEO_TILES_PATH unset is NOT a boot error — the
//! tiles endpoints answer fail-closed 503 (src/tiles.rs).

mod auth;
mod config;
mod enrich;
mod import;
mod routes;
mod store;
mod tiles;

use std::sync::Arc;

use tracing::{info, warn};
use tracing_subscriber::EnvFilter;

/// Shared outbound HTTP client (payments RS-006 idiom): 5s connect / 30s
/// overall so a hung import URL can never park a request forever.
pub fn http_client() -> reqwest::Client {
    reqwest::Client::builder()
        .connect_timeout(std::time::Duration::from_secs(5))
        .timeout(std::time::Duration::from_secs(30))
        .build()
        .expect("reqwest client with static timeout configuration must build")
}

#[derive(Clone)]
pub struct AppState {
    pub pool: sqlx::PgPool,
    pub config: Arc<config::Config>,
    /// Permify manage_bookings check for boundary import (any scope).
    pub permify: auth::PermifyClient,
    /// Shared client for {url} imports.
    pub http: reqwest::Client,
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
            // Fail-closed startup: no silent defaults on the DB DSN.
            tracing::error!(error = %e, "invalid configuration; refusing to start");
            return Err(e.into());
        }
    };
    info!(bind = %cfg.bind, tiles_configured = cfg.tiles_path.is_some(), "starting geo-service");
    if cfg.tiles_path.is_none() {
        warn!("GEO_TILES_PATH unset: /v1/geo/tiles/* and /v1/geo/style.json answer 503 (disabled-config semantics)");
    }
    if cfg.authz_disabled {
        warn!("AUTHZ_DISABLED=true: boundary import skips the Permify manage_bookings gate (dev only)");
    }
    if cfg.permify_url.is_none() && !cfg.authz_disabled {
        warn!("PERMIFY_URL unset: boundary import authz gate is OFF (dev escape — billing-engine idiom)");
    }

    let pool = store::connect_with_retry(&cfg.database_url).await?;
    // Fail-closed: the RLS migration must apply before we serve anything.
    store::apply_geo_migration(&pool).await?;

    let state = AppState {
        pool,
        config: Arc::new(cfg.clone()),
        permify: auth::PermifyClient::new(cfg.permify_url.clone()),
        http: http_client(),
    };

    let app = routes::router(state);
    let listener = tokio::net::TcpListener::bind(cfg.bind).await?;
    info!(addr = %cfg.bind, "geo-service listening");
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await?;
    info!("geo-service stopped");
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
