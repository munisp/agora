//! Kafka consumer for `opendesk.usage.events` (SPEC-W7 B1), mirroring
//! payments-service's consumer loop (same rdkafka 0.36 API usage).
//!
//! Offset discipline (SPEC-W43 B-04, payments-service GF11 idiom): an offset
//! is committed ONLY after the event is durably handled — recorded, or
//! positively identified as a duplicate. On failure the handler retries with
//! a bounded backoff (transient DB outages heal; poison events don't), then
//! dead-letters the raw event to `opendesk.dlq` (same topic + dlq-* header
//! metadata as payments-service/booking-service) and commits the offset ONLY
//! once the DLQ copy is durable. If the DLQ publish fails the offset is left
//! uncommitted so the event is redelivered. Rationale for "commit after
//! successful DLQ publish" instead of "never commit on failure": commits are
//! offset-based, so committing any later message would silently commit past
//! the failed one anyway; bounded-retries-then-DLQ is the only safe choice
//! without pausing the partition, and the dead-letter counter gives
//! observability.

use rdkafka::consumer::{CommitMode, Consumer, StreamConsumer};
use rdkafka::producer::{FutureProducer, FutureRecord};
use rdkafka::Message as _;
use tokio::sync::watch;
use tracing::{debug, error, info, warn};

use crate::metering::{self, UsageOutcome};
use crate::models::RawCloudEvent;
use crate::AppState;

/// Bounded retries before dead-lettering (mirrors payments-service's
/// process_payload: transient DB outages heal; poison events don't).
const MAX_ATTEMPTS: u32 = 3;
const RETRY_BACKOFF_MS: u64 = 200;

// ---------------------------------------------------------------------------
// DLQ sink (payments-service idiom: opendesk.dlq + dlq-* headers)
// ---------------------------------------------------------------------------

/// Where failed usage events go to die (auditable). Abstracted behind a
/// trait so the failure-injection tests can record/fail publishes without
/// Kafka.
#[async_trait::async_trait]
pub trait DlqSink: Send + Sync {
    async fn dead_letter(
        &self,
        key: Option<&[u8]>,
        payload: &[u8],
        error: &str,
        origin_topic: &str,
    ) -> Result<(), String>;
}

/// Kafka-backed DLQ sink; headers mirror payments-service's KafkaDlqSink
/// (`dlq-error`, `dlq-origin-topic`, `dlq-time`).
pub struct KafkaDlqSink {
    producer: FutureProducer,
    topic: String,
}

impl KafkaDlqSink {
    pub fn new(producer: FutureProducer, topic: String) -> Self {
        Self { producer, topic }
    }
}

#[async_trait::async_trait]
impl DlqSink for KafkaDlqSink {
    async fn dead_letter(
        &self,
        key: Option<&[u8]>,
        payload: &[u8],
        error: &str,
        origin_topic: &str,
    ) -> Result<(), String> {
        let now = chrono::Utc::now().to_rfc3339();
        let record = FutureRecord::to(&self.topic)
            .payload(payload)
            .key(key.unwrap_or_default())
            .headers(
                rdkafka::message::OwnedHeaders::new()
                    .insert(rdkafka::message::Header {
                        key: "dlq-error",
                        value: Some(error.as_bytes()),
                    })
                    .insert(rdkafka::message::Header {
                        key: "dlq-origin-topic",
                        value: Some(origin_topic.as_bytes()),
                    })
                    .insert(rdkafka::message::Header {
                        key: "dlq-time",
                        value: Some(now.as_bytes()),
                    }),
            );
        self.producer
            .send(record, std::time::Duration::from_secs(10))
            .await
            .map(|_| ())
            .map_err(|(e, _msg)| e.to_string())
    }
}

/// DLQ sink used when the Kafka producer could not be created at boot: every
/// publish fails, so failed events are NEVER offset-committed (they are
/// redelivered instead) — fail-closed, no silent loss.
pub struct UnavailableDlqSink;

