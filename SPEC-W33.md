# SPEC-W33 — Real End-to-End AI/ML/DL Stack (Build Contract)

**Status:** build contract · **Date:** 2026-08-06 · **Wave:** W33 (owner directive: close the honest ML gaps)
**Prereqs:** W28 (graph), W29 (predictive layer), W30 (fraud detectors), W31 (GraphSAGE recs), lakehouse overlay (MinIO/Iceberg/Trino/Spark/dbt), analytics-pipeline bronze sinks.

## §0 Audit verdict (what is true today — the contract's baseline)

Established by 3-agent evidence audit (file:line verified, md5 double-read):
1. **Zero weight files repo-wide.** The only trainable model is GraphSAGE (recs), trained per tenant at runtime; even biometric/voice models are runtime-downloaded or mocks — nothing vendored.
2. **Fraud D1–D8 is 100% rule/threshold-based**; `anomaly.py` is median/MAD z-score, NOT isolation forest; credit `Score()` is a 3-signal rule sum; propensity is 3 hand-weighted formulas.
3. **The W31 GNN training loop is real** (Adam/BCE/epochs, convergence + exact-determinism tests) and trains on live FalkorDB extracts — but has no val split, no early stopping, no eval metrics, no offline dataset.
4. **No labels / no ground truth / no accuracy metrics anywhere.** Validation is synthetic-fixture-only; adjudication outcomes are recorded but never fed back.
5. **No registry server** (filesystem versioned dirs only), **no drift monitoring**, **no A/B testing**, **no Ray** (single 1-core Spark worker), **no experiment tracking** (zero MLflow code despite rumors).
6. Lakehouse: 4 Kafka topics → bronze Iceberg works; `cac.events`, campaign spend, and graph export have **TODO producers**; Spark jobs manually invoked; catalog is the dev iceberg-rest fixture.
7. Synthetic seeds encode genuine Nigerian realism (774 real LGAs, FX regimes, channel mix, naira ranges, Pidgin) but have **no transaction-behavior model** and no fraud labels.
8. Everything inference-side is CPU-capable (heuristic numpy-only; GNN CPU-first wheels + `map_location="cpu"`).

**Invariants (never break):**
- I1 honest degradation: every learned model has a rule-based fallback that is always correct; model failure → fallback, exit 0, logged. (Extends W29 gate-5 / W31 §0.1.)
- I2 honest provenance: every score/edge carries `model_version`; learned = `*-v{N}` from the registry, rules = `heuristic-v1`. No silent swaps.
- I3 honest metrics: any accuracy claim is computed by code in this repo against a labeled dataset shipped in this repo, with the dataset generation seed recorded. Synthetic-only validation is stated as synthetic.
- I4 tenant isolation + single write path (W28/W29 rules unchanged). Registry stages are per (model_family, tenant-or-global).
- I5 CPU-first: every model trains and infers on CPU; GPU/Ray are optional accelerators, never required. torch stays out of base images (overlay pattern).
- I6 PII discipline: training exports carry hashed phones, no raw PII (W28 hashing scheme).

## §1 Program slices

W33-A data foundation → W33-B learned models → W33-C ops (registry/monitor/AB/Ray/continuous). Each slice ships independently gated. B depends on A's labeled datasets; C depends on B's models having registry entries.

## §2 W33-A — Data foundation

### A1. Nigerian transaction-behavior generator (new: `scripts/seeds/naija_transactions.py` + tests)

Deterministic seeded generator (mimesis/stdlib RNG only, matching `scripts/seeds` conventions; seed via `SEED` env, default 42) producing **labeled** synthetic training data with realistic Nigerian patterns:

