"""HTTP routers (SPEC-W29 §3 WS-B, SPEC-W30 §4 WS-C).

Shared helpers:
* ``get_deps`` — dependency accessor (mirrors main.get_deps without a
  circular import).
* ``run_query`` — execute a CompiledQuery with the same metrics/error
  mapping the main app uses.
* ``require_internal_token`` — X-Internal-Token gate for internal routes.
  JWTs are NEVER consulted on internal routes (SPEC-W29 §3 WS-B: "never
  accept JWT on these routes"); the header must constant-time-match the
  INTERNAL_TOKEN env. Empty/missing config or header -> 401 (fail-closed).
"""

from __future__ import annotations

import hmac
from typing import Any

import structlog
from fastapi import Depends, HTTPException, Request

from .. import metrics
from ..backend import GraphError
from ..plans import CompiledQuery

log = structlog.get_logger("graph-service.routers")


def get_deps(request: Request) -> Any:
    return request.app.state.deps


async def run_query(
    deps: Any, kind: str, query: CompiledQuery, tenant_id: str
) -> list[dict[str, Any]]:
    try:
        with metrics.graph_query_latency.labels(kind=kind).time():
            rows = await deps.backend.execute(query, tenant_id)
    except GraphError as exc:
        metrics.graph_queries.labels(kind=kind, result="error").inc()
        log.warning("graph.query_failed", kind=kind, error=str(exc))
        raise HTTPException(status_code=502, detail=str(exc)) from exc
    metrics.graph_queries.labels(kind=kind, result="ok").inc()
    return rows


async def run_write(deps: Any, kind: str, write: Any, tenant_id: str) -> dict[str, Any]:
    try:
        with metrics.graph_query_latency.labels(kind=kind).time():
            result = await deps.backend.execute_write(write, tenant_id)
    except GraphError as exc:
        metrics.graph_queries.labels(kind=kind, result="error").inc()
        log.warning("graph.write_failed", kind=kind, error=str(exc))
        raise HTTPException(status_code=502, detail=str(exc)) from exc
    metrics.graph_queries.labels(kind=kind, result="ok").inc()
    return result


async def require_internal_token(request: Request) -> None:
    """401 unless X-Internal-Token constant-time-matches INTERNAL_TOKEN.

    The Authorization header is deliberately IGNORED: a valid JWT must not
    authenticate an internal write-back call."""
    settings = request.app.state.settings
    expected = settings.internal_token or ""
    provided = request.headers.get("X-Internal-Token") or ""
    if not expected:
        log.error("internal_token_unconfigured")
        raise HTTPException(status_code=401, detail="internal token not configured")
    ok = hmac.compare_digest(provided.encode("utf-8"), expected.encode("utf-8"))
    if not provided or not ok:
        raise HTTPException(status_code=401, detail="invalid or missing X-Internal-Token")


InternalAuth = Depends(require_internal_token)
