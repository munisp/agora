"""Tenant graph bulk export shaping (SPEC-W33 §2 A2 — graph_export.py producer).

Pure, Spark-free shaping logic behind ``GET /v1/graph/internal/export/nodes``
and ``.../edges`` (routers/internal_export.py). The backend returns
tenant-scoped raw pieces (labels + properties per node, typed edges with
both endpoints) and this module shapes them into the JSONL rows that
``infra/lakehouse/spark/jobs/graph_export.py`` documents as its input
contract:

  nodes: snapshot_date, tenant_id, label, node_id, in_degree, out_degree,
         consent_marketing, quarantine, bookings_total, ltv_cents,
         no_show_rate, propensity_show, propensity_convert,
         channel_of_first_touch, last_active_at
  edges: snapshot_date, tenant_id, edge_type, src_label, src_id, dst_label,
         dst_id, weight, edge_at

PII discipline (I6 / GA4): Person identifiers are exported ONLY as the W28
salted hash — sha256(salt|tenant_id|person_id) lowercase hex, the exact
scheme of graph-sync ``graph.PhoneHash`` (SPEC-W28 §3, cf. booking-service
leads dedupe). Raw phones are never stored in the graph (phone_hash only)
and name/free-text props are never projected, so no plaintext PII can leave
this path. Edge endpoints referencing Person nodes are hashed with the same
function so nodes/edges stay joinable.
"""

from __future__ import annotations

import hashlib
from typing import Any, Iterable, Mapping

# Node labels covered by the graph_export.py node contract.
EXPORT_NODE_LABELS = ("Person", "Contact", "Offering", "Campaign")

# Property carrying the public entity id, per label (SPEC-W28 §3).
_ID_PROPS = {
    "Person": "person_id",
    "Contact": "lead_id",
    "Offering": "offering_id",
    "Campaign": "campaign_id",
    "Booking": "booking_id",
    "Consent": "consent_id",
    "Tenant": "tenant_id",
}

# Fallback label order when resolving a node's primary label (export labels
# first so e.g. a Person+Lead node exports as Person).
_LABEL_ORDER = EXPORT_NODE_LABELS + ("Booking", "Consent", "Location", "Tenant")


def w28_hash(salt: str, tenant_id: str, value: str) -> str:
    """W28 salted SHA-256: sha256(salt|tenant_id|value) lowercase hex.

    Mirrors services/graph-sync internal/graph.PhoneHash exactly (the tenant
    is part of the input so hashes are not joinable across tenants).
    """
    payload = f"{salt}|{tenant_id}|{value}"
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def primary_label(labels: Iterable[str]) -> str | None:
    """A node's primary label using the export preference order."""
    have = set(labels)
    for label in _LABEL_ORDER:
        if label in have:
            return label
    return None


def _entity_id(label: str | None, node_key: str, props: Mapping[str, Any]) -> str | None:
    """Public entity id for a node; falls back to the backend node key."""
    if label is None:
        return None
    prop = _ID_PROPS.get(label)
    value = props.get(prop) if prop else None
    if value is None or str(value) == "":
        return node_key or None
    return str(value)


def export_id(salt: str, tenant_id: str, label: str | None, raw_id: str | None) -> str | None:
    """Export-safe identifier: Person ids are W28-hashed (I6); other entity
    ids (lead/offering/campaign/booking) are not PII and pass through."""
    if raw_id is None:
        return None
    if label == "Person":
        return w28_hash(salt, tenant_id, raw_id)
    return raw_id


def _ts_max(*values: Any) -> Any:
    """Max of ISO-8601-ish timestamp values, ignoring None/empty (string
    comparison is chronological for the platform's UTC ISO format)."""
    present = [str(v) for v in values if v]
    return max(present) if present else None


def _is_true(value: Any) -> bool:
    return value is True or (isinstance(value, str) and value.lower() == "true")


