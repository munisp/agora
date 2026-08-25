"""J-14 regression (SPEC-W42, W39 action register #14): asyncpg pool
tenant-GUC closure for the conversation DB.

Runs against REAL embedded Postgres (pgserver 0.1.4, same harness family as
tests/test_agent_rls.py) with the conversations/turns schema from
infra/postgres/init-scripts/03-conversation-schema.sql (FORCE ROW LEVEL
SECURITY, NULLIF-wrapped app.tenant_id policies) and an
app_conversation-style NOLOGIN group + LOGIN role mirroring
05-app-roles.sql, so RLS genuinely applies to the service pool.

Assertions:

1. Tenants A/B interleaved (sequentially AND concurrently) on a pool of
   size 1..2 produce ZERO cross-tenant reads through the real
   app.db.Database code paths (every statement runs inside _tenant_tx with
   SET LOCAL app.tenant_id).
2. An unset-tenant session (no set_config ever issued) as the
   non-superuser login role sees ZERO rows on the FORCE-RLS tables.
3. No session-level SET of app.tenant_id survives pool release: SET LOCAL
   auto-resets at commit, and even an adversarial session-level SET is
   cleared by the pool reset before the next checkout.
4. _tenant_tx(None) fails loudly instead of running with the GUC unset.
"""

from __future__ import annotations

import asyncio
import os
import sys
import uuid
from pathlib import Path
from types import SimpleNamespace

import asyncpg
import pytest
import pytest_asyncio

os.environ.setdefault("XDG_RUNTIME_DIR", "/tmp/xdg")
Path("/tmp/xdg").mkdir(parents=True, exist_ok=True)

import pgserver  # noqa: E402

sys.path.insert(0, ".")

from app.db import Database, NotFoundError  # noqa: E402

pytestmark = pytest.mark.asyncio

TENANT_A = uuid.uuid4()
TENANT_B = uuid.uuid4()

# Mirroring infra/postgres/init-scripts/05-app-roles.sql for the
# conversation service: NOLOGIN NOINHERIT group + LOGIN member.
GROUP_ROLE = "app_conversation"
LOGIN_ROLE = "app_conversation_login"
APP_DB = "conversation"

# Mirroring 03-conversation-schema.sql, post-startup-ensure state (the
# ensure_* ALTERs add contact_phone / sentiment / intent / entities /
# idempotency_key and widen the channel CHECK with 'ussd'). pgserver's
# bundled Postgres lacks contrib modules; gen_random_uuid() is core.
SCHEMA_SQL = """
CREATE TABLE conversations (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
    site_slug    TEXT NOT NULL,
    channel      TEXT NOT NULL DEFAULT 'voice'
                 CHECK (channel IN ('voice','chat','phone','api','ussd')),
    contact_phone TEXT,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at     TIMESTAMPTZ
);
CREATE INDEX idx_conversations_tenant_started
    ON conversations (tenant_id, started_at DESC);
CREATE INDEX idx_conversations_contact_phone
    ON conversations (tenant_id, contact_phone);

CREATE TABLE turns (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    seq             INTEGER NOT NULL,
    role            TEXT NOT NULL CHECK (role IN ('user','agent','system','tool')),
    text            TEXT NOT NULL,
    tool_calls      JSONB,
    sentiment       DOUBLE PRECISION,
    intent          TEXT,
    entities        JSONB,
    idempotency_key TEXT,
    ts              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, seq)
);
CREATE UNIQUE INDEX uq_turns_idempotency_key
    ON turns (conversation_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

ALTER TABLE conversations ENABLE ROW LEVEL SECURITY;
ALTER TABLE conversations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON conversations
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE turns ENABLE ROW LEVEL SECURITY;
ALTER TABLE turns FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON turns
    USING (EXISTS (
        SELECT 1 FROM conversations c
        WHERE c.id = turns.conversation_id
          AND c.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    ));
"""

