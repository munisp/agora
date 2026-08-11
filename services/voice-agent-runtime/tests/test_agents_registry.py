"""Agents registry client tests (SPEC-W38 F1): TTL cache, fail-open on
404/network/timeout, and payload parsing — with a mocked httpx transport."""

from __future__ import annotations

import httpx
import pytest

from app import agents_registry
from app.agents_registry import AgentRecord, AgentsRegistryClient

AGENT_PAYLOAD = {
    "agent": {
        "id": "agent-1",
        "tenant_id": "t-uuid",
        "tenant_slug": "acme",
        "name": "Front Desk",
        "slug": "front-desk",
        "phone_number": "+15551234567",
        "status": "active",
    },
    "definition": {
        "persona": "You are a warm, unhurried receptionist.",
        "tool_allowlist": ["get_business_info"],
        "context_budget_tokens": 8,
    },
}


def _client(handler, *, ttl: float = 30.0, clock=None) -> tuple[AgentsRegistryClient, list[httpx.Request]]:
    calls: list[httpx.Request] = []

    def _handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return handler(request)

    transport = httpx.MockTransport(_handler)
    http = httpx.AsyncClient(base_url="http://conversation:7007", transport=transport)
    kwargs = {"client": http, "cache_ttl_s": ttl}
    if clock is not None:
        kwargs["clock"] = clock
    return AgentsRegistryClient("http://conversation:7007", **kwargs), calls


# --------------------------------------------------------------------------
# resolve_agent_by_phone
# --------------------------------------------------------------------------
class TestResolve:
    async def test_resolves_agent_with_definition(self):
        client, calls = _client(lambda req: httpx.Response(200, json=AGENT_PAYLOAD))
        record = await client.resolve_agent_by_phone("+15551234567")
        assert record is not None
        assert record.id == "agent-1"
        assert record.tenant_slug == "acme"
        assert record.definition is not None
        assert record.definition.persona.startswith("You are a warm")
        assert record.definition.tool_allowlist == ["get_business_info"]
        assert record.definition.context_budget_tokens == 8
        # query param contract
        assert calls[0].url.path == "/v1/agents/resolve"
        assert calls[0].url.params["phone"] == "+15551234567"

    async def test_404_fails_open_to_none(self):
        client, _ = _client(lambda req: httpx.Response(404, json={"detail": "not found"}))
        assert await client.resolve_agent_by_phone("+49999000") is None

    async def test_network_error_fails_open_to_none(self):
        def boom(req):
            raise httpx.ConnectError("connection refused", request=req)

        client, _ = _client(boom)
        assert await client.resolve_agent_by_phone("+15551234567") is None

    async def test_timeout_fails_open_to_none(self):
        def boom(req):
            raise httpx.ReadTimeout("timed out", request=req)

        client, _ = _client(boom)
        assert await client.resolve_agent_by_phone("+15551234567") is None

    async def test_unexpected_status_fails_open(self):
        client, _ = _client(lambda req: httpx.Response(500, text="boom"))
        assert await client.resolve_agent_by_phone("+15551234567") is None

    async def test_malformed_payload_fails_open_and_is_not_cached(self):
        client, calls = _client(lambda req: httpx.Response(200, json={"unexpected": True}))
        assert await client.resolve_agent_by_phone("+15551234567") is None
        assert await client.resolve_agent_by_phone("+15551234567") is None
        # malformed 200s must not pin a miss for the whole TTL
        assert len(calls) == 2

    async def test_empty_phone_short_circuits(self):
        client, calls = _client(lambda req: httpx.Response(200, json=AGENT_PAYLOAD))
        assert await client.resolve_agent_by_phone("") is None
        assert calls == []


