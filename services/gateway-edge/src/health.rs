//! Consumer liveness tracking for `/healthz` (F15-07).
//!
//! Each background consumer task (Kafka booking events, Kafka enriched
//! turns, Fluvio transcript tail) owns a [`ConsumerHealth`] heartbeat: the
//! task beats on a fixed interval while its loop is alive and marks itself
//! exited when it returns for ANY reason (connection failure, subscribe
//! failure, clean shutdown). `/healthz` reports `503 {"status":"degraded"}`
//! when any tracked consumer has not beaten within [`STALE_AFTER_SECS`] or
//! has exited, so a wedged/dead source is fail-visible instead of the
//! static "ok" that hid it before.

use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::time::{SystemTime, UNIX_EPOCH};

/// A consumer heartbeat older than this is considered stale.
pub const STALE_AFTER_SECS: u64 = 30;

/// How often a live consumer task beats while its loop is running.
pub const BEAT_INTERVAL_SECS: u64 = 5;

/// Liveness state of one consumer task.
pub struct ConsumerHealth {
    name: &'static str,
    /// Unix-seconds of the last heartbeat (0 = never beaten).
    last_beat: AtomicU64,
    /// Set once the consumer task has returned (any reason).
    exited: AtomicBool,
}

fn now_secs() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

impl ConsumerHealth {
    pub const fn new(name: &'static str) -> Self {
        Self {
            name,
            last_beat: AtomicU64::new(0),
            exited: AtomicBool::new(false),
        }
    }

    /// Record a heartbeat at the current time. Consumers call this on a
    /// fixed interval (see `BEAT_INTERVAL_SECS`) independent of message
    /// flow, so an idle topic does NOT look dead.
    pub fn beat(&self) {
        self.last_beat.store(now_secs(), Ordering::Relaxed);
    }

    /// Mark the consumer task as exited (terminal for this process run).
    pub fn mark_exited(&self) {
        self.exited.store(true, Ordering::Relaxed);
    }

    /// Per-consumer status snapshot. `age_secs` saturates at the time since
    /// boot when the consumer has never beaten.
    pub fn status(&self) -> ConsumerStatus {
        let exited = self.exited.load(Ordering::Relaxed);
        let last = self.last_beat.load(Ordering::Relaxed);
        let now = now_secs();
        let age_secs = if last == 0 { now } else { now.saturating_sub(last) };
        ConsumerStatus {
            name: self.name,
            healthy: !exited && age_secs <= STALE_AFTER_SECS,
            exited,
            age_secs,
        }
    }

    /// Test-only: backdate the heartbeat to simulate a stale consumer.
    #[cfg(test)]
    pub(crate) fn set_last_beat_for_test(&self, secs: u64) {
        self.last_beat.store(secs, Ordering::Relaxed);
    }
}

pub struct ConsumerStatus {
    pub name: &'static str,
    pub healthy: bool,
    pub exited: bool,
    pub age_secs: u64,
}

/// Kafka `opendesk.booking.events` consumer (kafka_consumer.rs).
pub static KAFKA_BOOKING: ConsumerHealth = ConsumerHealth::new("kafka-booking-events");
/// Kafka `opendesk.conversation.enriched` consumer (enriched_consumer.rs).
pub static KAFKA_ENRICHED: ConsumerHealth = ConsumerHealth::new("kafka-enriched-turns");
/// Fluvio `opendesk.transcripts-raw` live tail (fluvio_consumer.rs).
pub static FLUVIO_TRANSCRIPTS: ConsumerHealth = ConsumerHealth::new("fluvio-transcripts");

pub const ALL: [&ConsumerHealth; 3] = [&KAFKA_BOOKING, &KAFKA_ENRICHED, &FLUVIO_TRANSCRIPTS];

/// Snapshot every tracked consumer. `all_healthy` is the /healthz verdict:
/// false when any consumer exited or its heartbeat is stale > 30s.
pub fn report() -> (bool, Vec<ConsumerStatus>) {
    let statuses: Vec<ConsumerStatus> = ALL.iter().map(|c| c.status()).collect();
    let all_healthy = statuses.iter().all(|s| s.healthy);
    (all_healthy, statuses)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fresh_heartbeat_is_healthy() {
        let h = ConsumerHealth::new("t-fresh");
        h.beat();
        let s = h.status();
        assert!(s.healthy, "fresh beat must be healthy");
        assert!(!s.exited && s.age_secs <= STALE_AFTER_SECS);
    }

    #[test]
    fn never_beaten_is_stale() {
        let h = ConsumerHealth::new("t-never");
        let s = h.status();
        assert!(!s.healthy, "never-beaten consumer must be unhealthy");
    }

    #[test]
    fn stale_heartbeat_is_unhealthy() {
        let h = ConsumerHealth::new("t-stale");
        h.beat();
        let now = now_secs();
        // Boundary: exactly STALE_AFTER_SECS old is still healthy...
        h.set_last_beat_for_test(now - STALE_AFTER_SECS);
        assert!(h.status().healthy, "exactly 30s old must still be healthy");
        // ...31s old is stale.
        h.set_last_beat_for_test(now - STALE_AFTER_SECS - 1);
        assert!(!h.status().healthy, "31s old must be stale");
    }

    #[test]
    fn exited_is_unhealthy_even_with_fresh_beat() {
        let h = ConsumerHealth::new("t-exited");
        h.beat();
        h.mark_exited();
        let s = h.status();
        assert!(!s.healthy && s.exited);
    }

    #[test]
    fn report_aggregates_all_consumers() {
        // Aggregate must be the AND of the per-consumer verdicts (reads
        // only; robust against other tests beating the shared statics).
        let (healthy, statuses) = report();
        assert_eq!(statuses.len(), 3);
        assert_eq!(healthy, statuses.iter().all(|s| s.healthy));
    }
}
