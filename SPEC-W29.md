# SPEC-W29 — Predictive Layer: Propensity Scores & Service Recommendations

**Status:** contract · **Depends on:** W28 (tenant knowledge graph) · **Date:** 2026-08-05

## §0 Scope & rulings

1. **Per-tenant only.** All scoring, recommendations, and models are computed and stored per tenant. No cross-tenant person-level data ever. (Aggregate benchmarks remain a future, k-anonymized product.)
2. **Heuristic-first, GNN-optional.** Every score has a deterministic heuristic baseline (recency/frequency/monetary + graph-degree features) that works on cold start with zero ML dependencies. A PyTorch Geometric backend (GraphSAGE: node propensity + Person→Offering link prediction) activates only when `GRAPH_ML_BACKEND=gnn` and torch/pyg are installed; otherwise the service runs pure heuristic mode. Tests cover heuristic mode only.
3. **Scores are data, not authority.** Scores never override consent, quarantine, or DND gates. They rank and filter *within* already-eligible populations.
4. **Provenance.** Every score/recommendation carries `model_version` and `scored_at`. Re-scoring overwrites in place (MERGE by person+offering, keep latest).

## §1 Architecture

```
FalkorDB (per-tenant graph)
   │  read: persons, bookings, offerings, referrals, consents, contacts
   ▼
graph-ml (new Python service, batch scorer; optional HTTP :7016 for on-demand rescore)
   │  heuristic path (numpy only)  |  gnn path (torch+PyG, optional)
   │  writes: Person.propensity_*, Person→Offering RECOMMENDED_FOR edges
   ▼
graph-service (extended)
   │  internal score write-back API (X-Internal-Token)
   │  new templates: next_best_services, churn_risk_band, referral_value, similar_persons
   │  segment DSL: numeric score filters
   ▼
admin-web: Person-360 "Recommended next services" panel + segment-builder score filters
```

## §2 Graph schema v2 additions (additive to docs/graph.md schema v1)

