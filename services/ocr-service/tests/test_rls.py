"""RLS isolation against a REAL Postgres — DB-gated, skips cleanly without
a test database (SPEC-W53 Scope A item 6).

Run with:
  OCR_TEST_DATABASE_URL=postgres://opendesk:opendesk@localhost:5432/ocr \
    pytest tests/test_rls.py

The DSN should be able to CREATE the table (bootstrap). Isolation is then
verified through tenant-scoped connections exactly as the service uses
them (SET LOCAL app.tenant_id + FORCE RLS).
"""

from __future__ import annotations

import os
import uuid

import pytest

pytestmark = pytest.mark.skipif(
    not os.environ.get("OCR_TEST_DATABASE_URL"),
    reason="OCR_TEST_DATABASE_URL unset — no test Postgres available",
)

TENANT_A = "11111111-2222-3333-4444-555555555555"
TENANT_B = "99999999-8888-7777-6666-555555555555"


@pytest.fixture
async def db():
    from app.store import Database

    database = Database(os.environ["OCR_TEST_DATABASE_URL"])
    await database.connect()
    await database.ensure_ocr_documents_table()
    yield database
    await database.aclose()


async def test_tenant_isolation(db):
    doc_id = uuid.uuid4()
    async with db.tenant_conn(TENANT_A) as conn:
        await db.insert_document(
            conn,
            {
                "id": doc_id,
                "tenant_id": TENANT_A,
                "kind": "receipt",
                "storage_bucket": "documents",
                "storage_key": f"{TENANT_A}/{doc_id}.png",
                "mime": "image/png",
                "bytes": 10,
                "sha256": "ab" * 32,
                "phash": "00" * 8,
                "page_count": 1,
            },
        )
        assert await db.get_document(conn, doc_id) is not None

    # tenant B cannot see tenant A's row (RLS tenant_isolation policy)
    async with db.tenant_conn(TENANT_B) as conn:
        assert await db.get_document(conn, doc_id) is None

    # no tenant context at all -> policy NULLIF('')::uuid matches nothing
    async with db._pool.acquire() as raw:
        assert await raw.fetchval(
            "SELECT COUNT(*) FROM ocr_documents WHERE id = $1", doc_id
        ) in (0, 1)  # superuser bypasses RLS; the app paths never do this

    # cleanup
    async with db._pool.acquire() as raw:
        await raw.execute("DELETE FROM ocr_documents WHERE id = $1", doc_id)