- **Entities:** persons (en_NG names, +234 phones hashed with the W28 SHA-256+salt scheme), POS agents, merchants, offerings; geography from the REAL `scripts/seeds/data/nigeria_lgas.csv` (774 LGAs) with metro weighting (Lagos/Kano/FCT/PH) and lat/lon inside Nigeria bbox.
- **Transaction sequences** (the gap): per-person time-ordered event streams over a configurable horizon (default 180 days): POS purchase, bank transfer (NIBSS-style), USSD session, agent cash-in/cash-out, booking, cancellation. Inter-arrival ~ exponential per persona (salary_worker, market_trader, student, agent); salary-day spike (25th–31st); hour-of-day curve (USSD/transfer evening peak 17–21h, POS midday).
- **Amounts (NGN):** log-normal per channel with documented regimes — transfers median ≈ ₦8,500 (p95 ≈ ₦120k), POS median ≈ ₦3,200, agent cash-in median ≈ ₦5,000, airtime/USSD ₦100–₦2,000 heavy tail; salary credits ₦50k–₦450k. Round-number bias (multiples of ₦500/₦1,000 over-represented).
- **Fraud injection WITH LABELS** (`labels: true` rows + `labels.json` ground-truth manifest): inject configurable rates (default 1.5%) of: referral rings (3–6 persons circular REFERRED + reward bookings), sybil clusters (same agent + same geo cell + burst + similar names), velocity bursts (>30 captures/hour by one agent), geo-impossibility (>120 km/h consecutive captures), ghost bookings (≥3 create→cancel ≤10min by same staff/day), structuring (many ₦9xxk sub-threshold transfers). Benign look-alikes (dense family referrals, market-day bursts) as hard negatives, labeled `fraud: false, scenario: benign_*`.
- **Outputs:** parquet (pyarrow) or JSONL fallback under `scripts/seeds/out/naija_txn/{seed}/`: `events`, `persons`, `graph_edges` (REFERRED/BOOKED/CAPTURED shapes matching `extract.TenantGraph` fields), `labels.json` {entity_id, scenario, fraud, injected_at}. Manifest carries seed + generation timestamp + row counts + injection rates (I3).
- **Tests:** determinism (same seed → byte-equal outputs), distribution sanity (amount percentiles within documented bands, hour histogram peaks 17–21h), label completeness (every injected scenario instance labeled; hard negatives labeled false), PII check (no raw +234 numbers in outputs — hashed only).

### A2. Pipeline closure (production/lakehouse → training datasets)

- **cac.events → bronze Iceberg:** extend `services/analytics-pipeline` consumer to land `cac.events` into `iceberg.bronze.cac_events` (closing the `cac_analytics.py:7` TODO), offset-commit-after-append semantics unchanged, auto-create table like the other 4.
- **Graph export producer (closes `graph_export.py:18-20` TODO):** graph-service gains internal endpoints `GET /v1/graph/internal/export/nodes` and `.../edges` (X-Internal-Token, tenant-scoped, JSONL streaming, PII-hashed) that `infra/lakehouse/spark/jobs/graph_export.py` consumes to write `iceberg.gold.graph_nodes` / `graph_edges`.
- **Training snapshot job (new Spark job `training_snapshot.py`):** lakehouse silver/gold → versioned training datasets at `s3://lake/training/{family}/{snapshot_date}/` (parquet, partitioned by tenant) + `manifest.json` (seed/source tables/row counts/created_at). Families: `fraud_features` (per-person structural features matching `features.py` semantics + labels joinable from A1), `credit_features`, `gnn_export`. Runnable manually (docker exec, documented) and later by the W33-C scheduler.
- **Tests:** consumer unit test for cac bronze append; export endpoint auth (401 wrong token) + tenant scoping + hash check; snapshot job manifest schema validation.

**Gates W33-A:** GA1 determinism byte-equal; GA2 label completeness incl. hard negatives; GA3 distribution bands; GA4 PII-free exports; GA5 pipeline round-trip (cac producer→bronze row visible via Trino in integration test env or pyiceberg in unit); GA6 honest docs (README updates state synthetic-only validation).

## 3. Slice B — Learned models: real PyTorch training loops, weights, CPU inference

Scope. Everything trains on Slice A labeled data (bootstrap) and, once the platform
accumulates them, on adjudication labels from the fraud feedback store (continuous
training input). Every learned path keeps the W30/W31 rule fallback: if weights are
absent for a tenant, scoring returns the current rule score with
`model_version: "heuristic-*"` (I1). Inference is CPU-only by default (I5).

We cannot ship `.pt` binaries through the text-only Git push channel, so weights are
produced two honest ways, both fully scripted:
  1. **Build-time bootstrap training**: each training script is deterministic
     (`SEED` env, default 42) and runs in CI / image build / container init to produce
     weights from Slice A synthetic data into the registry volume.
  2. **Continuous training**: the W33-C scheduler re-trains on platform snapshots and
     promotes through the calibration gate.

### B1 — Fraud learned scorer (autoencoder + supervised classifier)

