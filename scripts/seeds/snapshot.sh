#!/usr/bin/env bash
# snapshot.sh — tar --zstd snapshot of the lakehouse seed layers (SPEC-W17,
# Agent C; §8.5 step 9 of the bootstrap sequence, run by scripts/seeds/bootstrap.sh).
#
# Packages the lakehouse bronze/silver data dirs into seed-snapshot-v3.tar.zst
# and writes a manifest.txt of per-table file/row counts next to it.
#
# Env:
#   LAKEHOUSE_ROOT     base dir holding the bronze/silver layer dirs
#                      (default /var/lib/opendesk/lakehouse — a local mirror
#                      or mount of the MinIO `lake` bucket warehouse; the
#                      lakehouse itself keeps data in s3://lake/warehouse, see
#                      infra/docker-compose.lakehouse.yml)
#   SNAPSHOT_SRC_DIRS  space-separated layer dirs to include
#                      (default "$LAKEHOUSE_ROOT/bronze $LAKEHOUSE_ROOT/silver")
#   SNAPSHOT_OUT       output tarball path (default ./seed-snapshot-v3.tar.zst)
#
# Flags:
#   --dry-run   print the plan (dirs, sizes, output path) and write nothing.
#
# Rowcounts: when python3+pyarrow is available, parquet footer row counts are
# summed per table dir; otherwise the manifest records file counts + bytes and
# marks rowcounts unavailable (snapshot still succeeds — report IO must never
# fail the seed, contract A).

set -euo pipefail

DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    -h|--help)
      sed -n '2,30p' "$0"
      exit 0
      ;;
    *) echo "[snapshot] unknown flag: $arg" >&2; exit 2 ;;
  esac
done

LAKEHOUSE_ROOT="${LAKEHOUSE_ROOT:-/var/lib/opendesk/lakehouse}"
SNAPSHOT_SRC_DIRS="${SNAPSHOT_SRC_DIRS:-$LAKEHOUSE_ROOT/bronze $LAKEHOUSE_ROOT/silver}"
SNAPSHOT_OUT="${SNAPSHOT_OUT:-./seed-snapshot-v3.tar.zst}"
MANIFEST_OUT="${SNAPSHOT_OUT%.tar.zst}-manifest.txt"

if [ "$DRY_RUN" -eq 0 ] && ! command -v zstd >/dev/null 2>&1; then
  echo "[snapshot] ERROR: zstd not found (required for tar --zstd)." >&2
  echo "[snapshot] Install zstd or point SNAPSHOT_OUT handling at a host that has it." >&2
  exit 1
fi

echo "[snapshot] lakehouse root : $LAKEHOUSE_ROOT"
echo "[snapshot] source dirs    : $SNAPSHOT_SRC_DIRS"
echo "[snapshot] output tarball : $SNAPSHOT_OUT"
echo "[snapshot] manifest       : $MANIFEST_OUT"

present_dirs=()
for d in $SNAPSHOT_SRC_DIRS; do
  if [ -d "$d" ]; then
    present_dirs+=("$d")
  else
    echo "[snapshot] WARN: missing layer dir (skipped): $d"
  fi
done

if [ "${#present_dirs[@]}" -eq 0 ]; then
  echo "[snapshot] ERROR: no source dirs exist — nothing to snapshot." >&2
  exit 1
fi

if [ "$DRY_RUN" -eq 1 ]; then
  for d in "${present_dirs[@]}"; do
    echo "[snapshot] DRY-RUN would include: $d ($(du -sh "$d" 2>/dev/null | cut -f1))"
  done
  echo "[snapshot] DRY-RUN: no tarball or manifest written."
  exit 0
fi

# ---------------------------------------------------------------------------
# manifest.txt — per-table-dir parquet rowcounts (pyarrow) or file counts.
# ---------------------------------------------------------------------------
{
  echo "# seed snapshot manifest — $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# source dirs: ${present_dirs[*]}"
  echo "# layer/table files rows bytes"
  for d in "${present_dirs[@]}"; do
    layer="$(basename "$d")"
    find "$d" -mindepth 1 -maxdepth 1 -type d | sort | while read -r table_dir; do
      table="$(basename "$table_dir")"
      stats="$(python3 - "$table_dir" <<'PY' 2>/dev/null || true
import os, sys
root = sys.argv[1]
files = rows = nbytes = 0
try:
    import pyarrow.parquet as pq
except ImportError:
    pq = None
for dirpath, _dirs, names in os.walk(root):
    for name in names:
        path = os.path.join(dirpath, name)
        files += 1
        nbytes += os.path.getsize(path)
        if pq is not None and name.endswith(".parquet"):
            try:
                rows += pq.read_metadata(path).num_rows
            except Exception:
                pass
print(files, rows if pq is not None else -1, nbytes)
PY
)"
      files="$(echo "$stats" | awk '{print $1}')"
      rows="$(echo "$stats" | awk '{print $2}')"
      nbytes="$(echo "$stats" | awk '{print $3}')"
      if [ "$rows" = "-1" ] || [ -z "$rows" ]; then
        rows="unavailable(pyarrow-missing)"
      fi
      echo "$layer/$table files=${files:-0} rows=$rows bytes=${nbytes:-0}"
    done
  done
} > "$MANIFEST_OUT"
echo "[snapshot] wrote manifest: $MANIFEST_OUT"

# ---------------------------------------------------------------------------
# tarball — absolute paths stripped so the archive extracts relative to CWD.
# ---------------------------------------------------------------------------
tar -c --zstd -f "$SNAPSHOT_OUT" \
  $(for d in "${present_dirs[@]}"; do printf -- '-C %s %s ' "$(dirname "$d")" "$(basename "$d")"; done)
echo "[snapshot] wrote tarball: $SNAPSHOT_OUT ($(du -sh "$SNAPSHOT_OUT" | cut -f1))"
echo "[snapshot] OK"
