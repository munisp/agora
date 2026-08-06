# Graph Intelligence — Predictive Scoring & Fraud Detection

Waves W29 (predictive layer) and W30 (fraud & trust intelligence) extend the W28 tenant knowledge graph. Read `docs/graph.md` first — every rule there (per-tenant isolation, consent supremacy, erasure propagation) applies unchanged here.

## 1. Components

| Component | Port | Role |
|-----------|------|------|
| `graph-ml` | 7016 (internal) | Batch propensity scoring + service recommendations. Heuristic mode (default, zero ML deps) or optional GraphSAGE GNN backend. |
| `fraud-engine` | 7017 (internal) | Deterministic graph fraud detectors + GNN-anomaly consumer. Writes Alert nodes and quarantine flags. |
| `graph-service` | 7014 | Extended: internal write-back API (`/v1/graph/internal/*`), 4 predictive templates, segment score filters, fraud alert triage/resolve API. |
| admin-web | — | Person-360: propensity badges, recommendations panel, risk badge. New `/alerts` adjudication queue. Segment builder: score filters. |

Both new services are internal-only (no APISIX route, no host port requirement). Bring them up with the graph overlay:

```bash
docker compose -f infra/docker-compose.yml -f infra/docker-compose.graph.yml up -d graph-ml fraud-engine
```

## 2. Predictive scoring (W29)

### 2.1 Scores

| Property | Range | Meaning |
|----------|-------|---------|
| `Person.propensity_churn` | 0..1 | Likelihood of lapsing (no booking within tenant-typical interval) |
| `Person.propensity_convert` | 0..1 | Likelihood of booking from an outreach touch |
| `Person.propensity_turnout` | 0..1 | Campaign tenants: likelihood of showing up when contacted |
| `Person.risk_score` | 0..1 | Structural anomaly score (written by the sweep; consumed by fraud detector D7) |

Every score carries `model_version` (e.g. `heuristic-v1`, `graphsage-v1`) and `scored_at`. Re-scoring overwrites in place. Recommendations are `(Person)-[:RECOMMENDED_FOR {score, rank, reason, model_version, scored_at}]->(Offering)` edges, top-K per person (`GRAPH_ML_TOP_K`, default 5), each with a human-readable `reason`.

### 2.2 Backends

- **Heuristic (default, `GRAPH_ML_BACKEND=heuristic`):** deterministic recency/frequency/monetary + graph-degree features; sigmoid-normalized. Works on a 5-person graph; this is the cold-start and small-tenant path. Only requires `numpy`.
- **GNN (`GRAPH_ML_BACKEND=gnn`):** GraphSAGE node-regression heads + dot-product link predictor, trained per tenant on the exported subgraph. Requires `torch` + `torch-geometric` (install from `requirements-gnn.txt` — deliberately absent from the base image). If the import fails, the service logs a warning and runs heuristic; it never crashes on missing ML deps.

Model artifacts version under `GRAPH_ML_MODEL_DIR` (`graphsage-v{N}/`).

### 2.3 Operating the sweep

- Automatic: full tenant sweep every `SCORE_INTERVAL_MINUTES` (default 60).
- Manual: `POST :7016/v1/score/run` with `{"tenant_id": "..."}` (omit for all tenants).
- Status: `GET :7016/v1/score/status` (last run per tenant, backend in effect, scores written).

**Single write path:** graph-ml never touches FalkorDB for writes. Scores and recommendation edges go through graph-service's internal API with `X-Internal-Token`; graph-service verifies tenant ownership of every target node before merging. If someone bypasses this (direct graph writes from graph-ml), that is a compliance incident — the W29 gate tests for it.

### 2.4 Consuming scores

- **Segments:** the builder gains numeric score filters: `{"score_filters": [{"field": "propensity_churn", "op": ">=", "value": 0.7}]}` — compiled to bound parameters, unknown fields rejected with 422.
- **Templates** (GraphRAG ask + `/v1/graph/cypher`): `next_best_services`, `churn_risk_band`, `referral_value`, `similar_persons`. See the template registry for parameter shapes.
- **Person-360:** badges + recommendations panel render from the same endpoints the templates use.

Scores rank *within* already-eligible populations. They never override consent, quarantine, or DND gates.

## 3. Fraud detection (W30)

### 3.1 Detector catalog

