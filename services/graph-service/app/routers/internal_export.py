"""Internal tenant graph bulk export (SPEC-W33 §2 A2 — graph_export.py producer).

  GET /v1/graph/internal/export/nodes?tenant_id=...&snapshot_date=YYYY-MM-DD
  GET /v1/graph/internal/export/edges?tenant_id=...&snapshot_date=YYYY-MM-DD

These are the read-side producer endpoints the lakehouse
``infra/lakehouse/spark/jobs/graph_export.py`` job consumes (closing its
TODO producer note): newline-delimited JSON (application/x-ndjson) streams
shaped exactly to the node/edge input contracts documented in that job.

Security posture (matches internal_scores.py):
* Auth is X-Internal-Token == INTERNAL_TOKEN (constant-time compare) —
  JWTs are NEVER accepted here (routers.require_internal_token).
* Tenant scoping is explicit and mandatory: ``tenant_id`` is a required
  query parameter (same trust model as the tenant envelope on the internal
  write-back POSTs); the backend only ever returns nodes/edges carrying
  that tenant_id, and export.build_*_rows re-verifies per row.
* PII discipline (I6/GA4): Person identifiers are exported ONLY as the W28
  salted SHA-256 hash (export.w28_hash; sha256(salt|tenant|id), the
  graph-sync PhoneHash scheme). Names/phones are never projected.

``snapshot_date`` defaults to the current UTC date and may be pinned for
deterministic backfills; it is stamped on every row (the Spark contract
partitions by it).
"""

from __future__ import annotations

import json
from datetime import date, datetime, timezone
from typing import Any, Iterator

import structlog
from fastapi import APIRouter, Depends, HTTPException, Query
from fastapi.responses import StreamingResponse

from .. import metrics
from ..backend import GraphError
from ..export import build_edge_rows, build_node_rows
from . import InternalAuth, get_deps

log = structlog.get_logger("graph-service.internal_export")

router = APIRouter(prefix="/v1/graph/internal/export", tags=["internal"])

_DATE_DOC = "snapshot date stamped on every row (default: today, UTC)"


def _resolve_snapshot_date(raw: str | None) -> str:
    if raw is None or not str(raw).strip():
        return datetime.now(timezone.utc).date().isoformat()
    try:
        return date.fromisoformat(str(raw).strip()).isoformat()
    except ValueError as exc:
        raise HTTPException(
            status_code=422, detail=f"invalid snapshot_date {raw!r} (want YYYY-MM-DD)"
        ) from exc


def _jsonl(rows: list[dict[str, Any]]) -> Iterator[bytes]:
    """One JSON object per line (JSONL / ND-JSON), UTF-8."""
    for row in rows:
        yield (json.dumps(row, sort_keys=True, ensure_ascii=False) + "\n").encode("utf-8")


async def _export(deps: Any, kind: str, tenant_id: str) -> tuple[list, list]:
    """Fetch tenant-scoped raw nodes+edges with the run_query error/metrics
    mapping (GraphError -> 502)."""
    try:
        with metrics.graph_query_latency.labels(kind=kind).time():
            nodes = await deps.backend.export_nodes(tenant_id)
            edges = await deps.backend.export_edges(tenant_id)
    except GraphError as exc:
        metrics.graph_queries.labels(kind=kind, result="error").inc()
        log.warning("graph.export_failed", kind=kind, error=str(exc))
        raise HTTPException(status_code=502, detail=str(exc)) from exc
    metrics.graph_queries.labels(kind=kind, result="ok").inc()
    return nodes, edges


@router.get("/nodes", dependencies=[InternalAuth])
async def export_nodes(
    tenant_id: str = Query(min_length=1, max_length=100),
    snapshot_date: str | None = Query(default=None, description=_DATE_DOC),
    deps: Any = Depends(get_deps),
) -> StreamingResponse:
    snap = _resolve_snapshot_date(snapshot_date)
    nodes, edges = await _export(deps, "internal_export_nodes", tenant_id)
    rows = build_node_rows(
        nodes, edges, tenant_id, deps.settings.phone_hash_salt, snap
    )
    log.info("export.nodes", tenant=tenant_id, snapshot_date=snap, rows=len(rows))
    return StreamingResponse(
        _jsonl(rows),
        media_type="application/x-ndjson",
        headers={"X-Row-Count": str(len(rows)), "X-Snapshot-Date": snap},
    )


@router.get("/edges", dependencies=[InternalAuth])
async def export_edges(
    tenant_id: str = Query(min_length=1, max_length=100),
    snapshot_date: str | None = Query(default=None, description=_DATE_DOC),
    deps: Any = Depends(get_deps),
) -> StreamingResponse:
    snap = _resolve_snapshot_date(snapshot_date)
    nodes, edges = await _export(deps, "internal_export_edges", tenant_id)
    rows = build_edge_rows(
        nodes, edges, tenant_id, deps.settings.phone_hash_salt, snap
    )
    log.info("export.edges", tenant=tenant_id, snapshot_date=snap, rows=len(rows))
    return StreamingResponse(
        _jsonl(rows),
        media_type="application/x-ndjson",
        headers={"X-Row-Count": str(len(rows)), "X-Snapshot-Date": snap},
    )
