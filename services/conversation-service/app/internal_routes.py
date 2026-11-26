"""Internal service-to-service routes (SPEC-W45 K19): X-Internal-Token
gated, mirroring the K2 pattern of the Go services (constant-time compare;
503 fail-closed when CONVERSATION_INTERNAL_TOKEN is unset, 401 on a
missing/wrong token).

Mounted endpoints:

- GET  /internal/gdpr/conversations?tenant_id=&contact=
      Full data-subject export for GdprExportWorkflow (notification-worker):
      every conversation carrying the contact marker plus its turns.
- POST /internal/gdpr/erase  {tenant_id, phone?, email?}
      Direct purge invoked by GdprEraseWorkflow (notification-worker) in
      addition to the PrivacyEraseRequested tombstone (which remains the
      fan-out for booking/crm-sync). erase_contact_data is idempotent, so a
      tombstone redelivery after a direct purge is a no-op.

- GET  /internal/conversations/{conversation_id}?tenant_id=
      SPEC-W118: conversation row (channel/contact_phone/site_slug) for the
      chatwoot-bridge link-creation path.
- POST /internal/conversations/{conversation_id}/turns
      SPEC-W118: append a human-agent reply arriving FROM Chatwoot as an
      agent turn. role is restricted to "agent" (least privilege) and the
      turn is published with data.origin="chatwoot" so the bridge's
      outbound consumer skips its own echo (loop breaker). Idempotent via
      the caller-supplied idempotency_key.
"""

from __future__ import annotations

import hmac
import uuid
from typing import Annotated, Any

from fastapi import APIRouter, HTTPException, Query, Request, status
from pydantic import BaseModel

from .db import NotFoundError
from .logging import get_logger
from .routes import _persist_turn

log = get_logger(__name__)

router = APIRouter()


def _cfg(request: Request) -> Any:
    return request.app.state.cfg


def _require_internal_token(request: Request) -> None:
    """K2-pattern guard: fail closed when the server token is unset (503),
    401 on a missing/wrong X-Internal-Token (constant-time compare)."""
    token = getattr(_cfg(request), "internal_token", "") or ""
    if not token:
        raise HTTPException(
            status.HTTP_503_SERVICE_UNAVAILABLE,
            "internal routes disabled: CONVERSATION_INTERNAL_TOKEN unset",
        )
    presented = request.headers.get("X-Internal-Token", "")
    if not presented or not hmac.compare_digest(presented, token):
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "invalid internal token")


@router.get("/internal/gdpr/conversations")
async def gdpr_export_conversations(
    request: Request,
    tenant_id: Annotated[uuid.UUID, Query()],
    contact: Annotated[str, Query(min_length=1)],
) -> dict[str, Any]:
    """GDPR export collector (SPEC-W45 K19): conversations + turns for one
    data subject. Invoked by notification-worker's GdprCollectConversations
    activity via Dapr service invocation with X-Internal-Token."""
    _require_internal_token(request)
    db = request.app.state.db
    convs = await db.export_contact_conversations(tenant_id, contact)
    return {
        "tenant_id": str(tenant_id),
        "contact": contact,
        "conversations": convs,
    }


class GdprEraseRequest(BaseModel):
    tenant_id: uuid.UUID
    phone: str | None = None
    email: str | None = None


@router.post("/internal/gdpr/erase")
async def gdpr_erase(body: GdprEraseRequest, request: Request) -> dict[str, Any]:
    """Direct GDPR purge (SPEC-W45 K19): deletes the subject's turns and
    clears the contact marker. Idempotent — safe against the tombstone
    consumer replaying the same erase."""
    _require_internal_token(request)
    if not body.phone and not body.email:
        raise HTTPException(
            status.HTTP_400_BAD_REQUEST, "phone or email is required"
        )
    db = request.app.state.db
    ids, turns = await db.erase_contact_conversations(body.tenant_id, body.phone, body.email)
    log.info(
        "contact data erased via internal endpoint (GDPR)",
        tenant_id=str(body.tenant_id),
        conversations=len(ids),
        turns_deleted=turns,
    )
    # Erased text must not stay searchable. Postgres is already erased and the
    # call is idempotent, so a 503 here tells the caller to retry.
    search_index = getattr(request.app.state, "search_index", None)
    if search_index is not None and ids:
        if await search_index.delete_conversations(body.tenant_id, ids) is None:
            raise HTTPException(
                status.HTTP_503_SERVICE_UNAVAILABLE,
                "turns erased but the search index could not be updated; retry",
            )
    return {
        "status": "erased",
        "conversations_matched": len(ids),
        "turns_deleted": turns,
    }


