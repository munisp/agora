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
    /// Dev mode (`OPENDESK_DEV=1|true`): relaxes production startup
    /// invariants (KEYCLOAK_AUDIENCE — SEC#12). OFF by default and never
    /// set in compose.
    pub dev_mode: bool,
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

/// Boolean env flag accepting `1`/`true` (case-insensitive); absent or any
/// other value is false.
fn env_flag(key: &str) -> bool {
    std::env::var(key)
        .map(|v| v == "1" || v.eq_ignore_ascii_case("true"))
        .unwrap_or(false)
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
            auth_disabled: env_flag("EDGE_AUTH_DISABLED"),
            dev_mode: env_flag("OPENDESK_DEV"),
            jwks_cache_ttl_secs: env_parse("JWKS_CACHE_TTL_SECS", 300),
            ws_channel_capacity: env_parse("WS_CHANNEL_CAPACITY", 256),
            fluvio_endpoint: env_or("FLUVIO_ENDPOINT", "fluvio:9003"),
            fluvio_transcripts_topic: env_or(
                "FLUVIO_TRANSCRIPTS_TOPIC",
                "opendesk.transcripts-raw",
            ),
            fluvio_partitions: env_parse("FLUVIO_PARTITIONS", 6),
            fluvio_stub_allowed: env_flag("GATEWAY_EDGE_ALLOW_SIM"),
        }
    }

    /// Startup invariants (fail-closed). SEC#12: KEYCLOAK_AUDIENCE is
    /// REQUIRED unless this is an explicit dev run (`OPENDESK_DEV=1`) or
    /// auth is entirely disabled (`EDGE_AUTH_DISABLED=true`, itself a
    /// dev-only escape): without an audience, ANY RS256 token signed by the
    /// realm for ANY client would be accepted (aud validation disabled),
    /// which is never acceptable outside dev.
    pub fn validate(&self) -> Result<(), String> {
        if self.audience.is_none() && !self.dev_mode && !self.auth_disabled {
            return Err(
                "KEYCLOAK_AUDIENCE is not set: refusing to start without audience                  validation in non-dev mode (set KEYCLOAK_AUDIENCE, or opt into                  dev posture with OPENDESK_DEV=1)"
                    .to_string(),
            );
        }
        Ok(())
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

    /// SEC#12: audience is mandatory outside explicit dev posture.
    #[test]
    fn audience_required_unless_dev() {
        for k in ["KEYCLOAK_AUDIENCE", "OPENDESK_DEV", "EDGE_AUTH_DISABLED"] {
            std::env::remove_var(k);
        }
        let cfg = Config::from_env();
        assert!(cfg.audience.is_none() && !cfg.dev_mode && !cfg.auth_disabled);
        let err = cfg.validate().expect_err("non-dev without audience must fail");
        assert!(err.contains("KEYCLOAK_AUDIENCE"), "{err}");

        // Audience set -> ok.
        std::env::set_var("KEYCLOAK_AUDIENCE", "opendesk");
        assert!(Config::from_env().validate().is_ok());
        std::env::remove_var("KEYCLOAK_AUDIENCE");

        // OPENDESK_DEV=1 -> ok (explicit dev opt-in; accepts `1` and `true`).
        std::env::set_var("OPENDESK_DEV", "1");
        assert!(Config::from_env().validate().is_ok());
        std::env::set_var("OPENDESK_DEV", "true");
        assert!(Config::from_env().validate().is_ok());
        std::env::remove_var("OPENDESK_DEV");

        // EDGE_AUTH_DISABLED=true -> ok (dev-only escape, warns at boot).
        std::env::set_var("EDGE_AUTH_DISABLED", "true");
        assert!(Config::from_env().validate().is_ok());
        std::env::remove_var("EDGE_AUTH_DISABLED");
    }
}
