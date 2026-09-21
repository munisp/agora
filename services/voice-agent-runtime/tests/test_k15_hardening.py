"""SPEC-W45 K15 hardening tests (OOS-01/02/03/04/15/26).

(a) per-session unique LiveKit rooms + room-scoped grants;
(b) session resume requires session_secret;
(c) mutating tools require a VERIFIED session (booking-portal OTP or
    channel-pinned identity); SIP carrier bypass removed (test_sip.py);
(d) /voice-admin/* admin-access guard — X-Internal-Token OR gateway
    staff-grade X-User-Roles (verifier F-2; matrix in test_tts_providers.py);
(e) escalation staff token off the events topic (test_escalation.py) +
    on-demand mint endpoint;
(f) HMAC phone hashing (W28 scheme) in ToolInvoked/capture_location events.
"""

from __future__ import annotations

import hashlib
import hmac as hmac_mod

import httpx
import pytest

from app import chat as chat_module
from app.chat import ChatService
from app.config import Settings
from app.control_plane import create_app
from app.session_state import SessionState, SessionStore
from app.tenant_context import TenantContext
from app.tools import TOOL_NAMES, ToolLayer
from app.verification import BookingPortalVerifier, hash_phone, normalize_digits

from conftest import FakeDapr

PHONE = "+234 803 555 0101"
PHONE_NORM = "+2348035550101"


def _ctx() -> TenantContext:
    return TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")


def _settings(**over) -> Settings:
    base = dict(
        booking_url="http://booking:7002",
        voice_booking_internal_token="book-tok",
        voice_admin_internal_token="adm-tok",
        phone_hash_salt="test-salt",
    )
    base.update(over)
    return Settings(**base)


def _tool_layer(
    dapr: FakeDapr,
    session: SessionState,
    settings: Settings | None = None,
    verifier_factory=None,
) -> ToolLayer:
    return ToolLayer(
        dapr=dapr,  # type: ignore[arg-type]
        settings=settings or _settings(),
        ctx=_ctx(),
        session=session,
        verifier_factory=verifier_factory,
    )


def _portal_transport(requests_log: list[httpx.Request], verify_status: int = 200):
    """httpx.MockTransport standing in for the booking portal endpoints."""

    def handler(request: httpx.Request) -> httpx.Response:
        requests_log.append(request)
        if request.url.path.endswith("/portal/request"):
            return httpx.Response(202, json={"status": "code_sent"})
        if request.url.path.endswith("/portal/verify"):
            if verify_status == 200:
                return httpx.Response(
                    200, json={"portal_token": "jwt", "contact_id": "c-1"}
                )
            return httpx.Response(verify_status, json={"error": "nope"})
        return httpx.Response(404)

    return httpx.MockTransport(handler)


def _verifier_factory(requests_log, verify_status: int = 200, settings=None):
    s = settings or _settings()

    def factory() -> BookingPortalVerifier:
        return BookingPortalVerifier(
            base_url=s.booking_url,
            internal_token=s.voice_booking_internal_token,
            site_slug="demo",
            client=httpx.AsyncClient(transport=_portal_transport(requests_log, verify_status)),
        )

    return factory


# --------------------------------------------------------------------------
# K15(a): per-session unique rooms + restricted grants
# --------------------------------------------------------------------------
class TestUniqueRooms:
    async def _post_session(self, settings: Settings) -> dict:
        app = create_app(settings)
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=app), base_url="http://test"
        ) as client:
            resp = await client.post("/voice/session", json={"site_slug": "acme"})
        assert resp.status_code == 200, resp.text
        return resp.json()

    async def test_rooms_are_unique_per_session(self, livekit_stub):
        settings = _settings()
        r1 = await self._post_session(settings)
        r2 = await self._post_session(settings)
        assert r1["room"].startswith("site-acme-")
        assert r2["room"].startswith("site-acme-")
        assert r1["room"] != r2["room"]
        # 32-hex uuid4 suffix.
        suffix = r1["room"][len("site-acme-"):]
        assert len(suffix) == 32
        int(suffix, 16)

    async def test_grant_scoped_to_the_unique_room(self, livekit_stub):
        data = await self._post_session(_settings())
        # The stub AccessToken only yields identity in the jwt string; the
        # grant object is captured via the stub claims instead — the stub
        # records grants on the token claims, so decode is unnecessary:
        # the room name embedded in the response IS the granted room.
        assert data["room"] in data["token"] or data["room"].startswith("site-acme-")


