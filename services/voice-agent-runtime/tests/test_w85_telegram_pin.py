"""SPEC-W85 tests: Telegram contact-share identity pinning.

- request_contact (additive chat.py response flag): true iff channel is
  telegram AND the session is unverified AND the turn's tool trace carries
  a verification_required / verification_unavailable refusal. The gateway
  answers with a one-time request_contact keyboard; the resulting
  self-shared phone is provider-verified and bound chat_id -> phone.
- End-to-end pin: POST /voice/chat with channel=telegram + an E.164
  channel_identity + valid X-Internal-Token marks the session verified
  (telegram is already in PROVIDER_VERIFIED_CHANNELS — the W85 gateway
  finally feeds it a usable phone; this regression-pins the runtime side).

REDIRECTION PROOF: fails on the base tree (no request_contact key).
"""

from __future__ import annotations

from types import SimpleNamespace

import httpx
import pytest

try:  # pragma: no cover - import shim (real SDK present in the worker image)
    import app.livekit_worker as _lw  # noqa: F401
except ModuleNotFoundError:  # pragma: no cover - import shim
    import test_live_tts_chain  # noqa: F401 - installs the livekit stubs

from app import chat as chat_module
from app import metrics
from app.chat import ChatService
from app.config import Settings
from app.control_plane import create_app
from app.session_state import SessionStore
from app.tenant_context import TenantContext
from app.tools import (
    ToolLayer,
    _verification_required_payload,
    _verification_unavailable_payload,
)

from conftest import FakeDapr

PHONE_NORM = "+2348035550101"


@pytest.fixture(autouse=True)
def _fresh_registry():
    metrics.reset_registry()
    yield


def _ctx() -> TenantContext:
    return TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")


def _settings(**over) -> Settings:
    base = dict(
        booking_url="http://booking:7002",
        voice_booking_internal_token="book-tok",
        phone_hash_salt="test-salt",
    )
    base.update(over)
    return Settings(**base)


class _RefusingLLM:
    """Two-round LLM: first round calls a tool, second answers text."""

    def __init__(self, tool: str = "lookup_appointment") -> None:
        self.tool = tool
        self.calls = 0

    async def chat_with_tools(self, messages, tools):
        self.calls += 1
        if self.calls == 1:
            fn = SimpleNamespace(name=self.tool, arguments='{"phone": "+234 803 555 0101"}')
            tc = SimpleNamespace(id="call-1", type="function", function=fn)
            return SimpleNamespace(content="", tool_calls=[tc])
        return SimpleNamespace(content="I could not verify you.", tool_calls=[])


class _PlainLLM:
    async def chat_with_tools(self, messages, tools):
        return SimpleNamespace(content="ok", tool_calls=[])


def _chat_service(monkeypatch, settings: Settings, llm) -> ChatService:
    async def fake_fetch(dapr, settings_, site_slug):
        return _ctx()

    monkeypatch.setattr(chat_module, "fetch_tenant_context", fake_fetch)
    return ChatService(
        settings=settings,
        dapr=FakeDapr(),  # type: ignore[arg-type]
        llm=llm,  # type: ignore[arg-type]
        sessions=SessionStore(),
    )


def _force_dispatch(monkeypatch, payload):
    async def fake_dispatch(self, name, arguments):  # noqa: ANN001 - test stub
        return payload

    monkeypatch.setattr(ToolLayer, "dispatch", fake_dispatch)


