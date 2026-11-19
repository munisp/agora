"""SPEC-W81 WS-A tests: risk-tiered step-up 2FA (voice path).

- A1: request_verification_code / verify_caller_code are exposed on the
  LiveKit voice path (ReceptionistFunctions ai_callable wrappers delegating
  to the existing ToolLayer methods), both are SLOW_TOOLS, and a per-tool
  timeout override spares them from the 4s global tool timeout (the global
  default is untouched). The registration test is the G4 redirection proof:
  it FAILS on the base tree (wrappers absent) and passes on the candidate.
- A2: VOICE_TOOL_TIERS loader validation (fail-closed startup) and the
  dispatch choke-point enforcement per tier; the map formalizes the
  existing ladder and never weakens it (G5: built-in refusal payloads are
  byte-compatible with the pre-W81 shapes).
- A3: voice_stepup_required_total / voice_stepup_verified_total /
  voice_stepup_refused_total counters.
"""

from __future__ import annotations

import inspect
import json

import httpx
import pytest

from app import chat as chat_module
from app import metrics
from app.async_tools import SLOW_TOOLS, VERIFICATION_TOOLS, AsyncToolRunner
from app.chat import ChatService
from app.config import (
    DEFAULT_VOICE_TOOL_TIERS,
    Settings,
    load_settings,
    parse_voice_tool_tiers,
)
from app.session_state import SessionState, SessionStore
from app.tenant_context import TenantContext
from app.tools import (
    ToolLayer,
    _confirmation_payload,
    _verification_required_payload,
    _verification_unavailable_payload,
)
from app.verification import BookingPortalVerifier

from conftest import FakeDapr

try:  # pragma: no cover - import shim (real SDK present in the worker image)
    import app.livekit_worker as lw
except ModuleNotFoundError:  # pragma: no cover - import shim
    import test_live_tts_chain  # noqa: F401 - installs the livekit stubs

    import app.livekit_worker as lw

PHONE = "+234 803 555 0101"
PHONE_NORM = "+2348035550101"

OTP_TOOLS = ("request_verification_code", "verify_caller_code")


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


class _FakePlugin:
    def __init__(self, name: str) -> None:
        self.name = name
        self.executed_with: list[dict] = []

    def schema(self) -> dict:
        return {"type": "function", "function": {"name": self.name, "parameters": {}}}

    async def execute(self, arguments: dict) -> dict:
        self.executed_with.append(dict(arguments))
        return {"status": "ok", "tool": self.name}


def _layer(
    session: SessionState | None = None,
    settings: Settings | None = None,
    plugins: list | None = None,
    verifier_factory=None,
) -> ToolLayer:
    return ToolLayer(
        dapr=FakeDapr(),  # type: ignore[arg-type]
        settings=settings or _settings(),
        ctx=_ctx(),
        session=session or SessionState(conversation_id="conv-1", site_slug="demo"),
        plugin_tools=plugins or [],
        verifier_factory=verifier_factory,
    )


def _portal_verifier_factory(requests_log: list, verify_status: int = 200):
    def handler(request: httpx.Request) -> httpx.Response:
        requests_log.append(request)
        if request.url.path.endswith("/portal/request"):
            return httpx.Response(202, json={"status": "code_sent"})
        if request.url.path.endswith("/portal/verify"):
            if verify_status == 200:
                return httpx.Response(200, json={"portal_token": "jwt", "contact_id": "c-1"})
            return httpx.Response(verify_status, json={"error": "nope"})
        return httpx.Response(404)

    def factory() -> BookingPortalVerifier:
        return BookingPortalVerifier(
            base_url="http://booking:7002",
            internal_token="book-tok",
            site_slug="demo",
            client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
        )

    return factory


