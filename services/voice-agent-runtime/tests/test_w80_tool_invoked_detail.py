"""SPEC-W80 C5 + B7 (additive tests):

- C5: the book_appointment ToolInvoked event detail must carry the confirmed
  contact phone/email so crm-sync's AI-booking note path (detail.phone /
  detail.email) activates. Guarded: keys are included only when present.
- B7 (G9): capture_location's GET /internal/contacts is internAuth-gated —
  the call must send X-Internal-Token (VOICE_BOOKING_INTERNAL_TOKEN), the
  same token mechanism as v1/bookings (lookup_appointment).
"""

import asyncio

from app.config import Settings
from app.session_state import SessionState
from app.tenant_context import TenantContext
from app.tools import ToolLayer

from conftest import FakeDapr


async def _drain_events() -> None:
    """ToolInvoked publishes are fire-and-forget (SPEC-W46 P9) — yield to
    the loop so scheduled publishes land in FakeDapr."""
    await asyncio.sleep(0)
    await asyncio.sleep(0)


def _ctx() -> TenantContext:
    return TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")


def _settings(**over) -> Settings:
    base = dict(
        booking_url="http://booking:7002",
        voice_booking_internal_token="book-tok",
        voice_admin_internal_token="adm-tok",
        phone_hash_salt="test-salt",
        phone_confirmation_required=False,
    )
    base.update(over)
    return Settings(**base)


def _tool_invoked_events(dapr: FakeDapr) -> list[dict]:
    return [evt for _ps, _t, evt in dapr.best_effort
            if evt.get("type") == "com.opendesk.conversation.ToolInvoked"]


async def test_book_appointment_tool_event_includes_phone_and_email():
    dapr = FakeDapr()
    tools = ToolLayer(
        dapr=dapr,  # type: ignore[arg-type]
        settings=_settings(),
        ctx=_ctx(),
        session=SessionState(conversation_id="c1", site_slug="demo"),
    )
    result = await tools.book_appointment(
        offering_id="off-1",
        team_member_id="tm-1",
        starts_at="2030-01-01T10:00:00Z",
        phone="+15551234567",
        contact_name="Jane",
        email="jane@example.com",
    )
    assert result["status"] == "accepted"
    await _drain_events()
    events = _tool_invoked_events(dapr)
    assert len(events) == 1
    data = events[0]["data"]
    assert data["tool"] == "book_appointment"
    assert data["status"] == "accepted"
    assert data["detail"]["phone"] == "+15551234567"
    assert data["detail"]["email"] == "jane@example.com"
    # Pre-W80 keys are preserved.
    assert data["detail"]["offering_id"] == "off-1"
    assert data["detail"]["starts_at"] == "2030-01-01T10:00:00Z"


async def test_book_appointment_tool_event_omits_absent_email():
    dapr = FakeDapr()
    tools = ToolLayer(
        dapr=dapr,  # type: ignore[arg-type]
        settings=_settings(),
        ctx=_ctx(),
        session=SessionState(conversation_id="c1", site_slug="demo"),
    )
    result = await tools.book_appointment(
        offering_id="off-1",
        team_member_id="tm-1",
        starts_at="2030-01-01T10:00:00Z",
        phone="+15551234567",
    )
    assert result["status"] == "accepted"
    await _drain_events()
    events = _tool_invoked_events(dapr)
    assert len(events) == 1
    detail = events[0]["data"]["detail"]
    assert detail["phone"] == "+15551234567"
    assert "email" not in detail, "guarded: no email key when no email was given"


async def test_capture_location_sends_internal_token():
    """B7 (G9): GET /internal/contacts?phone= is internAuth-gated — the
    capture_location lane must send X-Internal-Token
    (VOICE_BOOKING_INTERNAL_TOKEN) on both booking-service calls."""
    dapr = FakeDapr()
    dapr.get_responses["internal/contacts"] = {"id": "contact-1", "phone": "+15551234567"}
    tools = ToolLayer(
        dapr=dapr,  # type: ignore[arg-type]
        settings=_settings(),
        ctx=_ctx(),
        session=SessionState(
            conversation_id="c9", site_slug="demo", confirmed_phone="+15551234567"
        ),
    )
    result = await tools.capture_location(lat=6.5244, lng=3.3792)
    assert result["status"] == "ok"

    get_app, get_method, _params, get_headers = dapr.invoke_get_calls[0]
    assert get_method == "internal/contacts"
    assert get_headers.get("X-Internal-Token") == "book-tok", (
        "internal/contacts must carry X-Internal-Token (B7)"
    )
    assert get_headers.get("X-Tenant-Slug") == "acme"

    _app, _method, _payload, put_headers = dapr.put_calls[0]
    assert put_headers.get("X-Internal-Token") == "book-tok", (
        "the contact location PUT must carry X-Internal-Token too"
    )


async def test_capture_location_empty_token_sends_no_header():
    """Empty token -> no header (peer fails closed); the pre-W80 header
    shape (X-Tenant-Slug only) is unchanged in that posture."""
    dapr = FakeDapr()
    dapr.get_responses["internal/contacts"] = {"id": "contact-1"}
    tools = ToolLayer(
        dapr=dapr,  # type: ignore[arg-type]
        settings=_settings(voice_booking_internal_token=""),
        ctx=_ctx(),
        session=SessionState(
            conversation_id="c10", site_slug="demo", confirmed_phone="+15550000000"
        ),
    )
    result = await tools.capture_location(lat=6.5, lng=3.3)
    assert result["status"] == "ok"
    _app, _method, _params, get_headers = dapr.invoke_get_calls[0]
    assert get_headers == {"X-Tenant-Slug": "acme"}
