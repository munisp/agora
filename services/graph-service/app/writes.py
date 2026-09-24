"""Structured write path (SPEC-W29 §3 WS-B, SPEC-W30 §4 WS-C).

Reads in this service go through CompiledQuery (cypher + plan); writes now
get the same treatment: every mutation is a ``CompiledWrite`` carrying BOTH

* ``cypher`` + ``params`` — canonical parameterized openCypher for FalkorDB
  (values always bound, never interpolated), plus an optional
  ``check_cypher`` that returns the target node's current tenant so the
  backend can reject cross-tenant write-back BEFORE any MERGE runs; and
* ``plan`` — the structured semantics the in-memory backend applies, so the
  pytest suite exercises identical behavior without a live graph DB.

Write plans are produced only by server-side compiler functions; no client
input ever becomes Cypher text.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

# Score properties the internal write-back API may set on Person nodes
# (SPEC-W29 §2 + SPEC-W30 §2 risk head). Anything else is rejected upstream
# by the request models; this tuple is the defense-in-depth allowlist.
SCORE_WRITE_FIELDS: tuple[str, ...] = (
    "propensity_churn",
    "propensity_convert",
    "propensity_turnout",
    "risk_score",
)


class CrossTenantWriteError(Exception):
    """Target node exists under a different tenant (mapped to 422)."""


class WriteTargetMissing(Exception):
    """Required write endpoint node does not exist (mapped to 404/skip)."""


# ---------------------------------------------------------------------------
# write plans
# ---------------------------------------------------------------------------
@dataclass(frozen=True)
class ScoreWritePlan:
    """MERGE-overwrite predictive scores on one Person (keep latest)."""

    person_id: str
    scores: dict[str, float]
    model_version: str
    scored_at: str


@dataclass(frozen=True)
class RecommendationWritePlan:
    """MERGE one (Person)-[:RECOMMENDED_FOR]->(Offering) edge; both endpoints
    must exist and belong to the writing tenant."""

    person_id: str
    offering_id: str
    score: float
    rank: int
    reason: str
    model_version: str
    scored_at: str


@dataclass(frozen=True)
class ScoreBatchWritePlan:
    """W46-F P12: one batch of ScoreWritePlan items (single batched write
    per request instead of N per-item writes)."""

    items: tuple[ScoreWritePlan, ...]


@dataclass(frozen=True)
class RecommendationBatchWritePlan:
    """W46-F P12: one batch of RecommendationWritePlan items."""

    items: tuple[RecommendationWritePlan, ...]


@dataclass(frozen=True)
class AlertResolvePlan:
    """Resolve one Alert; on ``dismissed`` clear the flagged person's
    quarantine ONLY when no other open high-severity alert flags them."""

    alert_id: str
    decision: str  # "confirmed" | "dismissed"
    reason: str
    resolved_by: str
    resolved_at: str


@dataclass(frozen=True)
class FixtureNode:
    """One node a fixture builder creates. ``id_label``/``id_prop`` name the
    server-side match key used to attach edges on the FalkorDB path."""

    key: str
    labels: tuple[str, ...]
    props: dict[str, Any]
    id_label: str
    id_prop: str


@dataclass(frozen=True)
class FixtureEdge:
    src_key: str
    dst_key: str
    type: str
    props: dict[str, Any] = field(default_factory=dict)


@dataclass(frozen=True)
class FixtureSeedPlan:
    """Dev/e2e fixture graph shape (server-side allowlisted scenario)."""

    scenario: str
    nodes: tuple[FixtureNode, ...]
    edges: tuple[FixtureEdge, ...]


WritePlan = ScoreWritePlan | RecommendationWritePlan | AlertResolvePlan | FixtureSeedPlan


@dataclass(frozen=True)
class CompiledBatchWrite:
    """W46-F P12: batched predictive write-back (single UNWIND round trip).

    * ``check_cypher`` + ``check_params`` — ONE pre-MERGE tenant
      verification over every referenced node id (any row under a different
      tenant aborts the batch with CrossTenantWriteError → HTTP 422,
      identical to the per-item path).
    * ``statements`` — grouped UNWIND writes (items are grouped by their
      score-field set, so a homogeneous batch is exactly one statement);
      each RETURNS the matched ids so the caller diffs input vs returned to
      recover the per-item skip semantics (unknown nodes are skipped +
      counted, never stub-created — verification gate WARN #4).
    * ``plan`` — the structured semantics the in-memory backend applies.
    """

    check_cypher: str
    check_params: dict[str, Any]
    statements: tuple[tuple[str, dict[str, Any]], ...]
    plan: ScoreBatchWritePlan | RecommendationBatchWritePlan


@dataclass(frozen=True)
class CompiledWrite:
    cypher: str
    params: dict[str, Any]
    plan: WritePlan
    # Pre-MERGE tenant verification: returns rows with a ``tenant_id``
    # column for the target node(s); any row under a different tenant
    # aborts the write with CrossTenantWriteError.
    check_cypher: str | None = None
    check_params: dict[str, Any] = field(default_factory=dict)
    # Self-guarding conditional follow-up (e.g. unquarantine only when no
    # other open high alerts); runs after the main statement on FalkorDB.
    followup_cypher: str | None = None
    followup_params: dict[str, Any] = field(default_factory=dict)
    # Extra parameterized statements (fixture seeding), executed in order.
    statements: tuple[tuple[str, dict[str, Any]], ...] = ()
    # When true, the main statement returning zero rows means the target
    # node was missing -> WriteTargetMissing.
    require_rows: bool = False


# ---------------------------------------------------------------------------
# compilers (server-side only; every value is a bound parameter)
# ---------------------------------------------------------------------------
def compile_score_write(
    *,
    person_id: str,
    scores: dict[str, float],
    model_version: str,
    scored_at: str,
) -> CompiledWrite:
    unknown = set(scores) - set(SCORE_WRITE_FIELDS)
    if unknown:
        raise ValueError(f"unknown score fields: {sorted(unknown)}")
    set_clauses = []
    params: dict[str, Any] = {"person_id": person_id}
    for i, (name, value) in enumerate(sorted(scores.items())):
        pname = f"sv{i}"
        params[pname] = float(value)
        set_clauses.append(f"p.{name} = ${pname}")
    set_clauses.append("p.model_version = $model_version")
    set_clauses.append("p.scored_at = $scored_at")
    params["model_version"] = model_version
    params["scored_at"] = scored_at
    # MATCH, not MERGE: scoring a person the graph doesn't know must NOT
    # create a bare stub Person (verification gate WARN #4). Zero rows ->
    # WriteTargetMissing -> the caller skips + counts the item.
    cypher = (
        "MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})\n"
        f"SET {', '.join(set_clauses)}\n"
        "RETURN p.person_id AS person_id"
    )
    check = (
        "MATCH (p:Person {person_id: $person_id})\n"
        "RETURN p.tenant_id AS tenant_id"
    )
    return CompiledWrite(
        cypher=cypher,
        params=params,
        plan=ScoreWritePlan(
            person_id=person_id,
            scores=dict(scores),
            model_version=model_version,
            scored_at=scored_at,
        ),
        check_cypher=check,
        check_params={"person_id": person_id},
        require_rows=True,
    )


def compile_recommendation_write(
    *,
    person_id: str,
    offering_id: str,
    score: float,
    rank: int,
    reason: str,
    model_version: str,
    scored_at: str,
) -> CompiledWrite:
    # MATCH-before-MERGE: the edge can only be created between two nodes
    # that already belong to the writing tenant — cross-tenant write-back is
    # impossible by construction, and the check query rejects it explicitly.
    cypher = (
        "MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})\n"
        "MATCH (o:Offering {tenant_id: $tenant_id, offering_id: $offering_id})\n"
        "MERGE (p)-[r:RECOMMENDED_FOR]->(o)\n"
        "SET r.score = $score, r.rank = $rank, r.reason = $reason,\n"
        "    r.model_version = $model_version, r.scored_at = $scored_at\n"
        "RETURN p.person_id AS person_id, o.offering_id AS offering_id"
    )
    params = {
        "person_id": person_id,
        "offering_id": offering_id,
        "score": float(score),
        "rank": int(rank),
        "reason": reason,
        "model_version": model_version,
        "scored_at": scored_at,
    }
    check = (
        "MATCH (n)\n"
        "WHERE (n:Person AND n.person_id = $person_id)\n"
        "   OR (n:Offering AND n.offering_id = $offering_id)\n"
        "RETURN DISTINCT n.tenant_id AS tenant_id"
    )
    return CompiledWrite(
        cypher=cypher,
        params=params,
        plan=RecommendationWritePlan(
            person_id=person_id,
            offering_id=offering_id,
            score=float(score),
            rank=int(rank),
            reason=reason,
            model_version=model_version,
            scored_at=scored_at,
        ),
        check_cypher=check,
        check_params={"person_id": person_id, "offering_id": offering_id},
        require_rows=True,
    )


def compile_score_batch_write(items: list[ScoreWritePlan]) -> CompiledBatchWrite:
    """W46-F P12: all score items in ONE check + one UNWIND write per
    distinct score-field set (homogeneous batches = a single statement).

    Per-item semantics are preserved via the RETURNING diff: the MATCH is
    tenant-scoped, so unknown persons match nothing and the caller counts
    them as skipped_unknown exactly like the per-item WriteTargetMissing
    path; cross-tenant nodes are rejected by the pre-check before any SET.
    """
    if not items:
        raise ValueError("score batch must not be empty")
    person_ids = [item.person_id for item in items]
    check = (
        "UNWIND $person_ids AS pid\n"
        "MATCH (p:Person {person_id: pid})\n"
        "RETURN p.person_id AS person_id, p.tenant_id AS tenant_id"
    )
    groups: dict[tuple[str, ...], list[dict[str, Any]]] = {}
    for item in items:
        unknown = set(item.scores) - set(SCORE_WRITE_FIELDS)
        if unknown:
            raise ValueError(f"unknown score fields: {sorted(unknown)}")
        key = tuple(sorted(item.scores))
        groups.setdefault(key, []).append(
            {
                "person_id": item.person_id,
                **{name: float(value) for name, value in item.scores.items()},
                "model_version": item.model_version,
                "scored_at": item.scored_at,
            }
        )
    statements: list[tuple[str, dict[str, Any]]] = []
    for fields, rows in groups.items():
        set_clauses = [f"p.{name} = row.{name}" for name in fields]
        set_clauses.append("p.model_version = row.model_version")
        set_clauses.append("p.scored_at = row.scored_at")
        cypher = (
            "UNWIND $rows AS row\n"
            "MATCH (p:Person {tenant_id: $tenant_id, person_id: row.person_id})\n"
            f"SET {', '.join(set_clauses)}\n"
            "RETURN row.person_id AS person_id"
        )
        statements.append((cypher, {"rows": rows}))
    return CompiledBatchWrite(
        check_cypher=check,
        check_params={"person_ids": person_ids},
        statements=tuple(statements),
        plan=ScoreBatchWritePlan(items=tuple(items)),
    )


def compile_recommendation_batch_write(
    items: list[RecommendationWritePlan],
) -> CompiledBatchWrite:
    """W46-F P12: all recommendation items in ONE check + ONE UNWIND write.

    RETURNING carries the matched (person_id, offering_id) pairs so the
    caller diffs input vs returned for the per-item skip list (missing
    endpoints are skipped, never created) — identical response semantics to
    the per-item WriteTargetMissing path.
    """
    if not items:
        raise ValueError("recommendation batch must not be empty")
    ids = [item.person_id for item in items] + [item.offering_id for item in items]
    check = (
        "UNWIND $ids AS xid\n"
        "MATCH (n)\n"
        "WHERE (n:Person AND n.person_id = xid)\n"
        "   OR (n:Offering AND n.offering_id = xid)\n"
        "RETURN DISTINCT n.tenant_id AS tenant_id"
    )
    cypher = (
        "UNWIND $rows AS row\n"
        "MATCH (p:Person {tenant_id: $tenant_id, person_id: row.person_id})\n"
        "MATCH (o:Offering {tenant_id: $tenant_id, offering_id: row.offering_id})\n"
        "MERGE (p)-[r:RECOMMENDED_FOR]->(o)\n"
        "SET r.score = row.score, r.rank = row.rank, r.reason = row.reason,\n"
        "    r.model_version = row.model_version, r.scored_at = row.scored_at\n"
        "RETURN row.person_id AS person_id, row.offering_id AS offering_id"
    )
    rows = [
        {
            "person_id": item.person_id,
            "offering_id": item.offering_id,
            "score": float(item.score),
            "rank": int(item.rank),
            "reason": item.reason,
            "model_version": item.model_version,
            "scored_at": item.scored_at,
        }
        for item in items
    ]
    return CompiledBatchWrite(
        check_cypher=check,
        check_params={"ids": ids},
        statements=((cypher, {"rows": rows}),),
        plan=RecommendationBatchWritePlan(items=tuple(items)),
    )


def compile_alert_resolve_write(
    *,
    alert_id: str,
    decision: str,
    reason: str,
    resolved_by: str,
    resolved_at: str,
) -> CompiledWrite:
    cypher = (
        "MATCH (a:Alert {tenant_id: $tenant_id, alert_id: $alert_id})\n"
        "SET a.status = $decision, a.resolved_at = $resolved_at,\n"
        "    a.resolved_by = $resolved_by, a.resolve_reason = $reason\n"
        "RETURN a.alert_id AS alert_id, a.person_id AS person_id, a.type AS type,\n"
        "       a.severity AS severity, a.status AS status"
    )
    params = {
        "alert_id": alert_id,
        "decision": decision,
        "reason": reason,
        "resolved_by": resolved_by,
        "resolved_at": resolved_at,
    }
    followup = None
    if decision == "dismissed":
        # Self-guarding: clears quarantine ONLY when no OTHER open
        # high-severity alert still flags this person (SPEC-W30 §4 WS-C).
        followup = (
            "MATCH (a:Alert {tenant_id: $tenant_id, alert_id: $alert_id})\n"
            "MATCH (p:Person {tenant_id: $tenant_id, person_id: a.person_id})\n"
            "WHERE NOT EXISTS {\n"
            "  MATCH (o:Alert)-[:FLAGGED]->(p)\n"
            "  WHERE o.tenant_id = $tenant_id AND o.alert_id <> $alert_id\n"
            "    AND o.status = 'open' AND o.severity = 'high'\n"
            "}\n"
            "SET p.quarantine = false\n"
            "RETURN p.person_id AS person_id"
        )
    return CompiledWrite(
        cypher=cypher,
        params=params,
        plan=AlertResolvePlan(
            alert_id=alert_id,
            decision=decision,
            reason=reason,
            resolved_by=resolved_by,
            resolved_at=resolved_at,
        ),
        followup_cypher=followup,
        followup_params={"alert_id": alert_id},
        require_rows=True,
    )