| ID | Type | Signal | Severity rule | Auto-quarantine? |
|----|------|--------|---------------|-------------------|
| D1 | `referral_cycle` | Cycles in REFERRED (length 2–4) | high if ring ≥3 with reward conversion, else medium | high only |
| D2 | `sybil_cluster` | Same agent + location + burst + name-embedding cosine ≥ 0.98 | high at ≥5 members, else medium | high only |
| D3 | `capture_velocity` | Agent captures > 30 leads/rolling hour | high if sustained 3 windows | high only |
| D4 | `geo_impossibility` | Consecutive captures implying > 120 km/h | medium; high on repeat | high only (repeat offenders) |
| D5 | `consent_backdating` | CONSENTED.granted_at after first MESSAGED.at for that purpose | **always high** | **never** — compliance queue |
| D6 | `ghost_booking` | ≥3 create→cancel cycles ≤10 min apart by same staff/day | medium | never |
| D7 | `gnn_anomaly` | `risk_score` ≥ 0.9 from the W29 sweep | low/medium | never |

Thresholds are env-tunable on fraud-engine (see compose). Cadence: event-driven triggers for D3/D5 plus a full sweep every `FRAUD_SWEEP_MINUTES` (default 15). Dedup: one open alert per (type, person) — re-sweeps merge, never duplicate.

### 3.2 Alert lifecycle

`open` → (`confirmed` | `dismissed`) via `POST /api/graph/alerts/{id}/resolve` with a mandatory reason (≥10 chars). Resolve sets `resolved_at`, `resolved_by` (JWT subject), and emits `com.opendesk.fraud.AlertResolved` to `opendesk.fraud.alerts.v1`. Dismissing clears the person's quarantine **only if** no other open high-severity alerts remain on them. Confirming keeps it.

Every alert carries a replayable `evidence` JSON (cycle path, cluster members, coordinates + implied speed, the two timestamps) — the admin UI renders it readably, and auditors can reconstruct exactly why the detector fired.

### 3.3 Enforcement semantics

- Detection ≠ punishment. Quarantine reuses the W28 gate: quarantined persons stay query-visible but are **audience-ineligible** — they silently drop out of every materialized audience until adjudicated.
- **No auto-erasure, ever.** Fraud flags are internal security processing; NDPA erasure remains the separate, consent-driven flow. A fraud suspect who files an erasure request still gets erased (W28 path) — the alert history is compliance metadata, not a person record.
- D5 (consent backdating) never auto-quarantines because it usually indicates an *operational* failure (import pipeline, agent behavior) — it routes to human compliance review as the highest-severity queue item.

### 3.4 Adjudication SOP (recommended)

1. Triage `open` queue by severity, D5 first (compliance exposure), then high, then medium/low.
2. Open the evidence drawer; for D1/D2 inspect the linked persons in Person-360.
3. `confirmed` → quarantine stands, optionally off-board the agent/staff member via identity-service.
4. `dismissed` → write the reason (it is the audit trail); quarantine lifts if no other open highs.
5. Weekly: review detector precision (dismissed/total per type); tune thresholds via env rather than code.

## 4. Security & compliance notes

- `INTERNAL_TOKEN` guards `/v1/graph/internal/*`. JWTs are never accepted there; the token is constant-time-compared. Rotate per environment; the dev default is obviously not for production.
- Alerts API is tenant-scoped JWT like every other graph route — fraud intel never crosses tenants in v1.
- Fraud flags are stored on the per-tenant graph; the W26-protocol audit and RLS posture are unaffected (graph data never lives in Postgres).
- Staff/agent attribution: graph-sync (W30 patch) projects `Contact.captured_by` (from event fields `agent_id`/`staff_id`/`captured_by`), `Booking.created_by`, and `Booking.cancelled_at` (stamped automatically on cancel events) **when upstream emitters include them**. Until booking-service/field-PWA emitters send staff identity, detectors D2/D3/D4/D6 stay silent in production — fail-closed, no false positives. Upstream emitter changes are additive event fields, no further graph-sync work needed.
- Known v1 limits: no device fingerprinting (no device telemetry exists), no cross-tenant fraud rings, GNN training/inference lands with the W31 GPU profile (the `gnn` backend currently degrades to heuristic per tenant, by design).

## 5. Testing & fixtures

The e2e suites (`tests/e2e/test_graph_predictive_wave.py`, `test_graph_fraud_wave.py`) run live against the compose overlay, like the W28 suite. Deterministic fixtures (referral rings, backdated consent, impossible travel — shapes no public API can legitimately create) are seeded via `POST /v1/graph/internal/fixtures/seed`, which exists **only** when `E2E_FIXTURES=1` (dev overlay default). It accepts a scenario name from a fixed server-side allowlist — never query text — plus the internal token. Never enable it in production; the posture is identical to `AUTHZ_DISABLED`.

