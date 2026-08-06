# model-registry (SPEC-W33 §4 C1/C2/C3/C5)

Model version registry + drift monitoring + A/B testing + continuous-training
scheduler for the OpenDesk/Agora ML platform. FastAPI + Postgres (`platform`
DB, FORCE ROW LEVEL SECURITY) + APScheduler + prometheus-client. **No torch,
no sklearn, no pandas, no MLflow** (I5 slim-image discipline).

## Why not MLflow (explicit SPEC decision)

SPEC-W33 §4 C1: *"MLflow is NOT adopted (runs counter to I5 slim-image
discipline and adds a second stack); our registry is ~200 lines and covers
exactly our stage/promotion/rollback needs."* Concretely: MLflow pulls in a
tracking server + its own DB schema + (for the registry UI) gunicorn/flask +
optionally S3 clients — a second stateful stack to run, patch and back up,
for features (experiment UI, model flavors, artifact proxying) the platform
does not use. Our needs are exactly: version rows with provenance, single-
production-per-(family,tenant) with atomic promote/rollback, RLS tenant
isolation. That fits in one small FastAPI service on the platform Postgres
we already operate, so the registry inherits the platform's backup/RLS/
roles conventions for free.

## Migration naming deviation (documented)

SPEC-W33 §4 C1 names the migration `V30__model_registry.sql` (Flyway style).
This repo has **no Flyway**; the convention is
`infra/postgres/init-scripts/NN-name.sql` run once by the postgres
docker-entrypoint-initdb.d psql path. We therefore deliver
`infra/postgres/init-scripts/30-model-registry.sql`, following the existing
scripts exactly (roles pattern from `05-app-roles.sql`, `\c platform` +
FORCE RLS + `current_setting('app.tenant_id', true)` policy from 01/03/04).
Roles `app_model_registry` / `app_model_registry_login` (dev password
`app_model_registry_dev_password`) are created in the same script because
`05-app-roles.sql` is owned by another wave.

## API

