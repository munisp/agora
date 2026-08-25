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
