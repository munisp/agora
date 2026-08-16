-- 03-conversation-schema.sql — conversation DB schema (SPEC §7) with tenant RLS.
\c conversation

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- A conversation session (voice call, web chat, telephony ingestion).
CREATE TABLE conversations (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL,
    site_slug  TEXT NOT NULL,
    channel    TEXT NOT NULL DEFAULT 'voice'
               CHECK (channel IN ('voice','chat','phone','api')),
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at   TIMESTAMPTZ
);
CREATE INDEX idx_conversations_tenant_started ON conversations (tenant_id, started_at DESC);
CREATE INDEX idx_conversations_site ON conversations (tenant_id, site_slug);

-- Ordered turns within a conversation.
CREATE TABLE turns (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    seq             INTEGER NOT NULL,
    role            TEXT NOT NULL CHECK (role IN ('user','agent','system','tool')),
    text            TEXT NOT NULL,
    tool_calls      JSONB,
    ts              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, seq)
);

-- Call-intelligence enrichment (SPEC-W3 §4, innovation 3). Idempotent; the
-- service also runs these ALTERs on startup for existing deployments.
ALTER TABLE turns ADD COLUMN IF NOT EXISTS sentiment DOUBLE PRECISION;
ALTER TABLE turns ADD COLUMN IF NOT EXISTS intent TEXT;
ALTER TABLE turns ADD COLUMN IF NOT EXISTS entities JSONB;

-- ---------------- Row Level Security (SPEC §7) ----------------
ALTER TABLE conversations ENABLE ROW LEVEL SECURITY;
ALTER TABLE conversations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON conversations
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- turns carries no tenant_id per SPEC §7; isolate through the parent row.
ALTER TABLE turns ENABLE ROW LEVEL SECURITY;
ALTER TABLE turns FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON turns
    USING (EXISTS (
        SELECT 1 FROM conversations c
        WHERE c.id = turns.conversation_id
          AND c.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    ));
