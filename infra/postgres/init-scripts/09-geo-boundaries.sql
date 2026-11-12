-- 09-geo-boundaries.sql — SPEC-W52 Scope A: geo-service boundary table.
-- Runs once on first container start (postgres docker-entrypoint-initdb.d),
-- after 06-booking-postgis.sql (PostGIS) and 05-app-roles.sql (app_geo
-- roles). geo-service also applies the same DDL idempotently at boot
-- (services/geo-service/migrations/0001_geo.sql via sqlx::raw_sql), so this
-- script is belt-and-braces for fresh volumes — same dual-ownership comment
-- convention as 01-booking-schema.sql.
\c booking

CREATE EXTENSION IF NOT EXISTS postgis;

-- Reference (tenant_id NULL = platform reference data, world-visible under
-- the RLS policy below) + tenant-private custom boundaries. All geometry is
-- WGS84 MultiPolygon (ST_Multi-promoted by the ingest path).
CREATE TABLE IF NOT EXISTS geo_boundaries (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('state', 'lga', 'ward', 'custom')),
    name       TEXT NOT NULL,
    code       TEXT NOT NULL,
    geom       geography(MultiPolygon, 4326) NOT NULL,
    source     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_geo_boundaries_scope_kind_code
    ON geo_boundaries (COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid), kind, code);
CREATE INDEX IF NOT EXISTS idx_geo_boundaries_geom ON geo_boundaries USING gist (geom);
CREATE INDEX IF NOT EXISTS idx_geo_boundaries_tenant_kind ON geo_boundaries (tenant_id, kind);

ALTER TABLE geo_boundaries ENABLE ROW LEVEL SECURITY;
ALTER TABLE geo_boundaries FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_policies
                   WHERE schemaname = 'public' AND tablename = 'geo_boundaries'
                     AND policyname = 'tenant_isolation') THEN
        -- NULL-reference variant: reference rows (tenant_id IS NULL) are
        -- visible to every tenant; tenant rows only under the request GUC.
        CREATE POLICY tenant_isolation ON geo_boundaries
            USING (tenant_id IS NULL
                   OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK (tenant_id IS NULL
                   OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END
$$;

-- Grants to the geo service role (created in 05-app-roles.sql, which runs
-- BEFORE this script on a fresh init). ALTER DEFAULT PRIVILEGES in 05
-- already covers future tables; these explicit GRANTs make the privilege
-- set self-contained (07-agents-capture-schema.sql idiom). Guarded so the
-- script also works standalone (role absent).
DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_geo') THEN
        EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.geo_boundaries TO app_geo';
    END IF;
END
$$;
