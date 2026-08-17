#!/bin/sh
# OpenDesk restore — companion to backup.sh.
#
# Restores a timestamped snapshot produced by infra/backups/backup.sh:
#   1. Postgres  — drop + recreate each dumped DB, then pg_restore
#   2. MinIO     — mc mirror the backed-up buckets back over the live ones
#   3. TigerBeetle — stop container, copy the data file back, start
#
# DESTRUCTIVE: overwrites live state. Requires RESTORE_CONFIRM=yes.
#
# Usage:
#   RESTORE_CONFIRM=yes ./infra/backups/restore.sh ./backups/20250101T031700Z
#   # or only some systems:
#   RESTORE_CONFIRM=yes SYSTEMS=postgres ./infra/backups/restore.sh <dir>
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

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*"; }
have() { [ -e "$1" ]; }

# Absolute path / volume source for the mirror container mount.
if [ -n "${BACKUP_DOCKER_VOLUME:-}" ]; then
  # Sidecar mode: pass an absolute path inside the volume, e.g. /backups/<ts>.
  MOUNT_SRC="$BACKUP_DOCKER_VOLUME"
  REL="${SRC#/backups/}"
else
  MOUNT_SRC="$(cd "$SRC" && pwd)"
fi

for system in $SYSTEMS; do
  case "$system" in
    postgres)
      log "postgres: restoring dumps from $SRC/postgres"
      for dump in "$SRC"/postgres/*.dump; do
        have "$dump" || continue
        db="$(basename "$dump" .dump)"
        log "  $db: drop + recreate + pg_restore"
        docker exec "$POSTGRES_CONTAINER" dropdb -U "$PG_USER" --if-exists "$db"
        docker exec "$POSTGRES_CONTAINER" createdb -U "$PG_USER" "$db"
        docker exec -i "$POSTGRES_CONTAINER" pg_restore -U "$PG_USER" -d "$db" --no-owner --no-privileges < "$dump"
      done
      ;;
    minio)
      log "minio: mirroring buckets back from $SRC/minio"
      for dir in "$SRC"/minio/*/; do
        have "$dir" || continue
        bucket="$(basename "$dir")"
        if [ -n "${BACKUP_DOCKER_VOLUME:-}" ]; then
          IN="/backup/${REL}/minio/$bucket"
        else
          IN="/backup/minio/$bucket"
        fi
        docker run --rm --network "$COMPOSE_NETWORK" \
          -v "$MOUNT_SRC:/backup" \
          --entrypoint /bin/sh \
          "$MC_IMAGE" -c "
            mc alias set local '$MINIO_ALIAS_URL' '$MINIO_ROOT_USER' '$MINIO_ROOT_PASSWORD' >/dev/null &&
            mc mb --ignore-existing 'local/$bucket' &&
            mc mirror --overwrite '$IN' 'local/$bucket'
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
