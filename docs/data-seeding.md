# Platform Data Seeding (SPEC-W17)

Master doc for the CAC platform data seeding strategy (uploaded spec "Section
8: Platform Data Seeding Strategy") as implemented against the real OpenDesk
repo. Covers: spec→implementation mapping with **every adaptation**, the
schema contract, per-environment runbook, refresh triggers (§8.6), acceptance
gates (§8.7), compliance (§8.8) and risks (§8.9).

Owners (SPEC-W17): Agent A — DDL + `_lib.py` + reference seeds; Agent B —
synthetic entity seeds; Agent C — lakehouse/geo/Mojaloop; Agent D —
orchestration, compliance, docs, acceptance gates (this doc).

---

## 1. Spec §8.x → implementation mapping (incl. all adaptations)

Standing OpenDesk rulings override the uploaded spec's tool choices. Every
substitution is deliberate and listed here.

| Uploaded spec §8 | CAC-doc tool/approach | Implementation (actual) | Why |
|---|---|---|---|
| §8.1 lakehouse medallion | Delta Lake bronze/silver/gold | **Iceberg** lakehouse (`sql/iceberg/seed_ddl.sql`, `cac_silver.*`, `cac_gold.*`; W13 idioms) | Iceberg is the standing platform lakehouse (infra/lakehouse) |
| §8.5 orchestration | n8n workflow | **`scripts/seeds/bootstrap.sh`** (9-step sequence below) + `make seed-all/seed-ci/seed-drift` + cron/CI | **n8n is SUL — license-excluded (SUL ruling)** |
| §8.2 synthetic generation | @faker-js/faker | **mimesis (MIT)** for ALL generation; en_NG-style name lists + Pidgin seed strings embedded in `seed_customers.py` | One Python generator stack; faker-js dropped |
| §8.2 row 2 wards | real ward list | **8,812 stable synthetic wards** ("Ward NN — <LGA>") distributed per LGA (`seed_wards.py`) | Real ward lists unavailable offline (documented substitution) |
| §8.2 row 7 locale strings | localized customer fields | Pidgin/en_NG strings drive **generation only**; `cac.customers` carries **no** `preferred_language`/`notes` columns | **Accepted deviation** (lead ruling on Agent B flag #2): keeps the customer table minimal; locale realism is exercised at generation time, not persisted |
| §8.3 FX rates | Langflow FX job / crawled rates | **Plain Python** `scripts/seeds/seed_fx.py`: anchored random walk 2021-08 ≈ ₦410 → 2026-08 ≈ ₦1,550 with official/parallel spread; ≥365 contiguous points (actual 1,826) | Langflow not platform-standard; no crawling offline |
| §8.2 channel costs | crawled unit economics | Deterministic pseudo-random walk per channel (`seed_channel_costs.py`), 32×24 monthly rows | No real crawling offline (documented) |
| §8.2 CMS locale seed | Wagtail/Strapi | **Validation report** over `industries/*.yaml` i18n blocks (`seed_locale.py`) → `cac.seed_run_log` + console table (en/pcm/ha/yo/ig per pack) | Existing pack i18n is the locale substrate; no new CMS |
| §8.2 engagement apps | Prism/Chatwoot/Mautic mocks | **NOT seeded** — license-excluded apps | License exclusions stand |
| §8.2 geo | OSM/Geofabrik mirror | Sedona job `seed_geo_points.py` generates LGA-anchored settlement points (offline GeoJSON from `cac.lgas` centroids when mirror absent — ASSUMPTION annotated by Agent C) | Offline operation |
| §8.4 radio coverage | real station lists | 774 LGAs × 200 synthetic stations, codes `NG-<STATE>-<nnn>` (`seed_coverage.py`) | Real lists unavailable offline |
| §8.9 Mojaloop | live simulator install | **Config/docs only**: `deploy/mojaloop/seed-simulator.md` + `simulator-participants.yaml` (1 hub + 5 DFSPs, `dfsp-seed-1..5`, NGN, staging) | No live cluster here |
| §8.8 TB accounts | TigerBeetle synthetic accounts | `scripts/seeds/out/tigerbeetle_accounts.json` manifest, `account_type=90`, one per agent | TB seam from W14 |

## 2. Components

| Component | Path | Notes |
|---|---|---|
| Shared seed lib (contract A) | `scripts/seeds/_lib.py` | `deterministic_id`, `hash_pii` (argon2, salt discarded), `emit_seed_report`, `log_seed_run`, `scaled`, all DB via helpers |
| Postgres DDL (contract D) | `sql/postgres/ddl/001_cac_schema.sql` | schema `cac`, 8 tables, idempotent |
| Iceberg DDL | `sql/iceberg/seed_ddl.sql` | `cac_silver.*`, `cac_gold.*`, day-partitioned where temporal |
| Reference seeds | `seed_lgas.py` (774), `seed_wards.py` (8,812), `seed_channels.py` (32), `seed_channel_costs.py` (768), `seed_locale.py` | fixed cardinality |
| Entity seeds | `seed_agents.py` (5,000×scale), `seed_customers.py` (200,000×scale), `seed_events.py` (50/customer×scale), `seed_fx.py` (1,826 daily) | scale via `SEED_SCALE`/`--scale` |
| Lakehouse jobs | `infra/lakehouse/spark/jobs/seed_geo_points.py`, `seed_coverage.py`, `seed_gold_load.py` | spark-submit when lakehouse up |
| Orchestrator | `scripts/seeds/bootstrap.sh` | §8.5 9-step sequence (n8n substitution) |
| Make targets | `Makefile`: `seed-all`, `seed-ci`, `seed-drift` | additive |
| Drift gate | `scripts/seeds/drift.sql` | §8.7 #5 — 0 rows on success |
| Collision guard | `scripts/seeds/collision_guard.py` | §8.7 #3 — BVN-shaped guard vs seeded idspace |
| Seed report dashboard | `infra/observability/dashboards/seed-report.json` | **path adaptation**: spec said `infra/grafana/dashboards/`; provisioning mounts `infra/observability/dashboards` (see `grafana/provisioning/dashboards/dashboards.yml`), so the dashboard lives where Grafana actually loads it. Panel `seed_report_summary` reads `cac.seed_run_log` via the provisioned `opendesk-postgres` datasource (additive entry in `grafana/provisioning/datasources/datasources.yml`) |
| Consent erasure fast-path | `services/identity-service/internal/consent/eligibility.go` | §8.8 — see §7 below |
| Snapshot | `scripts/seeds/snapshot.sh` | zstd tar of lakehouse seed dirs + manifest |
| Acceptance gates | `tests/seeds/test_acceptance.py` | DB-free §8.7 checks |

## 3. Schema contract (schema `cac`, database `analytics_meta`)

Platform-level synthetic reference data. **No tenant RLS on `cac.*`**: the
tables contain no tenant data and no real-person PII (all PII columns are
verification-only digests), so tenant isolation is N/A — enforcing RLS would
only break cross-tenant analytics joins (documented per SPEC-W17 contract D).

Tables (PKs are deterministic text ids; every data table carries
`is_synthetic boolean NOT NULL DEFAULT true` + `seeded_at timestamptz`):
`cac.lgas`, `cac.wards`, `cac.channels`, `cac.channel_unit_costs`,
`cac.agents`, `cac.customers`, `cac.fx_series` (daily key `series_date`),
`cac.seed_run_log`.

`cac.seed_run_log` (the drift/dashboard contract — one row per table, latest
run wins, upserted by `_lib.log_seed_run`):
`id text PK` = `deterministic_id('seed_run_log:<table>')`, `table_name`,
`rowcount`, `runner_id`, `git_sha`, `seeded_at`.

Deterministic ids (contract A): `sha256(SEED_SALT + "|" + natural_key)` hex.
Dev default salt `opendesk-dev-seed-salt-change-in-prod` — **rotate in any
shared/prod environment** (seed ids must not be guessable across envs, and a
salt change intentionally re-keys the whole synthetic idspace).
Entity natural keys (Agent B): `customer:{i:08d}`, `agent:{i:06d}`.

## 4. Runbook

### Local dev (full stack)
```bash
make up                       # postgres (+ lakehouse profile if desired)
pip install -r scripts/seeds/requirements-seeds.txt
make seed-all                 # 9-step bootstrap, SEED_SCALE=1.0
make seed-drift               # must print OK (0 rows)
```
`DATABASE_URL` defaults to `postgres://opendesk:opendesk@localhost:5432/analytics_meta`
(inside the compose network: host `postgres`). `bootstrap.sh` uses local
`psql` when present, else `docker exec postgres psql`.

### CI (DB-free)
```bash
make seed-ci                  # SEED_SCALE=0.05 SEED_KAFKA=off
# or fully DB-free:
./scripts/seeds/bootstrap.sh --dry-run
pytest tests/seeds tests/lakehouse
```
`--dry-run` skips DDL/drift/snapshot writes with logs and runs every seed
script with `--dry-run` (prints counts, no DB).

### Staging / prod-like
```bash
SEED_SALT=<env-secret> SEED_SCALE=1.0 SEED_KAFKA=on \
  DATABASE_URL=postgres://... make seed-all
```
Kafka on ⇒ CloudEvents to `cac.seed.report.<table>.v1` + FunnelEvents on
`cac.events`; off ⇒ JSONL outbox at `/var/tmp/seed_reports.jsonl` (same
payload shape). Iceberg DDL + gold load apply automatically when the
lakehouse is up, else skip/defer **with log** (manifests persist under
`/var/tmp/seed_manifests/`; rerun bootstrap after lakehouse start).

### 9-step sequence (bootstrap.sh, adapted §8.5)
1. apply `sql/postgres/ddl/*.sql` → 2. apply `sql/iceberg/seed_ddl.sql` via
trino/spark-sql when lakehouse up, else skip-with-log → 3. `seed_lgas` →
4. `seed_channels` + `seed_channel_costs` → 5. `seed_agents` +
`seed_customers` → 6. `seed_events` → 7. `seed_fx` + gold-load
(spark when up, else defer-with-log) → 8. drift check (`drift.sql` must
return 0 rows; any row = non-zero exit) → 9. `snapshot.sh` (skip-with-log
when no lakehouse layer dirs exist on the host — nothing to snapshot; when
dirs exist its exit code is honored, fail-loud).
`set -euo pipefail`, per-step timing, `SEED_SCALE` passthrough, `--dry-run`.

## 5. Refresh triggers (§8.6)

| Trigger | Action |
|---|---|
| Monthly | reseed channel unit costs (new 24-month window): `seed_channel_costs.py` then gold load |
| Daily (staging) | `seed_fx.py` (series extends; ≥365 contiguity gate) — dashboard "Last seed run" panel goes red >24h |
| Schema/DDL change | full reseed: `make seed-all && make seed-drift` |
| Salt rotation | full reseed (entire idspace re-keys; coordinate downstream caches) |
| New pack/locale | `seed_locale.py` validation report |
| Post-incident / suspected tampering | `make seed-drift` + `python3 scripts/seeds/collision_guard.py` |

Idempotency (contract B) makes every refresh safe: deterministic id →
`DELETE WHERE id IN (...)` → `INSERT ... ON CONFLICT (id) DO UPDATE` →
report event → `seed_run_log` upsert; fail-loud on any exception.

## 6. Acceptance gates (§8.7)

| # | Gate | How to run | Pass |
|---|---|---|---|
| 1 | Idempotency: double `--dry-run` equality | `pytest tests/seeds/test_acceptance.py -k double_dry_run` | identical deterministic output |
| 2 | Completeness: generator cardinalities | `pytest tests/seeds/test_acceptance.py -k cardinality` | 774 / 8,812 / 32 / 768 / scale-math / ≥365 FX |
| 3 | Collision guard | `python3 scripts/seeds/collision_guard.py` (JSON summary on stdout) | 0 collisions vs regenerated seeded idspace (customer+agent) |
| 4 | Determinism across runs | `pytest tests/seeds -k determin` (Agent A/B suites) | same ids |
| 5 | Drift check | `make seed-drift` / `psql -f scripts/seeds/drift.sql` | **0 rows** |
| 6 | FX contiguity | drift.sql arm + `seed_fx.py` self-gate | gap-free daily series |
| 7 | FunnelEvent shape | Agent B entity tests vs `analytics_pipeline/cac_events.py` fields | envelope match |
| 8 | TB manifest | Agent B tests | `account_type=90` |
| 9 | Dashboard loads | Grafana → "OpenDesk — Seed Report" | panels green, `seed_report_summary` populated |
| 10 | Suite | `pytest tests/seeds tests/lakehouse` (DB-free) | all green |

## 7. Compliance (§8.8)

- **`is_synthetic=true` on every seeded row** (+ `seeded_at`); drift.sql has a
  `non_synthetic_rows` arm and the dashboard a dedicated red stat — any
  real-person row in `cac.*` is a compliance incident.
- **PII storage**: phone/name columns exist ONLY as `hash_pii` digests
  (argon2id low-level, per-call random salt, **salt discarded** →
  non-reversible, verification-only; scrypt fallback logs loudly). Digests
  are not stable across runs by design and are never join keys.
- **BVN collision guard (§8.7 #3)**: 1,000 deterministic real-BVN-shaped
  11-digit strings hashed through the same `deterministic_id` path; asserted
  disjoint from the full seeded customer∪agent idspace (regenerated from the
  `customer:{i:08d}`/`agent:{i:06d}` natural-key contract — DB-free; `--idspace`
  file or live `DATABASE_URL` optional for an as-written check).
- **Consent erasure fast-path** (`internal/consent/eligibility.go`,
  ADDITIVE): `EvaluateErasureEligibility` classifies seed-tagged subjects —
  64-hex deterministic ids or `seed:`/`seed-` prefixes — as
  **immediate-eligible, skipping any waiting period**. No waiting period
  exists in current code (W12 erasure is tombstone-only immediate), so real
  subjects are behaviour-unchanged; the check is the seam where a future NDPA
  waiting period plugs in, with the synthetic short-circuit already first.
  The erasure response and the `ErasureRequested` CloudEvent now carry
  `synthetic: true|false` so downstream anonymizers can fast-path too.
  The synthetic **phone band is NOT auto-classified** (see §8 risk 1).
- **TigerBeetle**: synthetic accounts use `account_type=90` (manifest only).
- **License-excluded apps** (Prism/Chatwoot/Mautic/n8n) are never installed
  or seeded.
- Erasure of seed data itself: `DELETE FROM cac.<table>` is always safe —
  reseeding reproduces the identical idspace from the same salt.

## 8. Risks (§8.9)

1. **Synthetic phone band overlaps real allocations.** The W17 generator band
   `+234 80XX XXX XXXX` normalizes onto real Nigerian prefixes (0803/0806…).
   Mitigations: PII phones stored only as digests; the consent erasure
   fast-path deliberately does NOT classify by phone band (only id patterns),
   so a real person's erasure can never be fast-pathed by accident.
2. **Dev salt reuse.** `opendesk-dev-seed-salt-change-in-prod` is public;
   rotate `SEED_SALT` outside local dev or synthetic ids become guessable.
3. **scrypt fallback performance.** Without `argon2-cffi`, entity seeding is
   ~20–30× slower (observed: 10k customers 27s argon2 vs >100s scrypt) — CI
   must install `requirements-seeds.txt`.
4. **Lakehouse-down deferrals.** Steps 2/7 skip-with-log by design; a staging
   env that never runs with the lakehouse up has empty `cac_silver/cac_gold`
   seed tables — the dashboard covers Postgres only; check spark job logs.
5. **Mojaloop simulator** is config/docs-only (staging); never point its
   participants at a production hub (isolation warnings in
   `deploy/mojaloop/seed-simulator.md`).
6. **Ward/station/cost data is synthetic by construction** — never present it
   externally as observed reality; it is volume/topology filler for CAC
   analytics development.
7. **Snapshot size** at scale 1.0 (50k geo points + coverage grid) — use
   `snapshot.sh --dry-run` to plan; snapshots exclude any real dataset by
   construction but verify before off-box export.

## 9. Environment reference

| Var | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | `postgres://opendesk:opendesk@localhost:5432/analytics_meta` | seed Postgres (schema `cac`) |
| `SEED_SCALE` | `1.0` (CI `0.05`) | cardinality multiplier for agents/customers/events |
| `SEED_SALT` | `opendesk-dev-seed-salt-change-in-prod` | deterministic-id salt — rotate outside dev |
| `SEED_KAFKA` | `off` | `on` = publish CloudEvents; else JSONL outbox |
| `SEED_REPORT_PATH` | `/var/tmp/seed_reports.jsonl` | report outbox when Kafka off |
| `SEED_MANIFEST_DIR` | `/var/tmp/seed_manifests` | FX/TB manifest dir for gold load |
| `SEED_DATABASE_URL` | = `DATABASE_URL` | `make seed-drift` override |
