//! Environment-driven configuration (see README.md env table).

#[derive(Debug, Clone)]
pub struct Config {
    /// HTTP listen port (SPEC-W7: 7012).
    pub port: u16,
    /// Postgres DSN for the `billing` database (per-service role supported).
    pub database_url: String,
    /// Optional DSN for cross-tenant internal jobs (dunning sweep, Paystack
    /// webhook invoice lookup) — SPEC-W34 GF6. Point this at the
    /// `app_billing_internal_login` role (see migrations/0002_rls.sql) in any
    /// deployment where DATABASE_URL uses the least-privilege `app_billing`
    /// role; when unset, internal jobs share the main pool (fine for the dev
    /// default, where DATABASE_URL is the bootstrap superuser which bypasses
    /// RLS anyway).
    pub internal_database_url: Option<String>,
    pub kafka_brokers: String,
    pub kafka_group_id: String,
    /// Source topic for CloudEvents usage records (B1).
    pub usage_events_topic: String,
    pub kafka_consumer_enabled: bool,
    /// Outbound topic for com.opendesk.billing.* CloudEvents (B3).
    pub billing_events_topic: String,
    /// Ledger implementation selection (SIM-029): `postgres` (the real,
    /// durable implementation — DEFAULT) or `sim` (in-memory, dev/CI opt-in
    /// only). The service never silently falls back to the sim.
    pub billing_ledger_impl: String,
    /// Internal service-to-service token (RS-002): every request except
    /// /healthz and the signature-authenticated /webhooks/paystack must carry
    /// `x-internal-token` matching this value. REQUIRED — boot fails closed
    /// when unset, so the header-trust auth can never run unauthenticated.
    pub internal_token: String,
    /// Paystack secret key; when set, payment-link uses the live Paystack
    /// initialize API and the webhook signature check is enforced.
    pub paystack_secret_key: Option<String>,
    /// Default customer email for Paystack initialize when the request body
    /// does not supply one.
    pub paystack_default_email: String,
    /// Public callback URL handed to Paystack initialize.
    pub paystack_callback_url: String,
    /// Static-mode merchant account ("name/account") for the EMVCo-style
    /// fallback payload when PAYSTACK_SECRET_KEY is unset (SIM-030: REQUIRED
    /// in static mode — no hardcoded account default).
    pub billing_static_account: String,
    /// Merchant display name embedded in the static EMV payload (SIM-030:
    /// REQUIRED in static mode).
    pub billing_merchant_name: String,
    /// Dunning sweep cadence (B3).
    pub dunning_interval_s: u64,
    /// Issued invoices older than this many days become past_due.
    pub invoice_due_days: i64,
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

fn env_required(key: &str) -> Result<String, String> {
    std::env::var(key)
        .ok()
        .filter(|s| !s.trim().is_empty())
        .ok_or_else(|| format!("{key} is not set"))
}

impl Config {
    /// Load configuration from the environment. FAILS CLOSED (Err) when:
    /// - BILLING_INTERNAL_TOKEN is unset/empty (RS-002: header-trust auth
    ///   would otherwise be unauthenticated);
    /// - BILLING_LEDGER_IMPL is neither `postgres` nor `sim` (SIM-029);
    /// - in static payment mode (no PAYSTACK_SECRET_KEY) the merchant account
    ///   or display name is unset (SIM-030: no hardcoded billing account).
    pub fn from_env() -> Result<Self, String> {
        let paystack_secret_key = std::env::var("PAYSTACK_SECRET_KEY")
            .ok()
            .filter(|s| !s.trim().is_empty());

        let internal_token = env_required("BILLING_INTERNAL_TOKEN").map_err(|e| {
            format!(
                "{e}; refusing to start: internal-token auth would be disabled \
                 (set BILLING_INTERNAL_TOKEN and have APISIX inject x-internal-token \
                 after stripping any client-supplied copy)"
            )
        })?;

        let billing_ledger_impl = env_or("BILLING_LEDGER_IMPL", "postgres");
        if billing_ledger_impl != "postgres" && billing_ledger_impl != "sim" {
            return Err(format!(
                "unknown BILLING_LEDGER_IMPL '{billing_ledger_impl}' (expected postgres|sim)"
            ));
        }

        // SIM-030: static-mode merchant coordinates must be explicitly
        // configured — a hardcoded default account would silently misdirect
        // customer payments.
        let static_mode = paystack_secret_key.is_none();
        let billing_static_account = if static_mode {
            env_required("BILLING_STATIC_ACCOUNT").map_err(|e| {
                format!(
                    "{e}; refusing to start in static payment mode (PAYSTACK_SECRET_KEY \
                     unset): the QR payment account must be explicitly configured"
                )
            })?
        } else {
            env_or("BILLING_STATIC_ACCOUNT", "")
        };
        let billing_merchant_name = if static_mode {
            env_required("BILLING_MERCHANT_NAME").map_err(|e| {
                format!(
                    "{e}; refusing to start in static payment mode (PAYSTACK_SECRET_KEY \
                     unset): the merchant display name must be explicitly configured"
                )
            })?
        } else {
            env_or("BILLING_MERCHANT_NAME", "")
        };

        Ok(Self {
            port: env_parse("PORT", 7012),
            database_url: env_or(
                "DATABASE_URL",
                "postgres://opendesk:opendesk@postgres:5432/billing",
            ),
            internal_database_url: std::env::var("INTERNAL_DATABASE_URL")
                .ok()
                .filter(|s| !s.trim().is_empty()),
            kafka_brokers: env_or("KAFKA_BROKERS", "kafka:9092"),
            kafka_group_id: env_or("KAFKA_GROUP_ID", "billing-engine"),
            usage_events_topic: env_or("USAGE_EVENTS_TOPIC", "opendesk.usage.events"),
            kafka_consumer_enabled: env_parse("KAFKA_CONSUMER_ENABLED", true),
            billing_events_topic: env_or("BILLING_EVENTS_TOPIC", "opendesk.billing.events"),
            billing_ledger_impl,
            internal_token,
            paystack_secret_key,
            paystack_default_email: env_or("PAYSTACK_DEFAULT_EMAIL", "billing@opendesk.local"),
            paystack_callback_url: env_or(
                "PAYSTACK_CALLBACK_URL",
                "http://localhost:9080/billing/callback",
            ),
            billing_static_account,
            billing_merchant_name,
            dunning_interval_s: env_parse("DUNNING_INTERVAL_S", 3600),
            invoice_due_days: env_parse("INVOICE_DUE_DAYS", 14),
        })
    }

