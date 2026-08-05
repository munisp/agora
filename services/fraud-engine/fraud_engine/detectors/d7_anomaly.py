"""D7 gnn_anomaly (SPEC-W30 §3, fraud pattern F6).

graph-ml (W29) owns the anomaly math and writes Person.risk_score; this
detector only CONSUMES scores >= ANOMALY_ALERT_THRESHOLD (0.9) into Alert
nodes. Severity: >= ANOMALY_MEDIUM_THRESHOLD (0.97) => medium, else low.
Never auto-quarantines.
"""

from __future__ import annotations

from typing import Any

from ..config import Settings
from .base import Detector, Finding

CYPHER = """
// detector:d7_gnn_anomaly
MATCH (p:Person {tenant_id:$tenant_id})
WHERE p.risk_score IS NOT NULL AND p.risk_score >= $threshold
RETURN p.person_id AS person_id, p.risk_score AS risk_score
"""


class AnomalyDetector(Detector):
    name = "d7_gnn_anomaly"
    alert_type = "gnn_anomaly"

    def cypher(self, settings: Settings) -> str:
        return CYPHER

    def params(self, tenant_id: str, settings: Settings, now) -> dict[str, Any]:
        return {
            "tenant_id": tenant_id,
            "threshold": settings.anomaly_alert_threshold,
        }

    def analyze(self, rows: list[dict[str, Any]], settings: Settings, now) -> list[Finding]:
        findings: list[Finding] = []
        for row in rows:
            person_id = str(row.get("person_id") or "")
            if not person_id:
                continue
            try:
                score = float(row.get("risk_score"))
            except (TypeError, ValueError):
                continue
            severity = "medium" if score >= settings.anomaly_medium_threshold else "low"
            findings.append(
                Finding(
                    type=self.alert_type,
                    severity=severity,
                    dedup_key="risk-score",  # one open anomaly alert per person
                    person_id=person_id,
                    evidence={
                        "detector": self.name,
                        "person_id": person_id,
                        "risk_score": score,
                        "alert_threshold": settings.anomaly_alert_threshold,
                        "medium_threshold": settings.anomaly_medium_threshold,
                        "score_source": "w29 graph-ml sweep (Person.risk_score)",
                        "severity_rule": (
                            "medium: risk_score >= medium threshold"
                            if severity == "medium"
                            else "low: risk_score >= alert threshold"
                        ),
                    },
                )
            )
        return findings
