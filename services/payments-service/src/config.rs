//! Environment-driven configuration (see README.md env table).

#[derive(Debug, Clone)]
pub struct Config {
    /// HTTP listen port (SPEC §3: 7004).
    pub port: u16,
    /// `sim` (ADR-0007 dev/CI fallback) or `tigerbeetle` (requires `tb-live`
    /// feature). SPEC-W34 GF11: must be set EXPLICITLY via LEDGER_IMPL —
    /// silently defaulting to the in-memory sim in a money path is a finding,
    /// so `from_env` fails closed when the variable is missing/empty.
    pub ledger_impl: String,
    pub tb_addresses: String,
    pub tb_cluster_id: u128,
    pub kafka_brokers: String,
    pub kafka_group_id: String,
    pub kafka_commands_topic: String,
    pub kafka_consumer_enabled: bool,
    /// Dead-letter topic for failed payments commands (SPEC-W34 GF11; same
    /// DLQ naming as booking-service: `opendesk.dlq`).
    pub dlq_topic: String,
    pub dapr_host: String,
    pub dapr_http_port: u16,
    pub dapr_pubsub: String,
    pub events_topic: String,
    /// Mojaloop payout rail endpoint (SIM-003: no silent simulator default).
    /// The real rail is configured via MOJALOOP_ENDPOINT; the
    /// mojaloop-simulator URL is only ever used when MOJALOOP_ALLOW_SIM=true
    /// (dev/CI opt-in) — otherwise `from_env` fails closed.
    pub mojaloop_endpoint: String,
    /// True only when MOJALOOP_ALLOW_SIM opted in to the simulator rail.
    pub mojaloop_allow_sim: bool,
    /// Platform fee in basis points applied on captures/no-show fees.
    pub platform_fee_bps: u64,
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
    /// Load configuration from the environment. Fails closed (Err) when
    /// LEDGER_IMPL is unset/empty or not one of `sim|tigerbeetle` — the money
    /// path must never silently fall back to the in-memory sim (SPEC-W34
    /// GF11; previously it defaulted to `sim`).
    pub fn from_env() -> Result<Self, String> {
        let ledger_impl = std::env::var("LEDGER_IMPL")
            .ok()
            .filter(|s| !s.trim().is_empty())
            .ok_or_else(|| {
                "LEDGER_IMPL is not set; refusing to start without an explicit \
                 ledger selection (expected LEDGER_IMPL=sim for dev/CI or \
                 LEDGER_IMPL=tigerbeetle for the live ledger)"
                    .to_string()
            })?;
        if ledger_impl != "sim" && ledger_impl != "tigerbeetle" {
            return Err(format!(
                "unknown LEDGER_IMPL '{ledger_impl}' (expected sim|tigerbeetle)"
            ));
        }
        // SIM-003 mock-posture contract: the Mojaloop simulator rail is
        // opt-in. Default posture (MOJALOOP_ALLOW_SIM unset/false) requires an
        // explicitly configured real endpoint; with neither, fail closed at
        // boot — never silently aim payouts at the simulator.
        let mojaloop_allow_sim = env_parse("MOJALOOP_ALLOW_SIM", false);
        let mojaloop_endpoint = match std::env::var("MOJALOOP_ENDPOINT")
            .ok()
            .filter(|s| !s.trim().is_empty())
        {
            Some(ep) => ep,
            None if mojaloop_allow_sim => {
                // Dev/CI only: the mojaloop-simulator container.
                "http://mojaloop:8444".to_string()
            }
            None => {
                return Err(
                    "MOJALOOP_ENDPOINT is not set and MOJALOOP_ALLOW_SIM is not true; \
                     refusing to start: the payout rail would have no real Mojaloop \
                     endpoint (set MOJALOOP_ENDPOINT for the real rail, or opt in to \
                     the mojaloop-simulator for dev/CI with MOJALOOP_ALLOW_SIM=true)"
                        .to_string(),
                )
            }
        };
        Ok(Self {
            port: env_parse("PORT", 7004),
            ledger_impl,
            tb_addresses: env_or("TB_ADDRESSES", "tigerbeetle:3000"),
            tb_cluster_id: env_parse("TB_CLUSTER_ID", 0),
            kafka_brokers: env_or("KAFKA_BROKERS", "kafka:9092"),
            kafka_group_id: env_or("KAFKA_GROUP_ID", "payments-service"),
            kafka_commands_topic: env_or("PAYMENTS_COMMANDS_TOPIC", "opendesk.payments.commands"),
            kafka_consumer_enabled: env_parse("KAFKA_CONSUMER_ENABLED", true),
            dlq_topic: env_or("DLQ_TOPIC", "opendesk.dlq"),
            dapr_host: env_or("DAPR_HOST", "daprd-payments"),
            dapr_http_port: env_parse("DAPR_HTTP_PORT", 3500),
            dapr_pubsub: env_or("DAPR_PUBSUB_NAME", "pubsub-kafka"),
            events_topic: env_or("PAYMENTS_EVENTS_TOPIC", "opendesk.payments.events"),
            mojaloop_endpoint,
            mojaloop_allow_sim,
            platform_fee_bps: env_parse("PLATFORM_FEE_BPS", 250),
        })
    }

