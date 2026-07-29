"""SPEC-W11 Part C tests: live-turn emergency detection + priority lane.

Covers:
- the EN+PCM emergency lexicon (critical/high severities, hazard extraction,
  negatives) and the per-session EmergencyState latch;
- the live-call emergency lane (EmergencyTurnHook in app/livekit_worker.py):
  session flag, voice_emergency_sessions_total counter, warm handoff via the
  EXISTING escalation path with reason "emergency", location-first addendum;
- the capture_location tool (mocked Dapr, Wave-8 PUT contract);
- the spoken AI disclosure / recording consent greeting (on/off).
"""

from __future__ import annotations

import asyncio
from types import SimpleNamespace

import pytest

from app import metrics
from app.config import Settings
from app.emergency import EmergencyState, is_emergency, message_text
from app.prompts import (
    AI_DISCLOSURE_LINE,
    RECORDING_NOTICE_LINE,
    build_greeting,
)
from app.session_state import SessionState
from app.tenant_context import TenantContext
from app.tools import TOOL_NAMES, ToolLayer

from conftest import FakeDapr

try:  # pragma: no cover - import shim (real SDK present in the worker image)
    import app.livekit_worker as lw
except ModuleNotFoundError:  # pragma: no cover - import shim
    import test_live_tts_chain  # noqa: F401 - installs the livekit stubs

    import app.livekit_worker as lw


def _ctx() -> TenantContext:
    return TenantContext(
        site_slug="demo", tenant_id="t-uuid", tenant_slug="acme", display_name="Acme"
    )


class _ChatCtx:
    """Minimal chat-context double recording appended messages."""

    def __init__(self) -> None:
        self.appended: list[tuple[str, str]] = []

    def append(self, *, role: str, text: str):
        self.appended.append((role, text))
        return self


# ---------------------------------------------------------------------------
# Lexicon: English
# ---------------------------------------------------------------------------
@pytest.mark.parametrize(
    "text",
    [
        "someone is dying here, please",
        "there's blood everywhere",
        "they want to kidnap my son",
        "armed robbery in progress at the shop",
        "she was raped",
        "he has a gun and is threatening us",
    ],
)
def test_lexicon_critical_en(text):
    matched, severity, _hazards = is_emergency(text)
    assert matched is True
    assert severity == "critical"


@pytest.mark.parametrize(
    "text",
    [
        "this is an emergency",
        "there has been an accident on the expressway",
        "there is a fire in my building",
        "he collapsed in the market",
        "help me please",
        "my neighbour is unconscious",
    ],
)
def test_lexicon_high_en(text):
    matched, severity, _hazards = is_emergency(text)
    assert matched is True
    assert severity == "high"


# ---------------------------------------------------------------------------
# Lexicon: Nigerian Pidgin (PCM)
# ---------------------------------------------------------------------------
@pytest.mark.parametrize(
    "text,severity",
    [
        ("thief dey my house right now", "high"),
        ("thief don enter our compound", "high"),
        ("fire dey burn for my street", "high"),
        ("e don happen for Lagos road", "high"),
        ("wahala dey o, dem dey attack people", "high"),
        ("dem don kill am o", "critical"),
        ("dem carry gun, dey do armed robbery", "critical"),
    ],
)
def test_lexicon_pcm(text, severity):
    matched, sev, _hazards = is_emergency(text)
    assert matched is True
    assert sev == severity


def test_lexicon_case_and_whitespace_insensitive():
    matched, severity, _ = is_emergency("  THERE IS A FIRE   in the MARKET ")
    assert matched is True
    assert severity == "high"


# ---------------------------------------------------------------------------
# Hazard extraction
# ---------------------------------------------------------------------------
def test_hazards_fire_and_gas():
    _m, _s, hazards = is_emergency("there is a fire and a gas leak in the kitchen")
    assert hazards == ["fire", "gas"]


def test_hazards_traffic_and_injuries():
    _m, _s, hazards = is_emergency("car accident on the expressway, someone is bleeding")
    assert hazards == ["injuries", "traffic"]


def test_hazards_weapons():
    _m, _s, hazards = is_emergency("armed robbery, the man has a knife")
    assert "weapons" in hazards


def test_hazards_may_be_present_without_emergency_match():
    matched, severity, hazards = is_emergency("I smell gas, maybe a gas leak")
    assert matched is False
    assert severity is None
    assert hazards == ["gas"]


