"""D6 ghost_booking: >=3 create->cancel flash cycles (<=10 min) by the same
staff member in a day => medium."""

from datetime import timedelta

from fraud_engine.detectors.d6_ghost import GhostBookingDetector

from conftest import NOW, TENANT, ts


def _flash_cancel(graph, staff, booking_id, created_min_ago, held_minutes=5):
    created = NOW - timedelta(minutes=created_min_ago)
    cancelled = created + timedelta(minutes=held_minutes)
    graph.add_booking(
        TENANT, booking_id,
        status="cancelled", created_by=staff,
        created_at=created.isoformat(), cancelled_at=cancelled.isoformat(),
    )


def test_fires_medium_on_three_flash_cancels(client, graph, settings):
    for i in range(3):
        _flash_cancel(graph, "staff-1", f"bk-{i}", created_min_ago=60 + i * 20)
    findings = GhostBookingDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 1
    f = findings[0]
    assert f.type == "ghost_booking" and f.severity == "medium"
    assert f.agent_id == "staff-1" and f.person_id is None
    assert f.evidence["cycle_count"] == 3


def test_silent_with_only_two_cycles(client, graph, settings):
    for i in range(2):
        _flash_cancel(graph, "staff-1", f"bk-{i}", created_min_ago=60 + i * 20)
    assert GhostBookingDetector().detect(client, TENANT, settings, NOW) == []


def test_silent_when_cancels_are_slow(client, graph, settings):
    # held for an hour before cancelling: legitimate cancels, not ghosts
    for i in range(4):
        _flash_cancel(graph, "staff-1", f"bk-{i}", created_min_ago=300 + i * 90,
                      held_minutes=60)
    assert GhostBookingDetector().detect(client, TENANT, settings, NOW) == []


def test_silent_when_split_across_staff(client, graph, settings):
    _flash_cancel(graph, "staff-1", "bk-1", 60)
    _flash_cancel(graph, "staff-2", "bk-2", 80)
    _flash_cancel(graph, "staff-3", "bk-3", 100)
    assert GhostBookingDetector().detect(client, TENANT, settings, NOW) == []
