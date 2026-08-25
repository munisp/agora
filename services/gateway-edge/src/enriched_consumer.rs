//! Enriched-turns source: Kafka `opendesk.conversation.enriched`
//! (CloudEvents JSON carrying sentiment/intent/entities, SPEC-W3 §4).
//! Events are fanned out to the tenant's `intel:{slug}` channel (`/ws/intel`).
//!
//! Mirrors kafka_consumer.rs exactly: same rdkafka setup, same tenant
//! extraction, same commit/drop semantics — only the topic, channel and log
//! wording differ.

use std::sync::Arc;

use rdkafka::consumer::{CommitMode, Consumer, StreamConsumer};
use rdkafka::Message as _;
use tokio::sync::watch;
use tracing::{debug, error, info, warn};

use crate::bus;
use crate::bus::EventBus;
use crate::health;
use crate::kafka_consumer::{extract_tenant, RawCloudEvent};
use crate::metrics;

/// Task entry point: runs the consumer and, on ANY return path, records
/// the exit so `/healthz` goes degraded (F15-07).
pub async fn run(
    bus: Arc<EventBus>,
    brokers: String,
    group_id: String,
    topic: String,
    shutdown: watch::Receiver<bool>,
) {
    run_inner(bus, brokers, group_id, topic, shutdown).await;
    health::KAFKA_ENRICHED.mark_exited();
}

async fn run_inner(
    bus: Arc<EventBus>,
    brokers: String,
    group_id: String,
    topic: String,
    mut shutdown: watch::Receiver<bool>,
) {
    health::KAFKA_ENRICHED.beat();
    let mut beat = tokio::time::interval(std::time::Duration::from_secs(
        health::BEAT_INTERVAL_SECS,
    ));
    beat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let consumer: StreamConsumer = match rdkafka::config::ClientConfig::new()
        .set("group.id", &group_id)
        .set("bootstrap.servers", &brokers)
        .set("enable.auto.commit", "true")
        .set("auto.offset.reset", "latest")
        .set("session.timeout.ms", "10000")
        .create()
    {
        Ok(c) => c,
        Err(e) => {
            error!(error = %e, "failed to create kafka consumer; intel fan-out disabled");
            return;
        }
    };
    if let Err(e) = consumer.subscribe(&[&topic]) {
        error!(error = %e, topic = %topic, "failed to subscribe");
        return;
    }
    info!(topic = %topic, brokers = %brokers, "enriched turns consumer started");

    loop {
        tokio::select! {
            changed = shutdown.changed() => {
                if changed.is_ok() {
                    info!("enriched turns consumer shutting down");
                }
                break;
            }
            // F15-07: fixed-interval heartbeat independent of message flow.
            _ = beat.tick() => {
                health::KAFKA_ENRICHED.beat();
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
                                        let n = bus.publish(&bus::intel_channel(&tenant), raw).await;
                                        debug!(tenant = %tenant, receivers = n, "fanned out enriched turn");
                                    }
                                    None => {
                                        debug!("enriched turn without tenant id; dropped");
                                    }
                                }
                            }
                            Err(e) => {
                                warn!(error = %e, "unparseable enriched turn; skipped");
                            }
                        }
                        let _ = consumer.commit_message(&m, CommitMode::Async);
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
