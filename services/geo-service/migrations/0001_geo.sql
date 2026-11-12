-- 0001_geo.sql — geo-service schema + row-level security (SPEC-W52 Scope A).
--
-- geo-service owns `geo_boundaries` in the BOOKING database (the PostGIS
-- one): platform reference administrative boundaries (tenant_id NULL) plus
-- tenant-private custom boundaries, all WGS84 geography(MultiPolygon,4326).
--
-- Applied idempotently by the service at startup via sqlx::raw_sql (the
-- payments-service 0001_rls.sql idiom — NO psql backslash commands, no
-- CONCURRENTLY). infra/postgres/init-scripts/09-geo-boundaries.sql carries
-- the same DDL for fresh clusters; the guards make both re-runnable.
--
-- RLS posture (house idiom: ENABLE + FORCE + tenant_isolation policy on the
-- request-scoped GUC, NULLIF(current_setting(...), '')::uuid):
--   * tenant_id IS NULL rows are PLATFORM REFERENCE data — visible to every
--     tenant (the enrichment endpoint reads them without a tenant GUC);
--   * tenant rows are isolated: visible only when the transaction-local GUC
--     app.tenant_id equals the row's tenant (set via set_config(..., true)
--     at the start of every tenant-scoped transaction, src/store.rs);
--   * with the GUC unset/empty, NULLIF(...) is NULL and only reference rows
--     match — fail-closed for tenant data by construction;
--   * WITH CHECK mirrors USING: any role can upsert REFERENCE rows
--     (tenant_id NULL), which is why reference-scope imports are gated by
--     the Permify manage_bookings check at the HTTP layer (src/auth.rs) —
--     the database policy alone is intentionally NOT the admin gate for
--     reference data.
--
-- Roles mirror 05-app-roles.sql exactly (NOLOGIN group + LOGIN member,
-- pg_roles-guarded) so this file also applies standalone on a fresh cluster
-- (e.g. the pgserver-backed test gate); passwords MUST stay in sync with
-- 05-app-roles.sql.
--
-- NOTE: the applying connection must be able to CREATE TABLE (fresh DBs) and
-- own the table to ALTER it — dev default GEO_DATABASE_URL uses the
-- app_geo_login role which 05-app-roles.sql grants CREATE ON SCHEMA public
-- (documented bootstrap exception, payments idiom). PostGIS must already be
-- available (06-booking-postgis.sql / booking-service ensureGeoTables);
-- CREATE EXTENSION IF NOT EXISTS is a no-op then and fails the boot
-- (fail-closed, booking's ErrPostGISUnavailable posture) otherwise.

-- ---------------------------------------------------------------------------
-- Roles (cluster-wide; idempotent — mirrors 05-app-roles.sql)
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

-- PostGIS is a hard dependency (geography type + ST_Covers). No-op when
-- 06-booking-postgis.sql / booking-service already enabled it; error ->
-- boot abort (fail-closed) when the extension is missing and the runtime
-- role cannot create it.
CREATE EXTENSION IF NOT EXISTS postgis;

-- ---------------------------------------------------------------------------
-- geo_boundaries: reference (tenant_id NULL) + tenant custom boundaries
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS geo_boundaries (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NULL,  -- NULL = platform reference data (world-visible)
    kind       TEXT NOT NULL CHECK (kind IN ('state', 'lga', 'ward', 'custom')),
    name       TEXT NOT NULL,
    code       TEXT NOT NULL,
    geom       geography(MultiPolygon, 4326) NOT NULL,
    source     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Upsert key for POST /v1/geo/boundaries/import: one row per
-- (scope-tenant, kind, code); reference rows share the zero-uuid scope.
CREATE UNIQUE INDEX IF NOT EXISTS uq_geo_boundaries_scope_kind_code
    ON geo_boundaries (COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid), kind, code);
CREATE INDEX IF NOT EXISTS idx_geo_boundaries_geom ON geo_boundaries USING gist (geom);
CREATE INDEX IF NOT EXISTS idx_geo_boundaries_tenant_kind ON geo_boundaries (tenant_id, kind);

-- ---------------------------------------------------------------------------
-- RLS: ENABLE + FORCE + tenant_isolation (NULL-reference variant — see the
-- header comment for the exact semantics)
-- ---------------------------------------------------------------------------
ALTER TABLE geo_boundaries ENABLE ROW LEVEL SECURITY;
ALTER TABLE geo_boundaries FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON geo_boundaries;
CREATE POLICY tenant_isolation ON geo_boundaries
    USING (tenant_id IS NULL
           OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id IS NULL
           OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants (idempotent; role-guarded so the file also applies standalone).
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
