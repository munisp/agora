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
    /// Dead-letter topic for usage events that fail after bounded retries
    /// (SPEC-W43 B-04; same topic/convention as payments-service GF11).
    pub dlq_topic: String,
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
    /// fallback payload when PAYSTACK_SECRET_KEY is unset. SPEC-W44 F16-11:
    /// REQUIRED (boot error) when APP_ENV is production-like
    /// (production|staging); in dev a placeholder default is used with a WARN.
    pub billing_static_account: String,
    /// Merchant display name embedded in the static EMV payload (same
    /// F16-11 posture as the account).
    pub billing_merchant_name: String,
    /// SPEC-W44 K6: roles allowed to perform money mutations (`MONEY_ROLES`,
    /// comma-separated, default "owner,admin"); compared case-insensitively
    /// against the gateway-injected `X-User-Roles`.
    pub money_roles: Vec<String>,
    /// K1 dev escape (`OPENDESK_TRUST_DIRECT_TENANT=1`): accept the tenant
    /// context from caller params without the gateway-injected
    /// `X-Tenant-Slugs` binding (standalone dev only; never set in compose).
    pub trust_direct_tenant: bool,
    /// Direct HTTP base of identity-service (`IDENTITY_BASE_URL`), used by
    /// the K1 uuid binding: `GET {base}/v1/tenants/{slug}` resolves a
    /// gateway-claimed slug to its tenant uuid (src/identity.rs). Dev
    /// default `http://identity:7001` (compose service name); set EMPTY to
    /// disable resolution, which fails the uuid binding CLOSED (403).
    pub identity_base_url: String,
    /// `IDENTITY_INTERNAL_TOKEN` forwarded as `X-Internal-Token` on the
    /// identity tenant-resolution call (K2 — identity's getTenant accepts
    /// internal-token service callers). Unset/empty = no header; identity
    /// then answers 401 and resolution fails closed (503 to the caller).
    pub identity_internal_token: Option<String>,
    /// Slug->uuid resolution cache TTL (`TENANT_CACHE_TTL_SECONDS`,
    /// default 60). Only POSITIVE resolutions are cached; identity errors
    /// and unknown slugs always re-fetch (fail-closed authz decision — no
    /// stale serving, unlike booking's availability-oriented resolver).
    pub tenant_cache_ttl_s: u64,
    /// Dunning sweep cadence (B3).
    pub dunning_interval_s: u64,
    /// Issued invoices older than this many days become past_due.
    pub invoice_due_days: i64,
}

/// K6: csv env -> lowercase trimmed role list; empty input falls back to the
/// safe default so a misconfigured MONEY_ROLES="" never disables the gate.
fn parse_roles(raw: &str) -> Vec<String> {
    let roles: Vec<String> = raw
        .split(',')
        .map(|t| t.trim().to_ascii_lowercase())
        .filter(|t| !t.is_empty())
        .collect();
    if roles.is_empty() {
        vec!["owner".to_string(), "admin".to_string()]
    } else {
        roles
    }
}

