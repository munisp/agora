-- 05-app-roles.sql — per-service least-privilege DB roles (SPEC-W3 §2).
--
-- Pattern: a NOLOGIN NOINHERIT group role per service holds the grants; a
-- LOGIN variant inherits them via membership. Services connect with the
-- LOGIN variant, so:
--   * a compromised service role can only reach its OWN database's tables;
--   * it is not the table owner, so FORCE ROW LEVEL SECURITY (01/03/04
--     schemas) actually applies to it — the superuser `opendesk` bypasses
--     RLS, which is why per-service roles are required for real isolation.
--
-- DEV PASSWORDS below match .env.example — rotate in production
-- (docs/runbooks/secrets.md). Roles are cluster-global; this script is
-- idempotent via pg_roles existence checks. Runs under
-- docker-entrypoint-initdb.d's psql path (uses \c like the other scripts).
--
-- W51-C (V-FUNDS grants gap): several services apply schema DDL at runtime
-- on their OWN DSN (booking-service ensure* migrations, payments-service
-- boot bootstrap, notification-worker, crm-sync, kyc, billing,
-- conversation/knowledge helpers), so those tables are owned by the
-- app_*_login role — NOT by the opendesk/postgres bootstrap superuser.
-- Default privileges FOR ROLE opendesk/postgres alone therefore leave
-- runtime-created tables (booking_reviews, booking_payment_attempts, ...)
-- with no grants for the other app roles (notably the app_*_internal
-- cross-tenant escape groups). Every per-database section below now also
-- sets ALTER DEFAULT PRIVILEGES FOR ROLE <app_*_login> so future tables
-- inherit the same grants automatically. Tables already existing when this
-- script (re)runs are covered by the GRANT ... ON ALL TABLES statements.

-- ---------------------------------------------------------------------------
-- Roles (created once, cluster-wide)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_booking') THEN
        CREATE ROLE app_booking NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_booking_login') THEN
        CREATE ROLE app_booking_login LOGIN PASSWORD 'app_booking_dev_password' IN ROLE app_booking;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_conversation') THEN
        CREATE ROLE app_conversation NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_conversation_login') THEN
        CREATE ROLE app_conversation_login LOGIN PASSWORD 'app_conversation_dev_password' IN ROLE app_conversation;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_knowledge') THEN
        CREATE ROLE app_knowledge NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_knowledge_login') THEN
        CREATE ROLE app_knowledge_login LOGIN PASSWORD 'app_knowledge_dev_password' IN ROLE app_knowledge;
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- booking database
-- ---------------------------------------------------------------------------
\c booking

GRANT CONNECT ON DATABASE booking TO app_booking;
GRANT USAGE ON SCHEMA public TO app_booking;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_booking;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_booking;
-- Future tables created by the bootstrap superuser stay reachable.
ALTER DEFAULT PRIVILEGES FOR ROLE opendesk IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_booking;
ALTER DEFAULT PRIVILEGES FOR ROLE opendesk IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO app_booking;
-- W51-C: booking-service runs its ensure* migrations at boot on its runtime
-- DSN (app_booking_login owns booking_reviews, booking_payment_attempts,
-- ...) — extend default privileges to that migration-owner role too.
ALTER DEFAULT PRIVILEGES FOR ROLE app_booking_login IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_booking;
ALTER DEFAULT PRIVILEGES FOR ROLE app_booking_login IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO app_booking;

-- ---------------------------------------------------------------------------
-- conversation database
-- ---------------------------------------------------------------------------
\c conversation

GRANT CONNECT ON DATABASE conversation TO app_conversation;
GRANT USAGE ON SCHEMA public TO app_conversation;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_conversation;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_conversation;
ALTER DEFAULT PRIVILEGES FOR ROLE opendesk IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_conversation;
ALTER DEFAULT PRIVILEGES FOR ROLE opendesk IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO app_conversation;
-- W51-C: conversation-service applies helper DDL at runtime on its own DSN
-- (app/db.py, app/agent_db.py) as app_conversation_login.
ALTER DEFAULT PRIVILEGES FOR ROLE app_conversation_login IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_conversation;
ALTER DEFAULT PRIVILEGES FOR ROLE app_conversation_login IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO app_conversation;

