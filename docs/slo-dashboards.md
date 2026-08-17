# SLO Dashboards — Grafana Panels per SLO (SPEC-W15, Agent D)

Panel inventory for the CAC-program NFRs, mapped to **real Prometheus metric
names found in this repo**. Where no metric exists today the panel says
`needs instrumentation: <proposed name>` rather than inventing a name that
nothing emits.

**Wiring that already exists:**

- Scrape config: `infra/observability/prometheus/prometheus.yml` — jobs
  `identity, booking, notification, payments, gateway-edge, voice,
  conversation, knowledge, analytics, crm-sync, apisix, otel-collector`.
- APISIX exposes plugin metrics on `:9091/apisix/prometheus/metrics`
  (global rule `prometheus: {prefer_name: true}`,
  `infra/apisix/apisix.yaml`; plugin config `infra/apisix/config.yaml`).
- Dashboard provisioning: `infra/observability/grafana/provisioning/`;
  existing dashboards `infra/observability/dashboards/{platform-overview,
  ai-voice, temporal-saga, concurrency-ceilings}.json`. New SLO dashboards
  belong alongside them (this doc defines the panels; shipping the JSON is
  follow-up work).
- Compose: Prometheus + Grafana in `infra/docker-compose.observability.yml`.

**Honesty notes up front (verified by grep):**

1. The Go services scraped by jobs `identity`, `booking`, `notification`,
   `payments` expose `/healthz` only — **no `/metrics` handler** (checked
   `services/identity-service/internal/httpapi/server.go`,
   `services/booking-service/internal/httpapi/server.go`). Those scrape
   jobs are effectively `up == 0`/empty until per-service instrumentation
   lands. Real per-service metrics today come from: messaging-gateway
   (`messaging_gateway_sends_total`, `internal/metrics/metrics.go`),
   gateway-edge (`gateway_*`, `src/metrics.rs`), voice-agent-runtime
   (`voice_*`, `app/metrics.py`), analytics-pipeline (`analytics_*`,
   `analytics_pipeline/metrics.py`), crm-sync (`crm_sync_*`,
   `internal/metrics/metrics.go`).
2. `notifications_suppressed_total{reason}` exists as a process-local
   counter in notification-worker (`internal/pacer/guards.go`) but is **not
   yet exposed over HTTP** — the code comment says "a metrics endpoint can
   scrape them later".
3. W41 addition: the funds/booking hot paths now have an **offline measured
   baseline** (`tests/perf/RESULTS.md`, produced by `tests/funds-e2e` +
   `tests/perf/aggregate.py` + Go store benchmarks) with explicit budgets in
   `docs/performance-budgets.md`. That is a point-in-time release gate — it
   does NOT replace the per-service Prometheus histograms below, which remain
   needed for continuous production monitoring.
4. Some existing dashboard JSON references metrics **no service emits**
   (`voice_chat_turns_total`, `voice_llm_calls_total`,
   `voice_llm_errors_total`, `voice_llm_call_duration_seconds`,
   `voice_stt_duration_seconds`, `voice_tts_duration_seconds` in
   `ai-voice.json`; `opendesk_saga_*` in `temporal-saga.json`). The names
   actually emitted by the voice runtime are the `voice_*_latency_seconds`
   family in `services/voice-agent-runtime/app/metrics.py`. Panels below
   use the emitted names.

---

## SLO 1 — Customer API latency: p50 ≤ 300 ms, p99 ≤ 1.5 s

