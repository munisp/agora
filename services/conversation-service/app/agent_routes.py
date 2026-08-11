"""REST API: agents registry + capture primitive (SPEC-W38 F1/F3, §2).

Tenant resolution is IDENTICAL to /v1/conversations (app/routes.py):
?tenant=<uuid-or-slug> query param or X-Tenant-ID header -> tenant-scoped
Postgres transaction (app.tenant_id GUC -> RLS). Slugs (admin-web passes the
org slug) are resolved via identity-service (app/tenants.py). The only
exception is GET /v1/agents/resolve, which is INTERNAL (voice runtime calls
http://conversation:7007 directly, no APISIX) and resolves a dialed E.164
number to agent+definition across tenants — that is its entire purpose. Its
agent payload also carries tenant_slug (tenant_slugs projection) so the
voice runtime can bootstrap TenantContext.
"""

from __future__ import annotations

import uuid
from typing import Annotated, Any

import asyncpg
from fastapi import APIRouter, Depends, HTTPException, Query, Request, Response, status

from . import models
from .agent_db import DuplicatePhoneError, DuplicateSlugError
from .db import NotFoundError
from .routes import _require_tenant

router = APIRouter()


def _store(request: Request) -> Any:
    return request.app.state.agent_store


# ---------------------------------------------------------------------------
# agents (SPEC-W38 F1)
# ---------------------------------------------------------------------------


@router.post("/v1/agents", status_code=status.HTTP_201_CREATED)
async def create_agent(
    body: models.AgentCreate,
    request: Request,
    tenant_id: Annotated[uuid.UUID, Depends(_require_tenant)],
) -> models.Agent:
    try:
        row = await _store(request).create_agent(
            tenant_id,
            body.name,
            slug=body.slug,
            purpose=body.purpose,
            phone_number=body.phone_number,
            definition=body.definition,
        )
    except DuplicatePhoneError:
        raise HTTPException(
            status.HTTP_409_CONFLICT,
            f"phone_number {body.phone_number} already assigned to an agent",
        ) from None
    except DuplicateSlugError:
        raise HTTPException(
            status.HTTP_409_CONFLICT, f"agent slug {body.slug!r} already exists"
        ) from None
    return models.Agent(**row)


@router.get("/v1/agents")
async def list_agents(
    request: Request,
    tenant_id: Annotated[uuid.UUID, Depends(_require_tenant)],
    limit: int = Query(default=50, ge=1, le=200),
    offset: int = Query(default=0, ge=0),
) -> dict[str, Any]:
    rows = await _store(request).list_agents(tenant_id, limit, offset)
    return {
        "agents": [models.Agent(**r).model_dump(mode="json") for r in rows],
        "limit": limit,
        "offset": offset,
    }


# NOTE: /v1/agents/resolve is registered BEFORE /v1/agents/{agent_id} so the
# literal path wins over the path parameter.
@router.get("/v1/agents/resolve")
async def resolve_agent(
    request: Request,
    phone: Annotated[str, Query(min_length=2)],
    tenant: uuid.UUID | None = Query(default=None),
) -> models.AgentResolved:
    """Dialed-number -> {agent, definition}. INTERNAL: NOT exposed via APISIX
    (SPEC-W38 §2); the voice runtime calls it directly. Deliberately global
    (the caller knows the number, not the tenant); ?tenant= narrows when the
    tenant is already known."""
    agent = await _store(request).resolve_agent_by_phone(phone.strip(), tenant)
    if agent is None:
        raise HTTPException(
            status.HTTP_404_NOT_FOUND, f"no active agent for phone {phone}"
        )
    return models.AgentResolved(
        agent=models.Agent(**agent), definition=agent.get("definition") or {}
    )


@router.get("/v1/agents/{agent_id}")
async def get_agent(
    agent_id: uuid.UUID,
    request: Request,
    tenant_id: Annotated[uuid.UUID, Depends(_require_tenant)],
) -> models.Agent:
    try:
        row = await _store(request).get_agent(agent_id, tenant_id)
    except NotFoundError:
        raise HTTPException(
            status.HTTP_404_NOT_FOUND, f"agent {agent_id} not found"
        ) from None
    return models.Agent(**row)