## 6. Failure modes

| Symptom | Likely cause | Action |
|---------|--------------|--------|
| No scores after sweep | graph-service internal API 401 | INTERNAL_TOKEN mismatch between graph-ml and graph-service |
| GNN backend silently heuristic | torch/PyG not installed | expected; install from requirements-gnn.txt or accept heuristic |
| Alert flood after data import | D3/D5 tripped by bulk historical import | run import with detectors paused (`POST /v1/detect/run` per-detector control), or raise thresholds temporarily |
| Person unexpectedly audience-ineligible | quarantined by F1/F2/F3-high | check `/alerts` queue for that person; adjudicate |

## 7. GNN backend (W31)

The W29 `gnn` backend is now real: per-tenant unsupervised GraphSAGE training + link-prediction inference for RECOMMENDED_FOR edges. It **replaces only the recommendation half** of scoring — propensity scores stay `heuristic-v1` this wave (calibration gate below). Heuristic remains the permanent cold-start/degradation path.

### 7.1 Install & run

torch/torch-geometric never enter the base image; they install from the `requirements-gnn.txt` overlay into the `Dockerfile.gnn` variant only:

```
docker compose -f infra/docker-compose.graph.yml build graph-ml graph-ml-gpu
docker compose -f infra/docker-compose.graph.yml --profile gnn up graph-ml-gpu
```

The pinned wheels are the CPU variant (`GRAPH_ML_DEVICE=auto` → cuda-if-available-else-cpu). A CUDA base swap (e.g. `pytorch/pytorch:*-runtime` or the `+cu121` wheel index in `requirements-gnn.txt`) is documented-not-built: GPU is never mandatory. The profiled service shares the `graph-ml-models` volume with the heuristic one and serves on host port **7018**.

### 7.2 Train / score flow

- **Train:** `POST :7018/v1/score/train` with `{"tenant_id": "..."}` (omit for all tenants). Response `{run_id, trained: [{tenant_id, model_version, final_loss}], skipped: [{tenant_id, reason}], ok}` — undersized graphs, missing data, and per-tenant train errors land in `skipped`; the run never fails on them. On the heuristic base image the endpoint honestly refuses with `409 {"error": "gnn backend not enabled"}` (the e2e asserts exactly this).
- **Nightly train:** `GRAPH_ML_TRAIN_INTERVAL_MINUTES > 0` adds an APScheduler interval job alongside the untouched score sweep (default 0 = off). It is a no-op in heuristic mode.
- **Score:** unchanged — the interval sweep (or `POST /v1/score/run`) uses the latest trained model per tenant when one exists.
- **Min-size gate:** tenants below `GRAPH_ML_GNN_MIN_PERSONS` (20) / `GRAPH_ML_GNN_MIN_EDGES` (30) are skipped at train time and score heuristically — small tenants never get a half-trained model.

### 7.3 Model registry layout

```
{GRAPH_ML_MODEL_DIR}/{tenant_id}/graphsage-v{N}/model.pt
                                            meta.json
```

One model lineage per tenant; `N` increments per successful training. `meta.json` carries tenant_id, model_version, trained_at, feature/hidden dims, epochs, final_loss, node/edge counts, device, seed. A tenant's model never scores another tenant (paths and loads are tenant-scoped). `/healthz` reports `gnn_models_dir` + `gnn_tenants_with_models` (best-effort — omitted, never a 500, on filesystem errors).

### 7.4 Fallback ladder (W29 gate-5 semantics, unchanged)

Every GNN failure mode degrades that tenant to heuristic, logged, and the sweep still exits 0:

1. **no torch/PyG** → heuristic (import guard, startup warning)
2. **no trained model** for the tenant → heuristic
3. **undersized graph** (below the min-size gate) → heuristic
4. **training/inference error** → heuristic

Heuristic output is always correct; the GNN is purely additive. GNN-written RECOMMENDED_FOR edges carry `model_version=graphsage-v{N}` + `reason` + `scored_at` for provenance; heuristic edges keep `heuristic-v1`.

### 7.5 Propensity calibration gate (roadmap R5)

GNN propensity heads are explicitly deferred: propensity scores stay `heuristic-v1` until a calibration report shows **Brier < 0.2** for the GNN heads against held-out outcomes. Do not enable GNN propensity before that report exists — the gate is the honest-ML doctrine, not a performance question.