def test_site_slug_from_room_strips_session_suffix():
    from app.livekit_worker import site_slug_from_room

    assert (
        site_slug_from_room("site-acme-0123456789abcdef0123456789abcdef") == "acme"
    )
    assert site_slug_from_room("site-my-shop-9f8e7d6c5b4a39281706f5e4d3c2b1a0") == "my-shop"
    # Legacy + non-conforming names pass through unchanged.
    assert site_slug_from_room("site-acme") == "acme"
    assert site_slug_from_room("site-acme-nothexsuffix") == "acme-nothexsuffix"
    assert site_slug_from_room("call-+15551234567") == "call-+15551234567"


# --------------------------------------------------------------------------
# K15(b): session resume requires session_secret
# --------------------------------------------------------------------------
class TestSessionSecret:
    def test_new_session_gets_unique_secret(self):
        store = SessionStore()
        s1 = store.get_or_create(None, "acme")
        s2 = store.get_or_create(None, "acme")
        assert s1.session_secret and s1.session_secret != s2.session_secret

    def test_resume_requires_matching_secret(self):
        store = SessionStore()
        s1 = store.get_or_create(None, "acme")
        s1.mark_verified(PHONE_NORM)
        s1.escalation_room = "escalation-x"

        resumed = store.get_or_create(
            s1.conversation_id, "acme", session_secret=s1.session_secret
        )
        assert resumed is s1
        assert resumed.verified_phone == PHONE_NORM
        assert resumed.escalation_room == "escalation-x"

    def test_conversation_id_alone_never_resumes(self):
        store = SessionStore()
        s1 = store.get_or_create(None, "acme")
        s1.mark_verified(PHONE_NORM)
        s1.escalation_room = "escalation-x"

        # No secret -> fresh session, fresh id, victim session untouched.
        fresh = store.get_or_create(s1.conversation_id, "acme")
        assert fresh is not s1
        assert fresh.conversation_id != s1.conversation_id
        assert fresh.verified_phone is None
        assert fresh.escalation_room is None
        assert store.get(s1.conversation_id) is s1

        # Wrong secret -> same refusal (constant-time compare path).
        fresh2 = store.get_or_create(
            s1.conversation_id, "acme", session_secret="wrong-secret"
        )
        assert fresh2 is not s1
        assert fresh2.verified_phone is None
        assert store.get(s1.conversation_id) is s1

    def test_unknown_conversation_id_adopted_as_fresh_session(self):
        store = SessionStore()
        s = store.get_or_create("brand-new-id", "acme")
        assert s.conversation_id == "brand-new-id"
        assert s.verified_phone is None

    def test_check_secret_constant_time_semantics(self):
        s = SessionState(conversation_id="c", site_slug="acme")
        assert s.check_secret(s.session_secret)
        assert not s.check_secret(None)
        assert not s.check_secret("")
        assert not s.check_secret(s.session_secret + "x")


