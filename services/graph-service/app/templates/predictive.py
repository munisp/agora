"""Predictive read-only templates (SPEC-W29 §3 WS-B).

Registered into the existing template allowlist (``app.templates.TEMPLATES``)
by the package __init__. All four are read-only, tenant-scoped by
construction, and fully parameterized — values are bound ($person_id,
$min_score, ...), never interpolated (integer LIMIT caps follow the existing
template convention).

Templates:
  next_best_services  — RECOMMENDED_FOR edges by rank; same-tenant
                        co-occurrence fallback when no edges exist.
  churn_risk_band     — persons with propensity_churn >= min, quarantined
                        excluded, consent-purpose summary attached.
  referral_value      — referral out-degree + converted-referee count;
                        tenant leaderboard when person_id is omitted.
  similar_persons     — cosine over stored Ollama name_embeddings
                        (written by graph-sync), self excluded, k=10 default.
"""

from __future__ import annotations

import math
from typing import Any

from . import (
    GraphView,
    Template,
    TemplateError,
    _norm_person_id,
    _req,
    has_valid_consent,
)

EMBEDDING_PROP = "name_embedding"


# ---------------------------------------------------------------------------
# param normalization
# ---------------------------------------------------------------------------
def _norm_probability(raw: Any, name: str) -> float:
    if isinstance(raw, bool) or not isinstance(raw, (int, float)):
        raise TemplateError(f"{name} must be a number in [0, 1]")
    value = float(raw)
    if not 0.0 <= value <= 1.0:
        raise TemplateError(f"{name} must be in [0, 1]")
    return value


def _norm_k(raw: Any) -> int:
    try:
        k = int(raw)
    except (TypeError, ValueError) as exc:
        raise TemplateError("k must be an integer") from exc
    if not 1 <= k <= 100:
        raise TemplateError("k out of range [1, 100]")
    return k


def _norm_opt_person_id(params: dict[str, Any]) -> dict[str, Any]:
    raw = params.get("person_id")
    if raw is None or raw == "":
        return {}
    return {"person_id": _norm_person_id(raw)}


# ---------------------------------------------------------------------------
# shared evaluator helpers
# ---------------------------------------------------------------------------
def _find_person(view: GraphView, person_id: str, tenant_id: str) -> Any | None:
    for p in view.nodes_with("Person", tenant_id):
        if p.props.get("person_id") == person_id:
            return p
    return None


def _is_quarantined(person: Any) -> bool:
    # Canonical W28 property (docs/graph.md §3.2, audience gate): `quarantine`.
    return bool(person.props.get("quarantine"))


def _person_offering_ids(view: GraphView, person: Any, tenant_id: str) -> set[str]:
    ids: set[str] = set()
    for be in view.edges_from(person.node_id, "BOOKED"):
        b = view.node_by_id(be.dst)
        if b is None or "Booking" not in b.labels:
            continue
        if b.props.get("tenant_id") != tenant_id:
            continue
        for oe in view.edges_from(b.node_id, "FOR"):
            o = view.node_by_id(oe.dst)
            if o is not None and "Offering" in o.labels:
                if o.props.get("tenant_id") == tenant_id:
                    ids.add(o.props.get("offering_id"))
    return ids


def _person_location_node_ids(view: GraphView, person: Any, tenant_id: str) -> set[str]:
    locs: set[str] = set()
    for he in view.edges_from(person.node_id, "HAS_CONTACT"):
        ct = view.node_by_id(he.dst)
        if ct is None or "Contact" not in ct.labels:
            continue
        if ct.props.get("tenant_id") != tenant_id:
            continue
        for le in view.edges_from(ct.node_id, "CAPTURED_AT"):
            loc = view.node_by_id(le.dst)
            if loc is not None and "Location" in loc.labels:
                if loc.props.get("tenant_id") == tenant_id:
                    locs.add(loc.node_id)
    return locs


def _consent_purposes(view: GraphView, person: Any, tenant_id: str) -> list[str]:
    purposes: set[str] = set()
    for edge in view.edges_from(person.node_id, "CONSENTED"):
        purpose = edge.props.get("purpose")
        if purpose and has_valid_consent(view, person, purpose, tenant_id):
            purposes.add(purpose)
    return sorted(purposes)


