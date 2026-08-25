-- 08-code-bootstrap-parity.sql — SPEC-W43 G-07 (DATA#6 phase-1).
--
-- Moves the DDL that service boot code has been owning at runtime into the
-- infra init layer, so a FRESH cluster reaches the same final shape without
-- needing service-side CREATE/ALTER privileges (least-privilege lockdown,
-- G-01). Every statement is idempotent (IF NOT EXISTS / DROP+ADD), so the
-- services' ensure_* calls keep working as no-ops against this shape and
-- older deployments still converge via the service path.
--
-- docker-entrypoint order: after 07-agents-capture-schema.sql, before
-- 30-model-registry.sql.
--
-- Shapes below are copied EXACTLY from the service bootstrap code so code
-- and infra agree byte-for-byte on the final state:
--   * services/identity-service/internal/store/store.go   (tenants.metadata)
--   * services/conversation-service/app/db.py             (ensure_contact_column,
--                                                          ensure_turn_idempotency,
--                                                          ensure_ussd_channel)

-- ---------------------------------------------------------------------------
-- identity DB: tenants.metadata (SPEC-W3 §3 innovation 12 — free-form tenant
-- metadata for digital twins; store.go:55-59 owns this ALTER today).
-- ---------------------------------------------------------------------------
\c identity

ALTER TABLE tenants ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';

-- ---------------------------------------------------------------------------
-- conversation DB: GDPR contact marker + turn idempotency (db.py
-- ensure_contact_column / ensure_turn_idempotency).
-- ---------------------------------------------------------------------------
\c conversation

ALTER TABLE conversations ADD COLUMN IF NOT EXISTS contact_phone TEXT;
CREATE INDEX IF NOT EXISTS idx_conversations_contact_phone
    ON conversations (tenant_id, contact_phone);

-- SPEC-W3 §3: nullable idempotency key on turns; the partial unique index on
-- (conversation_id, idempotency_key) makes concurrent same-key appends safe
-- (loser gets UniqueViolation and re-reads the winner's row). NULL keys are
-- never deduplicated.
ALTER TABLE turns ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS uq_turns_idempotency_key
    ON turns (conversation_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- SPEC-W12 contract §2: 'ussd' joins the conversations.channel enum. Matches
-- ensure_ussd_channel's final shape exactly (drop + re-add, NOT VALID keeps
-- existing rows out of the scan while new inserts are validated). The
-- DROP+ADD pair is what makes this replayable under a single constraint name.
ALTER TABLE conversations
    DROP CONSTRAINT IF EXISTS conversations_channel_check;
ALTER TABLE conversations
    ADD CONSTRAINT conversations_channel_check
    CHECK (channel IN ('voice','chat','phone','api','ussd')) NOT VALID;
