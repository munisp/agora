#!/usr/bin/env python3
"""restore-drill — backup/restore + migration drill against REAL Postgres.

Two REAL Postgres clusters, one dump+restore cycle:

  instance A  — init-scripts applied exactly like the docker
                postgres docker-entrypoint-initdb.d psql path (psql stdin,
                \\c meta-commands supported),
                billing migrations 0001..0004 applied, marker rows seeded.
  pg_dump -Fc — one custom-format dump per application database (mirrors
                infra/backups/backup.sh).
  instance B  — FRESH cluster; createdb + roles/grants, then
                pg_restore --no-owner --no-privileges (mirrors
                infra/backups/restore.sh exactly).

Cluster backend (env DRILL_PG):
  * DRILL_PG=pgserver (DEFAULT) — embedded full PostgreSQL binaries via the
    pgserver package (real PostgreSQL 16, unix-socket only, no contrib
    modules). Adaptations listed below.
  * DRILL_PG=system (SPEC-W42) — locally installed system PostgreSQL
    (apt `postgresql`; binaries from DRILL_PG_BIN, else
    /usr/lib/postgresql/<ver>/bin, else PATH). initdb+pg_ctl run a real
    cluster per instance on a workdir-local unix socket (no TCP). This mode
    exists so the postgis init script (06-booking-postgis.sql) can be
    EXECUTED as part of the drill when the postgis packages are installed
    (apt `postgis` / `postgresql-<ver>-postgis-3`): 06 is applied verbatim
    on instance A, the extension rides the pg_dump/pg_restore cycle like any
    schema object, and instance B is asserted to have postgis live on the
    booking DB. If postgis is NOT available (pg_available_extensions lacks
    it), the drill prints an explicit `SKIP` line with the precise reason
    and records a skipped check in drill-summary.json — never a silent
    pass. If no system PostgreSQL is found at all, the whole system mode is
    an explicit SKIP with the apt remediation in the message (evidence for
    EXTERNAL_BLOCKED), exit 0. When running as root, initdb/pg_ctl/postgres
    are demoted to the `postgres` (else `nobody`) account via setpriv/su
    (PostgreSQL refuses to run as root); psql/pg_dump/pg_restore run as the
    invoking user over the unix socket.
    In system mode the pgcrypto strip is conditional: pgcrypto ships with
    the postgresql-contrib package, so when it is present the init scripts
    apply fully verbatim.

pgserver-mode adaptations (ALL documented, none weaken the drill):
  1. `CREATE EXTENSION IF NOT EXISTS pgcrypto;` lines are stripped before
     applying 01/02/03/04/07 — pgserver ships CORE PostgreSQL without contrib
     modules (no pgcrypto/postgis). gen_random_uuid() is core since PG13 and
     is all those schemas use pgcrypto for. 30-model-registry.sql already
     guards its own pgcrypto line and is applied verbatim.
  2. 06-booking-postgis.sql is SKIPPED with an explicit SKIP line: postgis
     is a contrib extension pgserver cannot provide. The booking store
     tolerates its absence (geo features are additive; the drill covers
     schema/RLS fidelity, not geo). Use DRILL_PG=system for postgis coverage.
  3. 05-app-roles.sql references the docker bootstrap superuser `opendesk`
     in ALTER DEFAULT PRIVILEGES ... FOR ROLE opendesk. pgserver's bootstrap
     superuser is `postgres`, so the drill (a) creates a NOLOGIN `opendesk`
     role so the script applies verbatim, and (b) adds the SAME default
     privileges FOR ROLE postgres — the generalized idiom billing's
     0002_rls.sql already uses (FOREACH ['opendesk','postgres']). Without
     (b), pg_restore --no-privileges (tables created by `postgres`) would
     leave app roles with no grants — in production restore.sh restores as
     `opendesk`, so the FOR ROLE opendesk defaults cover it. (In system mode
     the bootstrap superuser is likewise `postgres`; identical handling.)
  4. Cluster-global roles are NOT carried by per-database pg_dump (that is
     what pg_dumpall --globals-only would do; production restore.sh likewise
     assumes the target cluster already ran the init scripts). Instance B
     therefore gets the role/grant layer (05 + the app_billing_internal and
     app_model_registry role blocks) BEFORE pg_restore.

Assertions (exit 1 on any failure):
  * per-table row counts identical A vs B for every application table;
  * RLS policies present (pg_policies) on tenants/bookings/invoices/
    capture_records post-restore;
  * pg_class.relforcerowsecurity true on those four tables post-restore;
  * marker rows readable post-restore;
  * RLS still enforced post-restore: app_billing_login with a WRONG
    app.tenant_id sees 0 invoices; with app.tenant_id='' sees 0 (W40-6
    NULLIF fail-closed posture); with the RIGHT tenant sees exactly 1;
  * DRILL_PG=system with postgis available: 06 applied verbatim on A and
    pg_extension postgis present on booking post-restore on B.

Usage:
  python3 tests/restore-drill/drill.py [--workdir /tmp/restore-drill] [--keep]
  DRILL_PG=system python3 tests/restore-drill/drill.py   # system PG + postgis
Deps (pgserver mode): pip install pgserver==0.1.4 psycopg   (network: pgserver
downloads PG binaries once into the package dir on first get_server())
Deps (system mode): apt install postgresql postgis (or postgresql-<ver>-postgis-3)
"""

