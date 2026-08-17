# tests/restore-drill — RESULTS

Status: **EXECUTED** (2026-08-17, sandbox) — 29/29 checks PASS, exit code 0.
Verifier re-runs: overwrite the "Run 1" section or append a new dated run.

## Environment

| | |
|---|---|
| Host | sandbox, 2 CPU / 4 GB RAM |
| Python | 3.12.12 |
| pgserver | 0.1.4 (embedded PostgreSQL, core only — no contrib) |
| psycopg | 3.3.4 |
| Repo | opendesk mirror @ W41 base (remote `917644dabc2668da2d4319e7f1645d30e36caaa8`) |
| pg_dump/pg_restore | pgserver bundled (`pgserver/pginstall/bin/`) |

## Command

```bash
OPENDESK_REPO=<repo> XDG_RUNTIME_DIR=/tmp/xdg \
  python3 tests/restore-drill/drill.py --workdir /tmp/restore-drill
```

## Run 1 (2026-08-17, sandbox) — real output

Wall time: **4.68 s** (`drill-summary.json`). Exit code: **0**.

```
[drill] A: applied 00-create-dbs.sql
[drill] A: applied 01-booking-schema.sql
[drill] A: applied 02-identity-schema.sql
[drill] A: applied 03-conversation-schema.sql
[drill] A: applied 04-knowledge-schema.sql
[drill] A: applied 05-app-roles.sql
[drill] A: applied 07-agents-capture-schema.sql
[drill] A: applied 30-model-registry.sql
[drill] A: SKIPPED 06-booking-postgis.sql (pgserver has no postgis contrib)
[drill] A: applied billing migration 0001_init.sql
[drill] A: applied billing migration 0002_rls.sql
[drill] A: applied billing migration 0003_ledger.sql
[drill] A: applied billing migration 0004_outbox.sql
[drill] A: seeded markers {'tenant_id': '4b8f8cc8-ace1-4820-acb3-39610a69e67a',
  'offering_id': '196a8a84-5941-470a-a778-0b3bca38e7b1',
  'conversation_id': '611464aa-2ec6-456d-8875-384970001823',
  'capture_record_id': '66b3029c-6dff-40a1-9663-cbf110fb269d',
  'invoice_id': 'db624c05-2f32-4906-897f-21b7f743ab2c', 'model_family': 'drill-marker-family'}
[drill] dumped identity -> identity.dump (5548 bytes)
[drill] dumped booking -> booking.dump (18131 bytes)
[drill] dumped conversation -> conversation.dump (18303 bytes)
[drill] dumped knowledge -> knowledge.dump (6384 bytes)
[drill] dumped billing -> billing.dump (23833 bytes)
[drill] dumped platform -> platform.dump (25361 bytes)
[drill] booting FRESH pgserver instance B ...
[drill] B: role layer applied (05 + billing-internal + model-registry + postgres defaults bridge)
PASS  pg_restore identity (--no-owner --no-privileges)  — exit 0
PASS  pg_restore booking (--no-owner --no-privileges)  — exit 0
PASS  pg_restore conversation (--no-owner --no-privileges)  — exit 0
PASS  pg_restore knowledge (--no-owner --no-privileges)  — exit 0
PASS  pg_restore billing (--no-owner --no-privileges)  — exit 0
PASS  pg_restore platform (--no-owner --no-privileges)  — exit 0
PASS  row counts identical: identity
PASS  row counts identical: booking
PASS  row counts identical: conversation
PASS  row counts identical: knowledge
PASS  row counts identical: billing
PASS  row counts identical: platform
PASS  RLS policy present post-restore: identity.tenants  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: identity.tenants  — relforcerowsecurity=True
PASS  RLS policy present post-restore: booking.bookings  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: booking.bookings  — relforcerowsecurity=True
PASS  RLS policy present post-restore: billing.invoices  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: billing.invoices  — relforcerowsecurity=True
PASS  RLS policy present post-restore: conversation.capture_records  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: conversation.capture_records  — relforcerowsecurity=True
PASS  marker row readable: identity.tenants
PASS  marker row readable: booking.offerings
PASS  marker row readable: conversation.capture_records
PASS  marker row readable: billing.invoices
PASS  marker row readable: platform.model_family
PASS  RLS post-restore: wrong app.tenant_id sees 0 invoices
PASS  RLS post-restore: empty app.tenant_id sees 0 invoices (fail-closed)
PASS  RLS post-restore: GUC unset sees 0 invoices (fail-closed)
PASS  RLS post-restore: correct app.tenant_id sees its invoice
[drill] OK: 29/29 checks passed
```

