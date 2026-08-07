-- 0002_rls.sql — billing-engine row-level security (SPEC-W34 GF6).
--
-- W34 probe T2#2 proved live that the billing tables had NO RLS: the
-- least-privilege app role could read another tenant's invoices with zero
-- GUCs set. 0001 documented tenant isolation as an HTTP-layer contract only.
-- This migration closes the hole at the database layer, following the
-- platform idiom (01-booking-schema.sql / 03 / 04 / 30-model-registry.sql):
--
--   * ENABLE + FORCE ROW LEVEL SECURITY on every tenant table (FORCE so the
--     policy binds even the table owner; superusers still bypass RLS, which
--     is why services must connect with the per-service roles from
--     05-app-roles.sql in any security-sensitive deployment);
--   * a PUBLIC `tenant_isolation` policy keyed on the request-scoped GUC
--       tenant_id = current_setting('app.tenant_id', true)::uuid
--     The GUC is set TRANSACTION-LOCALLY (`SELECT set_config(..., true)`) by
--     billing-engine at the start of every transaction that touches tenant
--     tables (see src/tenant.rs). With the GUC unset, current_setting returns
--     NULL, the comparison is NULL, and the policy is FAIL-CLOSED (0 rows).
--   * internal cross-tenant jobs (dunning sweep, Paystack webhook lookup) use
--     a separate NOLOGIN group role `app_billing_internal`, gated in the
--     policy via pg_has_role(current_user, 'app_billing_internal', 'member')
--     — the post-GF1 platform idiom: role membership cannot be spoofed by a
--     GUC the way the retired `app.*_internal` settings could. The service
--     connects for those jobs with INTERNAL_DATABASE_URL (the
--     `app_billing_internal_login` member).
--
-- processed_events and plan_presets have NO tenant_id column (processed_events
-- is the global at-least-once idempotency ledger keyed by event_id;
-- plan_presets is the global plan catalog seeded in 0001). Their policy
-- therefore requires the tenant context to be present (fail-closed when the
-- GUC is unset) without per-tenant row filtering.
--
-- Roles mirror 05-app-roles.sql exactly (NOLOGIN group + LOGIN member,
-- guarded by pg_roles existence checks) so this file also applies standalone
-- on a fresh cluster (e.g. the pgserver-backed GF6 gate); where 05 already
-- ran, the guards make role creation a no-op. Like 0001, this file is applied
-- idempotently by the service at startup (sqlx::raw_sql — no psql backslash
-- commands); DROP POLICY IF EXISTS keeps re-application safe.
--
-- NOTE: the applying connection must own the tables (dev default
-- DATABASE_URL uses the bootstrap superuser `opendesk`, which also created
-- them via 0001). A least-privilege runtime role cannot apply migrations —
-- same assumption 0001 already makes.

-- ---------------------------------------------------------------------------
-- Roles (cluster-wide; idempotent — mirrors 05-app-roles.sql billing section,
-- plus the internal batch role following 30-model-registry.sql/GF1)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_billing') THEN
        CREATE ROLE app_billing NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_billing_login') THEN
        CREATE ROLE app_billing_login LOGIN PASSWORD 'app_billing_dev_password' IN ROLE app_billing;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_billing_internal') THEN
        CREATE ROLE app_billing_internal NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_billing_internal_login') THEN
        CREATE ROLE app_billing_internal_login LOGIN PASSWORD 'app_billing_internal_dev_password' IN ROLE app_billing_internal;
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- Grants (idempotent; mirrors 05-app-roles.sql for app_billing and extends
-- the same pattern to the internal batch role)
-- ---------------------------------------------------------------------------
GRANT USAGE ON SCHEMA public TO app_billing;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_billing;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_billing;

GRANT USAGE ON SCHEMA public TO app_billing_internal;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_billing_internal;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_billing_internal;

-- Future tables created by the bootstrap superuser stay reachable. The docker
-- Postgres bootstrap role is `opendesk`; the embedded test Postgres (pgserver)
-- uses `postgres` — grant for whichever exists (30-model-registry.sql idiom,
-- generalized to both).
DO $$
DECLARE
    bootstrap TEXT;
BEGIN
    FOREACH bootstrap IN ARRAY ARRAY['opendesk', 'postgres'] LOOP
        IF EXISTS (SELECT FROM pg_roles WHERE rolname = bootstrap) THEN
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_billing', bootstrap);
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT USAGE, SELECT ON SEQUENCES TO app_billing', bootstrap);
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_billing_internal', bootstrap);
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT USAGE, SELECT ON SEQUENCES TO app_billing_internal', bootstrap);
        END IF;
    END LOOP;
END
$$;

-- ---------------------------------------------------------------------------
-- Tenant-scoped tables: fail-closed on the request tenant GUC, with the
-- role-gated internal escape hatch for cross-tenant batch jobs.
-- ---------------------------------------------------------------------------
ALTER TABLE usage_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE usage_records FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON usage_records;
CREATE POLICY tenant_isolation ON usage_records
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR pg_has_role(current_user, 'app_billing_internal', 'member'))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR pg_has_role(current_user, 'app_billing_internal', 'member'));

ALTER TABLE rate_cards ENABLE ROW LEVEL SECURITY;
ALTER TABLE rate_cards FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON rate_cards;
CREATE POLICY tenant_isolation ON rate_cards
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR pg_has_role(current_user, 'app_billing_internal', 'member'))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR pg_has_role(current_user, 'app_billing_internal', 'member'));

ALTER TABLE invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoices FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON invoices;
CREATE POLICY tenant_isolation ON invoices
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR pg_has_role(current_user, 'app_billing_internal', 'member'))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR pg_has_role(current_user, 'app_billing_internal', 'member'));

-- ---------------------------------------------------------------------------
-- Global tables (no tenant_id column): require the tenant context to be set
-- at all (fail-closed when unset); internal role retains full access.
-- ---------------------------------------------------------------------------
ALTER TABLE processed_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE processed_events FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_context_required ON processed_events;
CREATE POLICY tenant_context_required ON processed_events
    USING (current_setting('app.tenant_id', true) IS NOT NULL
           OR pg_has_role(current_user, 'app_billing_internal', 'member'))
    WITH CHECK (current_setting('app.tenant_id', true) IS NOT NULL
           OR pg_has_role(current_user, 'app_billing_internal', 'member'));

ALTER TABLE plan_presets ENABLE ROW LEVEL SECURITY;
ALTER TABLE plan_presets FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_context_required ON plan_presets;
CREATE POLICY tenant_context_required ON plan_presets
    USING (current_setting('app.tenant_id', true) IS NOT NULL
           OR pg_has_role(current_user, 'app_billing_internal', 'member'))
    WITH CHECK (current_setting('app.tenant_id', true) IS NOT NULL
           OR pg_has_role(current_user, 'app_billing_internal', 'member'));
