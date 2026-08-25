-- 0005_hardening.sql — billing-engine hardening (SPEC-W43 B-07; SEC#11 /
-- DATA#9). Applied idempotently by the service at startup after 0004 (same
-- sqlx::raw_sql bootstrap pattern; no psql backslash commands).
--
--   1. RLS on billing_outbox: the outbox is a global table (no tenant_id
--      column) written from tenant-scoped transactions (invoice paid /
--      voided / payment-mismatch / duplicate-payment events) and drained by
--      the relay on the internal pool. Policy follows the 0002 idiom for
--      global tables: fail-closed without the request-scoped
--      `app.tenant_id` GUC, with the role-gated internal escape
--      (`app_billing_internal`) for the relay and the signature-
--      authenticated webhook path.
--   2. REVOKE DELETE on ledger_accounts / ledger_transfers from the
--      application roles: the receivables ledger is append-only; nothing in
--      the service (or any future bug in it) may delete accounts or
--      transfers. The bootstrap/owner role retains DELETE for migrations
--      and drills.
--   3. invoices.period format CHECK ('YYYY-MM', month 01..12), added
--      NOT VALID so existing rows are not rewritten, then VALIDATEd so
--      historical rows are verified once at apply time.

-- ---------------------------------------------------------------------------
-- 1) billing_outbox row-level security (0002 global-table idiom)
-- ---------------------------------------------------------------------------
ALTER TABLE billing_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing_outbox FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_context_required ON billing_outbox;
CREATE POLICY tenant_context_required ON billing_outbox
    USING (NULLIF(current_setting('app.tenant_id', true), '') IS NOT NULL
           OR pg_has_role(current_user, 'app_billing_internal', 'member'))
    WITH CHECK (NULLIF(current_setting('app.tenant_id', true), '') IS NOT NULL
           OR pg_has_role(current_user, 'app_billing_internal', 'member'));

-- ---------------------------------------------------------------------------
-- 2) The ledger is append-only for application roles (guarded on role
--    existence so this file also applies standalone on a fresh cluster;
--    REVOKE itself is idempotent)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_billing') THEN
        EXECUTE 'REVOKE DELETE ON ledger_accounts FROM app_billing';
        EXECUTE 'REVOKE DELETE ON ledger_transfers FROM app_billing';
    END IF;
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_billing_internal') THEN
        EXECUTE 'REVOKE DELETE ON ledger_accounts FROM app_billing_internal';
        EXECUTE 'REVOKE DELETE ON ledger_transfers FROM app_billing_internal';
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- 3) invoices.period format: 'YYYY-MM' with month 01..12 (NOT VALID +
--    VALIDATE; DROP first keeps re-application idempotent)
-- ---------------------------------------------------------------------------
ALTER TABLE invoices DROP CONSTRAINT IF EXISTS invoices_period_format;
ALTER TABLE invoices ADD CONSTRAINT invoices_period_format
    CHECK (period ~ '^\d{4}-(0[1-9]|1[0-2])$') NOT VALID;
ALTER TABLE invoices VALIDATE CONSTRAINT invoices_period_format;
