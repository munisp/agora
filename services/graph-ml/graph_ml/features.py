"""Feature vectors per person (SPEC-W29 §3 WS-A).

Pure functions over the extracted TenantGraph — numpy only, no I/O. All
features are computed within a single tenant's subgraph; nothing crosses
tenants here by construction (extraction is per-tenant).
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any

import numpy as np

from .extract import BookingRec, TenantGraph

# MESSAGED edge statuses that count as an engagement/response.
RESPONDED_STATUSES = frozenset({"responded", "replied", "clicked", "converted", "booked"})

# Cold-start defaults when a person/tenant has no booking history.
DEFAULT_INTERVAL_DAYS = 30.0
DEFAULT_RECENCY_DAYS = 365.0


def parse_time(value: Any) -> datetime | None:
    """Lenient timestamp parse: ISO-8601 str, datetime, or epoch seconds."""
    if value is None or value == "":
        return None
    if isinstance(value, datetime):
        return value if value.tzinfo else value.replace(tzinfo=timezone.utc)
    if isinstance(value, (int, float)):
        return datetime.fromtimestamp(value, tz=timezone.utc)
    if isinstance(value, str):
        text = value.strip()
        if text.endswith("Z"):
            text = text[:-1] + "+00:00"
        try:
            dt = datetime.fromisoformat(text)
        except ValueError:
            return None
        return dt if dt.tzinfo else dt.replace(tzinfo=timezone.utc)
    return None


def days_between(later: datetime, earlier: datetime | None) -> float | None:
    if earlier is None:
        return None
    return max(0.0, (later - earlier).total_seconds() / 86400.0)


@dataclass(frozen=True)
class PersonFeatures:
    person_id: str
    recency_days: float  # days since last booking (DEFAULT_RECENCY_DAYS if never)
    has_booked: bool
    booking_count: int
    booking_interval_mean: float  # DEFAULT_INTERVAL_DAYS when <2 bookings
    booking_interval_std: float
    monetary_total_cents: int
    distinct_offerings: int
    referral_out_degree: int
    referral_in_degree: int
    message_count: int
    message_response_rate: float  # 0.0 when never messaged
    message_to_booking_rate: float  # share of messages followed by a booking
    consent_purpose_count: int
    days_since_capture: float  # DEFAULT_RECENCY_DAYS when no Contact

    def vector(self) -> np.ndarray:
        """Fixed-order numeric vector (GNN input seam; heuristic uses fields)."""
        return np.array(
            [
                self.recency_days,
                float(self.booking_count),
                self.booking_interval_mean,
                self.booking_interval_std,
                float(self.monetary_total_cents),
                float(self.distinct_offerings),
                float(self.referral_out_degree),
                float(self.referral_in_degree),
                self.message_response_rate,
                float(self.consent_purpose_count),
                self.days_since_capture,
            ],
            dtype=np.float64,
        )


def _person_bookings(graph: TenantGraph) -> dict[str, list[BookingRec]]:
    by_person: dict[str, list[BookingRec]] = {}
    for b in graph.bookings:
        by_person.setdefault(b.person_id, []).append(b)
    for bookings in by_person.values():
        bookings.sort(key=lambda b: parse_time(b.at) or datetime.min.replace(tzinfo=timezone.utc))
    return by_person


def build_features(graph: TenantGraph, now: datetime | None = None) -> list[PersonFeatures]:
    """Compute the SPEC-W29 feature set for every Person of one tenant."""
    now = now or datetime.now(timezone.utc)
    bookings_by_person = _person_bookings(graph)

    referral_out: dict[str, int] = {}
    referral_in: dict[str, int] = {}
    for r in graph.referrals:
        referral_out[r.from_person_id] = referral_out.get(r.from_person_id, 0) + 1
        referral_in[r.to_person_id] = referral_in.get(r.to_person_id, 0) + 1

    msgs_by_person: dict[str, list[Any]] = {}
    for m in graph.messages:
        msgs_by_person.setdefault(m.person_id, []).append(m)

    consent_purposes: dict[str, set[str]] = {}
    for c in graph.consents:
        if c.purpose:
            consent_purposes.setdefault(c.person_id, set()).add(c.purpose)

    first_capture: dict[str, datetime] = {}
    for c in graph.contacts:
        ts = parse_time(c.captured_at)
        if ts is None:
            continue
        prev = first_capture.get(c.person_id)
        if prev is None or ts < prev:
            first_capture[c.person_id] = ts

    features: list[PersonFeatures] = []
    for person in graph.persons:
        pid = person.person_id
        bookings = bookings_by_person.get(pid, [])
        times = [t for t in (parse_time(b.at) for b in bookings) if t is not None]

        recency = days_between(now, times[-1]) if times else None
        intervals = [
            (times[i] - times[i - 1]).total_seconds() / 86400.0 for i in range(1, len(times))
        ]

        messages = msgs_by_person.get(pid, [])
        msg_count = len(messages)
        responded = sum(1 for m in messages if (m.status or "") in RESPONDED_STATUSES)
        # Past MESSAGED -> BOOKED conversion: share of messages followed by a
        # later booking from the same person.
        converted = 0
        for m in messages:
            m_at = parse_time(m.at)
            if m_at is not None and any(t > m_at for t in times):
                converted += 1

        captured = first_capture.get(pid)
        features.append(
            PersonFeatures(
                person_id=pid,
                recency_days=recency if recency is not None else DEFAULT_RECENCY_DAYS,
                has_booked=bool(bookings),
                booking_count=len(bookings),
                booking_interval_mean=(
                    float(np.mean(intervals)) if intervals else DEFAULT_INTERVAL_DAYS
                ),
                booking_interval_std=(float(np.std(intervals)) if intervals else 0.0),
                monetary_total_cents=sum(int(b.price_cents or 0) for b in bookings),
                distinct_offerings=len({b.offering_id for b in bookings}),
                referral_out_degree=referral_out.get(pid, 0),
                referral_in_degree=referral_in.get(pid, 0),
                message_count=msg_count,
                message_response_rate=(responded / msg_count) if msg_count else 0.0,
                message_to_booking_rate=(converted / msg_count) if msg_count else 0.0,
                consent_purpose_count=len(consent_purposes.get(pid, set())),
                days_since_capture=(
                    days_between(now, captured)
                    if captured is not None
                    else DEFAULT_RECENCY_DAYS
                ),
            )
        )
    return features


def tenant_median_interval(features: list[PersonFeatures]) -> float:
    """Tenant-typical booking interval (median of per-person means)."""
    intervals = [f.booking_interval_mean for f in features if f.booking_count >= 2]
    if not intervals:
        return DEFAULT_INTERVAL_DAYS
    return float(np.median(intervals))