| # | Panel | Metric / PromQL sketch | Status |
| --- | --- | --- | --- |
| 1.1 | Gateway-side p50/p99 per route | `histogram_quantile(0.50, sum by (le, route) (rate(apisix_http_latency{type="request"}[5m])))` and `0.99` — from the APISIX prometheus plugin (`prefer_name: true` gives route names) | **real** (`apisix_http_latency`, plugin-provided via `infra/apisix/apisix.yaml`) |
| 1.2 | Upstream latency split | `histogram_quantile(0.99, sum by (le, service) (rate(apisix_http_latency{type="upstream"}[5m])))` — gateway↔service share of 1.1 | **real** (same plugin metric, `type` label) |
| 1.3 | Per-service server latency (identity `/v1/tenants`, booking `/v1/bookings`, conversation, knowledge) | needs instrumentation: `http_server_request_duration_seconds{service,route,status}` histogram in each Go service (chi middleware next to `middleware.RequestID` in `internal/httpapi/server.go`). W41: booking create / public booking / tenant provisioning latency is now measured offline per release — `tests/perf/RESULTS.md` vs budgets B1/B2 (`docs/performance-budgets.md`) | **needs instrumentation** (offline baseline exists via `tests/perf`; continuous histogram still pending) |
| 1.4 | Error budget companion: 5xx ratio | `sum(rate(apisix_http_status{status=~"5.."}[5m])) / sum(rate(apisix_http_status[5m]))` | **real** (`apisix_http_status`, plugin-provided) |

## SLO 2 — Kafka publish latency: p95 ≤ 50 ms

| # | Panel | Metric / PromQL sketch | Status |
| --- | --- | --- | --- |
| 2.1 | Producer publish p95 per topic | needs instrumentation: `kafka_produce_duration_seconds{service,topic}` histogram in the producing services (booking, conversation, payments, identity, crm-sync producers) | **needs instrumentation** |
| 2.2 | Broker health / under-replicated partitions | needs instrumentation: deploy a Kafka exporter (or Strimzi metrics in k3s, cf. `deploy/k3s/mirror-maker2.yaml`) — `kafka_server_replicamanager_underreplicatedpartitions`, `kafka_network_requestmetrics_totaltimems{request="Produce"}` | **needs instrumentation** (no kafka exporter job in `prometheus.yml`) |
| 2.3 | End-to-end sanity proxy: edge ingest publish rate | `rate(gateway_kafka_messages_total[5m])` | **real** (`services/gateway-edge/src/metrics.rs`) |

## SLO 3 — Ingress + spine availability ≥ 99.5 %

"Ingress" = APISIX edge + admin/public traffic; "spine" = the gateway-edge
WS/Kafka/Fluvio ingest path.

| # | Panel | Metric / PromQL sketch | Status |
| --- | --- | --- | --- |
| 3.1 | Target liveness matrix | `up{job=~"apisix|gateway-edge|identity|booking|conversation|voice|analytics"}` (jobs from `infra/observability/prometheus/prometheus.yml`) | **real** (Prometheus-generated `up`) |
| 3.2 | Ingress success ratio (30d burn) | `1 - sum(rate(apisix_http_status{status=~"5.."}[5m])) / sum(rate(apisix_http_status[5m]))` with 99.5 % threshold line | **real** (`apisix_http_status`) |
| 3.3 | Spine: active WS ingest connections | `gateway_ws_connections_active` | **real** (`gateway-edge/src/metrics.rs`) |
| 3.4 | Spine: published vs dropped | `rate(gateway_events_published_total[5m])` vs `rate(gateway_events_dropped_slow_consumer_total[5m])` and `rate(gateway_events_no_subscriber_total[5m])` | **real** (same file) |
| 3.5 | Spine: auth failure spike | `rate(gateway_auth_failures_total[5m])` | **real** (same file) |
| 3.6 | Spine: analytics consumer alive | `analytics_consumer_running` (1 = consumer loop running) | **real** (`services/analytics-pipeline/analytics_pipeline/metrics.py`) |
| 3.7 | Fluvio edge ingest rate | `rate(gateway_fluvio_records_total[5m])` | **real** (`gateway-edge/src/metrics.rs`) |

## SLO 4 — Financial writes success ≥ 99.95 %

