"""D7 gnn_anomaly: consumes Person.risk_score (written by the W29 sweep)
>= ANOMALY_ALERT_THRESHOLD into Alert findings. low/medium severity only."""

from fraud_engine.detectors.d7_anomaly import AnomalyDetector

from conftest import NOW, TENANT


def test_fires_low_at_alert_threshold(client, graph, settings):
    graph.add_person(TENANT, "p1", risk_score=0.92)
    findings = AnomalyDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 1
    f = findings[0]
    assert f.type == "gnn_anomaly" and f.severity == "low"
    assert f.evidence["risk_score"] == 0.92
    assert f.evidence["score_source"].startswith("w29")


def test_medium_above_medium_threshold(client, graph, settings):
    graph.add_person(TENANT, "p1", risk_score=0.98)
    findings = AnomalyDetector().detect(client, TENANT, settings, NOW)
    assert findings[0].severity == "medium"


def test_silent_below_threshold(client, graph, settings):
    graph.add_person(TENANT, "p1", risk_score=0.5)
    graph.add_person(TENANT, "p2")  # no risk_score at all
    assert AnomalyDetector().detect(client, TENANT, settings, NOW) == []
