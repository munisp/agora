"""REST API: conversations + turns (SPEC §7 conversation schema)."""

from __future__ import annotations

import asyncio
import uuid
from typing import Annotated, Any

import asyncpg
from fastapi import APIRouter, Depends, Header, HTTPException, Query, Request, Response, status

from . import events, incidents, intel, models
from .db import NotFoundError

router = APIRouter()


def _tenant_header(x_tenant_id: Annotated[str | None, Header()] = None) -> uuid.UUID | None:
    if x_tenant_id is None:
        return None
    try:
        return uuid.UUID(x_tenant_id)
    except ValueError:
        raise HTTPException(status.HTTP_400_BAD_REQUEST, "invalid X-Tenant-ID header") from None


def _require_tenant(
    tenant: uuid.UUID | None = Query(default=None),
    header_tenant: uuid.UUID | None = Depends(_tenant_header),
) -> uuid.UUID:
    """Tenant scope comes from ?tenant= or X-Tenant-ID (required by RLS)."""
    t = tenant or header_tenant
    if t is None:
        raise HTTPException(
            status.HTTP_400_BAD_REQUEST,
            "tenant scope required: ?tenant=<uuid> query param or X-Tenant-ID header",
        )
    return t


def _state(request: Request) -> Any:
    return request.app.state


@router.post("/v1/conversations", status_code=status.HTTP_201_CREATED)
async def create_conversation(
    body: models.ConversationCreate, request: Request
) -> models.Conversation:
    db = _state(request).db
    row = await db.create_conversation(
        body.tenant_id, body.site_slug, body.channel, body.contact_phone
    )
    return models.Conversation(**dict(row))


@router.get("/v1/conversations")
async def list_conversations(
    request: Request,
    tenant_id: Annotated[uuid.UUID, Depends(_require_tenant)],
    limit: int = Query(default=50, ge=1, le=200),
    offset: int = Query(default=0, ge=0),
    contact: str | None = Query(default=None),
) -> dict[str, Any]:
    db = _state(request).db
    rows = await db.list_conversations(tenant_id, limit, offset, contact)
    return {
        "conversations": [models.Conversation(**dict(r)).model_dump(mode="json") for r in rows],
        "limit": limit,
        "offset": offset,
    }


@router.get("/v1/conversations/{conversation_id}")
async def get_conversation(
    conversation_id: uuid.UUID,
    request: Request,
    tenant_id: Annotated[uuid.UUID, Depends(_require_tenant)],
) -> models.ConversationWithTurns:
    db = _state(request).db
    try:
        conv = await db.get_conversation(conversation_id, tenant_id)
    except NotFoundError:
        raise HTTPException(
            status.HTTP_404_NOT_FOUND, f"conversation {conversation_id} not found"
        ) from None
    turns = await db.list_turns(conversation_id, tenant_id)
    return models.ConversationWithTurns(
        **dict(conv), turns=[models.Turn(**_turn_dict(t)) for t in turns]
    )


def _turn_dict(row: Any) -> dict[str, Any]:
    d = dict(row)
    if isinstance(d.get("tool_calls"), str):
        import json

        d["tool_calls"] = json.loads(d["tool_calls"])
    if isinstance(d.get("entities"), str):
        import json

        d["entities"] = json.loads(d["entities"])
    return d


