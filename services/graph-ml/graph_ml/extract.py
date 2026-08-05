"""Per-tenant subgraph extraction from FalkorDB (SPEC-W29 §3 WS-A).

Read-only access over the Redis protocol (``GRAPH.QUERY``). Every Cypher
statement is a static module-level template with bound ``$tenant_id`` —
values travel in the ``CYPHER k=v`` preamble via :func:`build_query`, never
interpolated into statement text. This service NEVER writes FalkorDB; the
single write path is the graph-service internal API (SPEC-W29 §4 gate 3).
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field
from typing import Any, Protocol

log = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Cypher templates (static text, $-bound parameters ONLY — never string-built)
# ---------------------------------------------------------------------------

TENANTS_QUERY = "MATCH (t:Tenant) RETURN t.tenant_id AS tenant_id"

PERSONS_QUERY = (
    "MATCH (p:Person {tenant_id: $tenant_id}) "
    "RETURN p.person_id AS person_id, p.name AS name, p.channels AS channels, "
    "p.consent_summary AS consent_summary, p.quarantine AS quarantine, "
    "p.updated_at AS updated_at, p.name_embedding AS name_embedding"
)

BOOKINGS_QUERY = (
    "MATCH (p:Person {tenant_id: $tenant_id})-[:BOOKED]->(b:Booking)-[:FOR]->(o:Offering) "
    "RETURN p.person_id AS person_id, b.booking_id AS booking_id, b.created_at AS at, "
    "b.status AS status, b.showed AS showed, o.offering_id AS offering_id, "
    "o.name AS offering_name, o.price_cents AS price_cents"
)

OFFERINGS_QUERY = (
    "MATCH (o:Offering {tenant_id: $tenant_id}) "
    "RETURN o.offering_id AS offering_id, o.name AS name, o.price_cents AS price_cents"
)

REFERRALS_QUERY = (
    "MATCH (a:Person {tenant_id: $tenant_id})-[r:REFERRED]->(b:Person) "
    "WHERE b.tenant_id = $tenant_id "
    "RETURN a.person_id AS from_person_id, b.person_id AS to_person_id, "
    "r.at AS at, r.program AS program"
)

MESSAGES_QUERY = (
    "MATCH (p:Person {tenant_id: $tenant_id})-[m:MESSAGED]->(c:Campaign) "
    "RETURN p.person_id AS person_id, m.campaign_id AS campaign_id, "
    "m.at AS at, m.status AS status"
)

CONSENTS_QUERY = (
    "MATCH (p:Person {tenant_id: $tenant_id})-[r:CONSENTED]->(c:Consent) "
    "RETURN p.person_id AS person_id, r.purpose AS purpose, r.at AS at, "
    "c.revoked_at AS revoked_at"
)

CONTACTS_QUERY = (
    "MATCH (p:Person {tenant_id: $tenant_id})-[:HAS_CONTACT]->(c:Contact) "
    "RETURN p.person_id AS person_id, c.lead_id AS lead_id, "
    "c.captured_at AS captured_at, c.channel_of_first_touch AS channel, "
    "c.source AS source"
)

ALL_TENANT_QUERIES = (
    PERSONS_QUERY,
    BOOKINGS_QUERY,
    OFFERINGS_QUERY,
    REFERRALS_QUERY,
    MESSAGES_QUERY,
    CONSENTS_QUERY,
    CONTACTS_QUERY,
)


# ---------------------------------------------------------------------------
# Parameter preamble — the ONLY place values become Cypher text, with escaping
# ---------------------------------------------------------------------------


def quote_param(value: Any) -> str:
    """Render a scalar as a safely-escaped Cypher literal for the preamble."""
    if value is None:
        return "null"
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, (int, float)):
        return repr(value)
    if isinstance(value, str):
        escaped = (
            value.replace("\\", "\\\\")
            .replace("'", "\\'")
            .replace("\n", "\\n")
            .replace("\r", "\\r")
        )
        return f"'{escaped}'"
    raise TypeError(f"unsupported Cypher parameter type: {type(value)!r}")


def build_query(statement: str, params: dict[str, Any] | None = None) -> str:
    """Bind params via the ``CYPHER k=v`` preamble; statement text is untouched."""
    if not params:
        return statement
    preamble = " ".join(f"{key}={quote_param(val)}" for key, val in params.items())
    return f"CYPHER {preamble} {statement}"


# ---------------------------------------------------------------------------
# Extracted tenant subgraph (plain dataclasses — backend-agnostic)
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class PersonRec:
    person_id: str
    name: str = ""
    channels: tuple[str, ...] = ()
    consent_summary: str = ""
    quarantine: bool = False
    updated_at: Any = None
    name_embedding: tuple[float, ...] = ()


@dataclass(frozen=True)
class BookingRec:
    person_id: str
    booking_id: str
    offering_id: str
    offering_name: str = ""
    at: Any = None
    status: str = ""
    showed: Any = None
    price_cents: int = 0


@dataclass(frozen=True)
class OfferingRec:
    offering_id: str
    name: str = ""
    price_cents: int = 0


@dataclass(frozen=True)
class ReferralRec:
    from_person_id: str
    to_person_id: str
    at: Any = None
    program: str = ""


@dataclass(frozen=True)
class MessageRec:
    person_id: str
    campaign_id: str
    at: Any = None
    status: str = ""


@dataclass(frozen=True)
class ConsentRec:
    person_id: str
    purpose: str
    at: Any = None
    revoked_at: Any = None


@dataclass(frozen=True)
class ContactRec:
    person_id: str
    lead_id: str
    captured_at: Any = None
    channel: str = ""
    source: str = ""


@dataclass
class TenantGraph:
    tenant_id: str
    persons: list[PersonRec] = field(default_factory=list)
    bookings: list[BookingRec] = field(default_factory=list)
    offerings: list[OfferingRec] = field(default_factory=list)
    referrals: list[ReferralRec] = field(default_factory=list)
    messages: list[MessageRec] = field(default_factory=list)
    consents: list[ConsentRec] = field(default_factory=list)
    contacts: list[ContactRec] = field(default_factory=list)


# ---------------------------------------------------------------------------
# GraphClient seam (real FalkorDB + in-memory fake for tests/dev)
# ---------------------------------------------------------------------------


class GraphClient(Protocol):
    """Read-only tenant-graph seam. Implementations MUST be tenant-scoped."""

    def list_tenants(self) -> list[str]: ...

    def fetch_tenant_graph(self, tenant_id: str) -> TenantGraph: ...

    def close(self) -> None: ...


# RedisGraph/FalkorDB non-compact scalar type tags.
_T_NULL, _T_STRING, _T_INT, _T_BOOL, _T_DOUBLE, _T_ARRAY = 1, 2, 3, 4, 5, 6
_T_EDGE, _T_NODE, _T_PATH, _T_MAP = 7, 8, 9, 10


def _decode(value: Any) -> Any:
    if isinstance(value, bytes):
        return value.decode("utf-8", errors="replace")
    return value


def _parse_scalar(tagged: Any) -> Any:
    """Parse one [type, value] cell from a non-compact GRAPH.QUERY row."""
    if not (isinstance(tagged, (list, tuple)) and len(tagged) == 2):
        return _decode(tagged)
    tag, value = int(tagged[0]), tagged[1]
    if tag == _T_NULL:
        return None
    if tag in (_T_STRING,):
        return _decode(value)
    if tag == _T_INT:
        return int(value)
    if tag == _T_BOOL:
        return bool(value)
    if tag == _T_DOUBLE:
        return float(value)
    if tag == _T_ARRAY:
        return [_parse_scalar(v) for v in value]
    if tag == _T_NODE:
        # [id, [labels], [[key, [type, value]], ...]]
        props = {
            _decode(p[0]): _parse_scalar(p[1]) if _is_tagged(p[1]) else _decode(p[1])
            for p in (value[2] if len(value) > 2 else [])
        }
        return {"_node_id": value[0], "_labels": [_decode(l) for l in value[1]], **props}
    if tag == _T_EDGE:
        return {"_edge_id": value[0], "_type": _decode(value[1])}
    if tag == _T_MAP:
        return {_decode(k): _parse_scalar(v) for k, v in value}
    return _decode(value)


def _is_tagged(value: Any) -> bool:
    return isinstance(value, (list, tuple)) and len(value) == 2 and isinstance(value[0], int)


class FalkorRedisClient:
    """FalkorDB reader over the Redis protocol (``redis`` package only)."""

    def __init__(
        self,
        host: str,
        port: int,
        graph: str,
        username: str = "",
        password: str = "",
    ) -> None:
        import redis  # imported here so tests need no live driver

        self._graph = graph
        self._redis = redis.Redis(
            host=host,
            port=port,
            username=username or None,
            password=password or None,
            decode_responses=False,
        )

    def close(self) -> None:
        self._redis.close()

    def _query(self, statement: str, params: dict[str, Any] | None = None) -> list[dict[str, Any]]:
        reply = self._redis.execute_command(
            "GRAPH.QUERY", self._graph, build_query(statement, params)
        )
        if not reply or len(reply) < 2:
            return []
        header, rows = reply[0], reply[1]
        names = []
        for col in header:
            if isinstance(col, (list, tuple)):
                col = col[0]
            names.append(_decode(col))
        out: list[dict[str, Any]] = []
        for row in rows or []:
            out.append({name: _parse_scalar(cell) for name, cell in zip(names, row)})
        return out

    def list_tenants(self) -> list[str]:
        rows = self._query(TENANTS_QUERY)
        return sorted(str(r["tenant_id"]) for r in rows if r.get("tenant_id"))

    def fetch_tenant_graph(self, tenant_id: str) -> TenantGraph:
        if not tenant_id:
            raise ValueError("tenant_id is required for extraction")
        params = {"tenant_id": tenant_id}
        graph = TenantGraph(tenant_id=tenant_id)

        for r in self._query(PERSONS_QUERY, params):
            graph.persons.append(
                PersonRec(
                    person_id=str(r["person_id"]),
                    name=r.get("name") or "",
                    channels=tuple(r.get("channels") or ()),
                    consent_summary=r.get("consent_summary") or "",
                    quarantine=bool(r.get("quarantine")),
                    updated_at=r.get("updated_at"),
                    name_embedding=tuple(r.get("name_embedding") or ()),
                )
            )
        for r in self._query(BOOKINGS_QUERY, params):
            graph.bookings.append(
                BookingRec(
                    person_id=str(r["person_id"]),
                    booking_id=str(r.get("booking_id") or ""),
                    offering_id=str(r["offering_id"]),
                    offering_name=r.get("offering_name") or "",
                    at=r.get("at"),
                    status=r.get("status") or "",
                    showed=r.get("showed"),
                    price_cents=int(r.get("price_cents") or 0),
                )
            )
        for r in self._query(OFFERINGS_QUERY, params):
            graph.offerings.append(
                OfferingRec(
                    offering_id=str(r["offering_id"]),
                    name=r.get("name") or "",
                    price_cents=int(r.get("price_cents") or 0),
                )
            )
        for r in self._query(REFERRALS_QUERY, params):
            graph.referrals.append(
                ReferralRec(
                    from_person_id=str(r["from_person_id"]),
                    to_person_id=str(r["to_person_id"]),
                    at=r.get("at"),
                    program=r.get("program") or "",
                )
            )
        for r in self._query(MESSAGES_QUERY, params):
            graph.messages.append(
                MessageRec(
                    person_id=str(r["person_id"]),
                    campaign_id=str(r.get("campaign_id") or ""),
                    at=r.get("at"),
                    status=(r.get("status") or "").lower(),
                )
            )
        for r in self._query(CONSENTS_QUERY, params):
            graph.consents.append(
                ConsentRec(
                    person_id=str(r["person_id"]),
                    purpose=r.get("purpose") or "",
                    at=r.get("at"),
                    revoked_at=r.get("revoked_at"),
                )
            )
        for r in self._query(CONTACTS_QUERY, params):
            graph.contacts.append(
                ContactRec(
                    person_id=str(r["person_id"]),
                    lead_id=str(r.get("lead_id") or ""),
                    captured_at=r.get("captured_at"),
                    channel=r.get("channel") or "",
                    source=r.get("source") or "",
                )
            )
        return graph


class StaticGraphClient:
    """In-memory GraphClient (Cypher-mock) for tests and local dev sweeps."""

    def __init__(self, tenants: dict[str, TenantGraph]) -> None:
        self._tenants = dict(tenants)
        self.fetch_calls: list[str] = []

    def list_tenants(self) -> list[str]:
        return sorted(self._tenants)

    def fetch_tenant_graph(self, tenant_id: str) -> TenantGraph:
        if not tenant_id:
            raise ValueError("tenant_id is required for extraction")
        self.fetch_calls.append(tenant_id)
        if tenant_id not in self._tenants:
            raise KeyError(f"unknown tenant: {tenant_id}")
        return self._tenants[tenant_id]

    def close(self) -> None:
        return None


def client_from_settings(settings: Any) -> GraphClient:
    return FalkorRedisClient(
        host=settings.falkordb_host,
        port=settings.falkordb_port,
        graph=settings.falkordb_db,
        username=settings.falkordb_username,
        password=settings.falkordb_password,
    )
