//! OpenDesk payments-service (SPEC §9): ledger-centric payments with a
//! TigerBeetle-compatible `LedgerClient` (ADR-0007), Mojaloop payout rail,
//! Dapr pubsub outbox to Kafka, Temporal activity handlers, and an idempotent
//! Kafka commands consumer.

mod auth;
mod config;
mod consumer;
mod dapr;
mod event_failures;
mod events;
mod flutterwave;
mod ledger;
mod mojaloop;
mod payouts;
mod registry;
mod routes;
mod tenant;
mod transfers;

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
    /// SPEC-W43 P-09 (C1): internal-token auth + gateway tenant binding.
    pub auth: auth::AuthConfig,
    /// SPEC-W43 P-01 (C3): durable payout attempt records for the reconciler
    /// (Postgres when a DSN is configured; in-memory dev fallback otherwise).
    pub payout_attempts: Arc<dyn payouts::PayoutAttemptStore>,
    /// SPEC-W44 K7: payee registry (payout beneficiaries) + deposit
    /// provenance. Postgres when a DSN is configured; in-memory dev fallback
    /// otherwise (same lifecycle as `payout_attempts`).
    pub registry: Arc<dyn registry::Registry>,
    /// SPEC-W45: durable rail-attempt records for /v1/transfers and the K12
    /// refund rail (replay returns the ORIGINAL outcome). Same DSN posture
    /// as `payout_attempts`.
    pub transfer_attempts: Arc<dyn transfers::TransferAttemptStore>,
    /// SPEC-W49 A1: durable log of capture events whose Dapr pubsub
    /// publication exhausted every retry (no capture event is silently
    /// lost). Same DSN posture as `payout_attempts`; replayed via the
    /// K2-guarded `POST /internal/events/replay` route.
    pub event_failures: Arc<dyn event_failures::EventFailureStore>,
    /// SPEC-W46 R3: per-process set of tenants whose ledger accounts have
    /// already been ensured by THIS process. `create_accounts` is idempotent
    /// (exists-ok) and ledger accounts are immutable once created, so after
    /// the first successful ensure the per-request create_accounts round
    /// trip (a full TB RTT on the live ledger) is pure waste. Entries are
    /// only inserted AFTER a successful ensure, so a failure is retried on
    /// the next request exactly as before.
    pub ensured_tenants: Arc<std::sync::Mutex<std::collections::HashSet<String>>>,
    pub events_published: Arc<AtomicU64>,
    pub events_failed: Arc<AtomicU64>,
    /// GF11 error metric: commands dead-lettered after bounded retries.
    pub commands_dead_lettered: Arc<AtomicU64>,
    /// F15-03 (/metrics): commands handled successfully.
    pub commands_processed: Arc<AtomicU64>,
    /// F15-03 (/metrics): payout attempts by outcome.
    pub payouts_attempted: Arc<AtomicU64>,
    pub payouts_committed: Arc<AtomicU64>,
    pub payouts_failed: Arc<AtomicU64>,
    pub payouts_unknown: Arc<AtomicU64>,
}

impl AppState {
    /// SPEC-W46 R3: ensure the tenant's ledger accounts exist, running the
    /// (idempotent, exists-ok) `create_accounts` round trip at most once per
    /// tenant per process. Callers that need the returned account snapshots
    /// (only POST /v1/accounts/provision) call `ledger.create_accounts`
    /// directly. Semantics unchanged: the first call for a tenant still
    /// awaits the ledger and propagates any error; nothing is cached on
    /// failure.
    pub async fn ensure_accounts(&self, tenant_id: &str) -> Result<(), ledger::LedgerError> {
        {
            let ensured = self
                .ensured_tenants
                .lock()
                .unwrap_or_else(|p| p.into_inner());
            if ensured.contains(tenant_id) {
                return Ok(());
            }
        }
        self.ledger.create_accounts(tenant_id).await?;
        self.ensured_tenants
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .insert(tenant_id.to_string());
        Ok(())
    }

