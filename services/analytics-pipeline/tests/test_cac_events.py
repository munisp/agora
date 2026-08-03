"""FunnelEvent parsing tests (SPEC-W13 contract §2): envelope shapes, key
tolerance, enum guard, timestamp/amount normalisation."""

from __future__ import annotations

from datetime import UTC, date, datetime
from decimal import Decimal

from analytics_pipeline.cac_events import parse_funnel_event

T1 = "11111111-1111-1111-1111-111111111111"
CAMP = "33333333-3333-3333-3333-333333333333"


def _envelope(**data):
    return {
        "specversion": "1.0",
        "id": "env-1",
        "source": "booking-service",
        "type": "com.opendesk.cac.FunnelEvent",
        "time": "2026-01-15T10:00:00Z",
        "tenantid": T1,
        "data": data,
    }


def _data(**over):
    base = {
        "event_id": "ev-1",
        "tenant_id": T1,
        "entity_type": "lead",
        "entity_id": "lead-9",
        "event_name": "lead_created",
        "event_ts": "2026-01-15T10:00:00Z",
        "channel": "whatsapp",
        "campaign_id": CAMP,
        "lga_id": 42,
        "amount_ngn": None,
        "idempotency_key": "idem-1",
    }
    base.update(over)
    return base


def test_full_envelope_parses():
    ev = parse_funnel_event(_envelope(**_data()))
    assert ev is not None
    assert ev.event_id == "ev-1"
    assert ev.tenant_id == T1
    assert ev.event_name == "lead_created"
    assert ev.channel == "whatsapp"
    assert ev.campaign_id == CAMP
    assert ev.lga_id == 42
    assert ev.idempotency_key == "idem-1"
    assert ev.day == date(2026, 1, 15)
    assert ev.is_lead and not ev.is_conversion
    assert ev.revenue_ngn == Decimal("0")


def test_camel_case_keys_tolerated():
    data = {
        "eventId": "ev-2",
        "tenantId": T1,
        "eventName": "converted",
        "eventTs": "2026-01-15T12:00:00+00:00",
        "channel": "web",
        "amountNgn": "15000.50",
        "idempotencyKey": "idem-2",
    }
    ev = parse_funnel_event(_envelope(**data))
    assert ev is not None
    assert ev.event_name == "converted"
    assert ev.is_conversion
    assert ev.amount_ngn == Decimal("15000.50")
    assert ev.revenue_ngn == Decimal("15000.50")


def test_bare_payload_tolerated():
    ev = parse_funnel_event(_data(event_name="first_txn", amount_ngn=20000))
    assert ev is not None
    assert not ev.is_conversion  # first_txn counts revenue, not conversion
    assert ev.revenue_ngn == Decimal("20000")


def test_unknown_event_name_dropped():
    assert parse_funnel_event(_envelope(**_data(event_name="page_view"))) is None


def test_non_funnel_type_dropped():
    payload = _envelope(**_data())
    payload["type"] = "com.opendesk.booking.BookingCreated"
    assert parse_funnel_event(payload) is None


def test_missing_required_fields_dropped():
    # data.tenant_id=None falls back to the CE `tenantid` extension (same
    # tolerance as mapping.py) — only missing in BOTH is a drop.
    payload = _envelope(**_data(tenant_id=None))
    assert parse_funnel_event(payload) is not None
    del payload["tenantid"]
    assert parse_funnel_event(payload) is None
    # event_ts missing in data falls back to envelope time; both missing -> drop
    payload = _envelope(**_data(event_ts=None))
    assert parse_funnel_event(payload) is not None
    del payload["time"]
    assert parse_funnel_event(payload) is None
    assert parse_funnel_event(_envelope(**_data(event_ts=None))) is not None
    assert parse_funnel_event(_envelope(**_data(event_name=None))) is None
    assert parse_funnel_event({}) is None
    assert parse_funnel_event("not-a-mapping") is None


def test_idempotency_key_falls_back_to_event_id_then_envelope_id():
    ev = parse_funnel_event(_envelope(**_data(idempotency_key=None)))
    assert ev is not None and ev.idempotency_key == "ev-1"
    ev = parse_funnel_event(_envelope(**_data(idempotency_key=None, event_id=None)))
    assert ev is not None and ev.idempotency_key == "env-1"


def test_epoch_millis_and_naive_timestamps():
    ev = parse_funnel_event(
        _envelope(**_data(event_ts=1768000000000))  # epoch millis
    )
    assert ev is not None and ev.event_ts.tzinfo is not None
    ev2 = parse_funnel_event(_envelope(**_data(event_ts="2026-01-15 10:00:00")))
    assert ev2 is not None and ev2.event_ts == datetime(2026, 1, 15, 10, 0, tzinfo=UTC)


def test_bad_amount_and_lga_are_null_safe():
    ev = parse_funnel_event(_envelope(**_data(amount_ngn="abc", lga_id="x")))
    assert ev is not None
    assert ev.amount_ngn is None
    assert ev.lga_id is None
