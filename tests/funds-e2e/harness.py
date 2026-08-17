#!/usr/bin/env python3
"""funds-e2e — public-interface funds flow harness vs REAL Postgres.

REAL everything: one embedded-Postgres cluster (pgserver == full PostgreSQL
16 binaries, unix-socket only), REAL service binaries as subprocesses, REAL
HMAC-SHA512 webhook signatures. No mocks, no fakes, no Dapr shim.

Services (ports are harness-local, overridable via env):
  identity-service  (Go)   PORT=17001  DATABASE_URL -> pgserver `identity`
                           INDUSTRIES_DIR=<repo>/industries
  booking-service   (Go)   PORT=17002  DATABASE_URL -> pgserver `booking`
                           AUTHZ_DISABLED=true CONSUMER_ENABLED=false
                           IDENTITY_BASE_URL=http://127.0.0.1:17001
                           (SPEC-W42: direct-GET tenant resolution fallback;
                           booking write path no longer needs Dapr)
  billing-engine    (Rust) PORT=17012  DATABASE_URL -> pgserver `billing`
                           BILLING_INTERNAL_TOKEN=<random hex per run>
                           BILLING_STATIC_ACCOUNT=OPENDESK/0123456789
                           BILLING_MERCHANT_NAME='OPENDESK DEMO'
                           KAFKA_CONSUMER_ENABLED=false
  payments-service  (Rust) PORT=17004  LEDGER_IMPL=sim MOJALOOP_ALLOW_SIM=true
                           KAFKA_CONSUMER_ENABLED=false
                           --- OR (SPEC-W42 real-ledger mode, TB_BINARY set) ---
                           LEDGER_IMPL=tigerbeetle TB_ADDRESSES=127.0.0.1:3000
                           TB_CLUSTER_ID=0 PLATFORM_FEE_BPS=250, built with
                           --features tb-live, against a REAL single-replica
                           TigerBeetle started by the harness.

Ledger modes (SPEC-W42 Coder H):
  * SIM mode (default; TB_BINARY unset): unchanged W41 behavior — payments
    runs the in-memory sim ledger.
  * REAL-LEDGER mode (TB_BINARY=/path/to/tigerbeetle binary): the harness
    runs `tigerbeetle format --cluster=0 --replica=0 --replica-count=1
    <workdir>/0_0.tigerbeetle` and `tigerbeetle start
    --addresses=$TB_ADDRESS (default 127.0.0.1:3000) --development`, waits
    for the TCP port, builds payments with `cargo build --locked
    --features tb-live` (PAYMENTS_BIN still honored — it MUST be a tb-live
    build or the service fails closed at boot), and pre-creates the five
    ledger accounts (tenant deposits/revenue + platform
    fees/clearing/payouts, ids derived exactly like ledger::account_id =
    uuid v5 URL-namespace) through a throwaway fixture crate compiled into
    the workdir that links the SAME pinned tigerbeetle-unofficial
    0.8.0+0.16.28 client crate the service uses. The FULL assertion suite
    runs in this mode too, plus real-ledger assertions: the hold is visible
    as pending on the real ledger, capture moves funds (balance deltas via
    GET /v1/accounts/{t}/balance match the capture response amounts
    exactly), and capture replay leaves every balance counter
    byte-identical (TigerBeetle `exists` results, no double-post).
    Single-replica --development semantics: no replication, no storage
    fault tolerance — this proves client/server correctness and the money
    path, not HA. The tb-live build needs libclang (bindgen) and downloads
    the Zig toolchain at build time (network). If TB_BINARY is unset there
    is no silent fallback claim — the harness runs sim mode and says so.

Binary resolution (per service, in order):
  1. env var IDENTITY_BIN / BOOKING_BIN / BILLING_BIN / PAYMENTS_BIN
  2. harness builds it: `go build ./cmd/server` for the Go services
     (Go from $GO_BIN, /tmp/go/bin/go, or PATH), `cargo build --locked` for
     the Rust services (CARGO_TARGET_DIR is redirected into the workdir so
     the source tree is NEVER written to — safe when the repo is a read-mostly
     /mnt mirror).

Schema bootstrap:
  * ALL databases referenced by the applied init-scripts are pre-created
    (identity, booking, billing, conversation, knowledge, kyc) —
    05-app-roles.sql \\c's into conversation/knowledge/kyc as well.
  * `identity` + `booking` DBs get infra/postgres/init-scripts/01 and 02 via
    pgserver's server.psql with ONLY the `CREATE EXTENSION IF NOT EXISTS
    pgcrypto;` line stripped — pgserver ships core PostgreSQL WITHOUT contrib
    modules (no pgcrypto); gen_random_uuid() is core since PG13 and is all
    those schemas use pgcrypto for.
  * `billing` schema is NOT applied manually: billing-engine self-applies
    migrations 0001-0004 at boot (src/main.rs:142-159), including the RLS
    migration 0002.
  * 05-app-roles.sql IS applied (with a NOLOGIN `opendesk` role pre-created,
    because 05 references the docker bootstrap superuser in
    ALTER DEFAULT PRIVILEGES FOR ROLE opendesk while pgserver's bootstrap
    superuser is `postgres`) so the least-privilege app_billing_login exists
    for the RLS adversarial probes.

Booking write path (SPEC-W42, Coder G contract):
  booking-service resolves tenants via DIRECT HTTP GET
  {IDENTITY_BASE_URL}/v1/tenants/{slug} (no Dapr), so both
  POST /v1/bookings (Bearer + X-Tenant-Slug) and
  POST /public/sites/{slug}/bookings commit the booking row + outbox row
  atomically and return 2xx. Degraded-mode honesty (asserted, not hidden):
  with no Temporal the booking STAYS status=pending (no saga confirms it)
  and with DAPR_HOST pointed at a dead port the outbox dispatcher never
  publishes, so outbox rows STAY sent_at IS NULL. The harness asserts
  exactly that posture. Seeding (SQL, superuser, same fixture-data pattern
  as the pre-W42 sites/offering rows): sites row, one offering, one team
  member, and all-week availability rules (the create path enforces
  availability via bookingops.checkSlot). Idempotency: replaying the same
  idempotency_key returns the original booking and the bookings row count
  for the key stays exactly 1.

Dapr-free scope (verified from source, see README.md):
  * payments-service publish_event is best-effort (main.rs:61); DAPR_HOST is
    pointed at 127.0.0.1:1 so failures are instant, logged, and never fail
    the money path. The booking outbox dispatcher likewise degrades
    (outbox/dispatcher.go: publish failure -> row stays sent_at NULL).
  * identity-service tenant side effects (Keycloak/Permify/Dapr) are
    best-effort by design (createTenant logs and continues).

Billing two-phase payment flow (verified constraint, see README):
  payment-link with PAYSTACK_SECRET_KEY set calls the LIVE Paystack API;
  the webhook handler 503s when it is UNSET. One process cannot exercise
  both offline. The harness therefore runs billing-engine in static mode for
  generate/issue/payment-link, then RESTARTS it with a test
  PAYSTACK_SECRET_KEY for the webhook phase (webhook verification is local
  HMAC only — no outbound network). The invoice row survives the restart
  (Postgres is the state).

Asserted flow:
  identity  POST /v1/tenants -> 201
  booking   site/offering/team-member/availability seeded via SQL;
            GET /public/sites/{slug}[/context|/offerings] -> 200;
            POST /v1/bookings (authed) -> 201 status=pending; outbox row
            sent_at IS NULL; idempotency_key replay -> same booking, exactly
            1 row; POST /public/sites/{slug}/bookings -> 201, same asserts
  billing   PUT /v1/rate-cards/{t} -> 200; usage seeded via SQL (the usage
            ingest path is Kafka-only; KAFKA_CONSUMER_ENABLED=false);
            POST /v1/invoices/generate -> 201; /issue -> 200;
            /payment-link -> 200 (mode=static)
  webhook   restart with test secret; WRONG signature -> 401 (proves the
            HMAC is really enforced); REAL HMAC-SHA512(secret, body) hex ->
            POST /webhooks/paystack -> 200 {"status":"paid"}; invoice paid;
            superuser SQL asserts: ledger_transfers has the deterministic
            invoice_paid transfer (uuid v5 "billing-paid:{invoice_id}",
            code 202) AND exactly one billing_outbox InvoicePaid row (the
            outbox row commits IN THE SAME TX as the paid transition —
            routes.rs RS-001); REPLAY identical webhook -> 200
            {"status":"already_paid"}, still exactly 1 ledger transfer and
            1 outbox row (idempotency).
  payments  POST /v1/deposits (hold, idempotency_key) -> 201; replay hold
            same key -> same deposit_id; POST /v1/deposits/{id}/capture ->
            200; replay capture -> identical posted amounts, no double-post
            (deterministic capture id); GET /v1/accounts/{t}/balance -> 200.
            REAL-LEDGER mode adds: pending hold visible on real TB,
            capture balance deltas == capture response amounts, replay
            leaves balances byte-identical.
  RLS       connect to `billing` as app_billing_login: wrong app.tenant_id
            -> 0 invoices; app.tenant_id='' -> 0 (W40-6 NULLIF fail-closed);
            correct tenant -> sees exactly its own rows.

Every HTTP call is timed (time.perf_counter) and all timings are dumped to
<workdir>/timings/funds-e2e-timings.json for tests/perf/aggregate.py.
Set FUNDS_E2E_PERF_ITERS=N (default 1) to repeat the idempotent hot calls
(webhook replay, hold+capture pairs, invoice generate over distinct
periods, booking creates over staggered slots) N times for p50/p99 and
sustained-throughput statistics.

Usage:
  python3 tests/funds-e2e/harness.py [--workdir /tmp/funds-e2e]
  TB_BINARY=/path/to/tigerbeetle python3 tests/funds-e2e/harness.py  # real ledger
Deps: pip install pgserver==0.1.4 psycopg ; Go toolchain for the Go builds;
cargo for the Rust builds (or pre-built binaries via *_BIN env vars).
Exit code 0 only if every check passes.
"""