    /// Best-effort outbox (ADR-0007 note): ledger ops commit first; event
    /// publication failures are logged + counted, not rolled back. A
    /// reconciler can republish from the ledger.
    ///
    /// SPEC-W46 R5: fire-and-forget. Every call site publishes ONLY after
    /// the durable ledger/attempt-record write, and NO route derives any
    /// part of its response from the publish result (failures were already
    /// swallowed into a counter + warn log), so the request path no longer
    /// awaits the daprd/broker RTT (up to the 30s reqwest timeout on sidecar
    /// trouble). The publish itself — including the metric accounting and
    /// error logging — runs in a spawned task; the event payload is fully
    /// built synchronously BEFORE the spawn, so what is published is exactly
    /// what the caller passed. `async` is kept on the signature purely so
    /// the existing `.await` call sites are untouched; the future completes
    /// immediately after the spawn.
    pub async fn publish_event<T: Serialize + Send + Sync + 'static>(
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
        let outbox = self.outbox.clone();
        let events_published = self.events_published.clone();
        let events_failed = self.events_failed.clone();
        tokio::spawn(async move {
            match outbox.publish(&event).await {
                Ok(()) => {
                    events_published.fetch_add(1, Ordering::Relaxed);
                }
                Err(e) => {
                    events_failed.fetch_add(1, Ordering::Relaxed);
                    warn!(
                        error = %e,
                        type_ = %event.type_,
                        "dapr pubsub publish failed (best-effort outbox)"
                    );
                }
            }
        });
    }

    /// SPEC-W49 A1: RELIABLE outbox for the `FlutterwavePaymentCaptured`
    /// event. payments-service has no DB outbox pattern (the ledger commits
    /// via the TigerBeetle-compatible LedgerClient, not a Postgres
    /// transaction — see src/event_failures.rs for the tier decision), so
    /// the SPEC's minimal honest alternative applies: retry-with-backoff
    /// ([`event_failures::PUBLISH_ATTEMPTS`] attempts), then a durable
    /// `payment_event_failures` record so no capture event is silently lost,
    /// replayable via the K2-guarded `POST /internal/events/replay`.
    ///
    /// Same SPEC-W46 R5 fire-and-forget posture as `publish_event`: the
    /// event is fully built BEFORE the spawn, the request path never awaits
    /// the sidecar/broker RTT, and no response depends on the outcome — the
    /// webhook's capture is already committed when this runs.
    pub async fn publish_event_reliable<T: Serialize + Send + Sync + 'static>(
        &self,
        type_name: &str,
        tx_ref: &str,
        tenant_id: &str,
        data: T,
    ) {
        let event = events::CloudEvent::new(
            "payments-service",
            &format!("com.opendesk.payments.{type_name}"),
            tx_ref,
            tenant_id,
            data,
        );
        let outbox = self.outbox.clone();
        let failures = self.event_failures.clone();
        let events_published = self.events_published.clone();
        let events_failed = self.events_failed.clone();
        let tx_ref = tx_ref.to_string();
        let tenant_id = tenant_id.to_string();
        tokio::spawn(async move {
            let mut last_err: Option<reqwest::Error> = None;
            for attempt in 1..=event_failures::PUBLISH_ATTEMPTS {
                if attempt > 1 {
                    let backoff = std::time::Duration::from_millis(
                        event_failures::RETRY_BACKOFF_MS[(attempt - 2) as usize],
                    );
                    tokio::time::sleep(backoff).await;
                }
                match outbox.publish(&event).await {
                    Ok(()) => {
                        events_published.fetch_add(1, Ordering::Relaxed);
                        return;
                    }
                    Err(e) => {
                        warn!(
                            attempt,
                            error = %e,
                            type_ = %event.type_,
                            tx_ref = %tx_ref,
                            "dapr pubsub publish failed; retrying"
                        );
                        last_err = Some(e);
                    }
                }
            }
            // Every retry exhausted: record the FULL CloudEvent durably so
            // the capture event is never silently lost; replay re-publishes
            // it verbatim (same event id => idempotent consumers dedupe).
            events_failed.fetch_add(1, Ordering::Relaxed);
            let err_text = last_err
                .map(|e| e.to_string())
                .unwrap_or_else(|| "publish failed".to_string());
            let failure = event_failures::PaymentEventFailure {
                id: uuid::Uuid::new_v4(),
                tx_ref: tx_ref.clone(),
                tenant_id,
                payload: serde_json::to_value(&event)
                    .unwrap_or_else(|_| serde_json::json!({})),
                error: err_text.clone(),
                created_at: chrono::Utc::now(),
            };
            if let Err(e) = failures.record(&failure).await {
                tracing::error!(
                    error = %e,
                    publish_error = %err_text,
                    tx_ref = %tx_ref,
                    "payment_event_failures record failed; capture event is ONLY in this log line"
                );
            } else {
                tracing::error!(
                    tx_ref = %tx_ref,
                    publish_error = %err_text,
                    "capture event publish exhausted retries; recorded to payment_event_failures \
                     (replay via POST /internal/events/replay)"
                );
            }
        });
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
    let registry_store: Arc<dyn registry::Registry>;
    let transfer_store: Arc<dyn transfers::TransferAttemptStore>;
    let event_failure_store: Arc<dyn event_failures::EventFailureStore>;
    // P-01 (C3): durable payout_attempts store. When a DSN is configured
    // (PAYMENTS_DATABASE_URL / DATABASE_URL / PG_DSN) the table is
    // bootstrapped at startup and an unreachable database fails the boot
    // (fail-closed). Without a DSN the store is in-memory (dev only).
    //
    // SPEC-W47 DB-01: with a DSN configured, all three stores bootstrap their
    // tables first, then migrations/0001_rls.sql is applied (ENABLE+FORCE
    // RLS + tenant policies + least-privilege roles) — fail-closed: if the
    // RLS migration cannot be applied the service refuses to start rather
    // than serving cross-tenant money data. The cross-tenant internal paths
    // (reconciler sweep, approval lookup) use
    // PAYMENTS_INTERNAL_DATABASE_URL when set, else the main pool.
    let payout_attempts: Arc<dyn payouts::PayoutAttemptStore> = match &cfg.database_url {
        Some(dsn) => {
            let payout_pg = payouts::PgPayoutAttemptStore::connect_with_retry(
                dsn,
                cfg.internal_database_url.as_deref(),
            )
            .await?;
            if cfg.internal_database_url.is_none() {
                warn!("PAYMENTS_INTERNAL_DATABASE_URL unset: cross-tenant internal payouts                        paths (reconciler sweep, approval lookup) use the main pool (dev superuser posture)");
            }
            // SPEC-W44 K7: payee registry + deposit provenance store (same
            // DSN posture as payout_attempts).
            let registry_pg = registry::PgRegistry::connect_with_retry(dsn).await?;
            // SPEC-W45: rail_attempts store for /v1/transfers + the K12
            // refund rail.
            let transfers_pg = transfers::PgTransferAttemptStore::connect_with_retry(dsn).await?;
            // SPEC-W49 A1: durable payment_event_failures store (capture
            // events whose publish exhausted every retry; replay route).
            let failures_pg = event_failures::PgEventFailureStore::connect_with_retry(
                dsn,
                cfg.internal_database_url.as_deref(),
            )
            .await?;
            // DB-01: RLS LAST, after every store's bootstrap DDL created its
            // tables (the policies require the relations to exist).
            tenant::apply_rls_migration(payout_pg.pool()).await?;
            registry_store = Arc::new(registry_pg);
            transfer_store = Arc::new(transfers_pg);
            event_failure_store = Arc::new(failures_pg);
            Arc::new(payout_pg)
        }
        None => {
            warn!("PAYMENTS_DATABASE_URL/DATABASE_URL/PG_DSN unset: payout_attempts store is                    in-memory (dev only — reconciliation records are lost on restart)");
            warn!("PAYMENTS_DATABASE_URL/DATABASE_URL/PG_DSN unset: payee registry / deposit                    provenance store is in-memory (dev only — beneficiary records are lost on restart)");
            warn!("PAYMENTS_DATABASE_URL/DATABASE_URL/PG_DSN unset: rail_attempts store is                    in-memory (dev only — transfer/refund replay records are lost on restart)");
            warn!("PAYMENTS_DATABASE_URL/DATABASE_URL/PG_DSN unset: payment_event_failures store is                in-memory (dev only — failed capture events are lost on restart)");
            registry_store = Arc::new(registry::MemRegistry::default());
            transfer_store = Arc::new(transfers::MemTransferAttemptStore::default());
            event_failure_store = Arc::new(event_failures::MemEventFailureStore::default());
            Arc::new(payouts::MemPayoutAttemptStore::default())
        }
    };
    let registry = registry_store;
    let transfer_attempts = transfer_store;
    let event_failures = event_failure_store;
    if cfg.payout_approval_threshold_cents > 0 {
        info!(
            threshold_kobo = cfg.payout_approval_threshold_cents,
            "K20: payouts above the threshold require POST /v1/payouts/:id/approve (owner role)"
        );
    }
    if cfg.internal_token.is_none() && !cfg.trust_direct_tenant {
        warn!("PAYMENTS_INTERNAL_TOKEN unset and OPENDESK_TRUST_DIRECT_TENANT off: money routes                fail closed (503) unless the gateway injects X-Tenant-Slugs (C1)");
    }
    let state = AppState {
        ledger,
        outbox,
        mojaloop: mojaloop::MojaloopAdapter::new(cfg.mojaloop_endpoint.clone()),
        flutterwave: flutterwave::FlutterwaveAdapter::from_env(),
        config: Arc::new(cfg.clone()),
        dlq,
        auth: auth::AuthConfig::new(
            cfg.internal_token.clone(),
            cfg.trust_direct_tenant,
            cfg.money_roles.clone(),
        ),
        payout_attempts,
        registry,
        transfer_attempts,
        event_failures,
        ensured_tenants: Arc::new(std::sync::Mutex::new(std::collections::HashSet::new())),
        events_published: Arc::new(AtomicU64::new(0)),
        events_failed: Arc::new(AtomicU64::new(0)),
        commands_dead_lettered: Arc::new(AtomicU64::new(0)),
        commands_processed: Arc::new(AtomicU64::new(0)),
        payouts_attempted: Arc::new(AtomicU64::new(0)),
        payouts_committed: Arc::new(AtomicU64::new(0)),
        payouts_failed: Arc::new(AtomicU64::new(0)),
        payouts_unknown: Arc::new(AtomicU64::new(0)),
    };

    let (shutdown_tx, shutdown_rx) = watch::channel(false);
    let consumer_handle = if cfg.kafka_consumer_enabled {
        Some(tokio::spawn(consumer::run(state.clone(), shutdown_rx)))
    } else {
        None
    };
    // P-01 (C3): reconciler sweeping unknown payout rail outcomes.
    let reconciler_handle = tokio::spawn(payouts::run_reconciler(
        state.clone(),
        shutdown_tx.subscribe(),
    ));

    // SPEC-W12 §6: Flutterwave routes merged additively (module self-registers
    // /v1/payments/flutterwave/initialize + /webhooks/flutterwave). SPEC-W49
    // A1: the K2-guarded failed-event replay route self-registers too.
    let app = routes::router(state.clone())
        .merge(flutterwave::router(state.clone()))
        .merge(event_failures::router(state));
    let addr = SocketAddr::from(([0, 0, 0, 0], cfg.port));
    let listener = tokio::net::TcpListener::bind(addr).await?;
    info!(%addr, "payments-service listening");
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal(shutdown_tx))
        .await?;

    if let Some(handle) = consumer_handle {
        let _ = handle.await;
    }
    let _ = reconciler_handle.await;
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