# ---------------------------------------------------------------------------
# A1: OTP tools on the LiveKit voice path (G4 redirection proof)
# ---------------------------------------------------------------------------
class TestVoicePathOtpTools:
    def test_otp_tools_are_slow_tools(self):
        assert VERIFICATION_TOOLS == frozenset(OTP_TOOLS)
        for name in OTP_TOOLS:
            assert name in SLOW_TOOLS

    def test_worker_wires_per_tool_timeout_override(self):
        """build_voice_agent passes timeout_overrides for the OTP tools
        sourced from settings.verification_tool_timeout_s."""
        src = inspect.getsource(lw.build_voice_agent)
        assert "timeout_overrides=" in src
        assert "verification_tool_timeout_s" in src
        assert "VERIFICATION_TOOLS" in src
        # Safe value: the override must exceed the portal client timeout
        # (http_timeout_s, 15s default) so the portal answer wins over the
        # runner's apology; the global 4s default is untouched.
        s = Settings()
        assert s.tool_timeout_s == 4.0
        assert s.verification_tool_timeout_s > s.http_timeout_s


class TestToolTimeoutOverrides:
    def test_timeout_for_defaults_to_global(self):
        runner = AsyncToolRunner(
            timeout_s=4.0, timeout_overrides={"verify_caller_code": 20.0}
        )
        assert runner.timeout_for("get_availability") == 4.0
        assert runner.timeout_for("verify_caller_code") == 20.0
        # No overrides: byte-identical pre-W81 behavior.
        assert AsyncToolRunner(timeout_s=3.0).timeout_for("verify_caller_code") == 3.0

    async def test_override_spares_otp_tool_from_global_timeout(self):
        runner = AsyncToolRunner(
            timeout_s=0.05,
            ack_grace_ms=10,
            timeout_overrides={name: 1.0 for name in OTP_TOOLS},
        )

        import asyncio

        async def slow():
            await asyncio.sleep(0.2)
            return {"status": "code_sent"}

        # Overridden tool survives past the (tiny) global timeout...
        assert await runner.run("verify_caller_code", slow) == {"status": "code_sent"}
        # ...while a non-overridden tool still hits the global timeout.
        result = await runner.run("get_availability", slow)
        assert result["status"] == "timeout"


# ---------------------------------------------------------------------------
# A2: VOICE_TOOL_TIERS loader (fail-closed startup)
# ---------------------------------------------------------------------------
class TestTierMapLoader:
    def test_default_map_is_the_locked_ladder(self):
        tiers = Settings().voice_tool_tiers
        assert tiers == DEFAULT_VOICE_TOOL_TIERS
        # Tier 0 (LOW): no identity.
        for name in (
            "get_business_info",
            "get_availability",
            "request_human",
            "navigate_to_page",
            "highlight_element",
            "prefill_booking",
            "request_verification_code",
            "verify_caller_code",
        ):
            assert tiers[name] == 0, name
        # Tier 1 (MEDIUM): confirmed identity.
        assert tiers["book_appointment"] == 1
        assert tiers["capture_location"] == 1
        # Tier 2 (HIGH): verified.
        for name in ("lookup_appointment", "reschedule_appointment", "cancel_appointment"):
            assert tiers[name] == 2, name

    def test_env_default_matches_builtin_map(self, monkeypatch):
        monkeypatch.delenv("VOICE_TOOL_TIERS", raising=False)
        assert load_settings().voice_tool_tiers == DEFAULT_VOICE_TOOL_TIERS

    def test_env_override_plugin_and_raise(self, monkeypatch):
        monkeypatch.setenv(
            "VOICE_TOOL_TIERS",
            json.dumps({"loyalty_signup": 2, "book_appointment": 2, "mcp__crm__vip": 0}),
        )
        tiers = load_settings().voice_tool_tiers
        assert tiers["loyalty_signup"] == 2
        assert tiers["book_appointment"] == 2  # raising a built-in is allowed
        assert tiers["mcp__crm__vip"] == 0
        # Untouched built-ins keep their floors.
        assert tiers["lookup_appointment"] == 2
        assert tiers["get_availability"] == 0

    @pytest.mark.parametrize(
        "raw",
        [
            "{not json",
            "[1, 2]",
            '"book_appointment"',
            '{"lookup_appointment": 1}',  # lowering a built-in floor
            '{"lookup_appointment": 0}',
            '{"capture_location": 0}',  # emergency lane floor is 1
            '{"book_appointment": 5}',
            '{"book_appointment": "high"}',
            '{"book_appointment": true}',
            '{"bad name!": 1}',
            '{"": 1}',
        ],
    )
    def test_invalid_env_fails_startup_closed(self, monkeypatch, raw):
        monkeypatch.setenv("VOICE_TOOL_TIERS", raw)
        with pytest.raises(ValueError):
            load_settings()

    def test_parse_unit_roundtrip(self):
        tiers = parse_voice_tool_tiers('{"loyalty_signup": 0}')
        assert tiers["loyalty_signup"] == 0
        with pytest.raises(ValueError):
            parse_voice_tool_tiers('{"cancel_appointment": 1}')


