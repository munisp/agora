# graph-ml — propensity scores & service recommendations (SPEC-W29 §3 WS-A)

Batch scorer over the W28 tenant knowledge graph (FalkorDB). Computes, **per
tenant**, three propensity scores per Person and top-K offering
recommendations, then writes them back **only** through the graph-service
internal API.

```
FalkorDB (agora_tenants graph)          graph-service (:7014)
  persons/bookings/offerings/             POST /v1/graph/internal/scores
  referrals/consents/contacts/    --->    POST /v1/graph/internal/recommendations
  MESSAGED edges                    |     (X-Internal-Token, chunked 500, per-tenant)
          graph-ml (:7016, this service) — heuristic (numpy) | optional GraphSAGE
```

## What it computes

| Output | Where it lands | Model |
|---|---|---|
| `propensity_churn` | Person property | `sigmoid(days_since_last_booking / tenant_median_interval)` |
| `propensity_convert` | Person property | `0.45·recency_term + 0.35·response_rate + 0.20·referral_in_term` |
| `propensity_turnout` | Person property | `0.5·response_rate + 0.5·(MESSAGED→BOOKED rate)`; cold prior 0.5 |
| `risk_score` | Person property | anomaly head (SPEC-W30 §1/§2): mean of robust median/MAD \|z\| over structural features (referral degrees, booking counts, intervals, response rates), `min(z/6, 1)` squash. Tenants with <5 persons or zero variance → 0.0. Calibrated so only genuine structural outliers cross 0.9 (fraud-engine D7 threshold) |
| `RECOMMENDED_FOR {score, rank, reason, model_version, scored_at}` | Person→Offering edge | offering co-occurrence lift, minus already-booked, top-K |

Every score/edge carries `model_version` (`heuristic-v1`, `graphsage-v{N}`)
and `scored_at` (SPEC-W29 §0.4). Reason strings are UI-displayable, e.g.
`booked_cleaning_2x`, `clients_like_them_booked` (cold start).

Scores are **data, not authority** (§0.3): they never touch consent,
quarantine, or DND gates — they rank within already-eligible populations.

## Backends

- **heuristic** (default, `GRAPH_ML_BACKEND=heuristic`): numpy is the only
  numeric dep. Works on cold start with a 5-person graph (§4 gate 4).
- **gnn** (`GRAPH_ML_BACKEND=gnn`): GraphSAGE node propensity heads +
  dot-product Person→Offering link predictor. Requires torch +
  torch-geometric (`requirements-gnn.txt`, GPU profile only). If the imports
  are missing the service **falls back to heuristic with a logged warning
  and keeps running** (§4 gate 5). Artifacts version under
  `GRAPH_ML_MODEL_DIR/graphsage-v{N}/`.

## Configuration

| Env | Default | Meaning |
|---|---|---|
| `FALKORDB_HOST` / `FALKORDB_PORT` | `localhost` / `6379` | graph store (READ-ONLY here) |
| `FALKORDB_DB` | `graph` | FalkorDB graph name |
| `GRAPH_SERVICE_URL` | `http://localhost:7014` | write-back target |
| `INTERNAL_TOKEN` | — (required) | `X-Internal-Token` shared secret |
| `GRAPH_ML_BACKEND` | `heuristic` | `heuristic` \| `gnn` |
| `GRAPH_ML_MODEL_DIR` | `./models` | GNN artifact root |
| `GRAPH_ML_TOP_K` | `5` | recommendations per person |
| `SCORE_INTERVAL_MINUTES` | `60` | full-sweep interval |
| `TENANT_CONCURRENCY` | `4` | parallel tenant workers (isolated per tenant) |
| `PORT` | `7016` | HTTP API port (internal only; compose exposes no host port) |

## HTTP API

- `GET /healthz` → `{status, backend, gnn_available}`
- `POST /v1/score/run` body `{"tenant_id": null}` — manual trigger; full
  sweep or one tenant.
- `GET /v1/score/status` → backend, run count, per-tenant results of the
  last sweep, next scheduled run.

## CLI (cron-style batch alternative)

```
python -m graph_ml.score                 # full sweep
python -m graph_ml.score --tenant t_acme # one tenant
python -m graph_ml.score --dry-run       # compute only, no write-back
```

Exit code is 0 even when `GRAPH_ML_BACKEND=gnn` degrades to heuristic;
non-zero only on real tenant failures.

## Invariants

- **Tenant isolation:** all Cypher is static templates with bound
  `$tenant_id` (values bound via the `CYPHER k=v` preamble with escaping —
  never string-built statements). One tenant's failure never kills a sweep.
- **Single write path (§4 gate 3):** graph-ml NEVER writes FalkorDB. Scores
  and edges go to graph-service's internal API (`X-Internal-Token`,
  chunked at 500, per-tenant) where tenant-match validation lives.
- **Heuristic mode has zero ML deps beyond numpy** — torch/pyg are confined
  to `requirements-gnn.txt` and import-guarded in `graph_ml/gnn.py`.

## Development

```
pip install -r requirements.txt
pytest -q            # ≥15 tests, heuristic mode only, no torch, no live DB
```