Extends `services/fraud-api` as a sibling package `services/fraud-api/fraud_api/ml/`:
  - `features.py`: deterministic feature vector builder v1 (16 dims, hashed-person
    aggregates: velocity windows 1h/24h/7d, amount z-stats per channel, round-number
    rate, night-hour rate, geo-distance last-txn, device-count, referral degree).
    Frozen schema versioned `fv1` in every score payload.
  - `autoencoder.py`: `FraudAE` — MLP autoencoder 16→8→3→8→16, BCE/MSE reconstruction
    loss; anomaly score = reconstruction error. Training: Adam, lr 1e-3, 200 epochs
    max, 90/10 train/val split, early stopping patience 20, seeded `torch.Generator`.
  - `classifier.py`: `FraudCLF` — MLP 16→32→16→1, BCE; supervised on A1 labels
    (fraud scenarios + benign hard negatives). Reports AUC-PR, AUC-ROC, Brier on val.
  - `train.py`: CLI `python -m fraud_api.ml.train --dataset <parquet|jsonl> --out
    <dir>` → `{out}/fraud-ae-v{N}/model.pt + meta.json` and `fraud-clf-v{N}/…`;
    meta.json: seed, git sha, dataset manifest hash, feature schema, val metrics.
  - `scorer.py`: `LearnedScorer` loads both models for a tenant (tenant-scoped
    registry dir per I4; global fallback dir for single-tenant bootstrap); score =
    `0.5*ae_norm + 0.5*clf_prob` blended, then UNION with existing rule verdicts —
    a rule hit never goes away (I1); when the blend crosses `score_threshold` the
    decision reasons include `"ml_blend ae=<x> clf=<y>"`. `model_version` stamped
    `fraud-ae-vN`/`fraud-clf-vN`; absent weights → pure rules, same as today.
  - Wiring: `fraud_api/scoring.py` gets `ml_scorer` optional kwarg; `/v1/score` and
    `/v1/score/batch` unchanged contract; feedback store labels (confirmed/suspected/
    cleared) are exported by `training_snapshot.py` (A2) as the continuous-training
    label source.

### B2 — Credit learned scorer v2

`services/credit-bureau/ml/` (same pattern): `features.py` (fv1: utilization, dpd
history, on-time rate, income band, tenure, exposure/limits, LGA group, product mix),
`model.py` `CreditMLP` regression (score 300–900) + classification head
(default-in-12m), `train.py` on A1 lending-outcome synthetic + repayment events,
`scorer.py` with blend `0.6*ml + 0.4*rule` and fallback to the existing
`Score()` rules; provenance `credit-ml-vN`. Keeps W24 reason codes: the ML score
never suppresses rule reasons, it adds `model_version` + `ml_contribution` fields.

### B3 — GNN supervised head (optional, behind config)

`graph-ml` gains `gnn_train.py --head classifier`: same GraphSAGE encoder, plus a
2-layer node-classification head trained on A1's labeled sybil/referral-ring nodes
(masked BCE, per-class precision/recall on val). Default remains the W31
link-prediction mode; classifier mode is opt-in per tenant via
`GNN_HEAD=classifier`. Registry layout unchanged (`graphsage-v{N}`).

### Gates (verified by a fresh independent agent)
- GB1 Determinism: two identical `train.py` invocations (same seed, same dataset)
  produce byte-equal `meta.json` and bit-identical final val loss.
- GB2 Metric floor on A1 labeled holdout: fraud-clf AUC-PR ≥ 0.80 and Brier ≤ 0.15
  (synthetic separability is high by design; floors reflect that honestly — and the
  report must state metrics are on synthetic data, I3).
- GB3 Fallback integrity: with registry dir empty, `/v1/score` output is byte-equal
  to pre-W33-B rule output for a fixed event fixture; with weights present, any rule
  AUTO_QUARANTINE hit remains AUTO_QUARANTINE (rules never weakened).
- GB4 CPU inference: scoring 100 events through the learned path on CPU-only
  container completes p95 < 50 ms/event.
- GB5 Provenance: every ML-scored response carries non-null `model_version` +
  `feature_schema`; registry meta.json round-trips through `load_latest`.
- GB6 Credit: same determinism/fallback/provenance gates adapted; blended score stays
  within 300–900 and rule reasons are never dropped.
- GB7 Combined suite green (fraud-api + credit-bureau + graph-ml, torch present and
  simulated-absent), plus new tests: training determinism, metric-floor smoke,
  fallback byte-equality, provenance round-trip.

## 4. Slice C — Ops: registry, drift, A/B, Ray, continuous training

