"""D2 sybil_cluster (SPEC-W30 §3, fraud pattern F2).

Persons captured by the same agent, at the same Location, within
SYBIL_WINDOW_MIN, whose name-embedding cosine >= SYBIL_SIM_THRESHOLD
(Ollama embeddings already stored on Person nodes by graph-sync).
Severity: medium; >= SYBIL_HIGH_SIZE in the cluster => high.
"""

from __future__ import annotations

import hashlib
import math
from datetime import datetime, timedelta
from typing import Any

from ..config import Settings
from .base import Detector, Finding, hours_ago, parse_ts

CYPHER = """
// detector:d2_sybil_cluster
MATCH (p:Person {tenant_id:$tenant_id})-[:HAS_CONTACT]->(c:Contact)-[:CAPTURED_AT]->(l:Location)
WHERE c.captured_by IS NOT NULL AND c.captured_at >= $since
RETURN p.person_id AS person_id, p.name AS name, p.name_embedding AS embedding,
       c.lead_id AS lead_id, c.captured_by AS agent, c.captured_at AS captured_at,
       l.lga AS lga, l.ward AS ward, l.lat AS lat, l.lon AS lon
"""


def cosine_similarity(a: list[float], b: list[float]) -> float:
    if not a or not b or len(a) != len(b):
        return 0.0
    dot = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(y * y for y in b))
    if na == 0.0 or nb == 0.0:
        return 0.0
    return dot / (na * nb)


class _UnionFind:
    def __init__(self, items: list[str]) -> None:
        self.parent = {x: x for x in items}

    def find(self, x: str) -> str:
        while self.parent[x] != x:
            self.parent[x] = self.parent[self.parent[x]]
            x = self.parent[x]
        return x

    def union(self, a: str, b: str) -> None:
        ra, rb = self.find(a), self.find(b)
        if ra != rb:
            self.parent[rb] = ra


def _location_key(row: dict[str, Any]) -> str:
    return "|".join(
        str(row.get(k) or "") for k in ("lga", "ward", "lat", "lon")
    )


class SybilClusterDetector(Detector):
    name = "d2_sybil_cluster"
    alert_type = "sybil_cluster"

    def cypher(self, settings: Settings) -> str:
        return CYPHER

    def params(self, tenant_id: str, settings: Settings, now: datetime) -> dict[str, Any]:
        return {
            "tenant_id": tenant_id,
            "since": hours_ago(now, settings.sybil_lookback_hours),
        }

    def analyze(
        self, rows: list[dict[str, Any]], settings: Settings, now: datetime
    ) -> list[Finding]:
        window = timedelta(minutes=settings.sybil_window_min)
        # Group candidates by (agent, location).
        groups: dict[tuple[str, str], list[dict[str, Any]]] = {}
        for row in rows:
            agent = str(row.get("agent") or "")
            if not agent:
                continue
            groups.setdefault((agent, _location_key(row)), []).append(row)

        findings: list[Finding] = []
        for (agent, location_key), members in sorted(groups.items()):
            members.sort(key=lambda r: str(r.get("captured_at") or ""))
            # Sliding-window buckets: any set of captures within the window.
            bucketed: dict[int, list[dict[str, Any]]] = {}
            for i, row in enumerate(members):
                t0 = parse_ts(row.get("captured_at"))
                if t0 is None:
                    continue
                bucket = [r for r in members[i:] if (parse_ts(r.get("captured_at")) or t0) - t0 <= window]
                bucketed[i] = bucket
            # Union similar-name persons within each window bucket.
            uf = _UnionFind([str(r["person_id"]) for r in members])
            pair_scores: dict[tuple[str, str], float] = {}
            for bucket in bucketed.values():
                for i in range(len(bucket)):
                    for j in range(i + 1, len(bucket)):
                        a, b = bucket[i], bucket[j]
                        pa, pb = str(a["person_id"]), str(b["person_id"])
                        sim = cosine_similarity(a.get("embedding") or [], b.get("embedding") or [])
                        if sim >= settings.sybil_sim_threshold:
                            uf.union(pa, pb)
                            pair_scores[(pa, pb)] = round(sim, 6)
            clusters: dict[str, list[str]] = {}
            for r in members:
                pid = str(r["person_id"])
                clusters.setdefault(uf.find(pid), []).append(pid)
            for cluster in clusters.values():
                cluster = sorted(set(cluster))
                if len(cluster) < 2:
                    continue
                severity = "high" if len(cluster) >= settings.sybil_high_size else "medium"
                dedup_key = hashlib.sha1(
                    f"{agent}|{location_key}|{','.join(cluster)}".encode()
                ).hexdigest()[:12]
                evidence = {
                    "detector": self.name,
                    "agent_id": agent,
                    "location_key": location_key,
                    "cluster": cluster,
                    "cluster_size": len(cluster),
                    "similarity_threshold": settings.sybil_sim_threshold,
                    "window_minutes": settings.sybil_window_min,
                    "pair_cosines": {
                        f"{a}~{b}": s for (a, b), s in sorted(pair_scores.items())
                        if a in cluster and b in cluster
                    },
                    "severity_rule": (
                        f"high: cluster size >= {settings.sybil_high_size}"
                        if severity == "high"
                        else "medium: 2..4 person similar-name burst"
                    ),
                }
                for pid in cluster:
                    findings.append(
                        Finding(
                            type=self.alert_type,
                            severity=severity,
                            dedup_key=dedup_key,
                            person_id=pid,
                            agent_id=agent,
                            evidence=evidence,
                        )
                    )
        return findings
