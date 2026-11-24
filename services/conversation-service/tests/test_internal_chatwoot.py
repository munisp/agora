"""SPEC-W118 §3: internal bridge routes (GET conversation + POST agent
turn with origin="chatwoot") on conversation-service's X-Internal-Token
surface. Offline: fake db/dapr/sinks + FastAPI TestClient, no Postgres,
no Kafka — the _persist_turn path mirrors tests/test_ussd.py's harness."""

from __future__ import annotations

import contextlib
import dataclasses
import sys
import uuid
from datetime import UTC, datetime

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

sys.path.insert(0, ".")

from app import events, internal_routes  # noqa: E402
from app.config import Config  # noqa: E402
from app.db import NotFoundError  # noqa: E402
from app.logging import get_logger  # noqa: E402

pytestmark = pytest.mark.asyncio

TENANT = uuid.uuid4()
CONV_ID = uuid.uuid4()
TOKEN = "test-conversation-internal-token"
HDR = {"X-Internal-Token": TOKEN}


class _FakeConn:
    async def fetchval(self, sql, tenant_id, year):
        return 1

    async def execute(self, *args):
        return None


class _FakeDB:
    """Same shape as test_ussd.py's _FakeDB (turn pipeline stand-in)."""

    def __init__(self):
        self.convs = {
            CONV_ID: dict(
                id=CONV_ID, tenant_id=TENANT, site_slug="acme",
                channel="whatsapp", contact_phone="+2348012345678",
                started_at=datetime.now(UTC), ended_at=None,
            )
        }
        self.turns: dict = {}
        self.conn = _FakeConn()
        self.outbox = {}
        self.emitted = {}

    @contextlib.asynccontextmanager
    async def _tenant_tx(self, tenant_id):
        yield self.conn

    async def get_conversation(self, cid, tenant_id):
        if cid not in self.convs:
            raise NotFoundError(f"conversation {cid} not found")
        return self.convs[cid]

    async def add_turn(self, cid, tenant_id, role, text, tool_calls,
                       sentiment=None, intent=None, entities=None,
                       idempotency_key=None, outbox=None):
        existing = self.turns.get(cid, [])
        if idempotency_key:
            for t in existing:
                if t["idempotency_key"] == idempotency_key:
                    return t, False, None
        rec = dict(id=uuid.uuid4(), conversation_id=cid,
                   seq=len(existing) + 1, role=role, text=text,
                   tool_calls=tool_calls, sentiment=sentiment, intent=intent,
                   entities=entities, idempotency_key=idempotency_key,
                   ts=datetime.now(UTC))
        existing.append(rec)
        self.turns[cid] = existing
        outbox_id = None
        if outbox is not None:
            topic, builder = outbox
            outbox_id = uuid.uuid4()
            self.outbox[outbox_id] = {"payload": builder(rec), "topic": topic,
                                      "sent": False}
        return rec, True, outbox_id

    async def outbox_mark_sent(self, outbox_id, tenant_id):
        self.outbox[outbox_id]["sent"] = True

    async def incident_emit_record(self, tenant_id, dedupe_key, build):
        return None, "duplicate"


class _FakeSink:
    async def publish(self, rec):
        return None


class _FakeDapr:
    def __init__(self):
        self.published: list[tuple[str, dict]] = []

    async def publish_event(self, topic, event):
        self.published.append((topic, event))


def _app(token: str = TOKEN) -> tuple[FastAPI, _FakeDB, _FakeDapr]:
    app = FastAPI()
    app.state.cfg = dataclasses.replace(Config(), internal_token=token)
    db = _FakeDB()
    dapr = _FakeDapr()
    app.state.db = db
    app.state.sink = _FakeSink()
    app.state.intel_sink = _FakeSink()
    app.state.dapr = dapr
    app.state.log = get_logger("internal-chatwoot-test")
    app.include_router(internal_routes.router)
    return app, db, dapr


# --------------------------------------------------------------- token guard
def test_get_conversation_guard_503_when_token_unset():
    app, _db, _dapr = _app(token="")
    with TestClient(app) as c:
        r = c.get(f"/internal/conversations/{CONV_ID}",
                  params={"tenant_id": str(TENANT)}, headers=HDR)
        assert r.status_code == 503


def test_both_routes_guard_401_on_wrong_or_missing_token():
    app, _db, _dapr = _app()
    with TestClient(app) as c:
        assert c.get(f"/internal/conversations/{CONV_ID}",
                     params={"tenant_id": str(TENANT)}).status_code == 401
        assert c.get(f"/internal/conversations/{CONV_ID}",
                     params={"tenant_id": str(TENANT)},
                     headers={"X-Internal-Token": "wrong"}).status_code == 401
        assert c.post(f"/internal/conversations/{CONV_ID}/turns",
                      json={"tenant_id": str(TENANT), "role": "agent",
                            "text": "hi"}).status_code == 401


