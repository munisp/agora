"""Full-platform e2e flow (SPEC-W3 §1):

    provision tenant -> seed catalog -> availability -> public booking ->
    saga events on Kafka -> crm-sync sync_map row -> OpenSearch
    conversation doc -> lakehouse bronze row.

Tests run in file order against a live compose stack (see conftest.py) and
share state through the `flow` fixture. Steps that depend on optional
profiles/components (crm-sync + Twenty, OpenSearch indexing, Trino/Iceberg)
skip with an explicit reason when that component is not up instead of
failing spuriously.
"""
from __future__ import annotations

from datetime import UTC, datetime, timedelta

import pytest
import requests

from conftest import (
    BOOKING,
    CRM_SYNC,
    GW,
    OPENSEARCH,
    json_lines,
    kafka_read,
    poll,
    psql,
    trino_query,
    wait_http_ok,
)

pytestmark = pytest.mark.docker


def test_01_tenant_provisioned_and_seeded(tenant, flow):
    """Provision + seed: tenant exists, catalog seeded, public site live."""
    flow["slug"] = tenant["slug"]
    flow["offering_id"] = tenant["offering"]["id"]
    flow["member_id"] = tenant["member"]["id"]
    assert tenant["context"]["offerings"], "public context has no bookable offerings"
    assert tenant["context"]["team_members"], "public context has no active team members"


def test_02_availability_has_slots(tenant, flow):
    """Availability engine returns bookable slots for the seeded member."""
    frm = datetime.now(UTC).replace(hour=0, minute=0, second=0, microsecond=0) + timedelta(days=1)
    to = frm + timedelta(days=7)
    r = requests.get(
        f"{GW}/api/bookings/public/sites/{flow['slug']}/availability",
        params={
            "offering_id": flow["offering_id"],
            "team_member_id": flow["member_id"],
            "from": frm.strftime("%Y-%m-%dT%H:%M:%SZ"),
            "to": to.strftime("%Y-%m-%dT%H:%M:%SZ"),
        },
        timeout=20,
    )
    assert r.status_code == 200, f"availability failed: {r.status_code} {r.text}"
    # CORR#12: the availability endpoint returns an OBJECT
    # {"offering_id", "slots": [{"starts_at", "ends_at"}, ...]}, not a bare
    # list of slot strings.
    body = r.json()
    assert isinstance(body, dict), f"availability must be an object, got {type(body)}"
    assert body.get("offering_id") == flow["offering_id"], (
        f"offering_id mismatch: {body.get('offering_id')} != {flow['offering_id']}"
    )
    slots = body.get("slots")
    assert isinstance(slots, list) and slots, "no availability slots in the next 7 days"
    first = slots[0]
    assert isinstance(first, dict) and first.get("starts_at") and first.get("ends_at"), (
        f"slot missing starts_at/ends_at: {first}"
    )
    flow["slot_start"] = first["starts_at"]


def test_03_public_booking_created(tenant, flow):
    """Anonymous visitor books through the gateway public route."""
    r = requests.post(
        f"{GW}/api/bookings/public/sites/{flow['slug']}/bookings",
        json={
            "offering_id": flow["offering_id"],
            "team_member_id": flow["member_id"],
            "starts_at": flow["slot_start"],
            "idempotency_key": f"e2e-{flow['slug']}",
            "contact": {
                "name": "E2E Visitor",
                "phone": "+447700900123",
                "email": f"visitor-{flow['slug']}@example.com",
            },
        },
        timeout=30,
    )
    assert r.status_code == 201, f"public booking failed: {r.status_code} {r.text}"
    booking = r.json()
    flow["booking_id"] = booking["id"]
    flow["contact_id"] = booking.get("contact_id")
    assert booking["status"] in ("pending", "confirmed")


