# SPEC-W17 — Platform Data Seeding (CAC App, Wave 6)

Implements the uploaded "Section 8: Platform Data Seeding Strategy" against the
REAL OpenDesk repo. Four builders, strict ownership. Delivery protocol identical
to SPEC-W12 §Delivery ($HOME workspaces — /tmp gets wiped; additive rsync to
/mnt; md5-verify FROM /mnt; real gate tails).

## Mandatory stack adaptations (the uploaded spec names CAC-doc tools that
## conflict with standing OpenDesk rulings — these rulings WIN):
1. **Iceberg, NOT Delta Lake.** Every "Delta Lake bronze/silver/gold" reference
   maps to the existing Iceberg lakehouse (infra/lakehouse, cac_gold.* from W13).
   Table names: cac_silver.geo_points, cac_silver.wards, cac_silver.coverage,
   cac_gold.channel_unit_costs, cac_gold.usd_shadow_prices, cac_gold.seed_run_log.
2. **NO n8n (SUL — license-excluded).** Orchestration = scripts/seeds/bootstrap.sh
   (9-step sequence §8.5) + cron/CI. Document the substitution.
3. **mimesis (MIT) for ALL synthetic generation** — @faker-js/faker dropped to
   keep one generator stack (Python). Nigerian locale realism via explicit
   en_NG-style data lists + Pidgin seed strings embedded in the seeder.
   Document the substitution.
