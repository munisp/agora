# Runbook — Operations

Day-2 procedures for the Agora data plane: DLQ replay, outbox monitoring,
Temporal workflow surgery, ledger reconciliation, lakehouse backfill, backups.
All commands assume the dev compose stack is up (`make up`) and run from the
repo root.

## 1. DLQ replay (`opendesk.dlq`)

Consumers dead-letter poison messages to the `opendesk.dlq` Kafka topic after
bounded retries, with error metadata attached (see booking-service README
/`DLQ_TOPIC`). Replay = inspect, fix the root cause, republish to the
**original** topic, then verify.

```bash
# 1. Inspect what's in the DLQ (key + value + headers)
docker exec kafka /opt/bitnami/kafka/bin/kafka-console-consumer.sh \
  --bootstrap-server localhost:9092 --topic opendesk.dlq \
  --from-beginning --property print.key=true --property print.headers=true \
  --timeout-ms 5000

# 2. Check lag to see how much is parked
docker exec kafka /opt/bitnami/kafka/bin/kafka-consumer-groups.sh \
  --bootstrap-server localhost:9092 --describe --all-groups | grep dlq
```

Each message carries the CloudEvents envelope (`type`, `source`, `tenantid`)
plus error metadata — the `source`/`type` tells you the original topic
(e.g. `com.opendesk.booking.*` → `opendesk.booking.events`).

```bash
# 3. Fix the root cause first (bad payload shape, missing tenant row, ...).

# 4. Replay one message to its original topic:
docker exec -i kafka /opt/bitnami/kafka/bin/kafka-console-producer.sh \
  --bootstrap-server localhost:9092 --topic opendesk.booking.events <<'EOF'
{"specversion":"1.0","id":"...","source":"booking-service","type":"com.opendesk.booking.BookingCreated","subject":"acme","time":"...","tenantid":"...","data":{...}}
EOF

# 5. Verify the consumer processed it (no new DLQ entry, workflow/event visible).
```

Bulk replay pattern (consume to file, then produce file line-by-line with the
same two commands; drop or edit invalid lines first). Never replay into
`opendesk.dlq` itself. If the root cause is not fixed, the replayed message
lands back in the DLQ after the consumer's retry budget — that is expected
behavior, not a replay bug.

## 2. Outbox monitoring

booking-service writes domain events to the `outbox` table in the `booking`
DB and a dispatcher drains them to `opendesk.booking.events` (sets
`sent_at`). A growing backlog means the dispatcher or Kafka is down.

```bash
# Backlog size (should trend to 0):
docker exec postgres psql -U opendesk -d booking -c \
  "SELECT count(*) AS unsent FROM outbox WHERE sent_at IS NULL;"

# Oldest stuck events (alert if older than a few minutes):
docker exec postgres psql -U opendesk -d booking -c \
  "SELECT id, aggregate_id, topic, payload->>'type' AS type
   FROM outbox WHERE sent_at IS NULL ORDER BY id LIMIT 20;"

# Drain rate sanity (last hour):
docker exec postgres psql -U opendesk -d booking -c \
  "SELECT date_trunc('minute', sent_at) AS m, count(*) FROM outbox
   WHERE sent_at > now() - interval '1 hour' GROUP BY 1 ORDER BY 1 DESC LIMIT 10;"
```

Remediation: `docker compose logs booking` (dispatcher errors), confirm Kafka
health (`make topics` also re-verifies the broker), then restart booking —
the dispatcher retries unsent rows automatically; no manual resend needed.

## 3. Temporal workflow inspection & retry

Namespace `opendesk`, task queue `opendesk-main`. UI: **http://localhost:8233**
(namespace selector top-left).

Workflows (SPEC §6): `BookingSagaWorkflow` (compensations ReleaseSlot /
VoidHold), `ReminderWorkflow` (T-24h/T-1h timers), `NoShowFollowupWorkflow`,
`TenantOnboardingWorkflow`.

In the UI:

1. **Workflows → filter** by Workflow Type or by the booking id / tenant slug
   in the workflow id.
2. Open the execution: the **History** tab shows each activity
   (ReserveSlot → HoldDeposit → ConfirmBooking → SendConfirmation) and, on
   failure, the compensation chain in reverse order.
3. **Retry paths:**
   - *Running but stuck activity* — check the pending activity's last error;
     fix the downstream service (activities hit booking/payments over Dapr);
     Temporal retries activities automatically per the retry policy.
   - *Failed execution* — use **Reset** (actions menu) to the last good
     event, or **Terminate** and re-drive the business operation through the
     API (booking mutations are idempotent via `idempotency_key`).
   - `ReminderWorkflow` is signal-driven — cancelling a booking signals the
     workflow; a terminated reminder can safely be restarted, timers are
     derived from the booking.