# ---------------------------------------------------------------------------
# Negatives: normal receptionist traffic must not match
# ---------------------------------------------------------------------------
@pytest.mark.parametrize(
    "text",
    [
        "I would like to book an appointment for tomorrow",
        "what time do you open on Saturdays",
        "please cancel my reservation",
        "do you offer home service",
        "how much is a haircut",
        "I want to reschedule my booking to next week",
        "thank you, goodbye",
    ],
)
def test_lexicon_negatives(text):
    matched, severity, hazards = is_emergency(text)
    assert matched is False
    assert severity is None
    assert hazards == []


def test_is_emergency_empty_and_none():
    assert is_emergency("") == (False, None, [])
    assert is_emergency(None) == (False, None, [])


# ---------------------------------------------------------------------------
# EmergencyState latch
# ---------------------------------------------------------------------------
def test_emergency_state_triggers_once():
    state = EmergencyState()
    hit = state.observe("there is a fire in my house")
    assert hit is not None
    severity, hazards = hit
    assert severity == "high"
    assert "fire" in hazards
    assert state.matched is True
    # Subsequent emergency turns do NOT re-trigger.
    assert state.observe("the fire is spreading") is None
    assert state.observe("and now someone is dying") is None
    # ...but severity still escalates and hazards union.
    assert state.severity == "critical"
    assert "injuries" in state.hazards


def test_emergency_state_ignores_normal_turns():
    state = EmergencyState()
    assert state.observe("I want to book a haircut") is None
    assert state.matched is False
    assert state.severity is None


# ---------------------------------------------------------------------------
# message_text extraction (defensive)
# ---------------------------------------------------------------------------
def test_message_text_shapes():
    assert message_text(None) == ""
    assert message_text("plain string") == "plain string"
    assert message_text(SimpleNamespace(content=["hello ", "there"])) == "hello  there"
    assert message_text(SimpleNamespace(content="single")) == "single"
    assert message_text(SimpleNamespace(text="fallback")) == "fallback"
    assert message_text(object()) == ""


# ---------------------------------------------------------------------------
# Live-call emergency lane (EmergencyTurnHook)
# ---------------------------------------------------------------------------
def _hook(dapr, session, agent=None):
    layer = ToolLayer(dapr=dapr, settings=Settings(), ctx=_ctx(), session=session)
    return lw.EmergencyTurnHook(tool_layer=layer, session=session, agent=agent)


async def test_emergency_turn_triggers_full_lane(livekit_stub):
    dapr = FakeDapr()
    session = SessionState(conversation_id="conv-e1", site_slug="demo")
    chat_ctx = _ChatCtx()
    agent = SimpleNamespace(chat_ctx=chat_ctx)
    hook = _hook(dapr, session, agent=agent)

    registry = metrics.reset_registry()
    sm = metrics.activate_session(metrics.SessionMetrics("conv-e1"))
    try:
        hook(SimpleNamespace(role="user", content=["There is a fire in my house, help me"]))
        # (a) session flag latched synchronously.
        assert session.emergency is True
        # (c) escalation runs as a scheduled task.
        await asyncio.sleep(0.05)
    finally:
        metrics.activate_session(None)

    # (c) warm handoff via the EXISTING escalation API, reason "emergency".
    assert session.escalation_room == "escalation-conv-e1"
    assert livekit_stub.create_room_calls == ["escalation-conv-e1"]
    escalations = [
        e
        for (_p, _t, e) in dapr.published
        if e["type"] == "com.opendesk.conversation.EscalationRequested"
    ]
    assert len(escalations) == 1
    assert escalations[0]["data"]["reason"] == "emergency"

    # (b) metric counter + per-session quality flag.
    assert "voice_emergency_sessions_total 1" in registry.render()
    payload = sm.quality_payload()
    assert payload is not None
    assert payload["emergency"] is True

    # (d) location-first addendum injected as a system message.
    assert any(
        role == "system" and "LOCATION FIRST" in text
        for role, text in chat_ctx.appended
    )

    # A second emergency turn must NOT re-trigger the escalation.
    hook(SimpleNamespace(role="user", content=["someone is dying now"]))
    await asyncio.sleep(0.05)
    escalations = [
        e
        for (_p, _t, e) in dapr.published
        if e["type"] == "com.opendesk.conversation.EscalationRequested"
    ]
    assert len(escalations) == 1
    assert registry.render().count("voice_emergency_sessions_total 1") == 1


