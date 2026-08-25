#!/bin/sh
# OpenDesk restore — companion to backup.sh, hardened SPEC-W43 G-04
# (DATA#8/REL#14).
#
# Restores a timestamped snapshot produced by infra/backups/backup.sh:
#   0. PRE-FLIGHT — every artifact the manifest marks `ok` must be present
#      and NONEMPTY BEFORE any dropdb runs (never destroy live state from an
#      incomplete snapshot)
#   1. Postgres  — globals.sql (cluster roles) + role layer
#      (05-app-roles.sql) applied BEFORE pg_restore; per DB:
#      dropdb --force (terminates connections) + createdb + pg_restore
#   2. MinIO     — mc mirror --overwrite --remove the backed-up buckets back
#      over the live ones (--remove: target ends up EXACTLY matching the
#      snapshot; stale objects left over from post-backup writes are deleted)
#   3. TigerBeetle — stop container, copy the data file back, start
#
# DESTRUCTIVE: overwrites live state. Requires RESTORE_CONFIRM=yes.
#
# Usage:
#   RESTORE_CONFIRM=yes ./infra/backups/restore.sh ./backups/20250101T031700Z
#   # or only some systems:
#   RESTORE_CONFIRM=yes SYSTEMS=postgres ./infra/backups/restore.sh <dir>
#   # tolerate targets the manifest marks `failed` (partial snapshot):
#   RESTORE_CONFIRM=yes RESTORE_ALLOW_PARTIAL=yes ./infra/backups/restore.sh <dir>
set -eu

SRC="${1:-}"
if [ -z "$SRC" ] || [ ! -d "$SRC" ]; then
  echo "usage: RESTORE_CONFIRM=yes $0 <backup-dir> [systems]" >&2
  exit 2
fi
if [ "${RESTORE_CONFIRM:-}" != "yes" ]; then
  echo "refusing to restore without RESTORE_CONFIRM=yes (this is DESTRUCTIVE)" >&2
  exit 2
fi
SYSTEMS="${SYSTEMS:-postgres minio tigerbeetle}"
ALLOW_PARTIAL="${RESTORE_ALLOW_PARTIAL:-no}"

