-- 0002_gazetteer.sql — geo-service offline gazetteer (SPEC-W63 Scope A).
--
-- `geo_places` holds platform reference place names (states, LGAs, and —
-- schema/import-ready but seeded EMPTY per gate G3 — localities) in the
-- BOOKING database, next to geo_boundaries. It is the offline gazetteer the
-- field-pwa downloads as a snapshot; it carries NO geometry (name/code/
-- optional point only), so unlike 0001_geo.sql it does NOT require PostGIS.
--
-- Applied idempotently by the service at startup via
-- gazetteer::apply_gazetteer_migration (sqlx::raw_sql, the 0001_geo.sql
-- idiom — NO psql backslash commands, no CONCURRENTLY). Fail-closed: a
-- failed apply aborts the boot (never serve without the RLS policy).
--
-- RLS posture: ENABLE + FORCE + the SAME NULL-reference `tenant_isolation`
-- variant as geo_boundaries (0001_geo.sql:60-78):
--   * tenant_id IS NULL rows are PLATFORM REFERENCE data — visible to every
--     tenant (the gazetteer search/snapshot endpoints read them WITHOUT the
--     tenant GUC, the enrich.rs reference-pass idiom);
--   * the tenant-row arm of the policy is kept deliberately so the schema
--     can admit tenant-private places later without a policy change;
--   * WITH CHECK mirrors USING: any role can upsert REFERENCE rows, which
--     is why gazetteer import is gated by the Permify manage_bookings check
--     at the HTTP layer (src/auth.rs) — the database policy alone is
--     intentionally NOT the admin gate for reference data.
--
-- Codes (pinned in src/gazetteer.rs::slug — uppercase, spaces/'/' -> '-',
-- strip other punctuation): states `NG-<STATE>`, LGAs `NG-<STATE>-<LGA>`,
-- localities importer-assigned. Upsert key: UNIQUE (kind, code).
--
-- Roles/grants mirror 0001_geo.sql (pg_roles-guarded DO blocks) so this
-- file also applies standalone on a fresh cluster / plain-PG test database.

-- ---------------------------------------------------------------------------
-- Roles (cluster-wide; idempotent — mirrors 05-app-roles.sql / 0001_geo.sql)
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_geo') THEN
        CREATE ROLE app_geo NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_geo_login') THEN
        CREATE ROLE app_geo_login LOGIN PASSWORD 'app_geo_dev_password' IN ROLE app_geo;
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- geo_places: platform reference gazetteer (tenant_id NULL this wave)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS geo_places (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NULL,  -- NULL = platform reference data (world-visible)
    kind        TEXT NOT NULL CHECK (kind IN ('state', 'lga', 'locality')),
    name        TEXT NOT NULL,
    code        TEXT NOT NULL,
    parent_code TEXT NULL,  -- LGA -> state code; locality -> LGA code
    lat         DOUBLE PRECISION NULL,
    lng         DOUBLE PRECISION NULL,
    source      TEXT NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Upsert key for POST /v1/geo/gazetteer/import and the boot seeder
-- (ON CONFLICT (kind, code)): one row per (kind, code).
CREATE UNIQUE INDEX IF NOT EXISTS uq_geo_places_kind_code
    ON geo_places (kind, code);
-- Cascading-picker lookup shape: LGAs of a state, localities of an LGA.
CREATE INDEX IF NOT EXISTS idx_geo_places_parent ON geo_places (parent_code);

-- ---------------------------------------------------------------------------
-- RLS: ENABLE + FORCE + tenant_isolation (NULL-reference variant — identical
-- semantics to geo_boundaries, see the header comment)
-- ---------------------------------------------------------------------------
ALTER TABLE geo_places ENABLE ROW LEVEL SECURITY;
ALTER TABLE geo_places FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON geo_places;
CREATE POLICY tenant_isolation ON geo_places
    USING (tenant_id IS NULL
           OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id IS NULL
                OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to the runtime role (guarded so the file also applies standalone).
-- app_geo_login is the migration owner on fresh DBs, so its own future
-- tables must keep default privileges for the group (W51-C idiom).
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_geo') THEN
        EXECUTE 'GRANT USAGE ON SCHEMA public TO app_geo';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_geo';
        EXECUTE 'GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_geo';
    END IF;
END
$$;

DO $$
DECLARE
    bootstrap TEXT;
BEGIN
    FOREACH bootstrap IN ARRAY ARRAY['opendesk', 'postgres', 'app_geo_login'] LOOP
        -- ALTER DEFAULT PRIVILEGES FOR ROLE requires membership in that role
        -- (or superuser); skip gracefully otherwise (payments 0001 idiom).
        IF EXISTS (SELECT FROM pg_roles WHERE rolname = bootstrap)
           AND pg_has_role(current_user, bootstrap, 'member') THEN
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_geo', bootstrap);
            EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
                'GRANT USAGE, SELECT ON SEQUENCES TO app_geo', bootstrap);
        END IF;
    END LOOP;
END
$$;
