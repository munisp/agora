//! OpenDesk payments-service (SPEC §9): ledger-centric payments with a
//! TigerBeetle-compatible `LedgerClient` (ADR-0007), Mojaloop payout rail,
//! Dapr pubsub outbox to Kafka, Temporal activity handlers, and an idempotent
//! Kafka commands consumer.

mod config;
mod consumer;
mod dapr;
mod events;
mod flutterwave;
mod ledger;
mod mojaloop;
mod routes;

use std::net::SocketAddr;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

use serde::Serialize;
use tokio::sync::watch;
use tracing::{info, warn};
use tracing_subscriber::EnvFilter;

use crate::ledger::{LedgerClient, sim::SimLedgerClient};

#[derive(Clone)]
pub struct AppState {
    pub ledger: Arc<dyn LedgerClient>,
    pub outbox: dapr::DaprOutbox,
    pub mojaloop: mojaloop::MojaloopAdapter,
    /// SPEC-W12 §6: Flutterwave checkout rail (env-configured; initialize is
    /// 503 until FLUTTERWAVE_SECRET_KEY is set, webhook 503 until
    /// FLUTTERWAVE_SECRET_HASH is set).
    pub flutterwave: flutterwave::FlutterwaveAdapter,
    pub config: Arc<config::Config>,
    pub events_published: Arc<AtomicU64>,
    pub events_failed: Arc<AtomicU64>,
}

impl AppState {
    /// Best-effort outbox (ADR-0007 note): ledger ops commit first; event
    /// publication failures are logged + counted, not rolled back. A
    /// reconciler can republish from the ledger.
    pub async fn publish_event<T: Serialize>(
        &self,
        type_name: &str,
        subject: &str,
        tenant_id: &str,
        data: T,
    ) {
        let event = events::CloudEvent::new(
            "payments-service",
            &format!("com.opendesk.payments.{type_name}"),
            subject,
            tenant_id,
            data,
        );
        match self.outbox.publish(&event).await {
            Ok(()) => {
                self.events_published.fetch_add(1, Ordering::Relaxed);
            }
            Err(e) => {
                self.events_failed.fetch_add(1, Ordering::Relaxed);
                warn!(
                    error = %e,
                    type_ = %event.type_,
                    "dapr pubsub publish failed (best-effort outbox)"
                );
            }
        }
    }
}

async fn build_ledger(
    cfg: &config::Config,
) -> Result<Arc<dyn LedgerClient>, Box<dyn std::error::Error>> {
    match cfg.ledger_impl.as_str() {
        "sim" => {
            info!(fee_bps = cfg.platform_fee_bps, "using in-memory sim ledger (ADR-0007)");
            Ok(Arc::new(SimLedgerClient::new(cfg.platform_fee_bps)))
        }
        "tigerbeetle" => {
            #[cfg(feature = "tb-live")]
            {
                info!(addresses = %cfg.tb_addresses, "connecting to tigerbeetle");
                let client = ledger::tigerbeetle::TigerBeetleClient::connect(
                    &cfg.tb_addresses,
                    cfg.tb_cluster_id,
                    ledger::LEDGER_ID,
                    cfg.platform_fee_bps,
                )
                .await?;
                Ok(Arc::new(client))
            }
            #[cfg(not(feature = "tb-live"))]
            {
                Err("LEDGER_IMPL=tigerbeetle requires building with `--features tb-live` \
                     (ADR-0007); default build ships the sim ledger only"
                    .into())
            }
        }
        other => Err(format!("unknown LEDGER_IMPL '{other}' (expected sim|tigerbeetle)").into()),
    }
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
    info!(
        port = cfg.port,
        ledger_impl = %cfg.ledger_impl,
        mojaloop = %cfg.mojaloop_endpoint,
        "starting payments-service"
    );

    let ledger = build_ledger(&cfg).await?;
    let outbox = dapr::DaprOutbox::new(
        cfg.dapr_base_url(),
        cfg.dapr_pubsub.clone(),
        cfg.events_topic.clone(),
    );
    let state = AppState {
        ledger,
        outbox,
        mojaloop: mojaloop::MojaloopAdapter::new(cfg.mojaloop_endpoint.clone()),
        flutterwave: flutterwave::FlutterwaveAdapter::from_env(),
        config: Arc::new(cfg.clone()),
        events_published: Arc::new(AtomicU64::new(0)),
        events_failed: Arc::new(AtomicU64::new(0)),
    };

    let (shutdown_tx, shutdown_rx) = watch::channel(false);
    let consumer_handle = if cfg.kafka_consumer_enabled {
        Some(tokio::spawn(consumer::run(state.clone(), shutdown_rx)))
    } else {
        None
    };

    // SPEC-W12 §6: Flutterwave routes merged additively (module self-registers
    // /v1/payments/flutterwave/initialize + /webhooks/flutterwave).
    let app = routes::router(state.clone()).merge(flutterwave::router(state));
    let addr = SocketAddr::from(([0, 0, 0, 0], cfg.port));
    let listener = tokio::net::TcpListener::bind(addr).await?;
    info!(%addr, "payments-service listening");
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal(shutdown_tx))
        .await?;

    if let Some(handle) = consumer_handle {
        let _ = handle.await;
    }
    info!("payments-service stopped");
    Ok(())
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
