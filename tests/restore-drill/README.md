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
  `app.tenant_id` GUC — W40-6 NULLIF fail-closed posture).

Both Postgres clusters are REAL PostgreSQL via
[pgserver](https://pypi.org/project/pgserver/) (embedded full PostgreSQL
binaries — the same engine the production docker image runs), not a
sqlite/mini compatibility layer.

## What the drill does

Instance A (fresh cluster):
1. applies `infra/postgres/init-scripts/00,01,02,03,04,05,07,30` via
   `server.psql` (real psql, `\c` meta-commands handled exactly like the
   docker `docker-entrypoint-initdb.d` psql path);
2. **skips `06-booking-postgis.sql`** — pgserver ships core PostgreSQL
   WITHOUT contrib modules, so postgis cannot load; the booking store
   tolerates its absence (geo is additive). Documented in `drill.py`;
3. strips ONLY the `CREATE EXTENSION IF NOT EXISTS pgcrypto;` lines
   (pgserver lacks contrib pgcrypto; `gen_random_uuid()` is core PG13+ and
   is all those schemas use);
4. applies billing migrations `0001..0004` to the `billing` DB exactly as
   billing-engine does at boot (`src/main.rs:142-159`);
5. seeds marker rows: `identity.tenants`, `booking.offerings`,
   `conversation.conversations` (+ agents/capture chain), `billing`
   (`processed_events`/`usage_records`/`invoices`), `platform.model_family`;
6. `pg_dump -Fc` each application DB (identity, booking, conversation,
   knowledge, billing, platform) using pgserver's bundled `pg_dump`.

Instance B (FRESH cluster):
7. `createdb` per app DB; applies the role/grant layer (05-app-roles +
   `app_billing_internal` + `app_model_registry*` role blocks) — cluster
   roles are NOT carried by per-database dumps, same as production
   `restore.sh`, which assumes the target cluster ran the init scripts;
8. `pg_restore --no-owner --no-privileges` per dump (restore.sh semantics);
9. asserts all checks listed above; exit 0 only if every check passes.

### pgserver adaptations (none weaken the drill)

* `pgcrypto` extension lines stripped (contrib unavailable; see 3 above).
* `05-app-roles.sql` references the docker bootstrap superuser `opendesk` in
  `ALTER DEFAULT PRIVILEGES ... FOR ROLE opendesk`; pgserver's bootstrap
  superuser is `postgres`. The drill creates a NOLOGIN `opendesk` role so
  the script applies verbatim, and bridges the same default privileges FOR
  ROLE `postgres` (the generalized idiom billing's own `0002_rls.sql` uses).
  Without the bridge, `pg_restore --no-privileges` (tables created by
  `postgres`) would strip app-role grants — in production restore.sh
  restores as `opendesk`, so the FOR ROLE opendesk defaults cover it.

## How to run

```bash
pip install pgserver==0.1.4 psycopg
export XDG_RUNTIME_DIR=/tmp/xdg && mkdir -p $XDG_RUNTIME_DIR && chmod 700 $XDG_RUNTIME_DIR
# from the repo root:
python3 tests/restore-drill/drill.py --workdir /tmp/restore-drill
echo $?   # 0 = all checks passed
```

Expected duration: **~5 s** on a 2-CPU sandbox (first-ever run adds a
one-time PostgreSQL binary download by pgserver, ~1-2 min depending on
network). `--keep` preserves both pgdata dirs for inspection;
`/tmp/restore-drill/drill-summary.json` is the machine-readable result.

CI notes: works on a stock `ubuntu-latest` runner with `pip install
pgserver psycopg`; no docker, no services, no Kafka, no network beyond the
one-time pgserver binary fetch.

## Results

See [RESULTS.md](RESULTS.md) — executed in-sandbox 2026-08-17, **29/29
checks PASS** (real output embedded there).