## Notes / findings

* The drill surfaced one real operational requirement, now covered by the
  role-layer step: per-database `pg_dump`/`pg_restore` does **not** carry
  cluster-global roles. Restoring onto a cluster that has NOT run the init
  scripts leaves RLS policies referencing non-existent roles
  (`app_billing_internal` etc.) and every app-role query fails with
  `role "..." does not exist`. Production `restore.sh` inherits this
  assumption; the quarterly drill procedure in `docs/data-residency.md`
  should always restore onto a cluster bootstrapped with the init scripts
  (this drill does exactly that on instance B).
* 06-booking-postgis.sql is intentionally out of scope (contrib postgis
  unavailable under pgserver); geo restore fidelity remains
  EXTERNAL_BLOCKED in this environment.

---

## Run 2 (2026-08-16 UTC) — INDEPENDENT VERIFIER (V-Harness) reproduction — 29/29 PASS, exit 0

Independent re-execution of the committed `tests/restore-drill/drill.py`
(mirror copy, default repo-root resolution via `OPENDESK_REPO`), fresh
environment: pgserver 0.1.4 + psycopg 3.3.4 pip-installed this session,
XDG_RUNTIME_DIR=/tmp/xdg, workdir /tmp/restore-drill. Result CONFIRMS
Coder C's Run 1 claim.

Command:

```bash
OPENDESK_REPO=<repo-copy> XDG_RUNTIME_DIR=/tmp/xdg \
  python3 tests/restore-drill/drill.py --workdir /tmp/restore-drill
# exit code: 0
```

Wall time: **7.63 s** (`/tmp/restore-drill/drill-summary.json`,
29 checks). Real output:

```
[drill] A: applied 00-create-dbs.sql
[drill] A: applied 01-booking-schema.sql
[drill] A: applied 02-identity-schema.sql
[drill] A: applied 03-conversation-schema.sql
[drill] A: applied 04-knowledge-schema.sql
[drill] A: applied 05-app-roles.sql
[drill] A: applied 07-agents-capture-schema.sql
[drill] A: applied 30-model-registry.sql
[drill] A: SKIPPED 06-booking-postgis.sql (pgserver has no postgis contrib)
[drill] A: applied billing migration 0001_init.sql
[drill] A: applied billing migration 0002_rls.sql
[drill] A: applied billing migration 0003_ledger.sql
[drill] A: applied billing migration 0004_outbox.sql
[drill] A: seeded markers {'tenant_id': '4c4d721d-34ec-47f4-932b-14a00ecb1af6',
  'offering_id': '45c87eef-f107-4665-8c44-35caffda6220',
  'conversation_id': '9eb439ee-59e9-4f68-a387-f61e11b7a1a0',
  'capture_record_id': '4b2567a5-9b74-4565-be60-44847eecaa20',
  'invoice_id': 'f7884114-8b2c-478a-a28d-6ca4bfe42971', 'model_family': 'drill-marker-family'}
[drill] dumped identity -> identity.dump (5548 bytes)
[drill] dumped booking -> booking.dump (18130 bytes)
[drill] dumped conversation -> conversation.dump (18301 bytes)
[drill] dumped knowledge -> knowledge.dump (6384 bytes)
[drill] dumped billing -> billing.dump (23832 bytes)
[drill] dumped platform -> platform.dump (25364 bytes)
[drill] booting FRESH pgserver instance B ...
[drill] B: role layer applied (05 + billing-internal + model-registry + postgres defaults bridge)
PASS  pg_restore identity (--no-owner --no-privileges)  — exit 0
PASS  pg_restore booking (--no-owner --no-privileges)  — exit 0
PASS  pg_restore conversation (--no-owner --no-privileges)  — exit 0
PASS  pg_restore knowledge (--no-owner --no-privileges)  — exit 0
PASS  pg_restore billing (--no-owner --no-privileges)  — exit 0
PASS  pg_restore platform (--no-owner --no-privileges)  — exit 0
PASS  row counts identical: identity
PASS  row counts identical: booking
PASS  row counts identical: conversation
PASS  row counts identical: knowledge
PASS  row counts identical: billing
PASS  row counts identical: platform
PASS  RLS policy present post-restore: identity.tenants  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: identity.tenants  — relforcerowsecurity=True
PASS  RLS policy present post-restore: booking.bookings  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: booking.bookings  — relforcerowsecurity=True
PASS  RLS policy present post-restore: billing.invoices  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: billing.invoices  — relforcerowsecurity=True
PASS  RLS policy present post-restore: conversation.capture_records  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: conversation.capture_records  — relforcerowsecurity=True
PASS  marker row readable: identity.tenants
PASS  marker row readable: booking.offerings
PASS  marker row readable: conversation.capture_records
PASS  marker row readable: billing.invoices
PASS  marker row readable: platform.model_family
PASS  RLS post-restore: wrong app.tenant_id sees 0 invoices
PASS  RLS post-restore: empty app.tenant_id sees 0 invoices (fail-closed)
PASS  RLS post-restore: GUC unset sees 0 invoices (fail-closed)
PASS  RLS post-restore: correct app.tenant_id sees its invoice
[drill] summary written: /tmp/restore-drill/drill-summary.json
[drill] OK: 29/29 checks passed
```