# ---------------------------------------------------------------------------
# 1. next_best_services
# ---------------------------------------------------------------------------
def _render_next_best_services(norm: dict[str, Any], cap: int) -> tuple[str, dict[str, Any]]:
    # Edges first (rank-ordered); the UNION leg only fires when the person
    # has NO RECOMMENDED_FOR edges — same-tenant co-occurrence fallback over
    # persons sharing a capture location ("clients like them booked ...").
    cypher = (
        "MATCH (p:Person)-[r:RECOMMENDED_FOR]->(o:Offering)\n"
        "WHERE p.tenant_id = $tenant_id AND p.person_id = $person_id\n"
        "  AND o.tenant_id = $tenant_id\n"
        "RETURN o.offering_id AS offering_id, o.name AS name, r.score AS score,\n"
        "       r.rank AS rank, r.reason AS reason, r.model_version AS model_version,\n"
        "       'edges' AS source\n"
        "UNION ALL\n"
        "MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})\n"
        "WHERE NOT EXISTS { (p)-[:RECOMMENDED_FOR]->(:Offering) }\n"
        "MATCH (p)-[:HAS_CONTACT]->(:Contact)-[:CAPTURED_AT]->(l:Location)\n"
        "MATCH (q:Person)-[:HAS_CONTACT]->(:Contact)-[:CAPTURED_AT]->(l)\n"
        "WHERE q.tenant_id = $tenant_id AND q.person_id <> $person_id\n"
        "MATCH (q)-[:BOOKED]->(bq:Booking)-[:FOR]->(o:Offering)\n"
        "WHERE bq.tenant_id = $tenant_id AND o.tenant_id = $tenant_id\n"
        "  AND NOT EXISTS { (p)-[:BOOKED]->(:Booking)-[:FOR]->(o) }\n"
        "RETURN o.offering_id AS offering_id, o.name AS name,\n"
        "       toFloat(count(DISTINCT q)) AS score, 999 AS rank,\n"
        "       'clients_like_them_booked' AS reason, null AS model_version,\n"
        "       'cooccurrence' AS source\n"
        "ORDER BY rank ASC, score DESC\n"
        f"LIMIT {int(cap)}"
    )
    return cypher, {"person_id": norm["person_id"]}


def _eval_next_best_services(view: GraphView, norm, tenant_id, cap):
    person = _find_person(view, norm["person_id"], tenant_id)
    if person is None:
        return []
    rows: list[dict[str, Any]] = []
    for edge in view.edges_from(person.node_id, "RECOMMENDED_FOR"):
        o = view.node_by_id(edge.dst)
        if o is None or "Offering" not in o.labels:
            continue
        if o.props.get("tenant_id") != tenant_id:
            continue
        rows.append(
            {
                "offering_id": o.props.get("offering_id"),
                "name": o.props.get("name"),
                "score": edge.props.get("score"),
                "rank": edge.props.get("rank"),
                "reason": edge.props.get("reason"),
                "model_version": edge.props.get("model_version"),
                "source": "edges",
            }
        )
    if rows:
        rows.sort(key=lambda r: (r["rank"] if r["rank"] is not None else 999))
        return rows[:cap]

    # Fallback: same-tenant co-occurrence — offerings booked by persons who
    # share a capture location with the target (or an offering), excluding
    # offerings the target already booked.
    target_offerings = _person_offering_ids(view, person, tenant_id)
    target_locs = _person_location_node_ids(view, person, tenant_id)
    counts: dict[str, dict[str, Any]] = {}
    for other in view.nodes_with("Person", tenant_id):
        if other.node_id == person.node_id:
            continue
        other_locs = _person_location_node_ids(view, other, tenant_id)
        other_offerings = _person_offering_ids(view, other, tenant_id)
        similar = bool(target_locs & other_locs) or bool(
            target_offerings & other_offerings
        )
        if not similar:
            continue
        for oid in other_offerings - target_offerings:
            entry = counts.setdefault(oid, {"count": 0, "name": None})
            entry["count"] += 1
    offerings = {o.props.get("offering_id"): o for o in view.nodes_with("Offering", tenant_id)}
    ranked = sorted(counts.items(), key=lambda kv: (-kv[1]["count"], kv[0]))
    rows = []
    for i, (oid, info) in enumerate(ranked, start=1):
        o = offerings.get(oid)
        rows.append(
            {
                "offering_id": oid,
                "name": o.props.get("name") if o else None,
                "score": float(info["count"]),
                "rank": i,
                "reason": "clients_like_them_booked",
                "model_version": None,
                "source": "cooccurrence",
            }
        )
    return rows[:cap]


