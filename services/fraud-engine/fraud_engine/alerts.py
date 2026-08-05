"""Alert node writes (SPEC-W30 §2 schema, §3 dedup).

Alert nodes are written DIRECTLY to FalkorDB by fraud-engine (tenant-verified
before write — every statement binds $tenant_id and is checked by
``assert_tenant_bound``). Dedup/idempotency: MERGE on
``alert_id = type:tenant:person:dedup_key``; sweep re-runs never duplicate
open alerts (SPEC-W30 §5 gate 5). ``evidence`` is a JSON string so auditors
can replay exactly why the alert fired (gate 3).
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from datetime import datetime
from typing import TYPE_CHECKING, Any

from .graph import GraphClient

if TYPE_CHECKING:  # avoid a circular import at runtime
    from .detectors.base import Finding

ALERT_MERGE_CYPHER = """
// write:alert_merge
MERGE (a:Alert {alert_id: $alert_id})
ON CREATE SET a.tenant_id = $tenant_id, a.type = $type, a.severity = $severity,
              a.status = 'open', a.person_id = $person_id, a.agent_id = $agent_id,
              a.evidence = $evidence, a.created_at = $created_at
ON MATCH SET  a.evidence = CASE WHEN a.status = 'open' THEN $evidence ELSE a.evidence END,
              a.severity = CASE WHEN a.status = 'open' THEN $severity ELSE a.severity END
RETURN a.created_at = $created_at AS created, a.status AS status
"""

FLAGGED_EDGE_CYPHER = """
// write:alert_flag_person
MATCH (a:Alert {alert_id: $alert_id}), (p:Person {tenant_id: $tenant_id, person_id: $person_id})
MERGE (a)-[:FLAGGED]->(p)
"""

RISK_FLAG_CYPHER = """
// write:person_risk_flag
MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})
SET p.risk_flags = CASE
    WHEN p.risk_flags IS NULL THEN [$flag]
    WHEN NOT $flag IN p.risk_flags THEN p.risk_flags + $flag
    ELSE p.risk_flags END
RETURN p.person_id AS person_id
"""


@dataclass(frozen=True)
class AlertRecord:
    alert_id: str
    tenant_id: str
    type: str
    severity: str
    status: str
    person_id: str | None
    agent_id: str | None
    evidence: dict[str, Any]
    created_at: str

    def event_data(self) -> dict[str, Any]:
        """CloudEvent data payload per SPEC-W30 §3."""
        data: dict[str, Any] = {
            "alert_id": self.alert_id,
            "type": self.type,
            "severity": self.severity,
        }
        if self.person_id:
            data["person_id"] = self.person_id
        if self.agent_id:
            data["agent_id"] = self.agent_id
        return data


def upsert_alert(
    client: GraphClient, tenant_id: str, finding: "Finding", now: datetime
) -> tuple[AlertRecord, bool]:
    """MERGE the alert node. Returns (record, created). Idempotent."""
    from .detectors.base import assert_tenant_bound, iso  # local: circular-safe

    alert_id = finding.alert_id(tenant_id)
    created_at = iso(now)
    evidence_json = json.dumps(finding.evidence, sort_keys=True, default=str)
    params = {
        "tenant_id": tenant_id,  # tenant-verified before write (asserted below)
        "alert_id": alert_id,
        "type": finding.type,
        "severity": finding.severity,
        "person_id": finding.person_id,
        "agent_id": finding.agent_id,
        "evidence": evidence_json,
        "created_at": created_at,
    }
    assert_tenant_bound(ALERT_MERGE_CYPHER, params)
    rows = client.query(ALERT_MERGE_CYPHER, params)
    created = bool(rows and rows[0].get("created"))
    status = str(rows[0].get("status")) if rows else "open"

    if finding.person_id:
        edge_params = {
            "tenant_id": tenant_id,
            "alert_id": alert_id,
            "person_id": finding.person_id,
        }
        assert_tenant_bound(FLAGGED_EDGE_CYPHER, edge_params)
        client.query(FLAGGED_EDGE_CYPHER, edge_params)

        flag_params = {
            "tenant_id": tenant_id,
            "person_id": finding.person_id,
            "flag": finding.type,
        }
        assert_tenant_bound(RISK_FLAG_CYPHER, flag_params)
        client.query(RISK_FLAG_CYPHER, flag_params)

    record = AlertRecord(
        alert_id=alert_id,
        tenant_id=tenant_id,
        type=finding.type,
        severity=finding.severity,
        status=status,
        person_id=finding.person_id,
        agent_id=finding.agent_id,
        evidence=finding.evidence,
        created_at=created_at,
    )
    return record, created
