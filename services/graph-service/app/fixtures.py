"""Dev/e2e fixture builders (SPEC-W30 WS-C addendum).

A FIXED server-side allowlist of graph shapes the e2e suites need and the
public APIs cannot create (e.g. backdated consent, impossible travel).
Clients pick a scenario and supply bounded integer/string params — never
query text. Every node/edge carries tenant_id (injected at write time by
the backend, which binds $tenant_id like everywhere else).

Each builder returns (FixtureSeedPlan, ids) where ids is the JSON-able
handle map the seeder endpoint returns to the caller.
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from typing import Any

from .writes import FixtureEdge, FixtureNode, FixtureSeedPlan

SCENARIOS: frozenset[str] = frozenset(
    {
        "small_tenant",
        "referral_ring",
        "backdated_consent",
        "impossible_travel",
        "capture_burst",
    }
)


class FixtureError(ValueError):
    """Unknown scenario or out-of-bounds params (mapped to 422)."""


def _int_param(params: dict[str, Any], name: str, default: int, lo: int, hi: int) -> int:
    raw = params.get(name, default)
    if isinstance(raw, bool) or not isinstance(raw, int):
        raise FixtureError(f"{name} must be an integer")
    if not lo <= raw <= hi:
        raise FixtureError(f"{name} out of range [{lo}, {hi}]")
    return raw


def _str_param(params: dict[str, Any], name: str, default: str) -> str:
    raw = params.get(name, default)
    if not isinstance(raw, str) or not raw.strip() or len(raw) > 100:
        raise FixtureError(f"{name} must be a non-empty string (<=100 chars)")
    return raw.strip()


def _person(key: str, name: str, **extra: Any) -> FixtureNode:
    return FixtureNode(
        key=key,
        labels=("Person",),
        props={"person_id": key, "name": name, "channels": ["whatsapp"], **extra},
        id_label="Person",
        id_prop="person_id",
    )


def _consent(key: str, purpose: str, granted_at: str) -> FixtureNode:
    return FixtureNode(
        key=key,
        labels=("Consent",),
        props={
            "consent_id": key,
            "purpose": purpose,
            "granted_at": granted_at,
            "revoked_at": None,
        },
        id_label="Consent",
        id_prop="consent_id",
    )


def _offering(key: str, name: str) -> FixtureNode:
    return FixtureNode(
        key=key,
        labels=("Offering",),
        props={"offering_id": key, "name": name},
        id_label="Offering",
        id_prop="offering_id",
    )


def _booking(key: str, created_at: str, status: str = "completed") -> FixtureNode:
    return FixtureNode(
        key=key,
        labels=("Booking",),
        props={"booking_id": key, "status": status, "created_at": created_at},
        id_label="Booking",
        id_prop="booking_id",
    )


def _contact(key: str, captured_at: str, agent: str | None = None) -> FixtureNode:
    props: dict[str, Any] = {
        "lead_id": key,
        "channel_of_first_touch": "field_pwa",
        "source": "fixture",
        "captured_at": captured_at,
    }
    if agent is not None:
        props["captured_by"] = agent
    return FixtureNode(
        key=key, labels=("Contact",), props=props, id_label="Contact", id_prop="lead_id"
    )


def _location(key: str, lga: str, lat: float, lon: float) -> FixtureNode:
    return FixtureNode(
        key=key,
        labels=("Location",),
        props={"location_id": key, "lga": lga, "lat": lat, "lon": lon},
        id_label="Location",
        id_prop="location_id",
    )


def _campaign(key: str, kind: str = "outreach") -> FixtureNode:
    return FixtureNode(
        key=key,
        labels=("Campaign",),
        props={"campaign_id": key, "kind": kind},
        id_label="Campaign",
        id_prop="campaign_id",
    )


# ---------------------------------------------------------------------------
# scenarios
# ---------------------------------------------------------------------------
def _small_tenant(params: dict[str, Any], now: datetime):
    persons = _int_param(params, "persons", 5, 1, 50)
    bookings = _int_param(params, "bookings", 8, 0, 200)
    offerings = _int_param(params, "offerings", 3, 1, 20)
    quarantine_last = bool(params.get("quarantine_last", False))
    nodes: list[FixtureNode] = []
    edges: list[FixtureEdge] = []
    granted = (now - timedelta(days=90)).isoformat()

    offering_keys = []
    for i in range(1, offerings + 1):
        key = f"fx-o{i}"
        nodes.append(_offering(key, f"Fixture Offering {i}"))
        offering_keys.append(key)

    person_keys = []
    for i in range(1, persons + 1):
        key = f"fx-p{i}"
        extra: dict[str, Any] = {}
        # Canonical W28 quarantine property (docs/graph.md §3.2).
        if quarantine_last and i == persons:
            extra["quarantine"] = True
        nodes.append(_person(key, f"Fixture Person {i}", **extra))
        person_keys.append(key)
        ckey = f"fx-c{i}"
        nodes.append(_consent(ckey, "marketing", granted))
        edges.append(
            FixtureEdge(key, ckey, "CONSENTED", {"purpose": "marketing", "at": granted})
        )
        ctkey = f"fx-lead{i}"
        nodes.append(_contact(ctkey, (now - timedelta(days=120)).isoformat()))
        edges.append(FixtureEdge(key, ctkey, "HAS_CONTACT"))

    for i in range(bookings):
        pkey = person_keys[i % persons]
        bkey = f"fx-b{i + 1}"
        created = (now - timedelta(days=10 + i)).isoformat()
        nodes.append(_booking(bkey, created))
        edges.append(FixtureEdge(pkey, bkey, "BOOKED", {"at": created}))
        edges.append(FixtureEdge(bkey, offering_keys[i % offerings], "FOR"))

    # A small referral chain so referral templates have something to chew on.
    for i in range(1, persons):
        edges.append(
            FixtureEdge(
                person_keys[i - 1],
                person_keys[i],
                "REFERRED",
                {"at": (now - timedelta(days=30 - i)).isoformat(), "program": "fx-ref"},
            )
        )
    return nodes, edges, {"person_ids": person_keys, "offering_ids": offering_keys}


def _referral_ring(params: dict[str, Any], now: datetime):
    size = _int_param(params, "size", 3, 3, 10)
    with_conversion = bool(params.get("with_conversion", False))
    consent_purpose = params.get("consent_purpose")
    if consent_purpose is not None:
        consent_purpose = _str_param(params, "consent_purpose", "marketing")
    nodes: list[FixtureNode] = []
    edges: list[FixtureEdge] = []
    person_keys = []
    for i in range(1, size + 1):
        key = f"fx-ring-p{i}"
        nodes.append(_person(key, f"Ring Person {i}"))
        person_keys.append(key)
        if consent_purpose:
            ckey = f"fx-ring-c{i}"
            granted = (now - timedelta(days=90)).isoformat()
            nodes.append(_consent(ckey, consent_purpose, granted))
            edges.append(
                FixtureEdge(key, ckey, "CONSENTED", {"purpose": consent_purpose, "at": granted})
            )
        if with_conversion:
            bkey = f"fx-ring-b{i}"
            created = (now - timedelta(days=5)).isoformat()
            nodes.append(_booking(bkey, created))
            edges.append(FixtureEdge(key, bkey, "BOOKED", {"at": created}))
    for i in range(size):
        edges.append(
            FixtureEdge(
                person_keys[i],
                person_keys[(i + 1) % size],
                "REFERRED",
                {"at": (now - timedelta(days=20)).isoformat(), "program": "fx-ring"},
            )
        )
    return nodes, edges, {"person_ids": person_keys}


def _backdated_consent(params: dict[str, Any], now: datetime):
    """One person MESSAGED at T with consent granted_at T+1h for the same
    purpose (F4 consent_backdating tripwire)."""
    messaged_at = now - timedelta(hours=2)
    granted_at = messaged_at + timedelta(hours=1)
    nodes = [
        _person("fx-bd-p1", "Backdated Consent"),
        _consent("fx-bd-c1", "marketing", granted_at.isoformat()),
        _campaign("fx-bd-camp1"),
    ]
    edges = [
        FixtureEdge(
            "fx-bd-p1",
            "fx-bd-c1",
            "CONSENTED",
            {
                "purpose": "marketing",
                "at": granted_at.isoformat(),
                "granted_at": granted_at.isoformat(),
            },
        ),
        FixtureEdge(
            "fx-bd-p1",
            "fx-bd-camp1",
            "MESSAGED",
            {
                "campaign_id": "fx-bd-camp1",
                "at": messaged_at.isoformat(),
                "status": "sent",
                "purpose": "marketing",
            },
        ),
    ]
    return nodes, edges, {"person_id": "fx-bd-p1"}


def _impossible_travel(params: dict[str, Any], now: datetime):
    """Two CAPTURED_AT captures by the same agent 10 minutes apart at
    locations ~150km apart (F4 geo_impossibility tripwire; Lagos vs Ibadan)."""
    agent = _str_param(params, "agent", "fx-agent-1")
    t0 = now - timedelta(hours=3)
    t1 = t0 + timedelta(minutes=10)
    nodes = [
        _person("fx-geo-p1", "Geo Capture One"),
        _person("fx-geo-p2", "Geo Capture Two"),
        _contact("fx-geo-lead1", t0.isoformat(), agent=agent),
        _contact("fx-geo-lead2", t1.isoformat(), agent=agent),
        _location("fx-geo-loc1", "Lagos Island", 6.4541, 3.3947),
        _location("fx-geo-loc2", "Ibadan North", 7.3775, 3.9470),
    ]
    edges = [
        FixtureEdge("fx-geo-p1", "fx-geo-lead1", "HAS_CONTACT"),
        FixtureEdge("fx-geo-p2", "fx-geo-lead2", "HAS_CONTACT"),
        FixtureEdge("fx-geo-lead1", "fx-geo-loc1", "CAPTURED_AT"),
        FixtureEdge("fx-geo-lead2", "fx-geo-loc2", "CAPTURED_AT"),
    ]
    return nodes, edges, {
        "agent": agent,
        "person_ids": ["fx-geo-p1", "fx-geo-p2"],
    }


def _capture_burst(params: dict[str, Any], now: datetime):
    """count captures by one agent inside a 30-minute window (F3 tripwire)."""
    agent = _str_param(params, "agent", "fx-agent-burst")
    count = _int_param(params, "count", 35, 1, 200)
    t0 = now - timedelta(hours=1)
    nodes: list[FixtureNode] = []
    edges: list[FixtureEdge] = []
    person_keys = []
    for i in range(1, count + 1):
        pkey = f"fx-burst-p{i}"
        ckey = f"fx-burst-lead{i}"
        captured = (t0 + timedelta(seconds=int(1800 * (i - 1) / max(count, 1)))).isoformat()
        nodes.append(_person(pkey, f"Burst Person {i}"))
        nodes.append(_contact(ckey, captured, agent=agent))
        edges.append(FixtureEdge(pkey, ckey, "HAS_CONTACT"))
        person_keys.append(pkey)
    return nodes, edges, {"agent": agent, "person_ids": person_keys}


_BUILDERS = {
    "small_tenant": _small_tenant,
    "referral_ring": _referral_ring,
    "backdated_consent": _backdated_consent,
    "impossible_travel": _impossible_travel,
    "capture_burst": _capture_burst,
}


def build_fixture(
    scenario: str, params: dict[str, Any], now: datetime | None = None
) -> tuple[FixtureSeedPlan, dict[str, Any]]:
    """Compile a fixture scenario into a seed plan + caller-visible ids.
    Raises FixtureError on unknown scenario / bad params."""
    builder = _BUILDERS.get(scenario)
    if builder is None:
        raise FixtureError(f"unknown scenario {scenario!r}; allowed: {sorted(_BUILDERS)}")
    now = now or datetime.now(timezone.utc)
    nodes, edges, ids = builder(dict(params or {}), now)
    return (
        FixtureSeedPlan(scenario=scenario, nodes=tuple(nodes), edges=tuple(edges)),
        ids,
    )


# ---------------------------------------------------------------------------
# FalkorDB rendering (parameterized; property KEYS are server-side constants
# from the builders above, values are always bound parameters)
# ---------------------------------------------------------------------------
def render_fixture_statements(plan: FixtureSeedPlan) -> tuple[tuple[str, dict[str, Any]], ...]:
    statements: list[tuple[str, dict[str, Any]]] = []
    by_key = {n.key: n for n in plan.nodes}
    for i, node in enumerate(plan.nodes):
        props = {k: v for k, v in node.props.items() if v is not None}
        params = {f"n{i}_{k}": v for k, v in props.items()}
        entries = ["tenant_id: $tenant_id"] + [f"{k}: $n{i}_{k}" for k in props]
        labels = ":".join(node.labels)
        cypher = f"CREATE (n:{labels} {{{', '.join(entries)}}})"
        statements.append((cypher, params))
    for j, edge in enumerate(plan.edges):
        src, dst = by_key[edge.src_key], by_key[edge.dst_key]
        params: dict[str, Any] = {
            f"e{j}_src": src.props[src.id_prop],
            f"e{j}_dst": dst.props[dst.id_prop],
        }
        edge_props = {k: v for k, v in edge.props.items() if v is not None}
        props_txt = ""
        if edge_props:
            params.update({f"e{j}_{k}": v for k, v in edge_props.items()})
            props_txt = " {" + ", ".join(f"{k}: $e{j}_{k}" for k in edge_props) + "}"
        cypher = (
            f"MATCH (a:{src.id_label} {{tenant_id: $tenant_id, {src.id_prop}: $e{j}_src}})\n"
            f"MATCH (b:{dst.id_label} {{tenant_id: $tenant_id, {dst.id_prop}: $e{j}_dst}})\n"
            f"CREATE (a)-[:{edge.type}{props_txt}]->(b)"
        )
        statements.append((cypher, params))
    return tuple(statements)


def compile_fixture_write(plan: FixtureSeedPlan):
    from .writes import CompiledWrite

    return CompiledWrite(
        cypher="RETURN 1 AS ok",  # statements carry the real writes
        params={},
        plan=plan,
        statements=render_fixture_statements(plan),
    )