    pub fn dapr_base_url(&self) -> String {
        format!("http://{}:{}", self.dapr_host, self.dapr_http_port)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{Mutex, OnceLock};

    /// Env-var mutation is process-global; serialize the config tests.
    fn env_lock() -> std::sync::MutexGuard<'static, ()> {
        static LOCK: OnceLock<Mutex<()>> = OnceLock::new();
        LOCK.get_or_init(|| Mutex::new(())).lock().unwrap()
    }

    /// Deterministic env for each test: clear every variable `from_env`
    /// treats as posture-sensitive.
    fn clear_posture_env() {
        std::env::remove_var("LEDGER_IMPL");
        std::env::remove_var("MOJALOOP_ENDPOINT");
        std::env::remove_var("MOJALOOP_ALLOW_SIM");
    }

    #[test]
    fn ledger_impl_must_be_explicit() {
        let _g = env_lock();
        clear_posture_env();
        let err = Config::from_env().unwrap_err();
        assert!(
            err.contains("LEDGER_IMPL"),
            "error should name the missing variable: {err}"
        );
    }

    #[test]
    fn ledger_impl_rejects_unknown_value() {
        let _g = env_lock();
        clear_posture_env();
        std::env::set_var("LEDGER_IMPL", "sqlite");
        let err = Config::from_env().unwrap_err();
        assert!(err.contains("sqlite"), "error should echo the value: {err}");
        clear_posture_env();
    }

    #[test]
    fn ledger_impl_explicit_sim_loads() {
        let _g = env_lock();
        clear_posture_env();
        std::env::set_var("LEDGER_IMPL", "sim");
        std::env::set_var("MOJALOOP_ALLOW_SIM", "true");
        let cfg = Config::from_env().expect("explicit sim must load");
        assert_eq!(cfg.ledger_impl, "sim");
        assert_eq!(cfg.dlq_topic, "opendesk.dlq");
        clear_posture_env();
    }

    // ------------------------------------------------------------------
    // SIM-003: Mojaloop simulator rail is opt-in, never the silent default.
    // ------------------------------------------------------------------

    #[test]
    fn mojaloop_fails_closed_when_endpoint_missing_and_sim_not_allowed() {
        let _g = env_lock();
        clear_posture_env();
        std::env::set_var("LEDGER_IMPL", "sim");
        // Default posture: no endpoint, no opt-in -> explicit boot error.
        let err = Config::from_env().unwrap_err();
        assert!(
            err.contains("MOJALOOP_ENDPOINT") && err.contains("MOJALOOP_ALLOW_SIM"),
            "error should name both variables: {err}"
        );
        // Explicit false must behave identically to unset.
        std::env::set_var("MOJALOOP_ALLOW_SIM", "false");
        let err = Config::from_env().unwrap_err();
        assert!(err.contains("MOJALOOP_ENDPOINT"), "false must not opt in: {err}");
        clear_posture_env();
    }

    #[test]
    fn mojaloop_sim_rail_requires_explicit_opt_in() {
        let _g = env_lock();
        clear_posture_env();
        std::env::set_var("LEDGER_IMPL", "sim");
        std::env::set_var("MOJALOOP_ALLOW_SIM", "true");
        let cfg = Config::from_env().expect("sim opt-in must load");
        assert!(cfg.mojaloop_allow_sim);
        assert_eq!(cfg.mojaloop_endpoint, "http://mojaloop:8444");
        clear_posture_env();
    }

    #[test]
    fn mojaloop_real_endpoint_loads_without_sim_opt_in() {
        let _g = env_lock();
        clear_posture_env();
        std::env::set_var("LEDGER_IMPL", "sim");
        std::env::set_var("MOJALOOP_ENDPOINT", "https://mojaloop.example.com");
        let cfg = Config::from_env().expect("explicit endpoint must load");
        assert!(!cfg.mojaloop_allow_sim);
        assert_eq!(cfg.mojaloop_endpoint, "https://mojaloop.example.com");
        clear_posture_env();
    }
}
