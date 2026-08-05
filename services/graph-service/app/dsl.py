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

from pydantic import BaseModel, Field, field_validator, model_validator

_PURPOSE_RE = r"^[a-z][a-z0-9_\-]{0,63}$"

# SPEC-W29 §3 WS-B: numeric score filters over the predictive layer (W29)
# and the fraud risk head (W30). Unknown fields are rejected at validation
# time (-> 422 at the API layer); values are always bound as parameters by
# the compiler, never interpolated.
SCORE_FILTER_FIELDS: tuple[str, ...] = (
    "propensity_churn",
    "propensity_convert",
    "propensity_turnout",
    "risk_score",
)
SCORE_FILTER_OPS: tuple[str, ...] = (">=", "<=", "between")


class ScoreFilter(BaseModel):
    """One numeric score predicate, e.g.
    ``{"field": "propensity_churn", "op": ">=", "value": 0.7}`` or
    ``{"field": "risk_score", "op": "between", "value": [0.2, 0.8]}``."""

    field: Literal[
        "propensity_churn", "propensity_convert", "propensity_turnout", "risk_score"
    ]
    op: Literal[">=", "<=", "between"]
    value: float | list[float]

    @model_validator(mode="after")
    def _value_matches_op(self) -> "ScoreFilter":
        if self.op == "between":
            if (
                not isinstance(self.value, list)
                or len(self.value) != 2
                or any(isinstance(v, bool) or not isinstance(v, (int, float)) for v in self.value)
            ):
                raise ValueError("op 'between' requires value [lo, hi] (two numbers)")
            lo, hi = float(self.value[0]), float(self.value[1])
            if lo > hi:
                raise ValueError("op 'between' requires lo <= hi")
            self.value = [lo, hi]
        else:
            if isinstance(self.value, bool) or not isinstance(self.value, (int, float)):
                raise ValueError(f"op {self.op!r} requires a single numeric value")
            self.value = float(self.value)
        return self


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
    # SPEC-W29 §3 WS-B: optional numeric score predicates. Persons without a
    # stored score never match a score filter (null comparisons are false).
    score_filters: list[ScoreFilter] = Field(default_factory=list, max_length=8)
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
