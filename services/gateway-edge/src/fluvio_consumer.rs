//! Fluvio live-tail consumer for `opendesk.transcripts-raw` (SPEC §5),
//! fanning records out to `transcripts:{slug}` channels (`/ws/transcripts`).
//!
//! The real consumer is compiled with `--features fluvio-live` (dependency
//! `fluvio`). The default build ships a stub that is FAIL-CLOSED: `main`
//! refuses to start unless the operator explicitly opts in with
//! `GATEWAY_EDGE_ALLOW_SIM=true` — the service never silently simulates
//! transcript consumption. The Kafka consumer (`opendesk.booking.events`)
//! remains the primary source (SPEC §5: "Fluvio mirror + Kafka fallback").

use std::sync::Arc;

use tokio::sync::watch;

use crate::bus::EventBus;

/// Transcript record shape (SPEC §4/§5; redacted by the pii-redact smart
/// module upstream of any sink, raw here on the internal topic).
#[derive(Debug, serde::Deserialize)]
struct TranscriptRecord {
    #[serde(rename = "tenantId", default)]
    tenant_id: Option<String>,
}

#[cfg(feature = "fluvio-live")]
mod imp {
    use super::*;
    use futures_util::StreamExt;
    use tracing::{error, info, warn};

    /// Minimal subset of the `fluvio` crate API used here:
    ///   - `fluvio::config::FluvioConfig::new(endpoint)`
    ///     + `Fluvio::connect_with_config(&cfg)`
    ///   - `fluvio.partition_consumer(topic, partition)`
    ///   - `consumer.stream(fluvio::Offset::end())` -> stream of `Record`
    ///     with `record.value()` bytes.
    /// If the pinned crate version drifts, this module is the single
    /// integration point to adjust.
    pub async fn run_live(
        bus: Arc<EventBus>,
        endpoint: String,
        topic: String,
        partitions: i32,
        shutdown: watch::Receiver<bool>,
    ) {
        let config = fluvio::config::FluvioConfig::new(endpoint.clone());
        let fluvio = match fluvio::Fluvio::connect_with_config(&config).await {
            Ok(f) => f,
            Err(e) => {
                error!(error = %e, endpoint = %endpoint, "fluvio connect failed; transcript tail disabled");
                return;
            }
        };
        info!(topic = %topic, endpoint = %endpoint, partitions, "fluvio transcript tail started");

        let mut handles = Vec::new();
        for partition in 0..partitions {
            let bus = bus.clone();
            let topic = topic.clone();
            let mut shutdown = shutdown.clone();
            let consumer = match fluvio.partition_consumer(topic.clone(), partition).await {
                Ok(c) => c,
                Err(e) => {
                    warn!(error = %e, partition, "fluvio partition consumer failed; skipping");
                    continue;
                }
            };
            handles.push(tokio::spawn(async move {
                let mut stream = match consumer.stream(fluvio::Offset::end()).await {
                    Ok(s) => s,
                    Err(e) => {
                        warn!(error = %e, partition, "fluvio stream failed");
                        return;
                    }
                };
                loop {
                    tokio::select! {
                        changed = shutdown.changed() => {
                            if changed.is_ok() {
                                info!(partition, "fluvio tail shutting down");
                            }
                            break;
                        }
                        item = stream.next() => {
                            match item {
                                Some(Ok(record)) => {
                                    crate::metrics::inc(&crate::metrics::FLUVIO_RECORDS_TOTAL);
                                    let payload = record.value().to_vec();
                                    forward(&bus, &topic, &payload).await;
                                }
                                Some(Err(e)) => {
                                    warn!(error = %e, partition, "fluvio record error");
                                    tokio::time::sleep(std::time::Duration::from_millis(500)).await;
                                }
                                None => break,
                            }
                        }
                    }
                }
            }));
        }
        for h in handles {
            let _ = h.await;
        }
    }
}

/// Parse the tenant out of a transcript record and fan the raw payload out.
async fn forward(bus: &Arc<EventBus>, topic: &str, payload: &[u8]) {
    match serde_json::from_slice::<TranscriptRecord>(payload) {
        Ok(rec) => match rec.tenant_id {
            Some(tenant) if !tenant.is_empty() => {
                let raw = String::from_utf8_lossy(payload).into_owned();
                bus.publish(&crate::bus::transcripts_channel(&tenant), raw).await;
            }
            _ => {
                tracing::debug!(topic, "transcript record without tenantId; dropped");
            }
        },
        Err(e) => {
            tracing::warn!(error = %e, topic, "unparseable transcript record; skipped");
        }
    }
}

#[cfg(feature = "fluvio-live")]
pub async fn run(
    bus: Arc<EventBus>,
    endpoint: String,
    topic: String,
    partitions: i32,
    shutdown: watch::Receiver<bool>,
) {
    imp::run_live(bus, endpoint, topic, partitions, shutdown).await
}

#[cfg(not(feature = "fluvio-live"))]
pub async fn run(
    _bus: Arc<EventBus>,
    _endpoint: String,
    topic: String,
    _partitions: i32,
    mut shutdown: watch::Receiver<bool>,
) {
    // Reachable only when the operator explicitly opted into the no-op stub
    // (GATEWAY_EDGE_ALLOW_SIM=true); main refuses to start otherwise, so this
    // path never silently simulates consumption.
    tracing::warn!(
        topic = %topic,
        "GATEWAY_EDGE_ALLOW_SIM=true: running WITHOUT the fluvio transcript tail \
         (explicit opt-in no-op stub; rebuild with --features fluvio-live for \
         the real consumer). Kafka booking events remain the primary source"
    );
    // Stay alive (so supervision restarts don't flap) until shutdown.
    let _ = shutdown.changed().await;
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn forward_routes_by_tenant_id() {
        let bus = Arc::new(EventBus::new(8));
        let mut rx = bus
            .subscribe(&crate::bus::transcripts_channel("acme"))
            .await;
        let payload = br#"{"conversationId":"c1","tenantId":"acme","role":"user","text":"hi","ts":"2024-01-01T00:00:00Z"}"#;
        forward(&bus, "opendesk.transcripts-raw", payload).await;
        let got = rx.recv().await.unwrap();
        assert!(got.contains("\"tenantId\":\"acme\""));
    }

    #[tokio::test]
    async fn forward_drops_records_without_tenant() {
        let bus = Arc::new(EventBus::new(8));
        // No subscribers and no tenantId: must not panic.
        let payload = br#"{"conversationId":"c1","role":"user","text":"hi","ts":"x"}"#;
        forward(&bus, "opendesk.transcripts-raw", payload).await;
    }
}
