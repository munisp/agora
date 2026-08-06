"""D8 report_spam (SPEC-W32 §3 WS-D).

Civic-report abuse signals over the W32 Case projection:

  (a) velocity — one reporter (Person-[:REPORTED]->Case) opening more than
      CIVIC_REPORT_MAX_PER_DAY (5) cases in a single UTC day;
  (b) coordinated spam — more than CIVIC_COORD_CASE_THRESHOLD (3) OPEN cases
      of the same category within CIVIC_COORD_RADIUS_M (500m, haversine over
      the AT->Location geo) AND CIVIC_COORD_WINDOW_HOURS (24h) across
      DIFFERENT reporters.

Severity is ALWAYS medium and D8 NEVER auto-quarantines (``report_spam`` is
deliberately absent from quarantine.AUTO_QUARANTINE_TYPES): citizens are
never banned from reporting — alerts inform operator triage only
(SPEC-W32 §5 gate 6). Evidence is replayable (gate 3): case refs, counts,
centroid, thresholds.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

from ..config import Settings
from ..graph import GraphClient
from .base import Detector, Finding, days_ago, iso, parse_ts
from .d4_geo import haversine_km

VELOCITY_CYPHER = """
// detector:d8_report_velocity
MATCH (p:Person {tenant_id:$tenant_id})-[:REPORTED]->(cs:Case {tenant_id:$tenant_id})
WHERE cs.created_at >= $since
RETURN p.person_id AS person_id, cs.case_id AS case_ref, cs.created_at AS created_at
"""

COORDINATED_CYPHER = """
// detector:d8_coordinated_spam
MATCH (cs:Case {tenant_id:$tenant_id})-[:AT]->(l:Location)
WHERE cs.created_at >= $since AND l.lat IS NOT NULL AND l.lon IS NOT NULL
OPTIONAL MATCH (p:Person {tenant_id:$tenant_id})-[:REPORTED]->(cs)
RETURN cs.case_id AS case_ref, cs.category AS category, cs.status AS status,
       cs.created_at AS created_at, l.lat AS lat, l.lon AS lon,
       p.person_id AS reporter_id
