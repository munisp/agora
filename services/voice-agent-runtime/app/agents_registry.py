"""Async client for the conversation-service agents registry (SPEC-W38 F1).

The registry (conversation-service, internal only — NOT exposed via APISIX)
owns agent-as-product records keyed by tenant and dialed phone number:

    GET {AGENTS_REGISTRY_URL}/v1/agents/resolve?phone=<E.164>
        200 -> {"agent": {...}, "definition": {...}}
        404 -> no agent for that number
    GET {AGENTS_REGISTRY_URL}/v1/agents/{agent_id}
        200 -> agent object (or {"agent": {...}, "definition": {...}})

FAIL-OPEN CONTRACT: any 404 / network error / timeout / malformed payload
resolves to None so the caller falls back to the legacy TENANT_PHONE_MAP /
SIP_DEFAULT_SITE path (dev mode keeps working when the registry is down or
unconfigured). Lookups use a 2s timeout and a short-TTL in-process cache
(AGENTS_CACHE_TTL_S, default 30s) so call setup never pays more than one
registry round-trip per number per TTL window — misses are cached too.

This module is deliberately free of livekit imports so it stays unit
-testable with a mocked httpx transport.
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import Any

import httpx

from .agent_definition import AgentDefinition
from .logging import get_logger

log = get_logger("agents-registry")

DEFAULT_REGISTRY_URL = "http://conversation:7007"
DEFAULT_CACHE_TTL_S = 30.0
DEFAULT_TIMEOUT_S = 2.0


@dataclass
class AgentRecord:
    """One agents-registry record plus its parsed definition."""

    id: str
    tenant_id: str = ""
    tenant_slug: str = ""  # optional on the payload; voice needs a slug to bootstrap
    name: str = ""
    slug: str = ""
    phone_number: str = ""
    status: str = ""
    definition: AgentDefinition | None = None
    raw: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def from_payload(cls, payload: Any) -> "AgentRecord | None":
        """Parse a registry response. Accepts both the resolve envelope
        ``{"agent": {...}, "definition": {...}}`` and a bare agent object
        (GET /v1/agents/{id}); the definition may live on either level."""
        if not isinstance(payload, dict):
            return None
        agent = payload.get("agent") if isinstance(payload.get("agent"), dict) else payload
        agent_id = str(agent.get("id") or "").strip()
        if not agent_id:
            return None
        definition_payload = payload.get("definition")
        if not isinstance(definition_payload, dict):
            definition_payload = agent.get("definition")
        return cls(
            id=agent_id,
            tenant_id=str(agent.get("tenant_id") or ""),
            tenant_slug=str(agent.get("tenant_slug") or payload.get("tenant_slug") or ""),
            name=str(agent.get("name") or ""),
            slug=str(agent.get("slug") or ""),
            phone_number=str(agent.get("phone_number") or ""),
            status=str(agent.get("status") or ""),
            definition=AgentDefinition.from_payload(definition_payload),
            raw=agent,
        )


class AgentsRegistryClient:
    """Fail-open async client with a short-TTL in-process cache."""

    def __init__(
        self,
        base_url: str = DEFAULT_REGISTRY_URL,
        *,
        cache_ttl_s: float = DEFAULT_CACHE_TTL_S,
        timeout_s: float = DEFAULT_TIMEOUT_S,
        client: httpx.AsyncClient | None = None,
        clock=time.monotonic,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._cache_ttl_s = cache_ttl_s
        self._client = client or httpx.AsyncClient(
            base_url=self._base_url, timeout=timeout_s
        )
        self._owns_client = client is None
        self._clock = clock
        # key -> (expires_at, AgentRecord | None); misses are cached too.
        self._cache: dict[str, tuple[float, AgentRecord | None]] = {}

    async def aclose(self) -> None:
        if self._owns_client:
            await self._client.aclose()

    def _cached(self, key: str) -> tuple[bool, AgentRecord | None]:
        entry = self._cache.get(key)
        if entry is None:
            return False, None
        expires_at, record = entry
        if self._clock() >= expires_at:
            self._cache.pop(key, None)
            return False, None
        return True, record

    def _store(self, key: str, record: AgentRecord | None) -> None:
        self._cache[key] = (self._clock() + self._cache_ttl_s, record)

    async def _get(self, path: str, *, params: dict[str, str] | None = None) -> Any:
        """GET with fail-open semantics: 404/network/timeout -> None."""
        try:
            resp = await self._client.get(path, params=params)
        except httpx.HTTPError as exc:
            log.warning("agents registry unreachable; failing open", error=str(exc)[:200])
            return None
        if resp.status_code == 404:
            return None
        if resp.status_code != 200:
            log.warning(
                "agents registry unexpected status; failing open",
                status=resp.status_code,
                path=path,
            )
            return None
        try:
            return resp.json()
        except ValueError:
            log.warning("agents registry returned non-JSON; failing open", path=path)
            return None

    async def resolve_agent_by_phone(self, phone: str) -> AgentRecord | None:
        """Resolve a dialed E.164 number to an agent record (or None)."""
        phone = (phone or "").strip()
        if not phone:
            return None
        key = f"phone:{phone}"
        hit, record = self._cached(key)
        if hit:
            return record
        payload = await self._get("/v1/agents/resolve", params={"phone": phone})
        record = AgentRecord.from_payload(payload) if payload is not None else None
        if payload is not None and record is None:
            log.warning("agents registry resolve payload malformed; failing open")
        # Cache the miss only for a definitive 404 — a malformed 200 should
        # not pin a None for the whole TTL.
        if payload is None or record is not None:
            self._store(key, record)
        return record

    async def get_agent(self, agent_id: str) -> AgentRecord | None:
        """Fetch an agent (with definition) by id (or None)."""
        agent_id = (agent_id or "").strip()
        if not agent_id:
            return None
        key = f"id:{agent_id}"
        hit, record = self._cached(key)
        if hit:
            return record
        payload = await self._get(f"/v1/agents/{agent_id}")
        record = AgentRecord.from_payload(payload) if payload is not None else None
        if payload is not None and record is None:
            log.warning("agents registry agent payload malformed; failing open")
        if payload is None or record is not None:
            self._store(key, record)
        return record


# Process-wide default client (mirrors the metrics registry pattern): call
# sites use ``get_registry_client``; tests swap via ``set_registry_client``.
_default_client: AgentsRegistryClient | None = None


def get_registry_client(settings: Any) -> AgentsRegistryClient | None:
    """Return the shared client, building it from settings on first use.

    Returns None when no registry URL is configured (empty AGENTS_REGISTRY_URL
    disables the registry path entirely — pure legacy TENANT_PHONE_MAP)."""
    global _default_client
    if _default_client is not None:
        return _default_client
    base_url = str(getattr(settings, "agents_registry_url", "") or "").strip()
    if not base_url:
        return None
    ttl = float(getattr(settings, "agents_cache_ttl_s", DEFAULT_CACHE_TTL_S) or DEFAULT_CACHE_TTL_S)
    _default_client = AgentsRegistryClient(base_url, cache_ttl_s=ttl)
    return _default_client


def set_registry_client(client: AgentsRegistryClient | None) -> None:
    """Swap/reset the shared client (test isolation)."""
    global _default_client
    _default_client = client
