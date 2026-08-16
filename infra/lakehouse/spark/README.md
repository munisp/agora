# Spark tier — silver/gold/batch jobs (SPEC §13)

Runs from `infra/docker-compose.lakehouse.yml`: `spark-master` (spark://spark-master:7077,
UI on **8081**) + one `spark-worker`. Job sources are mounted read-only into both
containers at `/opt/spark-jobs`.

## Job disposition (W39 SIM-039 — producer audit)

Every job below was audited for whether its declared inputs actually have a
producer. Jobs DEGRADE GRACEFULLY: a missing/unreadable input logs a warning
and yields an empty contract frame instead of failing (this is deliberate —
the jobs stay schedulable before their producers land). Status:

| Job | Inputs | Producer status |
|---|---|---|
| `silver_clean_bookings.py` | `bronze.booking_events` | LIVE — analytics-pipeline Kafka sink (`opendesk.booking.events`) |
| `silver_clean_transcripts.py` | `bronze.transcripts` | LIVE — analytics-pipeline Kafka sink (`opendesk.conversation.transcripts`, Fluvio PII-redacted upstream) |
| `cac_analytics.py` (SPEC-W13) | `bronze.cac_events` | LIVE — analytics-pipeline sink landed (SPEC-W33 §2 A2) |
| | campaign-spend parquet extract (`SPEND_PATH` on MinIO) | PARKED — no producer yet (TODO producer: JDBC/analytics-pipeline export of Postgres spend tables); job runs with an empty spend frame |
| `geo_analytics.py` (SPEC-W8) | `silver.booking_events` | LIVE (via `silver_clean_bookings.py`) |
| | `contact_locations` + `service_areas` MinIO extracts | PARKED — no producer yet (TODO producer: booking-service Postgres exporter); job degrades to bookings-only output |
| `revenue_intelligence.py` (SPEC-W3) | `silver.booking_events`, `bronze.payment_events` | LIVE — analytics-pipeline sinks |
| `graph_enrichment.py` (SPEC-W28) | `silver`/`gold` tables (all optional) | LIVE chain; emits `opendesk.graph.enrichment.v1` (graph-sync consumes; topic provisioned) |
| `graph_export.py` (SPEC-W28) | graph-service internal export API + `bronze.usage_events` | LIVE — seam-only wave by design (no GNN/ART training, SPEC-W28 §6) |
| `training_snapshot.py` (SPEC-W33) | `gold.graph_*_features` (from `graph_export.py`) + label extracts | LIVE chain for features; label extracts PARKED (optional, degrade) |
| `seed_coverage.py` / `seed_geo_points.py` / `seed_gold_load.py` (SPEC-W17) | seed manifests / synthetic generators | LIVE seed tooling — inputs are generated/loaded by these jobs themselves (documented SUBSTITUTION: synthetic stand-ins for unavailable offline data) |

## Jobs

| Job | Source (Iceberg) | Sink (Iceberg) | What it does |
|---|---|---|---|
| `jobs/silver_clean_bookings.py` | `bronze.booking_events` | `silver.booking_events` | Dedupe on `event_id` (latest `occurred_at` wins), drop unkeyed/timeless rows. |
| `jobs/silver_clean_transcripts.py` | `bronze.transcripts` | `silver.transcripts` | Normalize `role`, dedupe on `(conversation_id, ts, role)`. |
| `jobs/geo_analytics.py` (SPEC-W8) | `silver.booking_events` + MinIO extracts (`contact_locations`, `service_areas`) | `gold.geo_demand_h3`, `gold.geo_service_area_coverage`, `gold.geo_hourly_density` | Apache Sedona geospatial gold: H3 res-8 demand cells (with cell WKT), service-area coverage (ST_Within), hour-of-week heat cells. Session via `jobs/sedona_common.py`. |

Both jobs create the silver table on first run (schema CTAS-cloned from bronze,
**unpartitioned = spec v1**) and then perform **partition evolution** with
`ALTER TABLE ... ADD PARTITION FIELD days(<ts>)` (**spec v2**). Iceberg keeps old and new
specs readable; re-runs use dynamic partition overwrite, so jobs are idempotent.

## Dependencies (Ivy packages, downloaded once into the `spark-ivy` volume)

- `org.apache.iceberg:iceberg-spark-runtime-3.5_2.12:1.6.1` — Iceberg Spark integration
  (any `1.5.x`/`1.6.x` line matching Spark 3.5 works; pin consistently with the REST catalog).
- `org.apache.iceberg:iceberg-aws-bundle:1.6.1` — shaded AWS SDK v2 for `S3FileIO` (MinIO).
- `org.apache.sedona:sedona-spark-shaded-3.5_2.12:1.7.0` +
  `org.datasyslab:geotools-wrapper:1.7.0-28.5` — Apache Sedona for the geo job
  only (SPEC-W8; matches bitnami/spark:3.5 / Scala 2.12). `sedona_common.py`
  injects these into `spark.jars.packages` itself, so `geo_analytics.py` needs
  **no `--packages` flag**; H3 res-8 cells use `ST_H3CellIDs`/`ST_H3ToGeom`
  (Sedona ≥ 1.6.0; geohash fallback documented in the job).
- Alternative to the bundle: `org.apache.hadoop:hadoop-aws:3.3.4` +
  `com.amazonaws:aws-java-sdk-bundle:1.12.262` with `io-impl` switched to
  `org.apache.iceberg.hadoop.HadoopFileIO`. The bundle path above is what the jobs
  configure and is the recommended one.

## Running

```bash
docker compose -f infra/docker-compose.lakehouse.yml up -d   # whole lakehouse tier

docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
  --master spark://spark-master:7077 \
  --packages org.apache.iceberg:iceberg-spark-runtime-3.5_2.12:1.6.1,org.apache.iceberg:iceberg-aws-bundle:1.6.1 \
  /opt/spark-jobs/silver_clean_bookings.py

docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
  --master spark://spark-master:7077 \
  --packages org.apache.iceberg:iceberg-spark-runtime-3.5_2.12:1.6.1,org.apache.iceberg:iceberg-aws-bundle:1.6.1 \
  /opt/spark-jobs/silver_clean_transcripts.py

# Geospatial gold tables (Sedona packages injected by sedona_common.py):
docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
  --master spark://spark-master:7077 \
  /opt/spark-jobs/geo_analytics.py
```

Environment overrides honoured by the jobs: `ICEBERG_REST_URI` (default
`http://iceberg-rest:8181`), `S3_ENDPOINT` (`http://minio:9000`),
`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` (`minioadmin`/`minioadmin`).
`revenue_intelligence.py` additionally honours `RECO_WINDOW_DAYS` (default `30`).
`geo_analytics.py` additionally honours `GEO_H3_RESOLUTION` (default `8`),
`GEO_CONTACT_LOCATIONS_PATH` / `GEO_SERVICE_AREAS_PATH` (MinIO extract paths —
see docs/geospatial-infra.md §1 for the input contracts) and
`GEO_SERVICE_AREAS_FORMAT` (`parquet`|`geojson`).

Verify in Trino afterwards:

```sql
SELECT * FROM iceberg.silver.booking_events LIMIT 10;
SELECT partition FROM iceberg.silver."booking_events$partitions";
```