class TenantExportView:
    """Indexed view over the backend's tenant-scoped export pieces."""

    def __init__(self, nodes: list[dict[str, Any]], edges: list[dict[str, Any]]):
        # node key -> {"labels": [...], "props": {...}}
        self.nodes: dict[str, dict[str, Any]] = {}
        for node in nodes:
            key = node.get("key")
            if key is None:
                continue
            self.nodes[str(key)] = {
                "labels": list(node.get("labels") or []),
                "props": dict(node.get("props") or {}),
            }
        # edge dicts carry src/dst node keys + inline endpoint labels/props
        self.edges: list[dict[str, Any]] = list(edges)
        self.out_edges: dict[str, list[dict[str, Any]]] = {}
        self.in_edges: dict[str, list[dict[str, Any]]] = {}
        for edge in self.edges:
            self.out_edges.setdefault(str(edge.get("src_key")), []).append(edge)
            self.in_edges.setdefault(str(edge.get("dst_key")), []).append(edge)

    def endpoint(self, edge: Mapping[str, Any], side: str) -> dict[str, Any]:
        """Endpoint node dict for an edge side ('src'/'dst'); prefers the
        indexed node (full props) over the edge's inline endpoint copy."""
        node = self.nodes.get(str(edge.get(f"{side}_key")))
        if node is not None:
            return node
        return {
            "labels": list(edge.get(f"{side}_labels") or []),
            "props": dict(edge.get(f"{side}_props") or {}),
        }


def _has_marketing_consent(view: TenantExportView, person_key: str, tenant_id: str) -> bool:
    """Unrevoked CONSENTED edge to a same-tenant Consent node whose purpose
    matches 'marketing' (mirrors templates.has_valid_consent semantics)."""
    for edge in view.out_edges.get(person_key, []):
        if edge.get("type") != "CONSENTED":
            continue
        if edge.get("props", {}).get("purpose") != "marketing":
            continue
        consent = view.endpoint(edge, "dst")
        if "Consent" not in consent["labels"]:
            continue
        cprops = consent["props"]
        if cprops.get("tenant_id") != tenant_id or cprops.get("purpose") != "marketing":
            continue
        if cprops.get("revoked_at"):
            continue
        return True
    return False


def _booking_aggregates(view: TenantExportView, person_key: str, tenant_id: str):
    """(bookings_total, ltv_cents, no_show_rate) over Person-[:BOOKED]->Booking."""
    total = 0
    ltv: float | None = None
    showed_known = 0
    no_shows = 0
    latest_booking_at = None
    for edge in view.out_edges.get(person_key, []):
        if edge.get("type") != "BOOKED":
            continue
        booking = view.endpoint(edge, "dst")
        if "Booking" not in booking["labels"]:
            continue
        if booking["props"].get("tenant_id") != tenant_id:
            continue
        total += 1
        amount = booking["props"].get("price_cents")
        if amount is None:
            amount = booking["props"].get("amount_cents")
        if amount is not None:
            try:
                ltv = (ltv or 0) + int(amount)
            except (TypeError, ValueError):
                pass
        showed = booking["props"].get("showed")
        if showed is not None:
            showed_known += 1
            if not _is_true(showed):
                no_shows += 1
        latest_booking_at = _ts_max(
            latest_booking_at,
            booking["props"].get("created_at"),
            edge.get("props", {}).get("at"),
        )
    no_show_rate = (no_shows / showed_known) if showed_known else None
    return total or None, ltv if total else None, no_show_rate, latest_booking_at


def _first_touch_channel(view: TenantExportView, person_key: str, tenant_id: str) -> str | None:
    """channel_of_first_touch from the EARLIEST captured Contact (first touch)."""
    best_at = None
    best_channel = None
    for edge in view.out_edges.get(person_key, []):
        if edge.get("type") != "HAS_CONTACT":
            continue
        contact = view.endpoint(edge, "dst")
        if "Contact" not in contact["labels"]:
            continue
        if contact["props"].get("tenant_id") != tenant_id:
            continue
        captured = contact["props"].get("captured_at")
        channel = contact["props"].get("channel_of_first_touch")
        if channel is None:
            continue
        if best_at is None or (captured is not None and str(captured) < str(best_at)):
            best_at = captured if captured is not None else best_at
            best_channel = str(channel)
    return best_channel


