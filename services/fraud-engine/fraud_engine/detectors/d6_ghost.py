"""D6 ghost_booking (SPEC-W30 §3, fraud pattern F5).

>= GHOST_MIN (3) bookings created AND cancelled within GHOST_WINDOW_MIN (10)
by the same staff member in a single day => medium. (The secondary F5
signal — bookings with no contact/payment trail — is a v2 candidate; the
create->cancel burst rule is the v1 rule per SPEC.)
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

from ..config import Settings
from .base import Detector, Finding, days_ago, iso, parse_ts

CYPHER = """
// detector:d6_ghost_booking
MATCH (b:Booking {tenant_id:$tenant_id})
WHERE b.created_by IS NOT NULL AND b.status = 'cancelled'
  AND b.cancelled_at IS NOT NULL AND b.created_at >= $since
RETURN b.created_by AS staff, b.booking_id AS booking_id,
       b.created_at AS created_at, b.cancelled_at AS cancelled_at
"""


class GhostBookingDetector(Detector):
    name = "d6_ghost_booking"
    alert_type = "ghost_booking"

    def cypher(self, settings: Settings) -> str:
        return CYPHER

    def params(self, tenant_id: str, settings: Settings, now: datetime) -> dict[str, Any]:
        return {
            "tenant_id": tenant_id,
            "since": days_ago(now, settings.ghost_lookback_days),
        }

    def analyze(
        self, rows: list[dict[str, Any]], settings: Settings, now: datetime
    ) -> list[Finding]:
        window_seconds = settings.ghost_window_min * 60
        per_staff_day: dict[tuple[str, str], list[dict[str, Any]]] = {}
        for row in rows:
            staff = str(row.get("staff") or "")
            created = parse_ts(row.get("created_at"))
            cancelled = parse_ts(row.get("cancelled_at"))
            if not staff or created is None or cancelled is None:
                continue
            held_seconds = (cancelled - created).total_seconds()
            if held_seconds < 0 or held_seconds > window_seconds:
                continue  # not a create->cancel flash cycle
            day = created.date().isoformat()
            per_staff_day.setdefault((staff, day), []).append(
                {
                    "booking_id": str(row.get("booking_id") or ""),
                    "created_at": iso(created),
                    "cancelled_at": iso(cancelled),
                    "held_seconds": round(held_seconds, 1),
                }
            )

        findings: list[Finding] = []
        for (staff, day), cycles in sorted(per_staff_day.items()):
            if len(cycles) < settings.ghost_min:
                continue
            findings.append(
                Finding(
                    type=self.alert_type,
                    severity="medium",  # SPEC: medium
                    dedup_key=f"{staff}:{day}",
                    agent_id=staff,
                    evidence={
                        "detector": self.name,
                        "staff_id": staff,
                        "day": day,
                        "create_cancel_cycles": cycles,
                        "cycle_count": len(cycles),
                        "ghost_min": settings.ghost_min,
                        "ghost_window_minutes": settings.ghost_window_min,
                        "severity_rule": "medium: >= GHOST_MIN flash create/cancel cycles per staff-day",
                    },
                )
            )
        return findings
