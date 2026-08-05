"""Fraud alert triage API (SPEC-W30 §4 WS-C).

  GET  /v1/graph/alerts                  list (filters status/type/severity)
  GET  /v1/graph/alerts/{alert_id}       detail (404 cross-tenant)
  POST /v1/graph/alerts/{alert_id}/resolve   adjudicate

Tenant scoping rides the existing workforce auth seam (JWT sub, or
X-Tenant-Id in dev mode). Resolve requires a reason (min 10 chars), stamps
resolved_at/resolved_by (JWT sub), and on ``dismissed`` clears the flagged
person's quarantine ONLY when no other open high-severity alert still flags
them (``confirmed`` keeps quarantine). Every resolution emits the audit
CloudEvent ``com.opendesk.fraud.AlertResolved`` to the fraud alerts topic.
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any, Literal

import structlog
from fastapi import APIRouter, Depends, HTTPException, Query
from pydantic import BaseModel, Field

from .. import metrics
from ..auth import current_tenant
from ..events import build_alert_resolved_event
from ..templates import TemplateError, compile_template
from ..templates.alerts import ALERT_SEVERITIES, ALERT_STATUSES, ALERT_TYPES
from ..writes import (
    CrossTenantWriteError,
    WriteTargetMissing,
    compile_alert_resolve_write,
)
from . import get_deps, run_query, run_write

log = structlog.get_logger("graph-service.alerts")

router = APIRouter(prefix="/v1/graph/alerts", tags=["alerts"])


class ResolveRequest(BaseModel):
    decision: Literal["confirmed", "dismissed"]
    reason: str = Field(min_length=10, max_length=2000)


def _enum_query(value: str | None, name: str, allowed: frozenset[str]) -> str | None:
    if value is None:
        return None
    if value not in allowed:
        raise HTTPException(
            status_code=422, detail=f"{name} must be one of {sorted(allowed)}"
        )
    return value


def _cap(deps: Any) -> int:
    return deps.settings.query_row_cap


@router.get("")
async def list_alerts(
    status: str | None = Query(default=None),
    type: str | None = Query(default=None),
    severity: str | None = Query(default=None),
    tenant_id: str = Depends(current_tenant),
    deps: Any = Depends(get_deps),
) -> dict[str, Any]:
    params = {
        "status": _enum_query(status, "status", ALERT_STATUSES),
        "type": _enum_query(type, "type", ALERT_TYPES),
        "severity": _enum_query(severity, "severity", ALERT_SEVERITIES),
    }
    compiled = compile_template("alerts_list", params, row_cap=_cap(deps))
    rows = await run_query(deps, "alerts_list", compiled, tenant_id)
    return {"alerts": rows, "count": len(rows)}


async def _load_alert(deps: Any, tenant_id: str, alert_id: str) -> dict[str, Any]:
    try:
        compiled = compile_template("alert_by_id", {"alert_id": alert_id}, row_cap=1)
    except TemplateError as exc:
        raise HTTPException(status_code=422, detail=str(exc)) from exc
    rows = await run_query(deps, "alert_by_id", compiled, tenant_id)
    if not rows:
        # Unknown id AND cross-tenant id both answer 404 (no existence leak).
        raise HTTPException(status_code=404, detail="alert not found")
    return rows[0]


@router.get("/{alert_id}")
async def get_alert(
    alert_id: str,
    tenant_id: str = Depends(current_tenant),
    deps: Any = Depends(get_deps),
) -> dict[str, Any]:
    return await _load_alert(deps, tenant_id, alert_id)


@router.post("/{alert_id}/resolve")
async def resolve_alert(
    alert_id: str,
    payload: ResolveRequest,
    tenant_id: str = Depends(current_tenant),
    deps: Any = Depends(get_deps),
) -> dict[str, Any]:
    # The workforce auth seam resolves the caller from the JWT `sub` claim
    # (X-Tenant-Id in dev mode); that principal is the resolver identity.
    resolved_by = tenant_id
    alert = await _load_alert(deps, tenant_id, alert_id)
    if alert.get("status") != "open":
        raise HTTPException(
            status_code=409,
            detail=f"alert already resolved (status={alert.get('status')!r})",
        )
    resolved_at = datetime.now(timezone.utc).isoformat()
    write = compile_alert_resolve_write(
        alert_id=alert_id,
        decision=payload.decision,
        reason=payload.reason,
        resolved_by=resolved_by,
        resolved_at=resolved_at,
    )
    try:
        result = await run_write(deps, "alert_resolve", write, tenant_id)
    except CrossTenantWriteError as exc:
        raise HTTPException(status_code=422, detail=str(exc)) from exc
    except WriteTargetMissing as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc

    # SPEC-W30: `confirmed` keeps quarantine (no unquarantine path); the
    # backend cleared it only for `dismissed` with no other open highs.
    unquarantined = bool(
        result.get("unquarantined") or result.get("followup_rows")
    )
    metrics.alerts_resolved.labels(
        tenant=tenant_id, decision=payload.decision
    ).inc()

    event = build_alert_resolved_event(
        tenant_id=tenant_id,
        alert=alert,
        decision=payload.decision,
        reason=payload.reason,
        resolved_by=resolved_by,
        resolved_at=resolved_at,
    )
    await deps.events.publish(deps.settings.fraud_alerts_topic, event)
    log.info(
        "alert.resolved",
        tenant=tenant_id,
        alert_id=alert_id,
        decision=payload.decision,
        unquarantined=unquarantined,
    )
    return {
        "alert_id": alert_id,
        "status": payload.decision,
        "resolved_at": resolved_at,
        "resolved_by": resolved_by,
        "person_id": alert.get("person_id"),
        "quarantine_cleared": unquarantined,
        "event_type": event["type"],
        "topic": deps.settings.fraud_alerts_topic,
    }