POSTGRES_CONTAINER="${POSTGRES_CONTAINER:-postgres}"
TB_CONTAINER="${TB_CONTAINER:-tigerbeetle}"
PG_USER="${PG_USER:-opendesk}"
MINIO_ALIAS_URL="${MINIO_ALIAS_URL:-http://minio:9000}"
MINIO_ROOT_USER="${MINIO_ROOT_USER:-minioadmin}"
MINIO_ROOT_PASSWORD="${MINIO_ROOT_PASSWORD:-minioadmin}"
MC_IMAGE="${MC_IMAGE:-minio/mc:RELEASE.2024-07-11T18-01-46Z}"
# W41 fix: same as backup.sh -- the compose project is `agora` (root
# docker-compose.yml line 1), so the `opendesk` bridge network is
# created as `agora_opendesk`; the previous default `opendesk`
# matched no real network. Override via COMPOSE_NETWORK for
# non-default project names.
COMPOSE_NETWORK="${COMPOSE_NETWORK:-agora_opendesk}"
TB_DATA_FILE="${TB_DATA_FILE:-/data/0_0.tigerbeetle}"
# Role layer applied before pg_restore (cluster-global roles/grants; the
# script is idempotent — pg_roles guards + re-grants). Mounted read-only in
# the postgres container; we stream it from the host over stdin.
ROLE_LAYER_SQL="${ROLE_LAYER_SQL:-$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/../postgres/init-scripts/05-app-roles.sql}"

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*"; }
warn() { printf '%s WARN %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die() { echo "restore PRE-FLIGHT FAILED: $*" >&2; exit 1; }
have() { [ -e "$1" ]; }

# Absolute path / volume source for the mirror container mount.
if [ -n "${BACKUP_DOCKER_VOLUME:-}" ]; then
  # Sidecar mode: pass an absolute path inside the volume, e.g. /backups/<ts>.
  MOUNT_SRC="$BACKUP_DOCKER_VOLUME"
  # W43 fix: the old single-prefix strip (`${SRC#/backups/}`) did not strip
  # ./backups/ or backups/ (or a trailing slash), so the in-volume path was
  # wrong for any relative SRC. Strip every spelling of the backups prefix.
  REL="${SRC%/}"
  REL="${REL#/}"
  REL="${REL#./}"
  REL="${REL#backups/}"
else
  MOUNT_SRC="$(cd "$SRC" && pwd)"
fi

# ---------------------------------------------------------------------------
# 0. PRE-FLIGHT: verify EVERY required artifact exists and is nonempty BEFORE
#    touching live state. Expectations come from the manifest's per-target
#    booleans (W43); for legacy manifests without booleans, every *.dump
#    present must be nonempty and globals.sql absence only downgrades to a
#    warning (the role layer still runs).
# ---------------------------------------------------------------------------
preflight() {
  case " $SYSTEMS " in *" postgres "*) ;; *) return 0 ;; esac
  local_manifest="$SRC/manifest.txt"
  if [ -f "$local_manifest" ]; then
    # Targets explicitly marked failed: refuse unless RESTORE_ALLOW_PARTIAL.
    FAILED_TARGETS="$(sed -n 's/^\([a-z0-9_]*\): failed$/\1/p' "$local_manifest")"
    if [ -n "$FAILED_TARGETS" ] && [ "$ALLOW_PARTIAL" != "yes" ]; then
      die "snapshot has FAILED targets: $(printf '%s ' $FAILED_TARGETS)— incomplete backup; re-run backup.sh or set RESTORE_ALLOW_PARTIAL=yes"
    fi
    # Targets marked ok must have present+nonempty artifacts.
    for kv in $(sed -n 's/^\(postgres_db_[a-z0-9_]*\): ok$/\1/p' "$local_manifest"); do
      db="${kv#postgres_db_}"
      dump="$SRC/postgres/$db.dump"
      { have "$dump" && [ -s "$dump" ]; } || die "missing/empty artifact postgres/$db.dump (manifest says ok)"
    done
    if grep -q '^postgres_globals: ok$' "$local_manifest"; then
      { have "$SRC/postgres/globals.sql" && [ -s "$SRC/postgres/globals.sql" ]; }         || die "missing/empty artifact postgres/globals.sql (manifest says ok)"
    fi
    if grep -q '^tigerbeetle: ok$' "$local_manifest"; then
      case " $SYSTEMS " in *" tigerbeetle "*)
        { have "$SRC/tigerbeetle/0_0.tigerbeetle" && [ -s "$SRC/tigerbeetle/0_0.tigerbeetle" ]; }           || die "missing/empty artifact tigerbeetle/0_0.tigerbeetle (manifest says ok)";;
      esac
    fi
  else
    warn "no manifest.txt — legacy snapshot; falling back to per-dump checks"
  fi
  # Regardless of manifest: never restore from an empty dump.
  for dump in "$SRC"/postgres/*.dump; do
    have "$dump" || continue
    [ -s "$dump" ] || die "empty dump file: $dump"
  done
  if ! have "$SRC/postgres/globals.sql"; then
    warn "no postgres/globals.sql in snapshot — cluster roles will NOT be"
    warn "restored from globals; relying on the role layer ($ROLE_LAYER_SQL)"
  fi
  log "pre-flight OK: all required artifacts present and nonempty"
}

preflight

for system in $SYSTEMS; do
  case "$system" in
    postgres)
      log "postgres: restoring dumps from $SRC/postgres"

      # 1a. Cluster globals first (roles/tablespaces) — pg_restore needs the
      #     roles to exist for ownership/privilege mapping. CREATE ROLE lines
      #     error when the role already exists, so tolerate errors here and
      #     let the idempotent role layer below settle the final state.
      if have "$SRC/postgres/globals.sql"; then
        log "  applying globals.sql (cluster roles; errors for existing roles tolerated)"
        if ! docker exec -i "$POSTGRES_CONTAINER" psql -U "$PG_USER" -d postgres             -v ON_ERROR_STOP=0 -q < "$SRC/postgres/globals.sql" >/dev/null 2>&1; then
          warn "  globals.sql applied with errors (expected when roles already exist)"
        fi
      fi

      # 1b. Drop + recreate every dumped DB. --force terminates connected
      #     backends first (PG13+), so restores no longer stall on open
      #     service connections.
      for dump in "$SRC"/postgres/*.dump; do
        have "$dump" || continue
        db="$(basename "$dump" .dump)"
        log "  $db: drop (--force) + recreate"
        docker exec "$POSTGRES_CONTAINER" dropdb -U "$PG_USER" --force --if-exists "$db"
        docker exec "$POSTGRES_CONTAINER" createdb -U "$PG_USER" "$db"
      done

      # 1c. Role layer BEFORE pg_restore: least-privilege app_* roles and
      #     their grants (05 is idempotent — pg_roles guards + re-grants).
      #     Errors are tolerated with a warning: on a cluster that already
      #     ran the init scripts this is a no-op settle pass.
      if [ -f "$ROLE_LAYER_SQL" ]; then
        log "  applying role layer: $ROLE_LAYER_SQL"
        if ! docker exec -i "$POSTGRES_CONTAINER" psql -U "$PG_USER" -d postgres             -v ON_ERROR_STOP=0 -q < "$ROLE_LAYER_SQL" >/dev/null 2>&1; then
          warn "  role layer applied with errors (expected when roles/grants already exist)"
        fi
      else
        warn "  role layer not found at $ROLE_LAYER_SQL — skipping (set ROLE_LAYER_SQL)"
      fi

      # 1d. Data restore.
      for dump in "$SRC"/postgres/*.dump; do
        have "$dump" || continue
        db="$(basename "$dump" .dump)"
        log "  $db: pg_restore"
        docker exec -i "$POSTGRES_CONTAINER" pg_restore -U "$PG_USER" -d "$db" --no-owner --no-privileges < "$dump"
      done
      ;;
    minio)
      log "minio: mirroring buckets back from $SRC/minio (--overwrite --remove)"
      for dir in "$SRC"/minio/*/; do
        have "$dir" || continue
        bucket="$(basename "$dir")"
        if [ -n "${BACKUP_DOCKER_VOLUME:-}" ]; then
          IN="/backup/${REL}/minio/$bucket"
        else
          IN="/backup/minio/$bucket"
        fi
        # --remove: the live bucket ends up EXACTLY matching the snapshot —
        # objects written after the backup are deleted, so a partial mirror
        # can never leave a silently mixed state.
        docker run --rm --network "$COMPOSE_NETWORK" \
          -v "$MOUNT_SRC:/backup" \
          --entrypoint /bin/sh \
          "$MC_IMAGE" -c "
            mc alias set local '$MINIO_ALIAS_URL' '$MINIO_ROOT_USER' '$MINIO_ROOT_PASSWORD' >/dev/null &&
            mc mb --ignore-existing 'local/$bucket' &&
            mc mirror --overwrite --remove '$IN' 'local/$bucket'
          "
        log "  $bucket: ok"
      done
      ;;
    tigerbeetle)
      if have "$SRC/tigerbeetle/0_0.tigerbeetle"; then
        log "tigerbeetle: restoring data file (container restart)"
        docker stop "$TB_CONTAINER" >/dev/null
        docker cp "$SRC/tigerbeetle/0_0.tigerbeetle" "$TB_CONTAINER:$TB_DATA_FILE"
        docker start "$TB_CONTAINER" >/dev/null
        log "  tigerbeetle: ok"
      else
        log "tigerbeetle: no data file in snapshot, skipping"
      fi
      ;;
    *)
      echo "unknown system: $system" >&2
      exit 2
      ;;
  esac
done
log "restore complete from $SRC"