CLI equivalent (tctl ships in the auto-setup image):

```bash
docker exec temporal tctl --namespace opendesk workflow list
docker exec temporal tctl --namespace opendesk workflow describe -w <workflow-id>
docker exec temporal tctl --namespace opendesk workflow reset \
  -w <workflow-id> -r <run-id> --reset_type LastDecisionCompleted --reason "ops retry"
```

If the worker (notification-worker, :7003) is down, workflows queue up but do
not fail — they resume when the worker returns. Check
`docker compose logs notification`.

## 4. Ledger reconciliation (sim vs tigerbeetle)

payments-service abstracts the ledger behind `LedgerClient` with two
implementations selected by `LEDGER_IMPL` (ADR-0007):

* `sim` (dev default): embedded in-process ledger. **State is lost on
  restart** — after any payments-service restart in sim mode, balances reset
  and prior deposit ids 404. Reconciliation = re-drive from the source of
  truth (bookings) or accept the loss in dev.
* `tigerbeetle`: state lives in the `tigerbeetle-data` volume
  (`/data/0_0.tigerbeetle`, cluster 0 replica 0 on :3000) and survives
  restarts.

Routine checks:

```bash
# Which impl is live:
docker compose exec apisix curl -sf http://payments:7004/healthz | jq .ledger_impl  # service ports not host-published (W34 GF4)

# Tenant positions (posted vs pending, per account):
docker compose exec apisix curl -sf -H 'X-Tenant-Slug: acme' http://payments:7004/v1/accounts/acme/balance | jq .

# Deposits still held (should match bookings in status confirmed/pending
# with a deposit): compare accounts[*].pending_net against
docker exec postgres psql -U opendesk -d booking -c \
  "SELECT status, count(*) FROM bookings GROUP BY 1;"
```

**CRITICAL log line** — `mojaloop transfer committed but ledger payout
failed` (payout rail committed, ledger leg failed). Procedure:

1. Capture the `payout_id` and `error` from the payments logs.
2. Verify rail state: query the Mojaloop simulator for the transfer
   (`GET http://localhost:8444/transfers/{id}`) — it IS committed.
3. Retry the ledger leg by re-POSTing `/v1/payouts` with the **same
   `idempotency_key`** — the deterministic payout id makes the rail call
   idempotent, and the ledger transfer either posts or returns the existing
   one. Never invent a new idempotency key for the same payout.
4. If the ledger leg keeps failing (e.g. sim restart wiped state), record a
   manual adjusting entry by re-running the payout after state recovery and
   document it in the incident log.

## 5. Lakehouse backfill

Flow: analytics-pipeline sinks Kafka topics → Iceberg `bronze` (MinIO bucket
`lake`); Spark jobs clean to `silver`; dbt builds `gold` marts. Both layers
are idempotent (Spark uses dynamic partition overwrite; dbt models are
rebuildable), so backfill = re-run downstream jobs; no dedup bookkeeping.

```bash
# 1. Re-run silver cleaning jobs (sources mounted at /opt/spark-jobs):
docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
  --master spark://spark-master:7077 \
  --packages org.apache.iceberg:iceberg-spark-runtime-3.5_2.12:1.6.1,org.apache.iceberg:iceberg-aws-bundle:1.6.1 \
  /opt/spark-jobs/silver_clean_bookings.py

docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
  --master spark://spark-master:7077 \
  --packages org.apache.iceberg:iceberg-spark-runtime-3.5_2.12:1.6.1,org.apache.iceberg:iceberg-aws-bundle:1.6.1 \
  /opt/spark-jobs/silver_clean_transcripts.py

# 2. Rebuild gold marts (choose one):
make dbt                       # containerized dbt-trino build
# or from a checkout:
cd infra/lakehouse/dbt && export DBT_PROFILES_DIR=$PWD && dbt deps && dbt build

# 3. Verify in Trino:
make trino
#   trino> SELECT * FROM iceberg.gold.daily_bookings_per_tenant LIMIT 10;
```

If bronze itself is missing data (analytics-pipeline down), fix the pipeline
first — Kafka retention determines how far back a bronze backfill can reach;
there is no other raw copy.

## 6. Backups

**Postgres** (all service DBs — identity, booking, conversation, knowledge,
analytics_meta, temporal, keycloak, permify, iceberg):

```bash
# Full cluster dump:
docker exec postgres pg_dumpall -U opendesk > backup-$(date +%F).sql
# Per-DB (faster restore):
docker exec postgres pg_dump -U opendesk -Fc booking > booking-$(date +%F).dump
# Restore:
docker exec -i postgres psql -U opendesk -d booking < backup.sql
```

