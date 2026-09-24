//! Per-tenant broadcast bus with a drop-slow backpressure policy.
//!
//! Each tenant channel is a `tokio::sync::broadcast` ring buffer. Slow
//! consumers that fall more than `capacity` messages behind receive
//! `RecvError::Lagged` (their oldest messages are dropped) and the dropped
//! count is exported via `/metrics`.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use tokio::sync::{broadcast, RwLock};

use crate::metrics;

pub const BOOKING_CHANNEL_PREFIX: &str = "booking:";
pub const TRANSCRIPTS_CHANNEL_PREFIX: &str = "transcripts:";
pub const INTEL_CHANNEL_PREFIX: &str = "intel:";

/// SPEC-W46 R18 defaults: the channel map is bounded and idle entries are
/// evicted — previously one channel was retained per tenant EVER seen (slow
/// unbounded memory growth).
pub const DEFAULT_MAX_CHANNELS: usize = 10_000;
pub const DEFAULT_IDLE_EVICT: Duration = Duration::from_secs(600);

pub fn booking_channel(tenant: &str) -> String {
    format!("{BOOKING_CHANNEL_PREFIX}{tenant}")
}

pub fn transcripts_channel(tenant: &str) -> String {
    format!("{TRANSCRIPTS_CHANNEL_PREFIX}{tenant}")
}

pub fn intel_channel(tenant: &str) -> String {
    format!("{INTEL_CHANNEL_PREFIX}{tenant}")
}

fn now_millis() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}

struct ChannelEntry {
    tx: broadcast::Sender<Arc<str>>,
    /// Last subscribe/publish, epoch millis (atomic: publish only holds the
    /// map's read lock).
    last_active: AtomicU64,
}

pub struct EventBus {
    channels: RwLock<HashMap<String, ChannelEntry>>,
    capacity: usize,
    max_channels: usize,
    idle_evict: Duration,
}

impl EventBus {
    pub fn new(capacity: usize) -> Self {
        Self::with_limits(capacity, DEFAULT_MAX_CHANNELS, DEFAULT_IDLE_EVICT)
    }

    pub fn with_limits(capacity: usize, max_channels: usize, idle_evict: Duration) -> Self {
        Self {
            channels: RwLock::new(HashMap::new()),
            capacity,
            max_channels: max_channels.max(1),
            idle_evict,
        }
    }

    fn touch(entry: &ChannelEntry) {
        entry.last_active.store(now_millis(), Ordering::Relaxed);
    }

    /// Subscribe to a channel, creating it on demand. When the map is at its
    /// bound, idle zero-receiver channels are evicted first; if every channel
    /// is active the insert proceeds anyway (never drop live subscribers)
    /// with a warning.
    pub async fn subscribe(&self, channel: &str) -> broadcast::Receiver<Arc<str>> {
        {
            let channels = self.channels.read().await;
            if let Some(entry) = channels.get(channel) {
                Self::touch(entry);
                return entry.tx.subscribe();
            }
        }
        let mut channels = self.channels.write().await;
        if !channels.contains_key(channel) && channels.len() >= self.max_channels {
            let before = channels.len();
            Self::evict_idle_locked(&mut channels, self.idle_evict);
            if channels.len() >= self.max_channels {
                tracing::warn!(
                    max_channels = self.max_channels,
                    evicted = before - channels.len(),
                    "bus channel map at bound with all-active channels; allowing overflow"
                );
            }
        }
        let entry = channels
            .entry(channel.to_string())
            .or_insert_with(|| ChannelEntry {
                tx: broadcast::channel(self.capacity).0,
                last_active: AtomicU64::new(now_millis()),
            });
        Self::touch(entry);
        entry.tx.subscribe()
    }

    /// Publish an event to a channel. Returns the number of active receivers
    /// that accepted it. Publishing to a channel with no subscribers is a
    /// no-op counted as `no_subscriber`.
    pub async fn publish(&self, channel: &str, payload: String) -> usize {
        let tx = {
            let channels = self.channels.read().await;
            channels.get(channel).map(|entry| {
                Self::touch(entry);
                entry.tx.clone()
            })
        };
        match tx {
            Some(tx) => match tx.send(Arc::from(payload.as_str())) {
                Ok(receivers) => {
                    metrics::inc(&metrics::EVENTS_PUBLISHED);
                    receivers
                }
                Err(_) => {
                    metrics::inc(&metrics::EVENTS_NO_SUBSCRIBER);
                    0
                }
            },
            None => {
                metrics::inc(&metrics::EVENTS_NO_SUBSCRIBER);
                0
            }
        }
    }

