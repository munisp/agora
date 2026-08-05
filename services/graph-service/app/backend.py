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

from .plans import CompiledQuery, PersonFilterPlan, TemplatePlan, parse_instant
from .templates import TEMPLATES, GraphView, has_valid_consent


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
