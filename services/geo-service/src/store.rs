//! Postgres access for geo-service (SPEC-W52 Scope A) — payments-service
//! src/tenant.rs idiom.
//!
//! migrations/0001_geo.sql enables FORCE ROW LEVEL SECURITY on
//! `geo_boundaries`, keyed on the `app.tenant_id` GUC with the NULL-reference
//! variant (reference rows `tenant_id IS NULL` are world-visible; tenant rows
//! only under the request tenant). Every transaction that touches TENANT rows
//! sets the GUC transaction-locally (`set_config(..., true)`) as its first
//! statement so the policy sees exactly the request's tenant and the setting
//! can never leak across pooled connections. Reference-only reads (the
//! enrichment endpoint's first pass) intentionally run WITHOUT the GUC —
//! reference rows pass the policy for every role.

use sqlx::postgres::PgPoolOptions;
use sqlx::{PgPool, Postgres, Transaction};

/// Connect with retry (container boot races Postgres init); the pool runs
/// against the booking database (the PostGIS one).
pub async fn connect_with_retry(dsn: &str) -> Result<PgPool, String> {
    let mut last = String::new();
    for attempt in 1..=10 {
        match PgPoolOptions::new()
            .max_connections(8)
            .acquire_timeout(std::time::Duration::from_secs(10))
            .connect(dsn)
            .await
        {
            Ok(pool) => return Ok(pool),
            Err(e) => {
                last = format!("{e}");
                tracing::warn!(attempt, error = %last, "geo-service: postgres connect failed; retrying");
                tokio::time::sleep(std::time::Duration::from_secs(2)).await;
            }
        }
    }
    Err(format!("postgres connect failed after 10 attempts: {last}"))
}

/// SPEC-W52: apply migrations/0001_geo.sql (table + ENABLE+FORCE RLS +
//  NULL-reference tenant_isolation policy + roles/grants) ONCE at boot.
/// Fail-closed: if the migration cannot be applied (e.g. PostGIS absent —
/// booking's ErrPostGISUnavailable posture) the service refuses to start
/// rather than serving without isolation. Idempotent (IF NOT EXISTS guards +
/// DROP POLICY IF EXISTS), so restart re-application is a no-op.
pub async fn apply_geo_migration(pool: &PgPool) -> Result<(), String> {
    sqlx::raw_sql(include_str!("../migrations/0001_geo.sql"))
        .execute(pool)
        .await
        .map_err(|e| format!("geo migration (0001_geo.sql) failed: {e}"))?;
    tracing::info!(
        "geo migration applied (0001_geo.sql: geo_boundaries + ENABLE+FORCE RLS, NULL-reference policy)"
    );
    Ok(())
}

/// Begin a transaction with `app.tenant_id` set transaction-locally so the
/// RLS policy scopes every statement to `tenant_id`. Pass the ZERO uuid for
/// platform-reference writes: with the GUC set to the zero uuid (never a real
/// tenant), tenant rows match nothing while `tenant_id IS NULL` reference
/// rows pass the policy's WITH CHECK.
pub async fn begin_tenant_tx(
    pool: &PgPool,
    tenant_id: uuid::Uuid,
) -> Result<Transaction<'static, Postgres>, sqlx::Error> {
    let mut tx = pool.begin().await?;
    sqlx::query("SELECT set_config('app.tenant_id', $1, true)")
        .bind(tenant_id.to_string())
        .execute(&mut *tx)
        .await?;
    Ok(tx)
}