| # | Panel | Metric / PromQL sketch | Status |
| --- | --- | --- | --- |
| 4.1 | Ledger write success ratio | needs instrumentation: `payments_ledger_writes_total{op,result}` counter in payments-service (`src/ledger/tigerbeetle.rs` — the Rust service has no metrics module today) | **needs instrumentation** |
| 4.2 | Ledger write latency p99 | needs instrumentation: `payments_ledger_write_duration_seconds{op}` histogram (same location). W41: deposit hold/capture latency is measured offline per release — `tests/perf/RESULTS.md` vs budgets B6/B7 (`docs/performance-budgets.md`); measured with `LEDGER_IMPL=sim`, so production TigerBeetle latency still requires this histogram | **needs instrumentation** (offline sim-ledger baseline exists via `tests/perf`; production histogram still pending) |
| 4.3 | Payment event flow proxy | `rate(analytics_messages_consumed_total{topic="opendesk.payments.events"}[5m])` | **real** (analytics sink) |
| 4.4 | TigerBeetle cluster health | needs instrumentation: TigerBeetle exposes no Prometheus endpoint; probe via the payments-service healthz or a TB stats exporter — proposed `tigerbeetle_replica_status{replica}` | **needs instrumentation** |

## SLO 5 — CAC-by-channel refresh < 30 s

Time from a `cac.events` event to it being visible in
`GET /v1/cac/summary` (`services/analytics-pipeline/analytics_pipeline/server.py`;
API contract `docs/cac-analytics-api.md`).

| # | Panel | Metric / PromQL sketch | Status |
| --- | --- | --- | --- |
| 5.1 | Rollup consumer lag (cac.events) | `max(analytics_consumer_lag{topic="cac.events"})` | **real** |
| 5.2 | Rollup apply rate | `rate(analytics_cac_events_processed_total{outcome="applied"}[5m])` (with `outcome="replay"` as dedupe overlay) | **real** |
| 5.3 | Spend-lookup health | `rate(analytics_cac_spend_lookups_total{outcome="unavailable"}[5m])` | **real** |
| 5.4 | True event→visible lag | needs instrumentation: `cac_rollup_refresh_lag_seconds` (histogram/gauge of `now - event.occurred_at` at rollup apply time, analytics-pipeline Postgres rollup consumer) | **needs instrumentation** (lag gauge 5.1 is message-count lag, not seconds; the <30 s SLO needs 5.4) |
| 5.5 | Lakehouse gold freshness (dbt `daily_cac_by_channel`) | `histogram_quantile(0.95, rate(analytics_flush_duration_seconds_sum[5m]) / rate(analytics_flush_duration_seconds_count[5m]))` per table + `rate(analytics_flushes_total{outcome="error"}[5m])` | **real** (covers the Kafka→Iceberg leg, not the dbt run itself) |

## SLO 6 — USSD session success

Session contract: Africa's Talking POSTs cumulative `text`; `CON`/`END`
replies; 180 s session TTL (see `docs/runbook-wave12.md` §1,
`services/messaging-gateway/internal/channel/ussd.go`).

| # | Panel | Metric / PromQL sketch | Status |
| --- | --- | --- | --- |
| 6.1 | Session outcomes | needs instrumentation: `messaging_gateway_ussd_sessions_total{outcome="completed|timeout|error"}` counter (increment on `END ` vs TTL-expiry in `internal/channel/ussd.go`) | **needs instrumentation** |
| 6.2 | Turn latency p95 | needs instrumentation: `conversation_ussd_turn_duration_seconds` histogram around `POST /v1/ussd/turns` (conversation-service `app/ussd.py`) | **needs instrumentation** |
| 6.3 | USSD callback error ratio (gateway view) | `sum(rate(apisix_http_status{route=~".*messaging.*", status=~"5.."}[5m])) / sum(rate(apisix_http_status{route=~".*messaging.*"}[5m]))` | **real** once the USSD webhook route is labelled via APISIX `prefer_name` |