Verifier notes:

* Dump sizes match Run 1 to within a few bytes (UUID/timestamp variance) —
  consistent with a real dump, not a canned log.
* The drill's two documented pgserver adaptations (pgcrypto strip, postgis
  skip) were observed live (NOTICE lines + the SKIP line above).
* The instance-B role-layer step is load-bearing: verified in code that
  per-database pg_dump does not carry cluster roles; drill bootstraps roles
  on B before pg_restore, mirroring production restore.sh's assumption.


---

## W42 — EXECUTED (2026-08-17, sandbox) — FRESH verifier (gate G5)

Status: **EXECUTED.** pgserver mode: 2 independent runs, both **29/29 PASS +
1 explicit recorded postgis SKIP, exit 0**. System mode: run against a REAL
PostgreSQL 18.6 + postgis 3.6.4 (conda-forge, no root) — **30/30 PASS
including the 06-booking-postgis.sql leg, 0 skips, exit 0 (2 runs)**.
Postgis availability verdict: **EXECUTED** — postgis was obtained without
root (micromamba/conda-forge) after the apt route was blocked; the
EXTERNAL_BLOCKED fallback evidence (verbatim SKIP on a postgis-less system
cluster) was additionally captured with pgserver's PG binaries.

### W42 environment

| | |
|---|---|
| Host | sandbox, 2 CPU / 4 GB RAM, uid=999 (kimi) — NO root |
| Python | 3.12.12 |
| pgserver | 0.1.4 (pip user site) |
| psycopg | 3.3.4 |
| pytest | 9.1.1 |
| system PG (W42) | conda-forge `postgresql` 18.6 + `postgis` 3.6.4 via micromamba 2.9.0 static binary (prefix /tmp/ce) |
| Repo | /tmp/ws — byte-identical copy of the opendesk mirror (`diff -r` clean), `OPENDESK_REPO=/tmp/ws` |
| Workdirs | /tmp/drill-pgserver{,-r2}, /tmp/drill-system-fg, /tmp/drill-system-postgis, /tmp/drill-sys-pg2, /tmp/drill-adv-{A,B} |

### W42.1 pgserver mode — run 1 (exit 0, 29/29 + 1 recorded SKIP)

```bash
OPENDESK_REPO=/tmp/ws XDG_RUNTIME_DIR=/tmp/xdg \
  python3 tests/restore-drill/drill.py --workdir /tmp/drill-pgserver
```

Exit code: **0**. Wall time: **4.97 s** (`/tmp/drill-pgserver/drill-summary.json`;
full subprocess wall 11.3 s incl. interpreter/imports). `drill-summary.json`:
29 results all ok, `skips` array holds exactly 1 entry (the 06 postgis skip,
reason verbatim below), `drill_pg=pgserver`,
`pg_bin_dir=/home/kimi/.local/lib/python3.12/site-packages/pgserver/pginstall/bin`.
Real output:

```
[drill] repo=/tmp/ws
[drill] workdir=/tmp/drill-pgserver
[drill] DRILL_PG=pgserver
[drill] booting pgserver instance A ...
SKIP  06-booking-postgis.sql (postgis init script)  — pgserver ships core PostgreSQL without contrib modules — postgis cannot load. Use DRILL_PG=system on a host with the postgis packages installed for 06 coverage. (Unchanged pre-W42 behavior; now an explicit recorded SKIP.)
[drill] A: applied 00-create-dbs.sql
[drill] A: applied 01-booking-schema.sql
[drill] A: applied 02-identity-schema.sql
[drill] A: applied 03-conversation-schema.sql
[drill] A: applied 04-knowledge-schema.sql
[drill] A: applied 05-app-roles.sql
[drill] A: applied 07-agents-capture-schema.sql
[drill] A: applied 30-model-registry.sql
[drill] A: applied billing migration 0001_init.sql
[drill] A: applied billing migration 0002_rls.sql
[drill] A: applied billing migration 0003_ledger.sql
[drill] A: applied billing migration 0004_outbox.sql
[drill] A: seeded markers {'tenant_id': '02505a92-71c4-4222-a8c0-f13e50119165', 'offering_id': '66b76374-f766-4ee6-9f46-d0b8ceab7c83', 'conversation_id': '8ebac627-b5b6-4894-9300-6177230696a3', 'capture_record_id': '7a2046b0-63e6-4d76-bcad-b74e81395ef1', 'invoice_id': 'c92d01d2-78e1-4e98-9a82-bb14601b69ce', 'model_family': 'drill-marker-family'}
[drill] dumped identity -> identity.dump (5528 bytes)
[drill] dumped booking -> booking.dump (18110 bytes)
[drill] dumped conversation -> conversation.dump (18282 bytes)
[drill] dumped knowledge -> knowledge.dump (6365 bytes)
[drill] dumped billing -> billing.dump (23811 bytes)
[drill] dumped platform -> platform.dump (25345 bytes)
[drill] booting FRESH instance B ...
[drill] B: role layer applied (05 + billing-internal + model-registry + postgres defaults bridge)
PASS  pg_restore identity (--no-owner --no-privileges)  — exit 0
PASS  pg_restore booking (--no-owner --no-privileges)  — exit 0
PASS  pg_restore conversation (--no-owner --no-privileges)  — exit 0
PASS  pg_restore knowledge (--no-owner --no-privileges)  — exit 0
PASS  pg_restore billing (--no-owner --no-privileges)  — exit 0
PASS  pg_restore platform (--no-owner --no-privileges)  — exit 0
PASS  row counts identical: identity
PASS  row counts identical: booking
PASS  row counts identical: conversation
PASS  row counts identical: knowledge
PASS  row counts identical: billing
PASS  row counts identical: platform
PASS  RLS policy present post-restore: identity.tenants  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: identity.tenants  — relforcerowsecurity=True
PASS  RLS policy present post-restore: booking.bookings  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: booking.bookings  — relforcerowsecurity=True
PASS  RLS policy present post-restore: billing.invoices  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: billing.invoices  — relforcerowsecurity=True
PASS  RLS policy present post-restore: conversation.capture_records  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: conversation.capture_records  — relforcerowsecurity=True
PASS  marker row readable: identity.tenants
PASS  marker row readable: booking.offerings
PASS  marker row readable: conversation.capture_records
PASS  marker row readable: billing.invoices
PASS  marker row readable: platform.model_family
PASS  RLS post-restore: wrong app.tenant_id sees 0 invoices
PASS  RLS post-restore: empty app.tenant_id sees 0 invoices (fail-closed)
PASS  RLS post-restore: GUC unset sees 0 invoices (fail-closed)
PASS  RLS post-restore: correct app.tenant_id sees its invoice
[drill] summary written: /tmp/drill-pgserver/drill-summary.json
[drill] OK: 29/29 checks passed; 1 explicit SKIP(s)

(stderr carried only expected NOTICE lines: `policy "tenant_isolation" for
relation ... does not exist, skipping` from idempotent migrations.)

### W42.2 pgserver mode — run 2, independent (exit 0, 29/29 + 1 recorded SKIP)

Same command, fresh workdir `/tmp/drill-pgserver-r2`. Exit code: **0**.
Wall time: **5.04 s** (`drill-summary.json`: 29 results all ok, 1 skip).
Distinct marker UUIDs and per-run dump-size drift (UUID/timestamp variance)
confirm a real re-execution, not a canned log. Key lines:

```
[drill] repo=/tmp/ws
[drill] workdir=/tmp/drill-pgserver-r2
[drill] DRILL_PG=pgserver
SKIP  06-booking-postgis.sql (postgis init script)  — pgserver ships core PostgreSQL without contrib modules — postgis cannot load. Use DRILL_PG=system on a host with the postgis packages installed for 06 coverage. (Unchanged pre-W42 behavior; now an explicit recorded SKIP.)
[drill] A: seeded markers {'tenant_id': 'c0f8084c-c783-4c93-af75-9dd79187278d', 'offering_id': 'f9a40164-6ba0-4fa9-996c-4c8b7a42a8f9', 'conversation_id': '6fdc89e6-a2f1-4c84-90be-b9e2f3df93b1', 'capture_record_id': '05720e11-ea90-41e7-8d8c-7a438f8f8337', 'invoice_id': 'b75eaa51-98e0-4371-a0b1-d41c7f68bb29', 'model_family': 'drill-marker-family'}
[drill] dumped identity -> identity.dump (5529 bytes)
[drill] dumped booking -> booking.dump (18110 bytes)
[drill] dumped conversation -> conversation.dump (18285 bytes)
[drill] dumped knowledge -> knowledge.dump (6365 bytes)
[drill] dumped billing -> billing.dump (23811 bytes)
[drill] dumped platform -> platform.dump (25345 bytes)
[drill] summary written: /tmp/drill-pgserver-r2/drill-summary.json
[drill] OK: 29/29 checks passed; 1 explicit SKIP(s)