/// F16-11: production-like environments never get silent defaults.
fn is_production_like(app_env: &str) -> bool {
    matches!(app_env.to_ascii_lowercase().as_str(), "production" | "staging")
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
                 (set BILLING_INTERNAL_TOKEN; in-cluster service callers present it \
                 as x-internal-token — APISIX strips caller-sent copies and, since \
                 SPEC-W44 F1, never injects one on gateway routes)"
            )
        })?;

        let billing_ledger_impl = env_or("BILLING_LEDGER_IMPL", "postgres");
        if billing_ledger_impl != "postgres" && billing_ledger_impl != "sim" {
            return Err(format!(
                "unknown BILLING_LEDGER_IMPL '{billing_ledger_impl}' (expected postgres|sim)"
            ));
        }

        // SIM-030 + SPEC-W44 F16-11: static-mode merchant coordinates must be
        // explicitly configured in production-like environments (APP_ENV in
        // {production, staging}) — a hardcoded default account would silently
        // misdirect customer payments. In dev (any other APP_ENV, unset
        // included) a clearly-marked placeholder default is used with a WARN.
        let app_env = env_or("APP_ENV", "development");
        let static_mode = paystack_secret_key.is_none();
        let billing_static_account = if static_mode {
            match std::env::var("BILLING_STATIC_ACCOUNT")
                .ok()
                .filter(|s| !s.trim().is_empty())
            {
                Some(v) => v,
                None if is_production_like(&app_env) => {
                    return Err(
                        "BILLING_STATIC_ACCOUNT is not set; refusing to start in static \
                         payment mode (PAYSTACK_SECRET_KEY unset) with APP_ENV=\
                         production-like: the QR payment account must be explicitly \
                         configured"
                            .to_string(),
                    )
                }
                None => {
                    tracing::warn!(
                        "BILLING_STATIC_ACCOUNT unset in dev (APP_ENV={app_env}): using the \
                         placeholder dev account for static QR payloads — set it explicitly \
                         for any real environment"
                    );
                    "OPENDESK-DEV/0000000000".to_string()
                }
            }
        } else {
            env_or("BILLING_STATIC_ACCOUNT", "")
        };
        let billing_merchant_name = if static_mode {
            match std::env::var("BILLING_MERCHANT_NAME")
                .ok()
                .filter(|s| !s.trim().is_empty())
            {
                Some(v) => v,
                None if is_production_like(&app_env) => {
                    return Err(
                        "BILLING_MERCHANT_NAME is not set; refusing to start in static \
                         payment mode (PAYSTACK_SECRET_KEY unset) with APP_ENV=\
                         production-like: the merchant display name must be explicitly \
                         configured"
                            .to_string(),
                    )
                }
                None => {
                    tracing::warn!(
                        "BILLING_MERCHANT_NAME unset in dev (APP_ENV={app_env}): using the \
                         placeholder dev merchant name"
                    );
                    "OpenDesk Dev".to_string()
                }
            }
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
            dlq_topic: env_or("DLQ_TOPIC", "opendesk.dlq"),
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
            money_roles: parse_roles(&env_or("MONEY_ROLES", "owner,admin")),
            trust_direct_tenant: env_parse("OPENDESK_TRUST_DIRECT_TENANT", false),
            identity_base_url: env_or("IDENTITY_BASE_URL", "http://identity:7001")
                .trim_end_matches('/')
                .to_string(),
            identity_internal_token: std::env::var("IDENTITY_INTERNAL_TOKEN")
                .ok()
                .filter(|s| !s.trim().is_empty()),
            tenant_cache_ttl_s: env_parse("TENANT_CACHE_TTL_SECONDS", 60),
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
            "APP_ENV",
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

    /// SIM-030 + F16-11: static payment mode without explicit merchant
    /// coordinates fails closed in production-like APP_ENV (no silent
    /// fallback), while dev gets a placeholder default.
    #[test]
    fn static_mode_requires_explicit_account_and_merchant_name() {
        let _g = env_lock();
        clear_posture_env();
        std::env::set_var("BILLING_INTERNAL_TOKEN", "tok");
        std::env::set_var("APP_ENV", "production");
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
        // staging is production-like too (F16-11).
        std::env::remove_var("BILLING_STATIC_ACCOUNT");
        std::env::set_var("APP_ENV", "staging");
        let err = Config::from_env().unwrap_err();
        assert!(
            err.contains("BILLING_STATIC_ACCOUNT"),
            "staging must fail closed as well: {err}"
        );
        clear_posture_env();
    }

    /// F16-11: in dev (APP_ENV unset/non-production) the static coordinates
    /// fall back to a placeholder default (WARN logged) instead of failing.
    #[test]
    fn static_mode_dev_default_account_with_warn() {
        let _g = env_lock();
        clear_posture_env();
        std::env::set_var("BILLING_INTERNAL_TOKEN", "tok");
        // No APP_ENV, no PAYSTACK_SECRET_KEY, no static coordinates: loads.
        let cfg = Config::from_env().expect("dev static mode must load with defaults");
        assert_eq!(cfg.payment_mode(), "static");
        assert_eq!(cfg.billing_static_account, "OPENDESK-DEV/0000000000");
        assert_eq!(cfg.billing_merchant_name, "OpenDesk Dev");
        // K6 default roles.
        assert_eq!(cfg.money_roles, vec!["owner".to_string(), "admin".to_string()]);
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
