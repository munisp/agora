"""Shared fixtures for seed tests (SPEC-W17). DB-free: a fake conn stands in
for psycopg so the loader contract (DELETE -> INSERT ... ON CONFLICT) is
testable without Postgres."""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

SEEDS_DIR = Path(__file__).resolve().parents[2] / "scripts" / "seeds"
if str(SEEDS_DIR) not in sys.path:
    sys.path.insert(0, str(SEEDS_DIR))

import _lib  # noqa: E402


class FakeCursor:
    def __init__(self, conn: "FakeConn") -> None:
        self.conn = conn

    def execute(self, sql: str, params=None) -> None:
        self.conn.executed.append((sql, params))

    def close(self) -> None:  # psycopg cursor protocol
        pass


class FakeConn:
    """Records every (sql, params); commit/rollback counted."""

    def __init__(self) -> None:
        self.executed: list[tuple[str, object]] = []
        self.commits = 0

    def cursor(self) -> FakeCursor:
        return FakeCursor(self)

    def commit(self) -> None:
        self.commits += 1

    @property
    def sql(self) -> str:
        return "\n".join(s for s, _ in self.executed)


@pytest.fixture()
def fake_conn() -> FakeConn:
    return FakeConn()


@pytest.fixture(autouse=True)
def _default_salt(monkeypatch: pytest.MonkeyPatch) -> None:
    """Pin SEED_SALT so id assertions never depend on the host env."""
    monkeypatch.delenv("SEED_SALT", raising=False)
    _lib.SEED_SALT_CONSTANT = _lib.DEFAULT_SEED_SALT
