"""DetectionRunner end-to-end: full sweep across tenants, F1-high
auto-quarantine wiring, per-detector error isolation."""

from fraud_engine.detectors import DetectionRunner
from fraud_engine.detectors.base import Detector

from conftest import NOW, OTHER_TENANT, TENANT, add_booking_for, make_cycle


def test_f1_high_ring_quarantines_ring_members(client, graph, publisher, settings):
    make_cycle(graph, TENANT, ["p1", "p2", "p3"])
    add_booking_for(graph, TENANT, "p1")  # reward-bearing -> ring is HIGH
    report = DetectionRunner(client, publisher, settings).run(tenant_id=TENANT, now=NOW)
    assert report.alerts_created == 3
    assert sorted(report.quarantined) == ["p1", "p2", "p3"]
    for _, props in graph.persons(TENANT):
        assert props["quarantine"] is True
        assert "referral_cycle" in props["risk_flags"]


def test_medium_ring_does_not_quarantine(client, graph, publisher, settings):
    make_cycle(graph, TENANT, ["p1", "p2", "p3"])  # no conversion -> medium
    report = DetectionRunner(client, publisher, settings).run(tenant_id=TENANT, now=NOW)
    assert report.alerts_created == 3
    assert report.quarantined == []
    for _, props in graph.persons(TENANT):
        assert props.get("quarantine") is not True


def test_sweep_discovers_all_tenants(client, graph, publisher, settings):
    graph.add_tenant(TENANT)
    graph.add_tenant(OTHER_TENANT)
    make_cycle(graph, TENANT, ["p1", "p2", "p3"])
    make_cycle(graph, OTHER_TENANT, ["x1", "x2", "x3"])
    report = DetectionRunner(client, publisher, settings).run(now=NOW)
    assert report.tenants == sorted([TENANT, OTHER_TENANT])
    assert report.alerts_created == 6
    assert len(publisher.published) == 6


def test_one_detector_error_does_not_sink_sweep(client, graph, publisher, settings):
    class Boom(Detector):
        name = "boom"
        alert_type = "boom"

        def detect(self, client, tenant_id, settings, now):
            raise RuntimeError("graph went away")

    make_cycle(graph, TENANT, ["p1", "p2", "p3"])
    from fraud_engine.detectors import ALL_DETECTORS

    runner = DetectionRunner(client, publisher, settings, detectors=[Boom(), *ALL_DETECTORS])
    report = runner.run(tenant_id=TENANT, now=NOW)
    assert any("boom" in e for e in report.errors)
    assert report.alerts_created == 3  # D1 still ran