**Person new properties (all optional, all tenant-scoped by the node's existing tenant_id):**
- `propensity_churn` float 0..1 — likelihood of lapsing (no booking within tenant-typical interval)
- `propensity_convert` float 0..1 — likelihood of booking from an outreach touch
- `propensity_turnout` float 0..1 — campaign tenants: likelihood of showing up/voting when contacted
- `scored_at` ISO-8601, `model_version` string (e.g. `heuristic-v1`, `graphsage-v1`)

**New edge:** `(p:Person)-[:RECOMMENDED_FOR {score: float, rank: int, reason: string, model_version, scored_at}]->(o:Offering)`
- Both endpoints verified same-tenant before write. `reason` is human-readable (`"booked_cleaning_2x"`, `"clients_like_them_booked"`) for UI display.
- Re-score: MERGE on (person, offering), SET latest score/rank/reason. Top-K per person (K=5 default, `GRAPH_ML_TOP_K`).

## §3 Workstreams & file ownership

### WS-A — `services/graph-ml/**` (NEW service, Agent A owns everything under this path)
Python 3.11, FastAPI + APScheduler (or cron-style CLI entry `python -m graph_ml.score`). Deps: redis (FalkorDB), httpx, numpy; **torch/torch-geometric optional** (import guarded).
- `graph_ml/config.py` — env: FALKORDB_HOST/PORT/DB(=`graph`), GRAPH_SERVICE_URL, INTERNAL_TOKEN, GRAPH_ML_BACKEND (heuristic|gnn, default heuristic), GRAPH_ML_TOP_K (5), SCORE_INTERVAL_MINUTES (60), TENANT_CONCURRENCY.
- `graph_ml/extract.py` — pull per-tenant subgraphs via parameterized Cypher (never string-built): persons+bookings+offerings+referrals+consent purposes+contacts per tenant. Tenant discovery: `MATCH (t:Tenant) RETURN t.tenant_id`.
- `graph_ml/features.py` — feature vectors: recency_days, booking_count, booking_interval_mean/std, monetary_total, distinct_offerings, referral_out_degree, referral_in_degree, message_response_rate (from MESSAGED edges), consent_purpose_count, days_since_capture.
- `graph_ml/heuristic.py` — baseline scorers: churn = sigmoid(days_since_last_booking / tenant_median_interval); convert = f(recency, response_rate, referral_in_degree); turnout = f(response_rate, past MESSAGED→BOOKED conversion); recommendations = offering co-occurrence matrix (booked A → booked B lift) minus already-booked, top-K with reason strings.
- `graph_ml/gnn.py` — OPTIONAL. GraphSAGE node regression (propensity heads) + dot-product link predictor for RECOMMENDED_FOR. `ImportError` on torch → backend falls back to heuristic with a logged warning. Model artifacts under `GRAPH_ML_MODEL_DIR` (default `./models`), versioned dirs `graphsage-v{N}/`.
- `graph_ml/writeback.py` — POST scores/edges to graph-service internal API (`/v1/graph/internal/scores`, `/v1/graph/internal/recommendations`) with `X-Internal-Token`, chunked (500), per-tenant. Never writes FalkorDB directly (single write path = graph-service, so audit/validation live in one place).
- `graph_ml/main.py` — FastAPI: `GET /healthz`, `POST /v1/score/run` (body `{tenant_id?}` — manual trigger), `GET /v1/score/status`. Scheduler runs full sweep every SCORE_INTERVAL_MINUTES.
- Tests (pytest, fakeredis or Cypher-mock): heuristic scorer math on fixtures, top-K + exclusion of already-booked, reason strings present, chunking, gnn-fallback path (import guard), tenant loop isolation (one tenant failure doesn't kill sweep). ≥15 tests.

### WS-B — graph-service extensions (Agent C owns ONLY these files inside `services/graph-service/`)
- NEW `app/routers/internal_scores.py` — `POST /v1/graph/internal/scores` + `POST /v1/graph/internal/recommendations`. Auth: `X-Internal-Token` header must equal `INTERNAL_TOKEN` env (constant-time compare; 401 otherwise; never accept JWT on these routes). Validates tenant_id present on every item; verifies Person/Offering tenant match before MERGE (no cross-tenant write possible); MERGE semantics per §2; emits metric `scores_written_total{tenant}`.
- NEW `app/templates/predictive.py` — 4 read-only templates registered in the existing allowlist:
  1. `next_best_services` — params `{person_id}` → person's RECOMMENDED_FOR edges ordered by rank; **fallback** when no edges: offering co-occurrence from BOOKED patterns of similar persons (same location, overlapping offerings) — still tenant-scoped.
  2. `churn_risk_band` — params `{min_score=0.7}` → persons with propensity_churn ≥ min, plus consent-purpose summary; audience-safe (excludes quarantined).
  3. `referral_value` — params `{person_id?}` → referral out-degree, converted-referee count (referees who BOOKED), ranked; tenant leaderboard when person_id omitted.
  4. `similar_persons` — params `{person_id, k=10}` → cosine similarity over stored Ollama embeddings (W28 already embeds persons); excludes self; tenant-scoped.
- EDIT `app/segment/compiler.py` — segment DSL gains optional numeric filters block: `{"score_filters": [{"field": "propensity_churn"|"propensity_convert"|"propensity_turnout"|"risk_score", "op": ">="|"<="|"between", "value": float|[lo,hi]}]}` compiled to parameterized `WHERE p.propensity_churn >= $sf0` clauses (param binding, never interpolation). Unknown field → 422.
- EDIT `app/routers/segments.py` (or wherever ask/schema lives) — expose score-filter fields in segment schema introspection used by the UI.
- Tests: internal-token auth (missing/wrong/correct), tenant-mismatch rejection on write-back (a score for tenant A's person submitted under tenant B → rejected), MERGE-overwrite keeps latest, each template returns tenant-scoped results only, score_filters compile to params (assert `$sf0` binding, assert 422 on unknown field), quarantined exclusion in churn_risk_band. ≥15 tests.

### WS-C — admin-web predictive UI (Agent D owns ONLY these paths)
- NEW `apps/admin-web/components/segments/recommendations-panel.tsx` — "Recommended next services" panel: ranked list with score bar + reason string; "Create audience of similar clients" button (calls similar_persons → prefills segment builder); loading/empty/error states; brand tokens (cream #FAF6F0, ink #2B2118, terracotta #C0562F, amber #D99A4E, sage #7A8B6F).
- NEW `apps/admin-web/components/segments/propensity-badge.tsx` — churn/convert score chips for Person-360 header (color: sage low → amber mid → terracotta high; never red/blue).
- EDIT `apps/admin-web/app/app/[orgSlug]/persons/[id]/person-client.tsx` — mount panel + badges (fetch `/api/graph/persons/{id}` and template query via existing graph client pattern).
- EDIT `apps/admin-web/components/segments/segment-builder.tsx` — score-filter row type (field select, op select, value input) serializing into the DSL block from WS-B; validation client-side mirrors 422 rules.
- Types in `components/segments/types.ts`. No new nav items. No edits outside listed files.

### Integrator (orchestrator owns)
- `infra/docker-compose.graph.yml` — add `graph-ml` service (build services/graph-ml, internal only, no host port; env wiring).
- `docs/graph-intel.md` — operator docs: scoring cycle, model versions, manual rescore, template reference, heuristic-vs-GNN ops.
- `tests/e2e/test_graph_predictive_wave.py` — ≥8 e2e: full score cycle on fixture tenant (seed graph → run scorer → scores present with model_version), next_best_services with + without edges, churn band respects consent/quarantine, internal API rejects wrong token + cross-tenant write, segment score filter end-to-end, similar_persons excludes other tenants.
- `infra/kafka/create-topics.sh` — no new topics for W29 (batch design).

## §4 Quality gates (verification checklist)

1. **Tenant isolation:** no query, score, or recommendation crosses tenants — verified by test attempting cross-tenant write-back and read.
2. **Consent/quarantine supremacy:** scores never alter audience eligibility; churn_risk_band excludes quarantined.
3. **Single write path:** graph-ml writes only via graph-service internal API; direct FalkorDB writes from graph-ml = FAIL.
4. **Cold start:** with a 5-person fixture graph, heuristic scores + recommendations still produced (no crash, no empty).
5. **Degraded GNN:** `GRAPH_ML_BACKEND=gnn` without torch installed → heuristic fallback + warning, exit 0.
6. **Auth:** internal routes reject JWT, accept only matching X-Internal-Token.

## §5 Exclusions
- No cross-tenant models. No real-time/streaming scoring (batch only; W31 candidate). No AutoML. No person-level explainability beyond `reason` strings (W31 candidate: SHAP). No changes to W28 compliance gates.

## §6 Acceptance
- All WS tests green; e2e suite green; integrator compose/service wiring complete; docs complete; verification gate (independent) PASS; push with blob-SHA verification; W26-protocol full-tree audit clean.
