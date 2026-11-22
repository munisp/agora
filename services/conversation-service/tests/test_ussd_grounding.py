"""USSD knowledge-grounding tests (SPEC-W108 Part B).

Pass-through text mode: a non-empty free-text selection triggers a
knowledge-service GET /v1/context lookup (tenant-bound via X-Tenant-Slugs,
tight per-call timeout for the ~1s USSD budget). Grounded snippets replace
the static ack; ANY failure falls back to the ack — the callback never
fails because of grounding. Menu mode and the empty first turn never call
the KB.
"""

from __future__ import annotations

import asyncio
import contextlib
import sys
import time
import uuid
from datetime import UTC, datetime

import pytest

sys.path.insert(0, ".")

from app import incidents, ussd  # noqa: E402
from app.config import Config  # noqa: E402
from app.db import NotFoundError  # noqa: E402

pytestmark = pytest.mark.asyncio

TENANT = uuid.uuid4()
SESSION = "ATUid_grounding_session"
ACK = Config().ussd_text_mode_reply


class _FakeConn:
    async def execute(self, *a, **kw):
        return None


class _FakeDB:
    def __init__(self):
        self.convs = {}
        self.turns = {}
        self.outbox = {}
        self.emitted = {}

    async def get_conversation(self, cid, tenant_id):
        if cid not in self.convs:
            raise NotFoundError(f"conversation {cid} not found")
        return self.convs[cid]

    async def create_conversation(self, tenant_id, site_slug, channel,
                                  contact_phone, conversation_id=None):
        cid = conversation_id or uuid.uuid4()
        self.convs[cid] = dict(id=cid, tenant_id=tenant_id, site_slug=site_slug,
                               channel=channel, contact_phone=contact_phone)
        return self.convs[cid]

    async def add_turn(self, cid, tenant_id, role, text, tool_calls,
                       sentiment=None, intent=None, entities=None,
                       idempotency_key=None, outbox=None):
        existing = self.turns.get(cid, [])
        if idempotency_key:
            for t in existing:
                if t["idempotency_key"] == idempotency_key:
                    return t, False, None
        rec = dict(id=uuid.uuid4(), conversation_id=cid, seq=len(existing) + 1,
                   role=role, text=text, tool_calls=tool_calls,
                   idempotency_key=idempotency_key, ts=datetime.now(UTC))
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
        if outbox_id in self.outbox:
            self.outbox[outbox_id]["sent"] = True

    async def incident_emit_record(self, tenant_id, dedupe_key, build):
        return None, "duplicate"


class _FakeSink:
    async def publish(self, rec):
        return None


class _KbDapr:
    """Fake Dapr with an invoke() scripted for the knowledge leg."""

    def __init__(self, outcome):
        self.outcome = outcome
        self.invocations = []

    async def invoke(self, app_id, method, *, params=None, headers=None,
                     timeout_s=None):
        self.invocations.append(
            {"app_id": app_id, "method": method, "params": params,
             "headers": headers, "timeout_s": timeout_s}
        )
        if isinstance(self.outcome, Exception):
            raise self.outcome
        return self.outcome

    async def publish_event(self, topic, event):
        return None


def _app(cfg=None, kb_outcome=None):
    from fastapi import FastAPI

    from app.logging import get_logger
    from app.routes import router

    if kb_outcome is None:
        kb_outcome = {"snippets": ["We open 9am to 5pm, Monday to Friday."]}

    @contextlib.asynccontextmanager
    async def lifespan(app):
        app.state.cfg = cfg or Config()
        app.state.db = _FakeDB()
        app.state.sink = _FakeSink()
        app.state.intel_sink = _FakeSink()
        app.state.dapr = _KbDapr(kb_outcome)
        app.state.log = get_logger("ussd-grounding-test")
        yield

    app = FastAPI(lifespan=lifespan)
    app.include_router(router)
    return app


def _post(client, **over):
    body = {
        "tenant_id": str(TENANT),
        "site_slug": "acme",
        "session_id": SESSION,
        "service_code": "*384*99#",
        "phone_number": "+2348012345678",
        "text": "",
        "menu": None,
    }
    body.update(over)
    return client.post("/v1/ussd/turns", json=body)


@pytest.fixture(autouse=True)
def _clean_dedupe():
    incidents._reset_dedupe()
    yield
    incidents._reset_dedupe()