Notes: RLS uses `current_setting('app.tenant_id', true)` — dumps restore
cleanly because policies reference the GUC, not literal tenant ids. The
`iceberg` DB is the Iceberg REST catalog — losing it orphans the lake tables
even if Parquet files survive; back it up **together with** MinIO.

**MinIO** (bucket `lake` — all Iceberg data files):

```bash
# Filesystem-level copy of the volume (dev):
docker run --rm -v opendesk_minio-data:/data -v $PWD/backups:/backup \
  alpine tar czf /backup/minio-lake-$(date +%F).tgz -C /data lake
# Or use `mc mirror` from a client container for object-level replication.
```

Restore order for a full rebuild: Postgres first (incl. `iceberg` catalog
DB), then MinIO data files, then `make up`, `make topics`, and a lakehouse
backfill (§5) to rebuild silver/gold. Keycloak/Permify state can be
reprovisioned from `infra/keycloak/realm-opendesk.json` and
`infra/permify/schema.perm` + tenant onboarding workflows, but backing up
their Postgres DBs is cheaper. TigerBeetle state (prod `LEDGER_IMPL`) is the
`tigerbeetle-data` volume — copy `/data/0_0.tigerbeetle` while the replica
is stopped.

## 7. CRM sync operations (crm-sync ⇄ Twenty)

crm-sync (port 7010, Dapr app-id `crm-sync`) consumes
`opendesk.identity.events` / `opendesk.booking.events` /
`opendesk.conversation.events` and upserts Companies, People, Tasks and Notes
into Twenty. State lives in the `crm_sync` Postgres DB (`sync_map` table);
poison messages land in `opendesk.dlq` after 3 attempts.

### sync_map inspection

```bash
# Recent mappings (kind = tenant | contact | booking):
docker exec postgres psql -U opendesk -d crm_sync -c \
  "SELECT kind, opendesk_id, twenty_id, tenant_id, updated_at
   FROM sync_map ORDER BY updated_at DESC LIMIT 20;"

# Per-kind counts — compare tenants against identity:
docker exec postgres psql -U opendesk -d crm_sync -c \
  "SELECT kind, count(*) FROM sync_map GROUP BY 1;"
docker exec postgres psql -U opendesk -d identity -c "SELECT count(*) FROM tenants;"

# Find the Twenty object for an OpenDesk booking (or vice versa):
docker exec postgres psql -U opendesk -d crm_sync -c \
  "SELECT * FROM sync_map WHERE kind='booking' AND opendesk_id='<booking-uuid>';"

# Consumers healthy? (lag should trend to 0):
docker exec kafka /opt/bitnami/kafka/bin/kafka-consumer-groups.sh \
  --bootstrap-server localhost:9092 --describe --group crm-sync
```

Metrics: `:7010/metrics` exposes per-event counters and Twenty call latency —
a rising `crm_sync_twenty_call_duration_seconds` with flat event counters means
Twenty (or the rate limiter) is the bottleneck, not Kafka.

### DLQ replay for crm-sync

Same procedure as §1 — crm-sync dead-letters to the shared `opendesk.dlq` with
the original CloudEvent intact. Filter DLQ messages to this consumer by
`source` (identity/booking/conversation events) and the error metadata.

```bash
# 1. Inspect DLQ entries originating from crm-sync consumption:
docker exec kafka /opt/bitnami/kafka/bin/kafka-console-consumer.sh \
  --bootstrap-server localhost:9092 --topic opendesk.dlq \
  --from-beginning --property print.headers=true --timeout-ms 5000

# 2. Fix the root cause. Common ones for crm-sync:
#    - 401 from Twenty        → bad/expired TWENTY_API_KEY (rekey below)
#    - 404 from Twenty        → Company/Person deleted in Twenty; delete the
#                               stale sync_map row so the replay re-creates it
#    - ordering (booking before tenant) → nothing to fix, replay is enough

# 3. Replay to the ORIGINAL topic (e.g. opendesk.booking.events), never to the DLQ:
docker exec -i kafka /opt/bitnami/kafka/bin/kafka-console-producer.sh \
  --bootstrap-server localhost:9092 --topic opendesk.booking.events <<'EOF'
{"specversion":"1.0","id":"...","source":"booking-service","type":"com.opendesk.booking.BookingCreated","subject":"acme","time":"...","tenantid":"...","data":{...}}
EOF

# 4. Verify: new/updated sync_map row, object visible in Twenty, no new DLQ entry.
```

Upserts are idempotent via `UNIQUE(kind, opendesk_id, tenant_id)` — replaying
an already-processed event is a no-op, so when in doubt, replay.

### Rekey procedure (rotating `TWENTY_API_KEY`)

