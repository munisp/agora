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

/// Shared outbound HTTP client construction (RS-006): every reqwest client in
/// this service gets explicit timeouts — 5s connect, 30s overall — so a hung
/// rail/sidecar cannot park a money-path request forever.
pub fn http_client() -> reqwest::Client {
    reqwest::Client::builder()
        .connect_timeout(std::time::Duration::from_secs(5))
        .timeout(std::time::Duration::from_secs(30))
        .build()
        .expect("reqwest client with static timeout configuration must build")
}

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
    /// SPEC-W34 GF11: dead-letter sink for failed payments commands
    /// (`opendesk.dlq`, booking-service idiom). When the Kafka producer could
    /// not be created this is the fail-closed `UnavailableDlqSink`.
    pub dlq: Arc<dyn consumer::DlqSink>,
    pub events_published: Arc<AtomicU64>,
    pub events_failed: Arc<AtomicU64>,
    /// GF11 error metric: commands dead-lettered after bounded retries.
    pub commands_dead_lettered: Arc<AtomicU64>,
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

    let cfg = match config::Config::from_env() {
        Ok(c) => c,
        Err(e) => {
            // Fail-closed startup (GF11): no silent sim-ledger default.
            tracing::error!(error = %e, "invalid configuration; refusing to start");
            return Err(e.into());
        }
    };
    info!(
        port = cfg.port,
        ledger_impl = %cfg.ledger_impl,
        mojaloop = %cfg.mojaloop_endpoint,
        mojaloop_allow_sim = cfg.mojaloop_allow_sim,
        "starting payments-service"
    );
    if cfg.mojaloop_allow_sim && cfg.mojaloop_endpoint == "http://mojaloop:8444" {
        warn!("MOJALOOP_ALLOW_SIM=true: payout rail targets the mojaloop-simulator (dev/CI only)");
    }

    let ledger = build_ledger(&cfg).await?;
    let outbox = dapr::DaprOutbox::new(
        cfg.dapr_base_url(),
        cfg.dapr_pubsub.clone(),
        cfg.events_topic.clone(),
    );
    // GF11: DLQ producer for failed payments commands. If the producer cannot
    // be created, the sink fails closed (failed commands are then never
    // offset-committed, so they redeliver instead of being lost).
    let dlq: Arc<dyn consumer::DlqSink> = match rdkafka::config::ClientConfig::new()
        .set("bootstrap.servers", &cfg.kafka_brokers)
        .set("message.timeout.ms", "10000")
        .create::<rdkafka::producer::FutureProducer>()
    {
        Ok(p) => Arc::new(consumer::KafkaDlqSink::new(p, cfg.dlq_topic.clone())),
        Err(e) => {
            tracing::error!(error = %e, topic = %cfg.dlq_topic,
                "failed to create DLQ kafka producer; failed commands will rely on redelivery");
            Arc::new(consumer::UnavailableDlqSink)
        }
    };
    let state = AppState {
        ledger,
        outbox,
        mojaloop: mojaloop::MojaloopAdapter::new(cfg.mojaloop_endpoint.clone()),
        flutterwave: flutterwave::FlutterwaveAdapter::from_env(),
        config: Arc::new(cfg.clone()),
        dlq,
        events_published: Arc::new(AtomicU64::new(0)),
        events_failed: Arc::new(AtomicU64::new(0)),
        commands_dead_lettered: Arc::new(AtomicU64::new(0)),
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
