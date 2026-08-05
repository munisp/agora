"""D4 geo_impossibility (SPEC-W30 §3, fraud pattern F3).

Consecutive CAPTURED_AT points by the same agent whose haversine distance /
time delta implies travel faster than MAX_TRAVEL_KMH (120 km/h) => medium;
repeat offender (>= GEO_REPEAT_OFFENDER impossible jumps in the lookback
window) => high.
"""

from __future__ import annotations

import math
from datetime import datetime
from typing import Any

from ..config import Settings
from .base import Detector, Finding, hours_ago, iso, parse_ts

EARTH_RADIUS_KM = 6371.0088  # IUGG mean radius

CYPHER = """
// detector:d4_geo_impossibility
MATCH (c:Contact {tenant_id:$tenant_id})-[:CAPTURED_AT]->(l:Location)
WHERE c.captured_by IS NOT NULL AND c.captured_at >= $since
  AND l.lat IS NOT NULL AND l.lon IS NOT NULL
RETURN c.captured_by AS agent, c.lead_id AS lead_id, c.captured_at AS captured_at,
       l.lat AS lat, l.lon AS lon
"""

REPEAT_OFFENDER_JUMPS = 2  # >=2 impossible jumps in the window => high


def haversine_km(lat1: float, lon1: float, lat2: float, lon2: float) -> float:
    """Great-circle distance in kilometres."""
    p1, p2 = math.radians(lat1), math.radians(lat2)
    dphi = math.radians(lat2 - lat1)
    dlmb = math.radians(lon2 - lon1)
    a = math.sin(dphi / 2) ** 2 + math.cos(p1) * math.cos(p2) * math.sin(dlmb / 2) ** 2
    return 2 * EARTH_RADIUS_KM * math.asin(math.sqrt(a))


class GeoImpossibilityDetector(Detector):
    name = "d4_geo_impossibility"
    alert_type = "geo_impossibility"

    def cypher(self, settings: Settings) -> str:
        return CYPHER

    def params(self, tenant_id: str, settings: Settings, now: datetime) -> dict[str, Any]:
        return {
            "tenant_id": tenant_id,
            "since": hours_ago(now, settings.geo_lookback_hours),
        }

    def analyze(
        self, rows: list[dict[str, Any]], settings: Settings, now: datetime
    ) -> list[Finding]:
        per_agent: dict[str, list[dict[str, Any]]] = {}
        for row in rows:
            agent = str(row.get("agent") or "")
            ts = parse_ts(row.get("captured_at"))
            if not agent or ts is None:
                continue
            try:
                lat, lon = float(row["lat"]), float(row["lon"])
            except (TypeError, ValueError):
                continue
            per_agent.setdefault(agent, []).append(
                {"ts": ts, "lat": lat, "lon": lon, "lead_id": str(row.get("lead_id") or "")}
            )

        findings: list[Finding] = []
        for agent, points in sorted(per_agent.items()):
            points.sort(key=lambda p: p["ts"])
            jumps: list[dict[str, Any]] = []
            for a, b in zip(points, points[1:]):
                dt_hours = (b["ts"] - a["ts"]).total_seconds() / 3600.0
                if dt_hours <= 0:
                    continue
                dist = haversine_km(a["lat"], a["lon"], b["lat"], b["lon"])
                speed = dist / dt_hours
                if speed > settings.max_travel_kmh:
                    jumps.append(
                        {
                            "from_lead": a["lead_id"],
                            "to_lead": b["lead_id"],
                            "from": {"lat": a["lat"], "lon": a["lon"], "at": iso(a["ts"])},
                            "to": {"lat": b["lat"], "lon": b["lon"], "at": iso(b["ts"])},
                            "distance_km": round(dist, 3),
                            "elapsed_minutes": round(dt_hours * 60, 2),
                            "implied_speed_kmh": round(speed, 1),
                        }
                    )
            if not jumps:
                continue
            severity = "high" if len(jumps) >= REPEAT_OFFENDER_JUMPS else "medium"
            for jump in jumps:
                findings.append(
                    Finding(
                        type=self.alert_type,
                        severity=severity,
                        dedup_key=f"{agent}:{jump['from_lead']}>{jump['to_lead']}",
                        agent_id=agent,
                        evidence={
                            "detector": self.name,
                            "agent_id": agent,
                            "jump": jump,
                            "impossible_jumps_in_window": len(jumps),
                            "max_travel_kmh": settings.max_travel_kmh,
                            "severity_rule": (
                                f"high: repeat offender (>= {REPEAT_OFFENDER_JUMPS} impossible jumps)"
                                if severity == "high"
                                else "medium: single impossible jump"
                            ),
                        },
                    )
                )
        return findings
