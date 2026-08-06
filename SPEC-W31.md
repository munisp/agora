# SPEC-W31 — GraphSAGE GNN Backend (Build Contract)

**Status:** build contract · **Date:** 2026-08-06 · **Wave:** W31 (GNN item of SPEC-W31-roadmap; trigger fired manually by owner)
**Prereqs:** W29 (predictive layer: heuristic scores, RECOMMENDED_FOR edges, graph-ml service seam, models volume), W28 (tenant knowledge graph)

## §0 Scope & honest-ML doctrine

Implement the deferred W29 GraphSAGE backend: per-tenant unsupervised GraphSAGE training + inference for **recommendations** (Person→Offering RECOMMENDED_FOR edges). This wave does **NOT** replace heuristic propensity scores — per roadmap R5, propensity stays `heuristic-v1` until a calibration report shows Brier < 0.2. The GNN's deliverable is better link prediction (who is likely to book what), with heuristic as the permanent cold-start/degradation path.

**Invariants (never break):**
1. W29 gate-5 semantics: any GNN failure (no torch, no model, undersized graph, training error) → per-tenant heuristic fallback, logged, sweep exits 0. Heuristic is always correct.
2. Single write path: graph-ml → graph-service internal API only (X-Internal-Token). No direct FalkorDB writes of scores/edges.
3. Per-tenant isolation: one model per tenant under `{GRAPH_ML_MODEL_DIR}/{tenant_id}/graphsage-v{N}/`; a tenant's model never scores another tenant.
4. Provenance: RECOMMENDED_FOR edges written by the GNN carry `model_version=graphsage-v{N}`, `scored_at`, `reason` (e.g. `graphsage link_prediction rank=1`); heuristic edges keep `heuristic-v1`. Propensity scores keep `heuristic-v1` this wave.
5. torch/torch-geometric stay OUT of the base image (requirements-gnn.txt overlay only). The heuristic test-suite never imports torch; GNN tests are marked and skip cleanly when torch is absent.

## §1 WS-A — core ML (graph-ml package)

**`graph_ml/gnn_data.py` (new):** TenantGraph → PyG `Data`. Node types: Person (feature vector from `features.build_features` + stored Ollama `name_embedding` when present, projected/padded to a fixed `feature_dim`), Offering (one-hot/type + price/duration scalars — defined in the module). Edges: BOOKED (Person→Offering), REFERRED/CONTACT_OF/MESSAGED (Person→Person) per what `extract.TenantGraph` actually exposes (read extract.py; use only real fields). Deterministic node indexing (sorted ids) so artifacts are reproducible.

**`graph_ml/gnn_train.py` (new):**
- `SAGEConfig`: hidden_dim=64, num_layers=2, epochs (GRAPH_ML_EPOCHS, default 200), lr=1e-3, seed (GRAPH_ML_SEED, default 42), device (GRAPH_ML_DEVICE, `auto`→cuda-if-available-else-cpu).
- Unsupervised GraphSAGE objective (original link-based loss): random-walk/neighbor positive pairs + negative sampling over Person–Person edges; PLUS a supervised link-prediction head (dot-product decoder) on Person→Offering BOOKED edges with negative sampling (the recommendation signal).
- `train_tenant(graph, tenant_id, settings) -> TrainResult {model_version, model_dir, final_loss, epochs, node_counts, edge_counts, device, trained_at}`:
  - Min-size gate (GRAPH_ML_GNN_MIN_PERSONS default 20, GRAPH_ML_GNN_MIN_EDGES default 30): below → raise `GNNInsufficientData` (caller maps to heuristic fallback, NOT an error).
  - Versioned artifacts: `{model_dir}/{tenant_id}/graphsage-v{N}/model.pt` + `meta.json` {tenant_id, model_version, trained_at, feature_dim, hidden_dim, epochs, final_loss, node_counts, edge_counts, device, seed}. Version from `next_model_version({model_dir}/{tenant_id})` (move/generalize the W29 helper — keep it tenant-scoped; the old global call site in gnn.py is updated).
  - Determinism: torch seeded; same inputs+seed → same final_loss (tested, exact equality on CPU).
- `load_latest(model_dir, tenant_id) -> (state_dict, meta, feature_dim) | None` — None when no versioned dir (caller falls back).

