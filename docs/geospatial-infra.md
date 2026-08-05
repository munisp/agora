# Geospatial lakehouse infrastructure — Apache Sedona + GeoLibre (SPEC-W8 Part B)

Operational/analytical geospatial stack for Agora. PostGIS in booking-service
owns the *operational* model (Part A); this document covers the *analytical*
side: Apache Sedona jobs in the lakehouse, the gold geo tables they produce,
the GeoLibre GIS workbench, and Trino query recipes.

## 1. Sedona setup

The lakehouse Spark runtime is `bitnami/spark:3.5` (Spark 3.5, Scala 2.12 —
`infra/docker-compose.lakehouse.yml`). The matching Sedona artifacts, pinned in
`infra/lakehouse/spark/jobs/sedona_common.py`:

| Artifact | Version | Why |
|---|---|---|
| `org.apache.sedona:sedona-spark-shaded-3.5_2.12` | `1.7.0` | Sedona SQL functions + Kryo serde for Spark 3.5 |
| `org.datasyslab:geotools-wrapper` | `1.7.0-28.5` | Sedona's GeoTools dependency (`<sedona>-<geotools>`) |
| `org.apache.iceberg:iceberg-spark-runtime-3.5_2.12` | `1.6.1` | unchanged, same as existing jobs |
| `org.apache.iceberg:iceberg-aws-bundle` | `1.6.1` | unchanged (MinIO S3FileIO) |

`sedona_common.build_sedona_context()` merges these into `spark.jars.packages`
(with anything already configured via spark-defaults / `SPARK_JARS_PACKAGES`),
installs the Kryo serializer + `SedonaKryoRegistrator`, and returns a
`SedonaContext` wired to the same Iceberg REST catalog as the other jobs — so
**no `--packages` flag is needed** on submit:

```bash
docker compose -f infra/docker-compose.lakehouse.yml up -d   # lakehouse tier

docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
  --master spark://spark-master:7077 \
  /opt/spark-jobs/geo_analytics.py
```

### Inputs (extract contracts — TODO producers)

`geo_analytics.py` reads two extracts from MinIO in addition to
`iceberg.silver.booking_events`. A missing extract logs a warning and yields
empty gold tables rather than failing the pipeline:

- **Contact locations** — env `GEO_CONTACT_LOCATIONS_PATH`
  (default `s3://lake/extracts/contact_locations/`), parquet. One row per
  booking with the contact's latest location:
  `tenant_id, booking_id, contact_id, lat, lng, source, updated_at`.
  Producer TODO: export from booking-service Postgres (`contact_locations`
  joined to bookings) via analytics-pipeline or a JDBC extract job.
- **Service areas** — env `GEO_SERVICE_AREAS_PATH`
  (default `s3://lake/extracts/service_areas/`), format env
  `GEO_SERVICE_AREAS_FORMAT=parquet|geojson`. Parquet columns:
  `tenant_id, service_area_id, name, geojson` (geometry as GeoJSON string,
  e.g. `ST_AsGeoJSON(geom)` from PostGIS). GeoJSON format: a FeatureCollection
  whose features carry `{tenant_id, service_area_id, name}` properties.

### H3 vs geohash

`gold.geo_demand_h3` / `gold.geo_hourly_density` use **H3 res-8 cells** via
Sedona's `ST_H3CellIDs` / `ST_H3ToGeom` (H3 support landed in Sedona 1.6.0 and
is present in the pinned 1.7.0). If the Sedona pin ever drops below 1.6.0,
switch `assign_cells()` in `geo_analytics.py` to `ST_GeoHash(geom, 12)` +
`ST_GeomFromGeoHash` — the fallback is documented inline. Resolution is
tunable via `GEO_H3_RESOLUTION` (default 8).

## 2. Gold table reference

All three are Iceberg tables in `iceberg.gold`, partitioned by `tenant_id`,
written with dynamic partition overwrite (idempotent re-runs), and surfaced in
dbt as passthrough views (`infra/lakehouse/dbt/models/gold/geo_*.sql` + docs
in `schema.yml`). Geometry is stored as **WKT strings** (Iceberg has no
geometry type); `h3_cell`/`h3_cell_str` are the uint64 / hex-string forms of
the same H3 index.

### `gold.geo_demand_h3` — bookings per H3 cell per tenant per day

| Column | Type | Notes |
|---|---|---|
| `tenant_id` | varchar | partition key |
| `day` | date | from `starts_at` (fallback `occurred_at`) |
| `h3_cell` | bigint | H3 res-8 cell id |
| `h3_cell_str` | varchar | hex string index (JS/Trino interop) |
| `cell_wkt` | varchar | cell polygon WKT (`ST_GeometryFromText` in Trino) |
| `bookings` | bigint | BookingCreated count |

