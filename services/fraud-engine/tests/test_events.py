"""CloudEvent shape (SPEC-W30 §3) + Kafka trigger routing."""

from fraud_engine.alerts import AlertRecord
from fraud_engine.events import (
    ALERT_RAISED_TYPE,
    DEFAULT_ALERTS_TOPIC,
    InMemoryPublisher,
    alert_raised_event,
)
from fraud_engine.main import detectors_for_event

from conftest import TENANT


def _record() -> AlertRecord:
    return AlertRecord(
        alert_id=f"sybil_cluster:{TENANT}:p1:abc123",
        tenant_id=TENANT,
        type="sybil_cluster",
        severity="high",
        status="open",
        person_id="p1",
        agent_id="agent-1",
        evidence={"detector": "d2_sybil_cluster"},
        created_at="2026-08-05T12:00:00+00:00",
    )


def test_cloudevent_shape():
    event = alert_raised_event(TENANT, _record())
    assert event["specversion"] == "1.0"
    assert event["type"] == ALERT_RAISED_TYPE == "com.opendesk.fraud.AlertRaised"
    # SPEC: id = tenant:alert:alert_id
    assert event["id"] == f"{TENANT}:alert:sybil_cluster:{TENANT}:p1:abc123"
    assert event["tenantid"] == TENANT  # extension
    assert event["source"] == "fraud-engine"
    assert event["time"]
    assert event["data"] == {
        "alert_id": f"sybil_cluster:{TENANT}:p1:abc123",
        "type": "sybil_cluster",
        "severity": "high",
        "person_id": "p1",
        "agent_id": "agent-1",
    }
    assert DEFAULT_ALERTS_TOPIC == "opendesk.fraud.alerts.v1"


def test_cloudevent_data_omits_absent_ids():
    rec = _record()
    object.__setattr__(rec, "person_id", None)
    object.__setattr__(rec, "agent_id", None)
    data = alert_raised_event(TENANT, rec)["data"]
    assert "person_id" not in data and "agent_id" not in data


def test_inmemory_publisher_records_topic_key_event():
    pub = InMemoryPublisher()
    pub.publish(DEFAULT_ALERTS_TOPIC, "k1", {"x": 1})
    assert pub.published == [(DEFAULT_ALERTS_TOPIC, "k1", {"x": 1})]


def test_kafka_trigger_routing():
    # D3 on capture/lead events; D5 on consent/messaging events (SPEC §3)
    # REAL produced type on cac.events: booking-service leads.go EventTypeFunnel.
    assert detectors_for_event("com.opendesk.cac.FunnelEvent") == ["d3_capture_velocity"]
    # Legacy/fictional type kept as compatibility coverage.
    assert detectors_for_event("com.opendesk.cac.LeadCaptured") == ["d3_capture_velocity"]
    assert detectors_for_event("com.opendesk.identity.ConsentGranted") == ["d5_consent_backdating"]
    assert detectors_for_event("com.opendesk.conversation.MessageSent") == ["d5_consent_backdating"]
    assert detectors_for_event("com.opendesk.booking.BookingCreated") == []
