-- 32-chatwoot-bridge-schema.sql — chatwoot_bridge DB schema (SPEC-W118)
-- with tenant FORCE ROW LEVEL SECURITY on all three tables.
-- Mirrors the runtime idempotent bootstrap in
-- services/chatwoot-bridge/app/store.py (ensure_schema idiom): the init
-- script wins on fresh clusters; the service's DDL converges
-- existing/standalone deployments. tests/test_store_ddl.py enforces
-- byte-identical parity of the block below.
\c chatwoot_bridge

CREATE TABLE IF NOT EXISTS chatwoot_tenants (
    tenant_id        UUID PRIMARY KEY,
    account_id       BIGINT NOT NULL,
    inbox_id         BIGINT NOT NULL,
    api_access_token TEXT NOT NULL,
    webhook_token    TEXT NOT NULL UNIQUE,
    webhook_id       BIGINT,
    hmac_secret      TEXT,
    enabled          BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS chatwoot_links (
    tenant_id                UUID NOT NULL,
    conversation_id          UUID NOT NULL,
    chatwoot_conversation_id BIGINT NOT NULL,
    chatwoot_contact_id      BIGINT NOT NULL,
    contact_source_id        TEXT NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, conversation_id),
    UNIQUE (tenant_id, chatwoot_conversation_id)
);

CREATE TABLE IF NOT EXISTS chatwoot_sent_messages (
    tenant_id           UUID NOT NULL,
    source_id           TEXT NOT NULL,
    chatwoot_message_id BIGINT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, source_id)
);

ALTER TABLE chatwoot_tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE chatwoot_tenants FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON chatwoot_tenants;
CREATE POLICY tenant_isolation ON chatwoot_tenants
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE chatwoot_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE chatwoot_links FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON chatwoot_links;
CREATE POLICY tenant_isolation ON chatwoot_links
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE chatwoot_sent_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE chatwoot_sent_messages FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON chatwoot_sent_messages;
CREATE POLICY tenant_isolation ON chatwoot_sent_messages
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