    fn evict_idle_locked(
        channels: &mut HashMap<String, ChannelEntry>,
        idle_evict: Duration,
    ) -> usize {
        let cutoff = now_millis().saturating_sub(idle_evict.as_millis() as u64);
        let before = channels.len();
        // Only zero-receiver channels are evictable: a broadcast::Sender
        // with no receivers holds no buffered messages and its subscribers
        // (none) cannot be disturbed.
        channels.retain(|_, e| {
            e.tx.receiver_count() > 0 || e.last_active.load(Ordering::Relaxed) >= cutoff
        });
        before - channels.len()
    }

    /// R18: evict channels that have had no receivers and no activity for
    /// the configured idle TTL. Called by the periodic sweeper task
    /// (main.rs) and opportunistically at the map bound. Returns the number
    /// evicted.
    pub async fn evict_idle(&self) -> usize {
        let mut channels = self.channels.write().await;
        Self::evict_idle_locked(&mut channels, self.idle_evict)
    }

    /// Test/diagnostic: live channel-map size.
    #[cfg(test)]
    pub async fn channel_count(&self) -> usize {
        self.channels.read().await.len()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn publish_reaches_subscribers_per_tenant() {
        let bus = EventBus::new(16);
        let mut rx_a = bus.subscribe(&booking_channel("acme")).await;
        let mut rx_b = bus.subscribe(&booking_channel("globex")).await;
        bus.publish(&booking_channel("acme"), "{\"n\":1}".to_string()).await;
        let got = rx_a.recv().await.unwrap();
        assert_eq!(&*got, "{\"n\":1}");
        // globex subscriber has nothing.
        assert!(rx_b.try_recv().is_err());
    }

    #[tokio::test]
    async fn slow_consumers_are_dropped_with_lagged() {
        let bus = EventBus::new(2);
        let mut rx = bus.subscribe(&booking_channel("acme")).await;
        for i in 0..4 {
            bus.publish(&booking_channel("acme"), format!("m{i}")).await;
        }
        // Capacity 2, never received => oldest dropped.
        match rx.recv().await {
            Err(broadcast::error::RecvError::Lagged(n)) => assert!(n >= 1),
            other => panic!("expected Lagged, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn publish_without_subscribers_is_noop() {
        let bus = EventBus::new(4);
        let receivers = bus.publish(&booking_channel("nobody"), "x".to_string()).await;
        assert_eq!(receivers, 0);
    }

    /// R18: zero-receiver channels idle past the TTL are evicted; channels
    /// with live receivers (or recent activity) survive.
    #[tokio::test]
    async fn idle_zero_receiver_channels_are_evicted() {
        let bus = EventBus::with_limits(4, 100, Duration::from_millis(20));
        let live = bus.subscribe(&booking_channel("live")).await;
        let _dropped = bus.subscribe(&booking_channel("stale")).await;
        drop(_dropped);
        // Re-touch the live channel so only the stale one ages out.
        tokio::time::sleep(Duration::from_millis(40)).await;
        bus.publish(&booking_channel("live"), "x".to_string()).await;
        let evicted = bus.evict_idle().await;
        assert_eq!(evicted, 1, "only the idle zero-receiver channel evicts");
        assert_eq!(bus.channel_count().await, 1);
        // The surviving channel is the live one.
        let n = bus.publish(&booking_channel("live"), "y".to_string()).await;
        assert_eq!(n, 1);
        drop(live);
    }

    /// R18: at the map bound, subscribe evicts idle channels to make room;
    /// active channels are never dropped (overflow allowed with a warning).
    #[tokio::test]
    async fn map_bound_evicts_idle_before_growing() {
        let bus = EventBus::with_limits(4, 2, Duration::from_millis(20));
        let rx = bus.subscribe(&booking_channel("active")).await;
        let gone = bus.subscribe(&booking_channel("gone")).await;
        drop(gone);
        tokio::time::sleep(Duration::from_millis(40)).await;
        // Map full (2/2): subscribing a third evicts the idle one first.
        let _rx2 = bus.subscribe(&booking_channel("new")).await;
        assert_eq!(bus.channel_count().await, 2);
        drop(rx);
    }
}