@router.post("/v1/conversations/{conversation_id}/turns", status_code=status.HTTP_201_CREATED)
async def add_turn(
    conversation_id: uuid.UUID,
    body: models.TurnCreate,
    request: Request,
    response: Response,
    tenant_id: Annotated[uuid.UUID, Depends(_require_tenant)],
    idempotency_key: Annotated[str | None, Header()] = None,
) -> models.TurnCreated:
    st = _state(request)

    # Call intelligence (SPEC-W3 §4, innovation 3): lexicon sentiment always;
    # optional LLM NER when INTEL_LLM=on (failure degrades to lexicon-only).
    enrichment = await intel.enrich_turn(body.text, st.cfg)

    try:
        row, created = await st.db.add_turn(
            conversation_id, tenant_id, body.role, body.text, body.tool_calls,
            sentiment=enrichment["sentiment"],
            intent=enrichment["intent"],
            entities=enrichment["entities"],
            idempotency_key=idempotency_key,
        )
    except asyncpg.ForeignKeyViolationError:
        raise HTTPException(
            status.HTTP_404_NOT_FOUND, f"conversation {conversation_id} not found"
        ) from None
    except asyncpg.InsufficientPrivilegeError:
        # RLS denied: conversation belongs to another tenant
        raise HTTPException(
            status.HTTP_404_NOT_FOUND, f"conversation {conversation_id} not found"
        ) from None

    turn = models.Turn(**_turn_dict(row))

    # SPEC-W3 §3: Idempotency-Key replay — return the original turn with
    # 200 and do NOT re-publish sink/Dapr/enriched events (exactly-once
    # semantics for the caller).
    if not created:
        response.status_code = status.HTTP_200_OK
        return models.TurnCreated(turn=turn)

    # Fetch site_slug for the event subject (plus channel/contact for the
    # SPEC-W11 incident IDP context).
    site_slug = ""
    conv_channel = "voice"
    contact_phone = None
    try:
        conv = await st.db.get_conversation(conversation_id, tenant_id)
        site_slug = conv["site_slug"]
        conv_channel = conv["channel"]
        contact_phone = conv["contact_phone"]
    except Exception:
        pass

    # 1) raw record to the high-throughput transcript sink (Fluvio/Kafka)
    raw = {
        "conversationId": str(conversation_id),
        "tenantId": str(tenant_id),
        "role": turn.role,
        "text": turn.text,
        "ts": turn.ts.isoformat(),
    }
    try:
        await st.sink.publish(raw)
    except Exception as exc:
        st.log.error("transcript sink publish failed", error=str(exc),
                     conversation_id=str(conversation_id))

    # 2) CloudEvent to Kafka via Dapr pubsub `pubsub-kafka` (always, SPEC §4)
    event = events.conversation_turn_event(
        conversation_id=conversation_id,
        tenant_id=tenant_id,
        site_slug=site_slug,
        role=turn.role,
        text=turn.text,
        ts=turn.ts,
        audio_url=body.audio_url,
    )
    try:
        await st.dapr.publish_event(st.cfg.transcripts_topic, event)
    except Exception as exc:
        st.log.error("dapr transcript publish failed", error=str(exc),
                     conversation_id=str(conversation_id))

    # 3) Enriched turn to opendesk.conversation.enriched via aiokafka
    #    (SPEC-W3 §4, innovation 3; best-effort like the raw sink).
    enriched = {
        "conversationId": str(conversation_id),
        "tenantId": str(tenant_id),
        "siteSlug": site_slug,
        "seq": turn.seq,
        "role": turn.role,
        "text": turn.text,
        "sentiment": turn.sentiment,
        "sentimentLabel": enrichment["sentiment_label"],
        "intent": turn.intent,
        "entities": turn.entities,
        "ts": turn.ts.isoformat(),
    }
    try:
        await st.intel_sink.publish(enriched)
    except Exception as exc:
        st.log.error("enriched turn publish failed", error=str(exc),
                     conversation_id=str(conversation_id))

    # 4) SPEC-W11 Part A: emergency-intent detection on USER turns. The
    #    lexicon classify is cheap and inline; only when the score crosses
    #    INCIDENT_MIN_SCORE do we schedule IDP build+emit as a background
    #    asyncio task (non-blocking; emit_for_turn logs and never raises).
    #    Idempotency-Key replays return above (created=False), so this runs
    #    exactly once per persisted turn; emission itself also dedupes per
    #    conversation_id+turn_id.
    if st.cfg.incident_enabled and turn.role == "user":
        hit, _ = incidents.is_emergency(turn.text, st.cfg.incident_min_score)
        if hit:
            tasks = getattr(st, "background_tasks", None)
            if tasks is None:
                tasks = set()
                st.background_tasks = tasks
            task = asyncio.create_task(
                incidents.emit_for_turn(
                    cfg=st.cfg,
                    db=st.db,
                    dapr=st.dapr,
                    tenant_id=tenant_id,
                    conversation_id=conversation_id,
                    turn_id=turn.id,
                    text=turn.text,
                    channel=conv_channel,
                    site_slug=site_slug,
                    contact_phone=contact_phone,
                    captured_at=turn.ts,
                ),
                name=f"incident-idp-{turn.id}",
            )
            tasks.add(task)
            task.add_done_callback(tasks.discard)

    return models.TurnCreated(turn=turn)
