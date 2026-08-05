"""D5 consent_backdating: CONSENTED.granted_at AFTER first MESSAGED.at.
High always; never auto-quarantines (the quarantine matrix test covers the
wiring; here we assert the detector output)."""

from fraud_engine.detectors.d5_consent import ConsentBackdatingDetector

from conftest import NOW, TENANT, ts


def _person_with_consent_and_message(graph, person_id, messaged_at, granted_at, purpose="marketing"):
    pkey = graph.add_person(TENANT, person_id)
    ckey = graph.add_consent(TENANT, purpose, granted_at=granted_at)
    graph.add_edge(pkey, "CONSENTED", ckey, purpose=purpose, granted_at=granted_at)
    camp = graph.add_campaign(TENANT, f"camp-{person_id}")
    graph.add_edge(pkey, "MESSAGED", camp, campaign_id=f"camp-{person_id}", at=messaged_at,
                   status="sent", purpose=purpose)


def test_fires_high_when_messaged_before_consent(client, graph, settings):
    _person_with_consent_and_message(graph, "p1", messaged_at=ts(120), granted_at=ts(30))
    findings = ConsentBackdatingDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 1
    f = findings[0]
    assert f.type == "consent_backdating"
    assert f.severity == "high"  # SPEC: high always (compliance-critical)
    assert f.person_id == "p1" and f.dedup_key == "marketing"
    assert f.evidence["messages_before_consent"] == 1
    assert "NEVER" in f.evidence["quarantine"]


def test_silent_when_consent_precedes_message(client, graph, settings):
    _person_with_consent_and_message(graph, "p1", messaged_at=ts(30), granted_at=ts(120))
    assert ConsentBackdatingDetector().detect(client, TENANT, settings, NOW) == []


def test_silent_when_purpose_differs(client, graph, settings):
    pkey = graph.add_person(TENANT, "p1")
    ckey = graph.add_consent(TENANT, "marketing", granted_at=ts(30))
    graph.add_edge(pkey, "CONSENTED", ckey, purpose="marketing", granted_at=ts(30))
    camp = graph.add_campaign(TENANT, "camp-1")
    # messaged before consent but for a DIFFERENT purpose -> not backdating
    graph.add_edge(pkey, "MESSAGED", camp, campaign_id="camp-1", at=ts(120),
                   status="sent", purpose="transactional")
    assert ConsentBackdatingDetector().detect(client, TENANT, settings, NOW) == []
