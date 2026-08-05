"""Quarantine matrix (SPEC-W30 §3): ONLY F1/F2/F3 at HIGH severity
auto-quarantine. D5 NEVER auto-quarantines, even at high. No unquarantine
path exists in fraud-engine."""

import pytest

from fraud_engine.detectors.base import Finding
from fraud_engine.quarantine import AUTO_QUARANTINE_TYPES, apply_quarantine, should_auto_quarantine

from conftest import NOW, TENANT

ALL_TYPES = [
    "referral_cycle", "sybil_cluster", "capture_velocity",
    "geo_impossibility", "consent_backdating", "ghost_booking", "gnn_anomaly",
]

# (type, severity) -> auto-quarantine?
MATRIX = {
    ("referral_cycle", "high"): True,
    ("sybil_cluster", "high"): True,
    ("capture_velocity", "high"): True,
    ("geo_impossibility", "high"): True,    # F3 per §0 taxonomy (agent lead fabrication)
    ("referral_cycle", "medium"): False,
    ("sybil_cluster", "medium"): False,
    ("capture_velocity", "medium"): False,
    ("geo_impossibility", "medium"): False,
    ("consent_backdating", "high"): False,  # NEVER (compliance queue instead)
    ("ghost_booking", "medium"): False,
    ("gnn_anomaly", "medium"): False,
    ("gnn_anomaly", "low"): False,
}


@pytest.mark.parametrize(("atype", "severity"), list(MATRIX))
def test_quarantine_matrix(atype, severity, client, graph):
    graph.add_person(TENANT, "p1")
    finding = Finding(type=atype, severity=severity, dedup_key="k", person_id="p1")
    assert should_auto_quarantine(finding) is MATRIX[(atype, severity)]
    quarantined = apply_quarantine(client, TENANT, [finding], NOW)
    props = next(p for _, p in graph.persons(TENANT))
    if MATRIX[(atype, severity)]:
        assert quarantined == ["p1"]
        assert props.get("quarantine") is True
    else:
        assert quarantined == []
        assert props.get("quarantine") is not True


def test_d5_high_never_quarantines_end_to_end(client, graph, publisher, settings):
    from fraud_engine.detectors import DetectionRunner

    from conftest import ts
    pkey = graph.add_person(TENANT, "p1")
    ckey = graph.add_consent(TENANT, "marketing", granted_at=ts(30))
    graph.add_edge(pkey, "CONSENTED", ckey, purpose="marketing", granted_at=ts(30))
    camp = graph.add_campaign(TENANT, "camp-1")
    graph.add_edge(pkey, "MESSAGED", camp, campaign_id="camp-1", at=ts(120), purpose="marketing")

    runner = DetectionRunner(client, publisher, settings)
    report = runner.run(tenant_id=TENANT, detector="d5_consent_backdating", now=NOW)
    assert report.alerts_created == 1  # high alert created...
    assert report.quarantined == []    # ...but NO quarantine
    props = next(p for _, p in graph.persons(TENANT))
    assert props.get("quarantine") is not True


def test_no_unquarantine_path_in_package():
    """fraud-engine exposes no unquarantine path (SPEC-W30 §3: that lives in
    graph-service). Guard against regressions adding one."""
    import fraud_engine.quarantine as q

    assert not hasattr(q, "unquarantine")
    assert not hasattr(q, "clear_quarantine")
    assert "consent_backdating" not in AUTO_QUARANTINE_TYPES