from __future__ import annotations

import argparse
import json
import os
import pwd
import shlex
import shutil
import subprocess
import sys
import time
import urllib.parse
import uuid
from pathlib import Path

# pgserver wants a private XDG_RUNTIME_DIR (it places sockets/locks there).
os.environ.setdefault("XDG_RUNTIME_DIR", "/tmp/xdg")
Path(os.environ["XDG_RUNTIME_DIR"]).mkdir(parents=True, exist_ok=True)
os.chmod(os.environ["XDG_RUNTIME_DIR"], 0o700)

import pgserver  # noqa: E402
import psycopg  # noqa: E402
import psycopg.conninfo  # noqa: E402

REPO_ROOT = Path(os.environ.get("OPENDESK_REPO", Path(__file__).resolve().parents[2]))
INIT = REPO_ROOT / "infra/postgres/init-scripts"
BILLING_MIGRATIONS = REPO_ROOT / "services/billing-engine/migrations"

# DRILL_PG=pgserver (default, embedded PG binaries) | system (SPEC-W42:
# locally installed postgresql+postgis, enables the 06-booking-postgis.sql
# leg of the drill).
DRILL_PG = os.environ.get("DRILL_PG", "pgserver").strip().lower()

# Init scripts applied to instance A, in docker-entrypoint order. 06 is
# absent here: it is inserted (after 05) only when the cluster backend can
# provide the postgis extension — DRILL_PG=system with the postgis packages
# installed. In pgserver mode it is SKIPPED with an explicit line.
INIT_SCRIPTS = [
    "00-create-dbs.sql",
    "01-booking-schema.sql",
    "02-identity-schema.sql",
    "03-conversation-schema.sql",
    "04-knowledge-schema.sql",
    "05-app-roles.sql",
    "07-agents-capture-schema.sql",
    "30-model-registry.sql",
]
POSTGIS_SCRIPT = "06-booking-postgis.sql"  # docker order: after 05, before 07
BILLING_MIGRATION_FILES = ["0001_init.sql", "0002_rls.sql", "0003_ledger.sql", "0004_outbox.sql"]

# Application databases covered by the drill (schema-bearing).
APP_DBS = ["identity", "booking", "conversation", "knowledge", "billing", "platform"]

# Tables that MUST carry FORCE ROW LEVEL SECURITY + a policy post-restore.
RLS_ASSERTIONS = [
    ("identity", "tenants"),
    ("booking", "bookings"),
    ("billing", "invoices"),
    ("conversation", "capture_records"),
]

RESULTS: list[dict] = []
SKIPS: list[dict] = []


def record(name: str, ok: bool, detail: str = "") -> None:
    RESULTS.append({"check": name, "ok": ok, "detail": detail})
    print(f"{'PASS' if ok else 'FAIL'}  {name}" + (f"  — {detail}" if detail else ""), flush=True)


def record_skip(name: str, reason: str) -> None:
    """An explicit, recorded SKIP — evidence for EXTERNAL_BLOCKED, never a
    silent pass."""
    SKIPS.append({"check": name, "skipped": True, "reason": reason})
    print(f"SKIP  {name}  — {reason}", flush=True)


