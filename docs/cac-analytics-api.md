# CAC Analytics API — analytics-pipeline :7009 (SPEC-W13 §5)

Realtime CAC (customer acquisition cost) rollups consumed from the `cac.events`
Kafka topic and served over the analytics sidecar REST API. The admin-web BFF
reaches this service directly (`/api/analytics/<rest>` →
`http://analytics:7009/<rest>`); it is NOT routed through APISIX.

Dual-path (contract §5): these Postgres rollups are the **realtime** source;
the lakehouse gold tables (`cac_gold.daily_cac_by_channel`,
`cac_gold.daily_cac_by_lga`, Iceberg — Agent C) are the batch-verified source,
reconciled nightly. Numbers can drift briefly between the two; gold wins.

## Pipeline

```
cac.events (CloudEvent com.opendesk.cac.FunnelEvent, contract §2)
  └── CacConsumer (group analytics-cac, enable_auto_commit=False)
        └── CacStore.record_event  (ONE Postgres tx per event)
              ├── cac_processed_events   (tenant_id, idempotency_key)  ← dedupe
              ├── cac_rollup_channel     (tenant_id, day, channel)     ← +leads/+conversions/+revenue
              ├── cac_rollup_lga         (tenant_id, day, lga_id)      ← +leads/+conversions/+revenue
              └── cac_campaign_channel   (tenant_id, campaign_id)      ← first-touch channel mapping
```

**Idempotency**: offsets commit only AFTER the Postgres transaction; the
`cac_processed_events` PK `(tenant_id, idempotency_key)` is written in the SAME
transaction as the rollup upserts, so at-least-once redelivery is deduped
exactly (replay → no-op). Poison-pill messages (unparseable or off-contract
`event_name`) are logged, counted, and committed past — the bronze sink keeps
the raw copy.

**Rollup rules** (`analytics_pipeline/cac_events.py`):
- `lead_created` → `leads + 1`
- `converted` → `conversions + 1`, `revenue_ngn += amount_ngn`
- `first_txn` → `revenue_ngn += amount_ngn` (revenue only — `converted` owns the conversion)
- `contacted | opted_in | qualified | lost` → processed (idempotency) but no counters
- channel rollup requires `channel`; LGA rollup requires `lga_id`; campaign
  mapping requires `channel + campaign_id` (first seen wins, never overwritten —
  mirrors first-touch attribution, contract §3).

**Tenant RLS**: all four tables are `ENABLE + FORCE ROW LEVEL SECURITY` with
`tenant_isolation USING (tenant_id = current_setting('app.tenant_id', true)::uuid)`
in the `analytics_meta` database (SPEC §7 one-DB-per-service). Every statement
runs inside a transaction that first does
`SELECT set_config('app.tenant_id', $1, true)` — the same idiom as
conversation-service `app/db.py` and booking-service `internal/store/store.go`.
Schema bootstrap is idempotent DDL on startup (`pg_policies` existence check
for policies, same as booking-service `store/incidents.go`).

## GET /v1/cac/summary?from&to

Tenant comes from the `X-Tenant-Slug` header (resolved to the tenant UUID via
identity-service `GET /v1/tenants/{slug}` over Dapr, TTL-cached with
stale-on-outage fallback — same pattern as booking-service
`bookingops/resolver.go`) or, for parity with the other sidecar routes, a
`?tenant=<uuid>` query param. `from`/`to` are optional inclusive ISO dates
(400 on malformed or inverted ranges).

```json
{
  "tenant": "11111111-1111-1111-1111-111111111111",
  "from": "2026-01-01",
  "to": "2026-01-31",
  "by_channel": [
    {"channel": "whatsapp", "spend_ngn": 50000.0, "leads": 100,
     "conversions": 10, "cac_ngn": 5000.0}
  ],
  "by_lga": [
    {"lga_id": 42, "leads": 60, "conversions": 6,
     "spend_ngn": null, "cac_ngn": null, "geom": null}
  ],
  "blended_cac_ngn": 4666.67,
  "ltv_ngn": 24800.0,
  "payback_days_estimate": 5.8,
  "data_quality": "ok",
  "totals": {"spend_ngn": 70000.0, "leads": 150, "conversions": 15,
             "revenue_ngn": 372000.0}
}
```

Field semantics:

| Field | Meaning |
|---|---|
| `by_channel[].spend_ngn` | Campaign spend summed over the tenant's campaigns on that channel (see spend join). `0.0` when unknown. |
| `by_channel[].cac_ngn` | `spend_ngn / conversions`; **null when conversions = 0**. |
| `by_lga[].spend_ngn / cac_ngn` | **Always null** — spend is booked per campaign/channel; no honest per-LGA allocation exists yet. |
| `by_lga[].geom` | **Always null this wave** — no LGA boundary endpoint exists in booking-service (checked `internal/store/geolocations.go` + httpapi routes; adding one is Agent A's turf and was explicitly out of scope). Null-geom rows still appear; the dashboard renders them in a side table. |
| `blended_cac_ngn` | `total spend / total conversions`; null when conversions = 0. |
| `ltv_ngn` | Avg revenue per converted lead = `Σ amount_ngn (converted/first_txn) / conversions`; **null when no amounts** (honest "—" on the dashboard). |
| `payback_days_estimate` | `blended_cac_ngn / (ltv_ngn / period_days)`; period = `[from,to]` length, else the data span. Null when CAC, LTV or period is missing. |
| `data_quality` | `"ok"` or `"spend_unavailable"` (one or more spend lookups failed — those campaigns count as 0 spend). |
| `totals` | Additive convenience block (not part of contract §5). |

### Spend join (resilient by contract)

Spend lives in booking-service (`POST /v1/campaigns/{id}/spend`, contract §4).
The summary loops the tenant's known `campaign_id → channel` mappings and calls
booking-service via Dapr invoke:

```
GET /internal/campaigns/{id}/spend-sum?from=YYYY-MM-DD&to=YYYY-MM-DD
```

**CONTRACT ASSUMPTION (flagged to Agent A)**: expected response
`{"campaign_id": ..., "spend_ngn": <number>}`. The parser also tolerates
`total_spend_ngn` / `amount_ngn` keys and a bare numeric body. **A 404 or
unreachable booking-service never fails the summary** — that campaign's spend
counts as 0 and the response carries `data_quality: "spend_unavailable"`.

Limitation (documented, honest v1): a campaign with spend but **zero funnel
events ever seen** has no `campaign → channel` mapping, so its spend is not
included. First-touch mapping is written on the first event carrying both
`campaign_id` and `channel`.

**ASSUMPTION (payback margin)**: v1 has no COGS/gross-margin signal, so
`amount_ngn` on `converted`/`first_txn` events is treated as the gross margin
proxy. When a real margin feed lands, only `ltv_ngn`/`payback_days_estimate`
change meaning; the API shape does not.

## Errors

| Status | When |
|---|---|
| 400 | missing tenant (`X-Tenant-Slug`/`?tenant=`), malformed or inverted `from`/`to` |
| 404 | unknown tenant slug (identity-service 404) |
| 502 | identity-service unreachable (no cache entry), or Postgres rollup store error |
| 503 | CAC module disabled (`CAC_CONSUMER_ENABLED=false`) |

## Environment (additive)

| Var | Default | Purpose |
|---|---|---|
| `CAC_EVENTS_TOPIC` | `cac.events` | Funnel event topic (contract §7). |
| `CAC_EVENTS_GROUP` | `analytics-cac` | Consumer group (contract §7). |
| `PG_DSN` / `PG_DATABASE` | `postgres://opendesk:opendesk@postgres:5432` / `analytics_meta` | Rollup store (base DSN + database, conversation-service convention). |
| `PG_MIN_SIZE` / `PG_MAX_SIZE` | `1` / `4` | asyncpg pool. |
| `DAPR_HOST` / `DAPR_HTTP_PORT` | `daprd-analytics` / `3500` | Sidecar for spend join + tenant resolution. |
| `BOOKING_APP_ID` / `IDENTITY_APP_ID` | `booking` / `identity` | Dapr app-ids. |
| `TENANT_CACHE_TTL_SECONDS` | `300` | Tenant slug resolution cache TTL (stale-on-outage). |
| `CAC_CONSUMER_ENABLED` | `true` | `false` runs the bronze sink only; `/v1/cac/summary` answers 503. |

## Metrics (additive, GET /metrics)

- `analytics_cac_events_processed_total{outcome="applied|replay"}`
- `analytics_cac_spend_lookups_total{outcome="ok|unavailable"}`

## Tests

`tests/test_cac_events.py` (parsing), `tests/test_cac_store.py` (idempotent
record_event + RLS tenant-tx idiom, fake asyncpg), `tests/test_cac_consumer.py`
(consumer dedupe/poison-pill/no-commit-on-error), `tests/test_cac_summary.py`
(rollup math + spend-join resilience), `tests/test_cac_api.py` (endpoint
contract via FastAPI TestClient). All offline — no live Postgres/Kafka/Dapr.
