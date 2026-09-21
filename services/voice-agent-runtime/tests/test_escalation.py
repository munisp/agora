"""Warm handoff tests (SPEC-W3 §4, innovation 1): request_human tool behaviour
with a mocked livekit-api, plus copilot suggestion posting."""

from __future__ import annotations

import pytest

from app.config import Settings
from app.escalation import LiveKitEscalation, escalation_room_name
from app.session_state import SessionState
from app.tenant_context import TenantContext
from app.tools import TOOL_NAMES, ToolLayer

from conftest import FakeDapr


def _ctx() -> TenantContext:
    return TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")


def _tool_layer(dapr: FakeDapr, session: SessionState) -> ToolLayer:
    return ToolLayer(
        dapr=dapr,  # type: ignore[arg-type]
        settings=Settings(),
        ctx=_ctx(),
        session=session,
    )


def test_request_human_in_tool_names():
    assert "request_human" in TOOL_NAMES


def test_escalation_room_name():
    assert escalation_room_name("abc123") == "escalation-abc123"


async def test_request_human_publishes_event(livekit_stub):
    dapr = FakeDapr()
    session = SessionState(conversation_id="conv-1", site_slug="demo")
    layer = _tool_layer(dapr, session)

    result = await layer.request_human(reason="caller asked for a manager")

    assert result["status"] == "escalated"
    assert result["room"] == "escalation-conv-1"
    assert result["room_created"] is True
    assert "message" in result  # spoken confirmation for the caller
    assert livekit_stub.create_room_calls == ["escalation-conv-1"]
    assert session.escalation_room == "escalation-conv-1"

    # CloudEvent published to opendesk.conversation.events via Dapr
    assert len(dapr.published) == 1
    pubsub, topic, event = dapr.published[0]
    assert topic == "opendesk.conversation.events"
    assert event["type"] == "com.opendesk.conversation.EscalationRequested"
    assert event["subject"] == "acme"
    assert event["tenantid"] == "t-uuid"
    data = event["data"]
    assert data["conversation_id"] == "conv-1"
    assert data["tenant_id"] == "t-uuid"
    assert data["site_slug"] == "demo"
    assert data["room"] == "escalation-conv-1"
    # SPEC-W45 K15(e): the staff join token must NOT ride the events topic —
    # staff mint it on demand via the internal endpoint instead.
    assert "join_token_staff" not in data
    assert "token" not in data
    assert data["staff_token_endpoint"] == (
        "/voice-admin/escalations/conv-1/staff-token"
    )
    assert data["reason"] == "caller asked for a manager"


async def test_request_human_dispatch(livekit_stub):
    dapr = FakeDapr()
    session = SessionState(conversation_id="conv-2", site_slug="demo")
    layer = _tool_layer(dapr, session)

    result = await layer.dispatch("request_human", {"reason": "angry caller"})
    assert result["status"] == "escalated"
    assert session.escalation_room == "escalation-conv-2"


async def test_request_human_graceful_when_livekit_down(livekit_stub):
    livekit_stub.fail = True
    dapr = FakeDapr()
    session = SessionState(conversation_id="conv-3", site_slug="demo")
    layer = _tool_layer(dapr, session)

    result = await layer.request_human()

    # Degraded but functional: staff still get the event + room name.
    assert result["status"] == "escalated"
    assert result["room_created"] is False
    assert len(dapr.published) == 1
    assert dapr.published[0][2]["type"] == "com.opendesk.conversation.EscalationRequested"


async def test_staff_join_token_offline(livekit_stub):
    esc = LiveKitEscalation(Settings())
    token = esc.staff_join_token("escalation-x")
    assert token == "stub-jwt:staff-escalation-x"


async def test_deliver_staff_token_targeted(livekit_stub):
    """K15(e): the staff token goes into the room addressed to the staff
    participant ONLY (destination_identities) — never broadcast."""
    esc = LiveKitEscalation(Settings())
    ok = await esc.deliver_staff_token("escalation-c9", "staff-escalation-c9")
    assert ok is True
    assert len(livekit_stub.send_data_calls) == 1
    req = livekit_stub.send_data_calls[0]
    assert req.room == "escalation-c9"
    assert req.destination_identities == ["staff-escalation-c9"]
    assert req.topic == "staff-credentials"
    assert b"staff_join_token" in req.data
    assert b"stub-jwt:" in req.data


async def test_deliver_staff_token_graceful_when_down(livekit_stub):
    livekit_stub.fail = True
    esc = LiveKitEscalation(Settings())
    ok = await esc.deliver_staff_token("escalation-c9", "staff-escalation-c9")
    assert ok is False  # degraded, no exception


async def test_copilot_suggestion_posted(livekit_stub):
    esc = LiveKitEscalation(Settings())
    ok = await esc.post_suggestion("escalation-c1", {"type": "copilot_suggestion", "text": "try this"})
    assert ok is True
    assert len(livekit_stub.send_data_calls) == 1
    req = livekit_stub.send_data_calls[0]
    assert req.room == "escalation-c1"
    assert b"copilot_suggestion" in req.data
    assert req.topic == "copilot"


async def test_copilot_suggestion_graceful_when_down(livekit_stub):
    livekit_stub.fail = True
    esc = LiveKitEscalation(Settings())
    ok = await esc.post_suggestion("escalation-c2", {"type": "copilot_suggestion"})
    assert ok is False  # degraded, no exception


def test_http_url_conversion():
    from app.escalation import _http_url

    assert _http_url("ws://livekit:7880") == "http://livekit:7880"
    assert _http_url("wss://lk.example.com") == "https://lk.example.com"
    assert _http_url("http://livekit:7880") == "http://livekit:7880"
