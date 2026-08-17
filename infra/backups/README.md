# infra/backups — snapshot & restore

Real, self-contained backups of the three stateful systems in the dev/compose
topology. Shell scripts, no new dependencies (uses the running containers plus
the official `minio/mc` image for the mirror step).

## What gets captured

| System | Method | Output |
| --- | --- | --- |
| Postgres (all service DBs: identity, booking, conversation, knowledge, analytics_meta, temporal, keycloak, permify, iceberg, twenty, crm_sync, opendesk) | `pg_dump -Fc` per DB via `docker exec postgres` | `<ts>/postgres/<db>.dump` |
| MinIO buckets `lake` + `exports` | `mc mirror` in an ephemeral `minio/mc` container on the `opendesk` network | `<ts>/minio/<bucket>/...` |
| TigerBeetle ledger | data-file copy; container is **paused** during the copy (`TB_PAUSE=0` to skip) | `<ts>/tigerbeetle/0_0.tigerbeetle` |

Each run creates `./backups/<UTC timestamp>/` with a `manifest.txt`, and keeps
only the newest **7** snapshots (`BACKUP_KEEP` to change) — restic-style
keep-last rotation. The `iceberg` catalog DB and the `lake` bucket are dumped
in the same run so the lakehouse stays consistent (see runbook §6 note).

## Run manually (host, dev stack up)

```bash
./infra/backups/backup.sh                  # writes ./backups/<ts>/
BACKUP_KEEP=14 ./infra/backups/backup.sh /var/backups/opendesk
```

## Scheduled (sidecar)

`infra/docker-compose.observability.yml` defines:

- **`backup`** — a `docker:27-cli` container with the docker socket, the repo
  mounted at `/repo:ro`, and the `backups` named volume at `/backups`;
- **`ofelia`** — cron scheduler that runs `backup.sh /backups` inside the
  `backup` container daily at 03:17 (`ofelia.job-exec.backup.schedule` label;
  edit the label to change the schedule, `no-overlap` is on).

In sidecar mode the docker socket is the host's, so the MinIO mirror step
mounts the **`agora_backups` named volume** (`BACKUP_DOCKER_VOLUME`)
instead of a host path. Inspect snapshots with:

```bash
docker run --rm -v agora_backups:/b alpine ls -1 /b
```

## Restore (DESTRUCTIVE)

```bash
RESTORE_CONFIRM=yes ./infra/backups/restore.sh ./backups/<ts>
RESTORE_CONFIRM=yes SYSTEMS=postgres ./infra/backups/restore.sh ./backups/<ts>   # subset
```

Restore drops and recreates each dumped Postgres DB, mirrors the MinIO
buckets back over the live ones, and stops TigerBeetle to swap its data file
back in. There is no point-in-time recovery — you get the snapshot as-is.

## Honest limitations (dev topology)

- Postgres dumps are per-DB and not WAL-based; for production HA (Patroni,
  ADR-0008) use WAL-G/pgBackRest against the object store instead.
- The TigerBeetle pause makes the copy consistent but briefly stalls ledger
  writes; on a 3-node TB cluster back up a replica instead.
- Snapshots are local directories; ship them off-box (rsync/rclone/restic)
  for anything beyond dev.