async def test_normal_turn_does_not_trigger(livekit_stub):
    dapr = FakeDapr()
    session = SessionState(conversation_id="conv-e2", site_slug="demo")
    chat_ctx = _ChatCtx()
    hook = _hook(dapr, session, agent=SimpleNamespace(chat_ctx=chat_ctx))

    registry = metrics.reset_registry()
    hook(SimpleNamespace(role="user", content=["I want to book a haircut tomorrow"]))
    await asyncio.sleep(0.02)

    assert getattr(session, "emergency", False) is False
    assert session.escalation_room is None
    assert dapr.published == []
    assert chat_ctx.appended == []
    # No series recorded (only the HELP/TYPE header lines render).
    assert "voice_emergency_sessions_total 1" not in registry.render()


async def test_hook_tolerates_missing_agent(livekit_stub):
    """No agent handle: escalation + metrics still fire, addendum skipped."""
    dapr = FakeDapr()
    session = SessionState(conversation_id="conv-e3", site_slug="demo")
    hook = _hook(dapr, session, agent=None)

    metrics.reset_registry()
    hook(SimpleNamespace(role="user", content=["armed robbery happening now"]))
    await asyncio.sleep(0.05)

    assert session.emergency is True
    assert session.escalation_room == "escalation-conv-e3"


async def test_hook_skips_double_escalation(livekit_stub):
    """Caller already escalated (asked for a human): the emergency flag and
    metric still latch, but no second escalation room is opened."""
    dapr = FakeDapr()
    session = SessionState(
        conversation_id="conv-e4",
        site_slug="demo",
        escalation_room="escalation-conv-e4",
    )
    hook = _hook(dapr, session)

    metrics.reset_registry()
    hook(SimpleNamespace(role="user", content=["there is an emergency, fire"]))
    await asyncio.sleep(0.05)

    assert session.emergency is True
    assert session.escalation_room == "escalation-conv-e4"
    assert livekit_stub.create_room_calls == []  # no second room
    escalations = [
        e
        for (_p, _t, e) in dapr.published
        if e["type"] == "com.opendesk.conversation.EscalationRequested"
    ]
    assert escalations == []


async def test_hook_never_raises_on_garbage_message():
    session = SessionState(conversation_id="conv-e5", site_slug="demo")
    hook = _hook(FakeDapr(), session)
    hook(None)
    hook(object())
    hook(SimpleNamespace(content=[{"not": "text"}]))
    assert getattr(session, "emergency", False) is False


# ---------------------------------------------------------------------------
# capture_location tool (mocked Dapr, Wave-8 PUT contract)
# ---------------------------------------------------------------------------
def test_capture_location_in_tool_names():
    assert "capture_location" in TOOL_NAMES


def _layer(dapr, session):
    return ToolLayer(dapr=dapr, settings=Settings(), ctx=_ctx(), session=session)


async def test_capture_location_lat_lng():
    dapr = FakeDapr()
    dapr.get_responses["internal/contacts"] = {"id": "contact-1", "phone": "+234801"}
    session = SessionState(
        conversation_id="c1", site_slug="demo", confirmed_phone="+234801"
    )
    result = await _layer(dapr, session).capture_location(lat=6.5244, lng=3.3792)

    assert result["status"] == "ok"
    assert result["contact_id"] == "contact-1"
    # Contact resolved via the SIP caller-ID pattern (confirmed phone).
    assert dapr.invoke_get_calls[0][1] == "internal/contacts"
    assert dapr.invoke_get_calls[0][2] == {"phone": "+234801"}
    assert dapr.invoke_get_calls[0][3] == {"X-Tenant-Slug": "acme"}
    # Wave-8 contract: PUT /v1/contacts/{id}/location with {lat, lng}.
    app_id, method, payload, headers = dapr.put_calls[0]
    assert method == "v1/contacts/contact-1/location"
    assert payload == {"lat": 6.5244, "lng": 3.3792, "source": "manual"}
    assert headers == {"X-Tenant-Slug": "acme"}


