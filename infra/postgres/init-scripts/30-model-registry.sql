-- 30-model-registry.sql — model-registry service schema (SPEC-W33 §4 C1/C2/C3/C5).
--
-- DEVIATION FROM SPEC (documented in services/model-registry/README.md):
-- SPEC-W33 §4 C1 names this migration `V30__model_registry.sql` (Flyway-style),
-- but the repo convention is `infra/postgres/init-scripts/NN-name.sql` executed
-- by the postgres docker-entrypoint-initdb.d psql path (see 00..06). We follow
-- the repo convention; there is no Flyway in this stack.
--
-- Layout follows the existing scripts: cluster-global roles first (same pattern
-- as 05-app-roles.sql), then `CREATE DATABASE platform` (SELECT-gated
-- \gexec since SPEC-W43 G-06 — idempotent under replay), then `\c platform`
-- and the schema with FORCE ROW LEVEL SECURITY exactly like 03/04.
--
-- RLS convention (matches 01/03/04): tenant rows are visible/writable only when
--   tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
-- The service sets `app.tenant_id` via set_config(..., true) inside every
-- transaction (single write path, I4). ONE deliberate extension: the service's
-- internal batch jobs (drift sweep, nightly trainer) must enumerate rows across
-- tenants. SPEC-W34 GF1: that used to be a second GUC
-- `app.registry_internal = 'on'`, but ANY session as the login role could set
-- it (set_config needs no privilege) and read/UPDATE/DELETE all tenants.
-- Internal access is now gated on ROLE MEMBERSHIP instead:
--   pg_has_role(current_user, 'app_model_registry_internal', 'USAGE')
-- — a property of the connected role that no GUC can forge. Internal jobs
-- connect as `app_model_registry_batch` (MODEL_REGISTRY_INTERNAL_DSN); HTTP
-- request paths keep using `app_model_registry_login`, which is NOT a member,
-- so the policy is fail-closed for them when `app.tenant_id` is unset.

-- ---------------------------------------------------------------------------
-- Roles (cluster-wide; same NOLOGIN group + LOGIN member pattern as 05)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_model_registry') THEN
        CREATE ROLE app_model_registry NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_model_registry_login') THEN
        CREATE ROLE app_model_registry_login LOGIN PASSWORD 'app_model_registry_dev_password' IN ROLE app_model_registry;
    END IF;
    -- SPEC-W34 GF1: cross-tenant internal access group. NOLOGIN — privilege is
    -- only ever reached through membership, never by connecting directly.
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_model_registry_internal') THEN
        CREATE ROLE app_model_registry_internal NOLOGIN NOINHERIT;
    END IF;
    -- Batch/internal LOGIN role (drift sweep, nightly trainer). DEV PASSWORD,
    -- dev-compose only — rotate via secrets in production (docs/runbooks/secrets.md).
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_model_registry_batch') THEN
        CREATE ROLE app_model_registry_batch LOGIN PASSWORD 'app_model_registry_batch_dev_password' IN ROLE app_model_registry_internal;
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- platform database (ML platform catalog; SPEC-W33 §4 C1 "platform DB")
-- SPEC-W43 G-06 (DATA#17): SELECT-gated \gexec so re-applying this script
-- (restore drills, manual replays against an existing cluster) is idempotent
-- instead of erroring with 42P04 duplicate_database. Requires the psql path
-- (docker-entrypoint-initdb.d *.sql / drill harness) for \gexec.
-- ---------------------------------------------------------------------------
SELECT 'CREATE DATABASE platform'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'platform')\gexec

\c platform

-- gen_random_uuid() is core since Postgres 13, so pgcrypto is optional;
-- the embedded test Postgres (pgserver) ships no contrib extensions.
DO $$
BEGIN
    CREATE EXTENSION IF NOT EXISTS pgcrypto;
EXCEPTION WHEN OTHERS THEN
    RAISE NOTICE 'pgcrypto unavailable; relying on core gen_random_uuid()';
END
$$;

-- ---------------------------------------------------------------------------
-- Model families — global catalog (NOT tenant-scoped: family names like
-- 'fraud-clf', 'credit-clf', 'graphsage' are platform-level vocabulary).
-- ---------------------------------------------------------------------------
CREATE TABLE model_family (
    name       TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Model versions — one row per trained artifact (SPEC-W33 §4 C1).
-- stage transitions: staging → production → archived.
-- Provenance (I2): seed + dataset_hash + git_sha are recorded where knowable;
-- metrics jsonb is the honest-metrics carrier (synthetic labeled synthetic, I3).
-- ---------------------------------------------------------------------------
CREATE TABLE model_version (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family       TEXT NOT NULL REFERENCES model_family (name),
    tenant_id    UUID NOT NULL,
    version      INTEGER NOT NULL,
    artifact_uri TEXT NOT NULL,
    stage        TEXT NOT NULL DEFAULT 'staging'
                 CHECK (stage IN ('staging', 'production', 'archived')),
    metrics      JSONB NOT NULL DEFAULT '{}'::jsonb,
    seed         BIGINT,
    dataset_hash TEXT,
    git_sha      TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (family, tenant_id, version)
);

-- Single production per (family, tenant), enforced by the database even under
-- concurrent promotes (SPEC-W33 §4 C1, GC1).
CREATE UNIQUE INDEX model_version_single_production
    ON model_version (family, tenant_id)
    WHERE stage = 'production';

CREATE INDEX idx_model_version_lookup
    ON model_version (family, tenant_id, stage, created_at DESC);

-- ---------------------------------------------------------------------------
-- A/B experiments (SPEC-W33 §4 C3).
-- pct = percentage bucketed to challenger; bucketing is deterministic
-- (sha256(tenant|person|experiment) % 100 < pct). Promotion of a winner is
-- MANUAL via /v1/registry/promote — no auto-promotion column exists on purpose.
-- ---------------------------------------------------------------------------
CREATE TABLE experiments (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family             TEXT NOT NULL REFERENCES model_family (name),
    tenant_id          UUID NOT NULL,
    champion_version   INTEGER NOT NULL,
    challenger_version INTEGER NOT NULL,
    pct                INTEGER NOT NULL CHECK (pct BETWEEN 0 AND 100),
    status             TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'stopped')),
    starts_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    ends_at            TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (family, tenant_id, champion_version, challenger_version)
);

