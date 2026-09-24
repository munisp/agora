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
    RecommendationWritePlan,
    ScoreWritePlan,
    compile_recommendation_batch_write,
    compile_score_batch_write,
)
from . import InternalAuth, get_deps, run_batch_write

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
    # W46-F P12: the whole batch goes to the graph in ONE tenant pre-check +
    # one UNWIND write per distinct score-field set (homogeneous batches = a
    # single statement), replacing N×(check+write) sequential round trips.
    # Per-item semantics are preserved via the RETURNING diff: unknown
    # persons match nothing and are skipped + counted (never stub-created,
    # verification gate WARN #4); a cross-tenant node still 422s the request
    # before any write lands.
    plans: list[ScoreWritePlan] = []
    for item in payload.scores:
        _check_item_tenant(payload.tenant_id, item)
        plans.append(
            ScoreWritePlan(
                person_id=item.person_id,
                scores=item.scores(),
                model_version=item.model_version,
                scored_at=item.scored_at or _now_iso(),
            )
        )
    batch = compile_score_batch_write(plans)
    try:
        result = await run_batch_write(deps, "internal_scores", batch, payload.tenant_id)
    except CrossTenantWriteError as exc:
        log.warning(
            "scores.cross_tenant_rejected",
            tenant=payload.tenant_id,
        )
        raise HTTPException(status_code=422, detail=str(exc)) from exc
    matched = {row["person_id"] for row in result["rows"]}
    written = len(result["rows"])
    skipped_unknown = [
        plan.person_id for plan in plans if plan.person_id not in matched
    ]
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
    # W46-F P12: single batched write (one pre-check + one UNWIND MERGE)
    # with the RETURNING diff preserving the per-item skip list.
    plans: list[RecommendationWritePlan] = []
    for item in payload.recommendations:
        _check_item_tenant(payload.tenant_id, item)
        plans.append(
            RecommendationWritePlan(
                person_id=item.person_id,
                offering_id=item.offering_id,
                score=item.score,
                rank=item.rank,
                reason=item.reason,
                model_version=item.model_version,
                scored_at=item.scored_at or _now_iso(),
            )
        )
    batch = compile_recommendation_batch_write(plans)
    try:
        result = await run_batch_write(
            deps, "internal_recommendations", batch, payload.tenant_id
        )
    except CrossTenantWriteError as exc:
        log.warning(
            "recommendations.cross_tenant_rejected",
            tenant=payload.tenant_id,
        )
        raise HTTPException(status_code=422, detail=str(exc)) from exc
    matched = {
        (row["person_id"], row["offering_id"]) for row in result["rows"]
    }
    written = len(result["rows"])
    # Both endpoints are verified same-tenant before MERGE; a missing
    # endpoint cannot be verified, so the item is skipped (the MATCH in the
    # Cypher path writes nothing either).
    skipped = [
        {"person_id": plan.person_id, "offering_id": plan.offering_id}
        for plan in plans
        if (plan.person_id, plan.offering_id) not in matched
    ]
    metrics.scores_written.labels(tenant=payload.tenant_id).inc(written)
    return {
        "tenant_id": payload.tenant_id,
        "written": written,
        "skipped": skipped,
    }
