# ML Platform (SPEC-W33)

Real end-to-end AI/ML stack for OpenDesk/Agora: labeled data foundation (Slice A),
learned models (Slice B), and ops — registry, drift, A/B, Ray, continuous training
(Slice C). This document is the GC8 deliverable of SPEC-W33 §4.

Invariants the whole stack obeys (SPEC-W33 §0):

- **I1 honest degradation** — every learned model has a rule-based fallback; model
  failure → fallback, logged, never an error.
- **I2 honest provenance** — every score carries `model_version`: learned =
  `*-v{N}` from the registry, rules = `heuristic-v1`. No silent swaps.
- **I3 honest metrics** — accuracy claims are computed by repo code against
  datasets shipped/derived in-repo with recorded seeds. Synthetic-only validation
  is stated as synthetic.
- **I4 tenant isolation** — registry stages are per (model_family, tenant);
  Postgres FORCE RLS (`app.tenant_id`) on the control plane. Internal batch
  access (drift sweep, nightly trainer) is role-based, not GUC-based
  (SPEC-W34 GF1): the NOLOGIN role `app_model_registry_internal` gates
  cross-tenant rows via `pg_has_role(current_user,
  'app_model_registry_internal')`, and only the batch login
  `app_model_registry_batch` (member of that role) holds it; the
  `app.registry_internal` GUC mechanism is removed.
- **I5 CPU-first** — everything trains and infers on CPU; torch/Ray are optional
  overlays, never in base images.
- **I6 PII discipline** — training exports carry W28 salted-SHA-256 hashed
  identifiers only; no raw PII.

**Naming deviations from SPEC-W33 (documented, not silent):**

- SPEC §3 B1 names the fraud package `services/fraud-api/fraud_api/ml/`; the
  shipped code lives in `services/fraud-engine/fraud_engine/ml/` (the W30
  service it extends).
- SPEC §4 C1 names the migration `V30__model_registry.sql` (Flyway style); this
  repo has no Flyway, so the shipped migration is
  `infra/postgres/init-scripts/30-model-registry.sql` following the existing
  init-script conventions.

## 1. Architecture overview

```
                        Slice A — data foundation
  ┌────────────────────────────────────────────────────────────────┐
  │ scripts/seeds/naija_transactions.py  (seeded, deterministic)   │
  │   → <out>/naija_txn/{seed}/  events.jsonl persons.jsonl        │
  │     graph_edges.jsonl labels.json manifest.json                │
  │ lakehouse: training_snapshot.py → versioned snapshot dirs +    │
  │   manifest.json (reference histograms for drift)               │
  └───────────────┬────────────────────────────────────────────────┘
                  │ A1 dataset dir (labels.json = only ground truth)
                  ▼
                        Slice B — per-family trainers (CPU, seeded)
  ┌─────────────────────────┬───────────────────┬──────────────────┐
  │ fraud_engine.ml.train   │ credit_bureau.    │ graph_ml.        │
  │  FraudAE + FraudCLF     │  ml.train         │  gnn_train       │
  │  fv1, 16 dims           │  CreditMLP        │  --head link|    │
  │                         │  fv1, 12 dims     │  classifier      │
  └───────────┬─────────────┴────────┬──────────┴────────┬─────────┘
              ▼                      ▼                   ▼
   {out}/fraud-ae-v{N}/     {out}/credit-ml-v{N}/  {model_dir}/{tenant}/
   fraud-clf-v{N}/          model.pt + meta.json   graphsage-v{N}/
   model.pt + meta.json                            (+ head.pt, classifier)

                        Slice C — ops control plane
  ┌────────────────────────────────────────────────────────────────┐
  │ services/model-registry (FastAPI + Postgres `platform`, RLS)   │
  │  REST register / promote / rollback / production / versions    │
  │  A/B: experiments, assignment (fail-closed), outcomes, report  │
  │  drift.py sweep every 15 min: PSI>0.25 → Kafka ops.alerts      │
  │    + gauge opendesk_model_drift_psi{family,tenant}             │
  │  trainer.py nightly cron: calibration gate → auto-promote      │
  └───────────────┬────────────────────────────────────────────────┘
                  │ GET /v1/registry/{family}/{tenant}/production
                  │ (MODEL_REGISTRY_URL; file:// artifact URIs)
                  ▼
                     Consumers (score paths, CPU inference)
  ┌─────────────────────────┬───────────────────┬──────────────────┐
  │ fraud-engine            │ credit-bureau     │ graph-ml         │
  │ DetectionRunner         │ POST /v1/credit/  │ score.py /       │
  │ ml_scorer UNION:        │ score blend       │ heuristic        │
  │ 0.5·ae + 0.5·clf        │ 0.6·ml + 0.4·rule │ fallback per     │
  │ evidence ml_blend       │ clamp [300,900]   │ tenant           │
  └─────────────────────────┴───────────────────┴──────────────────┘
                  │ observations + outcomes (feedback loop)
                  ▼
   POST /v1/registry/observations/{scores,features}  → drift sweep
   POST /v1/registry/experiments/{id}/outcomes        → A/B report
   adjudication labels accumulate → nightly trainer re-trains/promotes
```

