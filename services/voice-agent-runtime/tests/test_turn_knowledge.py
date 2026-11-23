"""Per-turn knowledge grounding tests (SPEC-W109).

The chat path grounds EACH turn: the (post-translation) user message becomes
the knowledge-service /v1/context query, tenant-bound via X-Tenant-Slugs
(W108 posture), under a tight best-effort timeout. Turn snippets lead the
bootstrap snippets in the system prompt; any failure keeps the bootstrap
snippets and never fails the turn.
"""

from __future__ import annotations

import asyncio
import sys
from types import SimpleNamespace

sys.path.insert(0, ".")

import pytest

from app.chat import ChatService
from app.session_state import SessionStore
from app.tenant_context import TenantContext, fetch_turn_knowledge

from conftest import FakeDapr
from test_w93_elevenlabs_dtmf import _settings

pytestmark = pytest.mark.asyncio


# ------------------------------------------------- fetch_turn_knowledge unit
async def test_turn_fetch_contract_params_and_header():
    dapr = FakeDapr()
    dapr.get_responses["v1/context"] = {"snippets": ["Refunds within 30 days."]}
    settings = _settings()
    out = await fetch_turn_knowledge(
        dapr, settings, "acme", "what is your refund policy?", timeout_s=1.5
    )
    assert out == ["Refunds within 30 days."]
    app_id, method, params, headers = dapr.invoke_get_calls[0]
    assert app_id == "knowledge"
    assert method == "v1/context"
    assert params == {"tenant": "acme", "q": "what is your refund policy?"}
    assert headers == {"X-Tenant-Slugs": "acme"}


async def test_turn_fetch_empty_query_short_circuits():
    dapr = FakeDapr()
    out = await fetch_turn_knowledge(dapr, _settings(), "acme", "   ", timeout_s=1.5)
    assert out == []
    assert dapr.invoke_get_calls == []


async def test_turn_fetch_error_returns_empty():
    class _Down(FakeDapr):
        async def invoke_get(self, app_id, method, *, params=None, headers=None):
            raise RuntimeError("503 knowledge down")

    out = await fetch_turn_knowledge(_Down(), _settings(), "acme", "q", timeout_s=1.5)
    assert out == []


async def test_turn_fetch_timeout_returns_empty():
    class _Slow(FakeDapr):
        async def invoke_get(self, app_id, method, *, params=None, headers=None):
            await asyncio.sleep(5)
            return {}

    out = await fetch_turn_knowledge(_Slow(), _settings(), "acme", "q", timeout_s=0.05)
    assert out == []


# ------------------------------------------------- chat path integration
class _StubLLM:
    def __init__(self) -> None:
        self.seen: list[list[dict]] = []  # the messages of every request, copied as sent

    async def chat_with_tools(self, messages, tools):
        self.seen.append([dict(m) for m in messages])
        return SimpleNamespace(content="Sure!", tool_calls=[])

    async def stream_with_tools(self, messages, tools):
        async def gen():
            yield SimpleNamespace(
                choices=[SimpleNamespace(delta=SimpleNamespace(content="Sure!", tool_calls=[]))]
            )

        return gen()


def _ctx_with_bootstrap() -> TenantContext:
    ctx = TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")
    ctx.knowledge_snippets = ["bootstrap: we open 9am-5pm"]
    return ctx


def _service(monkeypatch, settings) -> tuple[ChatService, FakeDapr]:
    from app import chat as chat_module

    async def fake_fetch(dapr, settings_, site_slug):
        return _ctx_with_bootstrap()

    monkeypatch.setattr(chat_module, "fetch_tenant_context", fake_fetch)
    dapr = FakeDapr()
    dapr.get_responses["v1/context"] = {"snippets": ["turn: refunds within 30 days"]}
    service = ChatService(
        settings=settings, dapr=dapr, llm=_StubLLM(), sessions=SessionStore()
    )
    return service, dapr


def _system_prompt(service: ChatService, conversation_id: str) -> str:
    history = service._histories.messages[conversation_id]
    assert history[0]["role"] == "system"
    return history[0]["content"]


async def test_chat_turn_grounded_by_per_turn_query(monkeypatch):
    service, dapr = _service(monkeypatch, _settings(knowledge_turn_queries=True))
    result = await service.handle_message(
        site_slug="demo", message="what is your refund policy?", conversation_id=None
    )
    # the KB query was THIS turn's message, not the static bootstrap query
    kb_calls = [c for c in dapr.invoke_get_calls if c[1] == "v1/context"]
    assert len(kb_calls) == 1
    assert kb_calls[0][2]["q"] == "what is your refund policy?"
    assert kb_calls[0][3] == {"X-Tenant-Slugs": "acme"}
    # 2026-11-23: the bootstrap snippet stays in the stable system prompt; this turn's snippet rides the user's
    # message for that request only (the system prompt never changes per turn, so the GPU server can reuse it) ...
    prompt = _system_prompt(service, result["conversation_id"])
    assert "bootstrap: we open 9am-5pm" in prompt
    assert "turn: refunds within 30 days" not in prompt
    sent_user = [m for m in service._llm.seen[-1] if m["role"] == "user"][-1]["content"]
    assert sent_user.startswith("what is your refund policy?") and "turn: refunds within 30 days" in sent_user
    # ... and the stored history keeps exactly what the customer typed.
    history = service._histories.messages[result["conversation_id"]]
    assert [m["content"] for m in history if m["role"] == "user"][-1] == "what is your refund policy?"


async def test_chat_turn_kb_failure_keeps_bootstrap_grounding(monkeypatch):
    service, dapr = _service(monkeypatch, _settings(knowledge_turn_queries=True))

    class _Down(FakeDapr):
        async def invoke_get(self, app_id, method, *, params=None, headers=None):
            raise RuntimeError("503")

    service._dapr = _Down()
    result = await service.handle_message(
        site_slug="demo", message="anything", conversation_id=None
    )
    assert result["reply"] == "Sure!"  # the turn NEVER fails on grounding
    prompt = _system_prompt(service, result["conversation_id"])
    assert "bootstrap: we open 9am-5pm" in prompt


async def test_chat_turn_grounding_disabled_makes_no_kb_call(monkeypatch):
    service, dapr = _service(monkeypatch, _settings(knowledge_turn_queries=False))
    result = await service.handle_message(
        site_slug="demo", message="refunds?", conversation_id=None
    )
    kb_calls = [c for c in dapr.invoke_get_calls if c[1] == "v1/context"]
    assert kb_calls == []
    prompt = _system_prompt(service, result["conversation_id"])
    # pre-W109 behavior: bootstrap snippets only
    assert "bootstrap: we open 9am-5pm" in prompt
    assert "turn: refunds" not in prompt