# ---------------------------------------------------------------------------
# A2: dispatch choke-point enforcement
# ---------------------------------------------------------------------------
class TestTierEnforcement:
    async def test_tier0_plugin_passes_anonymous(self):
        plugin = _FakePlugin("opening_hours_plus")
        layer = _layer(
            plugins=[plugin],
            settings=_settings(
                voice_tool_tiers={**DEFAULT_VOICE_TOOL_TIERS, "opening_hours_plus": 0}
            ),
        )
        result = await layer.dispatch("opening_hours_plus", {})
        assert result["status"] == "ok"
        assert plugin.executed_with == [{}]

    async def test_plugin_default_is_tier1(self):
        """A plugin tool absent from the map requires a confirmed identity —
        never silently tier 0."""
        plugin = _FakePlugin("loyalty_signup")
        layer = _layer(plugins=[plugin])
        result = await layer.dispatch("loyalty_signup", {"phone": PHONE})
        assert result == {
            "status": "confirmation_required",
            "error": "confirmed_phone_required",
            "tool": "loyalty_signup",
            "tier": 1,
            "message": result["message"],
        }
        assert plugin.executed_with == []  # never executed

    async def test_tier1_plugin_passes_with_confirmed_phone(self):
        plugin = _FakePlugin("loyalty_signup")
        session = SessionState(conversation_id="conv-1", site_slug="demo")
        session.confirmed_phone = PHONE_NORM
        layer = _layer(session=session, plugins=[plugin])
        result = await layer.dispatch("loyalty_signup", {})
        assert result["status"] == "ok"

    async def test_tier1_plugin_passes_with_verified_phone(self):
        plugin = _FakePlugin("loyalty_signup")
        session = SessionState(conversation_id="conv-1", site_slug="demo")
        session.mark_verified(PHONE_NORM)
        layer = _layer(session=session, plugins=[plugin])
        assert (await layer.dispatch("loyalty_signup", {}))["status"] == "ok"

    async def test_tier2_plugin_refused_anonymous_byte_compatible(self):
        """G5: a tier-2 refusal is the EXISTING _require_verified payload,
        byte-identical."""
        plugin = _FakePlugin("loyalty_points")
        layer = _layer(
            plugins=[plugin],
            settings=_settings(
                voice_tool_tiers={**DEFAULT_VOICE_TOOL_TIERS, "loyalty_points": 2}
            ),
        )
        result = await layer.dispatch("loyalty_points", {"phone": PHONE})
        assert result == _verification_required_payload()
        assert plugin.executed_with == []

    async def test_tier2_plugin_fail_closed_when_unconfigured(self):
        plugin = _FakePlugin("loyalty_points")
        layer = _layer(
            plugins=[plugin],
            settings=Settings(
                voice_tool_tiers={**DEFAULT_VOICE_TOOL_TIERS, "loyalty_points": 2}
            ),
        )
        result = await layer.dispatch("loyalty_points", {"phone": PHONE})
        assert result == _verification_unavailable_payload()

    async def test_tier2_plugin_passes_when_verified(self):
        plugin = _FakePlugin("loyalty_points")
        session = SessionState(conversation_id="conv-1", site_slug="demo")
        session.mark_verified(PHONE_NORM)
        layer = _layer(
            session=session,
            plugins=[plugin],
            settings=_settings(
                voice_tool_tiers={**DEFAULT_VOICE_TOOL_TIERS, "loyalty_points": 2}
            ),
        )
        assert (await layer.dispatch("loyalty_points", {}))["status"] == "ok"

    async def test_builtin_tier2_refusal_payload_unchanged_via_dispatch(self):
        """G5: built-ins at their default tier keep the byte-compatible
        _require_verified refusal (the choke point does not double-gate)."""
        layer = _layer()
        result = await layer.dispatch("lookup_appointment", {"phone": PHONE})
        assert result == _verification_required_payload()
        result = await layer.dispatch("cancel_appointment", {"booking_id": "b", "phone": PHONE})
        assert result == _verification_required_payload()

    async def test_builtin_tier1_confirmation_flow_unchanged(self):
        layer = _layer()
        result = await layer.dispatch(
            "book_appointment",
            {
                "offering_id": "o",
                "team_member_id": "t",
                "starts_at": "2030-01-01T10:00:00+00:00",
                "phone": PHONE,
            },
        )
        assert result == _confirmation_payload(PHONE)

    async def test_raised_builtin_tier_is_hard_gated(self):
        """VOICE_TOOL_TIERS may RAISE a built-in (book_appointment -> 2);
        dispatch then enforces the raised tier at the choke point."""
        layer = _layer(
            settings=_settings(
                voice_tool_tiers={**DEFAULT_VOICE_TOOL_TIERS, "book_appointment": 2}
            )
        )
        result = await layer.dispatch(
            "book_appointment",
            {
                "offering_id": "o",
                "team_member_id": "t",
                "starts_at": "2030-01-01T10:00:00+00:00",
                "phone": PHONE,
            },
        )
        assert result == _verification_required_payload()

    async def test_unknown_tool_error_unchanged(self):
        layer = _layer()
        result = await layer.dispatch("definitely_not_a_tool", {})
        assert result["status"] == "error"
        assert "unknown tool" in result["message"]

    def test_tool_tier_fallbacks(self):
        layer = _layer()
        assert layer._tool_tier("get_availability") == 0
        assert layer._tool_tier("cancel_appointment") == 2
        # Unknown (plugin) names default to tier 1, never silently tier 0.
        assert layer._tool_tier("some_pack_tool") == 1


