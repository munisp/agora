"""D8 report_spam (SPEC-W32 §3 WS-D).

(a) one reporter opening > CIVIC_REPORT_MAX_PER_DAY (5) cases/day => medium;
(b) >3 open cases same category within 500m AND 24h across DIFFERENT
    reporters (coordinated spam) => medium. Severity is ALWAYS medium and
    report_spam NEVER auto-quarantines (citizens are never banned from
    reporting).
"""

from __future__ import annotations

from datetime import timedelta

from fraud_engine.detectors import ALL_DETECTORS, DetectionRunner
from fraud_engine.detectors.d8_civic import ReportSpamDetector
from fraud_engine.quarantine import AUTO_QUARANTINE_TYPES

from conftest import NOW, OTHER_TENANT, TENANT, ts

# Ikeja reference point; ~55m per 0.0005 degree of latitude.
BASE_LAT, BASE_LON = 6.6018, 3.3515


def add_report(
    g,
    tenant,
    person_id,
    ref,
    *,
    minutes_ago=60,
    category="roads",
    status="new",
    lat=None,
    lon=None,
):
    """Project one civic case: Person -[:REPORTED]-> Case [-[:AT]-> Location]."""
    pkey = None
    if person_id is not None:
        pkey = g.person_key(tenant, person_id) or g.add_person(tenant, person_id)
    ckey = g.add_case(
        tenant, ref, category=category, status=status, created_at=ts(minutes_ago)
    )
    if pkey is not None:
        g.add_edge(pkey, "REPORTED", ckey, tenant_id=tenant)
    if lat is not None:
        lkey = g.add_location(tenant, lat=lat, lon=lon)
        g.add_edge(ckey, "AT", lkey, tenant_id=tenant)
    return ckey


def velocity_fixture(g, tenant, person_id="p-spam", count=6):
    for i in range(count):
        add_report(g, tenant, person_id, f"REF-{tenant}-{i}", minutes_ago=30 + i)


def coordinated_fixture(g, tenant, count=4, category="waste", status="new"):
    for i in range(count):
        add_report(
            g,
            tenant,
            f"reporter-{i}",
            f"COORD-{tenant}-{i}",
            minutes_ago=60 + i * 30,
            category=category,
            status=status,
            lat=BASE_LAT + i * 0.0004,  # ~45m steps, all inside 500m
            lon=BASE_LON,
        )


# -- signal (a): reporter velocity ------------------------------------------


def test_velocity_fires_over_daily_max(client, graph, settings):
    velocity_fixture(graph, TENANT, count=6)
    findings = ReportSpamDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 1
    f = findings[0]
    assert f.type == "report_spam" and f.severity == "medium"
    assert f.person_id == "p-spam"
    ev = f.evidence
    assert ev["signal"] == "reporter_velocity"
    assert ev["case_count"] == 6 and ev["threshold"] == 5
    assert len(ev["case_refs"]) == 6 and ev["day"] == NOW.date().isoformat()


def test_velocity_silent_at_exactly_threshold(client, graph, settings):
    velocity_fixture(graph, TENANT, count=5)
    assert ReportSpamDetector().detect(client, TENANT, settings, NOW) == []


def test_velocity_silent_when_spread_across_days(client, graph, settings):
    for i in range(4):
        add_report(graph, TENANT, "p-busy", f"D0-{i}", minutes_ago=30 + i)
    for i in range(4):  # yesterday: 4 more, still <= 5 per UTC day
        add_report(graph, TENANT, "p-busy", f"D1-{i}", minutes_ago=30 + i + 24 * 60)
    assert ReportSpamDetector().detect(client, TENANT, settings, NOW) == []


# -- signal (b): coordinated geo/category spam -------------------------------


def test_coordinated_fires_across_reporters(client, graph, settings):
    coordinated_fixture(graph, TENANT, count=4)
    findings = ReportSpamDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 1
    f = findings[0]
    assert f.type == "report_spam" and f.severity == "medium"
    assert f.person_id is None, "coordinated spam has no single subject"
    ev = f.evidence
    assert ev["signal"] == "coordinated_spam"
    assert ev["case_count"] == 4 and ev["reporter_count"] == 4
    assert ev["category"] == "waste"
    assert abs(ev["centroid"]["lat"] - (BASE_LAT + 0.0006)) < 1e-6
    assert abs(ev["centroid"]["lon"] - BASE_LON) < 1e-6
    assert len(ev["case_refs"]) == 4 and ev["radius_m"] == 500.0


def test_coordinated_silent_at_threshold(client, graph, settings):
    coordinated_fixture(graph, TENANT, count=3)  # threshold is >3
    assert ReportSpamDetector().detect(client, TENANT, settings, NOW) == []