"""

# Civic lifecycle (SPEC-W32 §2): resolved/closed cases no longer count as
# open for the coordinated signal.
CLOSED_STATUSES = frozenset({"resolved", "closed"})


class _UnionFind:
    def __init__(self, n: int) -> None:
        self.parent = list(range(n))

    def find(self, i: int) -> int:
        while self.parent[i] != i:
            self.parent[i] = self.parent[self.parent[i]]
            i = self.parent[i]
        return i

    def union(self, i: int, j: int) -> None:
        ri, rj = self.find(i), self.find(j)
        if ri != rj:
            self.parent[rj] = ri


class ReportSpamDetector(Detector):
    name = "d8_report_spam"
    alert_type = "report_spam"

    def cypher(self, settings: Settings) -> str:
        return VELOCITY_CYPHER

    def detect(
        self, client: GraphClient, tenant_id: str, settings: Settings, now: datetime
    ) -> list[Finding]:
        since = days_ago(now, settings.civic_report_lookback_days)
        vel_rows = self._run(
            client, VELOCITY_CYPHER, {"tenant_id": tenant_id, "since": since}
        )
        coord_rows = self._run(
            client, COORDINATED_CYPHER, {"tenant_id": tenant_id, "since": since}
        )
        return self._velocity_findings(vel_rows, settings, now) + self._coordinated_findings(
            coord_rows, settings, now
        )

    # -- signal (a): per-reporter daily velocity ----------------------------
    def _velocity_findings(
        self, rows: list[dict[str, Any]], settings: Settings, now: datetime
    ) -> list[Finding]:
        per_person_day: dict[tuple[str, str], list[str]] = {}
        for row in rows:
            person = str(row.get("person_id") or "")
            ts = parse_ts(row.get("created_at"))
            ref = str(row.get("case_ref") or "")
            if not person or ts is None or not ref:
                continue
            per_person_day.setdefault((person, ts.date().isoformat()), []).append(ref)

        findings: list[Finding] = []
        for (person, day), refs in sorted(per_person_day.items()):
            refs = sorted(set(refs))
            if len(refs) <= settings.civic_report_max_per_day:
                continue
            findings.append(
                Finding(
                    type=self.alert_type,
                    severity="medium",  # ALWAYS medium (SPEC-W32 §3 WS-D)
                    dedup_key=f"velocity:{person}:{day}",
                    person_id=person,
                    evidence={
                        "detector": self.name,
                        "signal": "reporter_velocity",
                        "person_id": person,
                        "day": day,
                        "case_refs": refs,
                        "case_count": len(refs),
                        "threshold": settings.civic_report_max_per_day,
                        "severity_rule": "medium (always): civic spam never escalates "
                        "severity and never auto-quarantines",
                    },
                )
            )
        return findings

    # -- signal (b): coordinated geo/category spam --------------------------
    def _coordinated_findings(
        self, rows: list[dict[str, Any]], settings: Settings, now: datetime
    ) -> list[Finding]:
        radius_km = settings.civic_coord_radius_m / 1000.0
        window_s = settings.civic_coord_window_hours * 3600.0
        per_category: dict[str, list[dict[str, Any]]] = {}
        for row in rows:
            status = str(row.get("status") or "").strip().lower()
            if status in CLOSED_STATUSES:
                continue  # open cases only
            ts = parse_ts(row.get("created_at"))
            category = str(row.get("category") or "").strip()
            if ts is None or not category:
                continue
            try:
                lat, lon = float(row["lat"]), float(row["lon"])
            except (TypeError, ValueError, KeyError):
                continue
            per_category.setdefault(category, []).append(
                {
                    "ref": str(row.get("case_ref") or ""),
                    "ts": ts,
                    "lat": lat,
                    "lon": lon,
                    "reporter": str(row.get("reporter_id") or "") or None,
                }
            )

        findings: list[Finding] = []
        for category, cases in sorted(per_category.items()):
            if len(cases) <= settings.civic_coord_case_threshold:
                continue
            cases.sort(key=lambda c: (c["ts"], c["ref"]))
            uf = _UnionFind(len(cases))
            for i in range(len(cases)):
                for j in range(i + 1, len(cases)):
                    a, b = cases[i], cases[j]
                    if abs((b["ts"] - a["ts"]).total_seconds()) > window_s:
                        continue
                    if haversine_km(a["lat"], a["lon"], b["lat"], b["lon"]) <= radius_km:
                        uf.union(i, j)
            clusters: dict[int, list[dict[str, Any]]] = {}
            for i, case in enumerate(cases):
                clusters.setdefault(uf.find(i), []).append(case)
            for cluster in clusters.values():
                if len(cluster) <= settings.civic_coord_case_threshold:
                    continue
                reporters = sorted({c["reporter"] for c in cluster if c["reporter"]})
                if len(reporters) < 2:
                    continue  # must span DIFFERENT reporters
                cluster.sort(key=lambda c: (c["ts"], c["ref"]))
                refs = [c["ref"] for c in cluster]
                centroid_lat = sum(c["lat"] for c in cluster) / len(cluster)
                centroid_lon = sum(c["lon"] for c in cluster) / len(cluster)
                findings.append(
                    Finding(
                        type=self.alert_type,
                        severity="medium",  # ALWAYS medium (SPEC-W32 §3 WS-D)
                        dedup_key=f"coordinated:{category}:{refs[0]}",
                        person_id=None,  # spans multiple reporters — no single subject
                        evidence={
                            "detector": self.name,
                            "signal": "coordinated_spam",
                            "category": category,
                            "case_refs": refs,
                            "case_count": len(refs),
                            "reporter_count": len(reporters),
                            "reporter_person_ids": reporters,
                            "centroid": {
                                "lat": round(centroid_lat, 6),
                                "lon": round(centroid_lon, 6),
                            },
                            "radius_m": settings.civic_coord_radius_m,
                            "window_hours": settings.civic_coord_window_hours,
                            "threshold": settings.civic_coord_case_threshold,
                            "first_seen": iso(cluster[0]["ts"]),
                            "last_seen": iso(cluster[-1]["ts"]),
                            "severity_rule": "medium (always): civic spam never escalates "
                            "severity and never auto-quarantines",
                        },
                    )
                )
        return findings

    def analyze(self, rows, settings, now):  # pragma: no cover - detect() overrides
        raise NotImplementedError("ReportSpamDetector.detect handles analysis")