def strip_contrib_extensions(sql: str) -> str:
    """Remove `CREATE EXTENSION IF NOT EXISTS pgcrypto;` lines.

    pgserver ships core PostgreSQL WITHOUT contrib modules, so pgcrypto is
    unavailable; every use in these schemas is gen_random_uuid(), which has
    been core since PG13. The line is replaced by a comment so the applied
    text stays auditable. Nothing else is stripped. (In DRILL_PG=system mode
    this is only used when the system cluster also lacks pgcrypto — the
    postgresql-contrib package normally provides it.)
    """
    out = []
    for line in sql.splitlines():
        if line.strip().lower().startswith("create extension if not exists pgcrypto"):
            out.append("-- [restore-drill: stripped — backend has no contrib pgcrypto] " + line)
        else:
            out.append(line)
    return "\n".join(out)


def psql(server, sql: str) -> None:
    """Apply SQL verbatim through real psql (handles \\c like initdb does)."""
    server.psql("\\set ON_ERROR_STOP on\n" + sql)  # raises CalledProcessError


# ---------------------------------------------------------------------------
# System-PostgreSQL cluster backend (DRILL_PG=system, SPEC-W42)
# ---------------------------------------------------------------------------

_SYSTEM_BIN: Path | None = None  # resolved lazily by system_pg_bindir()


def system_pg_bindir() -> Path | None:
    """Locate system PostgreSQL binaries: DRILL_PG_BIN, then the highest
    /usr/lib/postgresql/<ver>/bin, then PATH."""
    global _SYSTEM_BIN
    if _SYSTEM_BIN is not None:
        return _SYSTEM_BIN
    cands: list[Path] = []
    env_dir = os.environ.get("DRILL_PG_BIN")
    if env_dir:
        cands.append(Path(env_dir))
    pg_lib = Path("/usr/lib/postgresql")
    if pg_lib.is_dir():
        for d in sorted(pg_lib.iterdir(), key=lambda p: p.name, reverse=True):
            cands.append(d / "bin")
    which_psql = shutil.which("psql")
    if which_psql:
        cands.append(Path(which_psql).resolve().parent)
    for d in cands:
        if (d / "initdb").exists() and (d / "psql").exists() and (d / "pg_ctl").exists():
            _SYSTEM_BIN = d
            return d
    return None


def _unpriv_account() -> tuple[int, int, str] | None:
    """(uid, gid, name) for running postgres when we are root: the Debian
    `postgres` account (created by the apt package) preferred, else nobody.
    None when not root (no demotion needed) or no account exists."""
    if not hasattr(os, "geteuid") or os.geteuid() != 0:
        return None
    for name in ("postgres", "nobody"):
        try:
            ent = pwd.getpwnam(name)
            return ent.pw_uid, ent.pw_gid, name
        except KeyError:
            continue
    return None


