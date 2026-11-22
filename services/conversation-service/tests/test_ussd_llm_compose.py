"""LLM-compressed USSD answers (SPEC-W110).

Unit tests patch httpx.AsyncClient with a MockTransport-backed client (test
seam, W107 precedent — production builds the real client). Route tests reuse
the W108 wiring idiom; the compose HTTP call is the only httpx user inside
the grounding path (FakeDapr fakes the KB fetch).
"""

from __future__ import annotations

import contextlib
import json
import sys
import uuid
from datetime import UTC, datetime

import httpx
import pytest

sys.path.insert(0, ".")

from app import incidents, kb_compose  # noqa: E402
from app.config import Config  # noqa: E402
from app.db import NotFoundError  # noqa: E402

pytestmark = pytest.mark.asyncio

TENANT = uuid.uuid4()
SESSION = "ATUid_compose_session"
ACK = Config().ussd_text_mode_reply

_REAL_CLIENT = httpx.AsyncClient  # capture before any monkeypatching


def _patch_llm(monkeypatch, handler):
    transport = httpx.MockTransport(handler)
    monkeypatch.setattr(
        httpx, "AsyncClient", lambda **kw: _REAL_CLIENT(transport=transport, **kw)
    )


def _llm_ok(text="We refund within 30 days of purchase."):
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={"choices": [{"message": {"content": text}}]},
        )

    return handler


# ------------------------------------------------------- compose_answer unit
async def test_compose_happy_path_payload_and_answer(monkeypatch):
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["payload"] = json.loads(request.content)
        return httpx.Response(
            200, json={"choices": [{"message": {"content": " Refunds: 30 days. "}}]}
        )

    _patch_llm(monkeypatch, handler)
    cfg = Config(ussd_kb_llm=True)
    out = await kb_compose.compose_answer(
        "do you refund?", ["Refunds within 30 days of purchase."], cfg
    )
    assert out == "Refunds: 30 days."
    assert seen["url"] == "http://ollama:11434/v1/chat/completions"
    payload = seen["payload"]
    assert payload["model"] == "qwen3:8b"
    assert payload["temperature"] == 0.0
    assert payload["max_tokens"] == 120
    user = payload["messages"][1]["content"]
    assert "do you refund?" in user
    assert "Refunds within 30 days of purchase." in user


async def test_compose_enforces_ussd_char_budget(monkeypatch):
    _patch_llm(monkeypatch, _llm_ok("word " * 200))
    cfg = Config(ussd_kb_llm=True)
    out = await kb_compose.compose_answer("q", ["fact"], cfg)
    assert out is not None
    assert len(out) <= cfg.ussd_kb_max_reply_chars
    assert out.endswith("…")


async def test_compose_http_error_returns_none(monkeypatch):
    _patch_llm(monkeypatch, lambda req: httpx.Response(500, text="boom"))
    cfg = Config(ussd_kb_llm=True)
    assert await kb_compose.compose_answer("q", ["fact"], cfg) is None


async def test_compose_connect_error_returns_none(monkeypatch):
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("down", request=request)

    _patch_llm(monkeypatch, handler)
    cfg = Config(ussd_kb_llm=True)
    assert await kb_compose.compose_answer("q", ["fact"], cfg) is None


async def test_compose_empty_content_returns_none(monkeypatch):
    _patch_llm(monkeypatch, _llm_ok("   "))
    cfg = Config(ussd_kb_llm=True)
    assert await kb_compose.compose_answer("q", ["fact"], cfg) is None


async def test_compose_empty_inputs_short_circuit():
    cfg = Config(ussd_kb_llm=True)
    assert await kb_compose.compose_answer("  ", ["fact"], cfg) is None
    assert await kb_compose.compose_answer("q", [], cfg) is None


# ------------------------------------------------------- route wiring
class _FakeConn:
    async def execute(self, *a, **kw):
        return None


class _FakeDB:
    def __init__(self):
        self.convs = {}
        self.turns = {}
        self.outbox = {}

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
    def __init__(self):
        self.invocations = []

    async def invoke(self, app_id, method, *, params=None, headers=None,
                     timeout_s=None):
        self.invocations.append({"params": params, "headers": headers})
        return {"snippets": ["Refunds within 30 days of purchase."]}

    async def publish_event(self, topic, event):
        return None


def _app(cfg):
    from fastapi import FastAPI

    from app.logging import get_logger
    from app.routes import router

    @contextlib.asynccontextmanager
    async def lifespan(app):
        app.state.cfg = cfg
        app.state.db = _FakeDB()
        app.state.sink = _FakeSink()
        app.state.intel_sink = _FakeSink()
        app.state.dapr = _KbDapr()
        app.state.log = get_logger("ussd-compose-test")
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
        "text": "do you refund?",
        "menu": None,
    }
    body.update(over)
    return client.post("/v1/ussd/turns", json=body)


@pytest.fixture(autouse=True)
def _clean_dedupe():
    incidents._reset_dedupe()
    yield
    incidents._reset_dedupe()


def test_llm_on_composed_answer_used(monkeypatch):
    from fastapi.testclient import TestClient

    _patch_llm(monkeypatch, _llm_ok())
    app = _app(Config(ussd_kb_llm=True))
    with TestClient(app) as c:
        body = _post(c).json()
        assert body["reply"] == "We refund within 30 days of purchase."
        assert body["grounded"] is True
        assert body["composed"] is True


def test_llm_down_falls_back_to_raw_snippet(monkeypatch):
    from fastapi.testclient import TestClient

    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("ollama down", request=request)

    _patch_llm(monkeypatch, handler)
    app = _app(Config(ussd_kb_llm=True))
    with TestClient(app) as c:
        body = _post(c).json()
        assert body["reply"] == "Refunds within 30 days of purchase."
        assert body["grounded"] is True
        assert body["composed"] is False


def test_llm_off_is_byte_identical_to_w108(monkeypatch):
    from fastapi.testclient import TestClient

    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(str(request.url))
        return httpx.Response(200, json={})

    _patch_llm(monkeypatch, handler)
    app = _app(Config())  # ussd_kb_llm default OFF
    with TestClient(app) as c:
        body = _post(c).json()
        assert body["reply"] == "Refunds within 30 days of purchase."
        assert body["grounded"] is True
        assert body["composed"] is False
        assert calls == []  # the LLM was never contacted
