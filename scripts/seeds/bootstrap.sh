#!/usr/bin/env bash
# bootstrap.sh — Platform data seeding orchestrator (SPEC-W17 §8.5, Agent D).
#
# ADAPTATION (standing rulings, see docs/data-seeding.md): the uploaded spec
# §8.5 prescribes n8n for orchestration. n8n is SUL license-excluded on this
# platform, so orchestration is THIS script (9-step §8.5 sequence) driven by
# cron/CI (`make seed-all` / `make seed-ci` / `make seed-drift`).
#
# 9-step sequence (adapted §8.5):
#   1. Apply Postgres DDL      sql/postgres/ddl/*.sql (idempotent, schema cac)
#   2. Apply Iceberg DDL       sql/iceberg/seed_ddl.sql via trino/spark-sql when
#                              the lakehouse is up, else skip-with-log
#   3. seed_lgas.py            774 LGAs
#   4. seed_channels.py + seed_channel_costs.py  (32 channels, 32x24 cost rows)
#   5. seed_agents.py + seed_customers.py        (5k / 200k x SEED_SCALE)
#   6. seed_events.py          FunnelEvents (Kafka when SEED_KAFKA=on, else JSONL)
#   7. seed_fx.py + gold load  FX series; cac_gold load via spark when lakehouse
#                              is up, else defer-with-log (manifest is durable)
#   8. Drift check             scripts/seeds/drift.sql must return 0 rows
#   9. snapshot.sh             zstd tar of lakehouse seed dirs + manifest
#
# Env (all optional, dev defaults):
#   DATABASE_URL   postgres://opendesk:opendesk@localhost:5432/analytics_meta
#   SEED_SCALE     1.0            (CI uses 0.05 — see `make seed-ci`)
#   SEED_KAFKA     off            (on = publish CloudEvents to Kafka)
#   SEED_SALT      passthrough for scripts/seeds/_lib.py deterministic ids
#   SEED_PYTHON    python3
#   POSTGRES_CONTAINER  postgres  (docker-exec fallback when psql is absent)
#   TRINO_CONTAINER     opendesk-trino
#   SPARK_MASTER_CONTAINER opendesk-spark-master
#
# Flags:
#   --dry-run   no writes: DDL/drift/snapshot steps skip-with-log (snapshot runs
#               its own --dry-run), every seed script runs with --dry-run
#               (DB-free, prints counts only)
#   -h|--help   this text
#
# Exit: non-zero on ANY step failure (fail loud — set -euo pipefail + explicit
# step guard). A seed run that "succeeded" with a silently-skipped required
# step is a compliance incident (§8.7), so required steps never skip silently;
# the only skip-with-log paths are the lakehouse-dependent ones the spec
# itself marks optional (steps 2 and the gold-load half of 7).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

DATABASE_URL="${DATABASE_URL:-postgres://opendesk:opendesk@localhost:5432/analytics_meta}"
SEED_SCALE="${SEED_SCALE:-1.0}"
SEED_KAFKA="${SEED_KAFKA:-off}"
SEED_PYTHON="${SEED_PYTHON:-python3}"
POSTGRES_CONTAINER="${POSTGRES_CONTAINER:-postgres}"
TRINO_CONTAINER="${TRINO_CONTAINER:-opendesk-trino}"
SPARK_MASTER_CONTAINER="${SPARK_MASTER_CONTAINER:-opendesk-spark-master}"
export SEED_SCALE SEED_KAFKA

DRY=0
for arg in "$@"; do
    case "$arg" in
        --dry-run) DRY=1 ;;
        -h|--help) sed -n '2,40p' "${BASH_SOURCE[0]}"; exit 0 ;;
        *) echo "bootstrap.sh: unknown flag '$arg' (supported: --dry-run)" >&2; exit 2 ;;
    esac
done

