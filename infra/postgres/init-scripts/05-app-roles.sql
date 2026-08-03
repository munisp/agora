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