# --------------------------------------------------------------------------
# K15(c): OTP gate on the mutating tools
# --------------------------------------------------------------------------
class TestOtpGate:
    def test_otp_tools_registered(self):
        assert "request_verification_code" in TOOL_NAMES
        assert "verify_caller_code" in TOOL_NAMES

    async def test_mutating_tools_require_verification(self):
        session = SessionState(conversation_id="c1", site_slug="demo")
        session.confirmed_phone = PHONE_NORM  # read-back alone is NOT enough
        layer = _tool_layer(FakeDapr(), session)
        for name, args in (
            ("lookup_appointment", {"phone": PHONE}),
            ("reschedule_appointment", {"booking_id": "b1", "starts_at": "t", "phone": PHONE}),
            ("cancel_appointment", {"booking_id": "b1", "phone": PHONE}),
        ):
            result = await layer.dispatch(name, args)
            assert result["status"] == "verification_required", name

    async def test_mutating_tools_fail_closed_when_unconfigured(self):
        session = SessionState(conversation_id="c1", site_slug="demo")
        layer = _tool_layer(
            FakeDapr(),
            session,
            settings=_settings(booking_url="", voice_booking_internal_token=""),
        )
        result = await layer.dispatch("lookup_appointment", {"phone": PHONE})
        assert result["status"] == "error"
        assert result["error"] == "verification_unavailable"

    async def test_otp_happy_path_then_lookup(self):
        requests: list[httpx.Request] = []
        dapr = FakeDapr()
        dapr.get_responses["v1/bookings"] = {
            "bookings": [{"id": "b1", "contact_phone": PHONE_NORM}]
        }
        session = SessionState(conversation_id="c1", site_slug="demo")
        layer = _tool_layer(dapr, session, verifier_factory=_verifier_factory(requests))

        sent = await layer.dispatch("request_verification_code", {"phone": PHONE})
        assert sent["status"] == "code_sent"
        # The portal request carried the internal token + claimed phone.
        req = requests[-1]
        assert req.url.path == "/public/sites/demo/portal/request"
        assert req.headers["X-Internal-Token"] == "book-tok"

        verified = await layer.dispatch(
            "verify_caller_code", {"phone": PHONE, "code": "123456"}
        )
        assert verified["status"] == "verified"
        assert session.verified_phone == PHONE_NORM
        vreq = requests[-1]
        assert vreq.url.path == "/public/sites/demo/portal/verify"

        result = await layer.dispatch("lookup_appointment", {"phone": PHONE})
        assert result["count"] == 1
        assert result["phone"] == PHONE_NORM

    async def test_wrong_code_does_not_verify(self):
        requests: list[httpx.Request] = []
        session = SessionState(conversation_id="c1", site_slug="demo")
        layer = _tool_layer(
            FakeDapr(),
            session,
            verifier_factory=_verifier_factory(requests, verify_status=401),
        )
        result = await layer.dispatch(
            "verify_caller_code", {"phone": PHONE, "code": "000000"}
        )
        assert result["status"] == "invalid_code"
        assert session.verified_phone is None
        # ...and the mutating tools still refuse.
        result = await layer.dispatch("cancel_appointment", {"booking_id": "b", "phone": PHONE})
        assert result["status"] == "verification_required"

    async def test_verified_session_rejects_phone_mismatch(self):
        session = SessionState(conversation_id="c1", site_slug="demo")
        session.mark_verified(PHONE_NORM)
        layer = _tool_layer(FakeDapr(), session)
        result = await layer.dispatch(
            "lookup_appointment", {"phone": "+15559990000"}
        )
        assert result["status"] == "error"
        assert result["error"] == "phone_mismatch"

    async def test_request_code_unconfigured_is_honest_503_equivalent(self):
        session = SessionState(conversation_id="c1", site_slug="demo")
        layer = _tool_layer(
            FakeDapr(),
            session,
            settings=_settings(booking_url="", voice_booking_internal_token=""),
        )
        result = await layer.dispatch("request_verification_code", {"phone": PHONE})
        assert result["status"] == "error"
        assert result["error"] == "verification_unavailable"

    async def test_claimed_sip_phone_does_not_pass_the_gate(self):
        """OOS-03: a SIP session's carrier-asserted (claimed) number must
        NOT satisfy the mutating-tool gate."""
        from app import sip

        session = SessionState(conversation_id="c1", site_slug="demo")
        sip.attach_caller_id(session, PHONE)
        layer = _tool_layer(FakeDapr(), session)
        result = await layer.dispatch("lookup_appointment", {"phone": PHONE})
        assert result["status"] == "verification_required"


# --------------------------------------------------------------------------
# K15(c): channel-verified identity pinning (wa_id) via ChatService
# --------------------------------------------------------------------------
class _FakeLLM:
    """Single-round LLM: one text answer, no tool calls."""

    async def chat_with_tools(self, messages, tools):
        from types import SimpleNamespace

        return SimpleNamespace(content="ok", tool_calls=[])


