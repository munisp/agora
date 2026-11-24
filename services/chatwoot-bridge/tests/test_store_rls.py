"""RLS regression (SPEC-W118 §4): chatwoot-bridge tables enforce tenant
isolation under FORCE ROW LEVEL SECURITY for a non-superuser, non-owner
role — mirroring production (app_chatwoot_bridge_login + app.tenant_id).

Runs against REAL embedded Postgres 16 (pgserver, same harness family as
conversation-service/tests/test_agent_rls.py).
"""

from __future__ import annotations

import contextlib
import os
import sys
import uuid
from pathlib import Path

import asyncpg
import pytest
import pytest_asyncio

os.environ.setdefault("XDG_RUNTIME_DIR", "/tmp/xdg")
Path("/tmp/xdg").mkdir(parents=True, exist_ok=True)

import pgserver  # noqa: E402

sys.path.insert(0, ".")

from app.store import DDL, Database  # noqa: E402

pytestmark = pytest.mark.asyncio

TENANT_A = str(uuid.uuid4())
TENANT_B = str(uuid.uuid4())
SUBJECT_ROLE = "rls_subject_bridge"


@pytest.fixture(scope="module")
def pg(tmp_path_factory):
    server = pgserver.get_server(str(tmp_path_factory.mktemp("pgdata")))
    yield server
    server.cleanup()


@pytest_asyncio.fixture(scope="module")
async def bootstrap(pg):
    """One-time DDL + role + grants; pool opened and CLOSED inside the
    fixture so no connection object outlives its event loop
    (pytest-asyncio strict-mode loop scoping)."""
    pool = await asyncpg.create_pool(pg.get_uri(database="postgres"))
    async with pool.acquire() as conn:
        await conn.execute(DDL)
        await conn.execute(
            f"""
            DO $$
            BEGIN
                IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '{SUBJECT_ROLE}') THEN
                    CREATE ROLE {SUBJECT_ROLE} LOGIN;
                END IF;
            END
            $$
            """
        )
        await conn.execute(f"GRANT USAGE ON SCHEMA public TO {SUBJECT_ROLE}")
        for tbl in ("chatwoot_tenants", "chatwoot_links",
                    "chatwoot_sent_messages"):
            await conn.execute(
                f"GRANT SELECT, INSERT, UPDATE, DELETE ON {tbl} TO {SUBJECT_ROLE}"
            )
    await pool.close()
    return True


@pytest_asyncio.fixture()
async def super_pool(pg, bootstrap):
    pool = await asyncpg.create_pool(pg.get_uri(database="postgres"))
    yield pool
    await pool.close()


@pytest_asyncio.fixture()
async def subject(pg, bootstrap):
    """Least-privilege subject connected AS the non-superuser role —
    asyncpg's user= kwarg override (test_agent_rls.py idiom); string
    surgery on the pgserver URI silently connects as the superuser."""
    db = Database("postgres://unused/unused")
    db._pool = await asyncpg.create_pool(
        pg.get_uri(database="postgres"), user=SUBJECT_ROLE
    )
    yield db
    await db.aclose()


async def test_bootstrap_enables_force_rls_and_policies(super_pool):
    async with super_pool.acquire() as conn:
        rows = await conn.fetch(
            """
            SELECT relname, relrowsecurity, relforcerowsecurity
            FROM pg_class
            WHERE relnamespace = 'public'::regnamespace
              AND relname IN ('chatwoot_tenants', 'chatwoot_links',
                              'chatwoot_sent_messages')
            ORDER BY relname
            """
        )
        flags = {r["relname"]: (r["relrowsecurity"], r["relforcerowsecurity"])
                 for r in rows}
        for tbl in ("chatwoot_tenants", "chatwoot_links",
                    "chatwoot_sent_messages"):
            assert flags.get(tbl) == (True, True), f"{tbl} missing FORCE RLS"


def _mapping(tenant_id: str, **kw):
    m = dict(
        account_id=1, inbox_id=2, api_access_token="tok",
        webhook_token=f"wh-{tenant_id[:8]}-{uuid.uuid4().hex[:6]}",
        webhook_id=None, hmac_secret=None,
    )
    m.update(kw)
    return m