Resolution order in every consumer (`*/ml/registry_client.py` or
`graph_ml/registry_client.py`, env `MODEL_REGISTRY_URL`): **(a)** the
model-registry `production` record for the family (families: `fraud-ml`,
`credit-ml`, `graphsage`), translated from a `file://<abs path>` artifact URI to
a local scope dir; **(b)** on ANY failure mode (unset URL, 404, timeout,
malformed record, unsupported scheme) — the W31/W33-B bootstrap local-dir scan
(tenant dir, then the global/`_global` dir), unchanged. When resolution came
from the registry, `model_version` is the registry record's version (I2);
otherwise the dir-derived version. No artifact at all → pure rules,
`model_version: "heuristic-v1"` (I1).

## 2. Honest-status table

One row per capability. **REAL** = shipped code with tests in this repo.
**SYNTHETIC-TRAINED** = real training code, but all labels the model has ever
seen are the documented synthetic A1 labels — metrics are on synthetic data and
are never represented as real-world accuracy (I3). **NOT-YET** = the mechanism
does not exist or is not exercised in this environment.

| Capability | Status | Evidence / honest caveat |
| --- | --- | --- |
| A1 labeled synthetic dataset generator | **REAL** | `scripts/seeds/naija_transactions.py` — deterministic (same seed → byte-equal outputs), six injected fraud scenarios + `benign_*` hard negatives, `labels.json` ground truth + `manifest.json` (seed, row counts, per-file sha256). Phones/names hashed (W28 scheme); no raw PII. |
| Fraud learned scorer (FraudAE + FraudCLF) | **SYNTHETIC-TRAINED** | `services/fraud-engine/fraud_engine/ml/` — real PyTorch training loops (determinism/fallback/provenance tests in `services/fraud-engine/tests/test_ml_*.py`), trained ONLY on A1 synthetic labels (`label_provenance` is synthetic by construction; person labels derive from `labels.json` only). No real fraud cases seen yet. |
| Credit learned scorer (CreditMLP) | **SYNTHETIC-TRAINED** | `services/credit-bureau/credit_bureau/ml/` — A1 carries no lending outcomes, so `train.py` derives default-in-12m labels from a documented seeded synthetic outcome model; every meta.json carries `label_provenance: "synthetic"` + `synthetic_outcome_model` constants (asserted in `services/credit-bureau/tests/test_ml_train.py`). |
| GNN link-prediction mode (W31) | **REAL** | `services/graph-ml/graph_ml/gnn_train.py` — trains per tenant on live FalkorDB extracts; convergence + exact-determinism tests. This is the DEFAULT (`--head link`); unchanged by W33. |
| GNN classifier head (W33-B3) | **SYNTHETIC-TRAINED** | `graph_ml/gnn_head.py` + `gnn_labels.py` — real training loop (masked BCE, stratified val split, early stopping, AUC-PR/Brier by repo code), trained on A1 `labels.json` synthetic positives (`sybil_cluster`/`referral_ring`). Opt-in per run via `--head classifier` / `GNN_HEAD=classifier`. |
| Rule fallbacks (D1–D8, credit Go-rule port, graph heuristic) | **REAL** | Always-on and always correct on rule terms; learned paths only ADD (UNION) — a rule AUTO_QUARANTINE hit is never weakened (fraud-engine `detectors/base.py`; credit `rules.py`, faithful port of `services/booking-service/internal/lending/lending.go:388-426`; graph-ml `heuristic.py`). |
| Model registry REST (register/promote/rollback/production) | **REAL** | `services/model-registry/` — round-trip, single-production atomic flip (partial unique index), and cross-tenant RLS tests in `services/model-registry/tests/`. Slim image: no torch/sklearn/MLflow. |
| Consumer registry wiring (`MODEL_REGISTRY_URL`) | **REAL** | Registry-first resolution + bootstrap fallback tested in each consumer (`test_ml_registry_client.py`, `test_registry_client.py`). Only `file://` absolute artifact URIs are supported this wave (shared local volume). |
| Drift monitoring (PSI/KS sweep) | **REAL math + sweep; alert transport partially pending** | `model_registry/drift.py` PSI + KS are pure stdlib with unit tests; the 15-min sweep sets gauge `opendesk_model_drift_psi{family,tenant}` and publishes to `ops.alerts` via an import-guarded publisher (`kafka-python` optional; absent broker/lib → log-only, never crashes). Serving-side observation producers (`POST .../observations/*`) are NOT yet wired into the scoring services — until they are, the sweep honestly skips families with no data. |
| Kafka `ops.alerts` topic | **DECLARE this wave; consume NOT-YET** | Declared in `infra/kafka/create-topics.sh` (this change). The producer side (model-registry drift + training gate) ships with `alerts.py`; a consumer (e.g. notification-service routing) does not exist yet — alerts are also mirrored to logs. |
| A/B testing | **REAL control plane; serving integration NOT-YET** | `model_registry/ab.py` — deterministic sha256 bucketing, fail-closed-to-champion assignment, outcomes, report endpoint (all tested, `test_ab.py`/`test_report.py`). Scoring services do not yet query `/v1/registry/experiments/assignment` per request; promotion is MANUAL by design. |
| Continuous training scheduler | **REAL orchestration; family-trainer bindings NOT-YET** | `model_registry/trainer.py` — nightly cron, calibration gate (Brier ≤ 0.20 AND AUC-PR regression ≤ 0.02 vs production), auto-promote on pass, hold + alert on fail (sabotaged-model fixture test in `test_trainer.py`). Family trainers plug in via the `FamilyTrainer` protocol and live in sibling services; `TRAIN_ENABLED=false` by default. Real adjudication labels accumulate as the platform produces them — real-data validation is therefore NOT-YET, by design growing from day one. |
| Ray distributed training | **REAL code, single-node dev topology; multi-node NOT-YET** | `graph_ml/ray_train.py` — serial fallback and Ray path are bit-identical per tenant (GC5 scheme: `tenant_seed = base + tenant_index`, torch pinned to 1 thread); tested in `test_ray_train.py`. `infra/docker-compose.ray.yml` is YAML-`config`-validated but NOT image-built in this environment; multi-node (`RAY_ADDRESS`) is untested. |
| Production-data validation | **NOT-YET** | No learned model has been validated on real adjudicated fraud/credit outcomes. The machinery (feedback outcomes → A/B report; snapshots → nightly trainer) validates against every labeled case the platform produces as they accumulate. |