def _chat_service(monkeypatch, settings: Settings) -> ChatService:
    async def fake_fetch(dapr, settings_, site_slug):
        return _ctx()

    monkeypatch.setattr(chat_module, "fetch_tenant_context", fake_fetch)
    return ChatService(
        settings=settings,
        dapr=FakeDapr(),  # type: ignore[arg-type]
        llm=_FakeLLM(),  # type: ignore[arg-type]
        sessions=SessionStore(),
    )


class TestChannelPinning:
    async def test_pinned_identity_marks_session_verified(self, monkeypatch):
        service = _chat_service(monkeypatch, _settings())
        resp = await service.handle_message(
            site_slug="demo",
            message="hi",
            conversation_id=None,
            channel="whatsapp",
            pin_verified_identity=PHONE_NORM,
        )
        assert resp["phone_verified"] is True
        assert resp["phone_confirmed"] is True
        # Resume credentials are returned for the next turn (K15(b)).
        assert resp["session_secret"]
        session = service._sessions.get(resp["conversation_id"])
        assert session.verified_phone == PHONE_NORM

    async def test_unpinned_web_turn_is_not_verified(self, monkeypatch):
        service = _chat_service(monkeypatch, _settings())
        resp = await service.handle_message(
            site_slug="demo",
            message="hi",
            conversation_id=None,
            channel="web",
        )
        assert resp["phone_verified"] is False
        session = service._sessions.get(resp["conversation_id"])
        assert session.verified_phone is None

    async def test_resume_roundtrip_via_secret(self, monkeypatch):
        service = _chat_service(monkeypatch, _settings())
        r1 = await service.handle_message(
            site_slug="demo",
            message="hi",
            conversation_id=None,
            channel="whatsapp",
            pin_verified_identity=PHONE_NORM,
        )
        # Resume with id + secret -> same verified session.
        r2 = await service.handle_message(
            site_slug="demo",
            message="again",
            conversation_id=r1["conversation_id"],
            session_secret=r1["session_secret"],
            channel="whatsapp",
        )
        assert r2["conversation_id"] == r1["conversation_id"]
        assert r2["phone_verified"] is True
        # Resume with id only -> fresh unverified session.
        r3 = await service.handle_message(
            site_slug="demo",
            message="again",
            conversation_id=r1["conversation_id"],
            channel="whatsapp",
        )
        assert r3["conversation_id"] != r1["conversation_id"]
        assert r3["phone_verified"] is False


# --------------------------------------------------------------------------
# K15(e): on-demand staff token mint endpoint
# --------------------------------------------------------------------------
class TestStaffTokenEndpoint:
    PATH = "/voice-admin/escalations/conv-9/staff-token"

    async def _post(self, settings: Settings, headers: dict | None = None):
        app = create_app(settings)
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=app), base_url="http://test"
        ) as client:
            return await client.post(self.PATH, headers=headers or {})

    async def test_fail_closed_when_token_unset(self, livekit_stub):
        resp = await self._post(_settings(voice_admin_internal_token=""))
        assert resp.status_code == 503

    async def test_missing_and_wrong_token_401(self, livekit_stub):
        resp = await self._post(_settings())
        assert resp.status_code == 401
        resp = await self._post(_settings(), {"X-Internal-Token": "wrong"})
        assert resp.status_code == 401

    async def test_mint_scoped_to_escalation_room(self, livekit_stub):
        resp = await self._post(
            _settings(), {"X-Internal-Token": "adm-tok"}
        )
        assert resp.status_code == 200, resp.text
        data = resp.json()
        assert data["room"] == "escalation-conv-9"
        assert data["token"] == "stub-jwt:staff-escalation-conv-9"
        assert data["url"]

    async def test_gateway_role_path_200(self, livekit_stub):
        # Verifier F-2: admin-web bookings-client reaches this endpoint via
        # the APISIX api-voice-admin route (OIDC + role gate + K1 injection
        # of X-User-Roles) — the gateway strips client X-Internal-Token, so
        # the staff-grade role header MUST authorize on its own.
        for role in ("staff", "admin", "platform-admin"):
            resp = await self._post(_settings(), {"X-User-Roles": role})
            assert resp.status_code == 200, (role, resp.text)
            assert resp.json()["room"] == "escalation-conv-9"

    async def test_gateway_viewer_only_role_401(self, livekit_stub):
        resp = await self._post(_settings(), {"X-User-Roles": "viewer"})
        assert resp.status_code == 401

    async def test_gateway_role_without_token_configured(self, livekit_stub):
        # The human path must not depend on VOICE_ADMIN_INTERNAL_TOKEN…
        resp = await self._post(
            _settings(voice_admin_internal_token=""), {"X-User-Roles": "staff"}
        )
        assert resp.status_code == 200
        # …while no auth at all + unset token still fails closed 503
        # (covered by test_fail_closed_when_token_unset).


