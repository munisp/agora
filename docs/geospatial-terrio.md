# terrio geospatial foundation — geo-service (SPEC-W52)

Operational geospatial services for Agora built on the **terrio** Rust crate
(pure-Rust geospatial I/O). This document covers the new `geo-service`:
boundary ingest, point-in-polygon enrichment, self-hosted basemap tiles, and
the booking coverage-zone enforcement that consumes it. For the *analytical*
stack (Sedona/lakehouse) see `docs/geospatial-infra.md`; for the original W8
operational model (contact locations, service areas, geo campaigns) see
`docs/geospatial.md`.

## 1. Architecture

```
admin-web (MapLibre) ──► BFF /api/* ──► APISIX route api-geo (/api/geo/*) ──► geo-service
booking-service ───────────────────────► GEO_SERVICE_URL ───────────────────► geo-service
                                                                              │
                                              ┌───────────────────────────────┤
                                              ▼                               ▼
                                    booking Postgres (PostGIS)        PMTiles archive
                                    table: geo_boundaries             at GEO_TILES_PATH
```

`services/geo-service` is a small Rust service (same structure as
payments-service: `src/main.rs` + startup-applied `migrations/0001_geo.sql` via
`sqlx::raw_sql`, `/healthz`). It owns one new table in the **booking Postgres
database** (the PostGIS one):

- **`geo_boundaries`** — `id uuid PK`, `tenant_id uuid NULL` (NULL = platform
  reference data, world-visible), `kind text CHECK IN
  ('state','lga','ward','custom')`, `name`, `code`, `geom
  geography(MultiPolygon,4326)`, `source`, `created_at`; GiST index on
  `geom`; UNIQUE on `(COALESCE(tenant_id,'00000000-…'), kind, code)`.
  RLS is `ENABLE` + `FORCE` with the house `tenant_isolation` policy extended
  to let reference rows through: `tenant_id IS NULL OR tenant_id =
  NULLIF(current_setting('app.tenant_id', true), '')::uuid`.

### terrio's role

terrio does all vector I/O and CRS work inside geo-service:

- **Reading** boundary files at import: GeoJSON, GeoPackage, and zipped
  shapefiles (plus `{url}` fetches of the same).
- **Reprojection** to EPSG:4326 via terrio's CRS support when the source CRS
  differs (official Nigerian survey data is often UTM 31–33N /
  EPSG:32631–32633).
- **Tiles**: reading the PMTiles archive for the tile/style endpoints.

The crate is pinned with an exact version and `default-features = false` +
minimal features — **see `services/geo-service/Cargo.toml` for the pinned
version** (single source of truth; do not copy the number into docs).

### Endpoints

All behind APISIX as `/api/geo/*` (openid-connect + limit-count; the gateway
strips the `/api/geo` prefix):

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | upstream health check |
| `POST /v1/geo/boundaries/import` | multipart file (`.geojson`/`.json`/`.gpkg`/`.zip` shapefile) **or** JSON `{url}`; params `kind`, `name_field`, `code_field`, `source`, `scope` (`reference`\|`tenant`). Response `{imported, skipped, errors[]}`. Caps: 50,000 features / 100 MB. |
| `GET /v1/geo/enrich?lat=&lng=` | point-in-polygon over reference boundaries, then tenant custom ones; response `{state, lga, ward, boundaries:[{kind,name,code}]}` — nulls when nothing covers the point. |
| `GET /v1/geo/tiles/{z}/{x}/{y}.mvt` | vector tiles from the PMTiles archive; **503 when `GEO_TILES_PATH` is unset** (fail-closed, not a stub). |
| `GET /v1/geo/style.json` | MapLibre style document pointing at the tiles/glyph endpoints; **503 when tiles are unprovisioned**. |

Authorization: imports require Permify `manage_bookings` on
`organization:{tenant_id}` (dev bypass `AUTHZ_DISABLED=true` honored); tenant
is resolved from the `X-Tenant-Slug` header. The admin console reaches the
import endpoint through the BFF (`/api/geo/v1/geo/boundaries/import`) exactly
like any other gateway route.

## 2. Boundary import runbook

Goal: load **real** Nigerian administrative boundaries as `reference` rows so
enrichment and map overlays stop depending on the synthetic `cac.*`
placeholder geography (which remains for tests/analytics seeds only).

### Sources (attribution REQUIRED)

- **geoBoundaries** — <https://www.geoboundaries.org>, license **CC-BY-4.0**.
  Download the Nigeria ADM1 (states) / ADM2 (LGAs) GeoJSON or shapefiles.
  Attribution string suggestion: `geoBoundaries NGA ADM2, CC-BY-4.0
  (geoboundaries.org)`.
- **GRID3 Nigeria** — <https://grid3.org>, includes LGA and ward boundaries
  for Nigeria; check the per-dataset license and record it in `source`.

Whatever the source, put the attribution in the import's `source` field — it
is stored on every row and shown to operators.

> **Bundled fixtures are test-only.** The tiny GeoJSON fixtures under
> `services/geo-service` (used by `cargo test`) are hand-written, synthetic
> polygons. They are NOT real boundaries and must never be imported into a
> production database as reference data.

### Import via the admin console

Locations → **Import boundaries**: pick the file, choose `kind`
(state/lga/ward/custom) and `scope`, name the attribute fields that carry the
boundary name and code (`name_field`, `code_field` — e.g. geoBoundaries uses
`shapeName`/`shapeISO`; GRID3 exports vary, inspect the properties first),
and set `source` to the attribution string. The result panel shows
`{imported, skipped, errors[]}`; re-running the same file is safe — rows
upsert on `(scope-tenant, kind, code)` and replays count as `skipped` or
updated, not duplicated.

### Import via curl (large/binary files)