**`graph_ml/gnn.py` (rewrite inference, keep guards):** `GraphSAGEBackend.score_tenant(graph, now, top_k)`:
- Raises `GNNBackendUnavailable` (torch absent) — unchanged.
- No trained model or insufficient graph → raise `GNNModelNotFound`/`GNNInsufficientData` (new exceptions); `score.py` catches these IN ADDITION TO `NotImplementedError` → per-tenant heuristic fallback with `model_version=heuristic-v1` (broaden the except clause; behavior identical to W29 gate-5).
- With a model: embed persons, dot-product score all offerings, exclude already-booked (same exclusion as heuristic), top-K=settings.top_k, build recommendations with reason + model_version + scored_at. Propensity scores: return the heuristic scores unchanged (call into `heuristic.score_tenant` for the score half — GNN replaces ONLY the recommendation half this wave).
- Keep `resolve_backend`, import guard, `GNN_AVAILABLE` semantics byte-compatible (existing test_gnn.py must pass unmodified except where this SPEC explicitly changes behavior).

**`graph_ml/config.py`:** add `device`, `seed`, `gnn_epochs`, `gnn_hidden_dim`, `gnn_min_persons`, `gnn_min_edges`, `train_interval_minutes` (0=off) with env names above; defaults safe (CPU, small).

**Tests (≥15 new, `pytest.mark.requires_torch` — skip when torch absent; run green where present):** data-build shapes/dtype/deterministic indexing; min-size gate raises; training on toy graph (30 persons, 8 offerings) converges (final_loss < initial_loss); artifacts + meta.json fields; version increments per tenant; load_latest None vs round-trip; inference top-K excludes already-booked; per-tenant isolation (two tenants, no cross-read); determinism (two runs same seed → identical final_loss); score_one_tenant backend=gnn with no model → ok=True + heuristic-v1 (fallback); with trained model → recommendations model_version=graphsage-vN while propensity stays heuristic-v1.

## §2 WS-B — API, scheduler, ops

**`graph_ml/main.py`:** `POST /v1/score/train` (body `{tenant_id?: str}`, same auth posture as `/v1/score/run`):
- backend=heuristic mode → `409 {"error": "gnn backend not enabled"}` (honest degradation; e2e asserts this).
- backend=gnn: train one tenant or sweep all; response `{run_id, trained: [{tenant_id, model_version, final_loss}], skipped: [{tenant_id, reason}], ok}` — undersized/no-data tenants land in `skipped`, never fail the run.
- Optional APScheduler nightly train when `GRAPH_ML_TRAIN_INTERVAL_MINUTES > 0` (default 0=off; score sweep scheduler unchanged).
- `/healthz` gains `gnn_models_dir` + `gnn_tenants_with_models` count (best-effort, never 500).

**`services/graph-ml/Dockerfile`:** stays base (heuristic). Add `Dockerfile.gnn`: `FROM` the base image build stage, `pip install -r requirements-gnn.txt` (CPU wheels note in comment; CUDA variant documented, not built).

**`infra/docker-compose.graph.yml`:** new service `graph-ml-gpu` (profile `gnn`): build Dockerfile.gnn, same env as graph-ml but `GRAPH_ML_BACKEND=gnn`, `GRAPH_ML_DEVICE: ${GRAPH_ML_DEVICE:-auto}`, shares `graph-ml-models` volume, `profiles: [gnn]`, ports `7017:7016` (avoid clash with graph-ml on 7016 — check fraud-engine's 7017 first and pick a free one), comment: `docker compose --profile gnn up graph-ml-gpu`.

**`docs/graph-intel.md`:** new "GNN backend (W31)" ops section: install/train/score flow, model registry layout, fallback ladder (no torch → heuristic; no model → heuristic; undersized → heuristic; error → heuristic), calibration gate for propensity (Brier<0.2 before GNN propensity heads — R5), GPU profile usage.

**`tests/e2e/test_graph_predictive_wave.py`:** add `test_09_train_endpoint_honest_in_heuristic_mode` — POST /v1/score/train → 409 (stack runs heuristic base image). Session-guarded like the rest.

## §3 Gates

- G1 fallback integrity: every failure mode → heuristic, exit 0 (unit + the score_one_tenant fallback test).
- G2 per-tenant isolation: model paths, load_latest, inference never cross tenants.
- G3 provenance: GNN recs = graphsage-vN; propensity = heuristic-v1; reason/scored_at present.
- G4 determinism: seeded CPU training reproducible.
- G5 heuristic purity: base image / heuristic suite untouched by torch (import-guard proof).
- G6 honest API: train endpoint 409 in heuristic mode; skipped-not-failed per-tenant in gnn mode.

## §4 Non-goals

GNN propensity heads (R5 calibration gate), cross-tenant models, streaming training (R6), GPU mandatory (CPU-first), changes to heuristic scoring logic.