ROLES_SQL = f"""
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '{GROUP_ROLE}') THEN
        CREATE ROLE {GROUP_ROLE} NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '{LOGIN_ROLE}') THEN
        CREATE ROLE {LOGIN_ROLE} LOGIN PASSWORD 'app_conversation_dev_password'
            IN ROLE {GROUP_ROLE};
    END IF;
END
$$;
GRANT CONNECT ON DATABASE {APP_DB} TO {GROUP_ROLE};
"""


def _app_dsn(super_dsn: str) -> str:
    """Same socket DSN as the superuser URI but as the least-privilege
    login role (pgserver socket auth is trust; the password in ROLES_SQL
    mirrors 05-app-roles.sql and is unused locally)."""
    scheme, rest = super_dsn.split("://", 1)
    _, rest = rest.split("@", 1)
    return f"{scheme}://{LOGIN_ROLE}@{rest}"


def _make_db(dsn: str, max_size: int) -> Database:
    """Real app.db.Database over the given DSN (SimpleNamespace carries the
    exact Config attributes Database uses)."""
    cfg = SimpleNamespace(
        database_dsn=dsn,
        pg_database=APP_DB,
        pg_min_size=1,
        pg_max_size=max_size,
    )
    return Database(cfg)


@pytest.fixture(scope="module")
def pg(tmp_path_factory):
    server = pgserver.get_server(str(tmp_path_factory.mktemp("pgdata")))
    yield server
    server.cleanup()


@pytest_asyncio.fixture(scope="module")
async def dsns(pg):
    """Bootstrap the conversation DB + schema + app roles once per server;
    returns (super_dsn, app_dsn) for the APP_DB database."""
    super_dsn = pg.get_uri(database="postgres")
    conn = await asyncpg.connect(super_dsn)
    try:
        exists = await conn.fetchval(
            "SELECT 1 FROM pg_database WHERE datname = $1", APP_DB
        )
        if not exists:
            await conn.execute(f'CREATE DATABASE "{APP_DB}"')
        await conn.execute(ROLES_SQL)
    finally:
        await conn.close()

    db_dsn = pg.get_uri(database=APP_DB)
    conn = await asyncpg.connect(db_dsn)
    try:
        await conn.execute(SCHEMA_SQL)
        # 05-app-roles.sql conversation-database section (verbatim shape).
        await conn.execute(f"GRANT USAGE ON SCHEMA public TO {GROUP_ROLE}")
        await conn.execute(
            "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES "
            f"IN SCHEMA public TO {GROUP_ROLE}"
        )
        await conn.execute(
            "GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public "
            f"TO {GROUP_ROLE}"
        )
        # The embedded superuser is 'postgres' (not 'opendesk' as in 05).
        await conn.execute(
            "ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public "
            f"GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO {GROUP_ROLE}"
        )
        await conn.execute(
            "ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA public "
            f"GRANT USAGE, SELECT ON SEQUENCES TO {GROUP_ROLE}"
        )
        # Sanity: the subject role must be non-superuser and NOT the table
        # owner, otherwise FORCE RLS would not apply and the suite would
        # prove nothing.
        is_super = await conn.fetchval(
            "SELECT rolsuper FROM pg_roles WHERE rolname = $1", LOGIN_ROLE
        )
        assert is_super is False, "login role must be non-superuser"
    finally:
        await conn.close()
    return db_dsn, _app_dsn(db_dsn)


@pytest_asyncio.fixture()
async def app_db(dsns):
    """The service's Database connected as app_conversation_login with a
    deliberately tiny pool (size 1) to maximize connection reuse."""
    _, app_dsn = dsns
    db = _make_db(app_dsn, max_size=1)
    await db.connect()
    try:
        yield db
    finally:
        await db.close()