class SystemPG:
    """A real system-PostgreSQL cluster on a workdir-local unix socket.

    Same interface the drill uses on a pgserver server object:
    psql(sql), get_uri(database), socket_dir (attribute), cleanup().
    """

    def __init__(self, pgdata: Path, label: str):
        bindir = system_pg_bindir()
        if bindir is None:
            raise RuntimeError("no system PostgreSQL binaries found")
        self.bin = bindir
        self.pgdata = pgdata
        self.label = label
        self.socket_dir = str(pgdata.parent / f"sock-{label}")
        self.log = str(pgdata.parent / f"postgres-{label}.log")
        self._acct = _unpriv_account()
        Path(self.socket_dir).mkdir(parents=True, exist_ok=True)
        if self._acct:
            uid, gid, _name = self._acct
            # postgres refuses to run as root: hand the dirs to the demoted
            # account and make the workdir chain traversable.
            for d in (pgdata.parent, Path(self.socket_dir)):
                os.chown(d, uid, gid)
                os.chmod(d, 0o777)
            os.chmod(pgdata.parent.parent, os.stat(pgdata.parent.parent).st_mode | 0o755)
        self._initdb()
        self._start()

    def _run_demoted(self, argv: list[str], **kw) -> subprocess.CompletedProcess:
        if self._acct is None:
            return subprocess.run(argv, **kw)
        uid, gid, name = self._acct
        if shutil.which("setpriv"):
            return subprocess.run(
                ["setpriv", f"--reuid={uid}", f"--regid={gid}", "--clear-groups"] + argv, **kw)
        return subprocess.run(["su", "-s", "/bin/sh", name, "-c", shlex.join(argv)], **kw)

    def _initdb(self) -> None:
        r = self._run_demoted(
            [str(self.bin / "initdb"), "-D", str(self.pgdata), "-U", "postgres",
             "--auth=trust", "-E", "UTF8", "--no-instructions"],
            capture_output=True, text=True)
        if r.returncode != 0:
            raise RuntimeError(f"initdb failed for {self.label}: {r.stderr.strip()[:400]}")

    def _start(self) -> None:
        r = self._run_demoted(
            [str(self.bin / "pg_ctl"), "-D", str(self.pgdata), "-l", self.log,
             "-o", f"-k {self.socket_dir}",
             "-o", "-c listen_addresses=''",  # unix socket only, no TCP ports
             "-o", "-c unix_socket_permissions=0777",
             "-w", "-t", "60", "start"],
            capture_output=True, text=True)
        if r.returncode != 0:
            raise RuntimeError(
                f"pg_ctl start failed for {self.label}: {r.stderr.strip()[:300]}; see {self.log}")

    def psql(self, sql: str) -> None:
        """Drop-in for pgserver's server.psql: real psql over the socket,
        stdin input (\\c handled natively), CalledProcessError on failure."""
        r = subprocess.run(
            [str(self.bin / "psql"), "-X", "-h", self.socket_dir, "-U", "postgres",
             "-d", "postgres"],
            input=sql, capture_output=True, text=True)
        if r.returncode != 0:
            raise subprocess.CalledProcessError(r.returncode, "psql", output=r.stdout,
                                                stderr=r.stderr)

    def psql_out(self, sql: str) -> str:
        """psql -tA returning stdout (probes)."""
        r = subprocess.run(
            [str(self.bin / "psql"), "-X", "-tA", "-h", self.socket_dir, "-U", "postgres",
             "-d", "postgres", "-c", sql],
            capture_output=True, text=True)
        if r.returncode != 0:
            raise subprocess.CalledProcessError(r.returncode, "psql", output=r.stdout,
                                                stderr=r.stderr)
        return r.stdout.strip()

    def extension_available(self, name: str) -> bool:
        return self.psql_out(
            "SELECT 1 FROM pg_available_extensions WHERE name = "
            + "'" + name.replace("'", "''") + "'") == "1"

    def get_uri(self, database: str = "postgres") -> str:
        # Keyword-conninfo form (socket dir in host=); psycopg.conninfo
        # parses it identically to pgserver's URI form.
        return f"host={self.socket_dir} user=postgres dbname={database}"

    def cleanup(self) -> None:
        self._run_demoted([str(self.bin / "pg_ctl"), "-D", str(self.pgdata),
                           "-m", "fast", "-w", "-t", "30", "stop"],
                          capture_output=True, text=True)


def boot_cluster(pgdata: Path, label: str):
    """Cluster backend per DRILL_PG: pgserver embedded PG (default) or a
    system PostgreSQL cluster (SPEC-W42)."""
    if DRILL_PG == "system":
        return SystemPG(pgdata, label)
    return pgserver.get_server(str(pgdata))


def pg_bin(name: str) -> str:
    """pg_dump/pg_restore/psql from the active backend: the system bin dir in
    DRILL_PG=system mode (client and cluster versions must match), else
    pgserver's bundled full bin/ dir."""
    if DRILL_PG == "system":
        bindir = system_pg_bindir()
        if bindir is not None and (bindir / name).exists():
            return str(bindir / name)
        found = shutil.which(name)
        if found:
            return found
        raise RuntimeError(f"{name} not found (DRILL_PG_BIN /usr/lib/postgresql/*/bin nor PATH)")
    bundled = Path(pgserver.__file__).resolve().parent / "pginstall" / "bin" / name
    if bundled.exists():
        return str(bundled)
    found = shutil.which(name)
    if found:
        return found
    raise RuntimeError(f"{name} not found (pgserver pginstall/bin nor PATH)")


def socket_dir(server) -> str:
    """Socket dir of the active cluster (attribute on SystemPG; ?host= query
    of pgserver's URI otherwise)."""
    sd = getattr(server, "socket_dir", None)
    if isinstance(sd, str):
        return sd
    q = urllib.parse.parse_qs(urllib.parse.urlparse(server.get_uri()).query)
    return q["host"][0]


def dsn_for(server, database: str, user: str | None = None, password: str | None = None) -> str:
    info = psycopg.conninfo.conninfo_to_dict(server.get_uri(database=database))
    if user:
        info["user"] = user
    if password:
        info["password"] = password
    return psycopg.conninfo.make_conninfo(**info)