## 3. CPU/GPU matrix (I5)

Every component runs CPU-first. Torch lives ONLY in overlay dependency sets —
never in base images. Heuristic deployments import every module safely without
torch (guarded imports raise `MLBackendUnavailable` / `GNNBackendUnavailable` /
`RayUnavailable` at CALL time, never at import time).

| Component | Base image | torch | GPU | Fallback when torch/weights absent |
| --- | --- | --- | --- | --- |
| fraud-engine | `services/fraud-engine/Dockerfile` — torch-free (`requirements.txt`) | Ad-hoc overlay only (`pip install torch`); `fraud_engine.ml` import-guards it | Optional (`--device auto` → cuda-if-present-else-cpu); CI is CPU | Pure D1–D8 rules; `LearnedScorer.load` → `None`; no `ml_blend` evidence |
| credit-bureau | `services/credit-bureau/Dockerfile` — torch-free | `requirements-ml.txt` overlay (`torch==2.8.0`), explicitly "never part of the base image" | Optional; CPU pinned (`map_location="cpu"`, single thread) | Ported Go rule score, `model_version: "heuristic-v1"`, byte-stable |
| graph-ml | `services/graph-ml/Dockerfile` — torch-free | `requirements-gnn.txt` overlay (`torch==2.5.1`, `torch-geometric==2.6.1` — CPU PyPI wheels); `Dockerfile.gnn` overlay image | CUDA base swap documented but deliberately NOT built | Per-tenant `heuristic.py`, `model_version: "heuristic-v1"` |
| model-registry | `services/model-registry/Dockerfile` — slim | **None ever** (no torch/sklearn/pandas/MLflow in the service) | n/a | Kafka down/absent → log-only alerts; DB down → honest 503 `/healthz` |
| Ray fleet | `infra/docker-compose.ray.yml` — overlay on `graph-ml:latest` + gnn reqs + `ray[default]>=2.9,<3` | Via the same overlay | No GPU claims: parallelism is ACROSS tenants, CPU only | Ray absent/unreachable or `--ray-address local` → serial fallback, **bit-identical** per-tenant results |

