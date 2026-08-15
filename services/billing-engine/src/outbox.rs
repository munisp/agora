//! Durable billing event outbox (RS-001).
//!
//! `InvoicePaid` (and any future billing event) is INSERTed into
//! `billing_outbox` in the SAME database transaction as the paid commit, then
//! published to Kafka by the relay below with bounded exponential backoff.
//! An event is therefore never silently droppable: until the broker accepts
//! the record the row stays pending, every failure bumps the
//! `billing_events_failed_total` counter (exposed on /metrics) and is logged
//! — error-level once retries stack up. The paid commit itself is never
//! rolled back by a publication failure.
//!
//! Provisioning: Kafka auto-create is OFF, so the target topic (default
//! `opendesk.billing.events`, env BILLING_EVENTS_TOPIC) must exist — see the
//! wave HANDOFF for the infra coder.

use std::sync::atomic::Ordering;
use std::time::Duration;

use rdkafka::producer::FutureRecord;
use sqlx::{Postgres, Row, Transaction};
use tokio::sync::watch;
use tracing::{error, info, warn};
use uuid::Uuid;

use crate::AppState;

/// Relay poll cadence (the `outbox_notify` signal triggers immediate flushes
/// right after the owning transaction commits).
const POLL_INTERVAL: Duration = Duration::from_secs(2);
/// Kafka produce timeout per record.
const SEND_TIMEOUT: Duration = Duration::from_secs(10);
/// Backoff: min(2^attempts * 250ms, 60s).
const BACKOFF_BASE_MS: u64 = 250;
const BACKOFF_MAX_S: u64 = 60;
const BATCH_LIMIT: i64 = 50;

/// Exponential backoff with a 60s ceiling, by previous attempt count.
pub fn retry_backoff(attempts: i32) -> Duration {
    let shift = attempts.clamp(0, 20) as u32;
    let ms = BACKOFF_BASE_MS.saturating_mul(1u64 << shift);
    Duration::from_millis(ms).min(Duration::from_secs(BACKOFF_MAX_S))
}