def table_counts(server, database: str) -> dict[str, int]:
    with psycopg.connect(dsn_for(server, database), autocommit=True) as c:
        tables = [
            r[0]
            for r in c.execute(
                "SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY 1"
            )
        ]
        return {t: c.execute(f'SELECT count(*) FROM "{t}"').fetchone()[0] for t in tables}


def apply_role_layer(server) -> None:
    """Cluster-global roles + grants + default privileges (pre-pg_restore).

    Mirrors what a fresh docker cluster has after the init scripts ran. The
    app_billing_internal / app_model_registry blocks mirror the DO-blocks in
    services/billing-engine/migrations/0002_rls.sql and
    infra/postgres/init-scripts/30-model-registry.sql respectively (those
    files also carry schema, which here arrives via pg_restore instead).
    """
    # 05 references FOR ROLE opendesk (docker bootstrap superuser); the drill
    # cluster's is `postgres` (pgserver bootstrap; initdb -U postgres in
    # system mode), so create the name and bridge defaults for BOTH (the
    # 0002_rls.sql FOREACH idiom).
    psql(server, "DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='opendesk')"
                 " THEN CREATE ROLE opendesk NOLOGIN; END IF; END $$;")
    psql(server, (INIT / "05-app-roles.sql").read_text())
    # billing internal batch role (0002_rls.sql DO-block, verbatim SQL)
    psql(server, """
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_billing_internal') THEN
        CREATE ROLE app_billing_internal NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_billing_internal_login') THEN
        CREATE ROLE app_billing_internal_login LOGIN PASSWORD 'app_billing_internal_dev_password' IN ROLE app_billing_internal;
    END IF;
END
$$;
-- model-registry roles (30-model-registry.sql DO-block, verbatim SQL)
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_model_registry') THEN
        CREATE ROLE app_model_registry NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_model_registry_login') THEN
        CREATE ROLE app_model_registry_login LOGIN PASSWORD 'app_model_registry_dev_password' IN ROLE app_model_registry;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_model_registry_internal') THEN
        CREATE ROLE app_model_registry_internal NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_model_registry_batch') THEN
        CREATE ROLE app_model_registry_batch LOGIN PASSWORD 'app_model_registry_batch_dev_password' IN ROLE app_model_registry_internal;
    END IF;
END
$$;
"""
    )
    # DB-level grants + default privileges FOR ROLE postgres so tables
    # created by pg_restore (as the cluster superuser) are reachable by the
    # app roles — exactly what FOR ROLE opendesk defaults do in production.
    db_roles = {
        "booking": ["app_booking"],
        "conversation": ["app_conversation"],
        "knowledge": ["app_knowledge"],
        "billing": ["app_billing", "app_billing_internal"],
        "kyc": ["app_kyc"],
        "platform": ["app_model_registry", "app_model_registry_internal"],
    }
    for db, roles in db_roles.items():
        stmts = [f"\\c {db}"]
        for role in roles:
            stmts.append(f"GRANT CONNECT ON DATABASE {db} TO {role};")
            stmts.append(f"GRANT USAGE ON SCHEMA public TO {role};")
            stmts.append(
                f"ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public "
                f"GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO {role};"
            )
            stmts.append(
                f"ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public "
                f"GRANT USAGE, SELECT ON SEQUENCES TO {role};"
            )
        psql(server, "\n".join(stmts))


