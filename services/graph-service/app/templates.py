"""Read-only Cypher template allowlist (SPEC-W28 §4 WS-B, compliance gate 5).

/v1/graph/cypher and /v1/graph/ask can ONLY execute queries from this
registry. There is no code path that runs client- or LLM-supplied Cypher
text: templates render canonical parameterized openCypher with the tenant
predicate baked in (``$tenant_id`` is bound at execution time), every result
is capped, and each template pairs its Cypher with the equivalent in-memory
evaluator so tests need no live FalkorDB.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from typing import Any, Callable, Protocol

from .plans import CompiledQuery, TemplatePlan, parse_instant

_IDENT_RE = re.compile(r"^[A-Za-z0-9_\-]{1,100}$")
_PURPOSE_RE = re.compile(r"^[a-z][a-z0-9_\-]{0,63}$")


class TemplateError(ValueError):
    """Unknown template or invalid params (mapped to 4xx by the API layer)."""


class GraphView(Protocol):
    """Duck-typed read access over a property graph (backend-provided)."""

    def nodes_with(self, label: str, tenant_id: str) -> list[Any]: ...

    def edges_from(self, node_id: str, edge_type: str | None = None) -> list[Any]: ...

    def edges_to(self, node_id: str, edge_type: str | None = None) -> list[Any]: ...

    def node_by_id(self, node_id: str) -> Any | None: ...


# ---------------------------------------------------------------------------
# shared semantics
# ---------------------------------------------------------------------------
def has_valid_consent(view: GraphView, person: Any, purpose: str, tenant_id: str) -> bool:
    """Purpose-matching, unrevoked CONSENTED edge to a Consent node whose
    purpose also matches (belt-and-braces: edge AND node carry purpose)."""
    for edge in view.edges_from(person.node_id, "CONSENTED"):
        if edge.props.get("purpose") != purpose:
            continue
        consent = view.node_by_id(edge.dst)
        if consent is None or "Consent" not in consent.labels:
            continue
        if consent.props.get("tenant_id") != tenant_id:
            continue
        if consent.props.get("purpose") != purpose:
            continue
        if consent.props.get("revoked_at"):
            continue
        return True
    return False


def _bookings(view: GraphView, person: Any, tenant_id: str) -> list[Any]:
    out = []
    for edge in view.edges_from(person.node_id, "BOOKED"):
        b = view.node_by_id(edge.dst)
        if b is not None and "Booking" in b.labels and b.props.get("tenant_id") == tenant_id:
            out.append(b)
    return out


def _messages(view: GraphView, person: Any) -> list[Any]:
    return list(view.edges_from(person.node_id, "MESSAGED"))


# ---------------------------------------------------------------------------
# param normalization
# ---------------------------------------------------------------------------
def _norm_person_id(raw: Any) -> str:
    value = str(raw or "")
    if not _IDENT_RE.match(value):
        raise TemplateError("person_id must match [A-Za-z0-9_-]{1,100}")
    return value


def _norm_purpose(raw: Any) -> str:
    value = str(raw or "")
    if not _PURPOSE_RE.match(value):
        raise TemplateError("purpose must match [a-z][a-z0-9_-]{0,63}")
    return value


def _norm_date(raw: Any) -> datetime:
    try:
        return parse_instant(str(raw))
    except (ValueError, TypeError) as exc:
        raise TemplateError(f"invalid ISO date/datetime: {raw!r}") from exc


def _norm_days(raw: Any) -> int:
    try:
        days = int(raw)
    except (ValueError, TypeError) as exc:
        raise TemplateError("days must be an integer") from exc
    if not 0 <= days <= 3650:
        raise TemplateError("days out of range [0, 3650]")
    return days


# ---------------------------------------------------------------------------
# registry
# ---------------------------------------------------------------------------
@dataclass(frozen=True)
class Template:
    name: str
    description: str
    params_doc: dict[str, str]
    normalize: Callable[[dict[str, Any], datetime], dict[str, Any]]
    render: Callable[[dict[str, Any], int], tuple[str, dict[str, Any]]]
    evaluate: Callable[[GraphView, dict[str, Any], str, int], list[dict[str, Any]]]


def _person_row(p: Any) -> dict[str, Any]:
    return {
        "person_id": p.props.get("person_id"),
        "name": p.props.get("name"),
        "phone_hash": p.props.get("phone_hash"),
        "channels": p.props.get("channels") or [],
        "consent_summary": p.props.get("consent_summary"),
        "quarantine": bool(p.props.get("quarantine", False)),
        "updated_at": p.props.get("updated_at"),
    }


def _find_person(view: GraphView, person_id: str, tenant_id: str) -> Any | None:
    for p in view.nodes_with("Person", tenant_id):
        if p.props.get("person_id") == person_id:
            return p
    return None


# --- person 360 building blocks --------------------------------------------
def _render_person_by_id(norm: dict[str, Any], cap: int) -> tuple[str, dict[str, Any]]:
    cypher = (
        "MATCH (p:Person)\n"
        "WHERE p.tenant_id = $tenant_id AND p.person_id = $person_id\n"
        "RETURN p.person_id AS person_id, p.name AS name, p.phone_hash AS phone_hash,\n"
        "       p.channels AS channels, p.consent_summary AS consent_summary,\n"
        "       coalesce(p.quarantine, false) AS quarantine, p.updated_at AS updated_at\n"
        "LIMIT 1"
    )
    return cypher, {"person_id": norm["person_id"]}


def _eval_person_by_id(view, norm, tenant_id, cap):
    p = _find_person(view, norm["person_id"], tenant_id)
    return [_person_row(p)] if p is not None else []


def _render_person_contacts(norm, cap):
    cypher = (
        "MATCH (p:Person)-[:HAS_CONTACT]->(ct:Contact)\n"
        "WHERE p.tenant_id = $tenant_id AND p.person_id = $person_id\n"
        "  AND ct.tenant_id = $tenant_id\n"
        "OPTIONAL MATCH (ct)-[:CAPTURED_AT]->(l:Location)\n"
        "RETURN ct.lead_id AS lead_id, ct.channel_of_first_touch AS channel_of_first_touch,\n"
        "       ct.source AS source, ct.captured_at AS captured_at,\n"
        "       l.lga AS lga, l.ward AS ward\n"
        f"ORDER BY lead_id\nLIMIT {cap}"
    )
    return cypher, {"person_id": norm["person_id"]}


def _eval_person_contacts(view, norm, tenant_id, cap):
    p = _find_person(view, norm["person_id"], tenant_id)
    if p is None:
        return []
    rows = []
    for edge in view.edges_from(p.node_id, "HAS_CONTACT"):
        ct = view.node_by_id(edge.dst)
        if ct is None or "Contact" not in ct.labels or ct.props.get("tenant_id") != tenant_id:
            continue
        lga = ward = None
        for ce in view.edges_from(ct.node_id, "CAPTURED_AT"):
            loc = view.node_by_id(ce.dst)
            if loc is not None and "Location" in loc.labels:
                lga, ward = loc.props.get("lga"), loc.props.get("ward")
        rows.append(
            {
                "lead_id": ct.props.get("lead_id"),
                "channel_of_first_touch": ct.props.get("channel_of_first_touch"),
                "source": ct.props.get("source"),
                "captured_at": ct.props.get("captured_at"),
                "lga": lga,
                "ward": ward,
            }
        )
    rows.sort(key=lambda r: (r["lead_id"] or ""))
    return rows[:cap]


def _render_person_bookings(norm, cap):
    cypher = (
        "MATCH (p:Person)-[:BOOKED]->(b:Booking)\n"
        "WHERE p.tenant_id = $tenant_id AND p.person_id = $person_id\n"
        "  AND b.tenant_id = $tenant_id\n"
        "OPTIONAL MATCH (b)-[:FOR]->(o:Offering)\n"
        "RETURN b.booking_id AS booking_id, b.status AS status, b.created_at AS created_at,\n"
        "       b.showed AS showed, o.offering_id AS offering_id, o.name AS offering_name\n"
        f"ORDER BY created_at DESC\nLIMIT {cap}"
    )
    return cypher, {"person_id": norm["person_id"]}


def _eval_person_bookings(view, norm, tenant_id, cap):
    p = _find_person(view, norm["person_id"], tenant_id)
    if p is None:
        return []
    rows = []
    for b in _bookings(view, p, tenant_id):
        offering_id = offering_name = None
        for oe in view.edges_from(b.node_id, "FOR"):
            o = view.node_by_id(oe.dst)
            if o is not None and "Offering" in o.labels:
                offering_id, offering_name = o.props.get("offering_id"), o.props.get("name")
        rows.append(
            {
                "booking_id": b.props.get("booking_id"),
                "status": b.props.get("status"),
                "created_at": b.props.get("created_at"),
                "showed": b.props.get("showed"),
                "offering_id": offering_id,
                "offering_name": offering_name,
            }
        )
    rows.sort(key=lambda r: (r["created_at"] or ""), reverse=True)
    return rows[:cap]


def _render_person_consents(norm, cap):
    cypher = (
        "MATCH (p:Person)-[ce:CONSENTED]->(c:Consent)\n"
        "WHERE p.tenant_id = $tenant_id AND p.person_id = $person_id\n"
        "  AND c.tenant_id = $tenant_id\n"
        "RETURN c.consent_id AS consent_id, c.purpose AS purpose, c.granted_at AS granted_at,\n"
        "       c.revoked_at AS revoked_at, ce.at AS consented_at\n"
        f"ORDER BY granted_at\nLIMIT {cap}"
    )
    return cypher, {"person_id": norm["person_id"]}


def _eval_person_consents(view, norm, tenant_id, cap):
    p = _find_person(view, norm["person_id"], tenant_id)
    if p is None:
        return []
    rows = []
    for edge in view.edges_from(p.node_id, "CONSENTED"):
        c = view.node_by_id(edge.dst)
        if c is None or "Consent" not in c.labels or c.props.get("tenant_id") != tenant_id:
            continue
        rows.append(
            {
                "consent_id": c.props.get("consent_id"),
                "purpose": c.props.get("purpose"),
                "granted_at": c.props.get("granted_at"),
                "revoked_at": c.props.get("revoked_at"),
                "consented_at": edge.props.get("at"),
            }
        )
    rows.sort(key=lambda r: (r["granted_at"] or ""))
    return rows[:cap]


def _render_person_referrals(norm, cap):
    cypher = (
        "MATCH (p:Person)-[r:REFERRED]->(q:Person)\n"
        "WHERE p.tenant_id = $tenant_id AND p.person_id = $person_id AND q.tenant_id = $tenant_id\n"
        "RETURN 'outgoing' AS direction, q.person_id AS person_id, q.name AS name,\n"
        "       r.program AS program, r.at AS at\n"
        "UNION\n"
        "MATCH (q:Person)-[r:REFERRED]->(p:Person)\n"
        "WHERE p.tenant_id = $tenant_id AND p.person_id = $person_id AND q.tenant_id = $tenant_id\n"
        "RETURN 'incoming' AS direction, q.person_id AS person_id, q.name AS name,\n"
        "       r.program AS program, r.at AS at\n"
        f"LIMIT {cap}"
    )
    return cypher, {"person_id": norm["person_id"]}


def _eval_person_referrals(view, norm, tenant_id, cap):
    p = _find_person(view, norm["person_id"], tenant_id)
    if p is None:
        return []
    rows = []
    for edge in view.edges_from(p.node_id, "REFERRED"):
        q = view.node_by_id(edge.dst)
        if q is not None and "Person" in q.labels and q.props.get("tenant_id") == tenant_id:
            rows.append(
                {
                    "direction": "outgoing",
                    "person_id": q.props.get("person_id"),
                    "name": q.props.get("name"),
                    "program": edge.props.get("program"),
                    "at": edge.props.get("at"),
                }
            )
    for edge in view.edges_to(p.node_id, "REFERRED"):
        q = view.node_by_id(edge.src)
        if q is not None and "Person" in q.labels and q.props.get("tenant_id") == tenant_id:
            rows.append(
                {
                    "direction": "incoming",
                    "person_id": q.props.get("person_id"),
                    "name": q.props.get("name"),
                    "program": edge.props.get("program"),
                    "at": edge.props.get("at"),
                }
            )
    return rows[:cap]


def _render_person_messages(norm, cap):
    cypher = (
        "MATCH (p:Person)-[m:MESSAGED]->(cp:Campaign)\n"
        "WHERE p.tenant_id = $tenant_id AND p.person_id = $person_id\n"
        "  AND cp.tenant_id = $tenant_id\n"
        "RETURN m.campaign_id AS campaign_id, cp.kind AS kind, m.at AS at, m.status AS status\n"
        f"ORDER BY at DESC\nLIMIT {cap}"
    )
    return cypher, {"person_id": norm["person_id"]}


def _eval_person_messages(view, norm, tenant_id, cap):
    p = _find_person(view, norm["person_id"], tenant_id)
    if p is None:
        return []
    rows = []
    for edge in _messages(view, p):
        cp = view.node_by_id(edge.dst)
        if cp is None or "Campaign" not in cp.labels or cp.props.get("tenant_id") != tenant_id:
            continue
        rows.append(
            {
                "campaign_id": edge.props.get("campaign_id") or cp.props.get("campaign_id"),
                "kind": cp.props.get("kind"),
                "at": edge.props.get("at"),
                "status": edge.props.get("status"),
            }
        )
    rows.sort(key=lambda r: (r["at"] or ""), reverse=True)
    return rows[:cap]


# --- analytics templates (also the /v1/graph/ask shape allowlist) -----------
def _render_persons_by_consent(norm, cap):
    cypher = (
        "MATCH (p:Person)-[ce:CONSENTED]->(c:Consent)\n"
        "WHERE p.tenant_id = $tenant_id AND ce.purpose = $purpose\n"
        "  AND c.tenant_id = $tenant_id AND c.purpose = $purpose\n"
        "  AND c.revoked_at IS NULL\n"
        "RETURN p.person_id AS person_id, p.name AS name,\n"
        "       coalesce(p.quarantine, false) AS quarantine\n"
        f"ORDER BY person_id\nLIMIT {cap}"
    )
    return cypher, {"purpose": norm["purpose"]}


def _eval_persons_by_consent(view, norm, tenant_id, cap):
    rows = [
        {
            "person_id": p.props.get("person_id"),
            "name": p.props.get("name"),
            "quarantine": bool(p.props.get("quarantine", False)),
        }
        for p in view.nodes_with("Person", tenant_id)
        if has_valid_consent(view, p, norm["purpose"], tenant_id)
    ]
    rows.sort(key=lambda r: r["person_id"] or "")
    return rows[:cap]


def _render_consent_counts(norm, cap):
    cypher = (
        "MATCH (p:Person)-[ce:CONSENTED]->(c:Consent)\n"
        "WHERE p.tenant_id = $tenant_id AND c.tenant_id = $tenant_id\n"
        "  AND ce.purpose = c.purpose AND c.revoked_at IS NULL\n"
        "RETURN c.purpose AS purpose, count(DISTINCT p) AS persons\n"
        f"ORDER BY purpose\nLIMIT {cap}"
    )
    return cypher, {}


def _eval_consent_counts(view, norm, tenant_id, cap):
    counts: dict[str, set[str]] = {}
    for p in view.nodes_with("Person", tenant_id):
        for edge in view.edges_from(p.node_id, "CONSENTED"):
            c = view.node_by_id(edge.dst)
            if c is None or "Consent" not in c.labels:
                continue
            if c.props.get("tenant_id") != tenant_id or c.props.get("revoked_at"):
                continue
            purpose = edge.props.get("purpose")
            if purpose and purpose == c.props.get("purpose"):
                counts.setdefault(purpose, set()).add(p.props.get("person_id"))
    rows = [
        {"purpose": purpose, "persons": len(ids)}
        for purpose, ids in sorted(counts.items())
    ]
    return rows[:cap]


def _render_persons_lapsed(norm, cap):
    cypher = (
        "MATCH (p:Person)-[:BOOKED]->(b:Booking)\n"
        "WHERE p.tenant_id = $tenant_id AND b.tenant_id = $tenant_id\n"
        "WITH p, max(b.created_at) AS last_booking\n"
        "WHERE last_booking < $before\n"
        "RETURN p.person_id AS person_id, p.name AS name, last_booking AS last_booking_at\n"
        f"ORDER BY last_booking_at\nLIMIT {cap}"
    )
    return cypher, {"before": norm["before"].isoformat()}


def _eval_persons_lapsed(view, norm, tenant_id, cap):
    rows = []
    for p in view.nodes_with("Person", tenant_id):
        stamps = [
            parse_instant(b.props.get("created_at"))
            for b in _bookings(view, p, tenant_id)
            if b.props.get("created_at")
        ]
        if not stamps:
            continue
        last = max(stamps)
        if last < norm["before"]:
            rows.append(
                {
                    "person_id": p.props.get("person_id"),
                    "name": p.props.get("name"),
                    "last_booking_at": last.isoformat(),
                }
            )
    rows.sort(key=lambda r: r["last_booking_at"])
    return rows[:cap]


def _render_bookings_per_offering(norm, cap):
    cypher = (
        "MATCH (b:Booking)-[:FOR]->(o:Offering)\n"
        "WHERE b.tenant_id = $tenant_id AND o.tenant_id = $tenant_id\n"
        "RETURN o.offering_id AS offering_id, o.name AS name, count(b) AS bookings\n"
        f"ORDER BY bookings DESC\nLIMIT {cap}"
    )
    return cypher, {}


def _eval_bookings_per_offering(view, norm, tenant_id, cap):
    counts: dict[str, dict[str, Any]] = {}
    for b in view.nodes_with("Booking", tenant_id):
        for oe in view.edges_from(b.node_id, "FOR"):
            o = view.node_by_id(oe.dst)
            if o is None or "Offering" not in o.labels:
                continue
            if o.props.get("tenant_id") != tenant_id:
                continue
            key = o.props.get("offering_id")
            entry = counts.setdefault(key, {"offering_id": key, "name": o.props.get("name"), "bookings": 0})
            entry["bookings"] += 1
    rows = sorted(counts.values(), key=lambda r: (-r["bookings"], r["offering_id"] or ""))
    return rows[:cap]


def _render_persons_not_messaged_since(norm, cap):
    cypher = (
        "MATCH (p:Person)\n"
        "WHERE p.tenant_id = $tenant_id\n"
        "OPTIONAL MATCH (p)-[m:MESSAGED]->(:Campaign)\n"
        "WITH p, max(m.at) AS last_msg\n"
        "WHERE last_msg IS NULL OR last_msg < $since\n"
        "RETURN p.person_id AS person_id, p.name AS name, last_msg AS last_messaged_at\n"
        f"ORDER BY person_id\nLIMIT {cap}"
    )
    return cypher, {"since": norm["since"].isoformat()}


def _eval_persons_not_messaged_since(view, norm, tenant_id, cap):
    rows = []
    for p in view.nodes_with("Person", tenant_id):
        stamps = [
            parse_instant(m.props.get("at"))
            for m in _messages(view, p)
            if m.props.get("at")
        ]
        last = max(stamps) if stamps else None
        if last is None or last < norm["since"]:
            rows.append(
                {
                    "person_id": p.props.get("person_id"),
                    "name": p.props.get("name"),
                    "last_messaged_at": last.isoformat() if last else None,
                }
            )
    rows.sort(key=lambda r: r["person_id"] or "")
    return rows[:cap]


def _req(params: dict[str, Any], name: str) -> Any:
    value = params.get(name)
    if value is None or value == "":
        raise TemplateError(f"missing required param {name!r}")
    return value


TEMPLATES: dict[str, Template] = {
    t.name: t
    for t in [
        Template(
            name="person_by_id",
            description="One person by person_id (person 360 anchor).",
            params_doc={"person_id": "string, required"},
            normalize=lambda p, now: {"person_id": _norm_person_id(_req(p, "person_id"))},
            render=_render_person_by_id,
            evaluate=_eval_person_by_id,
        ),
        Template(
            name="person_contacts",
            description="Contacts (leads) for a person, with capture location.",
            params_doc={"person_id": "string, required"},
            normalize=lambda p, now: {"person_id": _norm_person_id(_req(p, "person_id"))},
            render=_render_person_contacts,
            evaluate=_eval_person_contacts,
        ),
        Template(
            name="person_bookings",
            description="Bookings for a person, with offering.",
            params_doc={"person_id": "string, required"},
            normalize=lambda p, now: {"person_id": _norm_person_id(_req(p, "person_id"))},
            render=_render_person_bookings,
            evaluate=_eval_person_bookings,
        ),
        Template(
            name="person_consents",
            description="Consent records for a person (incl. revoked).",
            params_doc={"person_id": "string, required"},
            normalize=lambda p, now: {"person_id": _norm_person_id(_req(p, "person_id"))},
            render=_render_person_consents,
            evaluate=_eval_person_consents,
        ),
        Template(
            name="person_referrals",
            description="Referral edges for a person (both directions).",
            params_doc={"person_id": "string, required"},
            normalize=lambda p, now: {"person_id": _norm_person_id(_req(p, "person_id"))},
            render=_render_person_referrals,
            evaluate=_eval_person_referrals,
        ),
        Template(
            name="person_messages",
            description="Outreach messages sent to a person (campaigns).",
            params_doc={"person_id": "string, required"},
            normalize=lambda p, now: {"person_id": _norm_person_id(_req(p, "person_id"))},
            render=_render_person_messages,
            evaluate=_eval_person_messages,
        ),
        Template(
            name="persons_by_consent",
            description="Persons holding a valid (unrevoked) consent of a given purpose.",
            params_doc={"purpose": "string, required"},
            normalize=lambda p, now: {"purpose": _norm_purpose(_req(p, "purpose"))},
            render=_render_persons_by_consent,
            evaluate=_eval_persons_by_consent,
        ),
        Template(
            name="consent_counts",
            description="Count of consent-passing persons per purpose.",
            params_doc={},
            normalize=lambda p, now: {},
            render=_render_consent_counts,
            evaluate=_eval_consent_counts,
        ),
        Template(
            name="persons_lapsed",
            description="Persons whose most recent booking is before a date (lapsed customers).",
            params_doc={"before": "ISO date/datetime, required"},
            normalize=lambda p, now: {"before": _norm_date(_req(p, "before"))},
            render=_render_persons_lapsed,
            evaluate=_eval_persons_lapsed,
        ),
        Template(
            name="bookings_per_offering",
            description="Booking counts per offering.",
            params_doc={},
            normalize=lambda p, now: {},
            render=_render_bookings_per_offering,
            evaluate=_eval_bookings_per_offering,
        ),
        Template(
            name="persons_not_messaged_since",
            description="Persons not messaged in the last N days.",
            params_doc={"days": "integer 0..3650, required"},
            normalize=lambda p, now: {
                "since": now - timedelta(days=_norm_days(_req(p, "days")))
            },
            render=_render_persons_not_messaged_since,
            evaluate=_eval_persons_not_messaged_since,
        ),
    ]
}

# /v1/graph/ask may only map questions onto these read-only shapes
# (SPEC-W28 §4: READ-ONLY template allowlist).
ASK_ALLOWED: frozenset[str] = frozenset(TEMPLATES.keys())


def compile_template(
    name: str,
    params: dict[str, Any] | None,
    *,
    now: datetime | None = None,
    row_cap: int = 100,
) -> CompiledQuery:
    """Validate params and compile an allowlisted template. Raises TemplateError."""
    template = TEMPLATES.get(name)
    if template is None:
        raise TemplateError(f"unknown template {name!r}; allowlist: {sorted(TEMPLATES)}")
    now = now or datetime.now(timezone.utc)
    norm = template.normalize(dict(params or {}), now)
    cypher, cypher_params = template.render(norm, int(row_cap))
    return CompiledQuery(
        cypher=cypher,
        params=cypher_params,
        plan=TemplatePlan(name=name, params=norm),
    )
