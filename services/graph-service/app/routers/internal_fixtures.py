"""Dev/e2e fixture seeder (SPEC-W30 WS-C addendum).

  POST /v1/graph/internal/fixtures/seed

Mounted ONLY when E2E_FIXTURES=1 (see main.create_app) — production images
never register this router, so the route is a hard 404 there. Auth is the
same X-Internal-Token mechanism as the score write-back API; JWTs are never
accepted.

This is NOT raw client Cypher: callers pick one of the fixed server-side
scenario builders in app.fixtures and pass bounded params; every value is
bound as a parameter and every node/edge carries the tenant_id.
"""

from __future__ import annotations

from typing import Any, Literal

import structlog
from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, Field

from ..fixtures import FixtureError, build_fixture, compile_fixture_write
from ..writes import CrossTenantWriteError, WriteTargetMissing
from . import InternalAuth, get_deps, run_write

log = structlog.get_logger("graph-service.internal_fixtures")

router = APIRouter(prefix="/v1/graph/internal/fixtures", tags=["internal"])


class SeedRequest(BaseModel):
    tenant_id: str = Field(min_length=1, max_length=100)
    scenario: Literal[
        "small_tenant",
        "referral_ring",
        "backdated_consent",
        "impossible_travel",
        "capture_burst",
    ]
    params: dict[str, Any] = Field(default_factory=dict)


@router.post("/seed", dependencies=[InternalAuth])
async def seed_fixture(
    payload: SeedRequest,
    deps: Any = Depends(get_deps),
) -> dict[str, Any]:
    try:
        plan, ids = build_fixture(payload.scenario, payload.params)
    except FixtureError as exc:
        raise HTTPException(status_code=422, detail=str(exc)) from exc
    write = compile_fixture_write(plan)
    try:
        result = await run_write(deps, "fixture_seed", write, payload.tenant_id)
    except CrossTenantWriteError as exc:
        raise HTTPException(status_code=422, detail=str(exc)) from exc
    except WriteTargetMissing as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc
    log.info(
        "fixtures.seeded",
        tenant=payload.tenant_id,
        scenario=payload.scenario,
        nodes=result.get("nodes_written"),
        edges=result.get("edges_written"),
    )
    return {
        "ok": True,
        "tenant_id": payload.tenant_id,
        "scenario": payload.scenario,
        "ids": ids,
    }
