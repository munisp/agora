//! OpenDesk billing-engine (SPEC-W7 Part B): usage metering ingestion,
//! rating/invoicing, QR payments (Paystack + static EMV), dunning.
//!
//! Layout mirrors payments-service (config/consumer/ledger/routes + main),
//! with a Postgres pool (sqlx) as the system of record for usage/invoices and
//! an in-memory double-entry receivables ledger (ADR-0007 sim pattern).

mod config;
mod consumer;
mod dunning;
mod invoices;
mod ledger;
mod metering;
mod models;
mod payments_qr;
mod routes;
mod tenant;

use std::net::SocketAddr;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use rdkafka::producer::{FutureProducer, FutureRecord};
use serde::Serialize;
use sqlx::postgres::PgPoolOptions;
use sqlx::PgPool;
use tokio::sync::watch;
use tracing::{error, info, warn};
use tracing_subscriber::EnvFilter;

use crate::ledger::{BillingLedger, SimLedgerClient};

#[derive(Clone)]
pub struct AppState {
    pub pool: PgPool,
    /// Pool for cross-tenant internal jobs (dunning sweep, Paystack webhook
    /// invoice lookup) — SPEC-W34 GF6. Built from INTERNAL_DATABASE_URL (role
    /// `app_billing_internal_login`); falls back to the main pool when unset
    /// (dev default: bootstrap superuser, which bypasses RLS anyway).
    pub internal_pool: PgPool,
    pub ledger: Arc<dyn BillingLedger>,
    /// rdkafka producer for opendesk.billing.events (None when Kafka is
    /// unavailable at boot; event publication degrades to logged + counted).
    pub producer: Option<FutureProducer>,
    pub http: reqwest::Client,
    pub config: Arc<config::Config>,
    pub events_published: Arc<AtomicU64>,
    pub events_failed: Arc<AtomicU64>,
}

impl AppState {
    /// Best-effort event publication (same contract as payments-service):
    /// state changes commit first; publication failures are logged + counted.
    pub async fn publish_event<T: Serialize>(
        &self,
        type_name: &str,
        subject: &str,
        tenant_id: &str,
        data: T,
    ) {
        let event = models::CloudEvent::new(
            "billing-engine",
            &format!("com.opendesk.billing.{type_name}"),
            subject,
            tenant_id,
            data,
        );
        let payload = match serde_json::to_vec(&event) {
            Ok(p) => p,
            Err(e) => {
                self.events_failed.fetch_add(1, Ordering::Relaxed);
                warn!(error = %e, "billing event serialize failed");
                return;
            }
        };
        let Some(producer) = &self.producer else {
            self.events_failed.fetch_add(1, Ordering::Relaxed);
            warn!(type_ = %event.type_, "no kafka producer; billing event dropped");
            return;
        };
        let record = FutureRecord::to(self.config.billing_events_topic.as_str())
            .key(tenant_id)
            .payload(payload.as_slice());
        match producer.send(record, Duration::from_secs(5)).await {
            Ok(_) => {
                self.events_published.fetch_add(1, Ordering::Relaxed);
            }
            Err((e, _msg)) => {
                self.events_failed.fetch_add(1, Ordering::Relaxed);
                warn!(error = %e, type_ = %event.type_, "billing event publish failed");
            }
        }
    }
}

async fn connect_pool(database_url: &str) -> Result<PgPool, Box<dyn std::error::Error>> {
    let mut attempt = 0u32;
    loop {
        attempt += 1;
        match PgPoolOptions::new()
            .max_connections(10)
            .connect(database_url)
            .await
        {
            Ok(pool) => return Ok(pool),
            Err(e) if attempt < 30 => {
                warn!(error = %e, attempt, "postgres not ready; retrying");
                tokio::time::sleep(Duration::from_secs(2)).await;
            }
            Err(e) => return Err(e.into()),
        }
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
        payment_mode = cfg.payment_mode(),
        usage_topic = %cfg.usage_events_topic,
        "starting billing-engine"
    );

    let pool = connect_pool(&cfg.database_url).await?;
    // Idempotent schema bootstrap (same pattern as notification-worker; the
    // `billing` database itself is created by infra/postgres init scripts).
    sqlx::raw_sql(include_str!("../migrations/0001_init.sql"))
        .execute(&pool)
        .await?;
    // SPEC-W34 GF6: ENABLE+FORCE RLS on all billing tables. Applied by the
    // same bootstrap connection (must own the tables — dev default uses the
    // bootstrap superuser). Fail-closed: if RLS cannot be applied the service
    // refuses to start rather than serving cross-tenant data.
    sqlx::raw_sql(include_str!("../migrations/0002_rls.sql"))
        .execute(&pool)
        .await?;
    info!("billing schema applied (incl. 0002 RLS)");

    let internal_pool = match &cfg.internal_database_url {
        Some(dsn) => {
            info!("internal jobs pool: INTERNAL_DATABASE_URL configured");
            connect_pool(dsn).await?
        }
        None => {
            warn!(
                "INTERNAL_DATABASE_URL unset; internal jobs (dunning, webhook lookup) \
                 share the main pool — only safe while DATABASE_URL bypasses RLS \
                 (dev superuser default)"
            );
            pool.clone()
        }
    };

    let producer: Option<FutureProducer> = match rdkafka::config::ClientConfig::new()
        .set("bootstrap.servers", &cfg.kafka_brokers)
        .set("message.timeout.ms", "10000")
        .create()
    {
        Ok(p) => Some(p),
        Err(e) => {
            error!(error = %e, "failed to create kafka producer; events will be dropped");
            None
        }
    };

    let state = AppState {
        pool,
        internal_pool,
        ledger: Arc::new(SimLedgerClient::new()),
        producer,
        http: reqwest::Client::new(),
        config: Arc::new(cfg.clone()),
        events_published: Arc::new(AtomicU64::new(0)),
        events_failed: Arc::new(AtomicU64::new(0)),
    };

    let (shutdown_tx, shutdown_rx) = watch::channel(false);
    let consumer_handle = if cfg.kafka_consumer_enabled {
        Some(tokio::spawn(consumer::run(state.clone(), shutdown_rx.clone())))
    } else {
        None
    };
    let dunning_handle = tokio::spawn(dunning::run(state.clone(), shutdown_rx));

    let app = routes::router(state);
    let addr = SocketAddr::from(([0, 0, 0, 0], cfg.port));
    let listener = tokio::net::TcpListener::bind(addr).await?;
    info!(%addr, "billing-engine listening");
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal(shutdown_tx))
        .await?;

    if let Some(handle) = consumer_handle {
        let _ = handle.await;
    }
    let _ = dunning_handle.await;
    info!("billing-engine stopped");
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