-- ---------------------------------------------------------------------------
-- knowledge database
-- ---------------------------------------------------------------------------
\c knowledge

GRANT CONNECT ON DATABASE knowledge TO app_knowledge;
GRANT USAGE ON SCHEMA public TO app_knowledge;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_knowledge;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_knowledge;
ALTER DEFAULT PRIVILEGES FOR ROLE opendesk IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_knowledge;
ALTER DEFAULT PRIVILEGES FOR ROLE opendesk IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO app_knowledge;
-- W51-C: knowledge-service applies helper DDL at runtime on its own DSN
-- (app/suggestions.py) as app_knowledge_login.
ALTER DEFAULT PRIVILEGES FOR ROLE app_knowledge_login IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_knowledge;
ALTER DEFAULT PRIVILEGES FOR ROLE app_knowledge_login IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO app_knowledge;

-- ---------------------------------------------------------------------------
-- Wave 7 (SPEC-W7 Part B): billing-engine role
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_billing') THEN
        CREATE ROLE app_billing NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_billing_login') THEN
        CREATE ROLE app_billing_login LOGIN PASSWORD 'app_billing_dev_password' IN ROLE app_billing;
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- billing database
-- ---------------------------------------------------------------------------
\c billing

GRANT CONNECT ON DATABASE billing TO app_billing;
GRANT USAGE ON SCHEMA public TO app_billing;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_billing;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_billing;
ALTER DEFAULT PRIVILEGES FOR ROLE opendesk IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_billing;
ALTER DEFAULT PRIVILEGES FOR ROLE opendesk IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO app_billing;
-- W51-C: billing-engine applies its migrations at boot on the runtime DSN
-- (app_billing_login owns the migration-created tables).
ALTER DEFAULT PRIVILEGES FOR ROLE app_billing_login IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_billing;
ALTER DEFAULT PRIVILEGES FOR ROLE app_billing_login IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO app_billing;

-- ---------------------------------------------------------------------------
-- Wave 12 (SPEC-W12): kyc-service role
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_kyc') THEN
        CREATE ROLE app_kyc NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_kyc_login') THEN
        CREATE ROLE app_kyc_login LOGIN PASSWORD 'app_kyc_dev_password' IN ROLE app_kyc;
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- kyc database
-- ---------------------------------------------------------------------------
\c kyc

GRANT CONNECT ON DATABASE kyc TO app_kyc;
GRANT USAGE ON SCHEMA public TO app_kyc;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_kyc;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_kyc;
ALTER DEFAULT PRIVILEGES FOR ROLE opendesk IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_kyc;
ALTER DEFAULT PRIVILEGES FOR ROLE opendesk IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO app_kyc;
-- W51-C: kyc-service applies its verification-audit schema at boot on the
-- runtime DSN (app_kyc_login owns the kyc_audit tables).
ALTER DEFAULT PRIVILEGES FOR ROLE app_kyc_login IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_kyc;
ALTER DEFAULT PRIVILEGES FOR ROLE app_kyc_login IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO app_kyc;

