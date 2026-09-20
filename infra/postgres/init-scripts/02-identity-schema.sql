-- 02-identity-schema.sql — identity DB schema (SPEC §7) with tenant RLS.
\c identity

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Tenants (a.k.a. organizations). Slug drives the public booking page
-- /p/{siteSlug} and Keycloak group mapping /tenants/{slug}.
CREATE TABLE tenants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    timezone    TEXT NOT NULL DEFAULT 'UTC',
    currency    CHAR(3) NOT NULL DEFAULT 'USD',
    locale      TEXT NOT NULL DEFAULT 'en-US',
    terminology JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- SPEC-CRM §C1: industry workflow pack id (salon|clinic|consultancy|support-desk).
    -- Existing installs get this column via the identity-service bootstrap ALTER.
    industry    TEXT NOT NULL DEFAULT 'salon',
    -- 'twin' is the internal digital-twin plan (SPEC-W3 §3 innovation 12):
    -- set ONLY by identity-service createTwin; it is NOT in the public
    -- POST /v1/tenants plan set (httpapi validPlans). Existing installs get
    -- the widened CHECK via the identity-service bootstrap constraint rewrite.
    plan        TEXT NOT NULL DEFAULT 'free'
                CHECK (plan IN ('free','pro','enterprise','twin')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- User <-> tenant memberships with realm role mirror (owner|admin|staff|viewer).
CREATE TABLE memberships (
    tenant_id UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id   UUID NOT NULL,
    role      TEXT NOT NULL DEFAULT 'staff'
              CHECK (role IN ('owner','admin','staff','viewer')),
    PRIMARY KEY (tenant_id, user_id)
);
CREATE INDEX idx_memberships_user ON memberships (user_id);

-- SPEC-W45 K17: tenant API keys for programmatic access (validated via
-- identity-service /internal/api-keys/validate). Only the SHA-256 hash of
-- the full key ("<prefix>.<secret>") is stored; the secret is returned once
-- at creation. Revocation is a soft delete (revoked_at) so usage stays
-- auditable. Existing installs get this table via the identity-service
-- bootstrap CREATE IF NOT EXISTS.
CREATE TABLE tenant_api_keys (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    prefix     TEXT NOT NULL,
    key_hash   TEXT NOT NULL UNIQUE,
    scopes     TEXT[] NOT NULL DEFAULT '{}',
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);
CREATE INDEX idx_tenant_api_keys_tenant ON tenant_api_keys (tenant_id);
CREATE INDEX idx_tenant_api_keys_hash ON tenant_api_keys (key_hash) WHERE revoked_at IS NULL;

-- ---------------- Row Level Security (SPEC §7) ----------------
-- tenants IS the tenant table: its tenant_id is its own id.
ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenants
    USING (id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON memberships
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE tenant_api_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_api_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant_api_keys
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
