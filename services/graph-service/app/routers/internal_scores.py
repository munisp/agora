"""Internal predictive write-back API (SPEC-W29 §3 WS-B).

  POST /v1/graph/internal/scores            Person.propensity_* / risk_score
  POST /v1/graph/internal/recommendations   (Person)-[:RECOMMENDED_FOR]->(Offering)

These are the ONLY write paths graph-ml uses (single write path gate).
Auth is X-Internal-Token == INTERNAL_TOKEN (constant-time compare) — JWTs
are never accepted here (see routers.require_internal_token).

Every item carries tenant_id and it must equal the envelope tenant_id;
before any MERGE the backend verifies the target Person/Offering is not
owned by another tenant (cross-tenant write-back -> 422). MERGE semantics
keep the latest score (overwrite in place). Each accepted write increments
``scores_written_total{tenant}``.
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

import structlog
from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, ConfigDict, Field, model_validator

from .. import metrics
from ..writes import (
    SCORE_WRITE_FIELDS,
    CrossTenantWriteError,
    WriteTargetMissing,
    compile_recommendation_write,
    compile_score_write,
)
from . import InternalAuth, get_deps, run_write

log = structlog.get_logger("graph-service.internal_scores")

router = APIRouter(prefix="/v1/graph/internal", tags=["internal"])

_IDENT = r"^[A-Za-z0-9_\-]{1,100}$"


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


class ScoreItem(BaseModel):
    model_config = ConfigDict(protected_namespaces=())

    tenant_id: str = Field(min_length=1, max_length=100)
    person_id: str = Field(pattern=_IDENT)
    propensity_churn: float | None = Field(default=None, ge=0.0, le=1.0)
    propensity_convert: float | None = Field(default=None, ge=0.0, le=1.0)
    propensity_turnout: float | None = Field(default=None, ge=0.0, le=1.0)
    risk_score: float | None = Field(default=None, ge=0.0, le=1.0)
    model_version: str = Field(default="heuristic-v1", min_length=1, max_length=100)
    scored_at: str | None = Field(default=None, max_length=64)

    @model_validator(mode="after")
    def _at_least_one_score(self) -> "ScoreItem":
        if not any(getattr(self, f) is not None for f in SCORE_WRITE_FIELDS):
            raise ValueError("at least one score field is required")
        return self

    def scores(self) -> dict[str, float]:
        return {
            f: float(getattr(self, f))
            for f in SCORE_WRITE_FIELDS
            if getattr(self, f) is not None
        }


class ScoresRequest(BaseModel):
    tenant_id: str = Field(min_length=1, max_length=100)
    scores: list[ScoreItem] = Field(min_length=1, max_length=5000)


class RecommendationItem(BaseModel):
    model_config = ConfigDict(protected_namespaces=())

    tenant_id: str = Field(min_length=1, max_length=100)
    person_id: str = Field(pattern=_IDENT)
    offering_id: str = Field(pattern=_IDENT)
    score: float = Field(ge=0.0, le=1.0)
    rank: int = Field(ge=1, le=1000)
    reason: str = Field(default="", max_length=200)
    model_version: str = Field(default="heuristic-v1", min_length=1, max_length=100)
    scored_at: str | None = Field(default=None, max_length=64)


class RecommendationsRequest(BaseModel):
    tenant_id: str = Field(min_length=1, max_length=100)
    recommendations: list[RecommendationItem] = Field(min_length=1, max_length=5000)


def _check_item_tenant(envelope_tenant: str, item: Any) -> None:
    """Per-item tenant_id validation: an item whose tenant disagrees with
    the envelope is a cross-tenant write-back attempt -> 422."""
    if item.tenant_id != envelope_tenant:
        raise HTTPException(
            status_code=422,
            detail=(
                f"item tenant_id {item.tenant_id!r} does not match envelope "
                f"tenant {envelope_tenant!r}; cross-tenant write-back rejected"
            ),
        )


@router.post("/scores", dependencies=[InternalAuth])
async def write_scores(
    payload: ScoresRequest,
    deps: Any = Depends(get_deps),
) -> dict[str, Any]:
    written = 0
    skipped_unknown: list[str] = []
    for item in payload.scores:
        _check_item_tenant(payload.tenant_id, item)
        write = compile_score_write(
            person_id=item.person_id,
            scores=item.scores(),
            model_version=item.model_version,
            scored_at=item.scored_at or _now_iso(),
        )
        try:
            await run_write(deps, "internal_scores", write, payload.tenant_id)
        except CrossTenantWriteError as exc:
            log.warning(
                "scores.cross_tenant_rejected",
                tenant=payload.tenant_id,
                person_id=item.person_id,
            )
            raise HTTPException(status_code=422, detail=str(exc)) from exc
        except WriteTargetMissing:
            # MATCH-not-MERGE: unknown persons are skipped + counted, never
            # created as bare stub nodes (verification gate WARN #4).
            skipped_unknown.append(item.person_id)
            continue
        written += 1
    metrics.scores_written.labels(tenant=payload.tenant_id).inc(written)
    return {
        "tenant_id": payload.tenant_id,
        "written": written,
        "skipped_unknown": len(skipped_unknown),
        "skipped_unknown_ids": skipped_unknown,
    }


@router.post("/recommendations", dependencies=[InternalAuth])
async def write_recommendations(
    payload: RecommendationsRequest,
    deps: Any = Depends(get_deps),
) -> dict[str, Any]:
    written = 0
    skipped: list[dict[str, str]] = []
    for item in payload.recommendations:
        _check_item_tenant(payload.tenant_id, item)
        write = compile_recommendation_write(
            person_id=item.person_id,
            offering_id=item.offering_id,
            score=item.score,
            rank=item.rank,
            reason=item.reason,
            model_version=item.model_version,
            scored_at=item.scored_at or _now_iso(),
        )
        try:
            await run_write(deps, "internal_recommendations", write, payload.tenant_id)
        except CrossTenantWriteError as exc:
            log.warning(
                "recommendations.cross_tenant_rejected",
                tenant=payload.tenant_id,
                person_id=item.person_id,
                offering_id=item.offering_id,
            )
            raise HTTPException(status_code=422, detail=str(exc)) from exc
        except WriteTargetMissing:
            # Both endpoints are verified same-tenant before MERGE; a missing
            # endpoint cannot be verified, so the item is skipped (the MATCH
            # in the Cypher path writes nothing either).
            skipped.append(
                {"person_id": item.person_id, "offering_id": item.offering_id}
            )
            continue
        written += 1
    metrics.scores_written.labels(tenant=payload.tenant_id).inc(written)
    return {
        "tenant_id": payload.tenant_id,
        "written": written,
        "skipped": skipped,
    }
