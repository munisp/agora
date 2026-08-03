"""Tenant slug -> UUID resolution via identity-service (Dapr invoke).

Mirrors booking-service internal/bookingops/resolver.go: identity
`GET /v1/tenants/{slug}` (Dapr app-id `identity`) returns the tenant context;
results are cached with a TTL and an EXPIRED entry is served stale when
identity is unreachable on refresh (a never-resolved slug still errors).
"""

from __future__ import annotations

import time
from dataclasses import dataclass

import structlog

from .dapr_client import DaprClient

log = structlog.get_logger()

DEFAULT_TENANT_CACHE_TTL_SECONDS = 300.0


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
    ):
        self._dapr = dapr
        self._app_id = identity_app_id
        self._ttl = ttl_seconds if ttl_seconds > 0 else DEFAULT_TENANT_CACHE_TTL_SECONDS
        self._cache: dict[str, tuple[TenantInfo, float]] = {}

    async def by_slug(self, slug: str) -> TenantInfo:
        entry = self._cache.get(slug)
        fresh = entry is not None and (time.monotonic() - entry[1]) < self._ttl
        if fresh:
            return entry[0]
        try:
            payload = await self._dapr.invoke(self._app_id, f"v1/tenants/{slug}")
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
            slug=slug,
            name=str(payload.get("name", "")),
            timezone=str(payload.get("timezone", "")),
            currency=str(payload.get("currency", "")),
        )
        self._cache[slug] = (info, time.monotonic())
        return info
