"""Avatar presence tests (SPEC-W9 Part A): provider registry, Tavus provider
(mocked httpx), MuseTalk intent provider, additive `avatar` field on
VoiceSessionResponse, and /voice/session wiring (fire-and-forget join)."""

from __future__ import annotations

import json
import sys
import types
from types import SimpleNamespace

import httpx

# app.control_plane imports app.livekit_worker (the heavy LiveKit Agents SDK
# stack) only for ROOM_PREFIX. In this lightweight test env the SDK is not
# installed, so stub that module JUST for the control_plane import, then
# remove it from sys.modules again — leaving it in place would poison
# sibling suites that importorskip("app.livekit_worker"). The room-scoped
# `livekit.api` surface used by the endpoint itself is stubbed per-test by
# the conftest `livekit_stub` fixture.
try:  # pragma: no cover - import shim
    import app.livekit_worker  # noqa: F401
except ModuleNotFoundError:  # pragma: no cover - import shim
    _lw_stub = types.ModuleType("app.livekit_worker")
    _lw_stub.ROOM_PREFIX = "site-"
    sys.modules["app.livekit_worker"] = _lw_stub
    import app.control_plane  # noqa: F401 - binds ROOM_PREFIX while stubbed
    del sys.modules["app.livekit_worker"]

from app.avatar import (
    PROVIDER_NONE,
    AvatarStatus,
    create_provider,
    provider_names,
    resolve_provider_name,
)
from app.avatar.musetalk import MuseTalkProvider
from app.avatar.tavus import (
    TAVUS_CONVERSATIONS_URL,
    TavusProvider,
    build_conversation_payload,
)
from app.config import Settings
from app.control_plane import VoiceSessionResponse, create_app


def _settings(**over) -> Settings:
    base = dict(
        avatar_provider="none",
        tavus_api_key="",
        tavus_replica_id="",
        tavus_persona_id="",
        avatar_renderer="disabled",
        avatar_renderer_mode="mock",
        musetalk_room_agent=False,
    )
    base.update(over)
    return Settings(**base)


def _mock_client(handler) -> httpx.AsyncClient:
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


# --------------------------------------------------------------------------
# Registry + name resolution
# --------------------------------------------------------------------------
class TestRegistry:
    def test_builtin_providers_registered(self):
        assert provider_names() == ["musetalk", "tavus"]

    def test_none_resolves_to_no_provider(self):
        assert create_provider(PROVIDER_NONE, _settings()) is None
        assert create_provider("bogus", _settings()) is None

    def test_create_provider_instances(self):
        assert isinstance(create_provider("tavus", _settings()), TavusProvider)
        assert isinstance(create_provider("musetalk", _settings()), MuseTalkProvider)

    def test_register_rejects_reserved_names(self):
        import pytest

        from app.avatar import register_provider

        with pytest.raises(ValueError):
            register_provider("none", TavusProvider)
        with pytest.raises(ValueError):
            register_provider("", TavusProvider)


class TestResolveProviderName:
    def test_defaults_to_none(self):
        assert resolve_provider_name(_settings()) == "none"

    def test_env_setting(self):
        assert resolve_provider_name(_settings(avatar_provider="tavus")) == "tavus"

    def test_tenant_override_wins(self):
        ctx = SimpleNamespace(avatar_provider="musetalk")
        assert resolve_provider_name(_settings(avatar_provider="tavus"), ctx) == "musetalk"

    def test_tenant_without_attribute_falls_back(self):
        # TenantContext today has no avatar_provider field — defensive getattr.
        ctx = SimpleNamespace(site_slug="acme")
        assert resolve_provider_name(_settings(avatar_provider="tavus"), ctx) == "tavus"

    def test_unknown_names_fall_back_to_none(self):
        assert resolve_provider_name(_settings(avatar_provider="bogus")) == "none"
        ctx = SimpleNamespace(avatar_provider="bogus")
        assert resolve_provider_name(_settings(avatar_provider="tavus"), ctx) == "none"

    def test_blank_override_falls_back(self):
        ctx = SimpleNamespace(avatar_provider="  ")
        assert resolve_provider_name(_settings(avatar_provider="tavus"), ctx) == "tavus"


# --------------------------------------------------------------------------
# AvatarStatus
# --------------------------------------------------------------------------
class TestAvatarStatus:
    def test_as_dict_omits_empty_detail(self):
        assert AvatarStatus(provider="tavus", status="joining").as_dict() == {
            "provider": "tavus",
            "status": "joining",
        }

    def test_as_dict_includes_detail(self):
        d = AvatarStatus(provider="tavus", status="unavailable", detail="x").as_dict()
        assert d == {"provider": "tavus", "status": "unavailable", "detail": "x"}

    def test_invalid_status_rejected(self):
        import pytest

        with pytest.raises(ValueError):
            AvatarStatus(provider="tavus", status="joined")


# --------------------------------------------------------------------------
# Tavus provider
# --------------------------------------------------------------------------
class TestTavusPayload:
    def test_documented_v2_conversations_shape(self):
        payload = build_conversation_payload(
            replica_id="r1", persona_id="p1", room="site-acme"
        )
        assert payload == {
            "replica_id": "r1",
            "persona_id": "p1",
            "properties": {"livekit_room_name": "site-acme"},
        }

    def test_empty_persona_omitted(self):
        payload = build_conversation_payload(replica_id="r1", persona_id="", room="r")
        assert "persona_id" not in payload


