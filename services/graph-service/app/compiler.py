"""Segment DSL -> compiled Cypher (SPEC-W28 §4 WS-B).

The compiler ALWAYS emits, in order:

1. ``p.tenant_id = $tenant_id`` — tenant isolation at the query layer
   (compliance gate 1); the value is a bound parameter, never interpolated.
2. quarantine exclusion — ``coalesce(p.quarantine, false) = false``
   (compliance gate 4: quarantined persons are audience-ineligible).
3. purpose-matching consent predicate — an unrevoked CONSENTED edge whose
   purpose matches (compliance gate 2). Cannot be removed via the DSL.

Optional DSL clauses narrow further (lapsed bookings, LGA, messaging recency).
"""

from __future__ import annotations

from datetime import datetime, time, timedelta, timezone

from .dsl import SegmentCreate
from .plans import CompiledQuery, PersonFilterPlan


def _as_utc(d) -> datetime:
    return datetime.combine(d, time.min, tzinfo=timezone.utc)


def compile_segment_query(
    segment: SegmentCreate,
    *,
    projection: str,  # "ids" | "count"
    now: datetime | None = None,
    row_cap: int = 10000,
) -> CompiledQuery:
    """Compile the segment DSL into a tenant-scoped, consent-gated query.

    ``now`` is injectable for deterministic tests; ``tenant_id`` is bound at
    execution time by the backend (``$tenant_id`` parameter)."""
    if projection not in ("ids", "count"):
        raise ValueError(f"unknown projection {projection!r}")
    now = now or datetime.now(timezone.utc)
    f = segment.filter
    consent_purpose = segment.consent_purpose

    params: dict[str, object] = {"consent_purpose": consent_purpose}
    conditions: list[str] = []

    # Mandatory consent gate (purpose-matching, unrevoked) — by construction.
    conditions.append(
        "EXISTS {\n"
        "    MATCH (p)-[ce:CONSENTED]->(c:Consent)\n"
        "    WHERE ce.purpose = $consent_purpose\n"
        "      AND c.tenant_id = $tenant_id\n"
        "      AND c.purpose = $consent_purpose\n"
        "      AND c.revoked_at IS NULL\n"
        "  }"
    )

    plan_kwargs: dict[str, object] = {"consent_purpose": consent_purpose}

    if f.last_booking_before is not None:
        # Lapsed customers: booked at least once, nothing since the cutoff.
        cutoff = _as_utc(f.last_booking_before)
        params["last_booking_before"] = cutoff.isoformat()
        conditions.append(
            "EXISTS {\n"
            "    MATCH (p)-[:BOOKED]->(b0:Booking)\n"
            "    WHERE b0.tenant_id = $tenant_id\n"
            "  }"
        )
        conditions.append(
            "NOT EXISTS {\n"
            "    MATCH (p)-[:BOOKED]->(b:Booking)\n"
            "    WHERE b.tenant_id = $tenant_id\n"
            "      AND b.created_at >= $last_booking_before\n"
            "  }"
        )
        plan_kwargs["last_booking_before"] = cutoff

    if f.lga:
        params["lga"] = f.lga
        conditions.append(
            "EXISTS {\n"
            "    MATCH (p)-[:HAS_CONTACT]->(:Contact)-[:CAPTURED_AT]->(l:Location)\n"
            "    WHERE l.tenant_id = $tenant_id\n"
            "      AND l.lga = $lga\n"
            "  }"
        )
        plan_kwargs["lga"] = f.lga

    if f.not_messaged_since_days is not None:
        since = now - timedelta(days=f.not_messaged_since_days)
        params["not_messaged_since"] = since.isoformat()
        conditions.append(
            "NOT EXISTS {\n"
            "    MATCH (p)-[m:MESSAGED]->(:Campaign)\n"
            "    WHERE m.at >= $not_messaged_since\n"
            "  }"
        )
        plan_kwargs["not_messaged_since"] = since

    where = "\n  AND ".join(conditions)
    if projection == "count":
        cypher = (
            "MATCH (p:Person)\n"
            "WHERE p.tenant_id = $tenant_id\n"
            "  AND coalesce(p.quarantine, false) = false\n"
            f"  AND {where}\n"
            "RETURN count(p) AS count"
        )
    else:
        # Audience member shape (orchestrator contract):
        # {person_id, phone_hash, lead_id}. lead_id is resolved from the
        # person's HAS_CONTACT edge -> Contact.lead_id; the MOST RECENT
        # Contact (by captured_at) with a non-null lead_id wins; null when
        # the person has no such Contact. The graph stays raw-PII-free —
        # phone resolution happens downstream in notification-worker.
        cypher = (
            "MATCH (p:Person)\n"
            "WHERE p.tenant_id = $tenant_id\n"
            "  AND coalesce(p.quarantine, false) = false\n"
            f"  AND {where}\n"
            "OPTIONAL MATCH (p)-[:HAS_CONTACT]->(ct:Contact)\n"
            "WHERE ct.tenant_id = $tenant_id AND ct.lead_id IS NOT NULL\n"
            "WITH p, ct ORDER BY ct.captured_at DESC\n"
            "WITH p, head(collect(ct.lead_id)) AS lead_id\n"
            "RETURN p.person_id AS person_id, p.phone_hash AS phone_hash, lead_id\n"
            "ORDER BY person_id\n"
            f"LIMIT {int(row_cap)}"
        )
    plan = PersonFilterPlan(projection=projection, **plan_kwargs)  # type: ignore[arg-type]
    return CompiledQuery(cypher=cypher, params=params, plan=plan)