class TestRequestContactFlag:
    async def test_telegram_refusal_requests_contact(self, monkeypatch):
        _force_dispatch(monkeypatch, _verification_required_payload())
        service = _chat_service(monkeypatch, _settings(), _RefusingLLM())
        resp = await service.handle_message(
            site_slug="demo", message="check my booking",
            conversation_id=None, channel="telegram",
        )
        assert resp["request_contact"] is True

    async def test_telegram_unavailable_requests_contact(self, monkeypatch):
        # Portal OTP unconfigured: the contact share is the ONLY path.
        _force_dispatch(monkeypatch, _verification_unavailable_payload())
        service = _chat_service(monkeypatch, _settings(), _RefusingLLM())
        resp = await service.handle_message(
            site_slug="demo", message="check my booking",
            conversation_id=None, channel="telegram",
        )
        assert resp["request_contact"] is True

    async def test_telegram_clean_turn_no_request(self, monkeypatch):
        service = _chat_service(monkeypatch, _settings(), _PlainLLM())
        resp = await service.handle_message(
            site_slug="demo", message="hello",
            conversation_id=None, channel="telegram",
        )
        assert resp["request_contact"] is False

    async def test_whatsapp_refusal_does_not_request(self, monkeypatch):
        # WhatsApp pins wa_id per turn — it never needs a contact share.
        _force_dispatch(monkeypatch, _verification_required_payload())
        service = _chat_service(monkeypatch, _settings(), _RefusingLLM())
        resp = await service.handle_message(
            site_slug="demo", message="check my booking",
            conversation_id=None, channel="whatsapp",
        )
        assert resp["request_contact"] is False

    async def test_web_refusal_does_not_request(self, monkeypatch):
        _force_dispatch(monkeypatch, _verification_required_payload())
        service = _chat_service(monkeypatch, _settings(), _RefusingLLM())
        resp = await service.handle_message(
            site_slug="demo", message="check my booking",
            conversation_id=None, channel="web",
        )
        assert resp["request_contact"] is False

    async def test_verified_telegram_session_does_not_request(self, monkeypatch):
        # Even with a refusal in the trace, an already-verified session
        # must not be re-prompted (the pin satisfies the gates).
        _force_dispatch(monkeypatch, _verification_required_payload())
        service = _chat_service(monkeypatch, _settings(), _RefusingLLM())
        resp = await service.handle_message(
            site_slug="demo", message="check my booking",
            conversation_id=None, channel="telegram",
            pin_verified_identity=PHONE_NORM,
        )
        assert resp["phone_verified"] is True
        assert resp["request_contact"] is False

    async def test_request_contact_is_boolean_and_present(self, monkeypatch):
        service = _chat_service(monkeypatch, _settings(), _PlainLLM())
        resp = await service.handle_message(
            site_slug="demo", message="hello",
            conversation_id=None, channel="web",
        )
        assert "request_contact" in resp
        assert resp["request_contact"] is False


class TestTelegramPinEndToEnd:
    """Regression pin for the runtime half of K15(c) on telegram: the
    control plane pins an E.164 channel_identity ONLY with a valid
    X-Internal-Token (the W85 gateway feeds exactly this shape)."""

    PATH = "/voice/chat"

    async def _post(self, settings: Settings, payload: dict, headers: dict):
        app = create_app(settings)
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=app), base_url="http://test"
        ) as client:
            return await client.post(self.PATH, json=payload, headers=headers)

    async def test_telegram_identity_pinned_with_valid_token(self, monkeypatch):
        captured: dict = {}

        async def fake_handle(self, **kwargs):  # noqa: ANN001 - test stub
            captured.update(kwargs)
            return {"conversation_id": "c1", "session_secret": "s", "reply": "ok"}

        monkeypatch.setattr(
            chat_module.ChatService, "handle_message", fake_handle
        )
        settings = _settings(voice_admin_internal_token="admin-tok")
        resp = await self._post(
            settings,
            {"site_slug": "acme", "message": "hi", "channel": "telegram",
             "channel_identity": "+2348012345678"},
            {"X-Internal-Token": "admin-tok"},
        )
        assert resp.status_code == 200, resp.text
        assert captured["pin_verified_identity"] == "+2348012345678"

    async def test_telegram_identity_ignored_without_token(self, monkeypatch):
        captured: dict = {}

        async def fake_handle(self, **kwargs):  # noqa: ANN001 - test stub
            captured.update(kwargs)
            return {"conversation_id": "c1", "session_secret": "s", "reply": "ok"}

        monkeypatch.setattr(
            chat_module.ChatService, "handle_message", fake_handle
        )
        settings = _settings(voice_admin_internal_token="admin-tok")
        resp = await self._post(
            settings,
            {"site_slug": "acme", "message": "hi", "channel": "telegram",
             "channel_identity": "+2348012345678"},
            {},
        )
        assert resp.status_code == 200, resp.text
        assert captured["pin_verified_identity"] is None

    async def test_digits_only_identity_pins_canonical_form(self, monkeypatch):
        # SPEC-W88 (supersedes the W85 contract documentation — the
        # follow-up audit registered below ran and landed): a digits-only
        # channel_identity (Meta wa_id form) now pins the CANONICAL
        # session identity form "+"-prefixed E.164 via
        # verification.canonical_e164, so WhatsApp/telegram/SIP verified
        # phones converge to ONE form and the pin/OTP compare paths are
        # form-independent. Non-conforming identities pin NOTHING
        # (covered in tests/test_w88_canonical.py).
        captured: dict = {}

        async def fake_handle(self, **kwargs):  # noqa: ANN001 - test stub
            captured.update(kwargs)
            return {"conversation_id": "c1", "session_secret": "s", "reply": "ok"}

        monkeypatch.setattr(
            chat_module.ChatService, "handle_message", fake_handle
        )
        settings = _settings(voice_admin_internal_token="admin-tok")
        resp = await self._post(
            settings,
            {"site_slug": "acme", "message": "hi", "channel": "telegram",
             "channel_identity": "42424242"},
            {"X-Internal-Token": "admin-tok"},
        )
        assert resp.status_code == 200, resp.text
        assert captured["pin_verified_identity"] == "+42424242"