### W42.3 postgis acquisition — ordered attempts, verbatim evidence

**(a) apt — BLOCKED (no root):**

```
$ apt-get install -y postgresql postgis
E: Could not open lock file /var/lib/dpkg/lock-frontend - open (13: Permission denied)
E: Unable to acquire the dpkg frontend lock (/var/lib/dpkg/lock-frontend), are you root?
(exit code 100)
$ sudo -n true
sudo: a password is required
(exit code 1)
```

**(b) conda/mamba — not installed; bootstrapped instead.** `which conda
mamba micromamba` all empty. Downloaded the micromamba 2.9.0 static binary
(`https://micro.mamba.pm/api/micromamba/linux-64/latest`, tar.bz2 integrity
verified with `bzip2 -t`), then:

```
$ micromamba create -p /tmp/ce --override-channels -c conda-forge postgis postgresql -y
...
Transaction finished
$ /tmp/ce/bin/postgres --version
postgres (PostgreSQL) 18.6
$ /tmp/ce/bin/psql --version && /tmp/ce/bin/pg_dump --version
psql (PostgreSQL) 18.6
pg_dump (PostgreSQL) 18.6
$ ls /tmp/ce/lib | grep postgis
postgis-3.so  postgis_raster-3.so  postgis_topology-3.so
$ ls /tmp/ce/share/extension | grep -c postgis
519
```

A REAL PostgreSQL 18.6 + postgis 3.6.4 toolchain, installed entirely in
userland — so the postgis leg below is **EXECUTED**, not EXTERNAL_BLOCKED.

**(c) pip route — confirmed impossible (pgserver ships no postgis).** Live
probe of a fresh pgserver cluster (`pgserver.get_server`, then
`pg_available_extensions`):

```
pg_available_extensions (2 entries):
plpgsql, vector
PROBE postgis: ABSENT
PROBE postgis_topology: ABSENT
PROBE pgcrypto: ABSENT
```

