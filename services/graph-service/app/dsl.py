"""Segment DSL (SPEC-W28 §4 WS-B): declarative JSON filter -> compiled Cypher.

Example:
    {
      "name": "Lapsed Alimosho customers",
      "purpose": "marketing",
      "filter": {
        "has_consent": "marketing",          // defaults to `purpose`
        "last_booking_before": "2026-01-01", // most recent booking before this date
        "lga": "Alimosho",
        "not_messaged_since_days": 30
      }
    }

Consent gating is BY CONSTRUCTION: the compiled query always carries a
purpose-matching CONSENTED-edge predicate and a quarantine exclusion — a
segment can never be compiled without them (compliance gates 2 + 4).
"""

from __future__ import annotations

from datetime import date
from typing import Literal

from pydantic import BaseModel, Field, field_validator

_PURPOSE_RE = r"^[a-z][a-z0-9_\-]{0,63}$"


class SegmentFilter(BaseModel):
    """Declarative segment filter. All fields optional; omitted fields simply
    do not narrow the segment. Consent + quarantine predicates are mandatory
    and injected by the compiler regardless."""

    has_consent: str | None = Field(
        default=None,
        pattern=_PURPOSE_RE,
        description="consent purpose required; defaults to the segment's purpose",
    )
    last_booking_before: date | None = Field(
        default=None,
        description="persons whose most recent booking is before this ISO date",
    )
    lga: str | None = Field(default=None, max_length=200)
    not_messaged_since_days: int | None = Field(default=None, ge=0, le=3650)
    # Compliance gate 4: quarantined persons are query-visible elsewhere but
    # NEVER segment/audience eligible. This flag exists only to make the
    # handling explicit in saved DSL; true is rejected.
    include_quarantined: Literal[False] = False


class SegmentCreate(BaseModel):
    name: str = Field(min_length=1, max_length=200)
    purpose: str = Field(
        pattern=_PURPOSE_RE,
        description="outreach purpose this segment feeds (drives the consent gate)",
    )
    description: str | None = Field(default=None, max_length=1000)
    filter: SegmentFilter = Field(default_factory=SegmentFilter)

    @field_validator("name")
    @classmethod
    def _name_not_blank(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("name must not be blank")
        return v.strip()

    @property
    def consent_purpose(self) -> str:
        """The consent purpose the compiled query enforces."""
        return self.filter.has_consent or self.purpose
