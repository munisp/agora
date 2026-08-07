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
    /// Paystack secret key; when set, payment-link uses the live Paystack
    /// initialize API and the webhook signature check is enforced.
    pub paystack_secret_key: Option<String>,
    /// Default customer email for Paystack initialize when the request body
    /// does not supply one.
    pub paystack_default_email: String,
    /// Public callback URL handed to Paystack initialize.
    pub paystack_callback_url: String,
    /// Static-mode merchant account ("name/account") for the EMVCo-style
    /// fallback payload when PAYSTACK_SECRET_KEY is unset.
    pub billing_static_account: String,
    /// Merchant display name embedded in the static EMV payload.
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

impl Config {
    pub fn from_env() -> Self {
        let paystack_secret_key = std::env::var("PAYSTACK_SECRET_KEY")
            .ok()
            .filter(|s| !s.trim().is_empty());
        Self {
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
            paystack_secret_key,
            paystack_default_email: env_or("PAYSTACK_DEFAULT_EMAIL", "billing@opendesk.local"),
            paystack_callback_url: env_or(
                "PAYSTACK_CALLBACK_URL",
                "http://localhost:9080/billing/callback",
            ),
            billing_static_account: env_or("BILLING_STATIC_ACCOUNT", "OPENDESK/0123456789"),
            billing_merchant_name: env_or("BILLING_MERCHANT_NAME", "OPENDESK DEMO"),
            dunning_interval_s: env_parse("DUNNING_INTERVAL_S", 3600),
            invoice_due_days: env_parse("INVOICE_DUE_DAYS", 14),
        }
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
