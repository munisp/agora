-- 01-booking-schema.sql — booking DB schema (SPEC §7) with tenant RLS.
\c booking

CREATE EXTENSION IF NOT EXISTS pgcrypto;
-- W46-I (P-DATA DDL #11): pg_trgm for leading-wildcard ILIKE search
-- (contacts.name below; tickets.subject / workorders.title are
-- service-bootstrap-owned tables — coder A adds those in the booking
-- migration + ensureDDL, do NOT duplicate here).
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Catalog of bookable offerings.
CREATE TABLE offerings (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
    name         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    duration_min INTEGER NOT NULL CHECK (duration_min > 0),
    buffer_min   INTEGER NOT NULL DEFAULT 0 CHECK (buffer_min >= 0),
    price_cents  INTEGER NOT NULL DEFAULT 0 CHECK (price_cents >= 0),
    currency     CHAR(3) NOT NULL DEFAULT 'USD',
    capacity     INTEGER NOT NULL DEFAULT 1 CHECK (capacity > 0),
    bookable     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_offerings_tenant ON offerings (tenant_id) WHERE bookable;

-- Team members who can take bookings.
-- SPEC-W45 CODER-M (STK O14 completion, single-column exception): nullable
-- user_id soft-links a member to an identity-service user. No FK —
-- identity owns users and booking does not existence-check the link.
-- EXISTING DATABASES are migrated by the booking-service bootstrap DDL
-- (ensureTeamMemberUserIDColumn, ALTER ... ADD COLUMN IF NOT EXISTS).
CREATE TABLE team_members (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name      TEXT NOT NULL,
    email     TEXT,
    role      TEXT NOT NULL DEFAULT 'staff',
    active    BOOLEAN NOT NULL DEFAULT TRUE,
    user_id   UUID
);
CREATE INDEX idx_team_members_tenant ON team_members (tenant_id) WHERE active;

-- Weekly availability windows per team member (minutes from midnight).
CREATE TABLE availability_rules (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL,
    team_member_id UUID NOT NULL REFERENCES team_members (id) ON DELETE CASCADE,
    weekday        SMALLINT NOT NULL CHECK (weekday BETWEEN 0 AND 6),
    start_min      SMALLINT NOT NULL CHECK (start_min BETWEEN 0 AND 1439),
    end_min        SMALLINT NOT NULL CHECK (end_min BETWEEN 1 AND 1440),
    effective_from DATE NOT NULL DEFAULT CURRENT_DATE,
    effective_to   DATE,
    CHECK (end_min > start_min)
);
-- Availability lookup by team member (and tenant for RLS-friendly plans).
CREATE INDEX idx_availability_team_member ON availability_rules (tenant_id, team_member_id, weekday);

-- CRM-lite contacts.
CREATE TABLE contacts (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name      TEXT NOT NULL,
    phone     TEXT,
    email     TEXT,
    notes     TEXT NOT NULL DEFAULT ''
);
-- SPEC-W45 CODER-A item 5: one contact per (tenant, phone) — the booking
-- write path matches-or-creates on this key instead of duplicating people
-- across channels. Partial: phone-less rows are exempt (NULL/'' handled by
-- the store). EXISTING DATABASES: this init script runs only on fresh
-- installs; existing deployments are migrated idempotently by the
-- store.ensureContactDedupe bootstrap (folds duplicate rows, re-points
-- bookings/loan references, then creates this same index).
CREATE UNIQUE INDEX uq_contacts_tenant_phone ON contacts (tenant_id, phone) WHERE phone IS NOT NULL AND phone <> '';
-- W46-I (P-DATA DDL #11, contacts part): GIN trgm index — crm360 contact
-- search uses ILIKE '%...%' (crm360/store.go:348) which defeats B-trees.
-- Plain CREATE INDEX (no CONCURRENTLY): init-db scripts must not use
-- CONCURRENTLY. EXISTING deployments: converged by coder A's migration +
-- ensureDDL mirror (W46-A), same pattern as uq_contacts_tenant_phone above.
CREATE INDEX IF NOT EXISTS idx_contacts_name_trgm ON contacts USING gin (name gin_trgm_ops);

-- Bookings. idempotency_key makes command retries safe.
CREATE TABLE bookings (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL,
    offering_id     UUID NOT NULL REFERENCES offerings (id),
    team_member_id  UUID REFERENCES team_members (id),
    contact_id      UUID REFERENCES contacts (id),
    starts_at       TIMESTAMPTZ NOT NULL,
    ends_at         TIMESTAMPTZ NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','confirmed','rescheduled','cancelled','no_show','completed')),
    source          TEXT NOT NULL DEFAULT 'voice'
                    CHECK (source IN ('voice','chat','web','api')),
    idempotency_key TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);
-- Bookings by tenant + starts_at (calendar/agenda queries).
CREATE INDEX idx_bookings_tenant_starts_at ON bookings (tenant_id, starts_at);
CREATE INDEX idx_bookings_tenant_status ON bookings (tenant_id, status);
-- Idempotency: one command result per key PER TENANT (SPEC-W43 G-03 / C5:
-- keys are only unique within a tenant; the store inserts with
-- NULLIF($key,'') so empty strings land as NULL and are never deduplicated).
CREATE UNIQUE INDEX uq_bookings_idempotency_key ON bookings (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
-- W46-I (P-DATA DDL #1/#2/#3): hot-path booking indexes. The bookings
-- CREATE TABLE lives HERE (init-scripts-owned), so fresh clusters get the
-- indexes at bootstrap; coder A (W46-A) owns the migration + ensureDDL
-- mirror that converges EXISTING deployments. Plain CREATE INDEX
-- IF NOT EXISTS — CONCURRENTLY is not allowed in init-db transactions.
--   #1 availability engine + slot-conflict recheck (bookings.go:140,316,
--      waitlist.go:275): (tenant, member, starts), cancelled rows excluded.
CREATE INDEX IF NOT EXISTS idx_bookings_tenant_member_starts
    ON bookings (tenant_id, team_member_id, starts_at) WHERE status <> 'cancelled';
--   #2 CRM-360 per-contact history + lending signals (crm360/store.go:505,
--      :647; lending/store_queries.go:155,169): contact_id was an
--      unindexed FK.
CREATE INDEX IF NOT EXISTS idx_bookings_tenant_contact_starts
    ON bookings (tenant_id, contact_id, starts_at DESC);
--   #3 stale-pending sweeper is CROSS-TENANT (bookings.go:500), so the
--      tenant-leading indexes above cannot serve it.
CREATE INDEX IF NOT EXISTS idx_bookings_pending_created
    ON bookings (created_at) WHERE status = 'pending';

-- Transactional outbox (drained to Kafka opendesk.booking.events).
CREATE TABLE outbox (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic        TEXT NOT NULL,
    payload      JSONB NOT NULL,
    sent_at      TIMESTAMPTZ
);
CREATE INDEX idx_outbox_unsent ON outbox (id) WHERE sent_at IS NULL;

-- ---------------- Row Level Security (SPEC §7) ----------------
-- Every tenant table: enabled + forced; app sets app.tenant_id per request.
ALTER TABLE offerings ENABLE ROW LEVEL SECURITY;
ALTER TABLE offerings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON offerings
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE team_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE team_members FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON team_members
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE availability_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE availability_rules FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON availability_rules
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE contacts ENABLE ROW LEVEL SECURITY;
ALTER TABLE contacts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON contacts
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE bookings ENABLE ROW LEVEL SECURITY;
ALTER TABLE bookings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON bookings
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