@router.post("/internal/search/prune")
async def search_prune(request: Request) -> dict[str, Any]:
    """Drop search docs of conversations that no longer exist in Postgres
    (also runs every INDEXER_PRUNE_SECONDS). X-Internal-Token gated."""
    _require_internal_token(request)
    search_index = getattr(request.app.state, "search_index", None)
    if search_index is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "indexer disabled")
    result = await search_index.prune_orphans(request.app.state.db)
    if result is None:
        raise HTTPException(status.HTTP_503_SERVICE_UNAVAILABLE, "search index unreachable")
    return {"pruned_docs": result}



@router.post("/internal/conversations/{conversation_id}/call-details")
async def merge_call_details(conversation_id: str, request: Request) -> dict:
    """2026-11-23: crm-sync saves the call's AI summary here so the dashboard shows the same summary as the CRM
    note. Body: {"tenant_id": "<uuid>", "details": {"summary": "..."}}. X-Internal-Token gated."""
    _require_internal_token(request)
    import uuid as _uuid
    from fastapi import HTTPException

    body = await request.json()
    try:
        cid, tid = _uuid.UUID(conversation_id), _uuid.UUID(str(body.get("tenant_id")))
    except (ValueError, TypeError):
        raise HTTPException(status_code=422, detail="conversation id and tenant_id must be UUIDs")
    details = body.get("details") or {}
    if not isinstance(details, dict) or not details:
        raise HTTPException(status_code=422, detail="details must be a non-empty object")
    # "handoff" (2026-11-23): {"active": bool, "agent": "<name>", "inbox_conversation_id": n} — set by the human inbox
    # bridge; while active the receptionist stays quiet on this conversation.
    allowed = {k: v for k, v in details.items() if k in ("summary", "purpose", "contact_name", "handoff")}
    ok = await request.app.state.db.merge_call_details(cid, tid, allowed)
    if not ok:
        raise HTTPException(status_code=404, detail="conversation not found")
    return {"updated": True}


class BridgeTurnRequest(BaseModel):
    tenant_id: uuid.UUID
    role: str
    text: str
    idempotency_key: str | None = None


@router.get("/internal/conversations/{conversation_id}")
async def bridge_get_conversation(
    conversation_id: uuid.UUID,
    request: Request,
    tenant_id: Annotated[uuid.UUID, Query()],
) -> dict[str, Any]:
    """SPEC-W118: conversation row for the chatwoot-bridge link-creation
    path (channel + contact_phone drive Chatwoot contact creation)."""
    _require_internal_token(request)
    db = request.app.state.db
    try:
        conv = await db.get_conversation(conversation_id, tenant_id)
    except NotFoundError:
        raise HTTPException(
            status.HTTP_404_NOT_FOUND,
            f"conversation {conversation_id} not found",
        ) from None
    return dict(conv)


@router.post("/internal/conversations/{conversation_id}/turns")
async def bridge_append_turn(
    conversation_id: uuid.UUID,
    body: BridgeTurnRequest,
    request: Request,
) -> dict[str, Any]:
    """SPEC-W118: append a Chatwoot human-agent reply as an agent turn.

    The turn is published with data.origin="chatwoot" so the bridge's
    outbound consumer skips its own echo. role is restricted to "agent" —
    the bridge never speaks for the customer.
    """
    _require_internal_token(request)
    if body.role != "agent":
        raise HTTPException(
            status.HTTP_400_BAD_REQUEST,
            "bridge turns must have role 'agent'",
        )
    if not body.text.strip():
        raise HTTPException(
            status.HTTP_400_BAD_REQUEST, "text must be non-empty"
        )
    turn, created = await _persist_turn(
        request.app.state,
        conversation_id,
        body.tenant_id,
        "agent",
        body.text,
        idempotency_key=body.idempotency_key,
        origin="chatwoot",
    )
    return {
        "id": str(turn.id),
        "conversation_id": str(turn.conversation_id),
        "seq": turn.seq,
        "role": turn.role,
        "ts": turn.ts.isoformat(),
        "created": created,
    }