async def test_tenant_isolation_mappings(subject):
    await subject.upsert_tenant(TENANT_A, **_mapping(TENANT_A))
    await subject.upsert_tenant(TENANT_B, **_mapping(TENANT_B))
    assert str((await subject.get_tenant(TENANT_A))["tenant_id"]) == TENANT_A
    # B's scope cannot see A's row (and vice versa via scoped reads only)
    async with subject.tenant_conn(TENANT_B) as conn:
        rows = await conn.fetch("SELECT * FROM chatwoot_tenants")
    assert [r["tenant_id"] for r in rows] == [uuid.UUID(TENANT_B)]


async def test_tenant_isolation_links_and_sent(subject):
    await subject.upsert_tenant(TENANT_A, **_mapping(TENANT_A))
    await subject.upsert_tenant(TENANT_B, **_mapping(TENANT_B))
    conv_a, conv_b = str(uuid.uuid4()), str(uuid.uuid4())
    await subject.insert_link(
        TENANT_A, conv_a, chatwoot_conversation_id=101,
        chatwoot_contact_id=201, contact_source_id="s-a",
    )
    await subject.insert_link(
        TENANT_B, conv_b, chatwoot_conversation_id=101,
        chatwoot_contact_id=202, contact_source_id="s-b",
    )
    await subject.sent_insert(TENANT_A, "od-1", 9001)
    await subject.sent_insert(TENANT_B, "od-1", 9002)
    # same chatwoot_conversation_id + same source_id coexist across tenants
    assert await subject.sent_lookup(TENANT_A, "od-1")
    assert await subject.sent_lookup(TENANT_B, "od-1")
    async with subject.tenant_conn(TENANT_A) as conn:
        links = await conn.fetch("SELECT * FROM chatwoot_links")
        sent = await conn.fetch("SELECT * FROM chatwoot_sent_messages")
    assert len(links) == 1 and links[0]["conversation_id"] == uuid.UUID(conv_a)
    assert len(sent) == 1 and sent[0]["chatwoot_message_id"] == 9001
    # reverse lookup stays tenant-scoped
    link = await subject.get_link_by_chatwoot_conversation(TENANT_A, 101)
    assert str(link["conversation_id"]) == conv_a


async def test_insert_link_idempotent_under_conflict(subject):
    await subject.upsert_tenant(TENANT_A, **_mapping(TENANT_A))
    conv = str(uuid.uuid4())
    first = await subject.insert_link(
        TENANT_A, conv, chatwoot_conversation_id=500,
        chatwoot_contact_id=1, contact_source_id="s-1",
    )
    second = await subject.insert_link(
        TENANT_A, conv, chatwoot_conversation_id=500,
        chatwoot_contact_id=1, contact_source_id="s-1",
    )
    assert str(first["conversation_id"]) == str(second["conversation_id"]) == conv


async def test_delete_tenant_cascades_scoped(subject):
    await subject.upsert_tenant(TENANT_A, **_mapping(TENANT_A))
    await subject.upsert_tenant(TENANT_B, **_mapping(TENANT_B))
    conv = str(uuid.uuid4())
    await subject.insert_link(
        TENANT_A, conv, chatwoot_conversation_id=700,
        chatwoot_contact_id=1, contact_source_id="s",
    )
    await subject.sent_insert(TENANT_A, "od-x", 1)
    await subject.sent_insert(TENANT_B, "od-x", 2)
    await subject.delete_tenant(TENANT_A)
    assert await subject.get_tenant(TENANT_A) is None
    assert await subject.get_link(TENANT_A, conv) is None
    assert not await subject.sent_lookup(TENANT_A, "od-x")
    # B untouched
    assert await subject.get_tenant(TENANT_B) is not None
    assert await subject.sent_lookup(TENANT_B, "od-x")


async def test_unset_tenant_guc_sees_nothing(subject):
    """Fail-closed: a query without app.tenant_id set returns zero rows
    (the NULLIF guard never matches)."""
    await subject.upsert_tenant(TENANT_A, **_mapping(TENANT_A))
    async with subject._pool.acquire() as conn:
        rows = await conn.fetch("SELECT * FROM chatwoot_tenants")
    assert rows == []