async def _seed(db: Database, tenant_id: uuid.UUID, n: int, tag: str):
    """Create n conversations with one turn each for a tenant; returns
    [(conversation_id, turn_id)]."""
    out = []
    for i in range(n):
        conv = await db.create_conversation(tenant_id, "site-x", "voice")
        turn, created, _outbox_id = await db.add_turn(
            conv["id"], tenant_id, "user", f"{tag}-{i}", None
        )
        assert created
        out.append((conv["id"], turn["id"]))
    return out


# ---------------------------------------------------------------- interleave
async def test_interleaved_tenants_no_cross_reads_pool1(app_db, dsns):
    """Sequential + concurrent A/B interleave on a size-1 pool: every
    checkout reuses the same backend, so any GUC leakage between checkouts
    would surface as cross-tenant reads."""
    _, app_dsn = dsns
    rows_a = await _seed(app_db, TENANT_A, 3, "a")
    rows_b = await _seed(app_db, TENANT_B, 3, "b")

    # Concurrent interleave on size-2 pool: force simultaneous checkouts.
    db2 = _make_db(app_dsn, max_size=2)
    await db2.connect()
    try:
        await asyncio.gather(
            _seed(db2, TENANT_A, 2, "a2"),
            _seed(db2, TENANT_B, 2, "b2"),
        )
    finally:
        await db2.close()

    # Alternating sequential reads on the SAME size-1 pool.
    for (cid_a, _), (cid_b, _) in zip(rows_a, rows_b):
        conv_a = await app_db.get_conversation(cid_a, TENANT_A)
        conv_b = await app_db.get_conversation(cid_b, TENANT_B)
        assert conv_a["tenant_id"] == TENANT_A
        assert conv_b["tenant_id"] == TENANT_B
        # Cross-tenant reads of the other tenant's row: hidden, not leaked.
        with pytest.raises(NotFoundError):
            await app_db.get_conversation(cid_b, TENANT_A)
        with pytest.raises(NotFoundError):
            await app_db.get_conversation(cid_a, TENANT_B)

    listed_a = await app_db.list_conversations(TENANT_A)
    listed_b = await app_db.list_conversations(TENANT_B)
    assert listed_a and all(r["tenant_id"] == TENANT_A for r in listed_a)
    assert listed_b and all(r["tenant_id"] == TENANT_B for r in listed_b)
    assert not ({r["id"] for r in listed_a} & {r["id"] for r in listed_b})

    # Turns are isolated through the parent conversation row.
    for cid, _ in rows_a:
        turns = await app_db.list_turns(cid, TENANT_A)
        assert turns and all(t["conversation_id"] == cid for t in turns)
        # Cross-tenant turn reads see nothing (RLS subquery on the parent).
        assert await app_db.list_turns(cid, TENANT_B) == []
    for cid, _ in rows_b:
        assert await app_db.list_turns(cid, TENANT_A) == []

    # Cross-tenant WRITE is denied: the parent row is invisible, so the
    # turns RLS policy (EXISTS subquery) rejects the insert — no turn can be
    # attached to another tenant's conversation. (FK violation would be the
    # equivalent denial if the parent were merely absent.)
    with pytest.raises(
        (asyncpg.InsufficientPrivilegeError, asyncpg.ForeignKeyViolationError)
    ):
        row, created, _ = await app_db.add_turn(rows_b[0][0], TENANT_A, "user", "x-tenant", None)

    # conversation_exists / conversation_meta never leak across tenants.
    assert await app_db.conversation_exists(rows_a[0][0], TENANT_A)
    assert not await app_db.conversation_exists(rows_a[0][0], TENANT_B)
    assert await app_db.conversation_meta(rows_a[0][0], TENANT_B) is None


# ------------------------------------------------------------- fail closed
async def test_unset_tenant_session_sees_zero_rows(dsns):
    """A non-superuser session that NEVER sets app.tenant_id sees ZERO rows
    on the FORCE-RLS tables — there is no app-level fallback path that can
    read tenant data with the GUC unset."""
    _, app_dsn = dsns
    fresh = await asyncpg.connect(app_dsn)
    try:
        assert await fresh.fetchval(
            "SELECT current_setting('app.tenant_id', true) IS NULL"
        )
        for tbl in ("conversations", "turns"):
            n = await fresh.fetchval(f"SELECT count(*) FROM {tbl}")
            assert n == 0, f"{tbl} readable with app.tenant_id unset"
    finally:
        await fresh.close()


