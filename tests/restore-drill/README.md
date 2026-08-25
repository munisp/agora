# tests/restore-drill — backup/restore + migration drill vs REAL Postgres

Verifies, end to end and with **no mocks**, that a full `pg_dump -Fc` /
`pg_restore --no-owner --no-privileges` cycle (the exact semantics of
`infra/backups/backup.sh` / `infra/backups/restore.sh`) preserves:

* every application-database row (per-table counts identical),
* the RLS posture (policies + `FORCE ROW LEVEL SECURITY` on
  `identity.tenants`, `booking.bookings`, `billing.invoices`,
  `conversation.capture_records`),
* tenant isolation enforcement post-restore (least-privilege
  `app_billing_login` still denied with a wrong/empty/unset
  `app.tenant_id` GUC — W40-6 NULLIF fail-closed posture),
* **(DRILL_PG=system only)** the postgis init script
  `06-booking-postgis.sql` applying verbatim on instance A and the postgis
  extension surviving the dump/restore onto instance B.

## Cluster backends (env `DRILL_PG`)

### `DRILL_PG=pgserver` (default)

Both clusters are REAL PostgreSQL via
[pgserver](https://pypi.org/project/pgserver/) (embedded full PostgreSQL
binaries — the same engine the production docker image runs), not a
sqlite/mini compatibility layer. pgserver ships **core PostgreSQL without
contrib modules**, so in this mode `06-booking-postgis.sql` is SKIPPED with
an explicit recorded reason (postgis cannot load; the booking store
tolerates its absence — geo is additive), and the `pgcrypto` extension
lines are stripped (`gen_random_uuid()` is core PG13+ and is all those
schemas use).

### `DRILL_PG=system` (SPEC-W42)

Uses a locally installed **system PostgreSQL** (binaries from
`DRILL_PG_BIN`, else the highest `/usr/lib/postgresql/<ver>/bin`, else
PATH) instead of pgserver: `initdb` + `pg_ctl` run one real cluster per
instance on workdir-local unix sockets (`listen_addresses=''` — no TCP
ports at all). When the **postgis** packages are installed (`apt install
postgis` / `postgresql-<ver>-postgis-3`), the drill additionally:

1. applies `06-booking-postgis.sql` **verbatim** on instance A (in docker
   init order — after 05, before 07);
2. lets the extension ride the `pg_dump`/`pg_restore` cycle like any other
   schema object;
3. asserts `pg_extension` shows postgis live on the booking DB of instance
   B post-restore.

When `postgresql-contrib` provides pgcrypto (usual with apt installs), ALL
init scripts apply fully verbatim — no stripping.

**SKIP discipline (never a silent pass):** if postgis is absent from the
system cluster's `pg_available_extensions`, the drill prints an explicit
`SKIP  06-booking-postgis.sql — <precise reason + apt remediation>` line
and records it under `skips` in `drill-summary.json`, while the rest of the
drill still runs on the system cluster. If no system PostgreSQL is found at
all, the whole system mode is a single explicit SKIP (the EXTERNAL_BLOCKED
evidence trail) and the drill exits 0 without claiming a pass.

**Root note:** PostgreSQL refuses to run as root; when the drill runs as
root, `initdb`/`pg_ctl`/`postgres` are demoted to the `postgres` (else
`nobody`) account via `setpriv` (or `su`), with the pgdata/socket dirs
chowned accordingly. `psql`/`pg_dump`/`pg_restore` run as the invoking user
over the unix socket.

## What the drill does

Instance A (fresh cluster):
1. applies `infra/postgres/init-scripts/00,01,02,03,04,05,07,30` via real
   `psql` over stdin (`\c` meta-commands handled exactly like the docker
   `docker-entrypoint-initdb.d` psql path) — plus **`06` in system mode
   when postgis is available** (see above), plus **`08-code-bootstrap-parity.sql`
   (SPEC-W43, CODER-G) inserted after `07` when the file exists** — when it
   does not, the drill records an explicit SKIP (`record_skip`) and
   continues; it never crashes on the missing file;
