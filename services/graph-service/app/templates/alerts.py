"""Fraud alert read-only templates (SPEC-W30 §4 WS-C).

Registered into the existing template allowlist by the package __init__ so
the alerts router (and the allowlisted /v1/graph/cypher seam) evaluate them
through the same tenant-scoped machinery as every other read. Alert nodes
carry tenant_id (SPEC-W30 §2); every evaluation binds the caller's tenant.
"""

from __future__ import annotations

from typing import Any

from . import GraphView, Template, TemplateError, _req

ALERT_TYPES: frozenset[str] = frozenset(
    {
        "referral_cycle",
        "sybil_cluster",
        "capture_velocity",
        "geo_impossibility",
        "consent_backdating",
        "ghost_booking",
        "gnn_anomaly",
    }
)
ALERT_SEVERITIES: frozenset[str] = frozenset({"low", "medium", "high"})
ALERT_STATUSES: frozenset[str] = frozenset({"open", "confirmed", "dismissed"})

_IDENT_RE_ERR = "alert_id must match [A-Za-z0-9_:-]{1,200}"


def _norm_alert_id(raw: Any) -> str:
    import re

    value = str(raw or "")
    if not re.match(r"^[A-Za-z0-9_:\-]{1,200}$", value):
        raise TemplateError(_IDENT_RE_ERR)
    return value


def _norm_enum(raw: Any, name: str, allowed: frozenset[str]) -> str | None:
    if raw is None or raw == "":
        return None
    value = str(raw)
    if value not in allowed:
        raise TemplateError(f"{name} must be one of {sorted(allowed)}")
    return value


def _alert_row(a: Any) -> dict[str, Any]:
    return {
        "alert_id": a.props.get("alert_id"),
        "type": a.props.get("type"),
        "severity": a.props.get("severity"),
        "status": a.props.get("status"),
        "person_id": a.props.get("person_id"),
        "agent_id": a.props.get("agent_id"),
        "evidence": a.props.get("evidence"),
        "created_at": a.props.get("created_at"),
        "resolved_at": a.props.get("resolved_at"),
        "resolved_by": a.props.get("resolved_by"),
        "resolve_reason": a.props.get("resolve_reason"),
    }


def _matches(a: Any, norm: dict[str, Any]) -> bool:
    for key in ("status", "type", "severity"):
        if norm.get(key) is not None and a.props.get(key) != norm[key]:
            return False
    return True


def _render_alerts_list(norm: dict[str, Any], cap: int) -> tuple[str, dict[str, Any]]:
    cypher = (
        "MATCH (a:Alert)\n"
        "WHERE a.tenant_id = $tenant_id\n"
        "  AND ($status IS NULL OR a.status = $status)\n"
        "  AND ($type IS NULL OR a.type = $type)\n"
        "  AND ($severity IS NULL OR a.severity = $severity)\n"
        "RETURN a.alert_id AS alert_id, a.type AS type, a.severity AS severity,\n"
        "       a.status AS status, a.person_id AS person_id, a.agent_id AS agent_id,\n"
        "       a.evidence AS evidence, a.created_at AS created_at,\n"
        "       a.resolved_at AS resolved_at, a.resolved_by AS resolved_by,\n"
        "       a.resolve_reason AS resolve_reason\n"
        "ORDER BY created_at DESC, alert_id ASC\n"
        f"LIMIT {int(cap)}"
    )
    return cypher, {
        "status": norm.get("status"),
        "type": norm.get("type"),
        "severity": norm.get("severity"),
    }


def _eval_alerts_list(view: GraphView, norm, tenant_id, cap):
    rows = [
        _alert_row(a)
        for a in view.nodes_with("Alert", tenant_id)
        if _matches(a, norm)
    ]
    rows.sort(key=lambda r: (r["created_at"] or "", r["alert_id"] or ""), reverse=True)
    return rows[:cap]


def _render_alert_by_id(norm: dict[str, Any], cap: int) -> tuple[str, dict[str, Any]]:
    cypher = (
        "MATCH (a:Alert)\n"
        "WHERE a.tenant_id = $tenant_id AND a.alert_id = $alert_id\n"
        "RETURN a.alert_id AS alert_id, a.type AS type, a.severity AS severity,\n"
        "       a.status AS status, a.person_id AS person_id, a.agent_id AS agent_id,\n"
        "       a.evidence AS evidence, a.created_at AS created_at,\n"
        "       a.resolved_at AS resolved_at, a.resolved_by AS resolved_by,\n"
        "       a.resolve_reason AS resolve_reason\n"
        "LIMIT 1"
    )
    return cypher, {"alert_id": norm["alert_id"]}


def _eval_alert_by_id(view: GraphView, norm, tenant_id, cap):
    for a in view.nodes_with("Alert", tenant_id):
        if a.props.get("alert_id") == norm["alert_id"]:
            return [_alert_row(a)]
    return []


def _normalize_list(params: dict[str, Any], now: Any) -> dict[str, Any]:
    return {
        "status": _norm_enum(params.get("status"), "status", ALERT_STATUSES),
        "type": _norm_enum(params.get("type"), "type", ALERT_TYPES),
        "severity": _norm_enum(params.get("severity"), "severity", ALERT_SEVERITIES),
    }


ALERT_TEMPLATES: dict[str, Template] = {
    t.name: t
    for t in [
        Template(
            name="alerts_list",
            description="Fraud alerts for the tenant, filterable by status/type/severity.",
            params_doc={
                "status": "open|confirmed|dismissed, optional",
                "type": "alert type, optional",
                "severity": "low|medium|high, optional",
            },
            normalize=_normalize_list,
            render=_render_alerts_list,
            evaluate=_eval_alerts_list,
        ),
        Template(
            name="alert_by_id",
            description="One fraud alert by alert_id (tenant-scoped).",
            params_doc={"alert_id": "string, required"},
            normalize=lambda p, now: {"alert_id": _norm_alert_id(_req(p, "alert_id"))},
            render=_render_alert_by_id,
            evaluate=_eval_alert_by_id,
        ),
    ]
}