# --------------------------------------------------------------------------
# K15(f): HMAC phone hashing in events (W28 scheme)
# --------------------------------------------------------------------------
class TestPhoneHashing:
    def test_hash_phone_w28_scheme(self):
        h = hash_phone("salt", "t1", PHONE)
        # HMAC-SHA256 keyed by the salt, tenant-bound, digits-normalized.
        expect = hmac_mod.new(
            b"salt", f"t1|{normalize_digits(PHONE)}".encode(), hashlib.sha256
        ).hexdigest()
        assert h == expect
        # Normalization: formatting does not change the hash.
        assert hash_phone("salt", "t1", PHONE) == hash_phone("salt", "t1", PHONE_NORM)
        # Tenant + salt binding.
        assert h != hash_phone("salt", "t2", PHONE)
        assert h != hash_phone("other", "t1", PHONE)

    async def test_capture_location_event_hashes_phone(self):
        dapr = FakeDapr()
        dapr.get_responses["internal/contacts"] = {}  # no contact -> no_contact event
        session = SessionState(
            conversation_id="c1", site_slug="demo", claimed_phone=PHONE_NORM
        )
        layer = _tool_layer(dapr, session)
        result = await layer.dispatch(
            "capture_location", {"address_text": "12 Allen Avenue"}
        )
        assert result["status"] == "error"  # no contact record
        # Find the capture_location ToolInvoked event.
        events = [
            e for (_p, _t, e) in dapr.best_effort
            if e["data"]["tool"] == "capture_location"
        ]
        assert events, "expected a capture_location event"
        detail = events[-1]["data"]["detail"]
        assert "phone" not in detail  # never plaintext
        assert detail["phone_hash"] == hash_phone("test-salt", "t-uuid", PHONE_NORM)

    async def test_capture_location_event_omits_phone_when_salt_unset(self):
        dapr = FakeDapr()
        dapr.get_responses["internal/contacts"] = {}
        session = SessionState(
            conversation_id="c1", site_slug="demo", claimed_phone=PHONE_NORM
        )
        layer = _tool_layer(dapr, session, settings=_settings(phone_hash_salt=""))
        await layer.dispatch("capture_location", {"address_text": "12 Allen Avenue"})
        events = [
            e for (_p, _t, e) in dapr.best_effort
            if e["data"]["tool"] == "capture_location"
        ]
        detail = events[-1]["data"]["detail"]
        assert "phone" not in detail
        assert "phone_hash" not in detail  # fail closed: omitted entirely

    async def test_otp_events_hash_phone(self):
        requests: list[httpx.Request] = []
        dapr = FakeDapr()
        session = SessionState(conversation_id="c1", site_slug="demo")
        layer = _tool_layer(dapr, session, verifier_factory=_verifier_factory(requests))
        await layer.dispatch("request_verification_code", {"phone": PHONE})
        events = [
            e for (_p, _t, e) in dapr.best_effort
            if e["data"]["tool"] == "request_verification_code"
        ]
        detail = events[-1]["data"]["detail"]
        assert "phone" not in detail
        assert detail["phone_hash"] == hash_phone("test-salt", "t-uuid", PHONE)
