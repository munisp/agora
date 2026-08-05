"""Compiled query representation.

Every query the service executes is a CompiledQuery carrying BOTH:

* ``cypher`` + ``params`` — the canonical openCypher text run against
  FalkorDB (and shown to callers, e.g. the ask endpoint response);
* ``plan`` — the structured semantics the in-memory backend evaluates, so
  the pytest suite needs no live graph DB.

Tenant scoping: callers pass ``tenant_id`` separately; it is bound as the
``$tenant_id`` parameter (FalkorDB) and as the evaluator scope (in-memory).
The tenant id is NEVER interpolated into query text from user input.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import date, datetime, time, timezone
from typing import Any, Literal


def parse_instant(value: Any) -> datetime:
    """Parse an ISO date/datetime (or pass through a datetime) into an
    aware UTC datetime. Naive inputs are assumed UTC; dates become midnight.
    Raises ValueError/TypeError on garbage — callers map that to 4xx."""
    if isinstance(value, datetime):
        dt = value
    elif isinstance(value, date):
        dt = datetime.combine(value, time.min)
    else:
        text = str(value).strip()
        if text.endswith(("Z", "z")):
            text = text[:-1] + "+00:00"
        if "T" not in text and " " not in text:
            dt = datetime.combine(date.fromisoformat(text), time.min)
        else:
            dt = datetime.fromisoformat(text)
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc)


@dataclass(frozen=True)
class PersonFilterPlan:
    """Segment matching semantics for the in-memory backend."""

    consent_purpose: str
    last_booking_before: datetime | None = None
    lga: str | None = None
    not_messaged_since: datetime | None = None
    projection: Literal["ids", "count"] = "ids"


@dataclass(frozen=True)
class TemplatePlan:
    """Allowlisted read template + validated params (templates.py registry)."""

    name: str
    params: dict[str, Any] = field(default_factory=dict)


Plan = PersonFilterPlan | TemplatePlan


@dataclass(frozen=True)
class CompiledQuery:
    cypher: str
    params: dict[str, Any]
    plan: Plan
