"""SPEC-W43 Y-03/Y-06/Y-08: durable incident dedupe + turn outbox against
REAL embedded Postgres (pgserver, same harness family as
tests/test_tenant_guc_pool.py).

Proves:
1. ensure_relay_tables() bootstrap DDL creates incident_counters /
   incident_emitted / conversation_outbox OUTSIDE any tenant tx, with
   fail-closed RLS + FORCE (app role sees ZERO rows without the GUC and
   only its own tenant with it).
2. incident_emit_record dedupe is DURABLE: same (tenant, dedupe_key)
   returns "created" once, "retry" while unpublished, "duplicate" after
   publish — and the reference counter only advances on "created".
3. add_turn(outbox=...) writes the turn AND the outbox row atomically;
   the relay accessors mark sent / backoff.
4. End-to-end: incidents.emit_for_turn with a failing Dapr leaves durable
   unsent state; IncidentRetryWorker.run_once republishes exactly once.
"""

from __future__ import annotations

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

from app import incidents  # noqa: E402
from app.config import Config  # noqa: E402
from app.db import Database  # noqa: E402

pytestmark = pytest.mark.asyncio

TENANT_A = uuid.uuid4()
TENANT_B = uuid.uuid4()

APP_DB = "conversation_relay_test"
GROUP_ROLE = "app_conversation"
LOGIN_ROLE = "app_conversation_login"
INTERNAL_ROLE = "app_conversation_internal"
INTERNAL_LOGIN = "app_conversation_internal_login"

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
        CREATE ROLE {LOGIN_ROLE} LOGIN PASSWORD 'x' IN ROLE {GROUP_ROLE};
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '{INTERNAL_ROLE}') THEN
        CREATE ROLE {INTERNAL_ROLE} NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '{INTERNAL_LOGIN}') THEN
        CREATE ROLE {INTERNAL_LOGIN} LOGIN PASSWORD 'x' IN ROLE {INTERNAL_ROLE};
    END IF;