def test_coordinated_silent_single_reporter(client, graph, settings):
    for i in range(4):
        add_report(
            graph,
            TENANT,
            "same-person",
            f"SOLO-{i}",
            minutes_ago=60 + i * 10,
            category="waste",
            lat=BASE_LAT + i * 0.0004,
            lon=BASE_LON,
        )
    assert ReportSpamDetector().detect(client, TENANT, settings, NOW) == []


def test_coordinated_silent_when_geo_spread(client, graph, settings):
    for i in range(4):
        add_report(
            graph,
            TENANT,
            f"far-{i}",
            f"FAR-{i}",
            minutes_ago=60 + i * 10,
            category="waste",
            lat=BASE_LAT + i * 0.02,  # ~2.2km steps
            lon=BASE_LON,
        )
    assert ReportSpamDetector().detect(client, TENANT, settings, NOW) == []


def test_coordinated_silent_when_time_spread(client, graph, settings):
    for i in range(4):
        add_report(
            graph,
            TENANT,
            f"slow-{i}",
            f"SLOW-{i}",
            minutes_ago=60 + i * 25 * 60,  # 25h apart: gaps break the 24h window
            category="waste",
            lat=BASE_LAT + i * 0.0004,
            lon=BASE_LON,
        )
    assert ReportSpamDetector().detect(client, TENANT, settings, NOW) == []


def test_coordinated_ignores_resolved_cases(client, graph, settings):
    coordinated_fixture(graph, TENANT, count=3)
    add_report(
        graph,
        TENANT,
        "reporter-x",
        "COORD-RESOLVED",
        minutes_ago=90,
        category="waste",
        status="resolved",
        lat=BASE_LAT,
        lon=BASE_LON,
    )
    assert ReportSpamDetector().detect(client, TENANT, settings, NOW) == []


def test_coordinated_requires_same_category(client, graph, settings):
    for i, cat in enumerate(["waste", "roads", "water", "power"]):
        add_report(
            graph,
            TENANT,
            f"mix-{i}",
            f"MIX-{i}",
            minutes_ago=60 + i * 10,
            category=cat,
            lat=BASE_LAT + i * 0.0004,
            lon=BASE_LON,
        )
    assert ReportSpamDetector().detect(client, TENANT, settings, NOW) == []


# -- cross-cutting: tenancy, dedup, quarantine posture -----------------------


def test_tenant_isolation(client, graph, settings):
    velocity_fixture(graph, OTHER_TENANT, count=6)
    coordinated_fixture(graph, OTHER_TENANT, count=4)
    assert ReportSpamDetector().detect(client, TENANT, settings, NOW) == []
    # and the tripped tenant does fire (the fixture is real)
    assert len(ReportSpamDetector().detect(client, OTHER_TENANT, settings, NOW)) == 2


def test_runner_dedup_second_sweep_creates_no_new_alerts(client, graph, publisher, settings):
    velocity_fixture(graph, TENANT, count=6)
    coordinated_fixture(graph, TENANT, count=4)
    runner = DetectionRunner(client, publisher, settings, detectors=[ReportSpamDetector()])
    first = runner.run(tenant_id=TENANT, now=NOW)
    assert first.alerts_created == 2 and first.alerts_deduped == 0
    second = runner.run(tenant_id=TENANT, now=NOW + timedelta(minutes=15))
    assert second.alerts_created == 0 and second.alerts_deduped == 2
    spam_alerts = [a for a in graph.alerts.values() if a["type"] == "report_spam"]
    assert len(spam_alerts) == 2
    assert all(a["severity"] == "medium" for a in spam_alerts)


def test_report_spam_never_auto_quarantines(client, graph, publisher, settings):
    assert "report_spam" not in AUTO_QUARANTINE_TYPES
    velocity_fixture(graph, TENANT, person_id="p-citizen", count=8)
    report = DetectionRunner(
        client, publisher, settings, detectors=[ReportSpamDetector()]
    ).run(tenant_id=TENANT, now=NOW)
    assert report.alerts_created == 1
    assert report.quarantined == []
    key = graph.person_key(TENANT, "p-citizen")
    assert graph.nodes[key]["props"].get("quarantine") is not True
    # The person still gets the risk flag for operator triage (medium alert).
    assert "report_spam" in graph.nodes[key]["props"].get("risk_flags", [])


def test_registered_in_detector_registry():
    names = [d.name for d in ALL_DETECTORS]
    assert "d8_report_spam" in names
    det = next(d for d in ALL_DETECTORS if d.name == "d8_report_spam")
    assert det.alert_type == "report_spam"


def test_kafka_trigger_routes_civic_events():
    from fraud_engine.main import detectors_for_event

    assert detectors_for_event("com.opendesk.civic.ReportReceived") == ["d8_report_spam"]
    assert detectors_for_event("com.opendesk.civic.StatusChanged") == ["d8_report_spam"]
    assert detectors_for_event("com.opendesk.booking.BookingCreated") == []