    /// `paystack` when a secret key is configured, otherwise `static` (EMV).
    pub fn payment_mode(&self) -> &'static str {
        if self.paystack_secret_key.is_some() {
            "paystack"
        } else {
            "static"
        }
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

    /// Deterministic env for each test: clear every posture variable.
    fn clear_posture_env() {
        for k in [
            "BILLING_INTERNAL_TOKEN",
            "BILLING_LEDGER_IMPL",
            "PAYSTACK_SECRET_KEY",
            "BILLING_STATIC_ACCOUNT",
            "BILLING_MERCHANT_NAME",
        ] {
            std::env::remove_var(k);
        }
    }

    /// RS-002: without an internal token the service must refuse to boot.
    #[test]
    fn internal_token_is_required() {
        let _g = env_lock();
        clear_posture_env();
        std::env::set_var("PAYSTACK_SECRET_KEY", "sk_test_x");
        let err = Config::from_env().unwrap_err();
        assert!(
            err.contains("BILLING_INTERNAL_TOKEN"),
            "error should name the missing variable: {err}"
        );
        clear_posture_env();
    }

    /// SIM-029: the ledger defaults to the real postgres implementation;
    /// `sim` requires an explicit opt-in; unknown values fail closed.
    #[test]
    fn ledger_impl_defaults_to_postgres_and_validates() {
        let _g = env_lock();
        clear_posture_env();
        std::env::set_var("BILLING_INTERNAL_TOKEN", "tok");
        std::env::set_var("PAYSTACK_SECRET_KEY", "sk_test_x");

        let cfg = Config::from_env().expect("defaults must load");
        assert_eq!(cfg.billing_ledger_impl, "postgres");

        std::env::set_var("BILLING_LEDGER_IMPL", "sim");
        let cfg = Config::from_env().expect("explicit sim must load");
        assert_eq!(cfg.billing_ledger_impl, "sim");

        std::env::set_var("BILLING_LEDGER_IMPL", "memory");
        let err = Config::from_env().unwrap_err();
        assert!(err.contains("memory"), "error should echo the value: {err}");
        clear_posture_env();
    }

    /// SIM-030: static payment mode without explicit merchant coordinates
    /// must fail closed (no hardcoded OPENDESK/0123456789 fallback).
    #[test]
    fn static_mode_requires_explicit_account_and_merchant_name() {
        let _g = env_lock();
        clear_posture_env();
        std::env::set_var("BILLING_INTERNAL_TOKEN", "tok");
        // Static mode (no PAYSTACK_SECRET_KEY): both must be required.
        let err = Config::from_env().unwrap_err();
        assert!(
            err.contains("BILLING_STATIC_ACCOUNT"),
            "missing account must fail closed: {err}"
        );
        std::env::set_var("BILLING_STATIC_ACCOUNT", "MERCHANT/123");
        let err = Config::from_env().unwrap_err();
        assert!(
            err.contains("BILLING_MERCHANT_NAME"),
            "missing merchant name must fail closed: {err}"
        );
        std::env::set_var("BILLING_MERCHANT_NAME", "MERCHANT LTD");
        let cfg = Config::from_env().expect("explicit static config must load");
        assert_eq!(cfg.payment_mode(), "static");
        assert_eq!(cfg.billing_static_account, "MERCHANT/123");
        assert_eq!(cfg.billing_merchant_name, "MERCHANT LTD");
        clear_posture_env();
    }

    /// In paystack mode the static coordinates are optional (unused).
    #[test]
    fn paystack_mode_does_not_require_static_coordinates() {
        let _g = env_lock();
        clear_posture_env();
        std::env::set_var("BILLING_INTERNAL_TOKEN", "tok");
        std::env::set_var("PAYSTACK_SECRET_KEY", "sk_test_x");
        let cfg = Config::from_env().expect("paystack mode must load");
        assert_eq!(cfg.payment_mode(), "paystack");
        assert_eq!(cfg.billing_static_account, "");
        clear_posture_env();
    }
}