Matches upstream (pypi.org/project/pgserver: "includes pgvector extension
but currently excludes postGIS"). No pip-installable postgis server binaries
exist (the `postgis` PyPI package is a Python client library, not binaries).

### W42.4 system mode on a postgis-LESS cluster (fallback leg) — verbatim SKIP, exit 0

`DRILL_PG_BIN` pointed at pgserver's real PG 16.2 binaries
(`/home/kimi/.local/lib/python3.12/site-packages/pgserver/pginstall/bin`)
exercises the full DRILL_PG=system machinery (initdb + pg_ctl per instance,
unix sockets, no TCP) on a cluster whose `pg_available_extensions` lacks
postgis. Exit code: **0**. Wall time: **5.15 s**
(`/tmp/drill-system-fg/drill-summary.json`: 29 results all ok, 1 skip). The
SKIP line is the honest outcome for this cluster and is captured verbatim in
the real output:

```
[drill] repo=/tmp/ws
[drill] workdir=/tmp/drill-system-fg
[drill] DRILL_PG=system
[drill] booting system PostgreSQL instance A ...
[drill] A: system cluster pgcrypto available=False — stripping pgcrypto lines (contrib not installed)
SKIP  06-booking-postgis.sql (postgis init script)  — DRILL_PG=system: extension 'postgis' absent from pg_available_extensions of the system cluster — the postgis packages are not installed (remediation: `apt install postgis` or `apt install postgresql-<ver>-postgis-3`, see `apt-cache search postgis`). The rest of the drill still runs on the system cluster; geo init-script coverage is EXTERNAL_BLOCKED, not silently passed.
[drill] A: applied 00-create-dbs.sql
[drill] A: applied 01-booking-schema.sql
[drill] A: applied 02-identity-schema.sql
[drill] A: applied 03-conversation-schema.sql
[drill] A: applied 04-knowledge-schema.sql
[drill] A: applied 05-app-roles.sql
[drill] A: applied 07-agents-capture-schema.sql
[drill] A: applied 30-model-registry.sql
[drill] A: applied billing migration 0001_init.sql
[drill] A: applied billing migration 0002_rls.sql
[drill] A: applied billing migration 0003_ledger.sql
[drill] A: applied billing migration 0004_outbox.sql
[drill] A: seeded markers {'tenant_id': 'db1be6d7-e883-41d9-9b83-7cf14ca29c88', 'offering_id': '98155246-384e-4a75-ba0c-ee7d18664e89', 'conversation_id': '8c6dfcae-a9dc-4dfd-9b03-dd0c3b66b319', 'capture_record_id': '3e708422-a084-4a83-84e2-3044bad6e575', 'invoice_id': 'e2128b7a-5e0e-40ac-b878-0144ac316801', 'model_family': 'drill-marker-family'}
[drill] dumped identity -> identity.dump (5529 bytes)
[drill] dumped booking -> booking.dump (18111 bytes)
[drill] dumped conversation -> conversation.dump (18290 bytes)
[drill] dumped knowledge -> knowledge.dump (6365 bytes)
[drill] dumped billing -> billing.dump (23814 bytes)
[drill] dumped platform -> platform.dump (25345 bytes)
[drill] booting FRESH instance B ...
[drill] B: role layer applied (05 + billing-internal + model-registry + postgres defaults bridge)
PASS  pg_restore identity (--no-owner --no-privileges)  — exit 0
PASS  pg_restore booking (--no-owner --no-privileges)  — exit 0
PASS  pg_restore conversation (--no-owner --no-privileges)  — exit 0
PASS  pg_restore knowledge (--no-owner --no-privileges)  — exit 0
PASS  pg_restore billing (--no-owner --no-privileges)  — exit 0
PASS  pg_restore platform (--no-owner --no-privileges)  — exit 0
PASS  row counts identical: identity
PASS  row counts identical: booking
PASS  row counts identical: conversation
PASS  row counts identical: knowledge
PASS  row counts identical: billing
PASS  row counts identical: platform
PASS  RLS policy present post-restore: identity.tenants  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: identity.tenants  — relforcerowsecurity=True
PASS  RLS policy present post-restore: booking.bookings  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: booking.bookings  — relforcerowsecurity=True
PASS  RLS policy present post-restore: billing.invoices  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: billing.invoices  — relforcerowsecurity=True
PASS  RLS policy present post-restore: conversation.capture_records  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: conversation.capture_records  — relforcerowsecurity=True
PASS  marker row readable: identity.tenants
PASS  marker row readable: booking.offerings
PASS  marker row readable: conversation.capture_records
PASS  marker row readable: billing.invoices
PASS  marker row readable: platform.model_family
PASS  RLS post-restore: wrong app.tenant_id sees 0 invoices
PASS  RLS post-restore: empty app.tenant_id sees 0 invoices (fail-closed)
PASS  RLS post-restore: GUC unset sees 0 invoices (fail-closed)
PASS  RLS post-restore: correct app.tenant_id sees its invoice
[drill] summary written: /tmp/drill-system-fg/drill-summary.json
[drill] OK: 29/29 checks passed; 1 explicit SKIP(s)

### W42.5 system mode with REAL postgis (conda-forge PG 18.6 + postgis 3.6.4) — postgis leg EXECUTED

```bash
DRILL_PG=system DRILL_PG_BIN=/tmp/ce/bin OPENDESK_REPO=/tmp/ws XDG_RUNTIME_DIR=/tmp/xdg \
  python3 tests/restore-drill/drill.py --workdir /tmp/drill-system-postgis
```

Run 1 — exit code: **0**. Wall time: **6.51 s**
(`/tmp/drill-system-postgis/drill-summary.json`: 30 results all ok,
`skips: []`). pgcrypto available ⇒ init scripts applied FULLY VERBATIM (no
stripping); postgis available ⇒ `06-booking-postgis.sql` applied verbatim in
docker order (after 05, before 07); the extension rode the
`pg_dump -Fc` / `pg_restore --no-owner --no-privileges` cycle and is asserted
live on instance B. Real output:

```
[drill] repo=/tmp/ws
[drill] workdir=/tmp/drill-system-postgis
[drill] DRILL_PG=system
[drill] booting system PostgreSQL instance A ...
[drill] A: system cluster pgcrypto available=True — init scripts applied FULLY VERBATIM
[drill] A: postgis available — 06-booking-postgis.sql joins the drill verbatim
[drill] A: applied 00-create-dbs.sql
[drill] A: applied 01-booking-schema.sql
[drill] A: applied 02-identity-schema.sql
[drill] A: applied 03-conversation-schema.sql
[drill] A: applied 04-knowledge-schema.sql
[drill] A: applied 05-app-roles.sql
[drill] A: applied 06-booking-postgis.sql
[drill] A: applied 07-agents-capture-schema.sql
[drill] A: applied 30-model-registry.sql
[drill] A: applied billing migration 0001_init.sql
[drill] A: applied billing migration 0002_rls.sql
[drill] A: applied billing migration 0003_ledger.sql
[drill] A: applied billing migration 0004_outbox.sql
[drill] A: seeded markers {'tenant_id': 'e5ecdb8e-0c9e-4e7c-9aca-c130171626a6', 'offering_id': 'a535eeae-d358-4de8-ab2c-05c48c57f4ae', 'conversation_id': 'd9a591da-66ca-457c-af92-236f3f6184a0', 'capture_record_id': '5878f775-1431-4d7f-9749-f82e321137df', 'invoice_id': 'b880e6dd-6082-402f-9d9d-4d758a20ddbd', 'model_family': 'drill-marker-family'}
[drill] dumped identity -> identity.dump (6016 bytes)
[drill] dumped booking -> booking.dump (19483 bytes)
[drill] dumped conversation -> conversation.dump (18951 bytes)
[drill] dumped knowledge -> knowledge.dump (6882 bytes)
[drill] dumped billing -> billing.dump (24133 bytes)
[drill] dumped platform -> platform.dump (26087 bytes)
[drill] booting FRESH instance B ...
[drill] B: role layer applied (05 + billing-internal + model-registry + postgres defaults bridge)
PASS  pg_restore identity (--no-owner --no-privileges)  — exit 0
PASS  pg_restore booking (--no-owner --no-privileges)  — exit 0
PASS  pg_restore conversation (--no-owner --no-privileges)  — exit 0
PASS  pg_restore knowledge (--no-owner --no-privileges)  — exit 0
PASS  pg_restore billing (--no-owner --no-privileges)  — exit 0
PASS  pg_restore platform (--no-owner --no-privileges)  — exit 0
PASS  row counts identical: identity
PASS  row counts identical: booking
PASS  row counts identical: conversation
PASS  row counts identical: knowledge
PASS  row counts identical: billing
PASS  row counts identical: platform
PASS  RLS policy present post-restore: identity.tenants  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: identity.tenants  — relforcerowsecurity=True
PASS  RLS policy present post-restore: booking.bookings  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: booking.bookings  — relforcerowsecurity=True
PASS  RLS policy present post-restore: billing.invoices  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: billing.invoices  — relforcerowsecurity=True
PASS  RLS policy present post-restore: conversation.capture_records  — policies=['tenant_isolation']
PASS  FORCE ROW LEVEL SECURITY post-restore: conversation.capture_records  — relforcerowsecurity=True
PASS  marker row readable: identity.tenants
PASS  marker row readable: booking.offerings
PASS  marker row readable: conversation.capture_records
PASS  marker row readable: billing.invoices
PASS  marker row readable: platform.model_family
PASS  postgis: extension live on booking DB post-restore (06 rode pg_dump/pg_restore)  — extversion=3.6.4
PASS  RLS post-restore: wrong app.tenant_id sees 0 invoices
PASS  RLS post-restore: empty app.tenant_id sees 0 invoices (fail-closed)
PASS  RLS post-restore: GUC unset sees 0 invoices (fail-closed)
PASS  RLS post-restore: correct app.tenant_id sees its invoice
[drill] summary written: /tmp/drill-system-postgis/drill-summary.json
[drill] OK: 30/30 checks passed

Run 2 — independent, `--keep` (workdir `/tmp/drill-sys-pg2`): exit code **0**,
**30/30**, 0 skips, wall **6.25 s**. Key lines:

```
[drill] repo=/tmp/ws
[drill] workdir=/tmp/drill-sys-pg2
[drill] DRILL_PG=system
[drill] A: system cluster pgcrypto available=True — init scripts applied FULLY VERBATIM
[drill] A: postgis available — 06-booking-postgis.sql joins the drill verbatim
[drill] A: applied 06-booking-postgis.sql
[drill] A: seeded markers {'tenant_id': 'a8d7d889-34bb-4af0-847c-c546b2d516d4', 'offering_id': '2fa9c7bd-f090-4115-8630-33df1fc1d5a9', 'conversation_id': '486b2f49-507e-49d8-a825-3a431f6f05bd', 'capture_record_id': '5aba075c-eb1f-4fb8-b546-89cf05fa8f4f', 'invoice_id': '2afc4d35-905e-437e-9123-5f43fcb203b7', 'model_family': 'drill-marker-family'}
[drill] dumped identity -> identity.dump (6016 bytes)
[drill] dumped booking -> booking.dump (19482 bytes)
[drill] dumped conversation -> conversation.dump (18955 bytes)
[drill] dumped knowledge -> knowledge.dump (6882 bytes)
[drill] dumped billing -> billing.dump (24134 bytes)
[drill] dumped platform -> platform.dump (26087 bytes)
PASS  postgis: extension live on booking DB post-restore (06 rode pg_dump/pg_restore)  — extversion=3.6.4
[drill] summary written: /tmp/drill-sys-pg2/drill-summary.json
[drill] OK: 30/30 checks passed

**Independent post-restore verification of instance B** (verifier did not
trust the drill's own assertion): cluster B from run 2 was restarted by hand
and queried directly:

```
$ pg_ctl -D /tmp/drill-sys-pg2/pgdata-B -o "-k /tmp/drill-sys-pg2/sock-B" -o "-c listen_addresses=''" start
$ psql -h /tmp/drill-sys-pg2/sock-B -U postgres -d booking
 extname  | extversion
----------+------------
 plpgsql  | 1.0
 pgcrypto | 1.4
 postgis  | 3.6.4

POSTGIS="3.6.4 f9c153b" [EXTENSION] PGSQL="180" GEOS="3.14.1-CAPI-1.20.5"
PROJ="9.8.1 ..." LIBXML="2.14.6" LIBJSON="0.18" LIBPROTOBUF="1.5.2" WAGYU="0.5.0"

SELECT ST_AsText(ST_MakePoint(1.5, 2.5)), count(*) FROM spatial_ref_sys;
      geom      | srs_rows
----------------+----------
 POINT(1.5 2.5) |     8500
```

postgis is not merely present but FUNCTIONAL post-restore, and its data
(8500 `spatial_ref_sys` rows) rode the dump. The dump itself carries the
extension (`pg_restore -l /tmp/drill-system-postgis/dumps/booking.dump`):

```
3; 3079 16848 EXTENSION - postgis
4295; 0 17167 TABLE DATA public spatial_ref_sys postgres
```

### W42.6 adversarial — the postgis SKIP cannot masquerade as PASS

* **Code path:** `record_skip()` appends ONLY to `SKIPS` (never `RESULTS`);
  the PASS count and the exit code derive exclusively from `RESULTS`
  (`return 0 if not failed else 1`); SKIP lines print with a `SKIP` prefix,
  never `PASS`. The final tally line prints `; N explicit SKIP(s)`
  separately.
* **ADV-A — `DRILL_PG_BIN=/nonexistent` (no usable binaries anywhere):**
  exit 0 BUT **zero checks recorded** (`drill-summary.json` `results: []`,
  `skips: 1`) and a single whole-mode SKIP — an explicit EXTERNAL_BLOCKED
  trail, not a green run:

  ```
  SKIP  DRILL_PG=system restore-drill mode  — no system PostgreSQL binaries found (initdb/psql/pg_ctl absent from DRILL_PG_BIN, /usr/lib/postgresql/*/bin and PATH) — install with `apt install postgresql postgis`; system-PG mode NOT executed. This SKIP is the EXTERNAL_BLOCKED evidence trail, not a pass.
  [drill] summary written: /tmp/drill-adv-A/drill-summary.json
  ```

  (No `PASS` lines, no `[drill] OK` tally — nothing here can be misread as
  a pass.)
* **ADV-B — `DRILL_PG_BIN=/tmp/fakebin` (initdb/psql/pg_ctl/pg_dump/
  pg_restore shell stubs that print `FAKE BINARY — deliberately broken` and
  exit 1):** exit code **1** with an honest traceback, no PASS lines, no
  summary greenwash:

  ```
  [drill] booting system PostgreSQL instance A ...
  RuntimeError: initdb failed for A: FAKE BINARY — deliberately broken
  (exit 1)
  ```

### W42 verdict

**PASS** — the W42 drill executes as specified in both modes: pgserver mode
unchanged at 29/29 + 1 recorded SKIP (2 independent runs, exit 0); system
mode exercised end-to-end on real clusters with per-instance initdb/pg_ctl
over unix sockets. Postgis availability verdict: **EXECUTED** —
`06-booking-postgis.sql` applied verbatim on instance A and postgis 3.6.4
verified live and functional on instance B post-restore (2 runs + independent
probe). The apt-blocked EXTERNAL_BLOCKED trail (W42.3a) and the verbatim
postgis SKIP on a postgis-less system cluster (W42.4) are recorded above as
the fallback evidence.
