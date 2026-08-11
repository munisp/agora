-- 07-agents-capture-schema.sql — W38 (SPEC-W38 §2): agents registry
-- (agent-as-product) + the capture primitive (capture_schemas /
-- capture_records), tenant-scoped with FORCE RLS mirroring
-- 03-conversation-schema.sql. Idempotent-ish: CREATE TABLE IF NOT EXISTS;
-- policies are guarded by pg_policies existence checks.
\c conversation

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- A voice agent as a first-class tenant entity (SPEC-W38 F1). The dialed
-- E.164 number lives directly on the agent (no phone_numbers table);
-- unique per tenant, nullable (web-only agents carry no number).
CREATE TABLE IF NOT EXISTS agents (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL,
    name       TEXT NOT NULL,
    slug       TEXT NOT NULL,
    purpose    TEXT,
    phone_number TEXT,
    status     TEXT NOT NULL DEFAULT 'active'
               CHECK (status IN ('active','disabled')),
    definition JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, slug)
);
-- Phone uniqueness is per tenant (NULLs excluded so numberless agents
-- never collide); the bare phone index backs /v1/agents/resolve.
CREATE UNIQUE INDEX IF NOT EXISTS uq_agents_tenant_phone
    ON agents (tenant_id, phone_number) WHERE phone_number IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_agents_phone
    ON agents (phone_number) WHERE phone_number IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_agents_tenant_status ON agents (tenant_id, status);

-- Declarative post-call extraction schema per agent (SPEC-W38 F3).
-- schema shape: {"fields":[{"key","type":"string|number|boolean|enum",
-- "label","required","options"?}]}
CREATE TABLE IF NOT EXISTS capture_schemas (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL,
    agent_id   UUID NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    schema     JSONB NOT NULL DEFAULT '{"fields": []}'::jsonb,
    active     BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_capture_schemas_agent
    ON capture_schemas (tenant_id, agent_id);

-- One extracted record per (schema, conversation) capture pass. Carries
-- its OWN tenant_id column + direct policy per SPEC-W38 §2 (do NOT
-- isolate via a parent-EXISTS join).
CREATE TABLE IF NOT EXISTS capture_records (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL,
    capture_schema_id    UUID NOT NULL REFERENCES capture_schemas (id) ON DELETE CASCADE,
    agent_id             UUID NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    conversation_id      UUID NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    data                 JSONB NOT NULL DEFAULT '{}'::jsonb,
    extraction_confidence DOUBLE PRECISION,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_capture_records_agent
    ON capture_records (tenant_id, agent_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_capture_records_conversation
    ON capture_records (tenant_id, conversation_id);

-- ---------------- Row Level Security (mirrors 03-conversation-schema.sql) --
ALTER TABLE agents ENABLE ROW LEVEL SECURITY;
ALTER TABLE agents FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_policies
                   WHERE schemaname = 'public' AND tablename = 'agents'
                     AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON agents
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END
$$;

ALTER TABLE capture_schemas ENABLE ROW LEVEL SECURITY;
ALTER TABLE capture_schemas FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_policies
                   WHERE schemaname = 'public' AND tablename = 'capture_schemas'
                     AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON capture_schemas
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END
$$;

ALTER TABLE capture_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE capture_records FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_policies
                   WHERE schemaname = 'public' AND tablename = 'capture_records'
                     AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON capture_records
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END
$$;

-- Grants to the conversation service role (created in 05-app-roles.sql,
-- which runs BEFORE this script on a fresh init). ALTER DEFAULT PRIVILEGES
-- in 05 already covers future tables; these explicit GRANTs make the
-- privilege set self-contained and cover re-runs. Guarded so the script
-- also works standalone (role absent).
DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_conversation') THEN
        EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.agents TO app_conversation';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.capture_schemas TO app_conversation';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.capture_records TO app_conversation';
    END IF;
END
$$;
