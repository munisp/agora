"""SQL-001 regression: ensure_agent_tables() must bootstrap FORCE ROW LEVEL
SECURITY + tenant_isolation policies (+ app_conversation GRANTs) for the W38
agents/capture tables — not just create bare tables.

Runs against REAL embedded Postgres 16 (pgserver, same harness family as
services/model-registry/tests/conftest.py). Tenant isolation is exercised
through AgentStore as a non-superuser, non-owner role — mirroring production,
where every tenant statement runs in a transaction with app.tenant_id set
(Database._tenant_tx). On pre-fix code every isolation assertion fails (bare
tables have no RLS, so the subject role sees all tenants' rows).
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

from app.agent_db import AgentStore  # noqa: E402
from app.db import NotFoundError  # noqa: E402

pytestmark = pytest.mark.asyncio

TENANT_A = uuid.uuid4()
TENANT_B = uuid.uuid4()

SUBJECT_ROLE = "rls_subject"  # non-superuser, non-owner — the RLS subject
APP_ROLE = "app_conversation"  # production role (grant target)


class _StoreDB:
    """Minimal stand-in for app.db.Database: pool access plus the exact
    _tenant_tx semantics (app.tenant_id set LOCAL inside a transaction)."""

    def __init__(self, pool: asyncpg.Pool) -> None:
        self._pool = pool

    def _pool_acquire(self):
        return self._pool.acquire()

    @contextlib.asynccontextmanager
    async def _tenant_tx(self, tenant_id: uuid.UUID):
        async with self._pool.acquire() as conn:
            async with conn.transaction():
                await conn.execute(
                    "SELECT set_config('app.tenant_id', $1, true)", str(tenant_id)
                )
                yield conn


@pytest.fixture(scope="module")
def pg(tmp_path_factory):
    server = pgserver.get_server(str(tmp_path_factory.mktemp("pgdata")))
    yield server
    server.cleanup()


@pytest.fixture(scope="module")
def super_dsn(pg) -> str:
    return pg.get_uri(database="postgres")


@pytest_asyncio.fixture(scope="module")
async def roles(pg, super_dsn):
    """Test roles, created once per server: the RLS subject (non-superuser,
    non-owner) and the production app role (grant target)."""
    pool = await asyncpg.create_pool(super_dsn)
    async with pool.acquire() as conn:
        await conn.execute(
            f"""
            DO $$
            BEGIN
                IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '{SUBJECT_ROLE}') THEN
                    CREATE ROLE {SUBJECT_ROLE} LOGIN;
                END IF;
                IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '{APP_ROLE}') THEN
                    CREATE ROLE {APP_ROLE} NOLOGIN;
                END IF;
            END
            $$
            """
        )
        await conn.execute(f"GRANT USAGE ON SCHEMA public TO {SUBJECT_ROLE}")
    await pool.close()
    return True


@pytest_asyncio.fixture()
async def stores(pg, super_dsn, roles):
    """Fresh-bootstrapped DB per test: superuser store ran the bootstrap;
    subject store is the least-privilege RLS subject role."""
    super_pool = await asyncpg.create_pool(super_dsn)
    async with super_pool.acquire() as conn:
        # No pgcrypto: pgserver's bundled Postgres lacks contrib modules, and
        # gen_random_uuid() is core since PG13.
        # Minimal stand-in for 03-conversation-schema.sql (capture_records
        # references conversations(id)); deliberately RLS-free here.
        await conn.execute(
            """
            CREATE TABLE IF NOT EXISTS conversations (
                id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
                tenant_id UUID NOT NULL
            )
            """
        )
        await conn.execute(
            f"GRANT SELECT, INSERT, UPDATE, DELETE ON conversations TO {SUBJECT_ROLE}"
        )

    super_store = AgentStore(_StoreDB(super_pool))
    await super_store.ensure_agent_tables()

    async with super_pool.acquire() as conn:
        # The production privilege set comes from 05-app-roles.sql defaults;
        # here the subject needs explicit DML grants (RLS still applies on
        # top of privileges — privileges alone must NOT leak rows).
        for tbl in ("agents", "capture_schemas", "capture_records", "tenant_slugs"):
            await conn.execute(
                f"GRANT SELECT, INSERT, UPDATE, DELETE ON {tbl} TO {SUBJECT_ROLE}"
            )

    subject_pool = await asyncpg.create_pool(super_dsn, user=SUBJECT_ROLE)
    subject_store = AgentStore(_StoreDB(subject_pool))
    try:
        yield super_store, subject_store, super_pool, subject_pool
    finally:
        await subject_pool.close()
        async with super_pool.acquire() as conn:
            for tbl in ("capture_records", "capture_schemas", "agents",
                        "tenant_slugs", "conversations"):
                await conn.execute(f"DROP TABLE IF EXISTS {tbl} CASCADE")
        await super_pool.close()


async def _mk_conversation(pool: asyncpg.Pool, tenant_id: uuid.UUID) -> uuid.UUID:
    async with pool.acquire() as conn:
        return await conn.fetchval(
            "INSERT INTO conversations (tenant_id) VALUES ($1) RETURNING id",
            tenant_id,
        )


# ------------------------------------------------------------- catalog state
async def test_bootstrap_enables_force_rls_and_policies(stores):
    _, _, super_pool, _ = stores
    async with super_pool.acquire() as conn:
        rows = await conn.fetch(
            """
            SELECT relname, relrowsecurity, relforcerowsecurity
            FROM pg_class
            WHERE relnamespace = 'public'::regnamespace
              AND relname IN ('agents', 'capture_schemas', 'capture_records',
                              'tenant_slugs')
            ORDER BY relname
            """
        )
        flags = {r["relname"]: (r["relrowsecurity"], r["relforcerowsecurity"])
                 for r in rows}
        # The three tenant tables: ENABLE + FORCE.
        for tbl in ("agents", "capture_schemas", "capture_records"):
            assert flags.get(tbl) == (True, True), f"{tbl} missing FORCE RLS"
        # tenant_slugs is deliberately NOT RLS-scoped (cross-tenant resolve).
        assert flags.get("tenant_slugs") == (False, False)

        policies = await conn.fetch(
            """
            SELECT c.relname AS tablename, p.polname AS policyname,
                   pg_get_expr(p.polqual, p.polrelid) AS qual
            FROM pg_policy p
            JOIN pg_class c ON c.oid = p.polrelid
            WHERE c.relnamespace = 'public'::regnamespace
              AND c.relname IN ('agents', 'capture_schemas', 'capture_records')
            """
        )
        by_table = {p["tablename"]: p for p in policies}
        for tbl in ("agents", "capture_schemas", "capture_records"):
            pol = by_table.get(tbl)
            assert pol is not None, f"{tbl} missing tenant_isolation policy"
            assert pol["policyname"] == "tenant_isolation"
            assert "current_setting('app.tenant_id'::text, true)" in pol["qual"]


async def test_bootstrap_grants_app_conversation(stores):
    """The GRANT block mirrors 07-agents-capture-schema.sql: app_conversation
    (exists in this fixture) must hold DML on the three tenant tables."""
    _, _, super_pool, _ = stores
    async with super_pool.acquire() as conn:
        for tbl in ("agents", "capture_schemas", "capture_records"):
            ok = await conn.fetchval(
                "SELECT has_table_privilege($1, $2, 'SELECT')"
                " AND has_table_privilege($1, $2, 'INSERT')"
                " AND has_table_privilege($1, $2, 'UPDATE')"
                " AND has_table_privilege($1, $2, 'DELETE')",
                APP_ROLE,
                f"public.{tbl}",
            )
            assert ok, f"app_conversation missing DML grant on {tbl}"


# ---------------------------------------------------------- tenant isolation
async def test_agents_tenant_isolation(stores):
    _, subject, _, _ = stores
    a1 = await subject.create_agent(TENANT_A, "Alpha One", phone_number="+234111")
    a2 = await subject.create_agent(TENANT_A, "Alpha Two")
    b1 = await subject.create_agent(TENANT_B, "Beta One", phone_number="+234111")

    seen_a = await subject.list_agents(TENANT_A)
    seen_b = await subject.list_agents(TENANT_B)
    assert {a["id"] for a in seen_a} == {a1["id"], a2["id"]}
    assert all(a["tenant_id"] == TENANT_A for a in seen_a)
    assert [b["id"] for b in seen_b] == [b1["id"]]

    # Cross-tenant direct reads are invisible (NotFoundError), not leaked.
    with pytest.raises(NotFoundError):
        await subject.get_agent(b1["id"], TENANT_A)
    with pytest.raises(NotFoundError):
        await subject.get_agent(a1["id"], TENANT_B)


async def test_capture_chain_tenant_isolation(stores):
    _, subject, _, subject_pool = stores
    # Conversations exist per tenant (RLS-free stand-in table).
    conv_a = await _mk_conversation(subject_pool, TENANT_A)
    conv_b = await _mk_conversation(subject_pool, TENANT_B)

    agent_a = await subject.create_agent(TENANT_A, "Capture A")
    agent_b = await subject.create_agent(TENANT_B, "Capture B")
    schema_a = await subject.create_capture_schema(
        tenant_id=TENANT_A, agent_id=agent_a["id"], name="kyc",
        schema={"fields": [{"key": "name", "type": "string"}]},
    )
    schema_b = await subject.create_capture_schema(
        tenant_id=TENANT_B, agent_id=agent_b["id"], name="kyc",
        schema={"fields": [{"key": "name", "type": "string"}]},
    )
    rec_a = await subject.insert_capture_record(
        TENANT_A, schema_a["id"], agent_a["id"], conv_a, {"name": "Ada"},
        extraction_confidence=0.9,
    )
    rec_b = await subject.insert_capture_record(
        TENANT_B, schema_b["id"], agent_b["id"], conv_b, {"name": "Bola"},
        extraction_confidence=0.8,
    )

    schemas_a = await subject.list_capture_schemas(TENANT_A)
    schemas_b = await subject.list_capture_schemas(TENANT_B)
    assert [s["id"] for s in schemas_a] == [schema_a["id"]]
    assert [s["id"] for s in schemas_b] == [schema_b["id"]]

    recs_a = await subject.list_capture_records(TENANT_A)
    recs_b = await subject.list_capture_records(TENANT_B)
    assert [r["id"] for r in recs_a] == [rec_a["id"]]
    assert [r["id"] for r in recs_b] == [rec_b["id"]]
    assert recs_a[0]["data"] == {"name": "Ada"}
    assert recs_b[0]["data"] == {"name": "Bola"}


async def test_no_tenant_context_sees_nothing(stores, super_dsn):
    """Deny-by-default: without app.tenant_id set the policy expression is
    NULL and even a privileged (all-grants) role reads zero rows.

    Uses a FRESH connection: asyncpg's pool reset leaves a recycled
    connection with app.tenant_id='' (not NULL) after a prior tenant tx —
    a fresh connection is the clean "no context was ever set" case.
    """
    _, subject, _, _ = stores
    await subject.create_agent(TENANT_A, "Hidden A")
    await subject.create_agent(TENANT_B, "Hidden B")
    fresh = await asyncpg.connect(super_dsn, user=SUBJECT_ROLE)
    try:  # NO set_config on this connection
        assert await fresh.fetchval(
            "SELECT current_setting('app.tenant_id', true) IS NULL"
        )
        for tbl in ("agents", "capture_schemas", "capture_records"):
            n = await fresh.fetchval(f"SELECT count(*) FROM {tbl}")
            assert n == 0, f"{tbl} visible without tenant context"
    finally:
        await fresh.close()


async def test_tenant_slugs_stays_cross_tenant_by_design(stores):
    """tenant_slugs intentionally has no RLS: /v1/agents/resolve must map
    tenant_id -> slug across tenants."""
    _, subject, _, subject_pool = stores
    await subject.remember_tenant_slug(TENANT_A, "acme")
    async with subject_pool.acquire() as conn:  # no tenant context
        slug = await conn.fetchval(
            "SELECT slug FROM tenant_slugs WHERE tenant_id = $1", TENANT_A
        )
    assert slug == "acme"