### `gold.geo_service_area_coverage` — inside vs outside each service area

| Column | Type | Notes |
|---|---|---|
| `tenant_id` | varchar | partition key |
| `day` | date | |
| `service_area_id` | varchar | from the service-areas extract |
| `service_area_name` | varchar | |
| `bookings_inside` | bigint | `ST_Within(point, area)` |
| `bookings_outside` | bigint | tenant-day total − inside |
| `coverage_share` | double | inside / total (0–1) |

### `gold.geo_hourly_density` — demand heat cells by hour-of-week

| Column | Type | Notes |
|---|---|---|
| `tenant_id` | varchar | partition key |
| `day_of_week` | int | 1=Sunday … 7=Saturday (Spark `dayofweek`) |
| `hour` | int | 0–23 |
| `h3_cell` / `h3_cell_str` | bigint / varchar | join to `geo_demand_h3` for geometry |
| `bookings` | bigint | |

## 3. GeoLibre workbench

### Deploy

GeoLibre is an **optional override**, not part of the base stack:

```bash
docker compose -f docker-compose.yml -f infra/compose/geolibre.compose.yml up -d geolibre
```

- Direct UI: `http://localhost:8085`
- Via the gateway: `http://localhost:9080/gis/` — APISIX route `gis-geolibre`
  (`/gis/*` → `geolibre:8085`) with the standard Keycloak jwt
  (openid-connect bearer_only) + prefix-strip rewrite + redis limit-count,
  same pattern as `/crm/*`.
- `GEOLIBRE_DISABLE_SIDECAR=1` by default (headless GIS workbench); set it to
  `0` in `.env` to enable GeoLibre's sidecar assistant.

Analyst notebook example: `infra/lakehouse/notebooks/geolibre-exploration.ipynb`
(pulls a `gold.geo_demand_h3` sample via Trino, renders it + service areas on a
GeoLibre `Map(center=(6.5244, 3.3792), zoom=11)`, saves a `.geolibre.json`
project).

### Embedding (`maponly` mode)

For admin dashboards, embed GeoLibre chrome-less with its `maponly` mode:

```html
<iframe src="http://localhost:9080/gis/?mode=maponly&project=opendesk-geo-demo.geolibre.json"
        width="100%" height="480" frameborder="0"></iframe>
```

`mode=maponly` hides the workbench chrome (no layer panel/toolbars) and renders
just the map canvas — the right fit for iframe embeds. Auth follows the gateway:
the `/gis/*` route requires the same Keycloak session as the admin app, so
embedded maps inherit SSO.

## 4. Trino geo cookbook

Trino 448 ships geospatial functions (`ST_*`); cell polygons come from the WKT
column. Run against `iceberg.gold` (dev Trino on `localhost:8088`).

**Top-20 demand cells for a tenant last week (as GeoJSON-ready geometries):**

```sql
SELECT day, h3_cell_str, bookings,
       ST_AsGeoJSON(ST_GeometryFromText(cell_wkt)) AS cell_geojson
FROM iceberg.gold.geo_demand_h3
WHERE tenant_id = '<tenant-uuid>'
  AND day >= date_trunc('week', current_date) - interval '7' day
ORDER BY bookings DESC
LIMIT 20;
```

**Coverage per service area this month (which areas under-serve demand):**

```sql
SELECT service_area_name,
       sum(bookings_inside)  AS inside,
       sum(bookings_outside) AS outside,
       round(avg(coverage_share), 3) AS avg_coverage
FROM iceberg.gold.geo_service_area_coverage
WHERE tenant_id = '<tenant-uuid>'
  AND day >= date_trunc('month', current_date)
GROUP BY service_area_name
ORDER BY avg_coverage ASC;
```

**Staffing heatmap — busiest cells on Friday evenings:**

```sql
SELECT d.hour, d.h3_cell_str, sum(d.bookings) AS bookings,
       ST_AsText(ST_GeometryFromText(any_value(h.cell_wkt))) AS cell_wkt
FROM iceberg.gold.geo_hourly_density d
LEFT JOIN iceberg.gold.geo_demand_h3 h
  ON h.tenant_id = d.tenant_id AND h.h3_cell = d.h3_cell
WHERE d.tenant_id = '<tenant-uuid>'
  AND d.day_of_week = 6            -- Friday (1=Sunday)
  AND d.hour BETWEEN 17 AND 21
GROUP BY d.hour, d.h3_cell_str
ORDER BY bookings DESC
LIMIT 50;
```

**Bonus — render any cell in PostGIS/GeoJSON tooling without Trino geometry
functions:** `cell_wkt` is directly consumable by `ST_GeomFromText` (PostGIS),
`shapely.wkt.loads` (Python), or `ST_AsGeoJSON(...)` as above.