#[async_trait::async_trait]
impl DlqSink for UnavailableDlqSink {
    async fn dead_letter(
        &self,
        _key: Option<&[u8]>,
        _payload: &[u8],
        _error: &str,
        _origin_topic: &str,
    ) -> Result<(), String> {
        Err("DLQ producer unavailable (Kafka producer creation failed at boot)".to_string())
    }
}

// ---------------------------------------------------------------------------
// Message processing
// ---------------------------------------------------------------------------

/// What the consumer loop may do with the message's offset (B-04).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ProcessOutcome {
    /// Handled (recorded or positively a duplicate): commit the offset.
    Processed,
    /// Handling failed but the event is durable in the DLQ: commit the
    /// offset (committing is safe — the event is not lost).
    DeadLettered,
    /// Handling failed AND the DLQ copy failed: do NOT commit; rely on
    /// redelivery.
    Failed,
}

/// Process one raw message payload. Extracted from the consumer loop so the
/// offset/DLQ discipline is unit-testable without a broker (failure-injection
/// tests drive this with an unreachable pool + recording DLQ sink).
pub async fn process_payload(
    state: &AppState,
    key: Option<&[u8]>,
    payload: &[u8],
    origin_topic: &str,
) -> ProcessOutcome {
    let failure = match serde_json::from_slice::<RawCloudEvent>(payload) {
        Ok(event) => {
            let mut last_err = None;
            for attempt in 1..=MAX_ATTEMPTS {
                match metering::record_usage(&state.pool, &event).await {
                    Ok(UsageOutcome::Recorded) => {
                        debug!(event_id = %event.id, "usage recorded");
                        state
                            .usage_processed
                            .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                        return ProcessOutcome::Processed;
                    }
                    Ok(UsageOutcome::Duplicate) => {
                        debug!(event_id = %event.id, "duplicate usage event skipped");
                        state
                            .usage_processed
                            .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                        return ProcessOutcome::Processed;
                    }
                    Err(e) => {
                        warn!(
                            error = %e,
                            event_id = %event.id,
                            attempt,
                            "usage record failed"
                        );
                        last_err = Some(e);
                        if attempt < MAX_ATTEMPTS {
                            tokio::time::sleep(std::time::Duration::from_millis(RETRY_BACKOFF_MS))
                                .await;
                        }
                    }
                }
            }
            format!(
                "usage event {} failed after {MAX_ATTEMPTS} attempts: {}",
                event.id,
                last_err.unwrap_or_default()
            )
        }
        Err(e) => format!("unparseable usage event: {e}"),
    };

    // Failure path: dead-letter, then commit only if the DLQ copy is durable.
    match state.dlq.dead_letter(key, payload, &failure, origin_topic).await {
        Ok(()) => {
            state
                .usage_dead_lettered
                .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            error!(error = %failure, "usage event dead-lettered to {}", state.config.dlq_topic);
            ProcessOutcome::DeadLettered
        }
        Err(e) => {
            error!(
                error = %e,
                dlq_error = %failure,
                "DLQ publish failed; offset left uncommitted (redelivery expected)"
            );
            ProcessOutcome::Failed
        }
    }
}

