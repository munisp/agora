# SPEC-W30 — Fraud & Trust Intelligence (Graph-Based Fraud Detection)

**Status:** contract · **Depends on:** W28 (tenant knowledge graph), W29 (scoring infrastructure) · **Date:** 2026-08-05

## §0 Fraud taxonomy — what the graph catches that row-based systems cannot

| ID | Fraud pattern | Why rows miss it | Graph signal |
|----|---------------|------------------|--------------|
| F1 | **Referral rings** — actors fabricate circular referral chains to harvest rewards/airtime | Each referral row looks valid alone | Cycles in `REFERRED` subgraph; ring clustering coefficient |
| F2 | **Sybil / duplicate fabrication** — one human as many Person nodes to multiply rewards or inflate lead counts | Phones differ per fake identity | Near-identical name embeddings + same capture agent + same location + burst timing |
| F3 | **Agent lead fabrication** — field agents invent captures to hit quotas (campaigns: fake voter contacts) | Counts look fine per day | Capture-velocity spikes per agent; GEO-impossibility between consecutive CAPTURED_AT points |
| F4 | **Consent backdating / forgery** — consent records created after first contact to retroactively legalize sends | Timestamps valid in isolation | CONSENTED edge `granted_at` AFTER first `MESSAGED.at` for same person+purpose |
| F5 | **Ghost bookings** — staff create/cancel bookings to game commissions or fake conversion metrics | Each booking is a valid row | Create→cancel cycles clustered per staff/tenant; booking without any contact or payment trail |
| F6 | **Anomalous actors (GNN)** — novel fraud that matches no rule | Unknown unknowns | GNN/embedding anomaly score: persons whose neighborhood structure is an outlier for that tenant |

**Enforcement model:** detection ≠ punishment. Detectors create `Alert` nodes and (for high-severity) set `Person.quarantined=true` — reusing the W28 gate: quarantined persons remain query-visible but are **audience-ineligible**. Humans adjudicate in the admin queue. **No auto-erasure ever** (fraud suspects retain audit rights; NDPA erasure remains a separate consent-driven flow).

## §1 Architecture

```
Kafka (booking/identity/transcripts events — same topics as graph-sync)     FalkorDB
   │                                                                            ▲
   ▼                                                                            │ read: detector Cyphers
fraud-engine (new Python service :7017)  ── writes Alert nodes + quarantine ────┘
   │                                                                            (direct write, tenant-verified)
   ├── emits CloudEvents com.opendesk.fraud.AlertRaised → opendesk.fraud.alerts.v1
   └── POST /v1/graph/internal/alerts/resolve hooks (via graph-service)
graph-service: NEW alerts router (list/triage/resolve, tenant-scoped, JWT)
admin-web: /alerts queue page + risk badges on Person-360
graph-ml (W29): risk_score head joins the scoring sweep (heuristic outlier score v1; GNN anomaly when backend=gnn)
```

## §2 Graph schema v3 additions

**New node:** `(a:Alert {alert_id, tenant_id, type, severity, status, person_id?, agent_id?, evidence, created_at, resolved_at?, resolved_by?, resolve_reason?})`
- `type` ∈ `referral_cycle|sybil_cluster|capture_velocity|geo_impossibility|consent_backdating|ghost_booking|gnn_anomaly`
- `severity` ∈ `low|medium|high` · `status` ∈ `open|confirmed|dismissed`
- `evidence` = JSON string of the matched pattern (node ids, cycle path, distances, timestamps) — auditors must be able to replay why it fired.

**New edges:** `(a:Alert)-[:FLAGGED]->(p:Person)` · `(a:Alert)-[:FLAGGED_AGENT]->(agent_ref)` — agent identified by staff id string property on Alert (staff are not graph nodes in v1).

**Person new properties:** `risk_score` float 0..1 (written by W29 sweep), `risk_flags` string list (active detector types).

## §3 Detectors (fraud-engine, Agent B)

Each detector = parameterized Cypher + severity rule + dedup (one open alert per type+person; MERGE on `alert_id = type:tenant:person:dedup_key`). All read queries bind `$tenant_id`. Cadence: event-driven triggers on Kafka where natural (F3 velocity on capture events, F4 on consent/messaging events) + full sweep every `FRAUD_SWEEP_MINUTES` (15).

- **D1 referral_cycle:** `MATCH p=(a:Person {tenant_id:$t})-[:REFERRED*2..4]->(a) RETURN p` — severity high if ring ≥3 persons with any reward-bearing conversion, else medium.
- **D2 sybil_cluster:** persons created by same agent within `SYBIL_WINDOW_MIN` (60), same Location, name-embedding cosine ≥ `SYBIL_SIM_THRESHOLD` (0.98, via Ollama embeddings already stored by graph-sync) → medium; ≥5 in cluster → high.
- **D3 capture_velocity:** agent captures > `CAPTURE_VELOCITY_MAX` (30) leads in any rolling 60 min → medium; sustained 3 windows → high.
- **D4 geo_impossibility:** consecutive CAPTURED_AT by same agent with haversine distance / time-delta implying speed > `MAX_TRAVEL_KMH` (120) → medium; repeat offender → high.
- **D5 consent_backdating:** `MATCH (p)-[c:CONSENTED]->(purpose) ... WHERE exists { (p)-[m:MESSAGED]->() WHERE m.at < c.granted_at AND m.purpose = purpose }` → **high always** (compliance-critical); does NOT quarantine automatically — routes to compliance queue.
- **D6 ghost_booking:** ≥ `GHOST_MIN` (3) bookings created+cancelled within `GHOST_WINDOW_MIN` (10) by same staff in a day → medium.
- **D7 gnn_anomaly:** consumes `risk_score` from the W29 sweep; heuristic v1 = isolation-forest-style z-score over W29 feature vectors per tenant (persons >3σ on neighborhood-structure features) → low/medium. Graph-ml owns the math; fraud-engine consumes scores ≥ `ANOMALY_ALERT_THRESHOLD` (0.9) into Alert nodes.

