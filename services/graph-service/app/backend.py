"""Graph backends (SPEC-W28 §4 WS-B).

``GraphBackend`` is the seam between the API layer and the graph store:

* ``FalkorBackend`` — production: runs the CompiledQuery's canonical Cypher
  with bound parameters against FalkorDB (graph name from
  ``FALKORDB_GRAPH``, default ``agora_tenants``). The ``falkordb`` driver is
  imported lazily so tests and dev tiers need no live graph DB.
* ``InMemoryBackend`` — dev/test: a property-graph in memory that evaluates
  the CompiledQuery's structured ``plan`` with the SAME semantics the
  Cypher encodes (consent gate, quarantine exclusion, tenant scope).

Tenant isolation is enforced at the query layer: ``execute`` takes the
authenticated ``tenant_id`` and (a) binds it as ``$tenant_id`` on FalkorDB,
(b) scopes every evaluation to nodes carrying that tenant_id in memory.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from typing import Any, Protocol

from .plans import (
    CompiledQuery,
    PersonFilterPlan,
    ScoreFilterSpec,
    TemplatePlan,
    parse_instant,
)
from .templates import TEMPLATES, GraphView, has_valid_consent
from .writes import (
    AlertResolvePlan,
    CompiledWrite,
    CrossTenantWriteError,
    FixtureSeedPlan,
    RecommendationWritePlan,
    ScoreWritePlan,
    WriteTargetMissing,
)


def _score_matches(raw: Any, spec: ScoreFilterSpec) -> bool:
    """Mirror of the compiled Cypher's comparison semantics: a missing or
    non-numeric score never matches (null comparisons are false)."""
    if raw is None or isinstance(raw, bool):
        return False
    try:
        value = float(raw)
    except (TypeError, ValueError):
        return False
    if spec.op == ">=":
        return value >= spec.lo
    if spec.op == "<=":
        return value <= spec.lo
    return spec.hi is not None and spec.lo <= value <= spec.hi


class GraphError(Exception):
    """Graph store failure (mapped to 502 by the API layer)."""


class GraphBackend(Protocol):
    async def execute(self, query: CompiledQuery, tenant_id: str) -> list[dict[str, Any]]: ...

    async def ping(self) -> bool: ...

    async def close(self) -> None: ...


# ---------------------------------------------------------------------------
# FalkorDB (production)
# ---------------------------------------------------------------------------
class FalkorBackend:
    def __init__(
        self,
        host: str,
        port: int,
        graph_name: str,
        username: str = "",
        password: str = "",
    ) -> None:
        try:
            from falkordb import FalkorDB  # lazy: driver not needed in tests
        except ImportError as exc:
            raise GraphError("falkordb package not installed") from exc
        kwargs: dict[str, Any] = {"host": host, "port": port}
        if username:
            kwargs["username"] = username
        if password:
            kwargs["password"] = password
        self._db = FalkorDB(**kwargs)
        self._graph = self._db.select_graph(graph_name)
        self.graph_name = graph_name

    async def execute(self, query: CompiledQuery, tenant_id: str) -> list[dict[str, Any]]:
        params = {**query.params, "tenant_id": tenant_id}

        def _run() -> list[dict[str, Any]]:
            result = self._graph.query(query.cypher, params)
            columns = [col[1] for col in result.header]
            return [dict(zip(columns, row)) for row in result.result_set]

        try:
            return await asyncio.to_thread(_run)
        except Exception as exc:  # noqa: BLE001 — driver/redis outage
            raise GraphError(f"falkordb query failed: {type(exc).__name__}: {exc}") from exc

    async def execute_write(self, write: CompiledWrite, tenant_id: str) -> dict[str, Any]:
        def _run() -> dict[str, Any]:
            if write.check_cypher:
                check_rows = self._query_rows(
                    write.check_cypher, {**write.check_params, "tenant_id": tenant_id}
                )
                for row in check_rows:
                    row_tenant = row.get("tenant_id")
                    if row_tenant is not None and row_tenant != tenant_id:
                        raise CrossTenantWriteError(
                            f"target node belongs to tenant {row_tenant!r}, "
                            f"refusing write from {tenant_id!r}"
                        )
            rows = self._query_rows(write.cypher, {**write.params, "tenant_id": tenant_id})
            if write.require_rows and not rows:
                raise WriteTargetMissing("write target node not found for tenant")
            followup_rows: list[dict[str, Any]] = []
            if write.followup_cypher:
                followup_rows = self._query_rows(
                    write.followup_cypher,
                    {**write.followup_params, "tenant_id": tenant_id},
                )
            for cypher, params in write.statements:
                self._graph.query(cypher, {**params, "tenant_id": tenant_id})
            return {"rows": rows, "followup_rows": followup_rows}

        try:
            return await asyncio.to_thread(_run)
        except (CrossTenantWriteError, WriteTargetMissing):
            raise
        except Exception as exc:  # noqa: BLE001 — driver/redis outage
            raise GraphError(f"falkordb write failed: {type(exc).__name__}: {exc}") from exc

    def _query_rows(self, cypher: str, params: dict[str, Any]) -> list[dict[str, Any]]:
        result = self._graph.query(cypher, params)
        columns = [col[1] for col in result.header]
        return [dict(zip(columns, row)) for row in result.result_set]

    async def ping(self) -> bool:
        try:
            await asyncio.to_thread(self._graph.query, "RETURN 1")
            return True
        except Exception:  # noqa: BLE001
            return False

    async def close(self) -> None:
        try:
            self._db.close()
        except Exception:  # noqa: BLE001
            pass


# ---------------------------------------------------------------------------
# In-memory (dev/test)
# ---------------------------------------------------------------------------
@dataclass
class GNode:
    node_id: str
    labels: frozenset[str]
    props: dict[str, Any]


@dataclass
class GEdge:
    src: str
    dst: str
    type: str
    props: dict[str, Any] = field(default_factory=dict)


class InMemoryGraph:
    """Minimal property graph matching SPEC-W28 §3 (nodes carry tenant_id)."""

    def __init__(self) -> None:
        self.nodes: dict[str, GNode] = {}
        self.edges: list[GEdge] = []

    def add_node(self, node_id: str, labels: set[str] | frozenset[str], **props: Any) -> GNode:
        node = GNode(node_id=node_id, labels=frozenset(labels), props=dict(props))
        self.nodes[node_id] = node
        return node

    def add_edge(self, src: str, dst: str, edge_type: str, **props: Any) -> GEdge:
        if src not in self.nodes or dst not in self.nodes:
            raise ValueError("edge endpoints must exist")
        edge = GEdge(src=src, dst=dst, type=edge_type, props=dict(props))
        self.edges.append(edge)
        return edge

    # --- GraphView protocol -------------------------------------------------
    def nodes_with(self, label: str, tenant_id: str) -> list[GNode]:
        return [
            n
            for n in self.nodes.values()
            if label in n.labels and n.props.get("tenant_id") == tenant_id
        ]

    def edges_from(self, node_id: str, edge_type: str | None = None) -> list[GEdge]:
        return [
            e
            for e in self.edges
            if e.src == node_id and (edge_type is None or e.type == edge_type)
        ]

    def edges_to(self, node_id: str, edge_type: str | None = None) -> list[GEdge]:
        return [
            e
            for e in self.edges
            if e.dst == node_id and (edge_type is None or e.type == edge_type)
        ]

    def node_by_id(self, node_id: str) -> GNode | None:
        return self.nodes.get(node_id)


class InMemoryBackend:
    """Evaluates CompiledQuery plans against an InMemoryGraph. The semantics
    mirror the compiled Cypher exactly (tenant scope, consent gate,
    quarantine exclusion); the Cypher text remains the production path."""

    def __init__(self, graph: InMemoryGraph | None = None) -> None:
        self.graph = graph or InMemoryGraph()

    async def execute(self, query: CompiledQuery, tenant_id: str) -> list[dict[str, Any]]:
        plan = query.plan
        if isinstance(plan, PersonFilterPlan):
            return self._eval_person_filter(plan, tenant_id)
        if isinstance(plan, TemplatePlan):
            template = TEMPLATES.get(plan.name)
            if template is None:
                raise GraphError(f"unknown template plan {plan.name!r}")
            # Evaluation is unbounded; callers cap via their CompiledQuery's
            # LIMIT — mirror that with a generous ceiling here.
            return template.evaluate(self.graph, plan.params, tenant_id, 10000)
        raise GraphError(f"unsupported plan {type(plan).__name__}")

    async def execute_write(self, write: CompiledWrite, tenant_id: str) -> dict[str, Any]:
        """Apply a CompiledWrite's plan with the SAME semantics the Cypher
        encodes (tenant verification before MERGE, conditional unquarantine)."""
        plan = write.plan
        if isinstance(plan, ScoreWritePlan):
            return self._apply_score_write(plan, tenant_id)
        if isinstance(plan, RecommendationWritePlan):
            return self._apply_recommendation_write(plan, tenant_id)
        if isinstance(plan, AlertResolvePlan):
            return self._apply_alert_resolve(plan, tenant_id)
        if isinstance(plan, FixtureSeedPlan):
            return self._apply_fixture_seed(plan, tenant_id)
        raise GraphError(f"unsupported write plan {type(plan).__name__}")

    # --- write plan semantics (mirror writes.py's Cypher) -------------------
    def _nodes_by_prop(self, label: str, prop: str, value: Any) -> list[GNode]:
        return [
            n
            for n in self.graph.nodes.values()
            if label in n.labels and n.props.get(prop) == value
        ]

    def _apply_score_write(self, plan: ScoreWritePlan, tenant_id: str) -> dict[str, Any]:
        existing = self._nodes_by_prop("Person", "person_id", plan.person_id)
        for node in existing:
            if node.props.get("tenant_id") != tenant_id:
                raise CrossTenantWriteError(
                    f"person {plan.person_id!r} belongs to tenant "
                    f"{node.props.get('tenant_id')!r}, refusing write from {tenant_id!r}"
                )
        if not existing:
            # MATCH-not-MERGE semantics: unknown persons are never created
            # as bare stubs (verification gate WARN #4).
            raise WriteTargetMissing(
                f"person {plan.person_id!r} not found for tenant {tenant_id!r}"
            )
        node = existing[0]
        node.props.update(plan.scores)
        node.props["model_version"] = plan.model_version
        node.props["scored_at"] = plan.scored_at
        return {"rows": [{"person_id": plan.person_id, "created": False}]}

    def _apply_recommendation_write(
        self, plan: RecommendationWritePlan, tenant_id: str
    ) -> dict[str, Any]:
        persons = self._nodes_by_prop("Person", "person_id", plan.person_id)
        offerings = self._nodes_by_prop("Offering", "offering_id", plan.offering_id)
        for node in persons + offerings:
            if node.props.get("tenant_id") != tenant_id:
                raise CrossTenantWriteError(
                    f"recommendation endpoint belongs to tenant "
                    f"{node.props.get('tenant_id')!r}, refusing write from {tenant_id!r}"
                )
        if not persons or not offerings:
            raise WriteTargetMissing(
                f"person {plan.person_id!r} or offering {plan.offering_id!r} "
                f"not found for tenant {tenant_id!r}"
            )
        person, offering = persons[0], offerings[0]
        props = {
            "score": plan.score,
            "rank": plan.rank,
            "reason": plan.reason,
            "model_version": plan.model_version,
            "scored_at": plan.scored_at,
        }
        for edge in self.graph.edges_from(person.node_id, "RECOMMENDED_FOR"):
            if edge.dst == offering.node_id:
                edge.props.update(props)  # MERGE-overwrite keeps the latest
                return {
                    "rows": [
                        {"person_id": plan.person_id, "offering_id": plan.offering_id}
                    ]
                }
        self.graph.add_edge(person.node_id, offering.node_id, "RECOMMENDED_FOR", **props)
        return {
            "rows": [{"person_id": plan.person_id, "offering_id": plan.offering_id}]
        }

    def _apply_alert_resolve(self, plan: AlertResolvePlan, tenant_id: str) -> dict[str, Any]:
        matches = self._nodes_by_prop("Alert", "alert_id", plan.alert_id)
        for node in matches:
            if node.props.get("tenant_id") != tenant_id:
                raise CrossTenantWriteError(
                    f"alert {plan.alert_id!r} belongs to tenant "
                    f"{node.props.get('tenant_id')!r}, refusing write from {tenant_id!r}"
                )
        if not matches:
            raise WriteTargetMissing(f"alert {plan.alert_id!r} not found")
        alert = matches[0]
        alert.props["status"] = plan.decision
        alert.props["resolved_at"] = plan.resolved_at
        alert.props["resolved_by"] = plan.resolved_by
        alert.props["resolve_reason"] = plan.reason

        unquarantined = False
        person_id = alert.props.get("person_id")
        if plan.decision == "dismissed" and person_id:
            persons = [
                p
                for p in self._nodes_by_prop("Person", "person_id", person_id)
                if p.props.get("tenant_id") == tenant_id
            ]
            person = persons[0] if persons else None
            if person is not None and not self._other_open_high_alerts(
                tenant_id, plan.alert_id, person
            ):
                person.props["quarantine"] = False  # canonical W28 property
                unquarantined = True
        return {
            "rows": [
                {
                    "alert_id": plan.alert_id,
                    "person_id": person_id,
                    "type": alert.props.get("type"),
                    "severity": alert.props.get("severity"),
                    "status": plan.decision,
                }
            ],
            "unquarantined": unquarantined,
        }

    def _other_open_high_alerts(
        self, tenant_id: str, resolved_alert_id: str, person: GNode
    ) -> bool:
        for alert in self.graph.nodes_with("Alert", tenant_id):
            if alert.props.get("alert_id") == resolved_alert_id:
                continue
            if alert.props.get("status") != "open":
                continue
            if alert.props.get("severity") != "high":
                continue
            # Flag linkage: FLAGGED edge to the person, or a person_id prop.
            if alert.props.get("person_id") == person.props.get("person_id"):
                return True
            if any(
                e.dst == person.node_id
                for e in self.graph.edges_from(alert.node_id, "FLAGGED")
            ):
                return True
        return False

    def _apply_fixture_seed(self, plan: FixtureSeedPlan, tenant_id: str) -> dict[str, Any]:
        key_to_node: dict[str, GNode] = {}
        for spec in plan.nodes:
            node_id = f"{tenant_id}:{spec.key}"
            props = {**spec.props, "tenant_id": tenant_id}
            existing = self.graph.nodes.get(node_id)
            if existing is not None:
                existing.props.update(props)
                key_to_node[spec.key] = existing
            else:
                key_to_node[spec.key] = self.graph.add_node(node_id, set(spec.labels), **props)
        for edge in plan.edges:
            src = key_to_node.get(edge.src_key) or self.graph.nodes.get(
                f"{tenant_id}:{edge.src_key}"
            )
            dst = key_to_node.get(edge.dst_key) or self.graph.nodes.get(
                f"{tenant_id}:{edge.dst_key}"
            )
            if src is None or dst is None:
                raise WriteTargetMissing(
                    f"fixture edge endpoint missing: {edge.src_key}->{edge.dst_key}"
                )
            self.graph.add_edge(src.node_id, dst.node_id, edge.type, **dict(edge.props))
        return {
            "rows": [],
            "nodes_written": len(plan.nodes),
            "edges_written": len(plan.edges),
        }

    async def ping(self) -> bool:
        return True

    async def close(self) -> None:
        return None

    # --- segment semantics (mirror compiler.py's Cypher) --------------------
    def _eval_person_filter(
        self, plan: PersonFilterPlan, tenant_id: str
    ) -> list[dict[str, Any]]:
        view: GraphView = self.graph
        matched: list[GNode] = []
        for person in view.nodes_with("Person", tenant_id):
            # Compliance gate 4: quarantined persons are never eligible.
            if person.props.get("quarantine"):
                continue
            # Compliance gate 2: purpose-matching unrevoked consent required.
            if not has_valid_consent(view, person, plan.consent_purpose, tenant_id):
                continue
            if plan.last_booking_before is not None:
                stamps = [
                    parse_instant(b.props.get("created_at"))
                    for e in view.edges_from(person.node_id, "BOOKED")
                    if (b := view.node_by_id(e.dst)) is not None
                    and "Booking" in b.labels
                    and b.props.get("tenant_id") == tenant_id
                    and b.props.get("created_at")
                ]
                # Lapsed: booked at least once, nothing since the cutoff.
                if not stamps or max(stamps) >= plan.last_booking_before:
                    continue
            if plan.lga is not None:
                lga_hit = False
                for he in view.edges_from(person.node_id, "HAS_CONTACT"):
                    contact = view.node_by_id(he.dst)
                    if contact is None or "Contact" not in contact.labels:
                        continue
                    for le in view.edges_from(contact.node_id, "CAPTURED_AT"):
                        loc = view.node_by_id(le.dst)
                        if (
                            loc is not None
                            and "Location" in loc.labels
                            and loc.props.get("tenant_id") == tenant_id
                            and loc.props.get("lga") == plan.lga
                        ):
                            lga_hit = True
                if not lga_hit:
                    continue
            if plan.not_messaged_since is not None:
                recent = any(
                    m.props.get("at")
                    and parse_instant(m.props["at"]) >= plan.not_messaged_since
                    for m in view.edges_from(person.node_id, "MESSAGED")
                )
                if recent:
                    continue
            # SPEC-W29 §3 WS-B: numeric score predicates. A person without a
            # stored score never matches (mirrors Cypher null semantics).
            if plan.score_filters:
                if not all(
                    _score_matches(person.props.get(spec.field), spec)
                    for spec in plan.score_filters
                ):
                    continue
            matched.append(person)

        if plan.projection == "count":
            return [{"count": len(matched)}]
        matched.sort(key=lambda p: p.props.get("person_id") or "")
        return [
            {
                "person_id": p.props.get("person_id"),
                "phone_hash": p.props.get("phone_hash"),
                "lead_id": self._resolve_lead_id(p, tenant_id),
            }
            for p in matched
        ]

    def _resolve_lead_id(self, person: GNode, tenant_id: str) -> Any:
        """lead_id of the person's MOST RECENT Contact (by captured_at) with
        a non-null lead_id; None when no such Contact exists (orchestrator
        contract for audience member shape)."""
        best: tuple[Any, Any] | None = None
        for he in self.graph.edges_from(person.node_id, "HAS_CONTACT"):
            contact = self.graph.node_by_id(he.dst)
            if contact is None or "Contact" not in contact.labels:
                continue
            if contact.props.get("tenant_id") != tenant_id:
                continue
            lead_id = contact.props.get("lead_id")
            if lead_id is None:
                continue
            raw = contact.props.get("captured_at")
            captured = parse_instant(raw) if raw else None
            # Contacts without captured_at sort oldest.
            key = captured or parse_instant("1970-01-01")
            if best is None or key > best[0]:
                best = (key, lead_id)
        return best[1] if best else None