Base: `http://model-registry:7019` (internal; not routed through APISIX).

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/healthz` | 200 when DB reachable, 503 otherwise (honest) |
| GET | `/metrics` | Prometheus exposition |
| POST | `/v1/registry/register` | Register version (stage `staging`; version auto-assigned max+1 per (family,tenant) when omitted). Body: `family, tenant_id, artifact_uri, metrics{}, seed?, dataset_hash?, git_sha?, version?` |
| POST | `/v1/registry/promote` | `staging → production`, archiving current production in ONE transaction. 404 if version not in staging; 409 on concurrent-promote race (partial unique index) |
| POST | `/v1/registry/rollback` | Re-promote most recent `archived` version, atomically. 404 if none archived |
| GET | `/v1/registry/{family}/{tenant_id}/production` | Current production row; **404 when empty** (consumers fall back — I1) |
| GET | `/v1/registry/{family}/{tenant_id}/versions` | All versions for a (family, tenant) |
| POST | `/v1/registry/experiments` | Create A/B experiment (`family, tenant_id, champion_version, challenger_version, pct, starts_at?, ends_at?`) |
| GET | `/v1/registry/experiments/assignment?family=&tenant_id=&person_id=` | Per-request arm. **FAIL-CLOSED to champion** on missing experiment or any error |
| POST | `/v1/registry/experiments/{id}/outcomes` | Record one labeled/unlabeled outcome (single write path) |
| GET | `/v1/registry/experiments/{id}/report` | Per-arm precision/recall/Brier over labeled outcomes (pure SQL). 404 for unknown id |
| POST | `/v1/registry/observations/scores` | Push serving scores (drift input) |
| POST | `/v1/registry/observations/features` | Push serving feature values (drift input) |

## A/B semantics (C3)

* Bucketing: `sha256(f"{tenant}|{person}|{experiment}") % 100 < pct → challenger`.
  Stateless and deterministic — same triple → same arm across processes/restarts.
* **Promotion of a winner is MANUAL** via `/v1/registry/promote`. There is no
  auto-promotion path by design (human gate).

## Drift monitoring (C2)

Every `DRIFT_INTERVAL_MINUTES` (default 15) per (family, tenant) production row:

* **(a)** PSI of serving feature distributions (last interval,
  `feature_observations`) vs the training-snapshot reference manifest.
* **(b)** PSI + population KS of serving score distributions (last interval,
  `score_observations`) vs the trailing 7-day baseline.
* PSI > `DRIFT_PSI_THRESHOLD` (default 0.25) → alert on Kafka topic
  `ops.alerts` **and** Prometheus gauge `opendesk_model_drift_psi{family,tenant}`.

### Reference manifest schema (`opendesk/training-manifest/v1`)

Written by the lakehouse `training_snapshot.py` job (Slice A, sibling owner);
this service codes against this documented schema (fixtures in
`tests/fixtures/`). One JSON file per family at `$DRIFT_MANIFEST_DIR/<family>.json`:

```json
{
  "schema": "opendesk/training-manifest/v1",
  "family": "fraud_features",
  "snapshot_date": "2025-06-01",
  "manifest_hash": "sha256:<hex>",
  "seed": 42,
  "features": {
    "<feature>": {"histogram": {"edges": [e0..eN], "counts": [c0..cN-1]}}
  },
  "score_baseline": {"histogram": {"edges": [...], "counts": [...]}}
}
```

`edges` has N+1 ascending edges, `counts` N bin populations; serving values
are binned on the same edges (out-of-range clamps to first/last bin).

## Continuous training (C5)

Nightly cron (`TRAIN_CRON_HOUR`, default 2; only scheduled when
`TRAIN_ENABLED=true`). Per family, through the pluggable `FamilyTrainer`
protocol (`model_registry/trainer.py`) — actual trainers live in sibling
services; this service orchestrates:

1. `latest_snapshot()` → pull latest training snapshot (Slice A output).
2. `train(snapshot, seed)` → train + evaluate on holdout.
3. **Calibration gate:** `brier ≤ 0.20` AND `AUC-PR regression vs current
   production ≤ 0.02` → auto-promote `staging→production`; else stay staging
   + alert on `ops.alerts`.
4. Provenance (I2): the new `model_version` row records snapshot manifest
   hash (`dataset_hash`), snapshot `seed`, service build `git_sha`, and the
   gate inputs inside `metrics.gate`.

The tick is a plain callable — `run_nightly_tick(store, trainers, now)` — so
a gate/operator can invoke it manually without waiting for cron.

## Configuration (env)

| Var | Default | Purpose |
| --- | --- | --- |
| `PORT` | `7019` | HTTP port |
| `MODEL_REGISTRY_PG_DSN` | `postgresql://app_model_registry_login:app_model_registry_dev_password@localhost:5432/platform` | psycopg v3 sync DSN |
| `KAFKA_ENABLED` | `true` | False → log-only alerts |
| `KAFKA_BOOTSTRAP_SERVERS` | `kafka:9092` | Kafka bootstrap |
| `ALERTS_TOPIC` | `ops.alerts` | Alert topic (drift + training gate) |
| `DRIFT_INTERVAL_MINUTES` | `15` | Drift sweep interval |
| `DRIFT_PSI_THRESHOLD` | `0.25` | PSI alert threshold |
| `DRIFT_MANIFEST_DIR` | `/data/manifests` | Reference manifest directory |
| `TRAIN_ENABLED` | `false` | Schedule the nightly trainer cron |
| `TRAIN_CRON_HOUR` | `2` | Nightly cron hour |
| `TRAIN_BRIER_MAX` / `TRAIN_AUCPR_TOLERANCE` | `0.20` / `0.02` | Calibration gate |
| `GIT_SHA` | `unknown` | Build provenance (I2) |

DB driver choice: **psycopg v3, synchronous** — control-plane workload
(low-QPS register/promote/report + two batch schedulers); sync keeps the
atomic-promote transaction and RLS GUC scoping explicit.

## Honest-status notes (I1/I3)

* **Kafka absent or down** → alerts are log-only; scheduler ticks never crash
  (import-guarded producer, `LogOnlyPublisher` fallback). Install
  `kafka-python==2.0.2` to publish for real; the `ops.alerts` topic must be
  declared in `infra/kafka/create-topics.sh` (owned by another wave — tracked
  as an integration TODO, not silently assumed).
* **Registry empty / no production row** → `GET .../production` is a plain
  404; consumers (fraud-api, credit-bureau, graph-ml) fall back to their
  bootstrap artifacts (I1).
* **No reference manifest / insufficient observations** → drift sweep skips
  that family honestly (skip reasons are logged and returned by `run_once`).
* **Metrics provenance**: this service stores whatever `metrics` the
  registering trainer supplies; trainers label synthetic-vs-real data basis
  inside the metrics payload (I3). The registry never computes headline
  metrics itself except the A/B report, which is labeled-outcomes-only.
* **Tenant isolation**: FORCE RLS on all tenant tables; a single write path
  (`model_registry.store.RegistryStore`) sets `app.tenant_id` per
  transaction. Cross-tenant reads exist only for internal batch jobs via
  `app.registry_internal='on'`; HTTP handlers never use it.

## Development

```bash
pip install -r requirements.txt
pip install pgserver 'psycopg[binary]'   # embedded Postgres for tests
mkdir -p /tmp/xdg && export XDG_RUNTIME_DIR=/tmp/xdg
pytest --junitxml=out.xml                 # console summary broken on pytest 9
```
