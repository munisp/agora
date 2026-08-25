-- 001_cac_schema.sql — CAC platform seed schema (SPEC-W17 contract D).
--
-- NEW schema `cac` holding platform-level synthetic reference/seed data for
-- the CAC (customer-acquisition-cost) app. It deliberately does NOT collide
-- with the tenant-scoped booking/identity RLS tables (those live in the
-- per-service databases under infra/postgres/init-scripts).
--
-- RLS: intentionally NOT enabled on cac.* — these tables carry platform-level
-- SYNTHETIC reference data only (no tenant PII). All person-identifying
-- columns (names, phones) are stored exclusively as non-reversible
-- verification digests produced by scripts/seeds/_lib.py hash_pii(); there is
-- no tenant data to isolate, so tenant RLS is N/A. Every row is stamped
-- is_synthetic=true + seeded_at so drift checks (scripts/seeds/drift.sql) and
-- the erasure fast-path (SPEC-W17 Agent D) can identify seed rows.
--
-- Idempotent: safe to re-apply (CREATE SCHEMA/TABLE IF NOT EXISTS).
-- PKs are deterministic text ids from _lib.deterministic_id(natural_key).

CREATE SCHEMA IF NOT EXISTS cac;

-- ---------------------------------------------------------------------------
-- Reference geography
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS cac.lgas (
    id           TEXT PRIMARY KEY,                -- deterministic_id('lga:<state>:<lga_name>')
    lga_name     TEXT NOT NULL,
    state        TEXT NOT NULL,
    zone         TEXT NOT NULL,                   -- geopolitical zone, e.g. 'North West'
    is_synthetic BOOLEAN NOT NULL DEFAULT TRUE,
    seeded_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lgas_zone_ck CHECK (zone IN (
        'North Central', 'North East', 'North West',
        'South East', 'South South', 'South West')),
    CONSTRAINT lgas_state_name_uq UNIQUE (state, lga_name)
);

