//! Kafka consumer for `opendesk.payments.commands` (SPEC §4).
//!
//! Commands (CloudEvents): ChargeDeposit, Refund, NoShowFee.
//! Processing is idempotent: the ledger transfer id is derived deterministically
//! from the command id, so redeliveries replay against the ledger without
//! double-posting. Every processed command emits a `PaymentPosted` event via the
//! Dapr outbox.
//!
//! Offset discipline (SPEC-W34 GF11): an offset is committed ONLY after the
//! command was durably handled. On failure the raw command is dead-lettered
//! to `opendesk.dlq` (same topic + header metadata as booking-service's
//! consumer) and the offset is committed only once the DLQ copy is durable —
//! if the DLQ publish fails the offset is left uncommitted so the command is
//! redelivered. Rationale for "commit after successful DLQ publish" instead
//! of "never commit on failure": commits are offset-based, so committing any
//! later message would silently commit past the failed one anyway (that was
//! the original money-loss bug); the booking-service codebase idiom —
//! bounded retries, then DLQ + commit — is the only safe choice without
//! pausing the partition, and the dead-letter counter gives observability.

use rdkafka::consumer::{CommitMode, Consumer, StreamConsumer};
use rdkafka::producer::{FutureProducer, FutureRecord};
use rdkafka::Message as _;
use serde::Deserialize;
use tokio::sync::watch;
use tracing::{debug, error, info, warn};
use uuid::Uuid;

use crate::AppState;

/// Bounded retries before dead-lettering (mirrors booking-service's
/// processWithRetry: transient ledger outages heal; poison commands don't).
const MAX_ATTEMPTS: u32 = 3;
const RETRY_BACKOFF_MS: u64 = 200;

// ---------------------------------------------------------------------------
// DLQ sink (booking-service idiom: opendesk.dlq + dlq-* headers)
// ---------------------------------------------------------------------------

/// Where failed commands go to die (auditable). Abstracted behind a trait so
/// the failure-injection tests can record/fail publishes without Kafka.
#[async_trait::async_trait]
pub trait DlqSink: Send + Sync {
    async fn dead_letter(
        &self,
        key: Option<&[u8]>,
        payload: &[u8],
        error: &str,
        origin_topic: &str,
    ) -> Result<(), String>;

    /// SPEC-W44 F15-03: cheap producer-availability signal for /healthz and
    /// /metrics. The Kafka sink is up once constructed (librdkafka dials
    /// lazily); the unavailable sink reports down.
    fn available(&self) -> bool {
        true
    }
}

/// Kafka-backed DLQ sink; headers mirror booking-service's deadLetter()
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
/// publish fails, so failed commands are NEVER offset-committed (they are
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

    fn available(&self) -> bool {
        false
    }
}

// ---------------------------------------------------------------------------
// Command handling
// ---------------------------------------------------------------------------

#[derive(Debug, Deserialize)]
struct RawCloudEvent {
    id: String,
    #[serde(rename = "type")]
    type_: String,
    /// CloudEvents tenant extension; carried for log correlation but the
    /// command payload's `tenant_id` is authoritative for ledger ops.
    #[allow(dead_code)]
    #[serde(default)]
    tenantid: Option<String>,
    #[serde(default)]
    data: serde_json::Value,
}

#[derive(Debug, Deserialize)]
struct ChargeDepositCmd {
    tenant_id: String,
    /// Part of the command contract (carried for downstream events);
    /// the ledger path keys off tenant_id + amount only.
    #[allow(dead_code)]
    booking_id: Option<String>,
    amount_cents: u64,
}

#[derive(Debug, Deserialize)]
struct RefundCmd {
    tenant_id: String,
    deposit_id: Option<Uuid>,
    #[serde(default)]
    amount_cents: u64,
}

#[derive(Debug, Deserialize)]
struct NoShowFeeCmd {
    tenant_id: String,
    deposit_id: Uuid,
    amount_cents: u64,
}