# ---------------------------------------------------------------------------
# A3: step-up metrics
# ---------------------------------------------------------------------------
class TestStepupMetrics:
    async def test_required_and_refused_counters(self):
        plugin = _FakePlugin("loyalty_points")
        layer = _layer(
            plugins=[plugin],
            settings=_settings(
                voice_tool_tiers={**DEFAULT_VOICE_TOOL_TIERS, "loyalty_points": 2}
            ),
        )
        await layer.dispatch("loyalty_points", {"phone": PHONE})
        text = metrics.render()
        assert 'voice_stepup_required_total{tier="2",tool="loyalty_points"} 1' in text
        assert 'voice_stepup_refused_total{reason="verification_required"} 1' in text

    async def test_builtin_refusal_counted_on_every_path(self):
        layer = _layer()
        # Direct method call (LiveKit voice path) — not via dispatch.
        await layer.lookup_appointment(phone=PHONE)
        text = metrics.render()
        assert 'voice_stepup_refused_total{reason="verification_required"} 1' in text

    async def test_book_confirmation_refusal_counted(self):
        layer = _layer()
        await layer.dispatch(
            "book_appointment",
            {
                "offering_id": "o",
                "team_member_id": "t",
                "starts_at": "2030-01-01T10:00:00+00:00",
                "phone": PHONE,
            },
        )
        text = metrics.render()
        assert 'voice_stepup_refused_total{reason="confirmation_required"} 1' in text
        assert 'voice_stepup_required_total{tier="1",tool="book_appointment"} 1' in text

    async def test_otp_success_counted(self):
        requests_log: list = []
        layer = _layer(verifier_factory=_portal_verifier_factory(requests_log))
        sent = await layer.dispatch("request_verification_code", {"phone": PHONE})
        assert sent["status"] == "code_sent"
        verified = await layer.dispatch(
            "verify_caller_code", {"phone": PHONE, "code": "123456"}
        )
        assert verified["status"] == "verified"
        assert 'voice_stepup_verified_total{method="otp"} 1' in metrics.render()

    async def test_channel_pin_counted(self, monkeypatch):
        async def fake_fetch(dapr, settings_, site_slug):
            return _ctx()

        class _FakeLLM:
            async def chat_with_tools(self, messages, tools):
                from types import SimpleNamespace

                return SimpleNamespace(content="ok", tool_calls=[])

        monkeypatch.setattr(chat_module, "fetch_tenant_context", fake_fetch)
        service = ChatService(
            settings=_settings(),
            dapr=FakeDapr(),  # type: ignore[arg-type]
            llm=_FakeLLM(),  # type: ignore[arg-type]
            sessions=SessionStore(),
        )
        resp = await service.handle_message(
            site_slug="demo",
            message="hi",
            conversation_id=None,
            channel="whatsapp",
            pin_verified_identity=PHONE_NORM,
        )
        assert resp["phone_verified"] is True
        assert 'voice_stepup_verified_total{method="channel"} 1' in metrics.render()