class TestTavusProvider:
    def test_check_ready_requires_key_and_replica(self):
        assert TavusProvider(_settings()).check_ready() == "TAVUS_API_KEY is not set"
        assert (
            TavusProvider(_settings(tavus_api_key="k")).check_ready()
            == "TAVUS_REPLICA_ID is not set"
        )
        assert (
            TavusProvider(_settings(tavus_api_key="k", tavus_replica_id="r")).check_ready()
            is None
        )

    async def test_join_success_posts_documented_shape(self):
        seen: dict = {}

        def handler(request: httpx.Request) -> httpx.Response:
            seen["url"] = str(request.url)
            seen["api_key"] = request.headers.get("x-api-key")
            seen["body"] = json.loads(request.content)
            return httpx.Response(200, json={"conversation_id": "c123"})

        provider = TavusProvider(
            _settings(tavus_api_key="secret-k", tavus_replica_id="r1", tavus_persona_id="p1"),
            client=_mock_client(handler),
        )
        status = await provider.join_room("site-acme")
        assert status.status == "joining"
        assert status.provider == "tavus"
        assert seen["url"] == TAVUS_CONVERSATIONS_URL
        assert seen["api_key"] == "secret-k"
        assert seen["body"] == {
            "replica_id": "r1",
            "persona_id": "p1",
            "properties": {"livekit_room_name": "site-acme"},
        }

    async def test_join_http_error_degrades_to_unavailable(self):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(500, text="boom")

        provider = TavusProvider(
            _settings(tavus_api_key="k", tavus_replica_id="r"),
            client=_mock_client(handler),
        )
        status = await provider.join_room("site-acme")
        assert status.status == "unavailable"
        assert "500" in (status.detail or "")

    async def test_join_timeout_never_raises(self):
        def handler(request: httpx.Request) -> httpx.Response:
            raise httpx.ReadTimeout("tavus edge hung")

        provider = TavusProvider(
            _settings(tavus_api_key="k", tavus_replica_id="r"),
            client=_mock_client(handler),
        )
        status = await provider.join_room("site-acme")
        assert status.status == "unavailable"
        assert status.detail

    async def test_join_misconfigured_skips_network(self):
        def handler(request: httpx.Request) -> httpx.Response:  # pragma: no cover
            raise AssertionError("network must not be attempted when misconfigured")

        provider = TavusProvider(_settings(), client=_mock_client(handler))
        status = await provider.join_room("site-acme")
        assert status.status == "unavailable"
        assert status.detail == "TAVUS_API_KEY is not set"


# --------------------------------------------------------------------------
# MuseTalk intent provider
# --------------------------------------------------------------------------
class TestMuseTalkProvider:
    def test_check_ready_gates(self):
        assert "MUSETALK_ROOM_AGENT" in (MuseTalkProvider(_settings()).check_ready() or "")
        assert "AVATAR_RENDERER" in (
            MuseTalkProvider(_settings(musetalk_room_agent=True)).check_ready() or ""
        )
        ready = MuseTalkProvider(
            _settings(musetalk_room_agent=True, avatar_renderer="enabled")
        )
        assert ready.check_ready() is None

    async def test_join_publishes_intent_when_enabled(self):
        provider = MuseTalkProvider(
            _settings(musetalk_room_agent=True, avatar_renderer="enabled")
        )
        status = await provider.join_room("site-acme")
        assert status.status == "joining"
        assert status.provider == "musetalk"

    async def test_join_unavailable_without_renderer(self):
        provider = MuseTalkProvider(_settings(musetalk_room_agent=True))
        status = await provider.join_room("site-acme")
        assert status.status == "unavailable"


# --------------------------------------------------------------------------
# VoiceSessionResponse additive field + /voice/session wiring
# --------------------------------------------------------------------------
class TestVoiceSessionResponse:
    def test_avatar_defaults_to_none(self):
        resp = VoiceSessionResponse(backend="livekit")
        assert resp.avatar is None

    def test_avatar_additive_roundtrip(self):
        resp = VoiceSessionResponse(
            backend="livekit", avatar={"provider": "tavus", "status": "joining"}
        )
        assert resp.avatar == {"provider": "tavus", "status": "joining"}
        assert resp.model_dump()["avatar"]["provider"] == "tavus"


class TestVoiceSessionEndpoint:
    """LiveKit path wiring (livekit module stubbed by conftest fixture)."""

    async def _post_session(self, settings: Settings) -> dict:
        app = create_app(settings)
        transport = httpx.ASGITransport(app=app)
        async with httpx.AsyncClient(
            transport=transport, base_url="http://test"
        ) as client:
            resp = await client.post("/voice/session", json={"site_slug": "acme"})
        assert resp.status_code == 200, resp.text
        return resp.json()

    async def test_default_no_avatar(self, livekit_stub):
        data = await self._post_session(_settings())
        assert data["backend"] == "livekit"
        assert data["avatar"] is None

    async def test_musetalk_joining(self, livekit_stub):
        data = await self._post_session(
            _settings(
                avatar_provider="musetalk",
                musetalk_room_agent=True,
                avatar_renderer="enabled",
            )
        )
        assert data["avatar"] == {"provider": "musetalk", "status": "joining"}

    async def test_misconfigured_tavus_reports_unavailable(self, livekit_stub):
        data = await self._post_session(_settings(avatar_provider="tavus"))
        assert data["avatar"]["provider"] == "tavus"
        assert data["avatar"]["status"] == "unavailable"
        assert "TAVUS_API_KEY" in data["avatar"]["detail"]