def test_04_saga_confirms_booking_and_emits_events(tenant, flow):
    """The BookingSagaWorkflow drives the booking to `confirmed` and the
    outbox dispatcher publishes CloudEvents to opendesk.booking.events."""
    booking_id = flow["booking_id"]

    def _confirmed():
        r = requests.get(
            f"{BOOKING}/v1/bookings/{booking_id}",
            headers=tenant["headers"],
            timeout=10,
        )
        if r.status_code == 200 and r.json().get("status") == "confirmed":
            return r.json()
        return None

    booking = poll(_confirmed, 180, desc="booking confirmed by saga")
    assert booking, "booking never reached status=confirmed (saga/worker down?)"
    flow["booking"] = booking

    def _event():
        for evt in json_lines(kafka_read("opendesk.booking.events")):
            data = evt.get("data", evt)
            if data.get("booking_id") == booking_id or data.get("id") == booking_id:
                return evt
        return None

    event = poll(_event, 120, interval=10, desc="booking event on kafka")
    assert event, "no CloudEvent for the booking found on opendesk.booking.events"
    assert event.get("type", "").startswith("com.opendesk.booking.")
    flow["event"] = event


def test_05_crm_sync_map_row(tenant, flow):
    """crm-sync consumes the booking event and records a sync_map mapping."""
    if not wait_http_ok(f"{CRM_SYNC}/healthz", 10):
        pytest.skip("crm-sync not running (compose without crm include?)")

    booking_id = flow["booking_id"]

    def _mapping():
        out = psql(
            "crm_sync",
            f"SELECT twenty_id FROM sync_map WHERE opendesk_id = '{booking_id}' LIMIT 1;",
        )
        return out or None

    twenty_id = poll(_mapping, 180, interval=10, desc="crm sync_map row")
    assert twenty_id, "no sync_map row for the booking in crm_sync DB"
    flow["twenty_id"] = twenty_id


def test_06_conversation_turn_indexed_in_opensearch(tenant, flow):
    """A voice-agent text turn is persisted and indexed into OpenSearch
    `conversations`. Requires the LLM backend (voice profile / LLM_BASE_URL);
    skips honestly when the chat plane cannot produce a reply."""
    r = requests.post(
        f"{GW}/voice/chat",
        json={"site_slug": flow["slug"], "message": "What services do you offer?"},
        timeout=120,
    )
    if r.status_code != 200:
        pytest.skip(f"voice chat unavailable in this stack: {r.status_code} {r.text[:200]}")
    body = r.json()
    if not body.get("reply"):
        pytest.skip(f"voice chat returned no reply (LLM backend down?): {body}")
    conversation_id = body.get("conversation_id") or body.get("conversation")

    if not wait_http_ok(f"{OPENSEARCH}/conversations/_count", 10):
        pytest.skip("opensearch not reachable")

    def _doc():
        q = {"query": {"match": {"tenant": flow["slug"]}}, "size": 1}
        if conversation_id:
            q = {"query": {"match": {"conversation_id": conversation_id}}, "size": 1}
        resp = requests.post(f"{OPENSEARCH}/conversations/_search", json=q, timeout=15)
        if resp.status_code == 200 and resp.json()["hits"]["total"]["value"] > 0:
            return resp.json()["hits"]["hits"][0]
        return None

    doc = poll(_doc, 180, interval=10, desc="opensearch conversation doc")
    assert doc, "no conversation turn indexed in OpenSearch for this tenant"


def test_07_lakehouse_bronze_row(tenant, flow):
    """analytics-pipeline sinks booking events into Iceberg bronze; the row
    is queryable through Trino."""
    if not wait_http_ok(f"{TRINO}/v1/info", 10):
        pytest.skip("trino not reachable (lakehouse profile not running?)")

    booking_id = flow["booking_id"]

    def _row():
        rows = trino_query(
            "SELECT booking_id, event_type, status "
            "FROM iceberg.bronze.booking_events "
            f"WHERE booking_id = '{booking_id}'"
        )
        return rows or None

    rows = poll(_row, 300, interval=15, desc="bronze booking_events row")
    assert rows, "booking never landed in iceberg.bronze.booking_events"
