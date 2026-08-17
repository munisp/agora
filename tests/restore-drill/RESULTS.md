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
