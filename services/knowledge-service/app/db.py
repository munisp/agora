"""Postgres access (asyncpg) with per-request tenant RLS context (SPEC §7).

Also hosts the SPEC-W43 C1 tenant-binding helpers (shared by app/main.py
and app/analytics.py): the tenant selector is bound against the
gateway-injected X-Tenant-Slugs header before slug resolution.
"""

from __future__ import annotations

import uuid
from contextlib import asynccontextmanager
from typing import Any

import asyncpg
from fastapi import HTTPException

from .dapr_client import DaprClient, DaprError
from .logging import get_logger

log = get_logger(__name__)

def _is_uuid(value: str) -> bool:
    try:
        uuid.UUID(value)
        return True
    except (ValueError, AttributeError):
        return False


class Database:
    def __init__(self, dsn: str):
        self._dsn = dsn
        self._pool: asyncpg.Pool | None = None

    async def connect(self) -> None:
        self._pool = await asyncpg.create_pool(self._dsn, min_size=1, max_size=8)
        log.info("postgres.pool_created")

    async def aclose(self) -> None:
        if self._pool:
            await self._pool.close()
            self._pool = None

    async def ping(self) -> bool:
        if not self._pool:
            return False
        try:
            async with self._pool.acquire() as conn:
                await conn.fetchval("SELECT 1")
            return True
        except Exception:
            return False

    async def ensure_suggestions_table(self) -> None:
        """SPEC-W3 §4 innovation 4: idempotent kb_suggestions bootstrap DDL."""
        from .suggestions import DDL

        assert self._pool is not None, "database not connected"
        async with self._pool.acquire() as conn:
            await conn.execute(DDL)
        log.info("kb_suggestions.table_ensured")

    @asynccontextmanager
    async def tenant_conn(self, tenant_id: str):
        """Yield a connection with `app.tenant_id` set for RLS.

        Uses SET LOCAL inside a transaction so the setting never leaks
        across pooled connections.
        """
        assert self._pool is not None, "database not connected"
        async with self._pool.acquire() as conn:
            async with conn.transaction():
                await conn.execute(
                    "SELECT set_config('app.tenant_id', $1, true)", tenant_id
                )
                yield conn


def parse_tenant_slugs(header_value: str | None) -> list[str]:
    """Gateway-injected X-Tenant-Slugs header (comma-separated slugs/uuids
    from the validated JWT tenant_slugs claim — SPEC-W43 C1)."""
    if not header_value:
        return []
    return [s.strip() for s in header_value.split(",") if s.strip()]


def bind_tenant_value(
    slugs: list[str], explicit: str | None, *, trust_direct: bool
) -> str:
    """SPEC-W43 C1 tenant binding.

    With X-Tenant-Slugs present (gateway-authenticated): an explicit
    query/body tenant selector is honored ONLY when it exactly matches one
    of the header entries (else 403); without an explicit selector a
    single-entry header selects that tenant and a multi-entry header is
    ambiguous (400). Without the header there is no authenticated tenant
    context — 401 unless the standalone-dev escape
    (OPENDESK_TRUST_DIRECT_TENANT=1) restored legacy direct selection.
    """
    if slugs:
        if explicit is not None:
            if explicit not in slugs:
                raise HTTPException(
                    status_code=403,
                    detail="tenant selector not granted to the authenticated "
                    "principal (must match X-Tenant-Slugs)",
                )
            return explicit
        if len(slugs) > 1:
            raise HTTPException(
                status_code=400,
                detail="principal has multiple tenants; pass an explicit "
                "tenant naming one of the X-Tenant-Slugs entries",
            )
        return slugs[0]
    if not trust_direct:
        raise HTTPException(
            status_code=401,
            detail="tenant context required: X-Tenant-Slugs header "
            "(injected by the gateway from the validated JWT)",
        )
    if explicit is None:
        raise HTTPException(status_code=401, detail="tenant required")
    return explicit


async def resolve_tenant_id(
    dapr: DaprClient, identity_app_id: str, tenant: str, internal_token: str = ""
) -> str:
    """Resolve the `tenant` filter to a tenant UUID.

    Accepts a UUID directly, otherwise treats it as a slug and resolves via
    identity-service GET /v1/tenants/{slug} through Dapr service invocation
    (server-side tenant resolution, SPEC §1). SPEC-W44 K2: the lookup carries
    X-Internal-Token (IDENTITY_INTERNAL_TOKEN) — identity gates tenant-scoped
    reads for service callers behind it.
    """
    if _is_uuid(tenant):
        return tenant
    headers = {"X-Internal-Token": internal_token} if internal_token else None
    try:
        ctx = await dapr.invoke(identity_app_id, f"v1/tenants/{tenant}", headers=headers)
    except DaprError as exc:
        raise LookupError(f"tenant '{tenant}' not found: {exc}") from exc
    tenant_id = (ctx or {}).get("id")
    if not tenant_id:
        raise LookupError(f"tenant '{tenant}' not found")
    return str(tenant_id)
