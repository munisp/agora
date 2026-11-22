"""Grounding-quality metrics tests (SPEC-W112).

Every eligible per-turn knowledge lookup (non-empty query, grounding not
disabled) increments voice_knowledge_grounding_total exactly once with
result hit|miss|timeout|error, labeled by bounded tenant and channel.
Tenant label cardinality is capped by GROUNDING_METRICS_MAX_TENANTS;
tenants beyond the cap aggregate under tenant="overflow".

REDIRECTION PROOF (SPEC-W112 R1): with the metrics.finally block removed
from fetch_turn_knowledge, the hit/miss/timeout/error tests here FAIL.
R4: with the tenant cap removed, the overflow test FAILS.
"""

from __future__ import annotations

import asyncio
import sys
from types import SimpleNamespace

sys.path.insert(0, ".")

import pytest

from app import metrics
from app.chat import ChatService
from app.session_state import SessionStore
from app.tenant_context import fetch_turn_knowledge

from conftest import FakeDapr
from test_w93_elevenlabs_dtmf import _settings

pytestmark = pytest.mark.asyncio


def _series(registry: metrics.Registry, **want: str) -> float:
    """Sum of the grounding counter for the given label subset."""
    total = 0.0
    for key, value in registry.knowledge_grounding._values.items():
        labels = dict(key)
        if all(labels.get(k) == v for k, v in want.items()):
            total += value
    return total


# ------------------------------------------------- fetch-level outcomes
async def test_hit_metric_recorded():
    registry = metrics.reset_registry()
    dapr = FakeDapr()
    dapr.get_responses["v1/context"] = {"snippets": ["Refunds within 30 days."]}
    out = await fetch_turn_knowledge(
        dapr, _settings(), "acme", "refund policy?", timeout_s=1.5, channel="web"
    )
    assert out == ["Refunds within 30 days."]
    assert _series(registry, tenant="acme", channel="web", result="hit") == 1.0


async def test_miss_metric_recorded_on_empty_snippets():
    registry = metrics.reset_registry()
    dapr = FakeDapr()
    dapr.get_responses["v1/context"] = {"snippets": []}
    out = await fetch_turn_knowledge(
        dapr, _settings(), "acme", "anything", timeout_s=1.5, channel="web"
    )
    assert out == []
    assert _series(registry, tenant="acme", channel="web", result="miss") == 1.0


async def test_timeout_metric_recorded():
    registry = metrics.reset_registry()

    class _Slow(FakeDapr):
        async def invoke_get(self, app_id, method, *, params=None, headers=None):
            await asyncio.sleep(5)
            return {}

    out = await fetch_turn_knowledge(
        _Slow(), _settings(), "acme", "q", timeout_s=0.05, channel="web"
    )
    assert out == []
    assert _series(registry, tenant="acme", channel="web", result="timeout") == 1.0


async def test_error_metric_recorded():
    registry = metrics.reset_registry()

    class _Down(FakeDapr):
        async def invoke_get(self, app_id, method, *, params=None, headers=None):
            raise RuntimeError("503 knowledge down")

    out = await fetch_turn_knowledge(
        _Down(), _settings(), "acme", "q", timeout_s=1.5, channel="web"
    )
    assert out == []
    assert _series(registry, tenant="acme", channel="web", result="error") == 1.0


async def test_empty_query_is_not_counted():
    registry = metrics.reset_registry()
    dapr = FakeDapr()
    out = await fetch_turn_knowledge(
        dapr, _settings(), "acme", "   ", timeout_s=1.5, channel="web"
    )
    assert out == []
    assert registry.knowledge_grounding._values == {}


async def test_unknown_channel_maps_to_other():
    registry = metrics.reset_registry()
    dapr = FakeDapr()
    dapr.get_responses["v1/context"] = {"snippets": ["s"]}
    await fetch_turn_knowledge(
        dapr, _settings(), "acme", "q", timeout_s=1.5, channel="carrier pigeon"
    )
    assert _series(registry, channel="other", result="hit") == 1.0


# ------------------------------------------------- tenant cardinality cap
async def test_tenant_label_overflow_after_cap():
    registry = metrics.reset_registry()
    dapr = FakeDapr()
    dapr.get_responses["v1/context"] = {"snippets": ["s"]}
    settings = _settings(grounding_metrics_max_tenants=2)
    for slug in ("tenant-a", "tenant-b", "tenant-c", "tenant-d"):
        await fetch_turn_knowledge(
            dapr, settings, slug, "q", timeout_s=1.5, channel="web"
        )
    assert _series(registry, tenant="tenant-a", result="hit") == 1.0
    assert _series(registry, tenant="tenant-b", result="hit") == 1.0
    # tenants beyond the cap aggregate under overflow
    assert _series(registry, tenant="overflow", result="hit") == 2.0
    # no raw third/fourth tenant labels leaked into the series
    assert _series(registry, tenant="tenant-c") == 0.0
    assert _series(registry, tenant="tenant-d") == 0.0
    assert len({dict(k)["tenant"] for k in registry.knowledge_grounding._values}) == 3


async def test_metrics_failure_never_fails_turn(monkeypatch):
    dapr = FakeDapr()
    dapr.get_responses["v1/context"] = {"snippets": ["s"]}

    def boom(**kwargs):
        raise RuntimeError("registry exploded")

    monkeypatch.setattr(
        "app.tenant_context.metrics.knowledge_grounding", boom
    )
    out = await fetch_turn_knowledge(
        dapr, _settings(), "acme", "q", timeout_s=1.5, channel="web"
    )
    assert out == ["s"]  # turn unaffected


# ------------------------------------------------- chat-path channel plumbing
class _StubLLM:
    async def chat_with_tools(self, messages, tools):
        return SimpleNamespace(content="Sure!", tool_calls=[])

    async def stream_with_tools(self, messages, tools):
        async def gen():
            yield SimpleNamespace(
                choices=[
                    SimpleNamespace(delta=SimpleNamespace(content="Sure!", tool_calls=[]))
                ]
            )

        return gen()


async def test_handle_message_passes_channel_to_grounding_metric(monkeypatch):
    registry = metrics.reset_registry()
    from app import chat as chat_module
    from app.tenant_context import TenantContext

    async def fake_fetch(dapr, settings_, site_slug):
        ctx = TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")
        ctx.knowledge_snippets = []
        return ctx

    monkeypatch.setattr(chat_module, "fetch_tenant_context", fake_fetch)
    dapr = FakeDapr()
    dapr.get_responses["v1/context"] = {"snippets": ["turn snippet"]}
    service = ChatService(
        settings=_settings(knowledge_turn_queries=True),
        dapr=dapr,
        llm=_StubLLM(),
        sessions=SessionStore(),
    )
    await service.handle_message(
        site_slug="demo",
        message="hello from whatsapp",
        conversation_id=None,
        channel="whatsapp",
    )
    assert _series(registry, tenant="acme", channel="whatsapp", result="hit") == 1.0
    assert _series(registry, channel="web") == 0.0