# --------------------------------------------------------------- GET route
def test_get_conversation_returns_row():
    app, _db, _dapr = _app()
    with TestClient(app) as c:
        r = c.get(f"/internal/conversations/{CONV_ID}",
                  params={"tenant_id": str(TENANT)}, headers=HDR)
        assert r.status_code == 200
        body = r.json()
        assert body["id"] == str(CONV_ID)
        assert body["channel"] == "whatsapp"
        assert body["contact_phone"] == "+2348012345678"
        assert body["site_slug"] == "acme"


def test_get_conversation_404_unknown():
    app, _db, _dapr = _app()
    with TestClient(app) as c:
        r = c.get(f"/internal/conversations/{uuid.uuid4()}",
                  params={"tenant_id": str(TENANT)}, headers=HDR)
        assert r.status_code == 404


# --------------------------------------------------------------- POST turn
def test_role_must_be_agent():
    app, _db, _dapr = _app()
    with TestClient(app) as c:
        for role in ("user", "system", "tool"):
            r = c.post(f"/internal/conversations/{CONV_ID}/turns",
                       json={"tenant_id": str(TENANT), "role": role,
                             "text": "hi"},
                       headers=HDR)
            assert r.status_code == 400, role


def test_empty_text_400():
    app, _db, _dapr = _app()
    with TestClient(app) as c:
        r = c.post(f"/internal/conversations/{CONV_ID}/turns",
                   json={"tenant_id": str(TENANT), "role": "agent",
                         "text": "   "},
                   headers=HDR)
        assert r.status_code == 400


def test_append_agent_turn_publishes_event_with_origin_chatwoot():
    app, db, dapr = _app()
    with TestClient(app) as c:
        r = c.post(f"/internal/conversations/{CONV_ID}/turns",
                   json={"tenant_id": str(TENANT), "role": "agent",
                         "text": "Agent Jane replies",
                         "idempotency_key": "chatwoot-7-42-9001"},
                   headers=HDR)
        assert r.status_code == 200
        body = r.json()
        assert body["created"] is True
        assert body["role"] == "agent"
    # exactly one turn persisted
    assert len(db.turns[CONV_ID]) == 1
    # the published transcript event carries the loop-breaker origin
    published = [e for _t, e in dapr.published
                 if e["type"] == "com.opendesk.conversation.ConversationTurn"]
    assert len(published) == 1
    assert published[0]["data"]["origin"] == "chatwoot"
    assert published[0]["data"]["role"] == "agent"
    # W34 GF3 unchanged: published text is redacted, not raw
    assert published[0]["data"]["redacted"] is True


def test_idempotency_replay_no_side_effects():
    app, db, dapr = _app()
    with TestClient(app) as c:
        payload = {"tenant_id": str(TENANT), "role": "agent",
                   "text": "same reply", "idempotency_key": "chatwoot-7-42-9"}
        r1 = c.post(f"/internal/conversations/{CONV_ID}/turns",
                    json=payload, headers=HDR)
        r2 = c.post(f"/internal/conversations/{CONV_ID}/turns",
                    json=payload, headers=HDR)
        assert r1.json()["created"] is True
        assert r2.json()["created"] is False
    assert len(db.turns[CONV_ID]) == 1
    assert len([e for _t, e in dapr.published
                if e["type"]
                == "com.opendesk.conversation.ConversationTurn"]) == 1


def test_append_404_unknown_conversation():
    app, _db, _dapr = _app()
    with TestClient(app) as c:
        r = c.post(f"/internal/conversations/{uuid.uuid4()}/turns",
                   json={"tenant_id": str(TENANT), "role": "agent",
                         "text": "hi"},
                   headers=HDR)
        assert r.status_code == 404


# --------------------------------------------------------------- event fn
def test_conversation_turn_event_origin_default_is_byte_identical():
    """SPEC-W118 §3: origin=None must NOT add any key (pre-W118 shape)."""
    kwargs = dict(
        conversation_id=uuid.uuid4(), tenant_id=uuid.uuid4(),
        site_slug="acme", role="user", text="hi",
        ts=datetime.now(UTC),
    )
    plain = events.conversation_turn_event(**kwargs)
    assert "origin" not in plain["data"]
    tagged = events.conversation_turn_event(**kwargs, origin="chatwoot")
    assert tagged["data"]["origin"] == "chatwoot"
    # and the rest of the payload is identical
    for key in plain["data"]:
        assert tagged["data"][key] == plain["data"][key]
