//! Primary event source: Kafka `opendesk.booking.events` (CloudEvents JSON,
//! SPEC §4). Events are fanned out to the tenant's `booking:{slug}` channel.

use std::sync::Arc;

use rdkafka::consumer::{CommitMode, Consumer, StreamConsumer};
use rdkafka::Message as _;
use serde::Deserialize;
use tokio::sync::watch;
use tracing::{debug, error, info, warn};

use crate::bus;
use crate::bus::EventBus;
use crate::health;
use crate::metrics;

#[derive(Debug, Deserialize)]
pub(crate) struct RawCloudEvent {
    /// CloudEvents `subject`: the TENANT SLUG per contract C6 (producers put
    /// the slug here). This is the primary routing key — subscribers listen
    /// on `{channel}:{slug}`.
    #[serde(default)]
    subject: Option<String>,
    /// `tenantid` extension: the tenant UUID (not the slug). Fallback
    /// routing key for producers that predate C6.
    #[serde(default)]
    tenantid: Option<String>,
    #[serde(default)]
    data: serde_json::Value,
}

/// Tenant routing key per contract C6: prefer the CloudEvents `subject`
/// (tenant slug), fall back to the `tenantid` extension (tenant uuid) when
/// subject is absent, then common `data.tenantId` / `data.tenant_id`
/// fallbacks. Shared with the enriched-turns consumer (enriched_consumer.rs).
pub(crate) fn extract_tenant(event: &RawCloudEvent) -> Option<String> {
    if let Some(s) = &event.subject {
        if !s.is_empty() {
            return Some(s.clone());
        }
    }
    if let Some(t) = &event.tenantid {
        if !t.is_empty() {
            return Some(t.clone());
        }
    }
    event
        .data
        .get("tenantId")
        .or_else(|| event.data.get("tenant_id"))
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn event(json: &str) -> RawCloudEvent {
        serde_json::from_str(json).expect("test event parses")
    }

    /// C6 regression: booking-style event — subject=slug, tenantid=uuid —
    /// must route to booking:{slug} (subscribers listen on the slug; before
    /// this fix the uuid was used and fan-out was silently dead).
    #[test]
    fn subject_slug_preferred_over_tenantid_uuid() {
        let e = event(r#"{
            "specversion":"1.0","type":"com.opendesk.booking.BookingCreated",
            "source":"booking-service","id":"evt-1",
            "subject":"acme",
            "tenantid":"9b1d0e6a-3d4f-4a2c-9c77-1f2a3b4c5d6e",
            "data":{"booking_id":"b1"}
        }"#);
        let tenant = extract_tenant(&e).expect("tenant");
        assert_eq!(tenant, "acme");
        assert_eq!(bus::booking_channel(&tenant), "booking:acme");
        assert_eq!(bus::intel_channel(&tenant), "intel:acme");
    }

    /// C6 regression: enriched intel event (subject=slug) routes to
    /// intel:{slug}.
    #[test]
    fn enriched_intel_event_routes_by_subject_slug() {
        let e = event(r#"{
            "specversion":"1.0","type":"com.opendesk.conversation.TurnEnriched",
            "source":"conversation-service","id":"evt-2",
            "subject":"globex",
            "tenantid":"11111111-2222-3333-4444-555555555555",
            "data":{"sentiment":"positive"}
        }"#);
        assert_eq!(extract_tenant(&e).as_deref(), Some("globex"));
    }

    /// Legacy producers without `subject` still route by tenantid (uuid).
    #[test]
    fn uuid_only_event_falls_back_to_tenantid() {
        let e = event(r#"{
            "specversion":"1.0","type":"com.opendesk.booking.BookingCreated",
            "tenantid":"9b1d0e6a-3d4f-4a2c-9c77-1f2a3b4c5d6e",
            "data":{}
        }"#);
        assert_eq!(
            extract_tenant(&e).as_deref(),
            Some("9b1d0e6a-3d4f-4a2c-9c77-1f2a3b4c5d6e")
        );
    }

    /// Empty subject is not a routing key; tenantid wins.
    #[test]
    fn empty_subject_falls_back_to_tenantid() {
        let e = event(r#"{"subject":"","tenantid":"t-uuid","data":{}}"#);
        assert_eq!(extract_tenant(&e).as_deref(), Some("t-uuid"));
    }

    /// Oldest producers: only data.tenantId / data.tenant_id.
    #[test]
    fn data_fallbacks_still_work() {
        let e = event(r#"{"data":{"tenantId":"acme"}}"#);
        assert_eq!(extract_tenant(&e).as_deref(), Some("acme"));
        let e = event(r#"{"data":{"tenant_id":"globex"}}"#);
        assert_eq!(extract_tenant(&e).as_deref(), Some("globex"));
        let e = event(r#"{"data":{}}"#);
        assert_eq!(extract_tenant(&e), None);
    }
}

/// SPEC-W46 R17 buffered-commit discipline (same as payments-service):
/// auto-commit OFF, manual commit every `COMMIT_EVERY` processed messages
/// or `COMMIT_INTERVAL`, whichever first. Commit-after-process keeps
/// at-least-once semantics (a crash before the flush redelivers).
pub const COMMIT_EVERY: u32 = 50;
pub const COMMIT_INTERVAL: std::time::Duration = std::time::Duration::from_secs(1);

/// Tracks the highest processed offset per (topic, partition) and flushes
/// them as one commit — replaces per-message `commit_message` (which also
/// ran redundantly on top of `enable.auto.commit=true`).
pub(crate) struct OffsetBuffer {
    latest: std::collections::HashMap<(String, i32), i64>,
    pending: u32,
}

impl OffsetBuffer {
    pub(crate) fn new() -> Self {
        Self {
            latest: std::collections::HashMap::new(),
            pending: 0,
        }
    }

    /// Record a processed message. Returns true when the buffered batch has
    /// reached the flush threshold.
    pub(crate) fn record<M: rdkafka::Message>(&mut self, m: &M) -> bool {
        self.latest
            .insert((m.topic().to_string(), m.partition()), m.offset());
        self.pending += 1;
        self.pending >= COMMIT_EVERY
    }

    /// Flush buffered offsets (one commit for all partitions), async.
    pub(crate) fn flush(&mut self, consumer: &StreamConsumer) {
        if self.pending == 0 {
            return;
        }
        let mut tpl = rdkafka::TopicPartitionList::new();
        for ((topic, partition), offset) in self.latest.drain() {
            // Committed offset = next message to read.
            if let Err(e) =
                tpl.add_partition_offset(&topic, partition, rdkafka::Offset::Offset(offset + 1))
            {
                warn!(error = %e, topic = %topic, partition, "offset buffer entry rejected");
            }
        }
        self.pending = 0;
        if let Err(e) = consumer.commit(&tpl, CommitMode::Async) {
            warn!(error = %e, "buffered offset commit failed; redelivery on rebalance covers it");
        }
    }
}

/// Task entry point: runs the consumer and, on ANY return path, records
/// the exit so `/healthz` goes degraded (F15-07) instead of silently
/// losing the booking-event source.
pub async fn run(
    bus: Arc<EventBus>,
    brokers: String,
    group_id: String,
    topic: String,
    shutdown: watch::Receiver<bool>,
) {
    run_inner(bus, brokers, group_id, topic, shutdown).await;
    health::KAFKA_BOOKING.mark_exited();
}

async fn run_inner(
    bus: Arc<EventBus>,
    brokers: String,
    group_id: String,
    topic: String,
    mut shutdown: watch::Receiver<bool>,
) {
    // Initial beat: the consumer is alive from spawn, before broker connect.
    health::KAFKA_BOOKING.beat();
    let mut beat = tokio::time::interval(std::time::Duration::from_secs(
        health::BEAT_INTERVAL_SECS,
    ));
    beat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let consumer: StreamConsumer = match rdkafka::config::ClientConfig::new()
        .set("group.id", &group_id)
        .set("bootstrap.servers", &brokers)
        // R17: manual buffered commits (see OffsetBuffer) — auto-commit off.
        .set("enable.auto.commit", "false")
        .set("auto.offset.reset", "latest")
        .set("session.timeout.ms", "10000")
        .create()
    {
        Ok(c) => c,
        Err(e) => {
            error!(error = %e, "failed to create kafka consumer; booking fan-out disabled");
            return;
        }
    };
    if let Err(e) = consumer.subscribe(&[&topic]) {
        error!(error = %e, topic = %topic, "failed to subscribe");
        return;
    }
    info!(topic = %topic, brokers = %brokers, "booking events consumer started");

    let mut offsets = OffsetBuffer::new();
    let mut commit_tick = tokio::time::interval(COMMIT_INTERVAL);
    commit_tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);

    loop {
        tokio::select! {
            changed = shutdown.changed() => {
                if changed.is_ok() {
                    info!("booking events consumer shutting down");
                }
                offsets.flush(&consumer);
                break;
            }
            // F15-07: fixed-interval heartbeat independent of message flow,
            // so an idle topic does not read as a dead consumer.
            _ = beat.tick() => {
                health::KAFKA_BOOKING.beat();
            }
            // R17: time-based flush so a low-traffic topic never parks
            // uncommitted offsets longer than COMMIT_INTERVAL.
            _ = commit_tick.tick() => {
                offsets.flush(&consumer);
            }
            msg = consumer.recv() => {
                match msg {
                    Ok(m) => {
                        metrics::inc(&metrics::KAFKA_MESSAGES_TOTAL);
                        let payload = m.payload().unwrap_or_default();
                        match serde_json::from_slice::<RawCloudEvent>(payload) {
                            Ok(event) => {
                                match extract_tenant(&event) {
                                    Some(tenant) => {
                                        let raw = String::from_utf8_lossy(payload).into_owned();
                                        let n = bus.publish(&bus::booking_channel(&tenant), raw).await;
                                        debug!(tenant = %tenant, receivers = n, "fanned out booking event");
                                    }
                                    None => {
                                        debug!("booking event without tenant id; dropped");
                                    }
                                }
                            }
                            Err(e) => {
                                warn!(error = %e, "unparseable booking event; skipped");
                            }
                        }
                        // Commit AFTER processing (at-least-once), buffered.
                        if offsets.record(&m) {
                            offsets.flush(&consumer);
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