from __future__ import annotations

import argparse
import hashlib
import hmac as hmac_mod
import json
import os
import secrets
import shutil
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path

os.environ.setdefault("XDG_RUNTIME_DIR", "/tmp/xdg")
Path(os.environ["XDG_RUNTIME_DIR"]).mkdir(parents=True, exist_ok=True)
os.chmod(os.environ["XDG_RUNTIME_DIR"], 0o700)

import pgserver  # noqa: E402
import psycopg  # noqa: E402
import psycopg.conninfo  # noqa: E402

REPO_ROOT = Path(os.environ.get("OPENDESK_REPO", Path(__file__).resolve().parents[2]))
INIT = REPO_ROOT / "infra/postgres/init-scripts"

PORTS = {
    "identity": int(os.environ.get("FUNDS_E2E_IDENTITY_PORT", "17001")),
    "booking": int(os.environ.get("FUNDS_E2E_BOOKING_PORT", "17002")),
    "billing": int(os.environ.get("FUNDS_E2E_BILLING_PORT", "17012")),
    "payments": int(os.environ.get("FUNDS_E2E_PAYMENTS_PORT", "17004")),
}
PERF_ITERS = int(os.environ.get("FUNDS_E2E_PERF_ITERS", "1"))

# SPEC-W42 real-ledger mode: TB_BINARY points at a tigerbeetle 0.16.28
# binary. Unset -> sim mode (unchanged W41 behavior).
TB_BINARY = os.environ.get("TB_BINARY", "")
TB_MODE = bool(TB_BINARY)
TB_ADDRESS = os.environ.get("TB_ADDRESS", "127.0.0.1:3000")
TB_PORT = int(TB_ADDRESS.rsplit(":", 1)[1])
# Pinned client crate (services/payments-service/Cargo.lock). The fixture
# crate the harness generates into the workdir pins the SAME version so the
# account-provisioning client and the service can never drift on the wire
# protocol.
TB_CLIENT_CRATE = 'tigerbeetle-unofficial = "=0.8.0+0.16.28"'
PLATFORM_FEE_BPS = 250  # payments-service default (config.rs); explicit in TB mode

RESULTS: list[dict] = []
TIMINGS: list[dict] = []
PROCS: list[subprocess.Popen] = []


def record(name: str, ok: bool, detail: str = "") -> None:
    RESULTS.append({"check": name, "ok": ok, "detail": detail})
    print(f"{'PASS' if ok else 'FAIL'}  {name}" + (f"  — {detail}" if detail else ""), flush=True)


# ---------------------------------------------------------------------------
# HTTP helper (stdlib; every call timed for tests/perf)
# ---------------------------------------------------------------------------

def http(call: str, method: str, url: str, body: dict | bytes | None = None,
         headers: dict[str, str] | None = None, timeout: float = 15.0) -> tuple[int, bytes]:
    data: bytes | None
    hdrs = dict(headers or {})
    if isinstance(body, dict):
        data = json.dumps(body).encode()
        hdrs.setdefault("Content-Type", "application/json")
    else:
        data = body
    req = urllib.request.Request(url, data=data, headers=hdrs, method=method)
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            status, payload = resp.status, resp.read()
    except urllib.error.HTTPError as e:
        status, payload = e.code, e.read()
    ms = (time.perf_counter() - t0) * 1000.0
    TIMINGS.append({"call": call, "method": method, "url": url, "status": status, "ms": round(ms, 2)})
    return status, payload


def http_json(call: str, method: str, url: str, body=None, headers=None):
    status, payload = http(call, method, url, body, headers)
    try:
        return status, json.loads(payload)
    except Exception:
        return status, payload.decode(errors="replace")


def wait_ready(name: str, port: int, proc: subprocess.Popen, timeout_s: float = 60.0) -> bool:
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        if proc.poll() is not None:
            return False
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=2) as r:
                if r.status == 200:
                    return True
        except Exception:
            time.sleep(0.3)
    return False


def wait_tcp(port: int, proc: subprocess.Popen, timeout_s: float = 60.0) -> bool:
    """Wait until a raw TCP port accepts connections (TigerBeetle has no
    /healthz; the fixture client's create-accounts is the real readiness
    probe on top of this)."""
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        if proc.poll() is not None:
            return False
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=2):
                return True
        except OSError:
            time.sleep(0.2)
    return False


# ---------------------------------------------------------------------------
# Builds / process management
# ---------------------------------------------------------------------------

def find_tool(candidates: list[str]) -> str | None:
    for c in candidates:
        if c and os.path.isfile(c) and os.access(c, os.X_OK):
            return c
        if c:
            w = shutil.which(c)
            if w:
                return w
    return None