## 4. Runbooks

### 4a. Bootstrap training (A1 → per-family trainers → artifact registry dirs)

```bash
# 1. Generate the labeled A1 dataset (deterministic; seed via --seed or SEED env)
python scripts/seeds/naija_transactions.py --seed 42 --out data --persons 800 --days 120
#   → data/naija_txn/42/{events,persons,graph_edges}.jsonl, labels.json, manifest.json

# 2. Fraud learned scorer (needs torch in the training environment)
cd services/fraud-engine
python -m fraud_engine.ml.train --dataset ../../data/naija_txn/42 --out ./models/_global
#   [--seed N] [--device auto|cpu|cuda] [--ae-epochs N] [--clf-epochs N]
#   → models/_global/fraud-ae-v1/{model.pt,meta.json} + fraud-clf-v1/…
#     meta.json: seed, git_sha, dataset_manifest_sha256, feature_schema fv1,
#     val metrics (AE losses; CLF AUC-PR/AUC-ROC/Brier), NO wall-clock stamps.

# 3. Credit learned scorer (torch overlay only — never the base image)
cd services/credit-bureau
pip install -r requirements-ml.txt
python -m credit_bureau.ml.train --dataset ../../data/naija_txn/42 --out ./models
#   [--seed N] [--epochs N] [--hidden-dim N] [--lr F]
#   → models/credit-ml-v1/{model.pt,meta.json} (+ label_provenance: "synthetic",
#     synthetic_outcome_model constants, val MAE / AUC-PR / Brier)

# 4. GNN (link mode default; classifier head opt-in)
cd services/graph-ml
pip install -r requirements-gnn.txt
python -m graph_ml.gnn_train --dataset ../../data/naija_txn/42 --model-dir ./models
python -m graph_ml.gnn_train --dataset ../../data/naija_txn/42 --head classifier \
  --tenant naija-txn --model-dir ./models
#   [--seed N] [--epochs N] [--hidden-dim N] [--patience N] [--val-fraction F]
#   → models/naija-txn/graphsage-v{N}/{model.pt,meta.json} (+ head.pt in classifier mode)
```

Serve the artifacts: fraud-engine reads `FRAUD_ML_REGISTRY_DIR` (tenant dir then
`_global`), credit-bureau reads `CREDIT_ML_REGISTRY_DIR` (`{dir}/{tenant}` then
`{dir}/global`), graph-ml reads its `--model-dir` / `GRAPH_ML_MODEL_DIR`. With
no artifacts (or no torch) every service answers pure rules — this is normal
operation, not an error (I1).

### 4b. Promotion (registry REST)