## SLO 7 — SMS delivery ≤ 4.5 s p50

NG aggregator path: AfricasTalking/Termii/eBulkSMS + failover
(`services/messaging-gateway/internal/provider/`).

| # | Panel | Metric / PromQL sketch | Status |
| --- | --- | --- | --- |
| 7.1 | Send attempts by provider/result | `sum by (provider, result) (rate(messaging_gateway_sends_total[5m]))` | **real** (`internal/metrics/metrics.go`, `GET /metrics` on messaging-gateway) |
| 7.2 | Send latency p50/p95 per provider | needs instrumentation: `messaging_gateway_send_duration_seconds{provider}` histogram alongside `IncSend` | **needs instrumentation** |
| 7.3 | DLR-confirmed delivery ratio + failover rate | needs instrumentation: `messaging_gateway_dlr_total{provider,status}` counter on aggregator delivery-receipt callbacks (no DLR webhook handler exists today) | **needs instrumentation** |
| 7.4 | Compliance suppression overlay | `notifications_suppressed_total{reason}` — counter exists process-locally in `internal/pacer/guards.go` | **needs instrumentation** (expose over HTTP; the counter and `{reason}` label set already exist) |

## SLO 8 — KYC resolution ≤ 8 s p95

Consent-gated `POST /v1/kyc/resolve` (`services/kyc-service/internal/httpapi/server.go`,
`docs/kyc.md`).

| # | Panel | Metric / PromQL sketch | Status |
| --- | --- | --- | --- |
| 8.1 | Resolve latency p95 | needs instrumentation: `kyc_resolve_duration_seconds{provider,result}` histogram in kyc-service (no `/metrics` today) | **needs instrumentation** |
| 8.2 | Resolve outcome rate | needs instrumentation: `kyc_resolutions_total{result="resolved|rejected|error|consent_denied"}` counter (same location) | **needs instrumentation** |
| 8.3 | Result-topic flow proxy | `rate(analytics_messages_consumed_total{topic="opendesk.kyc.resolved.v1"}[5m])` | **real** (analytics sink counts the emitted results; `opendesk.kyc.resolved.v1` is emitted by kyc-service per `infra/kafka/create-topics.sh`) |
| 8.4 | Gateway-side KYC p95 (stopgap until 8.1) | `histogram_quantile(0.95, sum by (le) (rate(apisix_http_latency{type="request", route=~".*kyc.*"}[5m])))` | **real** (includes network + auth overhead; overcounts vs. service-internal latency) |

---

## Voice/CAC quality panels already backed by real metrics (bonus)

These come straight from `services/voice-agent-runtime/app/metrics.py` and
are useful on the same dashboard board:

- Emergency-lane rate: `rate(voice_emergency_sessions_total[5m])`
- TTS provider failures: `sum by (provider) (rate(tts_provider_failures_total[5m]))`
- Voice pipeline latency: `voice_stt_latency_seconds`, `voice_llm_latency_seconds`,
  `voice_tts_latency_seconds` histograms (`histogram_quantile` over
  `*_bucket`)
- Tool-call outcomes: `sum by (tool, result) (rate(voice_tool_calls_total[5m]))`
- Active calls: `voice_active_sessions`

## Implementation notes

1. PromQL sketches assume the label names emitted today
   (`provider,result,topic,table,outcome,op,reason,...`); verify against a
   live `/metrics` scrape before locking dashboard JSON.
2. The four "needs instrumentation" clusters (per-service HTTP histograms,
   Kafka producer metrics, payments/ledger metrics, USSD/KYC/DLR counters)
   are deliberately additive: each is a small module next to the existing
   hand-rolled registries (`messaging-gateway/internal/metrics/metrics.go`
   and `voice-agent-runtime/app/metrics.py` are the style to copy — no new
   dependencies).
3. Dashboard JSON should be dropped into `infra/observability/dashboards/`
   (auto-provisioned) following the existing files' schema.
