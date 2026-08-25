//! OpenDesk billing-engine (SPEC-W7 Part B): usage metering ingestion,
//! rating/invoicing, QR payments (Paystack + static EMV), dunning.
//!
//! Layout mirrors payments-service (config/consumer/ledger/routes + main),
//! with a Postgres pool (sqlx) as the system of record for usage/invoices and
//! an in-memory double-entry receivables ledger (ADR-0007 sim pattern).

mod config;
mod consumer;
mod dunning;
mod identity;
mod invoices;
mod ledger;
mod metering;
mod models;
mod outbox;
mod payments_qr;
mod routes;
mod tenant;

use std::net::SocketAddr;
use std::sync::atomic::AtomicU64;
use std::sync::Arc;
use std::time::Duration;

use rdkafka::producer::FutureProducer;
use sqlx::postgres::PgPoolOptions;
use sqlx::PgPool;
use tokio::sync::watch;
use tracing::{error, info, warn};
use tracing_subscriber::EnvFilter;

use crate::ledger::BillingLedger;

/// Shared outbound HTTP client construction (RS-006): explicit timeouts —
/// 5s connect, 30s overall — so a hung rail cannot park a request forever.
pub fn http_client() -> reqwest::Client {
    reqwest::Client::builder()
        .connect_timeout(Duration::from_secs(5))
        .timeout(Duration::from_secs(30))
        .build()
        .expect("reqwest client with static timeout configuration must build")
}