Service: `http://model-registry:7019` (internal; not routed through APISIX).
Boot it with the compose profile: `docker compose --profile model-registry up model-registry`.

```bash
# Register a version (stage=staging; version auto-assigned max+1 if omitted)
curl -X POST http://model-registry:7019/v1/registry/register \
  -H 'Content-Type: application/json' \
  -d '{"family":"fraud-ml","tenant_id":"acme","artifact_uri":"file:///data/models/acme",
       "metrics":{"auc_pr":0.91,"brier":0.09,"data_basis":"synthetic"},
       "seed":42,"dataset_hash":"sha256:<manifest hash>","git_sha":"<sha>"}'

# Promote staging → production (archives current production in ONE transaction;
# 404 if the version is not in staging; 409 on a concurrent-promote race)
curl -X POST http://model-registry:7019/v1/registry/promote \
  -H 'Content-Type: application/json' \
  -d '{"family":"fraud-ml","tenant_id":"acme","version":2}'

# Inspect / roll back
curl http://model-registry:7019/v1/registry/fraud-ml/acme/production     # 404 when empty
curl http://model-registry:7019/v1/registry/fraud-ml/acme/versions
curl -X POST http://model-registry:7019/v1/registry/rollback \
  -H 'Content-Type: application/json' -d '{"family":"fraud-ml","tenant_id":"acme"}'
```

Consumers pick the flip up on their next `LearnedScorer.load` (registry-first,
`file://` artifact URI → local scope dir). Families: `fraud-ml`, `credit-ml`,
`graphsage`.

### 4c. Drift alerts

- Sweep: every `DRIFT_INTERVAL_MINUTES` (default 15), per (family, tenant)
  production row — (a) PSI of serving feature distributions vs the
  training-snapshot reference manifest (`$DRIFT_MANIFEST_DIR/<family>.json`,
  schema `opendesk/training-manifest/v1` written by `training_snapshot.py`);
  (b) PSI + population KS of serving score distributions vs the trailing 7-day
  baseline. Needs ≥ 10 samples (`min_samples`) else that family is skipped
  honestly (skip reasons logged).
- Reference-manifest sync (SPEC-W34 GF2): every snapshot `manifest.json`
  carries the drift contract alongside the legacy keys — `schema:
  "opendesk/training-manifest/v1"`, `features.<name>.histogram.{edges,counts}`
  (10 equal-width bins over observed min/max, degenerate ranges expanded),
  a documented-empty `score_baseline` (snapshots hold labels, not scores;
  the score leg uses the serving 7-day baseline), and a `manifest_hash`
  (sha256 over the canonical JSON minus the hash). Because the registry
  families (`fraud-ml`/`credit-ml`/`graphsage`) differ from the snapshot
  families (`fraud_features`/`credit_features`/`gnn_export`), a sync step
  writes `$DRIFT_MANIFEST_DIR/<registry-family>.json` through the explicit
  `FAMILY_REGISTRY_MAPPING` in `training_snapshot.py`: run
  `python training_snapshot.py --registry-sync $DRIFT_MANIFEST_DIR --sync-only
  [--snapshot-base PATH] [--snapshot-date DATE]` (Spark-free, local/file://
  snapshot trees), or set `REGISTRY_SYNC_DIR` during the Spark run to emit
  the registry manifests next to the snapshot (s3a included).
- Threshold: PSI > `DRIFT_PSI_THRESHOLD` (default 0.25). Bands: <0.1 stable,
  0.1–0.25 moderate, >0.25 alert.
- Where it lands: alert payload `{"type":"model_drift", family, tenant_id,
  subject, kind, psi, ks, threshold, observed_at}` → Kafka topic `ops.alerts`
  (env `ALERTS_TOPIC`); gauge `opendesk_model_drift_psi{family,tenant}` on
  `GET /metrics`.
- Feed it: scoring services (or a batch job) push observations:
  `curl -X POST .../v1/registry/observations/scores -d '{"family":...,"tenant_id":...,"scores":[...]}'`
  (202) and `.../observations/features` with `{"features":{"<name>":[...]}}`.
