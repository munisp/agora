"""Alert MERGE dedup / idempotency (SPEC-W30 §5 gate 5): N sweeps -> <=1 open
alert per (type, person); evidence is replayable JSON (gate 3)."""

import json

from fraud_engine.alerts import upsert_alert
from fraud_engine.detectors import DetectionRunner
from fraud_engine.detectors.base import Finding

from conftest import NOW, TENANT, make_cycle


def _finding(person_id="p1", type="gnn_anomaly", severity="low", dedup="k"):
    return Finding(
        type=type, severity=severity, dedup_key=dedup, person_id=person_id,
        evidence={"detector": "test", "person_id": person_id},
    )


def test_alert_id_format():
    f = _finding(person_id="p1")
    assert f.alert_id(TENANT) == f"gnn_anomaly:{TENANT}:p1:k"
    agent_only = Finding(type="capture_velocity", severity="medium", dedup_key="w", agent_id="a1")
    assert agent_only.alert_id(TENANT) == f"capture_velocity:{TENANT}:agent:a1:w"


def test_upsert_merges_on_second_run(client, graph, settings):
    graph.add_person(TENANT, "p1")
    _, created1 = upsert_alert(client, TENANT, _finding(), NOW)
    _, created2 = upsert_alert(client, TENANT, _finding(), NOW)
    assert created1 is True and created2 is False
    assert len(graph.alerts) == 1


def test_runner_sweeps_never_duplicate_open_alerts(client, graph, publisher, settings):
    graph.add_tenant(TENANT)
    make_cycle(graph, TENANT, ["p1", "p2", "p3"])
    runner = DetectionRunner(client, publisher, settings)
    r1 = runner.run(now=NOW)
    assert r1.alerts_created == 3  # one per ring member
    assert len(publisher.published) == 3
    r2 = runner.run(now=NOW)
    assert r2.alerts_created == 0
    assert r2.alerts_deduped == 3
    assert len(publisher.published) == 3  # no re-spam of the topic
    assert len(graph.alerts) == 3


def test_evidence_is_replayable_json(client, graph, settings):
    graph.add_person(TENANT, "p1")
    record, _ = upsert_alert(client, TENANT, _finding(), NOW)
    stored = graph.alerts[record.alert_id]
    parsed = json.loads(stored["evidence"])  # must round-trip
    assert parsed["detector"] == "test" and parsed["person_id"] == "p1"
    # SPEC §2 schema fields present on the node
    for key in ("alert_id", "tenant_id", "type", "severity", "status", "person_id",
                "evidence", "created_at"):
        assert key in stored
    assert stored["status"] == "open"


def test_risk_flags_accumulate_on_person(client, graph, settings):
    graph.add_person(TENANT, "p1")
    upsert_alert(client, TENANT, _finding(type="gnn_anomaly"), NOW)
    upsert_alert(client, TENANT, _finding(type="sybil_cluster", dedup="x"), NOW)
    props = next(p for _, p in graph.persons(TENANT))
    assert sorted(props["risk_flags"]) == ["gnn_anomaly", "sybil_cluster"]
