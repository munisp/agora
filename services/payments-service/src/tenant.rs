//! Tenant-scoped transactions (SPEC-W47, A-DB DB-01 CRIT).
//!
//! migrations/0001_rls.sql enables FORCE ROW LEVEL SECURITY on the payments
//! money tables (payout_beneficiaries, deposit_provenance, payout_attempts,
//! rail_attempts), keyed on the `app.tenant_id` GUC. Every transaction that
//! touches a tenant table must set that GUC TRANSACTION-LOCALLY
//! (`set_config(..., true)`) as its first statement, so the policy sees
//! exactly the request's tenant and the setting can never leak across pooled
//! connections. This mirrors billing-engine's src/tenant.rs idiom (GF6);
//! unlike billing, payments tenant ids are TEXT (Keycloak slugs or uuid
//! strings), so the policy does a plain text comparison.
//!
//! The tenant id comes from the already-authorized request context (the
//! K1/K2 tenant binding in routes.rs runs BEFORE any store call), or from
//! the durable row itself on the internal paths (the reconciler sweep reads
//! cross-tenant via the internal pool, then pins each settlement write to
//! the row's own tenant).
//!
//! Cross-tenant reads (reconciler `list_unknown`, the approval path's
//! learn-the-tenant lookup) connect through the internal pool
//! (`PAYMENTS_INTERNAL_DATABASE_URL`, role `app_payments_internal_login`)
//! whose policy access is gated by role membership, not by a spoofable GUC.
//! When `PAYMENTS_INTERNAL_DATABASE_URL` is unset the internal pool is the
//! main pool (dev default: bootstrap superuser, which bypasses RLS anyway).

use sqlx::{PgPool, Postgres, Transaction};

/// Begin a transaction with `app.tenant_id` set transaction-locally so the
/// RLS policies scope every statement to `tenant_id`. Fail-closed by
/// construction: without this call the policies match zero rows.
pub async fn begin_tenant_tx(
    pool: &PgPool,
    tenant_id: &str,
) -> Result<Transaction<'static, Postgres>, sqlx::Error> {
    let mut tx = pool.begin().await?;
    set_tenant_guc(&mut tx, tenant_id).await?;
    Ok(tx)
}

/// (Re)set `app.tenant_id` inside an already-open transaction.
pub async fn set_tenant_guc(
    tx: &mut Transaction<'_, Postgres>,
    tenant_id: &str,
) -> Result<(), sqlx::Error> {
    sqlx::query("SELECT set_config('app.tenant_id', $1, true)")
        .bind(tenant_id)
        .execute(&mut **tx)
        .await?;
    Ok(())
}

/// Begin a transaction on the internal pool (no tenant GUC): only for the
/// reconciler sweep and the approval path's cross-tenant attempt lookup.
pub async fn begin_internal_tx(pool: &PgPool) -> Result<Transaction<'static, Postgres>, sqlx::Error> {
    pool.begin().await
}

/// SPEC-W47 DB-01: ENABLE+FORCE RLS + tenant policies on the four payments
/// money tables. Applied ONCE at boot AFTER the stores' bootstrap DDL has
/// created the tables (the policy statements require the relations to
/// exist), through the same connection the bootstraps ran on. Fail-closed:
/// if RLS cannot be applied the service refuses to start rather than
/// serving cross-tenant money data. Idempotent (DROP POLICY IF EXISTS +
/// pg_roles guards), so restart re-application is a no-op.
pub async fn apply_rls_migration(pool: &PgPool) -> Result<(), String> {
    sqlx::raw_sql(include_str!("../migrations/0001_rls.sql"))
        .execute(pool)
        .await
        .map_err(|e| format!("payments RLS migration (0001_rls.sql) failed: {e}"))?;
    tracing::info!("payments RLS migration applied (0001_rls.sql: ENABLE+FORCE RLS on the money tables)");
    Ok(())
}