- What to do: PSI > 0.25 on a feature → check upstream data quality first,
  then re-train via the nightly trainer (4e) on a fresh snapshot; on `score`
  drift → compare against the A/B report before touching the champion. Kafka
  down or `kafka-python` absent → alerts are log-only (grep
  `alert (log-only, kafka unavailable)`); the sweep never crashes (I1).

### 4d. A/B experiments

```bash
# 1. Create (champion = current production version, challenger = candidate)
curl -X POST http://model-registry:7019/v1/registry/experiments \
  -H 'Content-Type: application/json' \
  -d '{"family":"credit-ml","tenant_id":"acme","champion_version":1,
       "challenger_version":2,"pct":10}'

# 2. Per-request assignment (deterministic:
#    sha256(f"{tenant}|{person}|{experiment}") % 100 < pct → challenger)
curl 'http://model-registry:7019/v1/registry/experiments/assignment?family=credit-ml&tenant_id=acme&person_id=per-0001'
#   → {"arm":"challenger","version":2,...}. FAIL-CLOSED to champion on a
#   missing/inactive/expired experiment or ANY error.

# 3. Record labeled/unlabeled outcomes (single write path)
curl -X POST http://model-registry:7019/v1/registry/experiments/<id>/outcomes \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id":"acme","person_id":"per-0001","assigned_arm":"challenger",
       "predicted_label":0,"predicted_score":0.12,"true_label":0}'

# 4. Report: per-arm precision/recall/Brier over labeled outcomes (pure SQL)
curl http://model-registry:7019/v1/registry/experiments/<id>/report
```

**Promotion of a winning challenger is MANUAL** — a human reviews the report
and runs `/v1/registry/promote` (4b). There is deliberately no auto-promotion
path from A/B (human gate, SPEC-W33 §4 C3).

### 4e. Continuous training (nightly calibration gate)

- Scheduler: APScheduler cron at `TRAIN_CRON_HOUR` (default 2), only when
  `TRAIN_ENABLED=true`. Per family via the pluggable `FamilyTrainer` protocol
  (`model_registry/trainer.py`; trainers live in sibling services):
  `latest_snapshot()` → `train(snapshot, seed)` → **calibration gate:
  Brier ≤ `TRAIN_BRIER_MAX` (0.20) AND AUC-PR regression vs current production
  ≤ `TRAIN_AUCPR_TOLERANCE` (0.02)** → auto-promote `staging→production`;
  else the version stays `staging` and an alert
  (`type: training_gate_failed`, gate detail included) goes to `ops.alerts`.
  First promotion of a family has no production baseline: only the Brier leg
  applies.
- Sabotage behavior (GC6, tested): a trainer returning a degraded model
  (Brier above the floor or AUC-PR regression > 2 points) is registered but
  HELD in staging + alerted — production is never touched by a failing
  candidate. A trainer exception is reported as `decision: "error"` +
  `training_tick_error` alert; the tick never crashes (I1).
- Manual tick (no waiting for cron): `run_nightly_tick(store, trainers)` is a
  plain callable — invoke from a REPL/job against the same store.
- Provenance: the new version row records `dataset_hash` = snapshot manifest
  hash, `seed` = snapshot seed, `git_sha` = build sha, and the gate inputs in
  `metrics.gate` (I2).

### 4f. Ray fleet training (multi-tenant parallelism)

Honest framing: Ray trains a FLEET of tenants concurrently; one tenant's
GraphSAGE trains fine in a single process (`gnn_train.py`). No GPU claims, no
data-parallel claims.

```bash
# Dev topology (single-node CPU cluster): build the base first, then the overlay
docker compose -f infra/docker-compose.graph.yml build graph-ml
docker compose -f infra/docker-compose.ray.yml build
docker compose -f infra/docker-compose.ray.yml up -d   # 1 head (1 CPU) + 2 workers (1 CPU each)

# Driver (batch, any box with the datasets + model volume):
docker compose -f infra/docker-compose.ray.yml run --rm ray-head \
  python -m graph_ml.ray_train --all-tenants \
    --datasets-root /datasets --model-dir /data/graph-ml-models \
    --ray-address ray-head:6379
#   [--head link|classifier] [--seed N] [--epochs N] [--hidden-dim N]
#   [--patience N] [--val-fraction F]

# Serial fallback (no cluster; also the test path) — bit-identical per-tenant
# results:  --ray-address local
python -m graph_ml.ray_train --all-tenants --datasets-root ./datasets \
  --model-dir ./models --ray-address local

# Multi-node (untested in this environment): run the head on one host, workers
# elsewhere with RAY_HEAD_ADDR=<head>:6379, and pass --ray-address (or set
# RAY_ADDRESS) to the driver.
```

