"""D3 capture_velocity (SPEC-W30 §3, fraud pattern F3).

An agent capturing more than CAPTURE_VELOCITY_MAX leads in any rolling
CAPTURE_WINDOW_MIN (60) window => medium; sustained over
CAPTURE_SUSTAINED_WINDOWS (3) consecutive windows => high.
"""

from __future__ import annotations

from datetime import datetime, timedelta
from typing import Any

from ..config import Settings
from .base import Detector, Finding, hours_ago, iso, parse_ts

CYPHER = """
// detector:d3_capture_velocity
MATCH (c:Contact {tenant_id:$tenant_id})
WHERE c.captured_by IS NOT NULL AND c.captured_at >= $since
RETURN c.captured_by AS agent, c.lead_id AS lead_id, c.captured_at AS captured_at
"""


class CaptureVelocityDetector(Detector):
    name = "d3_capture_velocity"
    alert_type = "capture_velocity"

    def cypher(self, settings: Settings) -> str:
        return CYPHER

    def params(self, tenant_id: str, settings: Settings, now: datetime) -> dict[str, Any]:
        return {
            "tenant_id": tenant_id,
            "since": hours_ago(now, settings.capture_lookback_hours),
        }

    def analyze(
        self, rows: list[dict[str, Any]], settings: Settings, now: datetime
    ) -> list[Finding]:
        window = timedelta(minutes=settings.capture_window_min)
        per_agent: dict[str, list[tuple[datetime, str]]] = {}
        for row in rows:
            agent = str(row.get("agent") or "")
            ts = parse_ts(row.get("captured_at"))
            if not agent or ts is None:
                continue
            per_agent.setdefault(agent, []).append((ts, str(row.get("lead_id") or "")))

        findings: list[Finding] = []
        for agent, events in sorted(per_agent.items()):
            events.sort(key=lambda e: e[0])
            times = [t for t, _ in events]
            # Rolling-window max count: for each i, events in [t_i, t_i+window).
            best_start: datetime | None = None
            best_count = 0
            j = 0
            for i, t in enumerate(times):
                while j < len(times) and times[j] - t <= window:
                    j += 1
                count = j - i
                if count > best_count:
                    best_count = count
                    best_start = t
            if best_count <= settings.capture_velocity_max or best_start is None:
                continue

            # Sustained rule: >= N consecutive aligned slots each over the max.
            slot = settings.capture_window_min * 60
            epoch0 = times[0]
            slot_counts: dict[int, int] = {}
            for t in times:
                idx = int((t - epoch0).total_seconds() // slot)
                slot_counts[idx] = slot_counts.get(idx, 0) + 1
            tripped = {i for i, c in slot_counts.items() if c > settings.capture_velocity_max}
            sustained = any(
                all((base + k) in tripped for k in range(settings.capture_sustained_windows))
                for base in tripped
            )
            severity = "high" if sustained else "medium"
            dedup_key = f"{agent}:{best_start.date().isoformat()}T{best_start.hour:02d}"
            findings.append(
                Finding(
                    type=self.alert_type,
                    severity=severity,
                    dedup_key=dedup_key,
                    agent_id=agent,
                    evidence={
                        "detector": self.name,
                        "agent_id": agent,
                        "max_captures_in_window": best_count,
                        "window_minutes": settings.capture_window_min,
                        "threshold": settings.capture_velocity_max,
                        "window_start": iso(best_start),
                        "sustained_windows": sustained,
                        "tripped_slot_counts": {
                            str(i): slot_counts[i] for i in sorted(tripped)
                        },
                        "severity_rule": (
                            f"high: over-threshold in {settings.capture_sustained_windows} "
                            "consecutive windows"
                            if severity == "high"
                            else "medium: single rolling window over threshold"
                        ),
                    },
                )
            )
        return findings
