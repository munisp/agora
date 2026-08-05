# fraud-engine

Graph-based fraud detection over the W28 tenant knowledge graph (FalkorDB).
**SPEC-W30 §4 WS-B.** Detection ≠ punishment: detectors create `Alert` nodes
and (only for F1/F2/F3 at high severity) set `Person.quarantine = true`.
Humans adjudicate via the graph-service alerts router (WS-C). **No
auto-erasure anywhere** — fraud suspects retain audit rights; NDPA erasure
remains a separate consent-driven flow.

## Run

```bash
pip install -r requirements.txt
FALKORDB_HOST=graph-db FALKORDB_PORT=6379 KAFKA_ENABLED=true \
  KAFKA_BOOTSTRAP_SERVERS=kafka:9092 python -m fraud_engine.main   # :7017
```

Docker: `docker build -t fraud-engine .` (Python 3.11, internal-only port 7017).

## Endpoints

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | liveness + graph reachability |
| `POST /v1/detect/run` `{tenant_id?, detector?}` | manual run (one tenant, one detector, or full sweep when omitted) |
| `GET /v1/detect/status` | last run report, thresholds, detector list |

Cadence (SPEC §3): Kafka event triggers (D3 on capture/lead events, D5 on
consent/messaging events — topics from `FRAUD_KAFKA_TOPICS`, same topics as
graph-sync) + a full sweep every `FRAUD_SWEEP_MINUTES` (15).

## Detectors (SPEC-W30 §3)

| Detector | Type | Rule | Severity |
|---|---|---|---|
| D1 `d1_referral_cycle` | `referral_cycle` | `REFERRED*2..4` cycles | high if ring ≥3 persons AND any non-cancelled booking (reward-bearing conversion), else medium |
| D2 `d2_sybil_cluster` | `sybil_cluster` | same agent + same Location + within `SYBIL_WINDOW_MIN` (60) + name-embedding cosine ≥ `SYBIL_SIM_THRESHOLD` (0.98) | medium; cluster ≥ `SYBIL_HIGH_SIZE` (5) → high |
| D3 `d3_capture_velocity` | `capture_velocity` | agent captures > `CAPTURE_VELOCITY_MAX` (30) in any rolling `CAPTURE_WINDOW_MIN` (60) | medium; over-threshold in `CAPTURE_SUSTAINED_WINDOWS` (3) consecutive windows → high |
| D4 `d4_geo_impossibility` | `geo_impossibility` | consecutive CAPTURED_AT points by same agent imply speed > `MAX_TRAVEL_KMH` (120), haversine | medium; repeat offender (≥2 impossible jumps in lookback) → high |
| D5 `d5_consent_backdating` | `consent_backdating` | CONSENTED `granted_at` AFTER first MESSAGED `at` for same person+purpose | **high always** (compliance-critical) |
| D6 `d6_ghost_booking` | `ghost_booking` | ≥ `GHOST_MIN` (3) create→cancel flash cycles ≤ `GHOST_WINDOW_MIN` (10) by same staff in a day | medium |
| D7 `d7_gnn_anomaly` | `gnn_anomaly` | consumes `Person.risk_score` ≥ `ANOMALY_ALERT_THRESHOLD` (0.9), written by the W29 sweep | low; ≥ `ANOMALY_MEDIUM_THRESHOLD` (0.97) → medium |

Dedup/idempotency: `alert_id = type:tenant:person:dedup_key`, MERGEed
(SPEC §5 gate 5: N sweeps → ≤1 open alert per (type,person)). CloudEvents
`com.opendesk.fraud.AlertRaised` (id `tenant:alert:alert_id`, extension
`tenantid`, data `{alert_id, type, severity, person_id?, agent_id?}`) are
published to `opendesk.fraud.alerts.v1` **only for newly created alerts**.
Every alert carries replayable `evidence` JSON (gate 3): node ids, cycle
paths, distances/speeds, timestamps, and the severity rule that fired.

## Quarantine (SPEC §3)

Auto-quarantine sets `Person.quarantine = true` **only** for high-severity
`referral_cycle` (F1), `sybil_cluster` (F2), `capture_velocity` **and**
`geo_impossibility` (F3 — §0 maps both D3 and D4 to F3 "agent lead
fabrication"). D5 never auto-quarantines (routes to the compliance queue via
its high alert). There is **no unquarantine path** in this service —
resolution/un-quarantine lives in graph-service. Quarantined persons remain
query-visible but audience-ineligible (the W28 gate, `p.quarantine = false`
in every audience predicate).

## Tenant isolation (gate 1)

Every Cypher this service emits — reads, MERGEs, quarantine writes — binds
`$tenant_id`; `assert_tenant_bound` refuses any statement that doesn't, and
the test suite inspects every recorded statement. The single exempt
statement is the sweep's `MATCH (t:Tenant) RETURN t.tenant_id` discovery,
which has no tenant parameter by nature. Direct FalkorDB writes (Alert
nodes, FLAGGED edges, quarantine) are tenant-verified before write.

## Schema expectations (inputs)

Detectors read these properties written by graph-sync / W29 (all nodes also
carry `tenant_id`):

- `Person.name_embedding` (Ollama), `Person.risk_score` (W29), `Person.risk_flags` (maintained here)
- `Contact.captured_by` (staff id), `Contact.captured_at`; `Location.lat/lon/lga/ward`
- `CONSENTED.granted_at` (or `Consent.granted_at`), `MESSAGED.at/purpose`
- `Booking.created_by` (staff id), `Booking.cancelled_at`, `Booking.status`

## Tests

```bash
pip install -r requirements-dev.txt
python -m pytest tests -q     # 66 tests, no live FalkorDB/Kafka needed
```

`tests/fakes.py` is an in-memory property graph + marker-dispatched fake
`GraphClient` that evaluates the exact Cyphers the package emits (with real
tenant scoping), so detector behavior, dedup, quarantine, event and API
tests are all behavioral without infrastructure.

## Deviations / decisions flagged for the integrator

1. SPEC-W30 §0 says "`Person.quarantined=true`" but the W28 audience gate
   reads `p.quarantine` (docs/graph.md §3.2). We set `quarantine` (plus
   audit props `quarantined_at`, `quarantine_reason`) so the W28 gate and
   the W30 e2e "money test" work unchanged.
2. F3-high is interpreted as D3 **and** D4 at high (§0 taxonomy maps both
   to F3), so `geo_impossibility`-high auto-quarantines.
3. D6's secondary F5 signal ("booking without any contact or payment
   trail") is not implemented in v1 — the create→cancel burst rule is the
   SPEC'd v1 rule.
4. Agent/staff attribution relies on `Contact.captured_by` /
   `Booking.created_by` properties (staff are not graph nodes in v1, per
   SPEC §2); graph-sync must populate them from capture/booking events.