2. in pgserver mode, strips ONLY the `CREATE EXTENSION IF NOT EXISTS
   pgcrypto;` lines (pgserver lacks contrib pgcrypto; `gen_random_uuid()`
   is core PG13+ and is all those schemas use);
3. applies billing migrations `0001..0004` to the `billing` DB exactly as
   billing-engine does at boot (`src/main.rs:142-159`), plus
   **`0005_hardening.sql` (SPEC-W43 B-07, CODER-B) when the file exists** —
   same conditional-SKIP idiom as the init scripts (explicit recorded SKIP,
   never a crash, when B's migration has not landed in the mirror yet);
4. seeds marker rows: `identity.tenants`, `booking.offerings`,
   `conversation.conversations` (+ agents/capture chain), `billing`
   (`processed_events`/`usage_records`/`invoices`), `platform.model_family`;
5. `pg_dump -Fc` each application DB (identity, booking, conversation,
   knowledge, billing, platform).

Instance B (FRESH cluster):
6. `createdb` per app DB; applies the role/grant layer (05-app-roles +
   `app_billing_internal` + `app_model_registry*` role blocks) — cluster
   roles are NOT carried by per-database dumps, same as production
   `restore.sh`, which assumes the target cluster ran the init scripts;
7. `pg_restore --no-owner --no-privileges` per dump (restore.sh semantics);
8. asserts all checks listed above; exit 0 only if every check passes
   (explicit SKIPs are reported separately, never counted as passes).

### Cluster adaptations (none weaken the drill)

* `pgcrypto` extension lines stripped **only when the backend lacks
  contrib** (always in pgserver mode; probed per-cluster in system mode).
* `05-app-roles.sql` references the docker bootstrap superuser `opendesk` in
  `ALTER DEFAULT PRIVILEGES ... FOR ROLE opendesk`; both backends bootstrap
  as `postgres`. The drill creates a NOLOGIN `opendesk` role so the script
  applies verbatim, and bridges the same default privileges FOR ROLE
  `postgres` (the generalized idiom billing's own `0002_rls.sql` uses).
  Without the bridge, `pg_restore --no-privileges` (tables created by
  `postgres`) would strip app-role grants — in production restore.sh
  restores as `opendesk`, so the FOR ROLE opendesk defaults cover it.

## How to run

```bash
# pgserver mode (default)
pip install pgserver==0.1.4 psycopg
export XDG_RUNTIME_DIR=/tmp/xdg && mkdir -p $XDG_RUNTIME_DIR && chmod 700 $XDG_RUNTIME_DIR
python3 tests/restore-drill/drill.py --workdir /tmp/restore-drill
echo $?   # 0 = all checks passed

# system-PG mode (SPEC-W42): locally installed PostgreSQL (+ postgis for
# the 06 leg)
apt install postgresql postgis        # or postgresql-<ver>-postgis-3
DRILL_PG=system python3 tests/restore-drill/drill.py --workdir /tmp/restore-drill-sys
# binaries override: DRILL_PG_BIN=/usr/lib/postgresql/16/bin DRILL_PG=system ...
```

Expected duration: **~5 s** per mode on a 2-CPU sandbox (first-ever
pgserver run adds a one-time PostgreSQL binary download by pgserver, ~1-2
min depending on network). `--keep` preserves both pgdata dirs for
inspection; `<workdir>/drill-summary.json` is the machine-readable result
(including the `skips` array with precise SKIP reasons).

CI notes: pgserver mode works on a stock `ubuntu-latest` runner with
`pip install pgserver psycopg`; no docker, no services, no Kafka, no
network beyond the one-time pgserver binary fetch. System mode needs
`apt install postgresql postgis` on the runner (ubuntu images ship apt
postgresql + postgis packages).

## Results

See [RESULTS.md](RESULTS.md) — pgserver mode executed in-sandbox
2026-08-17, **29/29 checks PASS** (real output embedded there; W42 runs —
both modes — append their own dated sections).
