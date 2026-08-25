#!/bin/sh
# OpenDesk backup — SPEC-W3 §1, hardened SPEC-W43 G-04 (DATA#8/REL#14).
#
# Captures a timestamped snapshot of the three stateful systems:
#   1. Postgres  — pg_dumpall --globals-only (roles/tablespaces) +
#                  pg_dump -Fc per database (all service DBs)
#   2. MinIO     — mc mirror of the `lake` and `exports` buckets
#   3. TigerBeetle — data-file copy (container paused during copy)
#
# FAIL-LOUD CONTRACT (W43): ANY failed target (pg_dump, globals dump, mc
# mirror, TB copy) makes the script exit NONZERO. The manifest records a
# per-target success boolean (`<target>: ok|failed`) which restore.sh's
# pre-flight reads: it refuses to destroy live state from an incomplete
# snapshot. "Mostly worked" is a backup failure, not a backup.
#
# Rotation: keeps the newest BACKUP_KEEP (default 7) timestamped dirs
# (restic-style: each snapshot is a self-contained directory).
#
# Usage:
#   ./infra/backups/backup.sh [backup-root]        # default ./backups
#   BACKUP_KEEP=14 ./infra/backups/backup.sh /backups
#
# Sidecar mode (ofelia in docker-compose.observability.yml): when the
# script runs inside the backup container, the docker socket belongs to the
# HOST, so `docker run -v` needs a host-visible source. Set
# BACKUP_DOCKER_VOLUME=<named-volume> (e.g. agora_backups) and the MinIO
# mirror step mounts that volume instead of a host path.
set -eu

BACKUP_ROOT="${1:-${BACKUP_ROOT:-./backups}}"
KEEP="${BACKUP_KEEP:-7}"
TS="$(date -u +%Y%m%dT%H%M%SZ)"
DEST="$BACKUP_ROOT/$TS"