@router.patch("/v1/agents/{agent_id}")
async def update_agent(
    agent_id: uuid.UUID,
    body: models.AgentUpdate,
    request: Request,
    tenant_id: Annotated[uuid.UUID, Depends(_require_tenant)],
) -> models.Agent:
    fields = body.model_fields_set
    try:
        row = await _store(request).update_agent(
            agent_id,
            tenant_id,
            name=body.name if "name" in fields else None,
            slug=body.slug if "slug" in fields else None,
            purpose=body.purpose if "purpose" in fields else None,
            phone_number=body.phone_number if "phone_number" in fields else None,
            status=body.status if "status" in fields else None,
            definition=body.definition if "definition" in fields else None,
            clear_phone="phone_number" in fields and body.phone_number is None,
        )
    except NotFoundError:
        raise HTTPException(
            status.HTTP_404_NOT_FOUND, f"agent {agent_id} not found"
        ) from None
    except DuplicatePhoneError:
        raise HTTPException(
            status.HTTP_409_CONFLICT,
            f"phone_number {body.phone_number} already assigned to an agent",
        ) from None
    except DuplicateSlugError:
        raise HTTPException(
            status.HTTP_409_CONFLICT, f"agent slug {body.slug!r} already exists"
        ) from None
    return models.Agent(**row)


@router.delete("/v1/agents/{agent_id}")
async def delete_agent(
    agent_id: uuid.UUID,
    request: Request,
    tenant_id: Annotated[uuid.UUID, Depends(_require_tenant)],
) -> models.Agent:
    """Soft delete per SPEC-W38 §2: status='disabled', row kept."""
    try:
        row = await _store(request).disable_agent(agent_id, tenant_id)
    except NotFoundError:
        raise HTTPException(
            status.HTTP_404_NOT_FOUND, f"agent {agent_id} not found"
        ) from None
    return models.Agent(**row)


# ---------------------------------------------------------------------------
# capture_schemas (SPEC-W38 F3)
# ---------------------------------------------------------------------------


@router.post("/v1/capture-schemas", status_code=status.HTTP_201_CREATED)
async def create_capture_schema(
    body: models.CaptureSchemaCreate,
    request: Request,
    tenant_id: Annotated[uuid.UUID, Depends(_require_tenant)],
) -> models.CaptureSchema:
    try:
        row = await _store(request).create_capture_schema(
            tenant_id, body.agent_id, body.name, body.schema, body.active
        )
    except asyncpg.ForeignKeyViolationError:
        # agent missing — or belongs to another tenant (RLS hides it)
        raise HTTPException(
            status.HTTP_404_NOT_FOUND, f"agent {body.agent_id} not found"
        ) from None
    return models.CaptureSchema(**row)


@router.get("/v1/capture-schemas")
async def list_capture_schemas(
    request: Request,
    tenant_id: Annotated[uuid.UUID, Depends(_require_tenant)],
    agent_id: uuid.UUID | None = Query(default=None),
) -> dict[str, Any]:
    rows = await _store(request).list_capture_schemas(tenant_id, agent_id)
    return {
        "capture_schemas": [
            models.CaptureSchema(**r).model_dump(mode="json") for r in rows
        ]
    }


@router.patch("/v1/capture-schemas/{schema_id}")
async def update_capture_schema(
    schema_id: uuid.UUID,
    body: models.CaptureSchemaUpdate,
    request: Request,
    tenant_id: Annotated[uuid.UUID, Depends(_require_tenant)],
) -> models.CaptureSchema:
    fields = body.model_fields_set
    try:
        row = await _store(request).update_capture_schema(
            schema_id,
            tenant_id,
            name=body.name if "name" in fields else None,
            schema=body.schema if "schema" in fields else None,
            active=body.active if "active" in fields else None,
        )
    except NotFoundError:
        raise HTTPException(
            status.HTTP_404_NOT_FOUND, f"capture schema {schema_id} not found"
        ) from None
    return models.CaptureSchema(**row)


@router.delete("/v1/capture-schemas/{schema_id}", status_code=status.HTTP_204_NO_CONTENT)
async def delete_capture_schema(
    schema_id: uuid.UUID,
    request: Request,
    tenant_id: Annotated[uuid.UUID, Depends(_require_tenant)],
) -> Response:
    try:
        await _store(request).delete_capture_schema(schema_id, tenant_id)
    except NotFoundError:
        raise HTTPException(
            status.HTTP_404_NOT_FOUND, f"capture schema {schema_id} not found"
        ) from None
    return Response(status_code=status.HTTP_204_NO_CONTENT)


# ---------------------------------------------------------------------------
# capture_records (SPEC-W38 F3)
# ---------------------------------------------------------------------------


@router.get("/v1/capture-records")
async def list_capture_records(
    request: Request,
    tenant_id: Annotated[uuid.UUID, Depends(_require_tenant)],
    agent_id: uuid.UUID | None = Query(default=None),
    conversation_id: uuid.UUID | None = Query(default=None),
    limit: int = Query(default=100, ge=1, le=500),
) -> dict[str, Any]:
    rows = await _store(request).list_capture_records(
        tenant_id, agent_id, conversation_id, limit
    )
    return {
        "capture_records": [
            models.CaptureRecord(**r).model_dump(mode="json") for r in rows
        ],
        "limit": limit,
    }
