"""Quarantine writer (SPEC-W30 §3 quarantine rule).

ONLY F1-high (referral_cycle), F2-high (sybil_cluster) and F3-high
(capture_velocity) set ``quarantine=true`` automatically. Everything else —
including D5 consent_backdating at high — awaits human confirmation in the
graph-service alerts queue. There is NO unquarantine path here by design:
resolution/un-quarantine lives in graph-service (SPEC-W30 §4 WS-C).

Property note: the W28 audience gate reads ``p.quarantine`` (docs/graph.md
§3.2), so that is the property set here — the same gate the SPEC's
"quarantined=true" wording refers to. Quarantined persons remain
query-visible but audience-ineligible; no auto-erasure ever.
"""

from __future__ import annotations

from datetime import datetime
from typing import TYPE_CHECKING

from .graph import GraphClient

if TYPE_CHECKING:
    from .detectors.base import Finding

# SPEC-W30 §3: "only F1-high, F2-high, F3-high set quarantined=true
# automatically". §0 maps F3 (agent lead fabrication) to BOTH the D3
# capture-velocity and D4 geo-impossibility signals, so F3-high here means
# {capture_velocity, geo_impossibility} at high. consent_backdating (F4) is
# NEVER in this set, even at high.
AUTO_QUARANTINE_TYPES = frozenset(
    {"referral_cycle", "sybil_cluster", "capture_velocity", "geo_impossibility"}
)

QUARANTINE_CYPHER = """
// write:quarantine
MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})
SET p.quarantine = true, p.quarantined_at = $at, p.quarantine_reason = $reason
RETURN p.person_id AS person_id
"""


def should_auto_quarantine(finding: "Finding") -> bool:
    return (
        finding.severity == "high"
        and finding.type in AUTO_QUARANTINE_TYPES
        and bool(finding.person_id)
    )


def apply_quarantine(
    client: GraphClient, tenant_id: str, findings: list["Finding"], now: datetime
) -> list[str]:
    """Idempotently quarantine persons per the SPEC rule. Returns person ids."""
    from .detectors.base import assert_tenant_bound, iso  # local: circular-safe

    quarantined: list[str] = []
    for finding in findings:
        if not should_auto_quarantine(finding):
            continue
        params = {
            "tenant_id": tenant_id,  # tenant-verified before write
            "person_id": finding.person_id,
            "at": iso(now),
            "reason": f"fraud-engine:{finding.type}:{finding.dedup_key}",
        }
        assert_tenant_bound(QUARANTINE_CYPHER, params)
        rows = client.query(QUARANTINE_CYPHER, params)
        if rows:
            quarantined.append(str(finding.person_id))
    return sorted(set(quarantined))