def build_or_find(service: str, env_var: str, workdir: Path,
                  cargo_features: list[str] | None = None) -> str:
    pre = os.environ.get(env_var)
    if pre:
        if not os.path.isfile(pre):
            raise RuntimeError(f"{env_var}={pre} does not exist")
        return pre
    out = workdir / "bin" / service
    out.parent.mkdir(parents=True, exist_ok=True)
    if service in ("identity-service", "booking-service"):
        go = find_tool([os.environ.get("GO_BIN", ""), "/tmp/go/bin/go", "go"])
        if not go:
            raise RuntimeError(f"no Go toolchain found to build {service} (set {env_var} or GO_BIN)")
        env = dict(os.environ)
        env.setdefault("GOFLAGS", "-mod=readonly")
        env.setdefault("GOCACHE", str(workdir / "gocache"))
        env.setdefault("GOMODCACHE", str(workdir / "gomodcache"))
        cmd = [go, "build", "-o", str(out), "./cmd/server"]
        cwd = REPO_ROOT / "services" / service
    else:
        cargo = find_tool([os.environ.get("CARGO_BIN", ""), str(Path.home() / ".cargo/bin/cargo"), "cargo"])
        if not cargo:
            raise RuntimeError(f"no cargo found to build {service} (set {env_var} or CARGO_BIN)")
        env = dict(os.environ)
        # CARGO_TARGET_DIR redirected into the workdir: the source tree is
        # never written to (safe when REPO_ROOT is the /mnt mirror).
        env["CARGO_TARGET_DIR"] = str(workdir / "cargo-target" / service)
        cmd = [cargo, "build", "--locked"]
        if cargo_features:
            cmd += ["--features", ",".join(cargo_features)]
        cwd = REPO_ROOT / "services" / service
    print(f"[harness] building {service}: {' '.join(cmd)} (cwd={cwd})", flush=True)
    t0 = time.time()
    log = open(workdir / "logs" / f"build-{service}.log", "w")
    r = subprocess.run(cmd, cwd=cwd, env=env, stdout=log, stderr=subprocess.STDOUT)
    log.close()
    print(f"[harness] build {service} exit={r.returncode} ({time.time()-t0:.0f}s)", flush=True)
    if r.returncode != 0:
        raise RuntimeError(f"build failed for {service}; see logs/build-{service}.log")
    if service in ("billing-engine", "payments-service"):
        target = workdir / "cargo-target" / service / "debug" / service
        if not target.exists():
            raise RuntimeError(f"expected cargo artifact missing: {target}")
        return str(target)
    if not out.exists():
        raise RuntimeError(f"expected go artifact missing: {out}")
    return str(out)


def start_service(name: str, binary: str, port: int, env_extra: dict[str, str], workdir: Path,
                  args: list[str] | None = None) -> subprocess.Popen:
    env = dict(os.environ)
    env.update(env_extra)
    env["PORT"] = str(port)  # ignored by non-HTTP processes (tigerbeetle)
    log = open(workdir / "logs" / f"{name}.log", "a")
    p = subprocess.Popen([binary] + list(args or []), env=env, stdout=log, stderr=subprocess.STDOUT,
                         start_new_session=True)
    PROCS.append(p)
    return p


def stop_all() -> None:
    for p in PROCS:
        try:
            os.killpg(p.pid, signal.SIGTERM)
        except Exception:
            pass
    deadline = time.time() + 8
    for p in PROCS:
        while p.poll() is None and time.time() < deadline:
            time.sleep(0.1)
        if p.poll() is None:
            try:
                os.killpg(p.pid, signal.SIGKILL)
            except Exception:
                pass


# ---------------------------------------------------------------------------
# TigerBeetle real-ledger mode (SPEC-W42): server + account fixture
# ---------------------------------------------------------------------------

# Throwaway fixture crate generated into the WORKDIR (never the repo). It
# links the same pinned tigerbeetle-unofficial client crate as
# payments-service and exists because the service intentionally exposes NO
# HTTP endpoint for account creation (ledger/mod.rs create_accounts is
# internal); the sim ledger auto-creates accounts on hold, real TigerBeetle
# correctly refuses transfers to unknown accounts. Account ids replicate
# ledger::account_id (uuid v5, URL namespace) so the fixture and the service
# agree byte-for-byte.
TB_FIXTURE_RS = r"""
use tigerbeetle_unofficial as tb;
use uuid::Uuid;

// Mirrors services/payments-service/src/ledger/mod.rs account_id().
fn account_id(name: &str) -> u128 {
    Uuid::new_v5(&Uuid::NAMESPACE_URL, name.as_bytes()).as_u128()
}

const LEDGER_ID: u32 = 1;

fn account_defs(tenant_id: &str) -> Vec<(String, u16)> {
    vec![
        (format!("tenant:{tenant_id}:deposits"), 10),
        (format!("tenant:{tenant_id}:revenue"), 20),
        ("platform:fees".to_string(), 30),
        ("platform:clearing".to_string(), 31),
        ("platform:payouts".to_string(), 32),
    ]
}

#[tokio::main]  // full multi-thread runtime: the TB client uses internal timers/spawns
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let args: Vec<String> = std::env::args().collect();
    let mode = args.get(1).map(String::as_str).unwrap_or("");
    let tenant_id = args.get(2).cloned().unwrap_or_default();
    let addresses = std::env::var("TB_ADDRESSES").unwrap_or_else(|_| "127.0.0.1:3000".into());
    let cluster_id: u128 = std::env::var("TB_CLUSTER_ID").unwrap_or_else(|_| "0".into()).parse()?;
    let client = tb::Client::new(cluster_id, addresses)?;
    match mode {
        "create-accounts" => {
            let defs = account_defs(&tenant_id);
            let accounts: Vec<tb::Account> = defs
                .iter()
                .map(|(name, code)| tb::Account::new(account_id(name), LEDGER_ID, *code))
                .collect();
            match client.create_accounts(accounts).await {
                Ok(()) => {}
                // `exists` = idempotent re-creation (same rule as the service).
                Err(tb::error::CreateAccountsError::Api(api))
                    if api.as_slice().iter().all(|e| matches!(e.kind(), tb::error::CreateAccountErrorKind::Exists)) => {}
                Err(e) => return Err(format!("create_accounts failed: {e}").into()),
            }
            println!("ok");
        }
        "lookup" => {
            // Diagnostic cross-check for the service's balance endpoint.
            let names = [
                format!("tenant:{tenant_id}:deposits"),
                format!("tenant:{tenant_id}:revenue"),
            ];
            let ids: Vec<u128> = names.iter().map(|n| account_id(n)).collect();
            let accounts = client.lookup_accounts(ids).await?;
            for (i, a) in accounts.iter().enumerate() {
                println!(
                    "{{\"account\":\"{}\",\"debits_pending\":{},\"credits_pending\":{},\"debits_posted\":{},\"credits_posted\":{}}}",
                    names[i],
                    a.debits_pending(),
                    a.credits_pending(),
                    a.debits_posted(),
                    a.credits_posted()
                );
            }
        }
        other => return Err(format!("usage: tb-fixture create-accounts|lookup <tenant_id> (got '{other}')").into()),
    }
    Ok(())
}
"""

TB_FIXTURE_TOML = """# Generated by tests/funds-e2e/harness.py into the workdir (never the repo).
# Pins the SAME TigerBeetle client crate as services/payments-service
# (Cargo.lock: tigerbeetle-unofficial 0.8.0+0.16.28).
[package]
name = "tb-fixture"
version = "0.1.0"
edition = "2021"

[dependencies]
""" + TB_CLIENT_CRATE + """
uuid = { version = "1", features = ["v5"] }
tokio = { version = "1", features = ["macros", "rt-multi-thread", "time", "sync"] }

[[bin]]
name = "tb-fixture"
path = "main.rs"
"""


def tb_format_and_start(workdir: Path) -> subprocess.Popen:
    """Format a fresh single-replica TB data file and start the server in
    --development mode (no replication — this mode proves client/server
    correctness of the money path, not HA)."""
    if not os.path.isfile(TB_BINARY) or not os.access(TB_BINARY, os.X_OK):
        raise RuntimeError(f"TB_BINARY={TB_BINARY} is not an executable file")
    datafile = workdir / "0_0.tigerbeetle"
    if datafile.exists():
        datafile.unlink()  # fresh ledger per run: balance deltas are exact
    fmt = subprocess.run(
        [TB_BINARY, "format", "--cluster=0", "--replica=0", "--replica-count=1", str(datafile)],
        capture_output=True, text=True)
    if fmt.returncode != 0:
        raise RuntimeError(f"tigerbeetle format failed: {fmt.stderr.strip()[:400]}")
    proc = start_service("tigerbeetle", TB_BINARY, TB_PORT, {}, workdir,
                         args=["start", f"--addresses={TB_ADDRESS}", "--development", str(datafile)])
    ok = wait_tcp(TB_PORT, proc)
    record(f"tigerbeetle: format + start --development, {TB_ADDRESS} accepting",
           ok, "" if ok else "see logs/tigerbeetle.log")
    if not ok:
        raise RuntimeError("tigerbeetle did not start; see logs/tigerbeetle.log")
    return proc