CREATE TABLE IF NOT EXISTS cac.wards (
    id           TEXT PRIMARY KEY,                -- deterministic_id('ward:<state>:<lga>:<nn>')
    lga_id       TEXT NOT NULL REFERENCES cac.lgas (id) ON DELETE CASCADE,
    ward_name    TEXT NOT NULL,                   -- synthetic: 'Ward NN — <LGA>' (see seed_wards.py)
    is_synthetic BOOLEAN NOT NULL DEFAULT TRUE,
    seeded_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_wards_lga ON cac.wards (lga_id);

-- ---------------------------------------------------------------------------
-- Acquisition channels + unit costs
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS cac.channels (
    id             TEXT PRIMARY KEY,              -- deterministic_id('channel:<channel_code>')
    channel_code   TEXT NOT NULL UNIQUE,          -- snake_case code, e.g. 'ussd'
    name           TEXT NOT NULL,
    channel_class  TEXT NOT NULL,                 -- 'above-the-line' | 'below-the-line'
    unit_desc      TEXT NOT NULL,                 -- what one 'unit' buys, e.g. 'per 30s radio slot'
    base_cost_ngn  NUMERIC(14,2) NOT NULL,        -- typical unit cost anchor (playbook, NGN)
    notes          TEXT NOT NULL DEFAULT '',
    is_synthetic   BOOLEAN NOT NULL DEFAULT TRUE,
    seeded_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT channels_class_ck CHECK (channel_class IN ('above-the-line', 'below-the-line'))
);

CREATE TABLE IF NOT EXISTS cac.channel_unit_costs (
    id            TEXT PRIMARY KEY,               -- deterministic_id('channel_cost:<code>:<yyyy-mm>')
    channel_id    TEXT NOT NULL REFERENCES cac.channels (id) ON DELETE CASCADE,
    month         DATE NOT NULL,                  -- first day of month
    unit_cost_ngn NUMERIC(14,2) NOT NULL CHECK (unit_cost_ngn >= 0),
    currency      CHAR(3) NOT NULL DEFAULT 'NGN',
    is_synthetic  BOOLEAN NOT NULL DEFAULT TRUE,
    seeded_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT channel_costs_uq UNIQUE (channel_id, month)
);
CREATE INDEX IF NOT EXISTS idx_channel_costs_month ON cac.channel_unit_costs (month);

-- ---------------------------------------------------------------------------
-- Synthetic entities (Agent B writes these; schema owned here per contract D)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS cac.agents (
    id           TEXT PRIMARY KEY,                -- deterministic_id('agent:<n>')
    name_hash    TEXT NOT NULL,                   -- hash_pii(full name) — digest only, never plaintext
    phone_hash   TEXT NOT NULL,                   -- hash_pii(+234...) — digest only
    state        TEXT NOT NULL,
    lga_id       TEXT REFERENCES cac.lgas (id),
    active       BOOLEAN NOT NULL DEFAULT TRUE,
    is_synthetic BOOLEAN NOT NULL DEFAULT TRUE,
    seeded_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agents_lga ON cac.agents (lga_id);

CREATE TABLE IF NOT EXISTS cac.customers (
    id           TEXT PRIMARY KEY,                -- deterministic_id('customer:<n>')
    name_hash    TEXT NOT NULL,                   -- hash_pii — digest only
    phone_hash   TEXT NOT NULL,                   -- hash_pii — digest only
    channel_id   TEXT REFERENCES cac.channels (id),   -- channel of first touch
    lga_id       TEXT REFERENCES cac.lgas (id),
    acquired_on  DATE,
    is_synthetic BOOLEAN NOT NULL DEFAULT TRUE,
    seeded_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_customers_lga ON cac.customers (lga_id);
CREATE INDEX IF NOT EXISTS idx_customers_channel ON cac.customers (channel_id);

-- ---------------------------------------------------------------------------
-- FX series (Agent B's seed_fx.py writes these)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS cac.fx_series (
    id                TEXT PRIMARY KEY,           -- deterministic_id('fx:<yyyy-mm-dd>')
    series_date       DATE NOT NULL UNIQUE,
    usd_ngn_official  NUMERIC(12,4) NOT NULL,     -- official window rate
    usd_ngn_parallel  NUMERIC(12,4) NOT NULL,     -- parallel market rate (>= official)
    source            TEXT NOT NULL DEFAULT 'synthetic-walk',  -- anchored walk, not crawled (seed_fx.py)
    is_synthetic      BOOLEAN NOT NULL DEFAULT TRUE,
    seeded_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_fx_series_date ON cac.fx_series (series_date);

-- SPEC-W43 G-06 (DATA#16): the parallel market rate can never be BELOW the
-- official window rate. NOT VALID so pre-existing rows are not scanned
-- (no lock-heavy rewrite on populated installs); new writes are enforced.
-- Idempotent via pg_constraint guard (this DDL file is re-applied by
-- scripts/seeds/bootstrap.sh step 1 on every seed run).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT FROM pg_constraint
        WHERE conname = 'fx_series_parallel_gte_official'
          AND conrelid = 'cac.fx_series'::regclass
    ) THEN
        ALTER TABLE cac.fx_series
            ADD CONSTRAINT fx_series_parallel_gte_official
            CHECK (usd_ngn_parallel >= usd_ngn_official) NOT VALID;
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- Seed run audit
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS cac.seed_run_log (
    id           TEXT PRIMARY KEY,                -- deterministic_id('seed_run_log:<table_name>')
    table_name   TEXT NOT NULL UNIQUE,            -- seeded table / report name (latest run wins)
    rowcount     BIGINT NOT NULL CHECK (rowcount >= 0),
    runner_id    TEXT NOT NULL DEFAULT 'unknown',
    git_sha      TEXT NOT NULL DEFAULT 'unknown',
    ran_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    is_synthetic BOOLEAN NOT NULL DEFAULT TRUE,
    seeded_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