**Quarantine rule:** only F1-high, F2-high, F3-high set `quarantined=true` automatically. Everything else awaits human confirmation. Un-quarantine happens on alert resolution (`dismissed`) via graph-service resolve endpoint (fraud-engine exposes no unquarantine path).

**CloudEvent:** `com.opendesk.fraud.AlertRaised`, id `tenant:alert:alert_id`, extension `tenantid`, data {alert_id, type, severity, person_id?, agent_id?} → topic `opendesk.fraud.alerts.v1`.

## §4 Workstreams & ownership

### WS-B — `services/fraud-engine/**` (NEW, Agent B owns)
Python 3.11 FastAPI + confluent-kafka + redis. Files: `fraud_engine/{config,graph,detectors/{base,d1_referral,d2_sybil,d3_velocity,d4_geo,d5_consent,d6_ghost,d7_anomaly},alerts,events,quarantine,main}.py` + tests.
- Endpoints: `/healthz`, `POST /v1/detect/run {tenant_id?, detector?}` (manual), `GET /v1/detect/status`.
- Dedup + idempotency: MERGE alert_ids; sweep re-runs never duplicate open alerts.
- Tests (≥15): each detector fires on a fixture graph built to trip it and stays silent on a clean graph; dedup; severity rules; quarantine only on F1/F2/F3-high; D5 never auto-quarantines; CloudEvent shape; tenant binding asserted on every emitted Cypher (code-review-able `tenant_id` param); haversine math sanity.

### WS-C — graph-service alerts router (Agent C, same agent as W29 WS-B)
NEW `app/routers/alerts.py`: `GET /v1/graph/alerts?status=&type=&severity=` (tenant-scoped JWT, existing auth), `GET /v1/graph/alerts/{id}`, `POST /v1/graph/alerts/{id}/resolve {decision: confirmed|dismissed, reason}` — resolve: sets status/resolved_at/resolved_by (from JWT sub)/resolve_reason (mandatory, min 10 chars); on `dismissed` → clears quarantine if no other open high alerts on that person; on `confirmed` → keeps quarantine, emits audit CloudEvent `com.opendesk.fraud.AlertResolved` → opendesk.fraud.alerts.v1. Tests ≥8 (tenant isolation of list, resolve validation, unquarantine-only-when-no-open-highs, audit event).

### WS-D — admin-web fraud UI (Agent D, same agent as W29 WS-C)
- NEW `apps/admin-web/app/app/[orgSlug]/alerts/page.tsx` + `alerts-client.tsx` + `components/alerts/{alerts-table.tsx,alert-detail.tsx,types.ts}`: filter by status/type/severity; detail drawer shows evidence JSON rendered readably (cycle path as list, geo points as coordinates+speed); resolve dialog with decision + reason; brand tokens; sage/amber/terracotta severity chips.
- EDIT `person-client.tsx`: risk badge (risk_score chip + active risk_flags) next to W29 propensity badges; link from person to their alerts (filtered queue).
- Nav: new "Alerts" item with `Shield` lucide icon after Segments, same role gate — **integrator applies** (org-nav.tsx is orchestrator-owned); Agent D exports the component set and leaves a comment marker.

### Integrator (orchestrator)
- `infra/docker-compose.graph.yml`: fraud-engine (:7017 internal only), env wiring.
- `infra/kafka/create-topics.sh`: + `opendesk.fraud.alerts.v1`.
- APISIX: none (alerts ride existing /api/graph/* route).
- `docs/graph-intel.md`: fraud section (taxonomy, thresholds, adjudication SOP, false-positive handling, NDPA note: fraud flags are internal security processing — documented lawful-basis posture).
- `tests/e2e/test_graph_fraud_wave.py`: ≥8 — fixture ring → alert appears via API; clean tenant → zero alerts; quarantined-by-fraud person audience-ineligible (the money test); resolve-dismissed unquarantines; resolve requires reason; alerts tenant-isolated; CloudEvent on resolve; sweep idempotent.

## §5 Quality gates
1. No detector query without bound `$tenant_id`. 2. Quarantine only per §3 rule; no auto-erasure anywhere. 3. Every alert carries replayable evidence. 4. Resolve audit trail complete (who/when/why). 5. Dedup: N sweeps → ≤1 open alert per (type,person). 6. Fraud flags never leave the tenant (alerts API tenant-scoped; no cross-tenant fraud intel in v1).

## §6 Exclusions
No payment-rail fraud scoring inside TigerBeetle (ledger stays pure; fraud signals arrive via events). No device fingerprinting (no device data exists yet — W31 candidate). No ML model training for fraud in v1 beyond the shared anomaly score. No automatic law-enforcement/INEC reporting hooks.

## §7 Acceptance
WS tests green; e2e green; integrator wiring complete; independent verification gate PASS; blob-SHA-verified push; W26-protocol full-tree audit clean.
