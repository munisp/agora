# CAC lakehouse — Iceberg gold tables + `cac_analytics.py` (SPEC-W13 §4, Agent C)

Batch-verified CAC (customer acquisition cost) gold tables in the lakehouse,
produced by `infra/lakehouse/spark/jobs/cac_analytics.py`. The job mirrors
`geo_analytics.py` (SPEC-W8): same Sedona session (`sedona_common.py`), same
Iceberg REST catalog + MinIO wiring, same graceful-degradation extract
pattern, same H3 res-8 drill-down, same dynamic partition overwrite
idempotency.

## 1. Dual-path architecture (SPEC-W13 §5)

CAC numbers reach dashboards by two paths:

1. **Realtime** — analytics-service consumes `cac.events` (consumer group
   `analytics-cac`) into Postgres rollup tables (`cac_rollup_channel` /
   `cac_rollup_lga`, RLS'd) and serves `GET /v1/cac/summary` (Agent B).
2. **Batch-verified (this document)** — the nightly lakehouse job recomputes
   the same rollups from the immutable bronze stream + campaign-spend extract
   into Iceberg gold tables. These are the source of truth for reconciliation:
   the realtime path is optimised for freshness, this path is idempotent,
   replayable, and Trino-queryable for ad-hoc/BI use and drift checks against
   the Postgres rollups.

## 2. Inputs

### 2a. `iceberg.bronze.cac_events` — funnel events (SPEC-W13 §2)

Kafka topic `cac.events` carries CloudEvents of type
`com.opendesk.cac.FunnelEvent`. The bronze table stores the CloudEvent `data`
payload flattened (TODO producer: extend the analytics-pipeline bronze sink to
cover `cac.events`, the same way it sinks `opendesk.booking.events` →
`bronze.booking_events`; until then the job logs a warning and writes empty
gold tables instead of failing — same pattern as `geo_analytics.py`'s
extracts).

| Bronze column | CloudEvent source | Notes |
|---|---|---|
| `event_id` | `data.event_id` (fallback `id`) | dedupe fallback key |
| `tenant_id` | `data.tenant_id` | required (rows without it are dropped) |
| `entity_type` | `data.entity_type` | `lead\|customer\|agent` |
| `entity_id` | `data.entity_id` | |
| `event_name` | `data.event_name` | `lead_created\|contacted\|opted_in\|qualified\|converted\|first_txn\|lost` |
| `event_ts` | `data.event_ts` (fallback `time`) | required; `day = to_date(event_ts)` |
| `channel` | `data.channel` | normalised lower/trim; empty → `unknown` |
| `campaign_id` | `data.campaign_id` | |
| `lga_id` | `data.lga_id` | int; NULL allowed (row stays out of the LGA table) |
| `amount_ngn` | `data.amount_ngn` | revenue signal on `first_txn`; not used in CAC |
| `idempotency_key` | `data.idempotency_key` | primary dedupe key |

Env override: `CAC_EVENTS_TABLE` (default `iceberg.bronze.cac_events`).

**Idempotent consumption:** at-least-once delivery is absorbed by dedupe on
`idempotency_key` (fallback `event_id`), latest `event_ts` wins — the same
window-dedupe idiom as `silver_clean_bookings.py`.

### 2b. Campaign spend extract — `POST /v1/campaigns/{id}/spend` (SPEC-W13 §4)

Spend is entered operationally in booking-service (`amount_ngn`, `channel`,
`day`). The lakehouse reads a parquet extract (TODO producer: JDBC/
analytics-pipeline export of the booking-service campaign-spend table):

- Path: env `CAC_CAMPAIGN_SPEND_PATH` (default `s3://lake/extracts/campaign_spend/`)
- Columns: `tenant_id string, campaign_id string, channel string, day date, amount_ngn double`
- Multiple rows per `(tenant_id, channel, day)` are summed by the job.
- Missing extract → warning + `spend_ngn = 0` everywhere (CAC still computes
  for organic conversions; it never blocks the pipeline).

### 2c. LGA boundaries extract

Nigerian LGA polygons — national reference data (not tenant-scoped). Same
dual-format reader as `geo_analytics.py`'s service-areas extract:

- Path: env `CAC_LGA_BOUNDARIES_PATH` (default `s3://lake/extracts/lga_boundaries/`)
- Format: env `CAC_LGA_BOUNDARIES_FORMAT` = `parquet` (default) | `geojson`
- Parquet columns: `lga_id int, name string, geojson string` (GeoJSON
  Polygon/MultiPolygon, e.g. `ST_AsGeoJSON(geom)` from a PostGIS export)
- GeoJSON: a FeatureCollection whose features carry `{lga_id, name}` properties
- Missing extract → warning + `geom`/`h3_cells` NULL (metric rows still land).

## 3. Delta → Iceberg port mapping

The CAC program doc specifies Databricks/Delta tables
(`cac.gold.daily_cac_by_channel`, `cac.gold.daily_cac_by_lga`). OpenDesk's
lakehouse is **Iceberg-only** (SPEC-W13 §4: "Iceberg — NOT Delta"), surfaced
through the `iceberg` Spark catalog and Trino. The port is name- and
column-faithful:

| CAC doc (Delta) | OpenDesk (Iceberg) | Mapping notes |
|---|---|---|
| catalog `cac`, schema `gold` | Spark catalog `iceberg`, namespace `cac_gold` | Dot-namespaces flatten to `_` (Trino: `iceberg.cac_gold`) |
| `cac.gold.daily_cac_by_channel` | `iceberg.cac_gold.daily_cac_by_channel` | columns identical: `day, tenant_id, channel, spend_ngn, leads, conversions, cac_ngn` |
| `cac.gold.daily_cac_by_lga` | `iceberg.cac_gold.daily_cac_by_lga` | columns identical (`day, tenant_id, lga_id, leads, conversions, cac_ngn, geom`) **plus** `h3_cells ARRAY<BIGINT>` drill-down |
| Delta `GEOMETRY`/`GEOGRAPHY` | `geom STRING` (WKT) | Iceberg has no geometry type — same WKT convention as `gold.geo_*` (SPEC-W8); parse in Trino with `ST_GeometryFromText` |
| Delta partition by `day` | `PARTITIONED BY (day)` (identity) | unchanged |
| `OPTIMIZE` / Z-ORDER | Iceberg `rewrite_data_files` / sort order | ops concern; not wired in W13 |
| Delta time travel (`VERSION AS OF`) | Iceberg snapshots (`iceberg.cac_gold."daily_cac_by_channel$snapshots"`) | same capability, different syntax |
| Delta `MERGE` upserts | dynamic partition overwrite (`writeTo(...).overwritePartitions()`) | idempotent re-runs replace only touched day partitions |

`amount_ngn`/`spend_ngn`/`cac_ngn` are `DOUBLE` (NGN amounts, not kobo cents
— the CAC program doc and SPEC-W13 §4 both speak NGN, unlike the payments
pipeline's `amount_cents`).

## 4. Gold table reference

Both tables live in `iceberg.cac_gold`, are partitioned by `day`, and are
written with dynamic partition overwrite — re-runs replace only the day
partitions present in the input (idempotent). Trino-visible immediately.

### `cac_gold.daily_cac_by_channel`

One row per `(day, tenant_id, channel)` with events and/or spend.

| Column | Type | Definition |
|---|---|---|
| `day` | date | `to_date(event_ts)` / spend extract day |
| `tenant_id` | varchar | |
| `channel` | varchar | lower/trim; empty → `unknown` (SPEC-W13 §1 vocabulary: voice, whatsapp, telegram, web, sms, webhook, ussd, qr, promo, field) |
| `spend_ngn` | double | summed campaign spend; 0 when no extract row |
| `leads` | bigint | deduped `lead_created` events |
| `conversions` | bigint | deduped `converted` events |
| `cac_ngn` | double | `spend_ngn / conversions`; **NULL when conversions = 0** (undefined, never infinite) |

Only `lead_created` and `converted` enter the math. `contacted`, `opted_in`,
`qualified`, `first_txn`, `lost` are funnel stages for drill analyses, not CAC
inputs (`first_txn` carries `amount_ngn` — a revenue/payback signal consumed
by the analytics-service `payback_days_estimate`, not by this job).

### `cac_gold.daily_cac_by_lga`

One row per `(day, tenant_id, lga_id)` (events with NULL `lga_id` are excluded;
they remain visible in the by-channel table).

| Column | Type | Definition |
|---|---|---|
| `day` / `tenant_id` / `lga_id` | date / varchar / int | |
| `leads` / `conversions` | bigint | deduped, same event filter as above |
| `cac_ngn` | double | see allocation below; NULL when conversions = 0 |
| `geom` | varchar | LGA polygon as WKT (from the boundaries extract; NULL if the extract is missing) |
| `h3_cells` | array(bigint) | H3 res-8 cells covering the LGA polygon — **drill-down column** (`ST_H3CellIDs(geom, 8)`; tunable via `CAC_H3_RESOLUTION`). Trino: `UNNEST(h3_cells)`; cell polygons via `ST_H3ToGeom`, same pattern as `gold.geo_demand_h3` |

**Spend allocation (documented assumption).** Spend is recorded per
`(tenant, channel, day)` — there is no per-LGA spend signal — so the LGA
table allocates the tenant-day pool (summed across channels) **pro-rata by
lead share** over geolocated leads:

```
allocated_spend(lga) = tenant_day_spend * lga_leads / tenant_day_leads
cac_ngn(lga)         = allocated_spend / lga_conversions      (NULL if 0)
```

Allocations across LGAs sum back to the tenant-day pool, so the LGA and
channel views reconcile. If a future wave adds per-LGA spend, swap the
allocation for a direct join — the schema does not change.

## 5. Session, dependencies, running

The job uses `sedona_common.build_sedona_context(app_name="opendesk-cac-analytics")`
— the pinned Sedona 1.7.0 / Spark 3.5 artifacts
(`sedona-spark-shaded-3.5_2.12:1.7.0` + `geotools-wrapper:1.7.0-28.5`) and the
Iceberg 1.6.1 runtime are merged into `spark.jars.packages` by `sedona_common`,
so **no `--packages` flag** is needed (same as `geo_analytics.py`):

```bash
docker compose -f infra/docker-compose.lakehouse.yml up -d   # lakehouse tier

docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
  --master spark://spark-master:7077 \
  /opt/spark-jobs/cac_analytics.py
```

Config is env-driven (no CLI args — same style as the other jobs):
`CAC_EVENTS_TABLE`, `CAC_CAMPAIGN_SPEND_PATH`, `CAC_LGA_BOUNDARIES_PATH`,
`CAC_LGA_BOUNDARIES_FORMAT` (`parquet`|`geojson`), `CAC_H3_RESOLUTION`
(default 8), plus the shared `ICEBERG_REST_URI` / `S3_ENDPOINT` /
`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`.

Verify in Trino:

```sql
SELECT * FROM iceberg.cac_gold.daily_cac_by_channel ORDER BY day DESC LIMIT 20;
SELECT day, tenant_id, lga_id, leads, conversions, cac_ngn
FROM iceberg.cac_gold.daily_cac_by_lga WHERE day = DATE '2026-03-01';
-- H3 drill-down: cell-level view of one LGA-day
SELECT c, ST_AsText(ST_H3ToGeom(ARRAY[c])) AS cell_wkt
FROM iceberg.cac_gold.daily_cac_by_lga
CROSS JOIN UNNEST(h3_cells) AS t(c)
WHERE tenant_id = '<tenant>' AND lga_id = 101 AND day = DATE '2026-03-01';
SELECT partition FROM iceberg.cac_gold."daily_cac_by_channel$partitions";
```

## 6. Tests

`infra/lakehouse/spark/jobs/test_cac_analytics.py` — `py_compile` of the job
plus pure-function unit tests of the aggregation math (dedupe, channel
normalisation, CAC null-safety, pro-rata allocation, tenant isolation,
contract column sets). No Spark session needed: the job module guards its
pyspark import and keeps Spark-free reference aggregators
(`aggregate_daily_by_channel` / `aggregate_daily_by_lga`) that the Spark
transforms mirror expression-for-expression.

```bash
python3 -m pytest infra/lakehouse/spark/jobs/test_cac_analytics.py -q
```

## 7. Known gaps / TODO producers

- Bronze sink for `cac.events` (analytics-pipeline covers booking/payment/
  transcript/usage topics only as of W13) — until it lands, the job degrades
  to empty gold tables with a warning.
- Campaign-spend extract producer (booking-service Postgres →
  `s3://lake/extracts/campaign_spend/`).
- LGA boundaries extract producer (PostGIS →
  `s3://lake/extracts/lga_boundaries/`).
- dbt passthrough views for the two gold tables (pattern:
  `infra/lakehouse/dbt/models/gold/geo_*.sql`) are left to a follow-up so this
  wave stays inside the assigned file ownership.
