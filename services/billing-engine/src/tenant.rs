//! Tenant-scoped transactions (SPEC-W34 GF6).
//!
//! migrations/0002_rls.sql enables FORCE ROW LEVEL SECURITY on the billing
//! tables, keyed on the `app.tenant_id` GUC. Every transaction that touches a
//! tenant table must set that GUC TRANSACTION-LOCALLY (`set_config(..., true)`)
//! as its first statement, so the policy sees exactly the request's tenant and
//! the setting can never leak across pooled connections.
//!
//! The tenant id comes from the request's tenant context (the
//! gateway-validated `X-Tenant-ID` header — the gateway strips client-spoofed
//! copies since W34 GF4, and the gateway is the only entry point), from the
//! usage event payload (metering consumer), or from the invoice row itself
//! after a signature-authenticated webhook lookup.
//!
//! Cross-tenant batch jobs (dunning sweep, webhook invoice lookup) connect
//! through the internal pool (`INTERNAL_DATABASE_URL`, role
//! `app_billing_internal_login`) whose policy access is gated by role
//! membership, not by a spoofable GUC. When `INTERNAL_DATABASE_URL` is unset
//! the internal pool is the main pool (dev default: bootstrap superuser,
//! which bypasses RLS anyway).

use sqlx::{PgPool, Postgres, Transaction};
use uuid::Uuid;

/// Begin a transaction with `app.tenant_id` set transaction-locally so the
/// RLS policies scope every statement to `tenant_id`. Fail-closed by
/// construction: without this call the policies match zero rows.
pub async fn begin_tenant_tx(
    pool: &PgPool,
    tenant_id: Uuid,
) -> Result<Transaction<'static, Postgres>, sqlx::Error> {
    let mut tx = pool.begin().await?;
    set_tenant_guc(&mut tx, tenant_id).await?;
    Ok(tx)
}

/// (Re)set `app.tenant_id` inside an already-open transaction. Used by the
/// Paystack webhook path: the signature-authenticated invoice lookup runs on
/// the internal pool, then the GUC is pinned to the invoice's own tenant for
/// the state transition, so writes are still tenant-scoped.
pub async fn set_tenant_guc(
    tx: &mut Transaction<'_, Postgres>,
    tenant_id: Uuid,
) -> Result<(), sqlx::Error> {
    sqlx::query("SELECT set_config('app.tenant_id', $1, true)")
        .bind(tenant_id.to_string())
        .execute(&mut **tx)
        .await?;
    Ok(())
}

/// Begin a transaction on the internal pool (no tenant GUC): only for the
/// signature-authenticated webhook lookup and the dunning sweep.
pub async fn begin_internal_tx(pool: &PgPool) -> Result<Transaction<'static, Postgres>, sqlx::Error> {
    pool.begin().await
}

// The GUC wiring is exercised end-to-end by the pgserver-backed GF6 gate
// probe (apply 0001+0002; app role sees only its tenant, GUC unset sees 0
// rows, FORCE binds the table owner). There is no embedded Postgres in the
// cargo harness, so there is nothing further to unit-test here.
