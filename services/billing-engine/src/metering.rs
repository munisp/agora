//! Metering ingestion (SPEC-W7 B1): idempotent persistence of
//! `opendesk.usage.events` CloudEvents into `usage_records`.
//!
//! The source is at-least-once (booking-service outbox), so idempotency is
//! anchored on `processed_events(event_id)`: the event id is claimed first
//! (INSERT ... ON CONFLICT DO NOTHING) and the usage row references it, so a
//! redelivered event is skipped and a partial crash cannot double-count.

use sqlx::PgPool;
use uuid::Uuid;

use crate::models::{RawCloudEvent, UsageRecordData};
use crate::tenant;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UsageOutcome {
    /// First delivery: usage row written.
    Recorded,
    /// Redelivery of an already-processed event id: skipped.
    Duplicate,
}

/// Persist one usage event. Errors are returned as strings so the consumer
/// can decide between retry (transient DB failure) and drop (poison payload).
pub async fn record_usage(pool: &PgPool, event: &RawCloudEvent) -> Result<UsageOutcome, String> {
    if event.id.trim().is_empty() {
        return Err("cloudevent missing id".to_string());
    }
    let data: UsageRecordData = serde_json::from_value(event.data.clone())
        .map_err(|e| format!("bad usage record payload: {e}"))?;
    if data.value < 0 {
        return Err(format!("negative usage value {}", data.value));
    }
    record_usage_data(pool, &event.id, &data).await
}

async fn record_usage_data(
    pool: &PgPool,
    event_id: &str,
    data: &UsageRecordData,
) -> Result<UsageOutcome, String> {
    // GF6: claim + insert run in ONE transaction with the RLS tenant GUC set
    // from the event payload (0002 policies are fail-closed without it). This
    // also makes the event-id claim atomic with the usage row: a crash
    // mid-write now rolls the claim back instead of stranding it.
    let mut tx = tenant::begin_tenant_tx(pool, data.tenant_id)
        .await
        .map_err(|e| e.to_string())?;

    // Claim the event id first; a concurrent/duplicate delivery loses the race.
    let claimed = sqlx::query(
        "INSERT INTO processed_events (event_id) VALUES ($1) ON CONFLICT (event_id) DO NOTHING",
    )
    .bind(event_id)
    .execute(&mut *tx)
    .await
    .map_err(|e| e.to_string())?;
    if claimed.rows_affected() == 0 {
        // Rolling back releases the transaction without touching anything.
        let _ = tx.rollback().await;
        return Ok(UsageOutcome::Duplicate);
    }

    let meta = if data.meta.is_null() {
        serde_json::json!({})
    } else {
        data.meta.clone()
    };
    sqlx::query(
        "INSERT INTO usage_records (tenant_id, metric, value, ts, meta, event_id) \
         VALUES ($1, $2, $3, $4, $5, $6)",
    )
    .bind(data.tenant_id)
    .bind(&data.metric)
    .bind(data.value)
    .bind(data.ts)
    .bind(&meta)
    .bind(event_id)
    .execute(&mut *tx)
    .await
    .map_err(|e| e.to_string())?;
    tx.commit().await.map_err(|e| e.to_string())?;
    Ok(UsageOutcome::Recorded)
}

/// Exposed for tests and potential admin tooling: the tenant a usage event
/// belongs to (used for structured logging).
#[cfg_attr(not(test), allow(dead_code))]
pub fn event_tenant(event: &RawCloudEvent) -> Option<Uuid> {
    serde_json::from_value::<UsageRecordData>(event.data.clone())
        .ok()
        .map(|d| d.tenant_id)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn event(id: &str, data: serde_json::Value) -> RawCloudEvent {
        RawCloudEvent {
            id: id.to_string(),
            type_: "com.opendesk.usage.UsageRecord".to_string(),
            data,
        }
    }

    #[test]
    fn usage_record_payload_decodes_booking_service_shape() {
        // Exact shape emitted by bookingops.MarshalUsageRecord.
        let e = event(
            "evt-1",
            serde_json::json!({
                "tenant_id": "9b0b0d52-1c8b-4d3f-9e2a-6f6a2b7c1d20",
                "metric": "booking",
                "value": 1,
                "ts": "2026-03-14T10:15:00Z",
                "meta": {"booking_id": "x", "price_cents": 5000}
            }),
        );
        let t = event_tenant(&e).unwrap();
        assert_eq!(
            t,
            Uuid::parse_str("9b0b0d52-1c8b-4d3f-9e2a-6f6a2b7c1d20").unwrap()
        );
    }

    #[test]
    fn usage_record_payload_rejects_malformed_data() {
        let e = event("evt-2", serde_json::json!({"metric": "booking"}));
        assert!(event_tenant(&e).is_none());
    }
}
