-- 0001_rls.sql — payments-service row-level security (SPEC-W47, A-DB DB-01 CRIT).
--
-- DB-01 proved statically (and billing's W34 T2#2 probe proved live for the
-- same posture) that the payments money tables — payout_beneficiaries,
-- deposit_provenance, payout_attempts, rail_attempts — had NO RLS at all and
-- were read/updated over the bootstrap superuser DSN: get()/mark() filtered
-- by payout_id only, so any holder of the DSN could read/update every
-- tenant's rows (payee party_id_info = financial PII).
--
-- (SPEC-W49 A1 added payment_event_failures, covered by the same
-- tenant_isolation policy below.)
--
-- This migration closes the hole at the database layer, mirroring
-- billing-engine/migrations/0002_rls.sql (the platform idiom):
--
--   * ENABLE + FORCE ROW LEVEL SECURITY on every tenant table (FORCE so the
--     policy binds even the table owner; superusers still bypass RLS, which
--     is why the runtime DSN must move to the per-service role below in any
--     security-sensitive deployment — that compose/init-scripts change is
--     W47-H's part of DB-01);
--   * a PUBLIC `tenant_isolation` policy keyed on the request-scoped GUC
--       tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
--     Unlike billing, payments tenant ids are TEXT (Keycloak slugs OR uuid
--     strings), so there is NO ::uuid cast. The GUC is set
--     TRANSACTION-LOCALLY (`SELECT set_config(..., true)`) by
--     payments-service at the start of every transaction that touches tenant
--     tables (see src/tenant.rs). With the GUC unset, current_setting
--     returns NULL, the comparison is NULL, and the policy is FAIL-CLOSED
--     (0 rows);
--   * internal cross-tenant jobs (the payout reconciler sweep over
--     `payout_attempts.state='unknown'`, the approval path's
--     learn-the-tenant-from-the-row lookup) use a separate NOLOGIN group
--     role `app_payments_internal`, gated in the policy via
--     pg_has_role(current_user, 'app_payments_internal', 'member') — role
--     membership cannot be spoofed by a GUC. The service connects for those
--     jobs with PAYMENTS_INTERNAL_DATABASE_URL (the
--     `app_payments_internal_login` member); when unset the internal pool is
--     the main pool (dev default: bootstrap superuser, which bypasses RLS
--     anyway).
--
-- Roles mirror the 05-app-roles.sql pattern exactly (NOLOGIN group + LOGIN
-- member, guarded by pg_roles existence checks) so this file also applies
-- standalone on a fresh cluster (e.g. the pgserver-backed gate); where the
-- init scripts already ran, the guards make role creation a no-op.
--
-- GRANT CREATE ON SCHEMA is included for app_payments ONLY because
-- payments-service has no separate migration runner: its schema bootstrap
-- (CREATE TABLE IF NOT EXISTS in src/payouts.rs / registry.rs /
-- transfers.rs) runs at boot on the SAME DSN the runtime uses, so the
-- least-privilege login role must be able to create its tables on a fresh
-- database (they are then owned by app_payments, and FORCE RLS binds the
-- owner). On an already-provisioned database CREATE TABLE IF NOT EXISTS is
-- a no-op.
--
-- This file is applied idempotently by the service at startup
-- (sqlx::raw_sql — no psql backslash commands); DROP POLICY IF EXISTS keeps
-- re-application safe.
--
-- NOTE: the applying connection must own the tables (dev default
-- PAYMENTS_DATABASE_URL uses the bootstrap superuser `opendesk`, which also
-- created them via the runtime bootstrap DDL). A least-privilege runtime
-- role cannot CREATE ROLE or ALTER other owners' tables — same assumption
-- billing's 0002 already makes.

-- ---------------------------------------------------------------------------
-- Roles (cluster-wide; idempotent — mirrors 05-app-roles.sql's per-service
-- pattern, plus the internal batch role following billing's 0002/GF1 idiom)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_payments') THEN
        CREATE ROLE app_payments NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_payments_login') THEN
        CREATE ROLE app_payments_login LOGIN PASSWORD 'app_payments_dev_password' IN ROLE app_payments;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_payments_internal') THEN
        CREATE ROLE app_payments_internal NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_payments_internal_login') THEN
        CREATE ROLE app_payments_internal_login LOGIN PASSWORD 'app_payments_internal_dev_password' IN ROLE app_payments_internal;
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- Grants (idempotent; extends the billing 0002 pattern to payments. The
-- CREATE ON SCHEMA grant exists because the payments schema bootstrap runs
-- on the runtime DSN — see the header note.)
-- ---------------------------------------------------------------------------
GRANT USAGE ON SCHEMA public TO app_payments;
GRANT CREATE ON SCHEMA public TO app_payments;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_payments;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_payments;

GRANT USAGE ON SCHEMA public TO app_payments_internal;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_payments_internal;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_payments_internal;

-- Future tables created by the bootstrap superuser stay reachable. The docker
-- Postgres bootstrap role is `opendesk`; the embedded test Postgres (pgserver)
-- uses `postgres` — grant for whichever exists (billing 0002 /
-- 30-model-registry.sql idiom, generalized to both).
DO $$
DECLARE
    bootstrap TEXT;
BEGIN
    FOREACH bootstrap IN ARRAY ARRAY['opendesk', 'postgres'] LOOP
        -- ALTER DEFAULT PRIVILEGES FOR ROLE requires membership in that role
        -- (or superuser); skip gracefully when the applying connection is a
        -- least-privilege app role provisioning a fresh database it owns.
        IF EXISTS (SELECT FROM pg_roles WHERE rolname = bootstrap)
           AND pg_has_role(current_user, bootstrap, 'member') THEN
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_payments', bootstrap);
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT USAGE, SELECT ON SEQUENCES TO app_payments', bootstrap);
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_payments_internal', bootstrap);
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT USAGE, SELECT ON SEQUENCES TO app_payments_internal', bootstrap);
        END IF;
    END LOOP;
END
$$;

-- ---------------------------------------------------------------------------
-- Tenant-scoped money tables: fail-closed on the request tenant GUC, with
-- the role-gated internal escape hatch for cross-tenant batch jobs
-- (reconciler sweep, approval lookup). tenant_id is TEXT here (Keycloak
-- slug or uuid string) — no ::uuid cast.
-- ---------------------------------------------------------------------------
ALTER TABLE payout_beneficiaries ENABLE ROW LEVEL SECURITY;
ALTER TABLE payout_beneficiaries FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON payout_beneficiaries;
CREATE POLICY tenant_isolation ON payout_beneficiaries
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
           OR pg_has_role(current_user, 'app_payments_internal', 'member'))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
           OR pg_has_role(current_user, 'app_payments_internal', 'member'));

