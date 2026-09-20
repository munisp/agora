"""Tests for the OOS-23 prod guard (SPEC-W45): scripts/seeds/_lib.py
assert_seed_allowed + the mirrored bash guard in bootstrap.sh.

Self-contained: inserts scripts/seeds on sys.path itself (no conftest).
"""

from __future__ import annotations

import re
import subprocess
import sys
from pathlib import Path

import pytest

SEEDS_DIR = Path(__file__).resolve().parents[1]
if str(SEEDS_DIR) not in sys.path:
    sys.path.insert(0, str(SEEDS_DIR))

import _lib  # noqa: E402

LOCAL_DSNS = [
    "postgres://opendesk:opendesk@localhost:5432/analytics_meta",
    "postgres://opendesk:opendesk@127.0.0.1:5432/analytics_meta",
    "postgresql://opendesk:opendesk@postgres:5432/analytics_meta",  # compose service
    "postgres://u:p@db/analytics_meta",
    "postgres://u:p@analytics-db:5432/analytics_meta",
    "postgres://u:p@host.docker.internal:5432/analytics_meta",
    "host=localhost port=5432 dbname=analytics_meta user=opendesk",  # keyword DSN
    "host=postgres dbname=analytics_meta",
]

REMOTE_DSNS = [
    "postgres://opendesk:secret@prod-db.eu-west-1.rds.amazonaws.com:5432/analytics_meta",
    "postgres://opendesk:secret@10.0.3.17:5432/analytics_meta",
    "postgres://opendesk:secret@db.example.com/analytics_meta",
    "host=prod.internal dbname=analytics_meta",
    "not-a-dsn-at-all",  # unparsable host → fail closed
    "",
]


@pytest.mark.parametrize("dsn", LOCAL_DSNS)
def test_local_hosts_allowed(dsn: str) -> None:
    _lib.assert_seed_allowed(dsn, environ={})


@pytest.mark.parametrize("dsn", REMOTE_DSNS)
def test_remote_hosts_refused(dsn: str) -> None:
    with pytest.raises(RuntimeError, match="SEED_ALLOW_PROD"):
        _lib.assert_seed_allowed(dsn, environ={})


@pytest.mark.parametrize("dsn", REMOTE_DSNS)
def test_remote_hosts_allowed_with_override(dsn: str) -> None:
    _lib.assert_seed_allowed(dsn, environ={"SEED_ALLOW_PROD": "1"})


def test_override_must_be_exactly_1() -> None:
    dsn = "postgres://u:p@prod.example.com/db"
    for bad in ("true", "yes", "0", ""):
        with pytest.raises(RuntimeError):
            _lib.assert_seed_allowed(dsn, environ={"SEED_ALLOW_PROD": bad})


def test_dsn_host_parsing() -> None:
    assert _lib.dsn_host("postgres://u:p@example.com:5432/db") == "example.com"
    assert _lib.dsn_host("postgres://u:p@[::1]:5432/db") == "::1"
    assert _lib.dsn_host("host=db.example.com port=5432") == "db.example.com"
    assert _lib.dsn_host("garbage") is None


def test_get_conn_runs_guard(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("DATABASE_URL", "postgres://u:p@prod.example.com/db")
    monkeypatch.delenv("SEED_ALLOW_PROD", raising=False)
    with pytest.raises(RuntimeError, match="SEED_ALLOW_PROD"):
        _lib.get_conn()


def test_bootstrap_guard_mirror() -> None:
    """The bash guard in bootstrap.sh must agree with _lib.py (both directions)."""
    script = SEEDS_DIR / "bootstrap.sh"
    assert script.exists()
    run = subprocess.run(
        ["bash", "-n", str(script)], capture_output=True, text=True, check=True
    )
    assert run.returncode == 0

    def bash_guard(dsn: str, allow_prod: str = "") -> int:
        # Extract and run just the seed_host_guard function under a controlled
        # DATABASE_URL, without running the seed steps.
        src = script.read_text(encoding="utf-8")
        m = re.search(r"seed_host_guard\(\) \{.*?\n\}", src, re.S)
        assert m, "seed_host_guard function not found in bootstrap.sh"
        harness = (
            "set -euo pipefail\n"
            "log() { :; }\nfail() { exit 1; }\n"
            f"DATABASE_URL={dsn!r}\nSEED_ALLOW_PROD={allow_prod!r}\n"
            + m.group(0)
            + "\nseed_host_guard\n"
        )
        p = subprocess.run(["bash", "-c", harness], capture_output=True)
        return p.returncode

    for dsn in LOCAL_DSNS:
        assert bash_guard(dsn) == 0, f"bash guard refused local DSN {dsn!r}"
    for dsn in REMOTE_DSNS:
        if dsn == "":  # bash guard only runs when DRY=0 with a real DSN
            continue
        assert bash_guard(dsn) != 0, f"bash guard allowed remote DSN {dsn!r}"
        assert bash_guard(dsn, "1") == 0, f"bash override failed for {dsn!r}"
