//! OpenDesk gateway-edge (SPEC §3/§5/§12): per-tenant WebSocket fan-out.
//!
//! Sources: Kafka `opendesk.booking.events` (primary) and Fluvio
//! `opendesk.transcripts-raw` (live tail, feature `fluvio-live`).
//! Sinks: `/ws` and `/ws/transcripts` per-tenant WebSocket channels with a
//! drop-slow backpressure policy and Prometheus text metrics at `/metrics`.

mod auth;
mod bus;
mod config;
mod enriched_consumer;
mod fluvio_consumer;
mod health;
mod kafka_consumer;
mod metrics;
mod ws;

use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use axum::{routing::get, Json, Router};
use tokio::sync::watch;
use tracing::info;
use tracing_subscriber::EnvFilter;

use crate::bus::EventBus;

#[derive(Clone)]
pub struct AppState {
    pub bus: Arc<EventBus>,
    pub auth: auth::Authenticator,
    pub config: Arc<config::Config>,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    tracing_subscriber::fmt()
        .with_env_filter(
            EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info")),
        )
        .json()
        .init();

    let cfg = config::Config::from_env();
    // SEC#12: hard startup error on invariant violations (KEYCLOAK_AUDIENCE
    // required outside explicit dev posture) — never boot half-configured.
    if let Err(e) = cfg.validate() {
        tracing::error!(error = %e, "gateway-edge configuration invalid; refusing to start");
        return Err(e.into());
    }
    info!(
        port = cfg.port,
        auth_disabled = cfg.auth_disabled,
        dev_mode = cfg.dev_mode,
        "starting gateway-edge"
    );

    let authenticator = if cfg.auth_disabled {
        tracing::warn!("EDGE_AUTH_DISABLED=true: JWT validation is OFF (dev only)");
        auth::Authenticator::Disabled
    } else {
        auth::Authenticator::Jwks(Arc::new(auth::JwksValidator::new(
            cfg.jwks_url.clone(),
            cfg.issuer.clone(),
            cfg.audience.clone(),
            Duration::from_secs(cfg.jwks_cache_ttl_secs),
        )))
    };

    let bus = Arc::new(EventBus::new(cfg.ws_channel_capacity));
    let state = AppState {
        bus: bus.clone(),
        auth: authenticator,
        config: Arc::new(cfg.clone()),
    };

    let (shutdown_tx, shutdown_rx) = watch::channel(false);

    // Kafka primary consumer (booking events).
    let kafka_handle = tokio::spawn(kafka_consumer::run(
        bus.clone(),
        cfg.kafka_brokers.clone(),
        cfg.kafka_group_id.clone(),
        cfg.booking_events_topic.clone(),
        shutdown_rx.clone(),
    ));

    // Kafka consumer for enriched conversation turns (-> /ws/intel).
    let enriched_handle = tokio::spawn(enriched_consumer::run(
        bus.clone(),
        cfg.kafka_brokers.clone(),
        cfg.kafka_group_id.clone(),
        cfg.enriched_topic.clone(),
        shutdown_rx.clone(),
    ));

    // Fluvio live-tail (transcripts). Real consumer only when built with
    // `fluvio-live`; otherwise FAIL CLOSED (no silent simulation) unless the
    // operator explicitly opts into the no-op stub via GATEWAY_EDGE_ALLOW_SIM.
    #[cfg(not(feature = "fluvio-live"))]
    if !cfg.fluvio_stub_allowed {
        tracing::error!(
            topic = %cfg.fluvio_transcripts_topic,
            endpoint = %cfg.fluvio_endpoint,
            "fluvio live-tail is not compiled into this build; refusing to \
             silently simulate transcript consumption. Rebuild with \
             `--features fluvio-live`, or set GATEWAY_EDGE_ALLOW_SIM=true to \
             explicitly opt into running without the transcript source"
        );
        return Err(
            "fluvio consumer unavailable: build with --features fluvio-live \
             or set GATEWAY_EDGE_ALLOW_SIM=true (opt-in stub)"
                .into(),
        );
    }
    let fluvio_handle = tokio::spawn(fluvio_consumer::run(
        bus.clone(),
        cfg.fluvio_endpoint.clone(),
        cfg.fluvio_transcripts_topic.clone(),
        cfg.fluvio_partitions,
        shutdown_rx,
    ));