def seed_markers(server) -> dict[str, str]:
    """One marker row per application domain (inserted as superuser)."""
    markers: dict[str, str] = {}
    tenant = uuid.uuid4()
    markers["tenant_id"] = str(tenant)
    with psycopg.connect(dsn_for(server, "identity"), autocommit=True) as c:
        c.execute(
            "INSERT INTO tenants (id, slug, name) VALUES (%s, 'drill-tenant', 'Drill Marker Co')",
            (tenant,),
        )
    with psycopg.connect(dsn_for(server, "booking"), autocommit=True) as c:
        markers["offering_id"] = str(
            c.execute(
                "INSERT INTO offerings (tenant_id, name, duration_min, price_cents)"
                " VALUES (%s, 'drill-marker-offering', 30, 1500) RETURNING id",
                (tenant,),
            ).fetchone()[0]
        )
    with psycopg.connect(dsn_for(server, "conversation"), autocommit=True) as c:
        conv = c.execute(
            "INSERT INTO conversations (tenant_id, site_slug) VALUES (%s, 'drill-site') RETURNING id",
            (tenant,),
        ).fetchone()[0]
        markers["conversation_id"] = str(conv)
        agent = c.execute(
            "INSERT INTO agents (tenant_id, name, slug) VALUES (%s, 'drill-agent', 'drill-agent') RETURNING id",
            (tenant,),
        ).fetchone()[0]
        schema = c.execute(
            "INSERT INTO capture_schemas (tenant_id, agent_id, name) VALUES (%s, %s, 'drill-schema') RETURNING id",
            (tenant, agent),
        ).fetchone()[0]
        markers["capture_record_id"] = str(
            c.execute(
                "INSERT INTO capture_records (tenant_id, capture_schema_id, agent_id, conversation_id, data)"
                " VALUES (%s, %s, %s, %s, '{\"marker\": true}'::jsonb) RETURNING id",
                (tenant, schema, agent, conv),
            ).fetchone()[0]
        )
    with psycopg.connect(dsn_for(server, "billing"), autocommit=True) as c:
        c.execute("INSERT INTO processed_events (event_id) VALUES ('drill-marker-event')")
        c.execute(
            "INSERT INTO usage_records (tenant_id, metric, value, ts, event_id)"
            " VALUES (%s, 'calls', 42, now(), 'drill-marker-event')",
            (tenant,),
        )
        markers["invoice_id"] = str(
            c.execute(
                "INSERT INTO invoices (tenant_id, period, status, subtotal_cents)"
                " VALUES (%s, '2026-08', 'issued', 4200) RETURNING id",
                (tenant,),
            ).fetchone()[0]
        )
    with psycopg.connect(dsn_for(server, "platform"), autocommit=True) as c:
        c.execute("INSERT INTO model_family (name) VALUES ('drill-marker-family')")
        markers["model_family"] = "drill-marker-family"
    return markers


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--workdir", default=os.environ.get("RESTORE_DRILL_WORKDIR", "/tmp/restore-drill"))
    ap.add_argument("--keep", action="store_true", help="keep pgdata dirs (debug)")
    args = ap.parse_args()
    workdir = Path(args.workdir)
    workdir.mkdir(parents=True, exist_ok=True)
    dumps = workdir / "dumps"
    dumps.mkdir(exist_ok=True)
    started = time.time()

    print(f"[drill] repo={REPO_ROOT}")
    print(f"[drill] workdir={workdir}")
    print(f"[drill] DRILL_PG={DRILL_PG}")

    if DRILL_PG not in ("pgserver", "system"):
        print(f"[drill] ERROR: unknown DRILL_PG={DRILL_PG!r} (expected pgserver|system)")
        return 2
    if DRILL_PG == "system" and system_pg_bindir() is None:
        record_skip(
            "DRILL_PG=system restore-drill mode",
            "no system PostgreSQL binaries found (initdb/psql/pg_ctl absent from "
            "DRILL_PG_BIN, /usr/lib/postgresql/*/bin and PATH) — install with "
            "`apt install postgresql postgis`; system-PG mode NOT executed. This "
            "SKIP is the EXTERNAL_BLOCKED evidence trail, not a pass.")
        finalize(workdir, started, None, None, args.keep)
        return 0

    # ---- instance A: init + migrate + seed -------------------------------
    print(f"[drill] booting {'system PostgreSQL' if DRILL_PG == 'system' else 'pgserver'} instance A ...")
    srv_a = boot_cluster(workdir / "pgdata-A", "A")

    # pgcrypto: pgserver (core-only binaries) always needs the strip; system
    # clusters get verbatim scripts when postgresql-contrib provides pgcrypto.
    strip_crypto = True
    if DRILL_PG == "system":
        strip_crypto = not srv_a.extension_available("pgcrypto")
        print(f"[drill] A: system cluster pgcrypto available={not strip_crypto}"
              + (" — init scripts applied FULLY VERBATIM" if not strip_crypto
                 else " — stripping pgcrypto lines (contrib not installed)"))

    # postgis (SPEC-W42): system mode executes 06-booking-postgis.sql as part
    # of the drill when the extension packages are installed; otherwise an
    # explicit SKIP with the precise reason.
    postgis_applied = False
    scripts = list(INIT_SCRIPTS)
    if DRILL_PG == "system":
        if srv_a.extension_available("postgis"):
            scripts.insert(scripts.index("05-app-roles.sql") + 1, POSTGIS_SCRIPT)
            postgis_applied = True
            print("[drill] A: postgis available — 06-booking-postgis.sql joins the drill verbatim")
        else:
            record_skip(
                f"{POSTGIS_SCRIPT} (postgis init script)",
                "DRILL_PG=system: extension 'postgis' absent from pg_available_extensions "
                "of the system cluster — the postgis packages are not installed "
                "(remediation: `apt install postgis` or `apt install "
                "postgresql-<ver>-postgis-3`, see `apt-cache search postgis`). The rest of "
                "the drill still runs on the system cluster; geo init-script coverage is "
                "EXTERNAL_BLOCKED, not silently passed.")
    else:
        record_skip(
            f"{POSTGIS_SCRIPT} (postgis init script)",
            "pgserver ships core PostgreSQL without contrib modules — postgis cannot "
            "load. Use DRILL_PG=system on a host with the postgis packages installed "
            "for 06 coverage. (Unchanged pre-W42 behavior; now an explicit recorded SKIP.)")

    psql(srv_a, "DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='opendesk')"
                " THEN CREATE ROLE opendesk NOLOGIN; END IF; END $$;")  # docstring adaptation 3
    for name in scripts:
        raw = (INIT / name).read_text()
        # 30 guards its own pgcrypto line; stripping is a no-op for it.
        psql(srv_a, strip_contrib_extensions(raw) if strip_crypto else raw)
        print(f"[drill] A: applied {name}")
    for m in BILLING_MIGRATION_FILES:
        psql(srv_a, "\\c billing\n" + (BILLING_MIGRATIONS / m).read_text())
        print(f"[drill] A: applied billing migration {m}")
    markers = seed_markers(srv_a)
    print(f"[drill] A: seeded markers {markers}")
    counts_a = {db: table_counts(srv_a, db) for db in APP_DBS}

    # ---- pg_dump -Fc every application DB (mirrors backup.sh) ------------
    for db in APP_DBS:
        out = dumps / f"{db}.dump"
        r = subprocess.run(
            [pg_bin("pg_dump"), "-Fc", "-h", socket_dir(srv_a), "-U", "postgres",
             "-d", db, "-f", str(out)],
            capture_output=True, text=True,
        )
        if r.returncode != 0:
            record(f"pg_dump {db}", False, r.stderr.strip()[:300])
            finalize(workdir, started, srv_a, None, args.keep)
            return 1
        print(f"[drill] dumped {db} -> {out.name} ({out.stat().st_size} bytes)")

    # ---- instance B: fresh cluster, roles, pg_restore ---------------------
    print("[drill] booting FRESH instance B ...")
    srv_b = boot_cluster(workdir / "pgdata-B", "B")
    # APP_DBS + kyc (05-app-roles.sql \c's into kyc for the kyc-service role).
    for db in sorted(set(APP_DBS) | {"kyc"}):
        psql(srv_b, f"CREATE DATABASE {db};")
    apply_role_layer(srv_b)
    print("[drill] B: role layer applied (05 + billing-internal + model-registry + postgres defaults bridge)")
    for db in APP_DBS:
        r = subprocess.run(
            [pg_bin("pg_restore"), "--no-owner", "--no-privileges",
             "-h", socket_dir(srv_b), "-U", "postgres", "-d", db, str(dumps / f"{db}.dump")],
            capture_output=True, text=True,
        )
        record(f"pg_restore {db} (--no-owner --no-privileges)", r.returncode == 0,
               r.stderr.strip()[:300] if r.returncode else f"exit 0")

    # ---- assertions -------------------------------------------------------
    counts_b = {db: table_counts(srv_b, db) for db in APP_DBS}
    for db in APP_DBS:
        same = counts_a[db] == counts_b[db]
        record(f"row counts identical: {db}", same,
               "" if same else f"A={counts_a[db]} B={counts_b[db]}")

    for db, table in RLS_ASSERTIONS:
        with psycopg.connect(dsn_for(srv_b, db), autocommit=True) as c:
            pols = [r[0] for r in c.execute(
                "SELECT policyname FROM pg_policies WHERE schemaname='public' AND tablename=%s",
                (table,))]
            forced = c.execute(
                "SELECT relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace"
                " WHERE n.nspname='public' AND c.relname=%s", (table,)).fetchone()
        record(f"RLS policy present post-restore: {db}.{table}", len(pols) > 0, f"policies={pols}")
        record(f"FORCE ROW LEVEL SECURITY post-restore: {db}.{table}",
               bool(forced and forced[0]), f"relforcerowsecurity={forced[0] if forced else None}")

    with psycopg.connect(dsn_for(srv_b, "identity"), autocommit=True) as c:
        n = c.execute("SELECT count(*) FROM tenants WHERE id=%s", (markers["tenant_id"],)).fetchone()[0]
    record("marker row readable: identity.tenants", n == 1)
    with psycopg.connect(dsn_for(srv_b, "booking"), autocommit=True) as c:
        n = c.execute("SELECT count(*) FROM offerings WHERE id=%s", (markers["offering_id"],)).fetchone()[0]
    record("marker row readable: booking.offerings", n == 1)
    with psycopg.connect(dsn_for(srv_b, "conversation"), autocommit=True) as c:
        n = c.execute("SELECT count(*) FROM capture_records WHERE id=%s", (markers["capture_record_id"],)).fetchone()[0]
    record("marker row readable: conversation.capture_records", n == 1)
    with psycopg.connect(dsn_for(srv_b, "billing"), autocommit=True) as c:
        n = c.execute("SELECT count(*) FROM invoices WHERE id=%s", (markers["invoice_id"],)).fetchone()[0]
    record("marker row readable: billing.invoices", n == 1)
    with psycopg.connect(dsn_for(srv_b, "platform"), autocommit=True) as c:
        n = c.execute("SELECT count(*) FROM model_family WHERE name=%s", (markers["model_family"],)).fetchone()[0]
    record("marker row readable: platform.model_family", n == 1)

    # postgis rode the dump/restore cycle (system mode, 06 applied on A):
    # pg_extension on the booking DB of instance B must show it live.
    if postgis_applied:
        with psycopg.connect(dsn_for(srv_b, "booking"), autocommit=True) as c:
            ext = c.execute(
                "SELECT extversion FROM pg_extension WHERE extname='postgis'").fetchone()
        record("postgis: extension live on booking DB post-restore (06 rode pg_dump/pg_restore)",
               ext is not None, f"extversion={ext[0] if ext else None}")

    # RLS still enforced post-restore (W40-6 NULLIF posture).
    app = dsn_for(srv_b, "billing", user="app_billing_login", password="app_billing_dev_password")

    def invoices_visible(guc: str | None) -> int:
        with psycopg.connect(app, autocommit=True) as c:
            if guc is not None:
                c.execute("SELECT set_config('app.tenant_id', %s, false)", (guc,))
            return c.execute("SELECT count(*) FROM invoices").fetchone()[0]

    record("RLS post-restore: wrong app.tenant_id sees 0 invoices",
           invoices_visible(str(uuid.uuid4())) == 0)
    record("RLS post-restore: empty app.tenant_id sees 0 invoices (fail-closed)",
           invoices_visible("") == 0)
    record("RLS post-restore: GUC unset sees 0 invoices (fail-closed)",
           invoices_visible(None) == 0)
    record("RLS post-restore: correct app.tenant_id sees its invoice",
           invoices_visible(markers["tenant_id"]) == 1)

    finalize(workdir, started, srv_a, srv_b, args.keep)
    failed = [r for r in RESULTS if not r["ok"]]
    print(f"[drill] {'OK' if not failed else 'FAILED'}: {len(RESULTS)-len(failed)}/{len(RESULTS)} checks passed"
          + (f"; {len(SKIPS)} explicit SKIP(s)" if SKIPS else ""))
    return 0 if not failed else 1


def finalize(workdir: Path, started: float, srv_a, srv_b, keep: bool) -> None:
    summary = {
        "results": RESULTS,
        "skips": SKIPS,
        "drill_pg": DRILL_PG,
        "wall_time_s": round(time.time() - started, 2),
        "pg_bin_dir": str(Path(pg_bin("pg_dump")).parent) if (DRILL_PG != "system" or system_pg_bindir()) else None,
    }
    out = workdir / "drill-summary.json"
    out.write_text(json.dumps(summary, indent=2))
    print(f"[drill] summary written: {out}")
    for srv in (srv_a, srv_b):
        if srv is not None:
            srv.cleanup()
    if not keep:
        shutil.rmtree(workdir / "pgdata-A", ignore_errors=True)
        shutil.rmtree(workdir / "pgdata-B", ignore_errors=True)
        if DRILL_PG == "system":
            shutil.rmtree(workdir / "sock-A", ignore_errors=True)
            shutil.rmtree(workdir / "sock-B", ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