# ---------------------------------------------------------------------------
# 2. churn_risk_band
# ---------------------------------------------------------------------------
def _render_churn_risk_band(norm: dict[str, Any], cap: int) -> tuple[str, dict[str, Any]]:
    # Audience-safe: quarantined persons are EXCLUDED (either property
    # spelling counts). Consent-purpose summary attached per person.
    cypher = (
        "MATCH (p:Person)\n"
        "WHERE p.tenant_id = $tenant_id\n"
        "  AND p.propensity_churn >= $min_score\n"
        "  AND coalesce(p.quarantine, false) = false\n"
        "OPTIONAL MATCH (p)-[ce:CONSENTED]->(c:Consent)\n"
        "WHERE c.tenant_id = $tenant_id AND ce.purpose = c.purpose\n"
        "  AND c.revoked_at IS NULL\n"
        "RETURN p.person_id AS person_id, p.name AS name,\n"
        "       p.propensity_churn AS propensity_churn,\n"
        "       p.scored_at AS scored_at, p.model_version AS model_version,\n"
        "       collect(DISTINCT c.purpose) AS consent_purposes\n"
        "ORDER BY propensity_churn DESC\n"
        f"LIMIT {int(cap)}"
    )
    return cypher, {"min_score": norm["min_score"]}


def _eval_churn_risk_band(view: GraphView, norm, tenant_id, cap):
    rows = []
    for p in view.nodes_with("Person", tenant_id):
        if _is_quarantined(p):
            continue  # gate: quarantined persons are audience-ineligible
        raw = p.props.get("propensity_churn")
        if raw is None or isinstance(raw, bool):
            continue
        try:
            churn = float(raw)
        except (TypeError, ValueError):
            continue
        if churn < norm["min_score"]:
            continue
        rows.append(
            {
                "person_id": p.props.get("person_id"),
                "name": p.props.get("name"),
                "propensity_churn": churn,
                "scored_at": p.props.get("scored_at"),
                "model_version": p.props.get("model_version"),
                "consent_purposes": _consent_purposes(view, p, tenant_id),
            }
        )
    rows.sort(key=lambda r: (-r["propensity_churn"], r["person_id"] or ""))
    return rows[:cap]


# ---------------------------------------------------------------------------
# 3. referral_value
# ---------------------------------------------------------------------------
def _render_referral_value(norm: dict[str, Any], cap: int) -> tuple[str, dict[str, Any]]:
    # person_id optional: bound param is NULL for the tenant leaderboard.
    cypher = (
        "MATCH (p:Person)\n"
        "WHERE p.tenant_id = $tenant_id\n"
        "  AND ($person_id IS NULL OR p.person_id = $person_id)\n"
        "OPTIONAL MATCH (p)-[:REFERRED]->(q:Person)\n"
        "WHERE q.tenant_id = $tenant_id\n"
        "WITH p, collect(DISTINCT q) AS referees\n"
        "RETURN p.person_id AS person_id, p.name AS name,\n"
        "       size([q IN referees WHERE q IS NOT NULL]) AS referral_out_degree,\n"
        "       size([q IN referees WHERE q IS NOT NULL AND EXISTS {\n"
        "           (q)-[:BOOKED]->(bq:Booking) WHERE bq.tenant_id = $tenant_id\n"
        "       }]) AS converted_referees\n"
        "ORDER BY converted_referees DESC, referral_out_degree DESC, person_id ASC\n"
        f"LIMIT {int(cap)}"
    )
    return cypher, {"person_id": norm.get("person_id")}


def _eval_referral_value(view: GraphView, norm, tenant_id, cap):
    wanted = norm.get("person_id")
    rows = []
    for p in view.nodes_with("Person", tenant_id):
        if wanted is not None and p.props.get("person_id") != wanted:
            continue
        out_degree = 0
        converted = 0
        for edge in view.edges_from(p.node_id, "REFERRED"):
            q = view.node_by_id(edge.dst)
            if q is None or "Person" not in q.labels:
                continue
            if q.props.get("tenant_id") != tenant_id:
                continue
            out_degree += 1
            has_booking = any(
                (b := view.node_by_id(be.dst)) is not None
                and "Booking" in b.labels
                and b.props.get("tenant_id") == tenant_id
                for be in view.edges_from(q.node_id, "BOOKED")
            )
            if has_booking:
                converted += 1
        rows.append(
            {
                "person_id": p.props.get("person_id"),
                "name": p.props.get("name"),
                "referral_out_degree": out_degree,
                "converted_referees": converted,
            }
        )
    rows.sort(
        key=lambda r: (
            -r["converted_referees"],
            -r["referral_out_degree"],
            r["person_id"] or "",
        )
    )
    return rows[:cap]