# ---------------------------------------------------------------------------
# G5: refusal payload shapes pinned byte-for-byte
# ---------------------------------------------------------------------------
class TestRefusalPayloadCompatibility:
    def test_verification_required_payload_shape(self):
        assert _verification_required_payload() == {
            "status": "verification_required",
            "message": (
                "For the caller's security, this phone number must be verified "
                "before bookings can be looked up or changed. Call "
                "request_verification_code with the caller's number to send a "
                "one-time code, ask the caller to read it back, then submit it "
                "with verify_caller_code."
            ),
        }

    def test_verification_unavailable_payload_shape(self):
        assert _verification_unavailable_payload() == {
            "status": "error",
            "error": "verification_unavailable",
            "message": (
                "Phone verification is temporarily unavailable, so I can't look "
                "up or change existing bookings right now. Please try again "
                "later or contact the business directly."
            ),
        }

    async def test_require_verified_semantics_unchanged(self):
        """_require_verified: verified phone wins; mismatch rejected; the
        fail-closed unavailable variant when unconfigured.

        SPEC-W87 note: _require_verified became async (grant honoring);
        this test is the await-only adaptation — every assertion is
        byte-identical to W81. The grant path is inert here (default
        channel "web" never honors grants)."""
        session = SessionState(conversation_id="conv-1", site_slug="demo")
        layer = _layer(session=session)
        # Unverified + configured -> verification_required payload.
        assert await layer._require_verified(PHONE) == _verification_required_payload()
        # Unverified + unconfigured -> fail-closed unavailable payload.
        bare = _layer(session=session, settings=Settings())
        assert await bare._require_verified(PHONE) == _verification_unavailable_payload()
        # Verified: the verified number always wins over a supplied one.
        session.mark_verified(PHONE_NORM)
        assert await layer._require_verified(PHONE_NORM) == PHONE_NORM
        mismatch = await layer._require_verified("+2349000000000")
        assert mismatch["status"] == "error"
        assert mismatch["error"] == "phone_mismatch"