def tb_fixture_bin(workdir: Path) -> str:
    """Build the throwaway account fixture crate (workdir-only)."""
    crate_dir = workdir / "tb-fixture"
    crate_dir.mkdir(parents=True, exist_ok=True)
    (crate_dir / "Cargo.toml").write_text(TB_FIXTURE_TOML)
    (crate_dir / "main.rs").write_text(TB_FIXTURE_RS)
    cargo = find_tool([os.environ.get("CARGO_BIN", ""), str(Path.home() / ".cargo/bin/cargo"), "cargo"])
    if not cargo:
        raise RuntimeError("no cargo found to build the tb-fixture crate (set CARGO_BIN)")
    env = dict(os.environ)
    env["CARGO_TARGET_DIR"] = str(workdir / "cargo-target" / "tb-fixture")
    print("[harness] building tb-fixture (pinned tigerbeetle-unofficial client)", flush=True)
    with open(workdir / "logs" / "build-tb-fixture.log", "w") as log:
        r = subprocess.run([cargo, "build"], cwd=crate_dir, env=env, stdout=log,
                           stderr=subprocess.STDOUT)
    if r.returncode != 0:
        raise RuntimeError("tb-fixture build failed; see logs/build-tb-fixture.log")
    target = workdir / "cargo-target" / "tb-fixture" / "debug" / "tb-fixture"
    if not target.exists():
        raise RuntimeError(f"expected tb-fixture artifact missing: {target}")
    return str(target)


def tb_create_accounts(fixture: str, tenant_id: str) -> bool:
    """Create the five ledger accounts on the REAL TigerBeetle (idempotent;
    `exists` per-item results are success, mirroring the service client).
    Doubles as the real readiness probe — a few retries cover the gap
    between TCP-accept and VSR request serving."""
    env = dict(os.environ)
    env["TB_ADDRESSES"] = TB_ADDRESS
    env["TB_CLUSTER_ID"] = "0"
    last = ""
    for _ in range(20):
        r = subprocess.run([fixture, "create-accounts", tenant_id],
                           env=env, capture_output=True, text=True, timeout=30)
        if r.returncode == 0:
            return True
        last = (r.stderr or r.stdout).strip()[:300]
        time.sleep(0.5)
    print(f"[harness] tb-fixture create-accounts failing: {last}", flush=True)
    return False


# ---------------------------------------------------------------------------
# SQL helpers
# ---------------------------------------------------------------------------

def strip_pgcrypto(sql: str) -> str:
    """Strip ONLY `CREATE EXTENSION IF NOT EXISTS pgcrypto;` — pgserver ships
    core PostgreSQL WITHOUT contrib modules; gen_random_uuid() is core PG13+
    and is all these schemas use pgcrypto for."""
    out = []
    for line in sql.splitlines():
        if line.strip().lower().startswith("create extension if not exists pgcrypto"):
            out.append("-- [funds-e2e: stripped — pgserver has no contrib pgcrypto] " + line)
        else:
            out.append(line)
    return "\n".join(out)


def dsn_for(server, database: str, user: str | None = None, password: str | None = None) -> str:
    info = psycopg.conninfo.conninfo_to_dict(server.get_uri(database=database))
    if user:
        info["user"] = user
    if password:
        info["password"] = password
    return psycopg.conninfo.make_conninfo(**info)


def sqlx_dsn_for(server, database: str) -> str:
    """DSN for the Rust/sqlx services (billing-engine). pgserver's native URI
    `postgresql://postgres:@/<db>?host=/sock/dir` has an EMPTY-PASSWORD
    userinfo (`postgres:@`) which the url crate inside sqlx 0.7.4 rejects —
    billing boots 30 retries then Error: Configuration(EmptyHost). The proven
    working form percent-encodes the unix-socket dir into the URI host
    position: `postgresql://postgres@%2Ftmp%2F...%2Fpgdata/<db>` (verifier
    V-Harness, W41: boots all 4 services). The Go/pgx services keep the
    `?host=` form (pgx parses it fine in smokes)."""
    info = psycopg.conninfo.conninfo_to_dict(server.get_uri(database=database))
    host = urllib.parse.quote(info["host"], safe="")
    return f"postgresql://{info['user']}@{host}/{database}"


# ---------------------------------------------------------------------------
# Main flow
# ---------------------------------------------------------------------------

PG_SERVER = None  # set by run(); cleaned up in main's finally


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--workdir", default=os.environ.get("FUNDS_E2E_WORKDIR", "/tmp/funds-e2e"))
    args = ap.parse_args()
    workdir = Path(args.workdir)
    (workdir / "logs").mkdir(parents=True, exist_ok=True)
    (workdir / "timings").mkdir(parents=True, exist_ok=True)
    started = time.time()
    try:
        return run(workdir, started)
    finally:
        stop_all()
        if PG_SERVER is not None:
            PG_SERVER.cleanup()


def balance_map(base_url: str, tenant_id: str, call: str) -> dict[str, dict]:
    """GET /v1/accounts/{t}/balance -> {account_name: AccountBalance}."""
    status, bal = http_json(call, "GET", f"{base_url}/v1/accounts/{tenant_id}/balance")
    if status != 200 or not isinstance(bal, dict):
        return {}
    return {a["account"]: a for a in bal.get("accounts") or []}