# ---------------------------------------------------------------------------
# 4. similar_persons
# ---------------------------------------------------------------------------
def _render_similar_persons(norm: dict[str, Any], cap: int) -> tuple[str, dict[str, Any]]:
    # Cosine over stored Ollama embeddings (graph-sync: Person.name_embedding).
    cypher = (
        "MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})\n"
        f"WHERE p.{EMBEDDING_PROP} IS NOT NULL\n"
        "MATCH (q:Person)\n"
        f"WHERE q.tenant_id = $tenant_id AND q.person_id <> $person_id\n"
        f"  AND q.{EMBEDDING_PROP} IS NOT NULL\n"
        f"WITH p, q,\n"
        f"     reduce(dot = 0.0, i IN range(0, size(p.{EMBEDDING_PROP}) - 1) |\n"
        f"         dot + p.{EMBEDDING_PROP}[i] * q.{EMBEDDING_PROP}[i]) AS dot,\n"
        f"     reduce(na = 0.0, x IN p.{EMBEDDING_PROP} | na + x * x) AS na,\n"
        f"     reduce(nb = 0.0, x IN q.{EMBEDDING_PROP} | nb + x * x) AS nb\n"
        "WITH q, dot, na, nb\n"
        "WHERE na > 0 AND nb > 0\n"
        "RETURN q.person_id AS person_id, q.name AS name,\n"
        "       dot / (sqrt(na) * sqrt(nb)) AS similarity\n"
        "ORDER BY similarity DESC\n"
        f"LIMIT {int(norm['k'])}"
    )
    return cypher, {"person_id": norm["person_id"]}


def _cosine(a: list[float], b: list[float]) -> float | None:
    if not a or not b or len(a) != len(b):
        return None
    dot = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(y * y for y in b))
    if na == 0 or nb == 0:
        return None
    return dot / (na * nb)


def _embedding(props: dict[str, Any]) -> list[float] | None:
    raw = props.get(EMBEDDING_PROP)
    if not isinstance(raw, (list, tuple)) or not raw:
        return None
    try:
        return [float(x) for x in raw]
    except (TypeError, ValueError):
        return None


def _eval_similar_persons(view: GraphView, norm, tenant_id, cap):
    target = _find_person(view, norm["person_id"], tenant_id)
    if target is None:
        return []
    target_vec = _embedding(target.props)
    if target_vec is None:
        return []
    rows = []
    for q in view.nodes_with("Person", tenant_id):
        if q.node_id == target.node_id:
            continue  # self excluded
        vec = _embedding(q.props)
        if vec is None:
            continue
        sim = _cosine(target_vec, vec)
        if sim is None:
            continue
        rows.append(
            {
                "person_id": q.props.get("person_id"),
                "name": q.props.get("name"),
                "similarity": sim,
            }
        )
    rows.sort(key=lambda r: (-r["similarity"], r["person_id"] or ""))
    return rows[: min(norm["k"], cap)]


# ---------------------------------------------------------------------------
# registry fragment (merged into TEMPLATES by the package __init__)
# ---------------------------------------------------------------------------
PREDICTIVE_TEMPLATES: dict[str, Template] = {
    t.name: t
    for t in [
        Template(
            name="next_best_services",
            description=(
                "Ranked RECOMMENDED_FOR offerings for a person; falls back to "
                "same-tenant co-occurrence (persons like them booked) when no "
                "recommendation edges exist."
            ),
            params_doc={"person_id": "string, required"},
            normalize=lambda p, now: {"person_id": _norm_person_id(_req(p, "person_id"))},
            render=_render_next_best_services,
            evaluate=_eval_next_best_services,
        ),
        Template(
            name="churn_risk_band",
            description=(
                "Persons with propensity_churn >= min_score (default 0.7), "
                "quarantined excluded, with consent-purpose summary."
            ),
            params_doc={"min_score": "float 0..1, optional (default 0.7)"},
            normalize=lambda p, now: {
                "min_score": _norm_probability(p.get("min_score", 0.7), "min_score")
            },
            render=_render_churn_risk_band,
            evaluate=_eval_churn_risk_band,
        ),
        Template(
            name="referral_value",
            description=(
                "Referral out-degree and converted-referee count per person, "
                "ranked; tenant leaderboard when person_id is omitted."
            ),
            params_doc={"person_id": "string, optional (leaderboard when omitted)"},
            normalize=lambda p, now: _norm_opt_person_id(p),
            render=_render_referral_value,
            evaluate=_eval_referral_value,
        ),
        Template(
            name="similar_persons",
            description=(
                "Top-k most similar persons by cosine over stored Ollama "
                "name embeddings; self excluded, tenant-scoped."
            ),
            params_doc={
                "person_id": "string, required",
                "k": "integer 1..100, optional (default 10)",
            },
            normalize=lambda p, now: {
                "person_id": _norm_person_id(_req(p, "person_id")),
                "k": _norm_k(p.get("k", 10)),
            },
            render=_render_similar_persons,
            evaluate=_eval_similar_persons,
        ),
    ]
}