/// Enqueue an event inside the caller's open transaction — the row commits
/// atomically with the state change it describes (RS-001 core guarantee).
pub async fn enqueue(
    tx: &mut Transaction<'_, Postgres>,
    topic: &str,
    key: &str,
    payload: &serde_json::Value,
) -> Result<(), sqlx::Error> {
    sqlx::query(
        "INSERT INTO billing_outbox (id, topic, event_key, payload) VALUES ($1, $2, $3, $4)",
    )
    .bind(Uuid::new_v4())
    .bind(topic)
    .bind(key)
    .bind(payload)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

/// Publish every due pending row once. Best-effort per row: failures are
/// recorded on the row (attempts/next_attempt_at/last_error) and counted.
async fn relay_once(state: &AppState) {
    let rows = match sqlx::query(
        "SELECT id, topic, event_key, payload, attempts FROM billing_outbox \
         WHERE published_at IS NULL AND next_attempt_at <= now() \
         ORDER BY created_at LIMIT $1",
    )
    .bind(BATCH_LIMIT)
    .fetch_all(&state.internal_pool)
    .await
    {
        Ok(r) => r,
        Err(e) => {
            error!(error = %e, "outbox relay: pending scan failed");
            return;
        }
    };
    if rows.is_empty() {
        return;
    }
    let Some(producer) = &state.producer else {
        warn!(
            pending = rows.len(),
            "outbox relay: no kafka producer (boot-time creation failed); \
             events stay durable in billing_outbox until a restart with a working broker"
        );
        return;
    };

    for row in rows {
        let id: Uuid = row.try_get("id").unwrap_or_default();
        let topic: String = row.try_get("topic").unwrap_or_default();
        let key: String = row.try_get("event_key").unwrap_or_default();
        let payload: serde_json::Value = row.try_get("payload").unwrap_or_default();
        let attempts: i32 = row.try_get("attempts").unwrap_or_default();
        let bytes = match serde_json::to_vec(&payload) {
            Ok(b) => b,
            Err(e) => {
                // Poison row: cannot ever be published; mark it published to
                // stop the retry loop but keep the row for forensics.
                error!(error = %e, outbox_id = %id, "outbox row payload unserializable; quarantining");
                let _ = sqlx::query(
                    "UPDATE billing_outbox SET published_at = now(), \
                     last_error = 'payload unserializable' WHERE id = $1",
                )
                .bind(id)
                .execute(&state.internal_pool)
                .await;
                continue;
            }
        };
        let record = FutureRecord::to(topic.as_str())
            .key(key.as_str())
            .payload(bytes.as_slice());
        match producer.send(record, SEND_TIMEOUT).await {
            Ok(_) => {
                if let Err(e) = sqlx::query(
                    "UPDATE billing_outbox SET published_at = now(), last_error = NULL \
                     WHERE id = $1",
                )
                .bind(id)
                .execute(&state.internal_pool)
                .await
                {
                    // The broker HAS the event; a failed mark only means one
                    // duplicate republish later (consumers are idempotent).
                    warn!(error = %e, outbox_id = %id, "outbox mark-published failed (at-least-once)");
                }
                state.events_published.fetch_add(1, Ordering::Relaxed);
            }
            Err((e, _msg)) => {
                let backoff = retry_backoff(attempts);
                state.events_failed.fetch_add(1, Ordering::Relaxed);
                let _ = sqlx::query(
                    "UPDATE billing_outbox SET attempts = attempts + 1, last_error = $2, \
                     next_attempt_at = now() + ($3 || ' milliseconds')::interval \
                     WHERE id = $1",
                )
                .bind(id)
                .bind(e.to_string())
                .bind(backoff.as_millis().to_string())
                .execute(&state.internal_pool)
                .await;
                if attempts + 1 >= 3 {
                    error!(
                        error = %e,
                        outbox_id = %id,
                        topic = %topic,
                        attempts = attempts + 1,
                        "CRITICAL: billing event still unpublished (topic provisioned? \
                         auto-create is OFF); row stays durable in billing_outbox"
                    );
                } else {
                    warn!(
                        error = %e,
                        outbox_id = %id,
                        topic = %topic,
                        attempts = attempts + 1,
                        backoff_ms = backoff.as_millis() as u64,
                        "billing event publish failed; will retry from outbox"
                    );
                }
            }
        }
    }
}

/// Relay loop: flush on boot, on every `outbox_notify` signal (fired right
/// after a committing transaction enqueues), and on the poll cadence.
pub async fn run(state: AppState, mut shutdown: watch::Receiver<bool>) {
    info!(
        topic = %state.config.billing_events_topic,
        "billing outbox relay started"
    );
    relay_once(&state).await;
    let mut ticker = tokio::time::interval(POLL_INTERVAL);
    ticker.tick().await;
    loop {
        tokio::select! {
            changed = shutdown.changed() => {
                if changed.is_ok() {
                    info!("billing outbox relay shutting down");
                }
                break;
            }
            _ = ticker.tick() => relay_once(&state).await,
            _ = state.outbox_notify.notified() => relay_once(&state).await,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn backoff_grows_exponentially_and_caps() {
        assert_eq!(retry_backoff(0), Duration::from_millis(250));
        assert_eq!(retry_backoff(1), Duration::from_millis(500));
        assert_eq!(retry_backoff(2), Duration::from_millis(1000));
        assert_eq!(retry_backoff(3), Duration::from_millis(2000));
        // Far along the curve the 60s ceiling binds.
        assert_eq!(retry_backoff(20), Duration::from_secs(60));
        // Defensive: negative attempt counts behave like the first attempt.
        assert_eq!(retry_backoff(-1), Duration::from_millis(250));
    }
}