-- ---------------------------------------------------------------------------
-- Wave 43 (SPEC-W43 G-01, SEC#4/DATA#3): identity / notifications / crm-sync
-- roles. Same NOLOGIN group + LOGIN member pattern as above; the group holds
-- the grants and services connect with the LOGIN variant so FORCE ROW LEVEL
-- SECURITY actually binds them (the opendesk superuser bypasses RLS).
--
-- The *_internal roles are NOLOGIN NOINHERIT cross-tenant escape groups
-- (post-GF1 idiom from 30-model-registry.sql / billing 0002_rls.sql):
-- privilege is reached ONLY through membership, and RLS policies gate
-- internal access on pg_has_role(current_user, '<role>', ...) — a role
-- property no request-scoped GUC can forge. Services add their own LOGIN
-- member in their bootstrap migrations when they need one (billing 0002
-- idiom: app_billing_internal_login).
--
-- TEMPORARY BOOTSTRAP EXCEPTION (documented, SPEC-W43 G-01): app_identity,
-- app_notifications and app_crm_sync keep CREATE ON SCHEMA public because
-- those services still apply bootstrap DDL at startup (identity store.go
-- ALTERs; notification-worker webhook/DND schema; crm-sync sync_map).
-- 08-code-bootstrap-parity.sql (G-07) is phase-1 of moving that DDL into
-- the infra layer; once a service's ensure_* path is verify-only (phase-2),
-- revoke with: REVOKE CREATE ON SCHEMA public FROM <role>.
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_identity') THEN
        CREATE ROLE app_identity NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_identity_login') THEN
        CREATE ROLE app_identity_login LOGIN PASSWORD 'app_identity_dev_password' IN ROLE app_identity;
    END IF;
    -- Cross-tenant internal escape group (SPEC-W43 I-03): tenants-table
    -- writes (createTenant twin provisioning) need it — the tenants RLS
    -- policy is keyed on app.tenant_id, which cannot pre-exist for a tenant
    -- being created.
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_identity_internal') THEN
        CREATE ROLE app_identity_internal NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_notifications') THEN
        CREATE ROLE app_notifications NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_notifications_login') THEN
        CREATE ROLE app_notifications_login LOGIN PASSWORD 'app_notifications_dev_password' IN ROLE app_notifications;
    END IF;
    -- Cross-tenant internal escape group (SPEC-W43 N-08): notification
    -- internal jobs (workflow-driven sends, civic escalation) enumerate rows
    -- across tenants; policies gate on this role, billing 0002 idiom.
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_notifications_internal') THEN
        CREATE ROLE app_notifications_internal NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_crm_sync') THEN
        CREATE ROLE app_crm_sync NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_crm_sync_login') THEN
        CREATE ROLE app_crm_sync_login LOGIN PASSWORD 'app_crm_sync_dev_password' IN ROLE app_crm_sync;
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- identity database
-- The \gset/\if guard lets this script apply on clusters where the DB does
-- not exist yet (tests/restore-drill's fresh instance B recreates only the
-- schema-bearing DBs before the role layer; the guarded section is then a
-- logged skip). Docker init and restore.sh always have the DB present.
-- ---------------------------------------------------------------------------
SELECT EXISTS (SELECT FROM pg_database WHERE datname = 'identity') AS identity_db_exists\gset
\if :identity_db_exists
\c identity

GRANT CONNECT ON DATABASE identity TO app_identity;
GRANT CONNECT ON DATABASE identity TO app_identity_internal;
GRANT USAGE ON SCHEMA public TO app_identity;
GRANT USAGE ON SCHEMA public TO app_identity_internal;
-- TEMPORARY bootstrap exception (see header note above): identity-service
-- still applies DDL at startup. Revoke once ensure_* is verify-only.
GRANT CREATE ON SCHEMA public TO app_identity;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_identity;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_identity_internal;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_identity;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_identity_internal;
-- Future tables created by the bootstrap superuser stay reachable. Docker's
-- bootstrap role is `opendesk`; the embedded test Postgres (pgserver, used
-- by tests/restore-drill) bootstraps as `postgres` — cover whichever exists
-- (billing 0002_rls.sql idiom, generalized). W51-C: identity-service also
-- applies startup DDL on its runtime DSN, so include the migration-owner
-- role app_identity_login in the same loop.
DO $$
DECLARE
    bootstrap TEXT;
BEGIN
    FOREACH bootstrap IN ARRAY ARRAY['opendesk', 'postgres', 'app_identity_login'] LOOP
        IF EXISTS (SELECT FROM pg_roles WHERE rolname = bootstrap) THEN
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_identity', bootstrap);
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT USAGE, SELECT ON SEQUENCES TO app_identity', bootstrap);
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_identity_internal', bootstrap);
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT USAGE, SELECT ON SEQUENCES TO app_identity_internal', bootstrap);
        END IF;
    END LOOP;
END
$$;
\else
\echo '05-app-roles: database identity absent — skipping its grants section'
\endif

-- ---------------------------------------------------------------------------
-- notifications database (schema is bootstrapped by notification-worker at
-- startup — Wave 5 #10 — so on a fresh volume ALL TABLES below is a no-op
-- and the default privileges + CREATE grant are what matter)
-- ---------------------------------------------------------------------------
SELECT EXISTS (SELECT FROM pg_database WHERE datname = 'notifications') AS notifications_db_exists\gset
\if :notifications_db_exists
\c notifications

GRANT CONNECT ON DATABASE notifications TO app_notifications;
GRANT CONNECT ON DATABASE notifications TO app_notifications_internal;
GRANT USAGE ON SCHEMA public TO app_notifications;
GRANT USAGE ON SCHEMA public TO app_notifications_internal;
-- TEMPORARY bootstrap exception (see header note above): notification-worker
-- creates webhook_subscriptions / webhook_deliveries / dnd / civic_ledger at
-- startup. Revoke once its ensure_* path is verify-only.
GRANT CREATE ON SCHEMA public TO app_notifications;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_notifications;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_notifications_internal;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_notifications;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_notifications_internal;
-- W51-C: notification-worker bootstraps its schema at startup on the
-- runtime DSN — include the migration-owner role app_notifications_login
-- alongside the bootstrap superusers.
DO $$
DECLARE
    bootstrap TEXT;
BEGIN
    FOREACH bootstrap IN ARRAY ARRAY['opendesk', 'postgres', 'app_notifications_login'] LOOP
        IF EXISTS (SELECT FROM pg_roles WHERE rolname = bootstrap) THEN
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_notifications', bootstrap);
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT USAGE, SELECT ON SEQUENCES TO app_notifications', bootstrap);
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_notifications_internal', bootstrap);
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT USAGE, SELECT ON SEQUENCES TO app_notifications_internal', bootstrap);
        END IF;
    END LOOP;
END
$$;
\else
\echo '05-app-roles: database notifications absent — skipping its grants section'
\endif

-- ---------------------------------------------------------------------------
-- crm_sync database (sync_map is created by crm-sync-service at startup —
-- same fresh-volume caveat as notifications above)
-- ---------------------------------------------------------------------------
SELECT EXISTS (SELECT FROM pg_database WHERE datname = 'crm_sync') AS crm_sync_db_exists\gset
\if :crm_sync_db_exists
\c crm_sync

GRANT CONNECT ON DATABASE crm_sync TO app_crm_sync;
GRANT USAGE ON SCHEMA public TO app_crm_sync;
-- TEMPORARY bootstrap exception (see header note above): crm-sync-service
-- creates sync_map at startup. Revoke once its bootstrap is verify-only.
GRANT CREATE ON SCHEMA public TO app_crm_sync;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_crm_sync;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_crm_sync;
-- W51-C: crm-sync-service creates sync_map at startup on the runtime DSN —
-- include the migration-owner role app_crm_sync_login alongside the
-- bootstrap superusers.
DO $$
DECLARE
    bootstrap TEXT;
BEGIN
    FOREACH bootstrap IN ARRAY ARRAY['opendesk', 'postgres', 'app_crm_sync_login'] LOOP
        IF EXISTS (SELECT FROM pg_roles WHERE rolname = bootstrap) THEN
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_crm_sync', bootstrap);
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT USAGE, SELECT ON SEQUENCES TO app_crm_sync', bootstrap);
        END IF;
    END LOOP;
END
$$;
\else
\echo '05-app-roles: database crm_sync absent — skipping its grants section'
\endif

-- ---------------------------------------------------------------------------
-- W47 (SPEC-W47 / A-DB DB-01 CRIT): payments-service roles.
-- Same NOLOGIN group + LOGIN member pattern as above. W47-D's
-- services/payments-service/migrations/0001_rls.sql creates the SAME four
-- roles (idempotently, pg_roles-guarded) so the migration also applies
-- standalone on a fresh cluster — passwords here MUST stay in sync with it.
--
--   app_payments(_login)              — runtime DSN role (PAYMENTS_DATABASE_URL)
--   app_payments_internal(_login)     — cross-tenant escape group for the
--                                       payout reconciler / approval lookups
--                                       (PAYMENTS_INTERNAL_DATABASE_URL); the
--                                       RLS policies gate on
--                                       pg_has_role(current_user,
--                                       'app_payments_internal', 'member') —
--                                       a role property no GUC can forge.
--
-- BOOTSTRAP EXCEPTION (documented): app_payments keeps CREATE ON SCHEMA
-- public because payments-service has no separate migration runner — its
-- schema bootstrap (CREATE TABLE IF NOT EXISTS) runs at boot on the runtime
-- DSN. Revoke once a migration-runner split lands.
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
-- payments database (schema is bootstrapped by payments-service at startup —
-- same fresh-volume caveat as notifications above: ALL TABLES is a no-op on
-- first init and the default privileges + CREATE grant are what matter)
-- ---------------------------------------------------------------------------
SELECT EXISTS (SELECT FROM pg_database WHERE datname = 'payments') AS payments_db_exists\gset
\if :payments_db_exists
\c payments

GRANT CONNECT ON DATABASE payments TO app_payments;
GRANT CONNECT ON DATABASE payments TO app_payments_internal;
GRANT USAGE ON SCHEMA public TO app_payments;
GRANT USAGE ON SCHEMA public TO app_payments_internal;
-- TEMPORARY bootstrap exception (see header note above): payments-service
-- creates its tables (payout_beneficiaries, deposit_provenance,
-- payout_attempts, rail_attempts) at startup on the runtime DSN.
GRANT CREATE ON SCHEMA public TO app_payments;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_payments;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_payments_internal;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_payments;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_payments_internal;
-- W51-C: payments-service bootstraps its schema at startup on the runtime
-- DSN — include the migration-owner role app_payments_login alongside the
-- bootstrap superusers (its own 0001_rls.sql covers only opendesk/postgres).
-- This is what lets the app_payments_internal escape group (payout
-- reconciler) reach tables the service creates at boot.
DO $$
DECLARE
    bootstrap TEXT;
BEGIN
    FOREACH bootstrap IN ARRAY ARRAY['opendesk', 'postgres', 'app_payments_login'] LOOP
        IF EXISTS (SELECT FROM pg_roles WHERE rolname = bootstrap) THEN
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
\else
\echo '05-app-roles: database payments absent — skipping its grants section'
\endif

-- ---------------------------------------------------------------------------
-- Wave 38 (SPEC-W38 F1/F3): agents registry + capture tables
-- ---------------------------------------------------------------------------
-- Explicit grants on the three W38 tables (created by
-- 07-agents-capture-schema.sql). NOTE: on a FRESH init this script (05) runs
-- BEFORE 07, so the table-existence guards below no-op here and the guarded
-- GRANTs at the bottom of 07 itself apply. This block covers deployments
-- where the tables already exist when 05 is (re-)run — same privilege set
-- as the conversation section above. ALTER DEFAULT PRIVILEGES above already
-- covers future tables; the explicit grants keep the intent readable.
-- ---------------------------------------------------------------------------
\c conversation

DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_conversation') THEN
        IF to_regclass('public.agents') IS NOT NULL THEN
            EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.agents TO app_conversation';
        END IF;
        IF to_regclass('public.capture_schemas') IS NOT NULL THEN
            EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.capture_schemas TO app_conversation';
        END IF;
        IF to_regclass('public.capture_records') IS NOT NULL THEN
            EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.capture_records TO app_conversation';
        END IF;
    END IF;
END
$$;