async def test_recycled_empty_guc_session_sees_zero_rows(dsns):
    """The recycled-connection state (app.tenant_id='' — what the asyncpg
    reset leaves behind) fails closed via the NULLIF policy: zero rows and
    no uuid-cast error."""
    _, app_dsn = dsns
    conn = await asyncpg.connect(app_dsn)
    try:
        await conn.execute("SELECT set_config('app.tenant_id', '', false)")
        assert await conn.fetchval(
            "SELECT current_setting('app.tenant_id', true)"
        ) == ""
        for tbl in ("conversations", "turns"):
            n = await conn.fetchval(f"SELECT count(*) FROM {tbl}")
            assert n == 0, f"{tbl} readable with empty-string tenant GUC"
    finally:
        await conn.close()


# ----------------------------------------------------------- pool release
async def test_set_local_auto_resets_and_never_survives_release(app_db):
    """SET LOCAL is transaction-scoped: inside _tenant_tx the GUC is the
    tenant; after commit it is gone; the next checkout of the SAME pooled
    backend carries no tenant context (pool size 1 forces reuse)."""
    await _seed(app_db, TENANT_A, 1, "guc")

    assert app_db._pool is not None
    async with app_db._pool.acquire() as conn:
        async with conn.transaction():
            await conn.execute(
                "SELECT set_config('app.tenant_id', $1, true)", str(TENANT_A)
            )
            assert await conn.fetchval(
                "SELECT current_setting('app.tenant_id', true)"
            ) == str(TENANT_A)
        # Committed: the LOCAL setting auto-reset inside the same session.
        after_commit = await conn.fetchval(
            "SELECT current_setting('app.tenant_id', true)"
        )
        assert after_commit in (None, ""), after_commit

    # Released back to the pool; the next checkout must not inherit it.
    async with app_db._pool.acquire() as conn2:
        leaked = await conn2.fetchval(
            "SELECT current_setting('app.tenant_id', true)"
        )
        assert leaked in (None, ""), f"tenant GUC survived pool release: {leaked}"
        # ...and cannot read tenant rows.
        assert await conn2.fetchval("SELECT count(*) FROM conversations") == 0
        assert await conn2.fetchval("SELECT count(*) FROM turns") == 0


async def test_adversarial_session_set_cleared_by_pool_reset(app_db):
    """Even a (buggy) SESSION-level SET does not leak across checkouts: the
    asyncpg pool reset clears it to '' before the next acquire, and '' is
    deny-by-default under the NULLIF policy. This pins the property the
    J-14 closure relies on: nothing session-scoped survives release."""
    async with app_db._pool.acquire() as conn:
        await conn.execute(
            "SELECT set_config('app.tenant_id', $1, false)", str(TENANT_B)
        )
        assert await conn.fetchval(
            "SELECT current_setting('app.tenant_id', true)"
        ) == str(TENANT_B)
    async with app_db._pool.acquire() as conn2:
        leaked = await conn2.fetchval(
            "SELECT current_setting('app.tenant_id', true)"
        )
        assert leaked in (None, ""), f"session GUC survived pool release: {leaked}"
        assert await conn2.fetchval("SELECT count(*) FROM conversations") == 0


# -------------------------------------------------------------- guard rail
async def test_tenant_tx_refuses_none(app_db):
    """_tenant_tx(None) fails loudly — an unresolved tenant context must
    never run tenant-scoped queries with the GUC unset."""
    with pytest.raises(ValueError):
        async with app_db._tenant_tx(None):  # type: ignore[arg-type]
            pass