def build_node_rows(
    nodes: list[dict[str, Any]],
    edges: list[dict[str, Any]],
    tenant_id: str,
    salt: str,
    snapshot_date: str,
) -> list[dict[str, Any]]:
    """Shape tenant-scoped raw nodes/edges into graph_export.py node rows.

    Rows are filtered to the contract labels (Person/Contact/Offering/
    Campaign); nodes without a resolvable entity id are dropped (contract:
    rows without node_id are dropped downstream — we drop at source).
    Output is deterministically ordered by (label, node_id).
    """
    view = TenantExportView(nodes, edges)
    rows: list[dict[str, Any]] = []
    for key, node in view.nodes.items():
        props = node["props"]
        if props.get("tenant_id") != tenant_id:
            continue  # belt-and-braces: backend is already tenant-scoped
        label = primary_label(node["labels"])
        if label not in EXPORT_NODE_LABELS:
            continue
        raw_id = _entity_id(label, key, props)
        node_id = export_id(salt, tenant_id, label, raw_id)
        if node_id is None:
            continue
        in_degree = len(view.in_edges.get(key, []))
        out_degree = len(view.out_edges.get(key, []))

        consent_marketing: bool | None = None
        quarantine: bool | None = None
        bookings_total = ltv_cents = None
        no_show_rate = propensity_show = propensity_convert = None
        channel_first_touch: str | None = None
        last_active_at = None
        if label == "Person":
            consent_marketing = _has_marketing_consent(view, key, tenant_id)
            quarantine = _is_true(props.get("quarantine"))
            bookings_total, ltv_cents, no_show_rate, latest_booking = (
                _booking_aggregates(view, key, tenant_id)
            )
            show = props.get("propensity_show")
            if show is None:
                show = props.get("propensity_turnout")
            propensity_show = float(show) if isinstance(show, (int, float)) else None
            conv = props.get("propensity_convert")
            propensity_convert = float(conv) if isinstance(conv, (int, float)) else None
            channel_first_touch = _first_touch_channel(view, key, tenant_id)
            latest_message = None
            for edge in view.out_edges.get(key, []):
                if edge.get("type") == "MESSAGED":
                    latest_message = _ts_max(latest_message, edge.get("props", {}).get("at"))
            last_active_at = _ts_max(props.get("updated_at"), latest_booking, latest_message)
        elif label == "Contact":
            channel_first_touch = (
                str(props["channel_of_first_touch"])
                if props.get("channel_of_first_touch") is not None
                else None
            )
            last_active_at = _ts_max(props.get("updated_at"), props.get("captured_at"))
        else:
            last_active_at = _ts_max(props.get("updated_at"))

        rows.append(
            {
                "snapshot_date": snapshot_date,
                "tenant_id": tenant_id,
                "label": label,
                "node_id": node_id,
                "in_degree": in_degree,
                "out_degree": out_degree,
                "consent_marketing": consent_marketing,
                "quarantine": quarantine,
                "bookings_total": bookings_total,
                "ltv_cents": ltv_cents,
                "no_show_rate": no_show_rate,
                "propensity_show": propensity_show,
                "propensity_convert": propensity_convert,
                "channel_of_first_touch": channel_first_touch,
                "last_active_at": last_active_at,
            }
        )
    rows.sort(key=lambda r: (r["label"], r["node_id"]))
    return rows


def build_edge_rows(
    nodes: list[dict[str, Any]],
    edges: list[dict[str, Any]],
    tenant_id: str,
    salt: str,
    snapshot_date: str,
) -> list[dict[str, Any]]:
    """Shape tenant-scoped raw edges into graph_export.py edge rows.

    Every edge whose endpoints both belong to the tenant is exported (the
    backend enforces this); Person endpoint ids are W28-hashed with the same
    function as the node export so the two JSONL streams join. Deterministic
    order: (edge_type, src_id, dst_id).
    """
    view = TenantExportView(nodes, edges)
    rows: list[dict[str, Any]] = []
    for edge in view.edges:
        src = view.endpoint(edge, "src")
        dst = view.endpoint(edge, "dst")
        if (
            src["props"].get("tenant_id") != tenant_id
            or dst["props"].get("tenant_id") != tenant_id
        ):
            continue  # never leak cross-tenant structure
        src_label = primary_label(src["labels"])
        dst_label = primary_label(dst["labels"])
        src_id = export_id(
            salt, tenant_id, src_label,
            _entity_id(src_label, str(edge.get("src_key")), src["props"]),
        )
        dst_id = export_id(
            salt, tenant_id, dst_label,
            _entity_id(dst_label, str(edge.get("dst_key")), dst["props"]),
        )
        if src_id is None or dst_id is None:
            continue
        eprops = dict(edge.get("props") or {})
        weight = eprops.get("weight")
        rows.append(
            {
                "snapshot_date": snapshot_date,
                "tenant_id": tenant_id,
                "edge_type": str(edge.get("type")),
                "src_label": src_label,
                "src_id": src_id,
                "dst_label": dst_label,
                "dst_id": dst_id,
                "weight": float(weight) if isinstance(weight, (int, float)) else None,
                "edge_at": eprops.get("at") or eprops.get("created_at"),
            }
        )
    rows.sort(key=lambda r: (r["edge_type"], r["src_id"], r["dst_id"]))
    return rows
