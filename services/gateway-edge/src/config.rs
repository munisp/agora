//! Environment-driven configuration (see README.md env table).

#[derive(Debug, Clone)]
pub struct Config {
    /// HTTP listen port (SPEC §3: 7005).
    pub port: u16,
    pub kafka_brokers: String,
    pub kafka_group_id: String,
    pub booking_events_topic: String,
    /// Enriched conversation turns (sentiment/intent/entities), fanned out
    /// to `/ws/intel` (SPEC-W3 §4).
    pub enriched_topic: String,
    /// Keycloak JWKS endpoint (SPEC §8).
    pub jwks_url: String,
    pub issuer: String,
    pub audience: Option<String>,
    /// Dev escape hatch: skip JWT validation entirely (never use in prod).
    pub auth_disabled: bool,
    pub jwks_cache_ttl_secs: u64,
    /// Per-tenant broadcast channel capacity; slow consumers are dropped
    /// (drop-slow policy) and counted in metrics.
    pub ws_channel_capacity: usize,
    pub fluvio_endpoint: String,
    pub fluvio_transcripts_topic: String,
    pub fluvio_partitions: i32,
    /// Explicit opt-in to run the no-op Fluvio stub (builds without the
    /// `fluvio-live` feature). Default OFF: the service refuses to start
    /// rather than silently simulate transcript consumption.
    pub fluvio_stub_allowed: bool,
}

fn env_or(key: &str, default: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| default.to_string())
}

fn env_parse<T: std::str::FromStr>(key: &str, default: T) -> T {
    std::env::var(key)
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(default)
}

impl Config {
    pub fn from_env() -> Self {
        Self {
            port: env_parse("PORT", 7005),
            kafka_brokers: env_or("KAFKA_BROKERS", "kafka:9092"),
            kafka_group_id: env_or("KAFKA_GROUP_ID", "gateway-edge"),
            booking_events_topic: env_or("BOOKING_EVENTS_TOPIC", "opendesk.booking.events"),
            enriched_topic: env_or("ENRICHED_TOPIC", "opendesk.conversation.enriched"),
            jwks_url: env_or(
                "KEYCLOAK_JWKS_URL",
                "http://keycloak:8080/realms/opendesk/protocol/openid-connect/certs",
            ),
            issuer: env_or("KEYCLOAK_ISSUER", "http://keycloak:8080/realms/opendesk"),
            audience: std::env::var("KEYCLOAK_AUDIENCE").ok().filter(|s| !s.is_empty()),
            auth_disabled: env_parse("EDGE_AUTH_DISABLED", false),
            jwks_cache_ttl_secs: env_parse("JWKS_CACHE_TTL_SECS", 300),
            ws_channel_capacity: env_parse("WS_CHANNEL_CAPACITY", 256),
            fluvio_endpoint: env_or("FLUVIO_ENDPOINT", "fluvio:9003"),
            fluvio_transcripts_topic: env_or(
                "FLUVIO_TRANSCRIPTS_TOPIC",
                "opendesk.transcripts-raw",
            ),
            fluvio_partitions: env_parse("FLUVIO_PARTITIONS", 6),
            fluvio_stub_allowed: env_parse("GATEWAY_EDGE_ALLOW_SIM", false),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Regression for RS-003: `enriched_topic` must be a real, parsed field
    /// of `Config` (the default build previously failed with E0063: missing
    /// field in initializer) and the Fluvio stub must default to fail-closed.
    #[test]
    fn from_env_parses_enriched_topic_and_stub_opt_in() {
        std::env::remove_var("ENRICHED_TOPIC");
        std::env::remove_var("GATEWAY_EDGE_ALLOW_SIM");
        let cfg = Config::from_env();
        assert_eq!(cfg.enriched_topic, "opendesk.conversation.enriched");
        assert!(
            !cfg.fluvio_stub_allowed,
            "fluvio stub must be opt-in, default OFF"
        );

        std::env::set_var("ENRICHED_TOPIC", "custom.enriched");
        std::env::set_var("GATEWAY_EDGE_ALLOW_SIM", "true");
        let cfg = Config::from_env();
        assert_eq!(cfg.enriched_topic, "custom.enriched");
        assert!(cfg.fluvio_stub_allowed);

        std::env::remove_var("ENRICHED_TOPIC");
        std::env::remove_var("GATEWAY_EDGE_ALLOW_SIM");
    }
}