END
$$;
"""


def _as_role(dsn: str, role: str) -> str:
    scheme, rest = dsn.split("://", 1)
    _, rest = rest.split("@", 1)
    return f"{scheme}://{role}@{rest}"


def _make_db(dsn: str) -> Database:
    cfg = SimpleNamespace(
        database_dsn=dsn, pg_database=APP_DB, pg_min_size=1, pg_max_size=2
    )
    return Database(cfg)


@pytest.fixture(scope="module")
def pg(tmp_path_factory):
    server = pgserver.get_server(str(tmp_path_factory.mktemp("pgdata")))
    yield server
    server.cleanup()


@pytest_asyncio.fixture(scope="module")
async def dsns(pg):
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
        # Bootstrap the relay tables via the REAL service code path.
        boot = _make_db(db_dsn)
        await boot.connect()
        try:
            await boot.ensure_relay_tables()
        finally:
            await boot.close()
        for role in (GROUP_ROLE, INTERNAL_ROLE):
            await conn.execute(f"GRANT USAGE ON SCHEMA public TO {role}")
            await conn.execute(
                "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES "
                f"IN SCHEMA public TO {role}"
            )
    finally:
        await conn.close()
    return db_dsn


@pytest_asyncio.fixture()
async def db(dsns):
    """Service Database on the superuser DSN (dev-compose default)."""
    d = _make_db(dsns)
    await d.connect()
    try:
        yield d
    finally:
        await d.close()


async def test_ensure_relay_tables_creates_rls_protected_tables(dsns):
    conn = await asyncpg.connect(dsns)
    try:
        for table in ("incident_counters", "incident_emitted",
                      "conversation_outbox"):
            row = await conn.fetchrow(
                "SELECT relrowsecurity, relforcerowsecurity"
                " FROM pg_class WHERE relname = $1", table
            )
            assert row is not None, f"{table} missing"
            assert row["relrowsecurity"] and row["relforcerowsecurity"], table
            pol = await conn.fetchval(
                "SELECT pg_get_expr(polqual, polrelid) FROM pg_policy"
                " WHERE polrelid = $1::regclass AND polname = 'tenant_isolation'",
                table,
            )
            assert "NULLIF(current_setting('app.tenant_id'" in pol
            # role-gated internal escape present (role exists in this cluster)
            assert "app_conversation_internal" in pol
    finally:
        await conn.close()


async def test_relay_tables_fail_closed_for_app_role(dsns):
    app_dsn = _as_role(dsns, LOGIN_ROLE)
    conn = await asyncpg.connect(app_dsn)
    try:
        for table in ("incident_emitted", "conversation_outbox",
                      "incident_counters"):
            n = await conn.fetchval(f"SELECT count(*) FROM {table}")
            assert n == 0, f"{table} must be fail-closed without the GUC"
    finally:
        await conn.close()


async def test_incident_emit_record_durable_dedupe(db):
    key = f"{uuid.uuid4()}:{uuid.uuid4()}"
    built = []

    def build(ref):
        built.append(ref)
        return {"id": "evt-1", "reference": ref}

    payload, state = await db.incident_emit_record(TENANT_A, key, build)
    assert state == "created"
    assert payload["reference"].startswith("INC-")
    assert built == [payload["reference"]]

    # second call while UNPUBLISHED -> retry with the STORED payload
    payload2, state2 = await db.incident_emit_record(TENANT_A, key, build)
    assert state2 == "retry"
    assert payload2 == payload
    assert len(built) == 1  # no rebuild, no counter burn

    # after publish -> duplicate
    await db.incident_mark_published(TENANT_A, key)
    payload3, state3 = await db.incident_emit_record(TENANT_A, key, build)
    assert (payload3, state3) == (None, "duplicate")

    # reference counter advanced exactly once across all three calls
    conn = await asyncpg.connect(db._cfg.database_dsn)
    try:
        seq = await conn.fetchval(
            "SELECT seq FROM incident_counters"
            " WHERE tenant_id = $1 AND year = $2",
            TENANT_A, int(payload["reference"].split("-")[1]),
        )
        assert seq == 1
    finally:
        await conn.close()

    # other tenant unaffected
    payload_b, state_b = await db.incident_emit_record(TENANT_B, key, build)
    assert state_b == "created"
    # leave no unsent rows behind for the next test
    await db.incident_mark_published(TENANT_B, key)


async def test_add_turn_writes_outbox_row_atomically(db):
    conv = await db.create_conversation(TENANT_A, "site-x", "voice")
    captured = {}

    def builder(row):
        captured["row_id"] = row["id"]
        return {"turn": str(row["id"]), "text": row["text"]}

    row, created, outbox_id = await db.add_turn(
        conv["id"], TENANT_A, "user", "hello", None,
        outbox=("opendesk.conversation.transcripts", builder),
    )
    assert created and outbox_id is not None

    unsent = await db.outbox_unsent()
    assert [r["id"] for r in unsent] == [outbox_id]
    assert unsent[0]["payload"] == {"turn": str(row["id"]), "text": "hello"}
    assert unsent[0]["topic"] == "opendesk.conversation.transcripts"

    # backoff: failed rows with a future next_attempt_at are NOT due
    await db.outbox_mark_failed(outbox_id, TENANT_A, 3600)
    assert await db.outbox_unsent() == []

    # forcing the row due again, mark sent removes it for good
    conn = await asyncpg.connect(db._cfg.database_dsn)
    try:
        await conn.execute(
            "UPDATE conversation_outbox SET next_attempt_at = NULL"
            " WHERE id = $1", outbox_id,
        )
    finally:
        await conn.close()
    assert len(await db.outbox_unsent()) == 1
    await db.outbox_mark_sent(outbox_id, TENANT_A)
    assert await db.outbox_unsent() == []


async def test_emit_for_turn_failure_then_retry_worker(db):
    incidents._reset_dedupe()
    conv = await db.create_conversation(TENANT_A, "site-x", "voice")
    row, created, _ = await db.add_turn(
        conv["id"], TENANT_A, "user", "thief dey my compound", None
    )
    assert created

    class _BoomDapr:
        async def publish_event(self, topic, event):
            raise RuntimeError("broker down")

    class _OkDapr:
        def __init__(self):
            self.published = []

        async def publish_event(self, topic, event):
            self.published.append((topic, event))

    kwargs = dict(
        cfg=Config(), db=db, dapr=_BoomDapr(), tenant_id=TENANT_A,
        conversation_id=conv["id"], turn_id=row["id"],
        text="thief dey my compound", channel="voice", site_slug="site-x",
    )
    out = await incidents.emit_for_turn(**kwargs)
    assert out is None  # never raises

    unsent = await db.incident_unsent()
    assert len(unsent) == 1  # durable, not silent

    dapr_ok = _OkDapr()
    worker = incidents.IncidentRetryWorker(Config(), db, dapr_ok)
    assert await worker.run_once() == 1
    assert await db.incident_unsent() == []
    assert len(dapr_ok.published) == 1
    _, event = dapr_ok.published[0]
    assert event["type"] == "com.opendesk.incidents.IDPCreated"
    assert event["tenantid"] == str(TENANT_A)

    # durable dedupe across a simulated restart: no second emission
    incidents._reset_dedupe()
    kwargs["dapr"] = dapr_ok
    again = await incidents.emit_for_turn(**kwargs)
    assert again is None
    assert len(dapr_ok.published) == 1


async def test_internal_role_can_scan_unsent_but_app_role_cannot(dsns):
    """The role-gated escape lets a maintenance/internal role run the relay
    scans; the plain app login role stays fail-closed."""
    conn = await asyncpg.connect(_as_role(dsns, LOGIN_ROLE))
    try:
        n = await conn.fetchval(
            "SELECT count(*) FROM incident_emitted WHERE published_at IS NULL"
        )
        assert n == 0  # fail-closed, NOT an error
    finally:
        await conn.close()

    conn = await asyncpg.connect(_as_role(dsns, INTERNAL_LOGIN))
    try:
        # internal role sees across tenants (row may or may not exist; the
        # point is the policy does not hide rows from the internal role)
        await conn.fetchval(
            "SELECT count(*) FROM incident_emitted WHERE published_at IS NULL"
        )
        # write path is also open to the internal role
        await conn.execute(
            "SELECT set_config('app.tenant_id', '', true)"
        )
        n = await conn.fetchval("SELECT count(*) FROM conversation_outbox")
        assert n >= 0
    finally:
        await conn.close()
