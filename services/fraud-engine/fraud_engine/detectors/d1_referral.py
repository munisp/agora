"""D1 referral_cycle (SPEC-W30 §3, fraud pattern F1).

Cycles in the REFERRED subgraph of length 2..4 (referral rings harvesting
rewards). Severity: high if the ring has >=3 persons AND any member has a
reward-bearing conversion (a non-cancelled booking), else medium.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

from ..config import Settings
from ..graph import GraphClient
from .base import Detector, Finding

CYPHER_TEMPLATE = """
// detector:d1_referral_cycle
MATCH path=(a:Person {{tenant_id:$tenant_id}})-[:REFERRED*{min_hops}..{max_hops}]->(a)
RETURN [n IN nodes(path) | n.person_id] AS cycle, length(path) AS hops
"""

CONVERSIONS_CYPHER = """
// detector:d1_conversions
MATCH (p:Person {tenant_id:$tenant_id})-[:BOOKED]->(b:Booking)
WHERE p.person_id IN $person_ids AND b.status <> 'cancelled'
RETURN DISTINCT p.person_id AS person_id
"""


def _canonical_cycle(cycle: list[str]) -> tuple[str, ...]:
    """Rotation-invariant canonical form so FalkorDB returning the same ring
    from each starting node dedups to one finding set."""
    if not cycle:
        return ()
    rotations = [tuple(cycle[i:] + cycle[:i]) for i in range(len(cycle))]
    return min(rotations)


class ReferralCycleDetector(Detector):
    name = "d1_referral_cycle"
    alert_type = "referral_cycle"

    def cypher(self, settings: Settings) -> str:
        # Path-length bounds must be literal in Cypher; they come from
        # int-typed settings only (never request input).
        return CYPHER_TEMPLATE.format(
            min_hops=settings.referral_cycle_min_hops,
            max_hops=settings.referral_cycle_max_hops,
        )

    def detect(
        self, client: GraphClient, tenant_id: str, settings: Settings, now: datetime
    ) -> list[Finding]:
        rows = self._run(client, self.cypher(settings), self.params(tenant_id, settings, now))
        # Dedup rotations of the same ring.
        cycles: dict[tuple[str, ...], int] = {}
        for row in rows:
            members = [str(m) for m in (row.get("cycle") or [])]
            hops = int(row.get("hops") or len(members))
            # nodes(path) closes the ring by repeating the start node —
            # drop it before canonical rotation.
            if len(members) > 1 and members[0] == members[-1]:
                members = members[:-1]
            canon = _canonical_cycle(members)
            if canon and canon not in cycles:
                cycles[canon] = hops

        findings: list[Finding] = []
        for canon, hops in cycles.items():
            members = sorted(set(canon))
            # Reward-bearing conversion check (severity rule, SPEC D1).
            conv_rows = self._run(
                client,
                CONVERSIONS_CYPHER,
                {"tenant_id": tenant_id, "person_ids": members},
            )
            converters = sorted({str(r["person_id"]) for r in conv_rows})
            severity = "high" if (len(members) >= 3 and converters) else "medium"
            dedup_key = ">".join(canon)
            evidence = {
                "detector": self.name,
                "cycle": list(canon),
                "ring_members": members,
                "ring_size": len(members),
                "hops": hops,
                "reward_bearing_members": converters,
                "severity_rule": (
                    "high: ring >= 3 persons with reward-bearing conversion"
                    if severity == "high"
                    else "medium: cycle without (size>=3 AND conversion)"
                ),
            }
            for member in members:
                findings.append(
                    Finding(
                        type=self.alert_type,
                        severity=severity,
                        dedup_key=dedup_key,
                        person_id=member,
                        evidence=evidence,
                    )
                )
        return findings

    def analyze(self, rows, settings, now):  # pragma: no cover - detect() overrides
        raise NotImplementedError("ReferralCycleDetector.detect handles analysis")
