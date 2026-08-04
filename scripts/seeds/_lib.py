"""Shared seed library — SPEC-W17 contract A. Every seed_<domain>.py imports this.

Contract summary (binds Agents A–D):

- ``deterministic_id(natural_key)`` — sha256(SEED_SALT_CONSTANT + "|" + natural_key)
  hex. SEED_SALT_CONSTANT comes from env ``SEED_SALT`` with a fixed dev default
  (``.env.example``: ``opendesk-dev-seed-salt-change-in-prod``). Deterministic
  across runs on the same salt ⇒ idempotent reseeding (same natural key → same id).
- ``hash_pii(value)`` — argon2 (argon2-cffi low-level) with a per-run random salt
  that is DISCARDED: the output is a non-reversible, verification-only digest
  encoded as ``argon2$<b64-salt>$<b64-digest>``. If argon2-cffi is unavailable we
  fall back to hashlib.scrypt (still per-call random salt, discarded after encoding)
  and log LOUDLY.
- ``emit_seed_report(table, rowcount, runner_id, git_sha)`` — CloudEvent on Kafka
  topic ``cac.seed.report.<table>.v1`` when ``SEED_KAFKA=on``; otherwise (default,
  CI) append the same payload shape as JSONL to ``SEED_REPORT_PATH``
  (default ``/var/tmp/seed_reports.jsonl``). NEVER fails the seed on report IO.
- ``log_seed_run(table, rowcount, conn)`` — upsert into ``cac.seed_run_log``
  (latest run per table wins).
- ``scaled(cardinality)`` — ``int(cardinality * SEED_SCALE)``; ``SEED_SCALE`` env
  float, default 1.0 (CI uses 0.05).
- DB: ``get_conn()`` connects via ``DATABASE_URL`` using psycopg v3 if installed,
  else psycopg2(-binary). ALL DB access goes through the helpers here
  (``delete_by_ids`` / ``upsert_rows`` / ``log_seed_run``) so tests can fake conn.

Loader idiom (contract B) shared by all seed scripts::

    rows = build_rows(scale)                       # pure, deterministic
    ids  = [r["id"] for r in rows]
    delete_by_ids(conn, TABLE, ids)                # DELETE WHERE id IN (...)
    upsert_rows(conn, TABLE, COLUMNS, rows)        # INSERT ... ON CONFLICT (id) DO UPDATE
    emit_seed_report(TABLE, len(rows), runner_id(), git_sha())
    log_seed_run(TABLE, len(rows), conn)

plus ``--dry-run`` (prints counts, no writes, no DB) and ``--scale X.Y`` override
on every script — ``seed_argparser()`` provides both.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import logging
import os
import socket
import subprocess
from datetime import datetime, timezone
from typing import Any, Iterable, Mapping, Sequence

log = logging.getLogger("seeds")

# ---------------------------------------------------------------------------
# Environment knobs (documented in .env.example)
# ---------------------------------------------------------------------------
DEFAULT_SEED_SALT = "opendesk-dev-seed-salt-change-in-prod"
SEED_SALT_CONSTANT: str = os.environ.get("SEED_SALT", DEFAULT_SEED_SALT)

DEFAULT_REPORT_PATH = "/var/tmp/seed_reports.jsonl"

CLOUDEVENT_SOURCE = "opendesk.seeds"
CLOUDEVENT_TYPE = "com.opendesk.cac.SeedReport"
CLOUDEVENT_SPECVERSION = "1.0"


# ---------------------------------------------------------------------------
# Deterministic ids
# ---------------------------------------------------------------------------
def deterministic_id(natural_key: str) -> str:
    """sha256(SEED_SALT_CONSTANT + '|' + natural_key) hex — stable across runs."""
    payload = f"{SEED_SALT_CONSTANT}|{natural_key}"
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


# ---------------------------------------------------------------------------
# PII hashing (verification-only digests; per-call random salt, discarded)
# ---------------------------------------------------------------------------
def hash_pii(value: str) -> str:
    """Non-reversible verification digest of a PII value.

    argon2-cffi low-level hash_secret_raw with a per-call random salt; the salt
    is prepended (b64) to the returned digest string purely so a future
    verification call could recompute — the plaintext is unrecoverable and the
    digest is NOT stable across runs (do not use as a join key; use
    deterministic_id for keys). Falls back to hashlib.scrypt, logged loudly,
    when argon2-cffi is not installed.
    """
    raw = value.encode("utf-8")
    try:
        from argon2.low_level import Type, hash_secret_raw  # type: ignore

        salt = os.urandom(16)
        digest = hash_secret_raw(
            secret=raw, salt=salt, time_cost=2, memory_cost=1024,
            parallelism=1, hash_len=32, type=Type.ID,
        )
        return "argon2${}${}".format(
            base64.b64encode(salt).decode(), base64.b64encode(digest).decode()
        )
    except ImportError:  # pragma: no cover - depends on environment
        log.warning(
            "argon2-cffi NOT available — falling back to hashlib.scrypt for "
            "hash_pii. Install argon2-cffi (requirements-seeds.txt) for the "
            "contract-A PII digest."
        )
        salt = os.urandom(16)
        digest = hashlib.scrypt(raw, salt=salt, n=2**14, r=8, p=1, dklen=32)
        return "scrypt${}${}".format(
            base64.b64encode(salt).decode(), base64.b64encode(digest).decode()
        )


# ---------------------------------------------------------------------------
# Scaling
# ---------------------------------------------------------------------------
def seed_scale() -> float:
    """SEED_SCALE env float, default 1.0 (CI sets 0.05)."""
    raw = os.environ.get("SEED_SCALE", "1.0")
    try:
        return float(raw)
    except ValueError:
        log.warning("SEED_SCALE=%r is not a float; using 1.0", raw)
        return 1.0


def scaled(cardinality: int) -> int:
    """int(cardinality * SEED_SCALE)."""
    return int(cardinality * seed_scale())


# ---------------------------------------------------------------------------
# Runner identity
# ---------------------------------------------------------------------------
def runner_id() -> str:
    """Best-effort runner identity: USER@host (or 'unknown')."""
    user = os.environ.get("USER") or os.environ.get("USERNAME") or "unknown"
    return f"{user}@{socket.gethostname()}"


def git_sha() -> str:
    """Best-effort short git sha; 'unknown' when git/repo unavailable."""
    try:
        out = subprocess.run(
            ["git", "rev-parse", "--short", "HEAD"],
            capture_output=True, text=True, timeout=5,
        )
        sha = out.stdout.strip()
        return sha if out.returncode == 0 and sha else "unknown"
    except Exception:  # noqa: BLE001 - never fail a seed on git lookup
        return "unknown"


# ---------------------------------------------------------------------------
# Seed report events (Kafka when SEED_KAFKA=on, else JSONL file)
# ---------------------------------------------------------------------------
def seed_report_event(
    table: str, rowcount: int, runner: str, sha: str
) -> dict[str, Any]:
    """CloudEvent 1.0 envelope for a seed run (topic cac.seed.report.<table>.v1)."""
    now = datetime.now(timezone.utc)
    return {
        "specversion": CLOUDEVENT_SPECVERSION,
        "id": deterministic_id(f"seed_report:{table}:{now.isoformat()}"),
        "source": CLOUDEVENT_SOURCE,
        "type": CLOUDEVENT_TYPE,
        "subject": table,
        "time": now.isoformat(),
        "datacontenttype": "application/json",
        "data": {
            "table": table,
            "rowcount": int(rowcount),
            "runner_id": runner,
            "git_sha": sha,
        },
    }


def _kafka_publish(topic: str, event: Mapping[str, Any]) -> None:
    from kafka import KafkaProducer  # type: ignore  # optional dep, guarded

    bootstrap = os.environ.get("SEED_KAFKA_BOOTSTRAP", "localhost:9092")
    producer = KafkaProducer(
        bootstrap_servers=bootstrap,
        value_serializer=lambda v: json.dumps(v).encode("utf-8"),
        key_serializer=lambda k: k.encode("utf-8"),
        acks="all",
    )
    try:
        producer.send(topic, key=event["subject"], value=event).get(timeout=30)
    finally:
        producer.close()


def emit_seed_report(table: str, rowcount: int, runner: str, sha: str) -> dict[str, Any]:
    """Emit the seed report CloudEvent. NEVER raises — report IO must not fail a seed.

    SEED_KAFKA=on  → publish to Kafka topic cac.seed.report.<table>.v1
                     (kafka-python, guarded import; on any failure we fall back
                     to the JSONL file and log loudly).
    otherwise      → append JSONL to SEED_REPORT_PATH (default
                     /var/tmp/seed_reports.jsonl), same payload shape.
    Returns the event dict either way.
    """
    event = seed_report_event(table, rowcount, runner, sha)
    topic = f"cac.seed.report.{table}.v1"
    try:
        if os.environ.get("SEED_KAFKA", "off").lower() == "on":
            try:
                _kafka_publish(topic, event)
                return event
            except Exception:  # noqa: BLE001
                log.exception(
                    "Kafka publish to %s failed; falling back to JSONL report file", topic
                )
        path = os.environ.get("SEED_REPORT_PATH", DEFAULT_REPORT_PATH)
        with open(path, "a", encoding="utf-8") as fh:
            fh.write(json.dumps(event, ensure_ascii=False) + "\n")
    except Exception:  # noqa: BLE001 - report IO must never fail the seed
        log.exception("emit_seed_report(%s) failed; continuing (report is best-effort)", table)
    return event


# ---------------------------------------------------------------------------
# Database access — ALL DB traffic goes through these helpers (test-fakeable)
# ---------------------------------------------------------------------------
def get_conn() -> Any:
    """Connect via DATABASE_URL: psycopg v3 if installed, else psycopg2(-binary)."""
    dsn = os.environ.get("DATABASE_URL")
    if not dsn:
        raise RuntimeError("DATABASE_URL is not set (needed for non-dry-run seeding)")
    try:
        import psycopg  # type: ignore  # psycopg v3

        return psycopg.connect(dsn, autocommit=False)
    except ImportError:
        import psycopg2  # type: ignore

        return psycopg2.connect(dsn)


def _execute(conn: Any, sql: str, params: Sequence[Any] | None = None) -> None:
    """Execute through a cursor (psycopg v3 and v2 both expose cursor())."""
    cur = conn.cursor()
    try:
        cur.execute(sql, params)
    finally:
        close = getattr(cur, "close", None)
        if callable(close):
            close()


def delete_by_ids(conn: Any, table: str, ids: Sequence[str]) -> int:
    """DELETE FROM <table> WHERE id = ANY(%s) — batch form of contract-B step 2."""
    if not ids:
        return 0
    _execute(conn, f"DELETE FROM {table} WHERE id = ANY(%s)", (list(ids),))
    return len(ids)


def upsert_rows(
    conn: Any, table: str, columns: Sequence[str], rows: Iterable[Mapping[str, Any]]
) -> int:
    """INSERT ... ON CONFLICT (id) DO UPDATE — contract-B step 3.

    One executemany-style loop over rows (kept driver-agnostic so a fake conn
    works in tests). Returns the number of rows written.
    """
    rows = list(rows)
    if not rows:
        return 0
    cols = ", ".join(columns)
    placeholders = ", ".join(["%s"] * len(columns))
    updates = ", ".join(f"{c} = EXCLUDED.{c}" for c in columns if c != "id")
    sql = (
        f"INSERT INTO {table} ({cols}) VALUES ({placeholders}) "
        f"ON CONFLICT (id) DO UPDATE SET {updates}"
    )
    cur = conn.cursor()
    count = 0
    try:
        for row in rows:
            cur.execute(sql, tuple(row[c] for c in columns))
            count += 1
    finally:
        close = getattr(cur, "close", None)
        if callable(close):
            close()
    return count


def log_seed_run(table: str, rowcount: int, conn: Any) -> None:
    """Upsert one row into cac.seed_run_log (latest run per table wins)."""
    row = {
        "id": deterministic_id(f"seed_run_log:{table}"),
        "table_name": table,
        "rowcount": int(rowcount),
        "runner_id": runner_id(),
        "git_sha": git_sha(),
    }
    upsert_rows(
        conn,
        "cac.seed_run_log",
        ["id", "table_name", "rowcount", "runner_id", "git_sha"],
        [row],
    )


def commit(conn: Any) -> None:
    c = getattr(conn, "commit", None)
    if callable(c):
        c()


# ---------------------------------------------------------------------------
# Shared CLI (contract B: --dry-run and --scale on every script)
# ---------------------------------------------------------------------------
def seed_argparser(description: str) -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=description)
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="print row counts and exit; no DB connection, no writes",
    )
    parser.add_argument(
        "--scale",
        type=float,
        default=None,
        metavar="X.Y",
        help="override SEED_SCALE for this run (default: env SEED_SCALE or 1.0)",
    )
    return parser


def apply_scale_arg(scale: float | None) -> float:
    """Resolve the effective scale: explicit --scale beats env SEED_SCALE."""
    if scale is not None:
        if scale <= 0:
            raise SystemExit("--scale must be > 0")
        return scale
    return seed_scale()
