"""Segment schema introspection (SPEC-W29 §3 WS-B).

GET /v1/graph/segments/schema — the filterable-field catalog the admin-web
segment builder renders from. W29 added the numeric score-filter block; this
endpoint is where the UI discovers those fields, their ops, and the DSL
shape (validation rules mirror dsl.ScoreFilter, whose violations map to 422).
"""

from __future__ import annotations

from typing import Any

from fastapi import APIRouter, Depends

from ..auth import current_tenant
from ..dsl import SCORE_FILTER_FIELDS, SCORE_FILTER_OPS

router = APIRouter(prefix="/v1/graph/segments", tags=["segments"])


@router.get("/schema")
async def segment_schema(tenant_id: str = Depends(current_tenant)) -> dict[str, Any]:
    return {
        "filter_fields": [
            {
                "field": "has_consent",
                "type": "string",
                "description": "consent purpose required; defaults to the segment's purpose",
            },
            {
                "field": "last_booking_before",
                "type": "date",
                "description": "persons whose most recent booking is before this ISO date",
            },
            {"field": "lga", "type": "string", "description": "capture-location LGA"},
            {
                "field": "not_messaged_since_days",
                "type": "integer",
                "range": [0, 3650],
                "description": "persons not messaged in the last N days",
            },
        ],
        "score_filter_fields": [
            {
                "field": field,
                "type": "float",
                "range": [0.0, 1.0],
                "ops": list(SCORE_FILTER_OPS),
            }
            for field in SCORE_FILTER_FIELDS
        ],
        "score_filters": {
            "description": (
                "Optional numeric score predicates; compiled to bound "
                "parameters ($sf0, ...). Persons without a stored score never "
                "match. Unknown field or op -> 422."
            ),
            "dsl_example": {
                "score_filters": [
                    {"field": "propensity_churn", "op": ">=", "value": 0.7},
                    {"field": "risk_score", "op": "between", "value": [0.2, 0.8]},
                ]
            },
        },
    }
