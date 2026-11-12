"""Tenant binding helpers (SPEC-W43 C1) — same contract as
knowledge-service app/db.py, split out so app/main.py stays slim.

Gateway-authenticated calls arrive with the APISIX-injected X-Tenant-Slugs
header; the explicit tenant selector must exactly match one of its entries
(else 403; no header + no dev escape -> 401). Slugs resolve to UUIDs via
identity-service over Dapr.
"""

from __future__ import annotations

import uuid

from fastapi import HTTPException, Request

from .config import settings
from .dapr_client import DaprClient, DaprError


def parse_tenant_slugs(header_value: str | None) -> list[str]:
    if not header_value:
        return []
    return [s.strip() for s in header_value.split(",") if s.strip()]


def bind_request_tenant(request: Request, explicit: str | None) -> str:
    """SPEC-W43 C1 tenant binding (see knowledge-service bind_tenant_value)."""
    slugs = parse_tenant_slugs(request.headers.get("x-tenant-slugs"))
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
    if not settings.trust_direct_tenant:
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
    """Resolve a tenant slug to its UUID (UUIDs pass through directly)."""
    try:
        uuid.UUID(tenant)
        return tenant
    except (ValueError, AttributeError):
        pass
    headers = {"X-Internal-Token": internal_token} if internal_token else None
    try:
        ctx = await dapr.invoke(identity_app_id, f"v1/tenants/{tenant}", headers=headers)
    except DaprError as exc:
        raise LookupError(f"tenant '{tenant}' not found: {exc}") from exc
    tenant_id = (ctx or {}).get("id")
    if not tenant_id:
        raise LookupError(f"tenant '{tenant}' not found")
    return str(tenant_id)