4. **Langflow → plain Python FX job** (scripts/seeds/seed_fx.py). Wagtail/Strapi
   → existing pack i18n blocks (industries/*.yaml); locale seed = validation
   report, not a CMS.
5. **Prism/Chatwoot/Mautic mocks**: NOT seeded (license-excluded apps). Skip with
   a one-line doc note.
6. Mojaloop simulator seed = config/manifest + docs only (no live cluster here).

## Cross-agent contracts (bind everyone)
A. **scripts/seeds/_lib.py** (Agent A owns, everyone imports):
   - `deterministic_id(natural_key: str) -> str` = sha256(SEED_SALT_CONSTANT + "|" + natural_key) hex.
     SEED_SALT_CONSTANT read from env SEED_SALT with a fixed dev default documented in .env.example.
   - `hash_pii(value: str) -> str` = argon2 (argon2-cffi low-level, per-run random salt, salt DISCARDED —
     output is non-reversible verification-only digest); fallback scrypt (hashlib) if argon2-cffi
     unavailable, logged loudly.
   - `emit_seed_report(table, rowcount, runner_id, git_sha)` → CloudEvent on Kafka topic
     `cac.seed.report.<table>.v1`; when SEED_KAFKA=off (default in CI) append JSONL to
     /var/tmp/seed_reports.jsonl instead — same payload shape. Never fail the seed on report IO.
   - `log_seed_run(table, rowcount, conn)` → upsert row into cac.seed_run_log.
   - `scaled(cardinality: int) -> int` = int(cardinality * SEED_SCALE); SEED_SCALE env float,
     default 1.0 (CI uses 0.05).
   - Pg conn via DATABASE_URL (psycopg v3 if installed, else psycopg2, else psycopg2-binary;
     requirements-seeds.txt pins). All DB access THROUGH _lib helpers so tests can fake them.
B. **Idempotent loader contract (§8.4)** for EVERY seed_<domain>.py:
   deterministic id → DELETE WHERE id IN (<idset>) → INSERT ... ON CONFLICT (id) DO UPDATE →
   emit report event → seed_run_log row → non-zero exit on any exception (fail loud).
   Every script supports `--dry-run` (prints counts, no writes) and `--scale X.Y` override.
C. **Every seeded row carries `is_synthetic boolean NOT NULL DEFAULT true`** (+ `seeded_at timestamptz`).
   TigerBeetle synthetic accounts manifest uses `account_type=90` (§8.8).
D. **Postgres schema `cac`** (NEW schema, does NOT collide with booking RLS tables):
   cac.lgas, cac.wards, cac.channels, cac.channel_unit_costs, cac.agents, cac.customers,
   cac.fx_series, cac.seed_run_log. DDL idempotent (CREATE TABLE IF NOT EXISTS ...),
   in sql/postgres/ddl/ as numbered files. PKs = the deterministic ids (text).
   NOTE: no RLS on cac.* seed tables — they are platform-level synthetic reference data;
   document why tenant RLS is N/A (no tenant PII).
E. **Events seed emits W13-shaped FunnelEvents** (com.opendesk.cac.FunnelEvent on topic
   `cac.events`) so CAC dashboards/analytics fill — reuse the W13 event envelope exactly
   (inspect services/analytics-pipeline/analytics_pipeline/cac_events.py).
F. Python: 3.11+, stdlib-first. Allowed deps (requirements-seeds.txt): mimesis, psycopg[binary]
   (or psycopg2-binary fallback), argon2-cffi, kafka-python (optional import, guarded), pyspark
   ONLY in the lakehouse job (Agent C), pytest for tests. Tests MUST pass with
   `pip install -r scripts/seeds/requirements-seeds.txt` on a clean venv (network via
   pypi.org or npmmirror fallback).

## Agent A — DDL + core lib + reference-data seeds
Owns: sql/postgres/ddl/ (NEW dir: 001_cac_schema.sql — all 8 tables per contract D +
indexes + seed_run_log), scripts/seeds/_lib.py (contract A), scripts/seeds/requirements-seeds.txt,
scripts/seeds/data/nigeria_lgas.csv (ALL 774 LGAs: lga_name, state, zone — real names;
well-known public data, write it out in full), scripts/seeds/seed_lgas.py (774 rows; <60s),
scripts/seeds/seed_wards.py (exactly 8,812 rows; real ward lists are unavailable offline —
generate STABLE synthetic ward names "Ward NN — <LGA>" distributed per LGA summing to 8,812;
document the substitution per §8.2 row 2), scripts/seeds/seed_channels.py (32 hand-curated
Nigerian acquisition channels from the playbook: USSD, SMS, WhatsApp Business, voice/IVR,
radio, agent networks, POS agents, cooperatives, churches/mosques, market associations, etc. —
each with channel_code, name, class above/below-the-line, typical unit economics),
scripts/seeds/seed_channel_costs.py (32 channels × 24 months of unit-cost rows in NGN,
deterministic pseudo-random walk seeded per channel — no real crawling offline; document),
scripts/seeds/seed_locale.py (validation report over industries/*.yaml i18n blocks →
cac.seed_run_log + console table of locale coverage en/pcm/ha/yo/ig per pack),
tests/seeds/test_lib.py + tests/seeds/test_reference_seeds.py (determinism: same ids across
runs; cardinality: 774/8812/32/768; upsert SQL shape; --dry-run works DB-free).
Gates: pytest green DB-free (fake conn), python -m py_compile all seeds, bash -n n/a.

## Agent B — synthetic entity seeds
Owns: scripts/seeds/seed_agents.py (5,000 × SEED_SCALE agents: Nigerian names/phones
+23480[0-9]XXXXXXX-style synthetic, state/LGA assignment from cac.lgas, PII columns stored
ONLY as hash_pii digests + is_synthetic; ALSO emits tigerbeetle_accounts.json manifest —
account_type=90, one account per agent, codes documented for the TB seam from W14),
scripts/seeds/seed_customers.py (200,000 × SEED_SCALE; mimesis Person/Datetime with the
embedded en_NG name lists + Pidgin strings; phone/PII hashed per contract A; channel_of_first_touch
distributed across the 32 channels; LGA-weighted),
scripts/seeds/seed_events.py (50 events/customer × SEED_SCALE, FunnelEvent-shaped per
contract E — stage progression impression→click→contact→qualified→converted with realistic
drop-off; batch publish via kafka-python when SEED_KAFKA=on else JSONL outbox; idempotent
producer semantics documented),
scripts/seeds/seed_fx.py (USD/NGN daily series, 5 years: anchored random walk from a
documented 2021-08 ≈ ₦410 start to ≈ ₦1,550 2026-08 regime with official/parallel spread;
≥365 contiguous points gate; writes cac.fx_series AND an Iceberg-side JSON manifest for
Agent C's gold load),
tests/seeds/test_entity_seeds.py (cardinality×scale math, phone-format regex, FunnelEvent
envelope vs the W13 schema fields, FX contiguity + monotonicity-of-date, TB manifest
account_type=90 assertion). DB-free gates like Agent A.

## Agent C — lakehouse + geo + Mojaloop
Owns: sql/iceberg/seed_ddl.sql (NEW dir; CREATE TABLE IF NOT EXISTS for
cac_silver.wards, cac_silver.geo_points, cac_silver.coverage, cac_gold.channel_unit_costs,
cac_gold.usd_shadow_prices, cac_gold.seed_run_log — PARTITIONED BY days(...) where temporal,
mirroring the W13 cac_analytics job's Iceberg idioms — inspect
infra/lakehouse/spark/jobs/cac_analytics.py first),
infra/lakehouse/spark/jobs/seed_geo_points.py (Sedona job: 50,000 settlement points via
ST/RS random-point generation anchored on LGA polygons read from an offline GeoJSON the job
also generates from cac.lgas centroids when Geofabrik/OSM mirror absent — ASSUMPTION annotated;
writes cac_silver.geo_points GeoParquet; pure-function core (point-in-polygon rejection
sampling, deterministic seed) MUST be unit-testable without Spark),
infra/lakehouse/spark/jobs/seed_coverage.py (radio coverage grid: 774 LGAs × 200 synthetic
station rows — station codes NG-<STATE>-<nnn>, band FM/AM, deterministic),
infra/lakehouse/spark/jobs/seed_gold_load.py (loads Agent A's channel costs + Agent B's FX
JSON manifests into cac_gold.* via Iceberg MERGE/overwritePartitions idiom from W13 + writes
cac_gold.seed_run_log row),
scripts/seeds/snapshot.sh (tar -c --zstd of the lakehouse bronze/silver dirs →
seed-snapshot-v3.tar.zst with a manifest.txt of rowcounts; dry-run flag),
deploy/mojaloop/seed-simulator.md + deploy/mojaloop/simulator-participants.yaml (1 hub +
5 DFSPs: deterministic participant ids dfsp-seed-1..5, currencies NGN, env=staging namespace
label + the §8.9 isolation warnings — config/docs only, NO live install claims),
tests/lakehouse/test_seed_geo.py (pure-function tests: determinism, 50k count at scale 1,
points inside Nigeria bbox, no pyspark import at test time — import guard).
Gates: pytest green; python -m py_compile the three jobs; bash -n snapshot.sh; SQL lint =
parse check via sqlparse if available else careful self-review (say which).

## Agent D — orchestration + compliance + docs + acceptance gates
Owns: scripts/seeds/bootstrap.sh (the §8.5 9-step sequence ADAPTED: 1 apply sql/postgres/ddl,
2 apply sql/iceberg DDL note/via spark-sql when lakehouse up else skip-with-log,
3 seed_lgas, 4 seed_channels + seed_channel_costs, 5 seed_agents + seed_customers,
6 seed_events, 7 seed_fx + gold load note, 8 drift check, 9 snapshot.sh;
set -euo pipefail, per-step timing, SEED_SCALE passthrough, --dry-run),
Makefile (ADDITIVE targets: seed-all, seed-ci (SEED_SCALE=0.05), seed-drift — read the
existing Makefile first, append only),
scripts/seeds/drift.sql (§8.7 #5: compares information_schema + rowcount/hash aggregates of
cac.* vs cac.seed_run_log expectations; returns 0 rows on success),
infra/grafana/dashboards/seed-report.json (NEW panel seed_report_summary: last-run
completion, per-table rowcounts from cac.seed_run_log — inspect an existing dashboard json
for the schema idiom; if provisioning dir differs, follow it),
compliance: scripts/seeds/collision_guard.py (§8.7 #3: hashes a small embedded real-BVN-shaped
dictionary — 1,000 synthetic 11-digit strings — through the SAME deterministic customer-id path
and asserts 0 collisions vs the seeded customer idspace),
identity-service erasure fast-path (ADDITIVE Go change, inspect
services/identity-service/internal/consent + the erasure consumer from W12): when an erasure
request targets a data subject whose record carries is_synthetic=true (or seed-tagged id
pattern), treat as immediate-eligible and skip any waiting period; if no waiting period exists
in current code, add the is_synthetic short-circuit at the eligibility check + unit test —
mirror existing test idioms; go build/vet/test green for the touched package),
docs/data-seeding.md (NEW — the master doc: spec §8.x → implementation mapping table
INCLUDING every adaptation (Iceberg, no-n8n, mimesis-only, no-Langflow, no-Wagtail, no-Prism,
wards synthetic, FX walk not crawled, radio stations synthetic), runbook per environment,
refresh triggers §8.6, acceptance gates §8.7, compliance §8.8, risks §8.9),
tests/seeds/test_acceptance.py (§8.7 gates that can run DB-free: idempotency via double
--dry-run equality, completeness via generator counts, collision guard, drift.sql parses).
Gates: bash -n bootstrap.sh + snapshot.sh, make -n seed-all, pytest green, Go gates if
consent touched. Do NOT push to GitHub.

## Delivery protocol: identical to SPEC-W12 §Delivery ($HOME workspaces).