# --------------------------------------------------------------------------
# TTL cache
# --------------------------------------------------------------------------
class TestCache:
    async def test_hit_cached_within_ttl(self):
        client, calls = _client(lambda req: httpx.Response(200, json=AGENT_PAYLOAD))
        first = await client.resolve_agent_by_phone("+15551234567")
        second = await client.resolve_agent_by_phone("+15551234567")
        assert first is second
        assert len(calls) == 1

    async def test_miss_cached_within_ttl(self):
        client, calls = _client(lambda req: httpx.Response(404))
        assert await client.resolve_agent_by_phone("+49999000") is None
        assert await client.resolve_agent_by_phone("+49999000") is None
        assert len(calls) == 1

    async def test_cache_expires_after_ttl(self):
        now = [1000.0]
        client, calls = _client(
            lambda req: httpx.Response(200, json=AGENT_PAYLOAD),
            ttl=30.0,
            clock=lambda: now[0],
        )
        await client.resolve_agent_by_phone("+15551234567")
        now[0] += 29.0
        await client.resolve_agent_by_phone("+15551234567")
        assert len(calls) == 1
        now[0] += 2.0  # past the 30s TTL
        await client.resolve_agent_by_phone("+15551234567")
        assert len(calls) == 2

    async def test_phone_and_id_caches_are_separate(self):
        def handler(req):
            if req.url.path == "/v1/agents/resolve":
                return httpx.Response(200, json=AGENT_PAYLOAD)
            return httpx.Response(200, json=AGENT_PAYLOAD["agent"])

        client, calls = _client(handler)
        await client.resolve_agent_by_phone("+15551234567")
        await client.get_agent("agent-1")
        assert len(calls) == 2


# --------------------------------------------------------------------------
# get_agent
# --------------------------------------------------------------------------
class TestGetAgent:
    async def test_bare_agent_object_payload(self):
        client, calls = _client(lambda req: httpx.Response(200, json=AGENT_PAYLOAD["agent"]))
        record = await client.get_agent("agent-1")
        assert record is not None
        assert record.id == "agent-1"
        assert calls[0].url.path == "/v1/agents/agent-1"

    async def test_404_fails_open(self):
        client, _ = _client(lambda req: httpx.Response(404))
        assert await client.get_agent("missing") is None


# --------------------------------------------------------------------------
# payload parsing + shared client wiring
# --------------------------------------------------------------------------
class TestPayloadParsing:
    def test_envelope_with_embedded_definition(self):
        record = AgentRecord.from_payload(AGENT_PAYLOAD)
        assert record is not None
        assert record.definition is not None
        assert record.definition.tool_allowlist == ["get_business_info"]

    def test_definition_on_agent_object(self):
        payload = {"id": "a1", "definition": {"instructions": "Be brief."}}
        record = AgentRecord.from_payload(payload)
        assert record is not None
        assert record.definition is not None
        assert record.definition.instructions == "Be brief."

    def test_agent_without_definition(self):
        record = AgentRecord.from_payload({"id": "a1", "name": "x"})
        assert record is not None
        assert record.definition is None

    def test_garbage_payloads(self):
        assert AgentRecord.from_payload(None) is None
        assert AgentRecord.from_payload("nope") is None
        assert AgentRecord.from_payload({"agent": {"name": "no id"}}) is None

    def test_unknown_keys_tolerated(self):
        record = AgentRecord.from_payload(
            {"id": "a1", "definition": {"persona": "p", "future_key": {"x": 1}}}
        )
        assert record is not None
        assert record.definition.persona == "p"


class TestSharedClient:
    def test_disabled_when_url_empty(self):
        agents_registry.set_registry_client(None)
        from types import SimpleNamespace

        settings = SimpleNamespace(agents_registry_url="", agents_cache_ttl_s=30)
        assert agents_registry.get_registry_client(settings) is None
        agents_registry.set_registry_client(None)

    def test_builds_from_settings_once(self):
        agents_registry.set_registry_client(None)
        from types import SimpleNamespace

        settings = SimpleNamespace(
            agents_registry_url="http://conversation:7007", agents_cache_ttl_s=30
        )
        first = agents_registry.get_registry_client(settings)
        assert first is agents_registry.get_registry_client(settings)
        agents_registry.set_registry_client(None)