POSTGRES_CONTAINER="${POSTGRES_CONTAINER:-postgres}"
TB_CONTAINER="${TB_CONTAINER:-tigerbeetle}"
PG_USER="${PG_USER:-opendesk}"
# W41 fix: the default list previously omitted notifications, billing and kyc
# (created by infra/postgres/init-scripts/00-create-dbs.sql) plus platform
# (created by infra/postgres/init-scripts/30-model-registry.sql).
PG_DBS="${PG_DBS:-identity booking conversation knowledge analytics_meta notifications billing kyc platform temporal keycloak permify iceberg twenty crm_sync opendesk}"
MINIO_ALIAS_URL="${MINIO_ALIAS_URL:-http://minio:9000}"
MINIO_ROOT_USER="${MINIO_ROOT_USER:-minioadmin}"
MINIO_ROOT_PASSWORD="${MINIO_ROOT_PASSWORD:-minioadmin}"
MINIO_BUCKETS="${MINIO_BUCKETS:-lake exports}"
MC_IMAGE="${MC_IMAGE:-minio/mc:RELEASE.2024-07-11T18-01-46Z}"
# W41 fix: the compose project is `agora` (root docker-compose.yml line 1), so
# the `opendesk` bridge network is created as `agora_opendesk` — the previous
# default `opendesk` matched no real network and the mc mirror step always
# failed. Override via COMPOSE_NETWORK for non-default project names.
COMPOSE_NETWORK="${COMPOSE_NETWORK:-agora_opendesk}"
TB_DATA_FILE="${TB_DATA_FILE:-/data/0_0.tigerbeetle}"
TB_PAUSE="${TB_PAUSE:-1}"

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*"; }
warn() { printf '%s WARN %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

mkdir -p "$DEST/postgres" "$DEST/minio" "$DEST/tigerbeetle"

# Per-target success booleans (written to the manifest; consumed by
# restore.sh's pre-flight). result <key> <ok|failed>.
RESULTS=""
FAILED=0
result() {
  RESULTS="${RESULTS}${1}: ${2}\n"
  if [ "$2" != "ok" ]; then FAILED=1; fi
}

# Absolute path / volume source for the MinIO mirror container mount.
if [ -n "${BACKUP_DOCKER_VOLUME:-}" ]; then
  MOUNT_SRC="$BACKUP_DOCKER_VOLUME"
else
  # Resolve to an absolute host path (busybox-compatible).
  MOUNT_SRC="$(cd "$BACKUP_ROOT" && pwd)"
fi

# ---------------- 1a. Postgres: cluster globals (roles/tablespaces) ---------
# pg_dump never carries cluster-global roles; without this a restore into a
# FRESH cluster has no app_* roles to grant (restore.sh applies this file
# plus the 05-app-roles.sql role layer before pg_restore).
log "postgres: dumping cluster globals -> $DEST/postgres/globals.sql"
if docker exec "$POSTGRES_CONTAINER" pg_dumpall -U "$PG_USER" --globals-only > "$DEST/postgres/globals.sql" 2>"$DEST/postgres/globals.err"    && [ -s "$DEST/postgres/globals.sql" ]; then
  rm -f "$DEST/postgres/globals.err"
  result postgres_globals ok
  log "  globals: ok ($(wc -c < "$DEST/postgres/globals.sql" | tr -d ' ') bytes)"
else
  warn "  globals: pg_dumpall --globals-only failed (see globals.err)"
  rm -f "$DEST/postgres/globals.sql"
  result postgres_globals failed
fi

# ---------------- 1b. Postgres: pg_dump per database ------------------------
log "postgres: dumping databases -> $DEST/postgres"
for db in $PG_DBS; do
  if docker exec "$POSTGRES_CONTAINER" pg_dump -U "$PG_USER" -Fc "$db" > "$DEST/postgres/$db.dump" 2>"$DEST/postgres/$db.err"      && [ -s "$DEST/postgres/$db.dump" ]; then
    rm -f "$DEST/postgres/$db.err"
    result "postgres_db_$db" ok
    log "  $db: ok ($(wc -c < "$DEST/postgres/$db.dump" | tr -d ' ') bytes)"
  else
    warn "  $db: pg_dump failed (see $db.err)"
    rm -f "$DEST/postgres/$db.dump"
    result "postgres_db_$db" failed
  fi
done

# ---------------- 2. MinIO: mc mirror lake + exports ------------------------
log "minio: mirroring buckets ($MINIO_BUCKETS) -> $DEST/minio"
for bucket in $MINIO_BUCKETS; do
  if docker run --rm --network "$COMPOSE_NETWORK" \
      -v "$MOUNT_SRC:/backup" \
      --entrypoint /bin/sh \
      "$MC_IMAGE" -c "
        mc alias set local '$MINIO_ALIAS_URL' '$MINIO_ROOT_USER' '$MINIO_ROOT_PASSWORD' >/dev/null &&
        mc mirror --overwrite 'local/$bucket' '/backup/$TS/minio/$bucket'
      "; then
    result "minio_bucket_$bucket" ok
    log "  $bucket: ok"
  else
    warn "  $bucket: mirror failed"
    result "minio_bucket_$bucket" failed
  fi
done

# ---------------- 3. TigerBeetle: data-file copy ----------------------------
# The data file is only consistent while the process is frozen (TB writes
# are checksummed, so a torn copy would likely still recover, but pausing is
# cheap and makes the snapshot clean). Set TB_PAUSE=0 to skip the pause.
log "tigerbeetle: copying $TB_DATA_FILE (pause=$TB_PAUSE)"
if [ "$TB_PAUSE" = "1" ]; then
  docker pause "$TB_CONTAINER" >/dev/null
fi
TB_OK=0
if docker cp "$TB_CONTAINER:$TB_DATA_FILE" "$DEST/tigerbeetle/0_0.tigerbeetle"    && [ -s "$DEST/tigerbeetle/0_0.tigerbeetle" ]; then
  TB_OK=1
fi
if [ "$TB_PAUSE" = "1" ]; then
  # Always unpause, even when the copy failed.
  docker unpause "$TB_CONTAINER" >/dev/null
fi
if [ "$TB_OK" = "1" ]; then
  result tigerbeetle ok
  log "  tigerbeetle: ok"
else
  warn "  tigerbeetle: copy failed"
  rm -f "$DEST/tigerbeetle/0_0.tigerbeetle"
  result tigerbeetle failed
fi

# ---------------- Manifest + rotation ---------------------------------------
{
  echo "timestamp: $TS"
  echo "postgres_dbs: $PG_DBS"
  echo "minio_buckets: $MINIO_BUCKETS"
  echo "tigerbeetle_file: $TB_DATA_FILE"
  echo "# per-target success booleans (SPEC-W43 G-04; restore.sh pre-flight"
  echo "# refuses to restore a snapshot whose required artifacts are absent)"
  # RESULTS accumulates literal 
-separated lines.
  printf '%b' "$RESULTS"
} > "$DEST/manifest.txt"
log "manifest written"

# Keep newest $KEEP snapshots (restic-style keep-last).
cd "$BACKUP_ROOT"
# shellcheck disable=SC2012
SNAPS="$(ls -1d 2???????T??????Z 2>/dev/null | sort -r || true)"
COUNT="$(printf '%s\n' "$SNAPS" | grep -c . || true)"
if [ "$COUNT" -gt "$KEEP" ]; then
  DROP="$(printf '%s\n' "$SNAPS" | tail -n +$((KEEP + 1)))"
  for d in $DROP; do
    log "rotation: dropping $d"
    rm -rf "$d"
  done
fi

if [ "$FAILED" -ne 0 ]; then
  warn "FAILED targets recorded in $DEST/manifest.txt — snapshot $TS is INCOMPLETE;"
  warn "restore.sh pre-flight will refuse it. Fix the failing target and re-run."
  exit 1
fi
log "done: $DEST ($COUNT snapshot(s), keep $KEEP)"