#[derive(Clone)]
pub struct AppState {
    pub pool: PgPool,
    /// Pool for cross-tenant internal jobs (dunning sweep, Paystack webhook
    /// invoice lookup, outbox relay) — SPEC-W34 GF6. Built from
    /// INTERNAL_DATABASE_URL (role `app_billing_internal_login`); falls back
    /// to the main pool when unset (dev default: bootstrap superuser, which
    /// bypasses RLS anyway).
    pub internal_pool: PgPool,
    pub ledger: Arc<dyn BillingLedger>,
    /// rdkafka producer for the outbox relay (None when Kafka is unavailable
    /// at boot; events stay durable in billing_outbox until a restart with a
    /// working broker — RS-001: they are never silently dropped).
    pub producer: Option<FutureProducer>,
    pub http: reqwest::Client,
    pub config: Arc<config::Config>,
    /// K1 slug -> uuid resolver (SPEC-W44 F1): binds a gateway-claimed
    /// X-Tenant-Slugs entry to a uuid-form tenant param via identity's
    /// GET /v1/tenants/{slug}. Fail-closed on identity errors.
    pub identity: Arc<identity::SlugResolver>,
    /// Signalled after a transaction commits with a fresh outbox row so the
    /// relay flushes immediately instead of waiting for the poll tick.
    pub outbox_notify: Arc<tokio::sync::Notify>,
    /// billing_events_published_total (Prometheus text exposition: /metrics).
    pub events_published: Arc<AtomicU64>,
    /// billing_events_failed_total.
    pub events_failed: Arc<AtomicU64>,
    /// Usage events dead-lettered after bounded retries (B-04).
    pub usage_dead_lettered: Arc<AtomicU64>,
    /// F15-07 (/metrics): usage events handled successfully.
    pub usage_processed: Arc<AtomicU64>,
    /// DLQ sink for failed usage events (Kafka-backed when the producer was
    /// created at boot, otherwise the fail-closed unavailable sink — B-04).
    pub dlq: Arc<dyn consumer::DlqSink>,
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

/// SIM-029: ledger selection. `postgres` (default) constructs the durable
/// PgLedgerClient and FAILS THE BOOT when it cannot be built (missing
/// tables/config/DB) — there is no silent fallback to the sim. `sim` is a
/// dev/CI opt-in only.
async fn build_ledger(
    cfg: &config::Config,
    pool: &PgPool,
) -> Result<Arc<dyn BillingLedger>, Box<dyn std::error::Error>> {
    match cfg.billing_ledger_impl.as_str() {
        "postgres" => {
            info!("billing ledger: postgres-backed (durable, default)");
            Ok(Arc::new(ledger::PgLedgerClient::new(pool.clone()).await?))
        }
        "sim" => {
            warn!("BILLING_LEDGER_IMPL=sim: in-memory ledger (dev/CI only; NON-DURABLE)");
            Ok(Arc::new(ledger::SimLedgerClient::new()))
        }
        // Unreachable (config::from_env validates), but fail closed anyway.
        other => Err(format!(
            "unknown BILLING_LEDGER_IMPL '{other}' (expected postgres|sim)"
        )
        .into()),
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
            // Fail-closed startup (RS-002/SIM-029/SIM-030): no silent defaults
            // on auth tokens, ledger selection, or merchant coordinates.
            error!(error = %e, "invalid configuration; refusing to start");
            return Err(e.into());
        }
    };
    info!(
        port = cfg.port,
        payment_mode = cfg.payment_mode(),
        ledger_impl = %cfg.billing_ledger_impl,
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
    // SIM-029: durable ledger tables (PgLedgerClient storage).
    sqlx::raw_sql(include_str!("../migrations/0003_ledger.sql"))
        .execute(&pool)
        .await?;
    // RS-001: durable event outbox (InvoicePaid can never be silently lost).
    sqlx::raw_sql(include_str!("../migrations/0004_outbox.sql"))
        .execute(&pool)
        .await?;
    // SPEC-W43 B-07: hardening — billing_outbox RLS (internal-role gated),
    // REVOKE DELETE on the ledger tables from the app roles, and the
    // invoices.period format CHECK.
    sqlx::raw_sql(include_str!("../migrations/0005_hardening.sql"))
        .execute(&pool)
        .await?;
    info!("billing schema applied (incl. 0002 RLS, 0003 ledger, 0004 outbox, 0005 hardening)");

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

    // SIM-029: build the ledger AFTER the schema bootstrap so the postgres
    // implementation's table probe can only fail on real mis-provisioning.
    let ledger = build_ledger(&cfg, &pool).await?;

    let producer: Option<FutureProducer> = match rdkafka::config::ClientConfig::new()
        .set("bootstrap.servers", &cfg.kafka_brokers)
        .set("message.timeout.ms", "10000")
        .create()
    {
        Ok(p) => Some(p),
        Err(e) => {
            // Not fatal: outbox rows stay durable (RS-001) and the relay
            // drains them on the next boot with a working broker.
            error!(error = %e, "failed to create kafka producer; billing events accumulate in the outbox");
            None
        }
    };

    // B-04: DLQ sink for the usage consumer. Kafka-backed when the producer
    // exists; otherwise the fail-closed unavailable sink (failed events are
    // then never offset-committed and get redelivered instead of lost).
    let dlq: Arc<dyn consumer::DlqSink> = match &producer {
        Some(p) => Arc::new(consumer::KafkaDlqSink::new(p.clone(), cfg.dlq_topic.clone())),
        None => Arc::new(consumer::UnavailableDlqSink),
    };

    let state = AppState {
        pool,
        internal_pool,
        ledger,
        producer,
        http: http_client(),
        config: Arc::new(cfg.clone()),
        identity: Arc::new(identity::SlugResolver::new(
            http_client(),
            &cfg.identity_base_url,
            cfg.identity_internal_token.clone(),
            Duration::from_secs(cfg.tenant_cache_ttl_s),
        )),
        outbox_notify: Arc::new(tokio::sync::Notify::new()),
        events_published: Arc::new(AtomicU64::new(0)),
        events_failed: Arc::new(AtomicU64::new(0)),
        usage_dead_lettered: Arc::new(AtomicU64::new(0)),
        usage_processed: Arc::new(AtomicU64::new(0)),
        dlq,
    };

    let (shutdown_tx, shutdown_rx) = watch::channel(false);
    let consumer_handle = if cfg.kafka_consumer_enabled {
        Some(tokio::spawn(consumer::run(state.clone(), shutdown_rx.clone())))
    } else {
        None
    };
    let dunning_handle = tokio::spawn(dunning::run(state.clone(), shutdown_rx.clone()));
    let outbox_handle = tokio::spawn(outbox::run(state.clone(), shutdown_rx));

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
    let _ = outbox_handle.await;
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