ALTER TABLE deposit_provenance ENABLE ROW LEVEL SECURITY;
ALTER TABLE deposit_provenance FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON deposit_provenance;
CREATE POLICY tenant_isolation ON deposit_provenance
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
           OR pg_has_role(current_user, 'app_payments_internal', 'member'))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
           OR pg_has_role(current_user, 'app_payments_internal', 'member'));

ALTER TABLE payout_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE payout_attempts FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON payout_attempts;
CREATE POLICY tenant_isolation ON payout_attempts
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
           OR pg_has_role(current_user, 'app_payments_internal', 'member'))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
           OR pg_has_role(current_user, 'app_payments_internal', 'member'));

ALTER TABLE rail_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE rail_attempts FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON rail_attempts;
CREATE POLICY tenant_isolation ON rail_attempts
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
           OR pg_has_role(current_user, 'app_payments_internal', 'member'))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
           OR pg_has_role(current_user, 'app_payments_internal', 'member'));

-- SPEC-W49 A1: durable failed-event log (capture events whose Dapr pubsub
-- publication exhausted every retry). Same tenant_isolation posture: writes
-- are pinned to the event's tenant GUC; the K2-guarded replay route reads
-- and deletes through the internal role escape.
ALTER TABLE payment_event_failures ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_event_failures FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON payment_event_failures;
CREATE POLICY tenant_isolation ON payment_event_failures
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
           OR pg_has_role(current_user, 'app_payments_internal', 'member'))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
           OR pg_has_role(current_user, 'app_payments_internal', 'member'));
