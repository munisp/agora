-- 0007_perf.sql — SPEC-W46 R10: list_invoices pagination index.
-- Applied idempotently by the service at startup (sqlx::raw_sql, same
-- bootstrap pattern as 0001-0006).
--
-- list_invoices is now bounded (LIMIT, default 50/max 200) and keyset
-- paginated on (created_at DESC, id DESC); the only pre-existing index that
-- led with tenant_id was (tenant_id, status), so every list paid a sort.
-- Plain (non-CONCURRENTLY) CREATE INDEX IF NOT EXISTS: matches the existing
-- migration-runner convention (raw_sql runs inside a transaction, where
-- CONCURRENTLY is disallowed).

CREATE INDEX IF NOT EXISTS idx_invoices_tenant_created
    ON invoices (tenant_id, created_at DESC);