def run(workdir: Path, started: float) -> int:
    print(f"[harness] repo={REPO_ROOT} workdir={workdir} perf_iters={PERF_ITERS} "
          f"ledger={'tigerbeetle(REAL, single-replica --development)' if TB_MODE else 'sim'}",
          flush=True)

    # ---- Postgres cluster -------------------------------------------------
    global PG_SERVER
    srv = pgserver.get_server(str(workdir / "pgdata"))
    PG_SERVER = srv
    # Create EVERY database the applied init-scripts \c into BEFORE applying
    # them: 01 -> booking, 02 -> identity, 05-app-roles.sql -> booking,
    # conversation, knowledge, billing, kyc (psql exits 3 on a missing \c
    # target under ON_ERROR_STOP).
    for db in ("identity", "booking", "billing", "conversation", "knowledge", "kyc"):
        try:
            srv.psql(f"CREATE DATABASE {db};")
        except subprocess.CalledProcessError:
            pass  # pre-existing on reused workdir
    # opendesk role so 05-app-roles.sql's FOR ROLE opendesk applies verbatim
    # (pgserver bootstrap superuser is `postgres`, docker's is `opendesk`).
    srv.psql("DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='opendesk')"
             " THEN CREATE ROLE opendesk NOLOGIN; END IF; END $$;")
    for script, why in (("01-booking-schema.sql", "booking schema"),
                        ("02-identity-schema.sql", "identity schema"),
                        ("05-app-roles.sql", "least-privilege app roles (RLS adversarial probes)")):
        srv.psql("\\set ON_ERROR_STOP on\n" + strip_pgcrypto((INIT / script).read_text()))
        print(f"[harness] applied {script} ({why})", flush=True)
    # NOTE: billing schema is NOT applied here — billing-engine self-applies
    # migrations 0001-0004 at boot (src/main.rs:142-159), which is part of
    # what this harness verifies.

    base = {name: f"http://127.0.0.1:{port}" for name, port in PORTS.items()}
    billing_token = secrets.token_hex(16)
    paystack_secret = "e2e-test-secret-" + secrets.token_hex(16)

    # ---- TigerBeetle real-ledger mode: server before payments build -------
    tb_fixture = None
    if TB_MODE:
        tb_format_and_start(workdir)
        tb_fixture = tb_fixture_bin(workdir)

    # ---- boot services ----------------------------------------------------
    bins = {
        "identity": build_or_find("identity-service", "IDENTITY_BIN", workdir),
        "booking": build_or_find("booking-service", "BOOKING_BIN", workdir),
        "billing": build_or_find("billing-engine", "BILLING_BIN", workdir),
        "payments": build_or_find("payments-service", "PAYMENTS_BIN", workdir,
                                  cargo_features=["tb-live"] if TB_MODE else None),
    }

    identity_env = {
        "DATABASE_URL": srv.get_uri(database="identity"),
        "INDUSTRIES_DIR": str(REPO_ROOT / "industries"),
        # No Keycloak/Permify/Dapr in this sandbox: point at a dead loopback
        # port so the best-effort side effects fail FAST (they are logged,
        # never fatal — httpapi/server.go createTenant).
        "KEYCLOAK_URL": "http://127.0.0.1:1",
        "PERMIFY_URL": "http://127.0.0.1:1",
        "DAPR_HOST": "127.0.0.1",
        "DAPR_HTTP_PORT": "1",
    }
    booking_env = {
        "DATABASE_URL": srv.get_uri(database="booking"),
        "AUTHZ_DISABLED": "true",
        "CONSUMER_ENABLED": "false",
        "DAPR_HOST": "127.0.0.1",
        "DAPR_HTTP_PORT": "1",
        "TEMPORAL_HOST_PORT": "127.0.0.1:1",
        # SPEC-W42 (Coder G contract): direct-GET tenant resolution —
        # TenantResolver.BySlug issues GET {IDENTITY_BASE_URL}/v1/tenants/{slug}
        # against the REAL identity-service instead of Dapr service invocation.
        "IDENTITY_BASE_URL": base["identity"],
    }
    billing_env_static = {
        "DATABASE_URL": sqlx_dsn_for(srv, "billing"),
        "BILLING_INTERNAL_TOKEN": billing_token,
        "BILLING_STATIC_ACCOUNT": "OPENDESK/0123456789",
        "BILLING_MERCHANT_NAME": "OPENDESK DEMO",
        "KAFKA_CONSUMER_ENABLED": "false",
        "BILLING_LEDGER_IMPL": "postgres",  # durable ledger: asserted via SQL
    }
    payments_env = {
        "LEDGER_IMPL": "sim",
        "MOJALOOP_ALLOW_SIM": "true",
        "KAFKA_CONSUMER_ENABLED": "false",
        "DAPR_HOST": "127.0.0.1",
        "DAPR_HTTP_PORT": "1",
    }
    if TB_MODE:
        payments_env.update({
            "LEDGER_IMPL": "tigerbeetle",
            "TB_ADDRESSES": TB_ADDRESS,
            "TB_CLUSTER_ID": "0",
            # Same value as the service default (config.rs); made explicit so
            # the fee-split balance assertions below are self-documenting.
            "PLATFORM_FEE_BPS": str(PLATFORM_FEE_BPS),
        })

    procs = {}
    for name, binary, env in (
        ("identity", bins["identity"], identity_env),
        ("booking", bins["booking"], booking_env),
        ("billing", bins["billing"], billing_env_static),
        ("payments", bins["payments"], payments_env),
    ):
        procs[name] = start_service(name, binary, PORTS[name], env, workdir)
    for name in ("identity", "booking", "billing", "payments"):
        ok = wait_ready(name, PORTS[name], procs[name])
        record(f"service up: {name} /healthz", ok,
               "" if ok else f"see logs/{name}.log")
        if not ok:
            return finalize(workdir, started)

    # ---- 1. identity: create tenant --------------------------------------
    slug = "e2e-" + secrets.token_hex(4)
    status, tenant_resp = http_json("identity.create_tenant", "POST",
                                    f"{base['identity']}/v1/tenants",
                                    {"slug": slug, "name": "E2E Demo Co"})
    record("identity: POST /v1/tenants -> 201", status == 201, f"status={status} body={str(tenant_resp)[:200]}")
    if status != 201:
        return finalize(workdir, started)
    status, tenant_ctx = http_json("identity.get_tenant", "GET",
                                   f"{base['identity']}/v1/tenants/{slug}")
    tenant_id = tenant_ctx.get("id") if isinstance(tenant_ctx, dict) else None
    record("identity: GET /v1/tenants/{slug} returns id", status == 200 and bool(tenant_id),
           f"status={status} id={tenant_id}")
    if not tenant_id:
        return finalize(workdir, started)

    # ---- 1b. real-ledger mode: create the TB accounts for this tenant -----
    # The service has no account-creation HTTP endpoint (by design; the sim
    # ledger auto-creates on hold, real TigerBeetle refuses transfers to
    # unknown accounts), so the harness provisions them through the pinned
    # client crate via the workdir fixture.
    if TB_MODE:
        ok = tb_create_accounts(tb_fixture, tenant_id)
        record("tigerbeetle: tenant+platform accounts created on the REAL ledger "
               "(pinned client crate, exists=idempotent)", ok)
        if not ok:
            return finalize(workdir, started)

    # ---- 2. booking: site + catalog seeding, public reads -----------------
    # The public site row is normally seeded by the TenantOnboardingWorkflow
    # (Temporal + Dapr — EXTERNAL_BLOCKED here); seed the `sites` row, one
    # offering, one team member and all-week availability rules directly via
    # SQL as harness fixture data. The sites table is bootstrap DDL without
    # RLS (store/store.go ensureSitesTable); the rest are RLS tables seeded
    # as the bootstrap superuser.
    with psycopg.connect(dsn_for(srv, "booking"), autocommit=True) as c:
        c.execute(
            "INSERT INTO sites (tenant_id, tenant_slug, slug, display_name, published)"
            " VALUES (%s, %s, %s, 'E2E Demo Co', TRUE)",
            (tenant_id, slug, slug),
        )
        offering_id = str(c.execute(
            "INSERT INTO offerings (tenant_id, name, duration_min, price_cents)"
            " VALUES (%s, 'E2E Consultation', 30, 5000) RETURNING id",
            (tenant_id,),
        ).fetchone()[0])
        member_id = str(c.execute(
            "INSERT INTO team_members (tenant_id, name, email)"
            " VALUES (%s, 'E2E Staff', 'e2e-staff@example.com') RETURNING id",
            (tenant_id,),
        ).fetchone()[0])
        # bookingops.Create enforces availability (checkSlot -> availability
        # .Covers): one all-day rule per weekday keeps any slot bookable.
        for wd in range(7):
            c.execute(
                "INSERT INTO availability_rules (tenant_id, team_member_id, weekday,"
                " start_min, end_min) VALUES (%s, %s, %s, 0, 1440)",
                (tenant_id, member_id, wd),
            )
    status, _ = http_json("booking.healthz", "GET", f"{base['booking']}/healthz")
    record("booking: GET /healthz -> 200", status == 200, f"status={status}")
    status, site = http_json("booking.public_site", "GET", f"{base['booking']}/public/sites/{slug}")
    record("booking: GET /public/sites/{slug} -> 200 (Dapr-free: tenant ctx fallback)",
           status == 200 and isinstance(site, dict) and site.get("site_slug") == slug,
           f"status={status} body={str(site)[:160]}")
    status, ctx = http_json("booking.public_context", "GET",
                            f"{base['booking']}/public/sites/{slug}/context")
    record("booking: GET /public/sites/{slug}/context -> 200 with offerings",
           status == 200 and isinstance(ctx, dict) and len(ctx.get("offerings", [])) == 1,
           f"status={status} offerings={len(ctx.get('offerings', [])) if isinstance(ctx, dict) else '?'}")
    status, offs = http_json("booking.public_offerings", "GET",
                             f"{base['booking']}/public/sites/{slug}/offerings")
    record("booking: GET /public/sites/{slug}/offerings -> 200 with seeded offering",
           status == 200 and isinstance(offs, list) and len(offs) == 1,
           f"status={status} n={len(offs) if isinstance(offs, list) else '?'}")

    # ---- 2b. booking: create coverage (SPEC-W42, IDENTITY_BASE_URL mode) --
    # Degraded-mode honesty: no Temporal => booking stays status=pending; no
    # Dapr => outbox dispatcher never publishes => sent_at IS NULL. The
    # harness asserts exactly that; it does NOT fake confirmation.
    slot_base = datetime.now(timezone.utc).replace(second=0, microsecond=0) + timedelta(days=9)

    def create_booking(call: str, url: str, key: str, slot_off_min: int,
                       headers: dict[str, str] | None, source: str | None = None):
        body = {
            "offering_id": offering_id,
            "team_member_id": member_id,
            "contact": {"name": "E2E Customer", "phone": "+15550001234",
                        "email": "customer@e2e.example"},
            "starts_at": (slot_base + timedelta(minutes=slot_off_min)).isoformat(),
            "idempotency_key": key,
        }
        if source:
            body["source"] = source
        return http_json(call, "POST", url, body, headers)

    def booking_db_state(booking_id: str, key: str) -> tuple[str | None, int, int, int]:
        """(row status, rows for idempotency key, outbox rows, unsent outbox rows)."""
        with psycopg.connect(dsn_for(srv, "booking"), autocommit=True) as c:
            row = c.execute("SELECT status FROM bookings WHERE id = %s", (booking_id,)).fetchone()
            n_key = c.execute("SELECT count(*) FROM bookings WHERE idempotency_key = %s",
                              (key,)).fetchone()[0]
            n_out = c.execute("SELECT count(*) FROM outbox WHERE aggregate_id = %s",
                              (booking_id,)).fetchone()[0]
            n_unsent = c.execute(
                "SELECT count(*) FROM outbox WHERE aggregate_id = %s AND sent_at IS NULL",
                (booking_id,)).fetchone()[0]
        return (row[0] if row else None, n_key, n_out, n_unsent)

    authed_headers = {"Authorization": "Bearer funds-e2e", "X-Tenant-Slug": slug}
    key_authed = "e2e-booking-authed-" + secrets.token_hex(8)
    status, bk = create_booking("booking.create_authed", f"{base['booking']}/v1/bookings",
                                key_authed, 0, authed_headers, source="api")
    authed_id = bk.get("id") if isinstance(bk, dict) else None
    record("booking: POST /v1/bookings (authed, IDENTITY_BASE_URL resolution) -> 201 pending",
           status == 201 and isinstance(bk, dict) and bk.get("status") == "pending" and bool(authed_id),
           f"status={status} body={str(bk)[:200]}")
    if authed_id:
        st, _n_key, n_out, n_unsent = booking_db_state(authed_id, key_authed)
        record("booking: authed create committed booking row status=pending "
               "(no saga — honest degraded mode)", st == "pending", f"row_status={st}")
        record("booking: authed create outbox row committed, sent_at IS NULL "
               "(no Dapr — honest degraded mode)",
               n_out >= 1 and n_unsent == n_out, f"outbox_rows={n_out} unsent={n_unsent}")
        status2, bk2 = create_booking("booking.create_authed_replay",
                                      f"{base['booking']}/v1/bookings",
                                      key_authed, 0, authed_headers, source="api")
        _st2, n_key2, _n2, _u2 = booking_db_state(authed_id, key_authed)
        replay_id = bk2.get("id") if isinstance(bk2, dict) else None
        record("booking: authed create REPLAY same idempotency_key -> same booking, exactly 1 row",
               status2 in (200, 201) and replay_id == authed_id and n_key2 == 1,
               f"status={status2} replay_id={replay_id} rows_for_key={n_key2}")

    key_public = "e2e-booking-public-" + secrets.token_hex(8)
    status, pbk = create_booking("booking.public_create_booking",
                                 f"{base['booking']}/public/sites/{slug}/bookings",
                                 key_public, 60, None)
    public_id = pbk.get("id") if isinstance(pbk, dict) else None
    record("booking: POST /public/sites/{slug}/bookings -> 201 pending "
           "(IDENTITY_BASE_URL resolution)",
           status == 201 and isinstance(pbk, dict) and pbk.get("status") == "pending" and bool(public_id),
           f"status={status} body={str(pbk)[:200]}")
    if public_id:
        st, _n_key, n_out, n_unsent = booking_db_state(public_id, key_public)
        record("booking: public create committed booking row status=pending "
               "(no saga — honest degraded mode)", st == "pending", f"row_status={st}")
        record("booking: public create outbox row committed, sent_at IS NULL "
               "(no Dapr — honest degraded mode)",
               n_out >= 1 and n_unsent == n_out, f"outbox_rows={n_out} unsent={n_unsent}")
        status2, pbk2 = create_booking("booking.public_create_booking_replay",
                                       f"{base['booking']}/public/sites/{slug}/bookings",
                                       key_public, 60, None)
        _st2, n_key2, _n2, _u2 = booking_db_state(public_id, key_public)
        replay_id = pbk2.get("id") if isinstance(pbk2, dict) else None
        record("booking: public create REPLAY same idempotency_key -> same booking, exactly 1 row",
               status2 in (200, 201) and replay_id == public_id and n_key2 == 1,
               f"status={status2} replay_id={replay_id} rows_for_key={n_key2}")

    # ---- 3. billing: rate card, usage, invoice generate/issue/link --------
    bh = {"x-internal-token": billing_token,
          "x-tenant-id": tenant_id,
          "x-user-roles": "owner"}
    status, _ = http_json("billing.put_rate_card", "PUT",
                          f"{base['billing']}/v1/rate-cards/{tenant_id}",
                          {"metric": "calls", "unit_price_cents": 100,
                           "included_quota": 0, "currency": "USD"}, bh)
    record("billing: PUT /v1/rate-cards/{t} -> 200", status == 200, f"status={status}")
    period = time.strftime("%Y-%m")
    with psycopg.connect(dsn_for(srv, "billing"), autocommit=True) as c:
        # Usage ingest is Kafka-only in the service (consumer disabled here),
        # so fixture usage rows are seeded directly; generate then rates them.
        for i in range(3):
            c.execute("INSERT INTO processed_events (event_id) VALUES (%s)", (f"e2e-evt-{i}",))
            c.execute(
                "INSERT INTO usage_records (tenant_id, metric, value, ts, event_id)"
                " VALUES (%s, 'calls', 10, now(), %s)", (tenant_id, f"e2e-evt-{i}"))
    status, inv = http_json("billing.invoice_generate", "POST",
                            f"{base['billing']}/v1/invoices/generate",
                            {"tenant_id": tenant_id, "period": period}, bh)
    invoice_id = inv.get("id") if isinstance(inv, dict) else None
    record("billing: POST /v1/invoices/generate -> 201",
           status == 201 and bool(invoice_id), f"status={status} id={invoice_id}")
    if not invoice_id:
        return finalize(workdir, started)
    record("billing: generated invoice subtotal == 3000 cents (30 calls x 100)",
           inv.get("subtotal_cents") == 3000, f"subtotal={inv.get('subtotal_cents')}")

    status, inv = http_json("billing.invoice_issue", "POST",
                            f"{base['billing']}/v1/invoices/{invoice_id}/issue", None, bh)
    record("billing: POST /v1/invoices/{id}/issue -> 200 status=issued",
           status == 200 and inv.get("status") == "issued",
           f"status={status} inv={str(inv)[:160]}")

    status, link = http_json("billing.payment_link", "POST",
                             f"{base['billing']}/v1/invoices/{invoice_id}/payment-link",
                             None, bh)
    record("billing: POST /v1/invoices/{id}/payment-link -> 200 mode=static",
           status == 200 and isinstance(link, dict) and link.get("mode") == "static",
           f"status={status} body={str(link)[:160]}")

    # ---- 4. webhook phase: restart billing with the test Paystack secret --
    # (payment-link in paystack mode would call the live Paystack API; the
    # webhook handler 503s without the secret — one process cannot do both
    # offline, so the harness restarts the SAME service binary with the
    # secret; invoice state persists in Postgres.)
    print("[harness] restarting billing-engine with PAYSTACK_SECRET_KEY for webhook phase", flush=True)
    os.killpg(procs["billing"].pid, signal.SIGTERM)
    procs["billing"].wait(timeout=10)
    PROCS.remove(procs["billing"])
    billing_env_live = dict(billing_env_static)
    billing_env_live["PAYSTACK_SECRET_KEY"] = paystack_secret
    procs["billing"] = start_service("billing", bins["billing"], PORTS["billing"],
                                     billing_env_live, workdir)
    ok = wait_ready("billing", PORTS["billing"], procs["billing"])
    record("billing: restarted with PAYSTACK_SECRET_KEY, /healthz ok", ok)
    if not ok:
        return finalize(workdir, started)

    webhook_body = json.dumps(
        {"event": "charge.success", "data": {"reference": invoice_id}},
        separators=(",", ":")).encode()
    sig = hmac_mod.new(paystack_secret.encode(), webhook_body, hashlib.sha512).hexdigest()
    # Negative control first: a WRONG signature must be rejected (proves the
    # HMAC is really verified, not stubbed).
    status, _ = http_json("billing.webhook_bad_sig", "POST", f"{base['billing']}/webhooks/paystack",
                          webhook_body, {"x-paystack-signature": "0" * 128,
                                         "Content-Type": "application/json"})
    record("billing: webhook with WRONG signature -> 401", status == 401, f"status={status}")
    status, resp = http_json("billing.webhook_paystack", "POST", f"{base['billing']}/webhooks/paystack",
                             webhook_body, {"x-paystack-signature": sig,
                                            "Content-Type": "application/json"})
    record("billing: webhook with REAL HMAC-SHA512 -> 200 status=paid",
           status == 200 and isinstance(resp, dict) and resp.get("status") == "paid",
           f"status={status} body={str(resp)[:160]}")

    status, inv = http_json("billing.get_invoice_paid", "GET",
                            f"{base['billing']}/v1/invoices/{invoice_id}", None, bh)
    record("billing: invoice now paid", status == 200 and inv.get("status") == "paid",
           f"status={status} inv_status={inv.get('status') if isinstance(inv, dict) else '?'}")

    # Same-tx durability + ledger assertions as superuser.
    paid_transfer_id = str(uuid.uuid5(uuid.NAMESPACE_URL, f"billing-paid:{invoice_id}"))
    issued_transfer_id = str(uuid.uuid5(uuid.NAMESPACE_URL, f"billing-issued:{invoice_id}"))
    with psycopg.connect(dsn_for(srv, "billing"), autocommit=True) as c:
        n_outbox = c.execute(
            "SELECT count(*) FROM billing_outbox"
            " WHERE payload->>'type' = 'com.opendesk.billing.InvoicePaid'"
            " AND payload->>'subject' = %s", (invoice_id,)).fetchone()[0]
        n_paid = c.execute("SELECT count(*) FROM ledger_transfers WHERE id = %s AND code = 202",
                           (paid_transfer_id,)).fetchone()[0]
        n_issued = c.execute("SELECT count(*) FROM ledger_transfers WHERE id = %s AND code = 200",
                             (issued_transfer_id,)).fetchone()[0]
    record("billing: invoice_issued ledger transfer posted (code 200, deterministic id)",
           n_issued == 1, f"rows={n_issued}")
    record("billing: invoice_paid ledger transfer posted (code 202, deterministic id)",
           n_paid == 1, f"rows={n_paid}")
    record("billing: billing_outbox InvoicePaid row committed (same-tx durability)",
           n_outbox == 1, f"rows={n_outbox}")

    # Idempotent replay of the IDENTICAL webhook bytes + signature.
    for i in range(max(1, PERF_ITERS)):
        status, resp = http_json("billing.webhook_replay", "POST",
                                 f"{base['billing']}/webhooks/paystack", webhook_body,
                                 {"x-paystack-signature": sig, "Content-Type": "application/json"})
        if i == 0:
            record("billing: webhook REPLAY -> 200 already_paid",
                   status == 200 and isinstance(resp, dict) and resp.get("status") == "already_paid",
                   f"status={status} body={str(resp)[:120]}")
    with psycopg.connect(dsn_for(srv, "billing"), autocommit=True) as c:
        n_outbox2 = c.execute(
            "SELECT count(*) FROM billing_outbox"
            " WHERE payload->>'type' = 'com.opendesk.billing.InvoicePaid'"
            " AND payload->>'subject' = %s", (invoice_id,)).fetchone()[0]
        n_paid2 = c.execute("SELECT count(*) FROM ledger_transfers WHERE id = %s",
                            (paid_transfer_id,)).fetchone()[0]
    record("billing: replay caused NO second ledger posting", n_paid2 == 1, f"rows={n_paid2}")
    record("billing: replay caused NO second outbox row", n_outbox2 == 1, f"rows={n_outbox2}")

    # ---- 5. payments: hold -> capture -> replay -> balance ----------------
    hold_key = "e2e-hold-" + secrets.token_hex(8)
    bal_pre_hold = balance_map(base["payments"], tenant_id, "payments.balance_pre_hold") if TB_MODE else {}
    status, hold = http_json("payments.hold", "POST", f"{base['payments']}/v1/deposits",
                             {"tenant_id": tenant_id, "amount_cents": 5000,
                              "currency": "USD", "idempotency_key": hold_key})
    deposit_id = hold.get("deposit_id") if isinstance(hold, dict) else None
    record("payments: POST /v1/deposits (hold) -> 201", status == 201 and bool(deposit_id),
           f"status={status} deposit_id={deposit_id}")
    if not deposit_id:
        return finalize(workdir, started)
    status, hold2 = http_json("payments.hold_replay", "POST", f"{base['payments']}/v1/deposits",
                              {"tenant_id": tenant_id, "amount_cents": 5000,
                               "currency": "USD", "idempotency_key": hold_key})
    record("payments: hold REPLAY same key -> same deposit_id (no double-hold)",
           status == 201 and hold2.get("deposit_id") == deposit_id,
           f"status={status} id2={hold2.get('deposit_id') if isinstance(hold2, dict) else '?'}")

    # The live TigerBeetle client requires an explicit capture amount (the
    # fee split needs it — ledger/tigerbeetle.rs capture_like_tb); the sim
    # accepts amount=None as full-capture. Sim mode keeps the W41 body
    # byte-identical.
    capture_body: dict = {"tenant_id": tenant_id}
    if TB_MODE:
        capture_body["amount_cents"] = 5000
    bal_post_hold = balance_map(base["payments"], tenant_id, "payments.balance_post_hold") if TB_MODE else {}
    status, cap = http_json("payments.capture", "POST",
                            f"{base['payments']}/v1/deposits/{deposit_id}/capture",
                            capture_body)
    record("payments: POST /v1/deposits/{id}/capture -> 200",
           status == 200 and isinstance(cap, dict), f"status={status} body={str(cap)[:200]}")
    # TB mode: the live TB client wraps replay results with fresh Utc::now()
    # created_at timestamps, so strip timestamp fields before the identical-
    # result comparison (every monetary field still compared strictly).
    # Sim mode reuses stored transfers: compare byte-identical, unchanged.
    def _replay_norm(obj):
        if not TB_MODE:
            return obj
        if isinstance(obj, dict):
            return {k: _replay_norm(v) for k, v in obj.items() if k != "created_at"}
        if isinstance(obj, list):
            return [_replay_norm(v) for v in obj]
        return obj
    first_result = json.dumps(_replay_norm(cap.get("result")), sort_keys=True) if isinstance(cap, dict) else None
    bal_post_capture = balance_map(base["payments"], tenant_id, "payments.balance_post_capture") if TB_MODE else {}
    for i in range(max(1, PERF_ITERS)):
        status, cap2 = http_json("payments.capture_replay", "POST",
                                 f"{base['payments']}/v1/deposits/{deposit_id}/capture",
                                 capture_body)
        if i == 0:
            same = isinstance(cap2, dict) and json.dumps(_replay_norm(cap2.get("result")), sort_keys=True) == first_result
            record("payments: capture REPLAY -> identical result, no double-post",
                   status == 200 and same, f"status={status} identical={same}")
    status, bal = http_json("payments.balance", "GET",
                            f"{base['payments']}/v1/accounts/{tenant_id}/balance")
    record("payments: GET /v1/accounts/{t}/balance -> 200 with accounts",
           status == 200 and isinstance(bal, dict) and bal.get("accounts") is not None,
           f"status={status} body={str(bal)[:200]}")

    # ---- 5b. REAL-LEDGER assertions (TB mode only) ------------------------
    # Balance deltas via the service's balance endpoint (real TB
    # lookup_accounts underneath) must match the capture response amounts
    # exactly:
    #   hold:    platform:clearing -> tenant:deposits PENDING 5000 (code 100)
    #   capture: post pending (deposits credits_posted +5000, pending
    #            released), then the split deposits -> revenue (net) +
    #            deposits -> platform:fees (fee) in ONE linked batch.
    if TB_MODE and isinstance(cap, dict) and isinstance(cap.get("result"), dict):
        dep = f"tenant:{tenant_id}:deposits"
        rev = f"tenant:{tenant_id}:revenue"
        res = cap["result"]
        post_amt = (res.get("post") or {}).get("amount")
        rev_amt = (res.get("revenue") or {}).get("amount")
        fee_amt = (res.get("platform_fee") or {}).get("amount", 0)

        def counter(snap: dict, account: str, field: str) -> int:
            return int(snap.get(account, {}).get(field, 0))

        pending_delta = (counter(bal_post_hold, dep, "credits_pending")
                         - counter(bal_pre_hold, dep, "credits_pending"))
        record("tigerbeetle: hold visible as PENDING on the real ledger "
               "(deposits credits_pending += hold amount)",
               pending_delta == 5000, f"delta={pending_delta}")
        moved = (
            post_amt == 5000
            and (counter(bal_post_capture, dep, "credits_posted")
                 - counter(bal_post_hold, dep, "credits_posted")) == post_amt
            and (counter(bal_post_capture, dep, "credits_pending")
                 - counter(bal_post_hold, dep, "credits_pending")) == -post_amt
            and (counter(bal_post_capture, dep, "debits_posted")
                 - counter(bal_post_hold, dep, "debits_posted")) == rev_amt + fee_amt
            and (counter(bal_post_capture, rev, "credits_posted")
                 - counter(bal_post_hold, rev, "credits_posted")) == rev_amt
        )
        record("tigerbeetle: capture MOVED FUNDS on the real ledger "
               "(balance deltas == post/revenue/fee amounts)",
               moved,
               f"post={post_amt} revenue={rev_amt} fee={fee_amt} "
               f"deposits={bal_post_capture.get(dep, {})} revenue={bal_post_capture.get(rev, {})}")
        bal_post_replay = balance_map(base["payments"], tenant_id, "payments.balance_post_replay")
        record("tigerbeetle: capture REPLAY left every balance counter byte-identical "
               "(real TB `exists`, no double-post)",
               bal_post_replay == bal_post_capture,
               f"post_capture={bal_post_capture} post_replay={bal_post_replay}")

    # Perf iterations for invoice generate over distinct periods (idempotent
    # hot-call repetition when FUNDS_E2E_PERF_ITERS>1), hold+capture pairs,
    # and booking creates over staggered slots.
    for i in range(1, max(1, PERF_ITERS)):
        per = f"2025-{(i % 12) + 1:02d}"
        for j in range(3):
            ev = f"e2e-perf-{i}-{j}"
            with psycopg.connect(dsn_for(srv, "billing"), autocommit=True) as c:
                c.execute("INSERT INTO processed_events (event_id) VALUES (%s) ON CONFLICT DO NOTHING", (ev,))
                c.execute("INSERT INTO usage_records (tenant_id, metric, value, ts, event_id)"
                          " VALUES (%s, 'calls', 10, %s::timestamptz, %s)",
                          (tenant_id, f"{per}-15T00:00:00Z", ev))
        http_json("billing.invoice_generate", "POST", f"{base['billing']}/v1/invoices/generate",
                  {"tenant_id": tenant_id, "period": per}, bh)
        # hold+capture pairs
        k = f"e2e-perf-hold-{i}"
        st1, h1 = http_json("payments.hold", "POST", f"{base['payments']}/v1/deposits",
                            {"tenant_id": tenant_id, "amount_cents": 500, "idempotency_key": k})
        if isinstance(h1, dict) and h1.get("deposit_id"):
            perf_cap: dict = {"tenant_id": tenant_id}
            if TB_MODE:
                perf_cap["amount_cents"] = 500
            http_json("payments.capture", "POST",
                      f"{base['payments']}/v1/deposits/{h1['deposit_id']}/capture",
                      perf_cap)
        # booking-create pairs over staggered slots (fresh idempotency keys;
        # 60-min spacing avoids availability.Fits conflicts at capacity=1).
        off = 120 + i * 120
        create_booking("booking.create_authed", f"{base['booking']}/v1/bookings",
                       f"e2e-perf-booking-a-{i}", off, authed_headers, source="api")
        create_booking("booking.public_create_booking",
                       f"{base['booking']}/public/sites/{slug}/bookings",
                       f"e2e-perf-booking-p-{i}", off + 60, None)

    # ---- 6. RLS adversarial on the billing DB ------------------------------
    app = dsn_for(srv, "billing", user="app_billing_login", password="app_billing_dev_password")

    def invoices_visible(guc: str | None) -> int:
        with psycopg.connect(app, autocommit=True) as c:
            if guc is not None:
                c.execute("SELECT set_config('app.tenant_id', %s, false)", (guc,))
            return c.execute("SELECT count(*) FROM invoices").fetchone()[0]

    record("RLS: app_billing_login with WRONG app.tenant_id sees 0 invoices",
           invoices_visible(str(uuid.uuid4())) == 0)
    record("RLS: app_billing_login with '' app.tenant_id sees 0 invoices (W40-6 fail-closed)",
           invoices_visible("") == 0)
    record("RLS: app_billing_login with GUC unset sees 0 invoices (fail-closed)",
           invoices_visible(None) == 0)
    n_own = invoices_visible(tenant_id)
    record("RLS: app_billing_login with CORRECT app.tenant_id sees only its invoices",
           n_own >= 1, f"rows={n_own}")

    return finalize(workdir, started)


def finalize(workdir: Path, started: float) -> int:
    timings_path = workdir / "timings" / "funds-e2e-timings.json"
    timings_path.write_text(json.dumps({
        "harness": "funds-e2e",
        "ledger_mode": "tigerbeetle" if TB_MODE else "sim",
        "started": started,
        "wall_time_s": round(time.time() - started, 2),
        "calls": TIMINGS,
    }, indent=2))
    failed = [r for r in RESULTS if not r["ok"]]
    summary = {
        "results": RESULTS, "wall_time_s": round(time.time() - started, 2),
        "ledger_mode": "tigerbeetle" if TB_MODE else "sim",
        "timings": str(timings_path),
    }
    (workdir / "funds-e2e-summary.json").write_text(json.dumps(summary, indent=2))
    print(f"[harness] timings -> {timings_path}", flush=True)
    print(f"[harness] {'OK' if not failed else 'FAILED'}: "
          f"{len(RESULTS) - len(failed)}/{len(RESULTS)} checks passed, "
          f"{len(TIMINGS)} timed calls, {summary['wall_time_s']}s", flush=True)
    return 0 if not failed else 1


if __name__ == "__main__":
    sys.exit(main())
