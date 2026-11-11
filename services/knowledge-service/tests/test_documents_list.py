"""SPEC-W47 (UIWEB-04 + DB-08): GET /v1/documents — tenant-scoped paginated
document list read from Postgres (source of truth), with the C1 tenant
binding (X-Tenant-Slugs) enforced exactly like the other knowledge routes.

Runs against a fake DB (no Postgres/OpenSearch needed): the route function
is mounted on a bare FastAPI app with a stubbed app.state.deps.
"""

from __future__ import annotations

import sys
import uuid
from contextlib import asynccontextmanager
from datetime import datetime, timezone

from fastapi import FastAPI
from fastapi.testclient import TestClient

sys.path.insert(0, ".")

from app import main as main_module  # noqa: E402

TENANT_A = "11111111-2222-3333-4444-555555555555"
TENANT_B = "99999999-8888-7777-6666-555555555555"


class FakeConn:
    """Records queries; returns scripted rows/total (asyncpg-shaped)."""

    def __init__(self, rows, total):
        self._rows = rows
        self._total = total
        self.fetch_calls: list[tuple[str, tuple]] = []
        self.fetchval_calls: list[tuple[str, tuple]] = []

    async def fetch(self, sql, *args):
        self.fetch_calls.append((sql, args))
        return list(self._rows)

    async def fetchval(self, sql, *args):
        self.fetchval_calls.append((sql, args))
        return self._total


class FakeDB:
    def __init__(self, conn):
        self._conn = conn
        self.tenant_calls: list[str] = []

    @asynccontextmanager
    async def tenant_conn(self, tenant_id):
        self.tenant_calls.append(tenant_id)
        yield self._conn


class FakeDeps:
    def __init__(self, db):
        self.db = db
        self.dapr = None  # UUID tenants skip the identity lookup


def _row(i: int) -> dict:
    return {
        "id": uuid.uuid4(),
        "title": f"Doc {i}",
        "body": f"body {i}",
        "source_url": None if i % 2 else f"https://example.com/{i}",
        "created_at": datetime(2026, 1, min(i, 28), tzinfo=timezone.utc),
        "chunk_count": i,
    }


def _client(rows, total):
    conn = FakeConn(rows, total)
    db = FakeDB(conn)
    app = FastAPI()
    app.get("/v1/documents")(main_module.list_documents)
    app.state.deps = FakeDeps(db)
    return TestClient(app), conn, db


def test_list_documents_happy_path():
    rows = [_row(1), _row(2)]
    client, conn, db = _client(rows, total=7)
    resp = client.get(
        "/v1/documents",
        params={"tenant": TENANT_A},
        headers={"X-Tenant-Slugs": TENANT_A},
    )
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["tenant_id"] == TENANT_A
    assert body["limit"] == 50  # default page size
    assert body["offset"] == 0
    assert body["count"] == 2
    assert body["total"] == 7
    assert [d["title"] for d in body["items"]] == ["Doc 1", "Doc 2"]
    assert body["items"][0]["body"] == "body 1"
    assert body["items"][0]["chunk_count"] == 1
    assert body["items"][1]["source_url"] == "https://example.com/2"
    assert body["items"][0]["created_at"].startswith("2026-01-01")

    # Reads ran under the RLS tenant context AND with an explicit tenant
    # predicate (belt and braces — no cross-tenant enumeration).
    assert db.tenant_calls == [TENANT_A]
    list_sql, list_args = conn.fetch_calls[0]
    assert "WHERE d.tenant_id = $1" in list_sql
    assert list_args == (uuid.UUID(TENANT_A), 50, 0)
    count_sql, count_args = conn.fetchval_calls[0]
    assert "tenant_id = $1" in count_sql
    assert count_args == (uuid.UUID(TENANT_A),)


def test_list_documents_pagination_params():
    client, conn, _ = _client([], total=0)
    resp = client.get(
        "/v1/documents",
        params={"tenant": TENANT_A, "limit": 5, "offset": 10},
        headers={"X-Tenant-Slugs": TENANT_A},
    )
    assert resp.status_code == 200, resp.text
    assert conn.fetch_calls[0][1] == (uuid.UUID(TENANT_A), 5, 10)
    body = resp.json()
    assert body["limit"] == 5
    assert body["offset"] == 10
    assert body["items"] == []


def test_list_documents_limit_bounds():
    client, _, _ = _client([], total=0)
    headers = {"X-Tenant-Slugs": TENANT_A}
    assert client.get("/v1/documents", params={"tenant": TENANT_A, "limit": 0},
                      headers=headers).status_code == 422
    assert client.get("/v1/documents", params={"tenant": TENANT_A, "limit": 201},
                      headers=headers).status_code == 422
    assert client.get("/v1/documents", params={"tenant": TENANT_A, "offset": -1},
                      headers=headers).status_code == 422


def test_list_documents_requires_bound_tenant():
    client, conn, db = _client([_row(1)], total=1)
    # No gateway header -> 401 (no unauthenticated tenant selection).
    resp = client.get("/v1/documents", params={"tenant": TENANT_A})
    assert resp.status_code == 401
    # Header/selector mismatch -> 403; NO query runs either way.
    resp = client.get(
        "/v1/documents",
        params={"tenant": TENANT_A},
        headers={"X-Tenant-Slugs": TENANT_B},
    )
    assert resp.status_code == 403
    assert conn.fetch_calls == []
    assert db.tenant_calls == []
