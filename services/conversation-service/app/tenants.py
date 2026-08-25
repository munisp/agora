"""Tenant slug -> UUID resolution via identity-service (Dapr invoke).

Mirrors analytics-pipeline/analytics_pipeline/tenants.py and booking-service
internal/bookingops/resolver.go: identity `GET /v1/tenants/{slug}` (Dapr
app-id `identity`) returns the tenant context; results are cached with a TTL
and an EXPIRED entry is served stale when identity is unreachable on refresh
(a never-resolved slug still errors).

The optional `remember` hook persists each successful resolution (write-
through) so the conversation DB can answer the REVERSE mapping
(tenant_id -> slug, AgentStore.remember_tenant_slug) for the internal
/v1/agents/resolve payload — identity exposes no by-id lookup, so the local
projection is the only deterministic source for it.
"""

from __future__ import annotations

import time
from collections.abc import Awaitable, Callable
from dataclasses import dataclass

from .dapr_client import DaprClient
from .logging import get_logger

log = get_logger(__name__)

DEFAULT_TENANT_CACHE_TTL_SECONDS = 300.0

__all__ = [
    "TenantInfo",
    "TenantNotFoundError",
    "TenantResolutionError",
    "TenantResolver",
]


class TenantNotFoundError(Exception):
    """identity-service answered 404 (or empty id) for the slug."""


class TenantResolutionError(Exception):
    """identity-service unreachable and no cached entry to fall back to."""


@dataclass(frozen=True)
class TenantInfo:
    id: str
    slug: str
    name: str = ""
    timezone: str = ""
    currency: str = ""


class TenantResolver:
    def __init__(
        self,
        dapr: DaprClient,
        identity_app_id: str = "identity",
        ttl_seconds: float = DEFAULT_TENANT_CACHE_TTL_SECONDS,
        remember: Callable[[str, str], Awaitable[None]] | None = None,
        internal_token: str = "",
    ):
        self._dapr = dapr
        self._app_id = identity_app_id
        self._ttl = ttl_seconds if ttl_seconds > 0 else DEFAULT_TENANT_CACHE_TTL_SECONDS
        self._cache: dict[str, tuple[TenantInfo, float]] = {}
        self._remember = remember
        # SPEC-W44 K2: identity gates tenant lookup for service callers
        # behind X-Internal-Token (IDENTITY_INTERNAL_TOKEN env).
        self._internal_token = internal_token

    async def by_slug(self, slug: str) -> TenantInfo:
        entry = self._cache.get(slug)
        fresh = entry is not None and (time.monotonic() - entry[1]) < self._ttl
        if fresh:
            return entry[0]
        try:
            headers = (
                {"X-Internal-Token": self._internal_token}
                if self._internal_token
                else None
            )
            payload = await self._dapr.invoke(
                self._app_id, f"v1/tenants/{slug}", headers=headers
            )
        except Exception as exc:  # noqa: BLE001 — daprd/identity outage
            status = getattr(exc, "status_code", None)
            if status == 404:
                raise TenantNotFoundError(f"tenant {slug!r} not found") from exc
            if entry is not None:
                log.warning(
                    "tenant.resolve_stale",
                    slug=slug,
                    error=f"{type(exc).__name__}: {exc}",
                )
                return entry[0]
            raise TenantResolutionError(
                f"resolve tenant {slug!r}: {type(exc).__name__}: {exc}"
            ) from exc
        if not isinstance(payload, dict) or not payload.get("id"):
            raise TenantNotFoundError(f"tenant {slug!r} not found")
        info = TenantInfo(
            id=str(payload["id"]),
            slug=str(payload.get("slug") or slug),
            name=str(payload.get("name", "")),
            timezone=str(payload.get("timezone", "")),
            currency=str(payload.get("currency", "")),
        )
        self._cache[slug] = (info, time.monotonic())
        if self._remember is not None:
            try:
                await self._remember(info.id, info.slug)
            except Exception as exc:  # noqa: BLE001 — projection is best-effort
                log.warning("tenant.remember_failed", slug=slug, error=str(exc))
        return info