CREATE INDEX idx_experiments_active
    ON experiments (family, tenant_id, status, starts_at);

-- Labeled outcomes per assignment; report metrics are computed over rows with
-- non-null true_label (pure SQL, no sklearn — I5).
CREATE TABLE experiment_outcomes (
    id              BIGSERIAL PRIMARY KEY,
    experiment_id   UUID NOT NULL REFERENCES experiments (id) ON DELETE CASCADE,
    tenant_id       UUID NOT NULL,
    person_id       TEXT NOT NULL,
    assigned_arm    TEXT NOT NULL CHECK (assigned_arm IN ('champion', 'challenger')),
    predicted_label INTEGER NOT NULL CHECK (predicted_label IN (0, 1)),
    predicted_score DOUBLE PRECISION NOT NULL CHECK (predicted_score BETWEEN 0.0 AND 1.0),
    true_label      INTEGER CHECK (true_label IN (0, 1)),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_experiment_outcomes_exp
    ON experiment_outcomes (experiment_id, assigned_arm);

-- ---------------------------------------------------------------------------
-- Observation sinks for drift monitoring (SPEC-W33 §4 C2). Consumers push
-- serving-time feature values and scores through the service's single write
-- path; the 15-minute drift sweep compares them against the training-snapshot
-- reference manifest and the trailing 7-day score baseline.
-- ---------------------------------------------------------------------------
CREATE TABLE feature_observations (
    id          BIGSERIAL PRIMARY KEY,
    family      TEXT NOT NULL,
    tenant_id   UUID NOT NULL,
    feature     TEXT NOT NULL,
    value       DOUBLE PRECISION NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_feature_observations_window
    ON feature_observations (family, tenant_id, feature, observed_at);

CREATE TABLE score_observations (
    id          BIGSERIAL PRIMARY KEY,
    family      TEXT NOT NULL,
    tenant_id   UUID NOT NULL,
    score       DOUBLE PRECISION NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_score_observations_window
    ON score_observations (family, tenant_id, observed_at);

-- ---------------------------------------------------------------------------
-- Row Level Security (SPEC §7 convention, I4)
-- ---------------------------------------------------------------------------
ALTER TABLE model_version ENABLE ROW LEVEL SECURITY;
ALTER TABLE model_version FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON model_version
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_model_registry_internal', 'USAGE'))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_model_registry_internal', 'USAGE'));

ALTER TABLE experiments ENABLE ROW LEVEL SECURITY;
ALTER TABLE experiments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON experiments
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_model_registry_internal', 'USAGE'))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_model_registry_internal', 'USAGE'));

ALTER TABLE experiment_outcomes ENABLE ROW LEVEL SECURITY;
ALTER TABLE experiment_outcomes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON experiment_outcomes
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_model_registry_internal', 'USAGE'))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_model_registry_internal', 'USAGE'));

ALTER TABLE feature_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE feature_observations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON feature_observations
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_model_registry_internal', 'USAGE'))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_model_registry_internal', 'USAGE'));

ALTER TABLE score_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE score_observations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON score_observations
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_model_registry_internal', 'USAGE'))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_model_registry_internal', 'USAGE'));

-- ---------------------------------------------------------------------------
-- Grants to the service role (05-app-roles.sql pattern)
-- ---------------------------------------------------------------------------
GRANT CONNECT ON DATABASE platform TO app_model_registry;
GRANT USAGE ON SCHEMA public TO app_model_registry;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_model_registry;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_model_registry;
-- SPEC-W34 GF1: the internal group needs the same table reach — the RLS
-- policies gate on membership, but membership alone grants no privileges.
GRANT CONNECT ON DATABASE platform TO app_model_registry_internal;
GRANT USAGE ON SCHEMA public TO app_model_registry_internal;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_model_registry_internal;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_model_registry_internal;
-- Default privileges reference the bootstrap superuser `opendesk`, which
-- exists in the docker Postgres image but NOT in the embedded test Postgres
-- (pgserver, bootstrap user `postgres`) — hence the existence guard so this
-- script applies verbatim in both.
DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'opendesk') THEN
        EXECUTE 'ALTER DEFAULT PRIVILEGES FOR ROLE opendesk IN SCHEMA public '
                'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_model_registry';
        EXECUTE 'ALTER DEFAULT PRIVILEGES FOR ROLE opendesk IN SCHEMA public '
                'GRANT USAGE, SELECT ON SEQUENCES TO app_model_registry';
        EXECUTE 'ALTER DEFAULT PRIVILEGES FOR ROLE opendesk IN SCHEMA public '
                'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_model_registry_internal';
        EXECUTE 'ALTER DEFAULT PRIVILEGES FOR ROLE opendesk IN SCHEMA public '
                'GRANT USAGE, SELECT ON SEQUENCES TO app_model_registry_internal';
    END IF;
END
$$;
