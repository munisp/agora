"""D5 consent_backdating (SPEC-W30 §3, fraud pattern F4).

A CONSENTED edge whose granted_at is AFTER the first MESSAGED.at for the
same person+purpose: consent was forged/backdated to retroactively legalize
sends. Severity: HIGH ALWAYS (compliance-critical). This detector NEVER
auto-quarantines (SPEC-W30 §3 quarantine rule) — it routes to the
compliance queue via the alert.
"""

from __future__ import annotations

from typing import Any

from ..config import Settings
from .base import Detector, Finding

CYPHER = """
// detector:d5_consent_backdating
MATCH (p:Person {tenant_id:$tenant_id})-[c:CONSENTED]->(co:Consent)
WITH p, co, coalesce(c.granted_at, co.granted_at) AS granted_at
WHERE granted_at IS NOT NULL
MATCH (p)-[m:MESSAGED]->(:Campaign)
WHERE m.at < granted_at AND (m.purpose IS NULL OR m.purpose = co.purpose)
RETURN p.person_id AS person_id, co.purpose AS purpose, granted_at AS granted_at,
       min(m.at) AS first_messaged_at, count(m) AS messages_before_consent
"""


class ConsentBackdatingDetector(Detector):
    name = "d5_consent_backdating"
    alert_type = "consent_backdating"

    def cypher(self, settings: Settings) -> str:
        return CYPHER

    def analyze(self, rows: list[dict[str, Any]], settings: Settings, now) -> list[Finding]:
        findings: list[Finding] = []
        for row in rows:
            person_id = str(row.get("person_id") or "")
            purpose = str(row.get("purpose") or "unknown")
            if not person_id:
                continue
            findings.append(
                Finding(
                    type=self.alert_type,
                    severity="high",  # SPEC: high always (compliance-critical)
                    dedup_key=purpose,
                    person_id=person_id,
                    evidence={
                        "detector": self.name,
                        "person_id": person_id,
                        "purpose": purpose,
                        "granted_at": row.get("granted_at"),
                        "first_messaged_at": row.get("first_messaged_at"),
                        "messages_before_consent": row.get("messages_before_consent"),
                        "severity_rule": "high: compliance-critical, always",
                        "quarantine": "NEVER auto-quarantines; routes to compliance queue",
                    },
                )
            )
        return findings