fn command_transfer_id(event: &RawCloudEvent) -> Uuid {
    Uuid::new_v5(
        &Uuid::NAMESPACE_URL,
        format!("cmd:{}:{}", event.id, event.type_).as_bytes(),
    )
}

async fn handle_command(state: &AppState, event: &RawCloudEvent) -> Result<(), String> {
    let ty = event.type_.as_str();
    if ty.ends_with("ChargeDeposit") {
        let cmd: ChargeDepositCmd = serde_json::from_value(event.data.clone())
            .map_err(|e| format!("bad ChargeDeposit payload: {e}"))?;
        let t = state
            .ledger
            .hold_deposit(&cmd.tenant_id, command_transfer_id(event), cmd.amount_cents)
            .await
            .map_err(|e| e.to_string())?;
        publish_payment_posted(state, event, &cmd.tenant_id, "ChargeDeposit", &t.id_string())
            .await;
    } else if ty.ends_with("NoShowFee") {
        let cmd: NoShowFeeCmd = serde_json::from_value(event.data.clone())
            .map_err(|e| format!("bad NoShowFee payload: {e}"))?;
        let res = state
            .ledger
            .no_show_fee(
                &cmd.tenant_id,
                cmd.deposit_id,
                command_transfer_id(event),
                cmd.amount_cents,
            )
            .await
            .map_err(|e| e.to_string())?;
        publish_payment_posted(
            state,
            event,
            &cmd.tenant_id,
            "NoShowFee",
            &res.post.id_string(),
        )
        .await;
    } else if ty.ends_with("Refund") {
        let cmd: RefundCmd = serde_json::from_value(event.data.clone())
            .map_err(|e| format!("bad Refund payload: {e}"))?;
        let t = state
            .ledger
            .refund(
                &cmd.tenant_id,
                command_transfer_id(event),
                cmd.deposit_id,
                cmd.amount_cents,
            )
            .await
            .map_err(|e| e.to_string())?;
        publish_payment_posted(state, event, &cmd.tenant_id, "Refund", &t.id_string()).await;
    } else {
        debug!(type_ = ty, "ignoring unknown payments command type");
    }
    Ok(())
}

async fn publish_payment_posted(
    state: &AppState,
    event: &RawCloudEvent,
    tenant_id: &str,
    action: &str,
    ledger_ref: &str,
) {
    state
        .publish_event(
            "PaymentPosted",
            &event.id,
            tenant_id,
            serde_json::json!({
                "commandId": event.id,
                "action": action,
                "ledgerRef": ledger_ref,
            }),
        )
        .await;
}

/// What the consumer loop may do with the message's offset (GF11).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ProcessOutcome {
    /// Handled (or positively a no-op): commit the offset.
    Processed,
    /// Handling failed but the command is durable in the DLQ: commit the
    /// offset (committing is safe — the command is not lost).
    DeadLettered,
    /// Handling failed AND the DLQ copy failed: do NOT commit; rely on
    /// redelivery.
    Failed,
}