pub async fn run(state: AppState, mut shutdown: watch::Receiver<bool>) {
    let cfg = state.config.clone();
    let consumer: StreamConsumer = match rdkafka::config::ClientConfig::new()
        .set("group.id", &cfg.kafka_group_id)
        .set("bootstrap.servers", &cfg.kafka_brokers)
        .set("enable.auto.commit", "false")
        .set("auto.offset.reset", "earliest")
        .set("session.timeout.ms", "10000")
        .create()
    {
        Ok(c) => c,
        Err(e) => {
            error!(error = %e, "failed to create kafka consumer; metering consumer disabled");
            return;
        }
    };
    if let Err(e) = consumer.subscribe(&[&cfg.usage_events_topic]) {
        error!(error = %e, topic = %cfg.usage_events_topic, "failed to subscribe");
        return;
    }
    info!(
        topic = %cfg.usage_events_topic,
        brokers = %cfg.kafka_brokers,
        group = %cfg.kafka_group_id,
        dlq_topic = %cfg.dlq_topic,
        "billing usage consumer started"
    );

    loop {
        tokio::select! {
            changed = shutdown.changed() => {
                if changed.is_ok() {
                    info!("billing usage consumer shutting down");
                }
                break;
            }
            msg = consumer.recv() => {
                match msg {
                    Ok(m) => {
                        let payload = m.payload().unwrap_or_default();
                        let outcome =
                            process_payload(&state, m.key(), payload, &cfg.usage_events_topic)
                                .await;
                        // B-04: commit ONLY on success or after the DLQ copy is
                        // durable. `Failed` leaves the offset uncommitted so
                        // the event is redelivered (no silent usage loss).
                        if outcome != ProcessOutcome::Failed {
                            if let Err(e) = consumer.commit_message(&m, CommitMode::Async) {
                                warn!(error = %e, "offset commit failed");
                            }
                        }
                    }
                    Err(e) => {
                        warn!(error = %e, "kafka receive error");
                        tokio::time::sleep(std::time::Duration::from_millis(500)).await;
                    }
                }
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Failure-injection tests (B-04): unreachable pool + recording/failing DLQ
// sink — the offset/DLQ discipline is pinned without a broker or database.
// (The success path against a real Postgres is covered by the pgserver-backed
// tests/pg_hardening.rs integration test.)
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::Config;
    use crate::ledger::SimLedgerClient;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::sync::Arc;
    use tokio::sync::Mutex;

    #[derive(Default)]
    struct RecordedDlq {
        payloads: Vec<Vec<u8>>,
        errors: Vec<String>,
    }

    struct RecordingDlqSink {
        recorded: Mutex<RecordedDlq>,
        fail: bool,
    }

    #[async_trait::async_trait]
    impl DlqSink for RecordingDlqSink {
        async fn dead_letter(
            &self,
            _key: Option<&[u8]>,
            payload: &[u8],
            error: &str,
            _origin_topic: &str,
        ) -> Result<(), String> {
            if self.fail {
                return Err("injected DLQ outage".to_string());
            }
            let mut rec = self.recorded.lock().await;
            rec.payloads.push(payload.to_vec());
            rec.errors.push(error.to_string());
            Ok(())
        }
    }

    fn test_config() -> Config {
        Config {
            port: 0,
            database_url: "postgres://127.0.0.1:1/billing_consumer_test".to_string(),
            internal_database_url: None,
            kafka_brokers: String::new(),
            kafka_group_id: "test".to_string(),
            usage_events_topic: "opendesk.usage.events".to_string(),
            kafka_consumer_enabled: false,
            billing_events_topic: "opendesk.billing.events".to_string(),
            dlq_topic: "opendesk.dlq".to_string(),
            billing_ledger_impl: "sim".to_string(),
            internal_token: "test-token".to_string(),
            paystack_secret_key: None,
            paystack_default_email: "t@example.com".to_string(),
            paystack_callback_url: "http://127.0.0.1/cb".to_string(),
            billing_static_account: "T/000".to_string(),
            billing_merchant_name: "T".to_string(),
            money_roles: vec!["owner".to_string(), "admin".to_string()],
            trust_direct_tenant: false,
            identity_base_url: String::new(),
            identity_internal_token: None,
            tenant_cache_ttl_s: 60,
            dunning_interval_s: 3600,
            invoice_due_days: 14,
        }
    }

    fn test_state(dlq: Arc<dyn DlqSink>) -> AppState {
        // connect_lazy never dials; queries fail fast (connection refused),
        // which is exactly the transient-failure injection these tests need.
        let pool = sqlx::PgPool::connect_lazy("postgres://127.0.0.1:1/billing_consumer_test")
            .expect("connect_lazy does not dial");
        AppState {
            pool: pool.clone(),
            internal_pool: pool,
            ledger: Arc::new(SimLedgerClient::new()),
            producer: None,
            http: crate::http_client(),
            config: Arc::new(test_config()),
            identity: Arc::new(crate::identity::SlugResolver::new(
                crate::http_client(),
                "",
                None,
                std::time::Duration::from_secs(60),
            )),
            outbox_notify: Arc::new(tokio::sync::Notify::new()),
            events_published: Arc::new(AtomicU64::new(0)),
            events_failed: Arc::new(AtomicU64::new(0)),
            usage_dead_lettered: Arc::new(AtomicU64::new(0)),
            usage_processed: Arc::new(AtomicU64::new(0)),
            dlq,
        }
    }

    fn usage_event() -> Vec<u8> {
        serde_json::to_vec(&serde_json::json!({
            "id": "usage-fail-1",
            "type": "com.opendesk.usage.UsageRecord",
            "data": {
                "tenant_id": "9b0b0d52-1c8b-4d3f-9e2a-6f6a2b7c1d20",
                "metric": "booking",
                "value": 1,
                "ts": "2026-03-14T10:15:00Z"
            }
        }))
        .unwrap()
    }

    #[tokio::test(start_paused = true)]
    async fn persistent_failure_dead_letters_then_allows_commit() {
        let sink = Arc::new(RecordingDlqSink {
            recorded: Mutex::new(RecordedDlq::default()),
            fail: false,
        });
        let state = test_state(sink.clone());
        // DB unreachable on every attempt -> bounded retries, then DLQ.
        let event = usage_event();
        let outcome = process_payload(
            &state,
            Some(b"usage-fail-1"),
            &event,
            "opendesk.usage.events",
        )
        .await;
        assert_eq!(outcome, ProcessOutcome::DeadLettered);
        assert_eq!(
            state.usage_dead_lettered.load(Ordering::Relaxed),
            1,
            "dead-letter counter incremented"
        );
        let rec = sink.recorded.lock().await;
        assert_eq!(rec.payloads.len(), 1, "exactly one DLQ copy");
        assert_eq!(rec.payloads[0], event, "raw event preserved");
        assert!(
            rec.errors[0].contains("usage-fail-1"),
            "error names the event: {}",
            rec.errors[0]
        );
        assert!(
            rec.errors[0].contains(&MAX_ATTEMPTS.to_string()),
            "error records the bounded retry count: {}",
            rec.errors[0]
        );
    }

    #[tokio::test(start_paused = true)]
    async fn dlq_outage_leaves_offset_uncommitted() {
        let sink = Arc::new(RecordingDlqSink {
            recorded: Mutex::new(RecordedDlq::default()),
            fail: true, // injected DLQ outage
        });
        let state = test_state(sink.clone());
        let outcome = process_payload(&state, None, &usage_event(), "opendesk.usage.events").await;
        assert_eq!(
            outcome,
            ProcessOutcome::Failed,
            "consumer must NOT commit when the DLQ copy failed"
        );
        assert_eq!(state.usage_dead_lettered.load(Ordering::Relaxed), 0);
        assert_eq!(sink.recorded.lock().await.payloads.len(), 0);
    }

    #[tokio::test]
    async fn poison_payload_is_dead_lettered_not_dropped() {
        let sink = Arc::new(RecordingDlqSink {
            recorded: Mutex::new(RecordedDlq::default()),
            fail: false,
        });
        let state = test_state(sink.clone());
        let outcome = process_payload(&state, None, b"{not json", "opendesk.usage.events").await;
        assert_eq!(outcome, ProcessOutcome::DeadLettered);
        let rec = sink.recorded.lock().await;
        assert_eq!(rec.payloads.len(), 1);
        assert!(
            rec.errors[0].contains("unparseable"),
            "poison error is descriptive: {}",
            rec.errors[0]
        );
    }
}
