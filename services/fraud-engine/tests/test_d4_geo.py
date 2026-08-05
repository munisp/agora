"""D4 geo_impossibility + haversine sanity."""

from fraud_engine.detectors.d4_geo import GeoImpossibilityDetector, haversine_km

from conftest import NOW, TENANT, add_capture, ts

LAGOS = (6.5244, 3.3792)
ABUJA = (9.0765, 7.3986)
KANO = (12.0022, 8.5920)


def test_haversine_sanity():
    assert haversine_km(*LAGOS, *LAGOS) == 0.0
    # Lagos -> Abuja is ~536 km great-circle
    assert 480 < haversine_km(*LAGOS, *ABUJA) < 620
    # antipodal ~ half the Earth's circumference
    assert 19_900 < haversine_km(0.0, 0.0, 0.0, 180.0) < 20_100
    # symmetry
    assert abs(haversine_km(*LAGOS, *ABUJA) - haversine_km(*ABUJA, *LAGOS)) < 1e-9


def test_fires_on_impossible_jump(client, graph, settings):
    # Lagos -> Abuja (~536 km) in 10 minutes => ~3200 km/h
    add_capture(graph, TENANT, "p1", "lead-1", "agent-1", ts(20), lat=LAGOS[0], lon=LAGOS[1], lga=None)
    add_capture(graph, TENANT, "p2", "lead-2", "agent-1", ts(10), lat=ABUJA[0], lon=ABUJA[1], lga=None)
    findings = GeoImpossibilityDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 1
    f = findings[0]
    assert f.type == "geo_impossibility" and f.severity == "medium"
    assert f.evidence["jump"]["implied_speed_kmh"] > 1000
    assert f.dedup_key == "agent-1:lead-1>lead-2"


def test_high_for_repeat_offender(client, graph, settings):
    add_capture(graph, TENANT, "p1", "lead-1", "agent-1", ts(30), lat=LAGOS[0], lon=LAGOS[1], lga=None)
    add_capture(graph, TENANT, "p2", "lead-2", "agent-1", ts(20), lat=ABUJA[0], lon=ABUJA[1], lga=None)
    add_capture(graph, TENANT, "p3", "lead-3", "agent-1", ts(10), lat=KANO[0], lon=KANO[1], lga=None)
    findings = GeoImpossibilityDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 2  # two impossible jumps
    assert all(f.severity == "high" for f in findings)


def test_silent_for_plausible_travel(client, graph, settings):
    # ~10 km in 60 minutes => 10 km/h
    add_capture(graph, TENANT, "p1", "lead-1", "agent-1", ts(70), lat=6.5244, lon=3.3792, lga=None)
    add_capture(graph, TENANT, "p2", "lead-2", "agent-1", ts(10), lat=6.6144, lon=3.3792, lga=None)
    assert GeoImpossibilityDetector().detect(client, TENANT, settings, NOW) == []


def test_silent_without_geo(client, graph, settings):
    add_capture(graph, TENANT, "p1", "lead-1", "agent-1", ts(10), lga="Ikeja")
    assert GeoImpossibilityDetector().detect(client, TENANT, settings, NOW) == []