### C1 — Model registry service (`services/model-registry/`)
FastAPI + Postgres (platform DB, RLS, migration `V30__model_registry.sql`):
tables `model_family(name)`, `model_version(family, tenant_id, version, artifact_uri,
metrics jsonb, seed, dataset_hash, created_at)`, stage transitions
`staging → production → archived` with single-production-per-(family,tenant)
enforced by partial unique index. REST: `POST /v1/registry/register`,
`POST /v1/registry/promote`, `POST /v1/registry/rollback`, `GET
/v1/registry/{family}/{tenant}/production`. Consumers (fraud-api, credit-bureau,
graph-ml) resolve "which artifact do I load" through this service, with the W31
local-dir scan as bootstrap fallback when the service is empty (I1 honest degrade).
This replaces the audit finding "no deployed registry" — MLflow is NOT adopted
(runs counter to I5 slim-image discipline and adds a second stack); our registry is
200 lines and covers exactly our stage/promotion/rollback needs.

### C2 — Drift monitoring (`services/model-registry/drift.py` + scheduled job)
PSI + population KS on (a) incoming feature distributions vs training-snapshot
reference, (b) score distributions vs last-7d baseline. Runs every 15 min
(APScheduler, same pattern as reconciliation); thresholds PSI > 0.25 → alert on
Kafka `ops.alerts` (consumed by notification-service) + Prometheus gauge
`opendesk_model_drift_psi{family,tenant}`. Reference stats are written by
`training_snapshot.py` (A2) into each snapshot manifest.

### C3 — A/B testing (`services/model-registry/ab.py`)
Deterministic bucketing `sha256(tenant|person|experiment) % 100 < pct → challenger`;
experiments table (family, tenant, champion_version, challenger_version, pct,
start/end). Scoring services query assignment per request (fail-closed to champion).
Outcomes join via the existing feedback store → report endpoint
`GET /v1/registry/experiments/{id}/report` (champion vs challenger precision/recall/
Brier on labeled outcomes). Promotion of a winning challenger is manual via
`/promote` — no auto-promotion from A/B (human gate).

### C4 — Ray distributed compute (compose profile `ray`)
`infra/docker-compose.ray.yml`: Ray head (1 CPU) + 2 workers (1 CPU each, tunable),
image overlay on `graph-ml-gpu` pattern (torch included, NOT base images, I5).
`services/graph-ml/graph_ml/ray_train.py`: `ray.remote` wrapper over per-tenant
`train_tenant` (and fraud/credit trainers) so a fleet of tenants trains in parallel;
driver CLI `python -m graph_ml.ray_train --all-tenants`. Honest framing in docs:
Ray here is for multi-tenant parallelism and future horizontal scale; a single-node
CPU cluster is the dev topology, multi-node is `RAY_ADDRESS` away.

### C5 — Continuous training scheduler (`services/model-registry/trainer.py`)
Nightly cron (APScheduler): for each family: pull latest `training_snapshot` → run
family trainer (Ray if cluster up, else local) → evaluate on holdout → **calibration
gate: Brier ≤ 0.20 AND no regression > 2 pts AUC-PR vs current production** →
auto-promote to `production` (registry stage flip) else stay `staging` + alert.
Full provenance chain recorded: snapshot manifest hash + seed + git sha in
model_version row. First run uses Slice A bootstrap data; later runs blend in real
adjudication labels as they accumulate (the honest answer to "never validated
against real fraud cases": the machinery validates against every labeled case the
platform produces, from day one).

### Gates (verified by a fresh independent agent)
- GC1 Registry: register → promote → rollback round-trip via REST; single-production
  invariant enforced (second promote flips atomically); RLS blocks cross-tenant read.
- GC2 Consumers: fraud-api with registry-empty boots and scores (bootstrap fallback);
  with registry populated loads `production` artifact and stamps its version.
- GC3 Drift: inject shifted distribution fixture → PSI alert emitted on `ops.alerts`
  within one scheduler tick; unshifted → no alert.
- GC4 A/B: deterministic assignment (same person twice → same arm across processes);
  50/50 fixture converges within 45–55% over 10k ids; fail-closed to champion when
  experiment row missing; report endpoint returns both arms' metrics.
- GC5 Ray: `ray_train --all-tenants` on 3-tenant fixture completes with per-tenant
  registries populated; identical results with cluster down (local fallback).
- GC6 Continuous training: run scheduler tick manually on bootstrap snapshot → new
  version registered, calibration gate decision recorded, promotion correct both
  when gate passes and when a sabotaged model fixture fails the gate.
- GC7 Combined suite green + all new services' tests; compose profiles `ray` and
  `model-registry` boot healthy in docker-compose config validation.
- GC8 Docs: `docs/ml-platform.md` — architecture, honest-status table (what is real
  vs synthetic-trained vs not-yet), CPU/GPU matrix, runbooks for bootstrap training,
  promotion, drift alerts, A/B experiments.