1. In Twenty (http://localhost:3100): **Settings → API & Webhooks → create
   API key**. Do **not** revoke the old key yet.
2. Update `TWENTY_API_KEY` on the `crm-sync` service (compose env or `.env`).
3. `docker compose up -d crm-sync` — in-flight retries recover automatically;
   events that exhausted their attempts during the outage are in the DLQ.
4. Confirm a sync succeeds (create a test booking or watch `/metrics`), then
   revoke the old key in Twenty.
5. Replay any DLQ entries that failed with `401` during the window (above).

The same flow applies to `TWENTY_WEBHOOK_SECRET` (reverse intake): update both
Twenty's webhook config and the crm-sync env, then restart crm-sync. Events
received with the old signature return `401` and are dropped by the sender —
they are not recoverable, so rotate during low traffic.

### Twenty upgrade / migration notes

* Image is pinned (`twentycrm/twenty:v1.3.2`) for both `twenty-api` and
  `twenty-worker` — always upgrade the two together; a version skew between
  API and worker breaks the Bull-MQ job protocol.
* Upgrade sequence:

  ```bash
  docker compose pull twenty-api twenty-worker
  docker compose up -d twenty-worker   # worker runs DB migrations on boot
  docker compose logs -f twenty-worker # wait for migrations to finish
  docker compose up -d twenty-api
  docker compose up -d crm-sync        # pick up any API shape changes
  ```

* Twenty's data lives in the shared `postgres` container (database `twenty`) —
  include it in the §6 backup (`pg_dump -d twenty`); `STORAGE_TYPE=local`
  attachments live on the twenty-api container filesystem, so back up that
  volume before upgrades if files are used.
* After an upgrade, smoke the bridge: create a booking in the seed tenant and
  confirm the Person + Task appear in Twenty; check crm-sync logs for `404` /
  schema errors — REST field names (e.g. `emails.primaryEmail`) occasionally
  change between Twenty minors, which would surface as DLQ entries on
  `findPerson`.
* Downgrades are not supported by Twenty's migrations; snapshot the `twenty`
  DB before upgrading if rollback matters.

## 7. Backup automation (infra/backups)

`infra/backups/backup.sh` captures a timestamped snapshot of all stateful
systems and keeps the newest 7 snapshots (restic-style keep-last,
`BACKUP_KEEP` to change):

* **Postgres** — `pg_dump -Fc` per database (identity, booking, conversation,
  knowledge, analytics_meta, temporal, keycloak, permify, iceberg, twenty,
  crm_sync, opendesk) from the `postgres` container. This is the automated
  form of the §6 per-DB pattern; it replaces ad-hoc `pg_dumpall` for routine
  snapshots.
* **MinIO** — `mc mirror` of the `lake` and `exports` buckets via an
  ephemeral `minio/mc` container on the `opendesk` network. The `iceberg`
  catalog DB is dumped in the same run, keeping the lakehouse consistent.
* **TigerBeetle** — data-file copy (`/data/0_0.tigerbeetle`); the container
  is **paused** during the copy so the snapshot is consistent (`TB_PAUSE=0`
  to skip). Ledger writes stall for the duration of the copy.

```bash
# Manual snapshot (host, stack up) -> ./backups/<UTC timestamp>/
./infra/backups/backup.sh

# Scheduled: the observability compose runs the same script daily at 03:17
# via an ofelia sidecar (`backup` + `ofelia` services in
# infra/docker-compose.observability.yml), writing to the `backups` named
# volume. Edit the ofelia.job-exec.backup.schedule label to reschedule.
docker logs opendesk-ofelia        # scheduler activity
docker logs opendesk-backup        # last run output (job-exec logs)

# Verify a snapshot:
ls -1 backups/                     # or: docker run --rm -v opendesk_backups:/b alpine ls -1 /b
cat backups/<ts>/manifest.txt

# Restore (DESTRUCTIVE — requires RESTORE_CONFIRM=yes):
RESTORE_CONFIRM=yes ./infra/backups/restore.sh backups/<ts>
RESTORE_CONFIRM=yes SYSTEMS=postgres ./infra/backups/restore.sh backups/<ts>   # subset
```

Restore drops/recreates each dumped Postgres DB and `pg_restore`s it,
mirrors the MinIO buckets back over the live ones, and stops TigerBeetle to
swap the data file back. There is **no point-in-time recovery** — you get
the snapshot as-is. After a Postgres restore of `booking`/`conversation`/
`knowledge`, re-verify RLS roles (§Agent B runbook) still match if you
restored with `--no-owner`.

Limitations (dev topology): per-DB dumps, not WAL archiving — production HA
(Patroni, ADR-0008) should use WAL-G/pgBackRest to object storage; on a
3-node TigerBeetle cluster back up a replica instead of pausing the leader;
ship snapshots off-box for anything beyond dev.