def test_grounded_text_mode_reply_replaces_static_ack():
    from fastapi.testclient import TestClient

    app = _app()
    with TestClient(app) as c:
        r = _post(c, text="When do you open?")
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["grounded"] is True
        assert body["reply"] == "We open 9am to 5pm, Monday to Friday."
        assert body["continue"] is True
        # KB contract: tenant binding header + query + tight timeout.
        call = app.state.dapr.invocations[0]
        assert call["app_id"] == "knowledge"
        assert call["method"] == "v1/context"
        assert call["params"] == {"tenant": str(TENANT), "q": "When do you open?"}
        assert call["headers"] == {"X-Tenant-Slugs": str(TENANT)}
        assert call["timeout_s"] == Config().ussd_kb_timeout_s


def test_grounded_reply_truncates_to_char_budget():
    from fastapi.testclient import TestClient

    long_snippet = "word " * 200
    app = _app(kb_outcome={"snippets": [long_snippet]})
    with TestClient(app) as c:
        r = _post(c, text="tell me everything")
        body = r.json()
        assert body["grounded"] is True
        assert len(body["reply"]) <= Config().ussd_kb_max_reply_chars
        assert body["reply"].endswith("…")


def test_kb_timeout_falls_back_to_static_ack():
    from fastapi.testclient import TestClient

    app = _app(kb_outcome=asyncio.TimeoutError("kb slow"))
    with TestClient(app) as c:
        r = _post(c, text="When do you open?")
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["grounded"] is False
        assert body["reply"] == ACK  # callback never fails on grounding


def test_kb_error_falls_back_to_static_ack():
    from fastapi.testclient import TestClient

    app = _app(kb_outcome=RuntimeError("503 sidecar down"))
    with TestClient(app) as c:
        r = _post(c, text="question")
        assert r.json()["grounded"] is False
        assert r.json()["reply"] == ACK


def test_empty_snippets_fall_back_to_static_ack():
    from fastapi.testclient import TestClient

    app = _app(kb_outcome={"snippets": []})
    with TestClient(app) as c:
        r = _post(c, text="obscure question")
        assert r.json()["grounded"] is False
        assert r.json()["reply"] == ACK


def test_grounding_disabled_is_bit_identical_to_pre_w108():
    from fastapi.testclient import TestClient

    cfg = Config(ussd_kb_grounding=False)
    app = _app(cfg=cfg)
    with TestClient(app) as c:
        r = _post(c, text="When do you open?")
        body = r.json()
        assert body["grounded"] is False
        assert body["reply"] == ACK
        assert app.state.dapr.invocations == []  # KB never touched


def test_menu_mode_never_calls_kb():
    from fastapi.testclient import TestClient

    menu = [{"key": "1", "label": "Book appointment", "action": "book"}]
    app = _app()
    with TestClient(app) as c:
        r = _post(c, text="1", menu=menu)
        assert r.json()["grounded"] is False
        assert "Book appointment" in r.json()["reply"]
        assert app.state.dapr.invocations == []


def test_empty_first_turn_never_calls_kb():
    from fastapi.testclient import TestClient

    app = _app()
    with TestClient(app) as c:
        r = _post(c, text="")  # session start: no query to ground
        assert r.json()["grounded"] is False
        assert r.json()["reply"] == ACK
        assert app.state.dapr.invocations == []


# --------------------------------------------- dapr timeout forwarding
def test_dapr_invoke_forwards_per_call_timeout():
    """SPEC-W108: invoke(timeout_s=...) must reach the httpx per-request
    timeout — the USSD 1s budget depends on it (client default is 15s)."""
    from app.dapr_client import DaprClient

    recorded = {}

    class _Resp:
        status_code = 200
        content = b"{}"
        headers = {"content-type": "application/json"}
        text = "{}"

        def json(self):
            return {}

    class _Recorder:
        async def get(self, url, params=None, headers=None, timeout=None):
            recorded["timeout"] = timeout
            return _Resp()

    async def run():
        client = DaprClient("localhost", 3500)
        client._client = _Recorder()
        await client.invoke("knowledge", "v1/context", timeout_s=0.8)

    asyncio.run(run())
    assert recorded["timeout"] == 0.8


# ------------------------------------------------------- pure helper
def test_grounded_reply_helper_collapses_and_limits_snippets():
    out = ussd.grounded_reply(
        ["  We   open\n9am-5pm. ", "", "second snippet", "third snippet"],
        max_snippets=2, max_chars=300,
    )
    assert out == "We open 9am-5pm. second snippet"


def test_grounded_reply_helper_empty_returns_empty():
    assert ussd.grounded_reply([], max_snippets=2, max_chars=300) == ""
    assert ussd.grounded_reply(["  ", "\n"], max_snippets=2, max_chars=300) == ""


def test_grounded_reply_helper_hard_cut_without_space():
    out = ussd.grounded_reply(["x" * 400], max_snippets=1, max_chars=300)
    assert len(out) == 300 and out.endswith("…")