    let app = Router::new()
        .route("/healthz", get(healthz))
        .route("/metrics", get(metrics_handler))
        .route("/ws", get(ws::ws_booking_events))
        .route("/ws/transcripts", get(ws::ws_transcripts))
        .route("/ws/intel", get(ws::ws_intel))
        .with_state(state);

    let addr = SocketAddr::from(([0, 0, 0, 0], cfg.port));
    let listener = tokio::net::TcpListener::bind(addr).await?;
    info!(%addr, "gateway-edge listening");
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal(shutdown_tx))
        .await?;

    let _ = kafka_handle.await;
    let _ = enriched_handle.await;
    let _ = fluvio_handle.await;
    info!("gateway-edge stopped");
    Ok(())
}

/// F15-07: dependency-aware health. 200 {"status":"ok"} only while every
/// consumer task (Kafka booking events, Kafka enriched turns, Fluvio
/// transcript tail) is alive and has beaten within the last 30s; otherwise
/// 503 {"status":"degraded"} with per-consumer detail. `/metrics` stays
/// static and is unaffected.
async fn healthz() -> (axum::http::StatusCode, Json<serde_json::Value>) {
    let (all_healthy, consumers) = health::report();
    let detail: Vec<serde_json::Value> = consumers
        .iter()
        .map(|c| {
            serde_json::json!({
                "name": c.name,
                "healthy": c.healthy,
                "exited": c.exited,
                "last_beat_age_secs": c.age_secs,
            })
        })
        .collect();
    let (code, status) = if all_healthy {
        (axum::http::StatusCode::OK, "ok")
    } else {
        (axum::http::StatusCode::SERVICE_UNAVAILABLE, "degraded")
    };
    (
        code,
        Json(serde_json::json!({
            "status": status,
            "service": "gateway-edge",
            "consumers": detail,
        })),
    )
}

async fn metrics_handler() -> (
    [(axum::http::header::HeaderName, &'static str); 1],
    String,
) {
    (
        [(axum::http::header::CONTENT_TYPE, "text/plain; version=0.0.4")],
        metrics::render(),
    )
}

async fn shutdown_signal(shutdown_tx: watch::Sender<bool>) {
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
    let _ = shutdown_tx.send(true);
}

#[cfg(test)]
mod tests {
    use super::*;

    /// F15-07 contract: /healthz is 503 {"status":"degraded"} when any
    /// consumer heartbeat is stale > 30s or the task exited, and 200
    /// {"status":"ok"} while all consumers beat within the window.
    #[tokio::test]
    async fn healthz_reflects_consumer_liveness() {
        // Stale (never beaten in the test process) -> degraded 503.
        let (code, Json(body)) = healthz().await;
        assert_eq!(code, axum::http::StatusCode::SERVICE_UNAVAILABLE);
        assert_eq!(body["status"], "degraded");
        assert_eq!(body["service"], "gateway-edge");
        assert_eq!(body["consumers"].as_array().unwrap().len(), 3);

        // All consumers beating -> ok 200.
        for c in health::ALL {
            c.beat();
        }
        let (code, Json(body)) = healthz().await;
        assert_eq!(code, axum::http::StatusCode::OK);
        assert_eq!(body["status"], "ok");

        // One exited consumer (fresh beat notwithstanding) -> degraded 503.
        health::KAFKA_ENRICHED.mark_exited();
        let (code, Json(body)) = healthz().await;
        assert_eq!(code, axum::http::StatusCode::SERVICE_UNAVAILABLE);
        assert_eq!(body["status"], "degraded");
        let enriched = body["consumers"]
            .as_array()
            .unwrap()
            .iter()
            .find(|c| c["name"] == "kafka-enriched-turns")
            .unwrap();
        assert_eq!(enriched["exited"], true);
        assert_eq!(enriched["healthy"], false);
    }
}