Determinism (GC5): per-tenant seed = `base_seed + tenant_index` (tenant index =
sorted discovery order under `--datasets-root`); the SAME worker function runs
as Ray remote task and inline in the serial path, with torch pinned to 1
thread. A tenant below the min-size gate yields `status: "fallback"` — the
per-tenant heuristic fallback is NOT an error; exit 0. Tune fleet size via
`RAY_HEAD_CPUS`, `RAY_WORKER_CPUS`, `RAY_WORKER_REPLICAS` (or
`--scale ray-worker=N`).

## 5. Provenance & determinism notes (I2 / I3)

- **`model_version` everywhere.** Fraud ML responses stamp
  `fraud-ae-vN+fraud-clf-vN` (or the registry record's version when resolved
  via the registry); credit stamps `credit-ml-v{N}` else `heuristic-v1`, plus
  `ml_contribution` (blend delta) and `feature_schema: "fv1"`; graph-ml stamps
  `graphsage-v{N}` else `heuristic-v1`. Rules never impersonate learned models
  and vice versa.
- **GB1-style bit-identical training.** All trainers seed python + torch
  (`seed_everything`), use seeded `torch.Generator` weight init, deterministic
  splits (fraud: seeded stdlib shuffle 90/10; GNN head: seeded stratified
  80/20), single torch thread, and write NO wall-clock timestamps into
  meta.json — two identical invocations produce byte-equal `meta.json` and
  bit-identical final val loss. `trained_at` binds to the dataset (manifest
  sha / A1 deterministic generation stamp), not the clock.
- **Dataset hash in every meta.** Fraud meta.json carries
  `dataset_manifest_sha256` (sha256 of `manifest.json`); GNN artifacts carry
  the dataset fingerprint sha256 over the joined files
  (`gnn_labels.dataset_fingerprint`) + `dataset_seed`; credit meta.json carries
  the synthetic outcome-model constants + `label_provenance: "synthetic"`;
  registry rows carry `dataset_hash`, `seed`, `git_sha` (nightly trainer writes
  snapshot manifest hash + seed + build sha).
- **Frozen feature schemas.** Fraud `fv1` = 16 dims
  (`fraud_engine/ml/features.py`, frozen order, per-channel amount stats frozen
  from the seed-42 reference generation); credit `fv1` = 12 dims
  (`credit_bureau/ml/features.py`, neutral-midpoint defaults for missing
  signals). Any change requires a schema bump (`fv2`), never an in-place edit;
  a mismatched artifact is ignored with a warning and falls back to rules.
- **A1 label quirk (loaders know it).** `persons.jsonl` fraud/scenario fields
  are NEVER used as ground truth — injected persons sampled from the benign
  population keep `fraud: false` on their person row (704 of them on the
  default seed-42 generation). Supervision comes exclusively from
  `labels.json`, joined three ways in the fraud trainer (`per-*` direct,
  `evt-*` via `person_id`, `edg-*` via edge actors) and directly on
  `entity_id == person_id` in `gnn_labels.py`. GNN positives are only
  `sybil_cluster`/`referral_ring`; other fraud scenarios are MASKED OUT of loss
  and metrics (neither positive nor trusted negative); `benign_*` entries are
  hard negatives.
- **Synthetic honesty (I3).** Every reported metric is a validation-set number
  against A1 synthetic labels. When real adjudication labels accumulate, the
  A/B report and the nightly gate consume them — and the metrics payloads keep
  their data-basis label (`data_basis`, `label_provenance`) so synthetic vs
  real is never blurred.