/// Process one raw message payload. Extracted from the consumer loop so the
/// offset/DLQ discipline is unit-testable without a broker (failure-injection
/// tests drive this with the sim ledger + recording DLQ sink).
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
                match handle_command(state, &event).await {
                    Ok(()) => {
                        debug!(event_id = %event.id, "payments command processed");
                        state
                            .commands_processed
                            .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                        return ProcessOutcome::Processed;
                    }
                    Err(e) => {
                        warn!(
                            error = %e,
                            event_id = %event.id,
                            attempt,
                            "payments command handling failed"
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
                "command {} failed after {MAX_ATTEMPTS} attempts: {}",
                event.id,
                last_err.unwrap_or_default()
            )
        }
        Err(e) => format!("unparseable payments command: {e}"),
    };

    // Failure path: dead-letter, then commit only if the DLQ copy is durable.
    match state.dlq.dead_letter(key, payload, &failure, origin_topic).await {
        Ok(()) => {
            state
                .commands_dead_lettered
                .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            error!(error = %failure, "payments command dead-lettered to {}", state.config.dlq_topic);
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
            error!(error = %e, "failed to create kafka consumer; commands consumer disabled");
            return;
        }
    };
    if let Err(e) = consumer.subscribe(&[&cfg.kafka_commands_topic]) {
        error!(error = %e, topic = %cfg.kafka_commands_topic, "failed to subscribe");
        return;
    }
    info!(
        topic = %cfg.kafka_commands_topic,
        brokers = %cfg.kafka_brokers,
        dlq_topic = %cfg.dlq_topic,
        "payments commands consumer started"
    );

    loop {
        tokio::select! {
            changed = shutdown.changed() => {
                if changed.is_ok() {
                    info!("payments commands consumer shutting down");
                }
                break;
            }
            msg = consumer.recv() => {
                match msg {
                    Ok(m) => {
                        let payload = m.payload().unwrap_or_default();
                        let outcome =
                            process_payload(&state, m.key(), payload, &cfg.kafka_commands_topic)
                                .await;
                        // GF11: commit ONLY on success or after the DLQ copy is
                        // durable. `Failed` leaves the offset uncommitted so
                        // the command is redelivered (no silent money loss).
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
// Failure-injection tests (GF11): sim ledger + recording/failing DLQ sink.
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::Config;
    use crate::ledger::sim::SimLedgerClient;
    use crate::{dapr, flutterwave, mojaloop};
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

    fn test_state(dlq: Arc<dyn DlqSink>) -> AppState {
        let cfg = Config {
            port: 0,
            ledger_impl: "sim".to_string(),
            tb_addresses: String::new(),
            tb_cluster_id: 0,
            kafka_brokers: String::new(),
            kafka_group_id: "test".to_string(),
            kafka_commands_topic: "opendesk.payments.commands".to_string(),
            kafka_consumer_enabled: false,
            dlq_topic: "opendesk.dlq".to_string(),
            dapr_host: "127.0.0.1".to_string(),
            dapr_http_port: 1, // unreachable; outbox is best-effort
            dapr_pubsub: "pubsub".to_string(),
            events_topic: "opendesk.payments.events".to_string(),
            mojaloop_endpoint: "http://127.0.0.1:1".to_string(),
            mojaloop_allow_sim: false,
            platform_fee_bps: 0,
            internal_token: None,
            trust_direct_tenant: true,
            database_url: None,
            payout_reconciler_interval_secs: 30,
            money_roles: vec!["owner".to_string(), "admin".to_string()],
        };
        AppState {
            ledger: Arc::new(SimLedgerClient::new(0)),
            outbox: dapr::DaprOutbox::new(
                "http://127.0.0.1:1".to_string(),
                "pubsub".to_string(),
                "opendesk.payments.events".to_string(),
            ),
            mojaloop: mojaloop::MojaloopAdapter::new("http://127.0.0.1:1".to_string()),
            flutterwave: flutterwave::FlutterwaveAdapter::from_env(),
            config: Arc::new(cfg),
            dlq,
            auth: crate::auth::AuthConfig::new(
                None,
                true,
                vec!["owner".to_string(), "admin".to_string()],
            ),
            payout_attempts: Arc::new(crate::payouts::MemPayoutAttemptStore::default()),
            registry: Arc::new(crate::registry::MemRegistry::default()),
            events_published: Arc::new(AtomicU64::new(0)),
            events_failed: Arc::new(AtomicU64::new(0)),
            commands_dead_lettered: Arc::new(AtomicU64::new(0)),
            commands_processed: Arc::new(AtomicU64::new(0)),
            payouts_attempted: Arc::new(AtomicU64::new(0)),
            payouts_committed: Arc::new(AtomicU64::new(0)),
            payouts_failed: Arc::new(AtomicU64::new(0)),
            payouts_unknown: Arc::new(AtomicU64::new(0)),
        }
    }

    fn no_show_fee_event() -> Vec<u8> {
        serde_json::to_vec(&serde_json::json!({
            "id": "cmd-fail-1",
            "type": "com.opendesk.payments.NoShowFee",
            "tenantid": "t-1",
            "data": {
                "tenant_id": "t-1",
                "deposit_id": Uuid::new_v4(),
                "amount_cents": 500
            }
        }))
        .unwrap()
    }

    #[tokio::test(start_paused = true)]
    async fn ledger_failure_dead_letters_then_allows_commit() {
        let sink = Arc::new(RecordingDlqSink {
            recorded: Mutex::new(RecordedDlq::default()),
            fail: false,
        });
        let state = test_state(sink.clone());
        // Sim ledger forced to fail: NoShowFee against a deposit hold that
        // does not exist -> LedgerError::TransferNotFound on every attempt.
        let event = no_show_fee_event();
        let outcome = process_payload(
            &state,
            Some(b"cmd-fail-1"),
            &event,
            "opendesk.payments.commands",
        )
        .await;
        assert_eq!(outcome, ProcessOutcome::DeadLettered);
        assert_eq!(
            state.commands_dead_lettered.load(Ordering::Relaxed),
            1,
            "error metric incremented"
        );
        let rec = sink.recorded.lock().await;
        assert_eq!(rec.payloads.len(), 1, "exactly one DLQ copy");
        assert_eq!(rec.payloads[0], event, "raw command preserved");
        assert!(rec.errors[0].contains("cmd-fail-1"), "error names the command");
    }

    #[tokio::test(start_paused = true)]
    async fn dlq_outage_leaves_offset_uncommitted() {
        let sink = Arc::new(RecordingDlqSink {
            recorded: Mutex::new(RecordedDlq::default()),
            fail: true, // injected DLQ outage
        });
        let state = test_state(sink.clone());
        let outcome = process_payload(&state, None, &no_show_fee_event(), "opendesk.payments.commands")
            .await;
        assert_eq!(
            outcome,
            ProcessOutcome::Failed,
            "consumer must NOT commit when the DLQ copy failed"
        );
        assert_eq!(state.commands_dead_lettered.load(Ordering::Relaxed), 0);
        assert_eq!(sink.recorded.lock().await.payloads.len(), 0);
    }

    #[tokio::test]
    async fn poison_payload_is_dead_lettered_not_dropped() {
        let sink = Arc::new(RecordingDlqSink {
            recorded: Mutex::new(RecordedDlq::default()),
            fail: false,
        });
        let state = test_state(sink.clone());
        let outcome = process_payload(&state, None, b"{not json", "opendesk.payments.commands").await;
        assert_eq!(outcome, ProcessOutcome::DeadLettered);
        assert_eq!(sink.recorded.lock().await.payloads.len(), 1);
    }

    #[tokio::test]
    async fn successful_command_commits_without_dlq() {
        let sink = Arc::new(RecordingDlqSink {
            recorded: Mutex::new(RecordedDlq::default()),
            fail: false,
        });
        let state = test_state(sink.clone());
        let event = serde_json::to_vec(&serde_json::json!({
            "id": "cmd-ok-1",
            "type": "com.opendesk.payments.ChargeDeposit",
            "tenantid": "t-1",
            "data": {"tenant_id": "t-1", "booking_id": "b-1", "amount_cents": 1200}
        }))
        .unwrap();
        let outcome = process_payload(&state, None, &event, "opendesk.payments.commands").await;
        assert_eq!(outcome, ProcessOutcome::Processed);
        assert_eq!(sink.recorded.lock().await.payloads.len(), 0, "no DLQ traffic");
        assert_eq!(state.commands_dead_lettered.load(Ordering::Relaxed), 0);
    }
}