The admin BFF proxy is optimized for JSON/text bodies; for large GeoPackage
or zipped-shapefile uploads prefer calling the gateway directly with a
service token:

```bash
curl -X POST "$GATEWAY/api/geo/v1/geo/boundaries/import" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Tenant-Slug: $ORG_SLUG" \
  -F "file=@nga-adm2.geojson" \
  -F "kind=lga" -F "name_field=shapeName" -F "code_field=shapeISO" \
  -F "source=geoBoundaries NGA ADM2, CC-BY-4.0" -F "scope=reference"
```

Or import from a URL the service can reach: `POST` JSON
`{"url": "https://…/nga-wards.gpkg", "kind": "ward", …}` to the same path.

### After importing

- `GET /v1/geo/enrich?lat=6.5244&lng=3.3792` should now return
  `{state:"Lagos", lga:…, ward:…}` for covered points.
- Civic reports and field captures with coordinates are auto-enriched from
  this point on (see §4); existing rows are not backfilled.

## 3. Offline tile provisioning runbook

The admin dashboards no longer hot-link `tile.openstreetmap.org` (ToS and
offline-hosting problem). Basemap tiles are served by geo-service from a
PMTiles archive you provision out-of-band:

1. Obtain an OSM-derived vector extract for the coverage area (e.g. a
   Geofabrik Nigeria extract; ODbL — attribution stays in the style).
2. Convert to PMTiles with terrio's CLI on any operator machine:

   ```bash
   terrio convert input.gpkg tiles/ng-basemap.pmtiles --maxzoom 12
   ```

   (Any toolchain that produces a valid PMTiles archive works; the service
   only reads the archive.)
3. Mount the archive into the container and point the service at it:

   ```yaml
   # docker-compose override
   services:
     geo:
       volumes:
         - ./tiles/ng-basemap.pmtiles:/data/geo/ng-basemap.pmtiles:ro
       environment:
         GEO_TILES_PATH: /data/geo/ng-basemap.pmtiles
   ```

4. Verify: `curl -i $GATEWAY/api/geo/style.json` returns 200 and the
   Locations/Geo-campaigns maps render with the self-hosted basemap.

When `GEO_TILES_PATH` is unset or the archive is missing, the tile/style
endpoints answer **503 with a plain error** and the admin maps show their
"map tiles unavailable" degraded panel — every other geo feature (enrichment,
import, coverage checks) keeps working.

## 4. Coverage-enforcement semantics

Booking-time geography gating lives in booking-service
(`internal/bookingops`), driven by two env vars:

- `GEO_SERVICE_URL` — unset/empty disables the enrichment client entirely
  (fail-open; nothing calls geo-service).
- `BOOKING_GEO_ENFORCE` — default `false`. When `true`, public booking
  creation is checked against the site's active `service_areas`:

  | Condition | Result |
  |---|---|
  | Enforcement disabled | pass |
  | Site has **no** active service areas | **pass** (documented semantics — a site without drawn zones is open to everyone, not rejected) |
  | Booking contact has no `contact_locations` point | pass |
  | Point covered (`ST_Covers`) by ≥1 active area | pass |
  | Point outside all active areas | `422 {code:"GEO_OUTSIDE_COVERAGE"}` |
  | PostGIS unavailable | 503 (existing `ErrPostGISUnavailable` behavior) |

Enrichment (civic cases + field captures) is separate from enforcement and is
always **fail-open**: when geo-service is slow (3 s timeout) or down, the
business write succeeds without `lga`/`ward` auto-fill. Enrichment only fills
`lga`/`ward` **when they are currently empty** — human/agent text is never
overwritten — and records provenance in `geo_enriched_at` /
`geo_enrichment_source = 'geo-service'`, which the admin console renders as a
small "auto" badge next to the values.

## 5. Environment variables

| Variable | Service | Default | Purpose |
|---|---|---|---|
| `GEO_DATABASE_URL` | geo-service | compose-provided | Postgres DSN for the booking DB (`geo_boundaries` lives there). |
| `GEO_TILES_PATH` | geo-service | **unset** | Path to the PMTiles archive. Unset ⇒ tiles/style endpoints fail closed with 503. |
| `GEO_BIND` | geo-service | compose-provided | Listen address (e.g. `0.0.0.0:7015`). |
| `GEO_SERVICE_URL` | booking-service | **empty = disabled** | Internal base URL used by the enrichment client (e.g. `http://geo:7015`). |
| `BOOKING_GEO_ENFORCE` | booking-service | `false` | Master switch for §4 coverage-zone enforcement. |
| `NEXT_PUBLIC_GEO_STYLE_URL` | admin-web | `/api/geo/style.json` | Basemap style URL consumed by MapLibre (via the BFF by default). |

All six are wired through `docker-compose.yml` as `${VAR:-default}` and
documented in `.env.example`.

## 6. Degraded-mode behavior (summary)

| Failure | Observable behavior |
|---|---|
| `GEO_TILES_PATH` unset / archive missing | `GET /api/geo/style.json` and `/v1/geo/tiles/*` → 503 plain error; admin maps render an inline **"Map tiles unavailable"** panel with retry (no blank canvas, no hotlink fallback). Data layers/filters keep working; polygon drawing is disabled while the basemap is absent. |
| geo-service down | Admin maps show the same degraded panel (style fetch fails); booking enrichment fails open (writes succeed, no auto-fill); coverage enforcement still applies when enabled (it uses local PostGIS `service_areas`, not geo-service). |
| PostGIS extension missing | geo tables bootstrap reports `ErrPostGISUnavailable` → 503 on geo routes; non-geo functionality unaffected. |
| Import of malformed features | Per-feature errors returned in `errors[]`; the valid features of the file commit together in a single transaction, upserted on `(scope-tenant, kind, code)`. |