async def test_capture_location_address_text():
    dapr = FakeDapr()
    dapr.get_responses["internal/contacts"] = {"id": "contact-2"}
    session = SessionState(
        conversation_id="c2", site_slug="demo", confirmed_phone="+234802"
    )
    result = await _layer(dapr, session).capture_location(
        address_text="12 Allen Avenue, Ikeja, Lagos"
    )

    assert result["status"] == "ok"
    # Address-only payload (server geocodes per the Wave-8 contract).
    assert dapr.put_calls[0][2] == {"address": "12 Allen Avenue, Ikeja, Lagos"}


async def test_capture_location_no_arguments():
    session = SessionState(
        conversation_id="c3", site_slug="demo", confirmed_phone="+234803"
    )
    result = await _layer(FakeDapr(), session).capture_location()
    assert result["status"] == "error"
    assert "address" in result["message"].lower()


async def test_capture_location_no_confirmed_phone():
    dapr = FakeDapr()
    session = SessionState(conversation_id="c4", site_slug="demo")
    result = await _layer(dapr, session).capture_location(address_text="Ikeja")
    assert result["status"] == "error"
    assert dapr.invoke_get_calls == []
    assert dapr.put_calls == []


async def test_capture_location_contact_not_found():
    dapr = FakeDapr()  # invoke_get default {} -> no contact id
    session = SessionState(
        conversation_id="c5", site_slug="demo", confirmed_phone="+234805"
    )
    result = await _layer(dapr, session).capture_location(address_text="Ikeja")
    assert result["status"] == "error"
    assert "contact" in result["message"].lower()
    assert dapr.put_calls == []


async def test_capture_location_never_raises_on_dapr_failure():
    dapr = FakeDapr()
    dapr.get_responses["internal/contacts"] = ConnectionError("sidecar down")
    session = SessionState(
        conversation_id="c6", site_slug="demo", confirmed_phone="+234806"
    )
    result = await _layer(dapr, session).capture_location(lat=6.5, lng=3.4)
    assert result["status"] == "error"
    assert "could not be saved" in result["message"]


async def test_capture_location_dispatch():
    dapr = FakeDapr()
    dapr.get_responses["internal/contacts"] = {"id": "contact-7"}
    session = SessionState(
        conversation_id="c7", site_slug="demo", confirmed_phone="+234807"
    )
    result = await _layer(dapr, session).dispatch(
        "capture_location", {"address_text": "Surulere, Lagos"}
    )
    assert result["status"] == "ok"
    assert dapr.put_calls[0][1] == "v1/contacts/contact-7/location"


# ---------------------------------------------------------------------------
# Spoken AI disclosure greeting (SPEC-W11 Part C §5)
# ---------------------------------------------------------------------------
DEFAULT_GREETING = "Hello, thank you for calling Acme. How can I help you today?"


def test_greeting_byte_identical_without_disclosure_block():
    ctx = _ctx()
    assert build_greeting(ctx) == DEFAULT_GREETING


def test_greeting_ignores_non_dict_disclosure():
    ctx = _ctx()
    ctx.disclosure = "yes"  # defensive: garbage values change nothing
    assert build_greeting(ctx) == DEFAULT_GREETING


def test_greeting_spoken_ai_disclosure_prepended():
    ctx = _ctx()
    ctx.disclosure = {"spokenAiDisclosure": True, "recordingConsent": False}
    greeting = build_greeting(ctx)
    assert greeting == f"{AI_DISCLOSURE_LINE} {DEFAULT_GREETING}"
    assert RECORDING_NOTICE_LINE not in greeting


def test_greeting_recording_consent_appended():
    ctx = _ctx()
    ctx.disclosure = {"spokenAiDisclosure": False, "recordingConsent": True}
    greeting = build_greeting(ctx)
    assert greeting == f"{DEFAULT_GREETING} {RECORDING_NOTICE_LINE}"


def test_greeting_full_disclosure_with_custom_text():
    ctx = _ctx()
    ctx.disclosure = {
        "spokenAiDisclosure": True,
        "recordingConsent": True,
        "text": "I can connect you to emergency services.",
    }
    greeting = build_greeting(ctx)
    assert greeting.startswith(
        f"{AI_DISCLOSURE_LINE} I can connect you to emergency services."
    )
    assert DEFAULT_GREETING in greeting
    assert greeting.endswith(RECORDING_NOTICE_LINE)
