"""D1 referral_cycle: fires on rings, silent on acyclic referral trees."""

from fraud_engine.detectors.d1_referral import ReferralCycleDetector

from conftest import NOW, TENANT, add_booking_for, make_cycle, make_referral_chain


def test_fires_on_three_person_ring(client, graph, settings):
    make_cycle(graph, TENANT, ["p1", "p2", "p3"])
    findings = ReferralCycleDetector().detect(client, TENANT, settings, NOW)
    assert {f.person_id for f in findings} == {"p1", "p2", "p3"}
    assert all(f.type == "referral_cycle" for f in findings)
    # no reward-bearing conversion -> medium (SPEC D1 severity rule)
    assert all(f.severity == "medium" for f in findings)
    ev = findings[0].evidence
    assert ev["ring_size"] == 3 and sorted(ev["cycle"]) == ["p1", "p2", "p3"]
    assert ev["hops"] == 3


def test_high_severity_when_ring_has_conversion(client, graph, settings):
    make_cycle(graph, TENANT, ["p1", "p2", "p3"])
    add_booking_for(graph, TENANT, "p2")
    findings = ReferralCycleDetector().detect(client, TENANT, settings, NOW)
    assert findings and all(f.severity == "high" for f in findings)
    assert findings[0].evidence["reward_bearing_members"] == ["p2"]


def test_two_person_cycle_is_medium(client, graph, settings):
    make_cycle(graph, TENANT, ["p1", "p2"])
    add_booking_for(graph, TENANT, "p1")  # conversion but ring < 3 -> still medium
    findings = ReferralCycleDetector().detect(client, TENANT, settings, NOW)
    assert findings and all(f.severity == "medium" for f in findings)


def test_silent_on_acyclical_referral_tree(client, graph, settings):
    make_referral_chain(graph, TENANT, ["p1", "p2", "p3", "p4"])
    assert ReferralCycleDetector().detect(client, TENANT, settings, NOW) == []


def test_silent_on_empty_graph(client, settings):
    assert ReferralCycleDetector().detect(client, TENANT, settings, NOW) == []
