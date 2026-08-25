# analytics-pipeline (port 7009)

Kafka → Iceberg **bronze** sink for the OpenDesk lakehouse (SPEC §4 topics, §13 stack),
plus a FastAPI sidecar exposing health and Prometheus metrics.

```
Kafka topics (CloudEvents 1.0)                     Iceberg REST catalog :8181
  opendesk.booking.events  ─┐   micro-batch          warehouse s3://lake/warehouse (MinIO)
  opendesk.payments.events ─┼─ (BATCH_SIZE / ──▶  bronze.booking_events
  opendesk.conversation.   ─┘   FLUSH_INTERVAL)   bronze.payment_events
    transcripts                                   bronze.transcripts
```

## How it works

1. One `aiokafka` consumer (group `analytics-pipeline`, `enable_auto_commit=False`)
   subscribes to the four topics and buffers messages **per topic**.
2. A buffer flushes when it reaches `BATCH_SIZE` messages **or** ages past
   `FLUSH_INTERVAL` seconds (checked every 1 s).
3. Flush = map payloads to bronze rows → `pyarrow.Table` → `pyiceberg` `table.append()`
   against the REST catalog. Offsets are committed **only after** a successful append:
   delivery is **at-least-once**; duplicates are removed downstream by the Spark silver
   jobs (`infra/lakehouse/spark/jobs`, dedupe on `event_id` / `(conversation_id, ts, role)`).
4. On startup the service retries until Kafka and the REST catalog are reachable, and
   auto-creates namespace `bronze` + the four tables with explicit pyiceberg schemas
   (`AUTO_CREATE_TABLES=true`).

## Bronze schema contract (consumed by dbt — do not drift)

Column names/order match `infra/lakehouse/dbt/models/silver/schema.yml` sources and are
guarded by `tests/test_dbt_conformance.py`:

| Table | Columns (Iceberg types) |
|---|---|
| `bronze.booking_events` | `event_id` string, `event_type` string, `tenant_id` string, `booking_id` string, `status` string, `source` string, `price_cents` long, `currency` string, `starts_at` timestamp, `occurred_at` timestamp, `offering_id` string *(appended in SPEC-W3 §3 for revenue intelligence; existing tables get it via Iceberg schema evolution in `ensure_bronze`, old rows read NULL)* |
| `bronze.payment_events` | `event_id` string, `event_type` string, `tenant_id` string, `booking_id` string, `amount_cents` long, `currency` string, `transfer_code` long, `ledger_ref` string, `occurred_at` timestamp |
| `bronze.transcripts` | `conversation_id` string, `tenant_id` string, `role` string, `text` string, `ts` timestamp, `audio_url` string |
| `bronze.usage_events` | `event_id` string, `tenant_id` string, `metric` string, `value` double, `occurred_at` timestamp, `meta` string *(Wave 5 #9; `value` is a double because call-minutes are fractional; `meta` is stringified JSON)* |

Mapping rules (`analytics_pipeline/mapping.py`):

- **CloudEvents envelope**: `event_id ← id`, `occurred_at ← time`, `tenant_id ← tenantid`
  extension (falls back to `data.tenantId`), `booking_id ← data.bookingId` (falls back to
  `subject`). `event_type` keeps only the **last segment** of `type`
  (`com.opendesk.booking.BookingCreated` → `BookingCreated`) so dbt's `lower(event_type)`
  comparisons hold.
- **Payload keys** are read in camelCase *or* snake_case (`bookingId`/`booking_id`, …).
- **Transcripts** also accept bare `ConversationTurn` messages (no envelope) for the raw
  Fluvio-fed path.
- **Usage events** (Wave 5 #9) accept the bare metering payload
  `{tenant_id, metric, value, ts, meta}` (CloudEvent envelopes tolerated);
  `occurred_at ← ts` with the envelope `time` as fallback. This service only
  *consumes* usage records — it never emits them itself.
- **Timestamps** are naive UTC (Iceberg `timestamp` without timezone), consistent with the
  Spark jobs' `spark.sql.iceberg.handle-timestamp-without-timezone=true`. ISO-8601,
  epoch seconds and epoch millis inputs are accepted.

## How dbt gold marts consume bronze

`infra/lakehouse/dbt` reads these tables through Trino (`iceberg.bronze.*`):
`silver/stg_booking_events` + `stg_transcripts` standardize casing/enums as **views**;
gold tables aggregate them — `daily_bookings_per_tenant`, `revenue_daily` (uses
`transfer_code` 101/102/103), `no_show_rate`, `agent_containment_rate` (a conversation is
contained when it has zero `role = 'human_agent'` turns), and `usage_daily`
(Wave 5 #9: `{tenant_id, date, metric, total_value}` from `bronze.usage_events`). The Spark silver jobs also read
bronze directly to produce deduplicated `silver.*` Iceberg tables. Because dbt tests
assert `accepted_values` on lowercased `event_type`/`source`/`role`, the sink preserves
raw casing and lets dbt normalize.

## Sidecar API (port 7009)

> **Trust boundary (W44/F15-15).** This service is **not** routed through the
> APISIX gateway and performs **no authentication** of its own. Port 7009 must
> stay on the internal container network only. Browser access goes through the
> admin-web BFF (`/api/analytics/*`), which requires a NextAuth session (401
> for anonymous callers), derives the tenant from the session's verified
> `tenant_slugs` Keycloak claim, and injects it as `X-Tenant-Slug` (and the
> `?tenant=` query for the UUID-style endpoints), **ignoring caller-supplied
> tenant values**. Treat `X-Tenant-Slug` / `?tenant=` as trusted in-network
> input only — never publish the port, and never rely on these endpoints being
> safe to expose directly.

| Endpoint | Description |
|---|---|
| `GET /healthz` | 200 once Kafka consumer is running and Iceberg bootstrap done (503 while starting). Body: per-topic `lag` (highwater − position), buffered count, target table, `last_error`. |
| `GET /metrics` | Prometheus text: `analytics_messages_consumed_total`, `analytics_rows_written_total`, `analytics_flushes_total{outcome}`, `analytics_flush_duration_seconds`, `analytics_buffer_messages`, `analytics_consumer_lag`, `analytics_consumer_running`. |
| `GET /v1/recommendations?tenant=<uuid>` | SPEC-W3 §3 innovation 9: latest pricing recommendation per offering for the tenant, read via pyiceberg scan of `gold.reco_pricing` (written by `infra/lakehouse/spark/jobs/revenue_intelligence.py`; uses the same `ICEBERG_REST_URI`/S3 env as the sink). Returns `{"tenant", "recommendations": [...]}` with peak-hour stats, `suggested_peak_multiplier` and `suggested_deposit_pct`; **empty list when the table does not exist yet** (no Spark run), 502 on lakehouse errors. |
| `GET /v1/metering?tenant=<uuid>&from=<date>&to=<date>` | Wave 5 #9: aggregated usage rows `{tenant_id, date, metric, total_value}` per tenant, read via pyiceberg scan of `bronze.usage_events` (same shape as the dbt `gold.usage_daily` mart, so results are consistent whether or not dbt has run). `from`/`to` are optional inclusive ISO dates; 400 on malformed/inverted ranges, 502 on lakehouse errors. **Empty list when no usage exists yet** — see *Usage metering (Wave 5 #9)* below. |
| `GET /v1/cac/summary?from=<date>&to=<date>` | SPEC-W13 §5: CAC dashboard summary `{by_channel, by_lga, blended_cac_ngn, ltv_ngn, payback_days_estimate, data_quality}` from the realtime Postgres rollups (`analytics_meta.cac_rollup_*`, tenant-RLS'd) fed by the `cac.events` consumer (group `analytics-cac`, idempotent on `idempotency_key`). Tenant via `X-Tenant-Slug` header (resolved through identity-service) or `?tenant=<uuid>`. Campaign spend is joined from booking-service (`GET /internal/campaigns/{id}/spend-sum` via Dapr) and **never fails the summary** — outages mark `data_quality: "spend_unavailable"` and count as 0. Full contract: `docs/cac-analytics-api.md`. |

## Usage metering (Wave 5 #9) — monetization hook

This is the data side of the usage-metered API monetization track
(STRATEGY.md §2 item 2): booking-service emits usage records
`{tenant_id, metric, value, ts, meta}` on `opendesk.usage.events` (v1
metric: `booking` — value 1 per booking lifecycle event), this service lands them in
`bronze.usage_events`, dbt rolls them into `gold.usage_daily`, and
`GET /v1/metering` serves per-tenant aggregates for billing/quota callers
(e.g. $/1k calls with tiered rate limits — the APISIX plan-tier limits are
already in place).

**Honest v1 scope (sparse data is expected):**

- **payments-service emits no usage records** — it is Rust and has no
  toolchain in this wave; payment metrics stay deferred.
- **voice-agent-runtime call-minutes are emitted by booking/conversation
  side paths only where available**; the voice runtime's own emission is
  tracked separately (another workstream).
- Every consumer of this pipeline — the dbt mart, the metering endpoint —
  therefore handles sparse/absent data gracefully: missing table, tenant
  without rows, empty range → empty result, never an error.

## Environment variables

| Var | Default | Purpose |
|---|---|---|
| `KAFKA_BOOTSTRAP_SERVERS` | `kafka:9092` | Kafka brokers (core compose). |
| `KAFKA_GROUP_ID` | `analytics-pipeline` | Consumer group. |
| `TOPIC_BOOKING_EVENTS` | `opendesk.booking.events` | Source topic → `bronze.booking_events`. |
| `TOPIC_PAYMENT_EVENTS` | `opendesk.payments.events` | Source topic → `bronze.payment_events`. |
| `TOPIC_TRANSCRIPTS` | `opendesk.conversation.transcripts` | Source topic → `bronze.transcripts`. |
| `TOPIC_USAGE_EVENTS` | `opendesk.usage.events` | Source topic → `bronze.usage_events` (Wave 5 #9). |
| `BATCH_SIZE` | `500` | Flush threshold per topic buffer. |
| `FLUSH_INTERVAL` | `15` (seconds) | Max buffer age before flush. |
| `ICEBERG_REST_URI` | `http://iceberg-rest:8181` | Iceberg REST catalog. |
| `ICEBERG_WAREHOUSE` | `s3://lake/warehouse` | Warehouse root (bucket `lake`). |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | `minioadmin` / `minioadmin` | MinIO credentials. |
| `AWS_ENDPOINT_URL` | `http://minio:9000` | S3 endpoint (mapped to `s3.endpoint`). |
| `AWS_REGION` | `us-east-1` | S3 region. |
| `AUTO_CREATE_TABLES` | `true` | Create `bronze` namespace + tables on boot. |
| `STARTUP_RETRY_SECONDS` / `STARTUP_MAX_ATTEMPTS` | `5` / `60` | Dependency wait loop. |
| `PORT` / `HOST` | `7009` / `0.0.0.0` | Sidecar bind. |
| `CAC_EVENTS_TOPIC` | `cac.events` | SPEC-W13 funnel event topic. |
| `CAC_EVENTS_GROUP` | `analytics-cac` | SPEC-W13 consumer group. |
| `PG_DSN` / `PG_DATABASE` | `postgres://opendesk:opendesk@postgres:5432` / `analytics_meta` | CAC rollup store (base DSN + database). |
| `PG_MIN_SIZE` / `PG_MAX_SIZE` | `1` / `4` | CAC store asyncpg pool. |
| `DAPR_HOST` / `DAPR_HTTP_PORT` | `daprd-analytics` / `3500` | Dapr sidecar (spend join + tenant resolution). |
| `BOOKING_APP_ID` / `IDENTITY_APP_ID` | `booking` / `identity` | Dapr app-ids. |
| `TENANT_CACHE_TTL_SECONDS` | `300` | Tenant slug cache TTL (stale-on-outage). |
| `CAC_CONSUMER_ENABLED` | `true` | `false` = bronze sink only; `/v1/cac/summary` answers 503. |

## Run

```bash
# local (needs Kafka + lakehouse compose tiers up, reachable hostnames)
pip install .[dev]
python -m analytics_pipeline.main

# tests (pure stdlib fallbacks included; no pytest required)
python tests/test_mapping.py && python tests/test_dbt_conformance.py

# docker
docker build -t opendesk/analytics-pipeline .
docker run --rm --network opendesk -p 7009:7009 opendesk/analytics-pipeline
```

Verify data lands:

```sql
-- via Trino on :8088
SELECT * FROM iceberg.bronze.booking_events LIMIT 5;
```