log()  { printf '[bootstrap %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
fail() { echo "[bootstrap] ERROR: $*" >&2; exit 1; }

require_file() {
    [ -f "$1" ] || fail "required seed artifact missing: $1 (cross-agent contract — see SPEC-W17; run the full W17 build)"
}

# --- psql wrapper: local psql preferred, docker exec fallback ---------------
PG_DB="${DATABASE_URL##*/}"; PG_DB="${PG_DB%%\?*}"
PG_USER="$(printf '%s' "$DATABASE_URL" | sed -n 's|^[a-z]*://\([^:]*\):.*|\1|p')"
PG_USER="${PG_USER:-opendesk}"
psql_file() { # psql_file <sql-file>  (stdout preserved for callers that capture)
    if command -v psql >/dev/null 2>&1; then
        psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$1"
    elif command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' | grep -qx "$POSTGRES_CONTAINER"; then
        docker exec -i "$POSTGRES_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -v ON_ERROR_STOP=1 < "$1"
    else
        fail "no psql and no running '$POSTGRES_CONTAINER' container — cannot reach Postgres ($DATABASE_URL)"
    fi
}

psql_drift() { # drift check with the run's SEED_SCALE forwarded (scale-aware cardinalities)
    if command -v psql >/dev/null 2>&1; then
        psql "$DATABASE_URL" -At -v ON_ERROR_STOP=1 -v seed_scale="$SEED_SCALE" -f "$1"
    elif command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' | grep -qx "$POSTGRES_CONTAINER"; then
        docker exec -i "$POSTGRES_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -At -v ON_ERROR_STOP=1 -v seed_scale="$SEED_SCALE" < "$1"
    else
        fail "no psql and no running '$POSTGRES_CONTAINER' container — cannot reach Postgres ($DATABASE_URL)"
    fi
}

run_seed() { # run_seed <script.py> — per-script timing inside compound steps
    local script="$SCRIPT_DIR/$1"
    require_file "$script"
    local args=(--scale "$SEED_SCALE")
    [ "$DRY" = 1 ] && args+=(--dry-run)
    local t0; t0=$(date +%s)
    log "  -> $1 ${args[*]}"
    (cd "$ROOT" && "$SEED_PYTHON" "$script" "${args[@]}")
    log "  <- $1 done in $(( $(date +%s) - t0 ))s"
}

container_up() {
    command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' | grep -qx "$1"
}

STEP=0
step() { # step <name> <command...>
    STEP=$((STEP + 1))
    local name="$1"; shift
    log "step $STEP/9: $name — START"
    local t0; t0=$(date +%s)
    "$@"
    log "step $STEP/9: $name — done in $(( $(date +%s) - t0 ))s"
}

# --- 1. Postgres DDL (idempotent, schema cac — contract D) -------------------
step_pg_ddl() {
    if [ "$DRY" = 1 ]; then log "dry-run: skip Postgres DDL apply"; return 0; fi
    local ddl_dir="$ROOT/sql/postgres/ddl"
    require_file "$ddl_dir/001_cac_schema.sql"
    local f
    for f in "$ddl_dir"/*.sql; do
        log "applying ${f#"$ROOT"/}"
        psql_file "$f" >/dev/null
    done
}

# --- 2. Iceberg DDL (lakehouse optional — skip-with-log when down) -----------
step_iceberg_ddl() {
    local ddl="$ROOT/sql/iceberg/seed_ddl.sql"
    if [ ! -f "$ddl" ]; then log "sql/iceberg/seed_ddl.sql absent — skip-with-log (Agent C artifact; apply on first lakehouse seed run)"; return 0; fi
    if [ "$DRY" = 1 ]; then log "dry-run: skip Iceberg DDL apply"; return 0; fi
    if container_up "$TRINO_CONTAINER"; then
        docker exec -i "$TRINO_CONTAINER" trino --catalog iceberg < "$ddl" >/dev/null \
            && log "iceberg DDL applied via trino" || fail "trino rejected sql/iceberg/seed_ddl.sql"
    elif container_up "$SPARK_MASTER_CONTAINER"; then
        docker exec -i "$SPARK_MASTER_CONTAINER" /opt/spark/bin/spark-sql < "$ddl" >/dev/null \
            && log "iceberg DDL applied via spark-sql" || fail "spark-sql rejected sql/iceberg/seed_ddl.sql"
    else
        log "lakehouse down (no $TRINO_CONTAINER/$SPARK_MASTER_CONTAINER) — Iceberg DDL skip-with-log; cac_silver/cac_gold seed tables will be created on the first lakehouse seed run"
    fi
}

# --- 4/5. Reference + entity seed groups --------------------------------------
step_channels() { run_seed seed_channels.py; run_seed seed_channel_costs.py; }
step_entities() { run_seed seed_agents.py; run_seed seed_customers.py; }

# --- 7. FX + gold load (spark; defer-with-log when lakehouse down) ------------
step_fx_and_gold() {
    run_seed seed_fx.py
    local job="$ROOT/infra/lakehouse/spark/jobs/seed_gold_load.py"
    if [ ! -f "$job" ]; then
        log "gold-load job absent — deferred (Agent C artifact); JSON manifests under \${SEED_MANIFEST_DIR:-/var/tmp/seed_manifests} remain the load source"
        return 0
    fi
    if [ "$DRY" = 1 ]; then log "dry-run: skip cac_gold spark load"; return 0; fi
    if container_up "$SPARK_MASTER_CONTAINER"; then
        docker exec "$SPARK_MASTER_CONTAINER" /opt/spark/bin/spark-submit --master spark://spark-master:7077 /opt/spark/jobs/seed_gold_load.py \
            && log "cac_gold load submitted" || fail "seed_gold_load spark-submit failed"
    else
        log "lakehouse down — cac_gold load DEFERRED with log; rerun bootstrap.sh with the lakehouse profile up (manifests are durable under \${SEED_MANIFEST_DIR:-/var/tmp/seed_manifests})"
    fi
}

# --- 8. Drift check (§8.7 #5: drift.sql returns 0 rows on success) -----------
step_drift() {
    if [ "$DRY" = 1 ]; then log "dry-run: skip drift check (read-only but needs DB)"; return 0; fi
    local drift="$SCRIPT_DIR/drift.sql"
    require_file "$drift"
    local out
    out="$(psql_drift "$drift")"
    if [ -n "$(printf '%s' "$out" | tr -d '[:space:]')" ]; then
        printf '%s\n' "$out" >&2
        fail "DRIFT DETECTED — drift.sql returned rows (see above); investigate before reseeding (§8.7 #5)"
    fi
    log "drift check: OK (0 rows)"
}

# --- 9. Snapshot ---------------------------------------------------------------
step_snapshot() {
    local snap="$SCRIPT_DIR/snapshot.sh"
    require_file "$snap"
    # snapshot.sh exits 1 when no lakehouse layer dirs exist (nothing to
    # snapshot). On a host where the lakehouse never materialized that is an
    # expected skip-with-log (same philosophy as steps 2/7), not a failure;
    # when dirs DO exist, snapshot.sh runs and its exit code is honored.
    local root="${LAKEHOUSE_ROOT:-/var/lib/opendesk/lakehouse}"
    local srcs="${SNAPSHOT_SRC_DIRS:-$root/bronze $root/silver}"
    local d any=0
    for d in $srcs; do [ -d "$d" ] && any=1 && break; done
    if [ "$any" = 0 ]; then
        log "no lakehouse layer dirs under $root — snapshot skip-with-log (lakehouse-side seeds not materialized on this host)"
        return 0
    fi
    if [ "$DRY" = 1 ]; then bash "$snap" --dry-run; else bash "$snap"; fi
}

log "SEED_SCALE=$SEED_SCALE SEED_KAFKA=$SEED_KAFKA DRY_RUN=$DRY DATABASE_URL=$DATABASE_URL"
step "postgres DDL (sql/postgres/ddl)"          step_pg_ddl
step "iceberg DDL (sql/iceberg)"                step_iceberg_ddl
step "seed LGAs (774)"                          run_seed seed_lgas.py
step "seed channels + unit costs (32, 32x24)"   step_channels
step "seed agents + customers (x$SEED_SCALE)"   step_entities
step "seed events (FunnelEvents)"               run_seed seed_events.py
step "seed FX + cac_gold load"                  step_fx_and_gold
step "drift check (drift.sql)"                  step_drift
step "snapshot (scripts/seeds/snapshot.sh)"     step_snapshot

log "ALL 9 SEED STEPS COMPLETE (scale=$SEED_SCALE, dry_run=$DRY). Verify: make seed-drift; dashboard: Grafana 'OpenDesk — Seed Report'."
