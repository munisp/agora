"""REST API: conversations + turns (SPEC §7 conversation schema)."""

from __future__ import annotations

import asyncio
import uuid
from typing import Annotated, Any

import asyncpg
from fastapi import APIRouter, Depends, Header, HTTPException, Query, Request, Response, status

from . import events, incidents, intel, models, redact, ussd
from .db import NotFoundError
from .tenants import TenantNotFoundError, TenantResolutionError

router = APIRouter()


def _tenant_header(x_tenant_id: Annotated[str | None, Header()] = None) -> uuid.UUID | None:
    if x_tenant_id is None:
        return None
    try:
        return uuid.UUID(x_tenant_id)
    except ValueError:
        raise HTTPException(status.HTTP_400_BAD_REQUEST, "invalid X-Tenant-ID header") from None


def _parse_tenant_slugs(raw: str | None) -> list[str]:
    """Gateway-injected X-Tenant-Slugs header (comma-separated slugs/uuids
    from the validated JWT tenant_slugs claim — SPEC-W43 C1)."""
    if not raw:
        return []
    return [s.strip() for s in raw.split(",") if s.strip()]


def _tenant_slugs_header(
    x_tenant_slugs: Annotated[str | None, Header()] = None,
) -> list[str]:
    return _parse_tenant_slugs(x_tenant_slugs)


async def _resolve_tenant_value(request: Request, value: str) -> uuid.UUID:
    """Resolve a tenant selector (UUID fast path, else slug via identity)."""
    try:
        return uuid.UUID(value)
    except ValueError:
        pass
    resolver = getattr(request.app.state, "tenant_resolver", None)
    if resolver is None:
        raise HTTPException(
            status.HTTP_503_SERVICE_UNAVAILABLE,
            "tenant slug resolution unavailable",
        )
    try:
        info = await resolver.by_slug(value)
    except TenantNotFoundError:
        raise HTTPException(
            status.HTTP_404_NOT_FOUND, f"tenant {value!r} not found"
        ) from None
    except TenantResolutionError as exc:
        raise HTTPException(status.HTTP_502_BAD_GATEWAY, str(exc)) from None
    return uuid.UUID(info.id)


def _trust_direct(request: Request) -> bool:
    """SPEC-W43 C1 standalone-dev escape (OPENDESK_TRUST_DIRECT_TENANT=1;
    default OFF, never set in compose)."""
    cfg = getattr(request.app.state, "cfg", None)
    return bool(cfg is not None and getattr(cfg, "trust_direct_tenant", False))


async def _require_tenant(
    request: Request,
    tenant: Annotated[str | None, Query()] = None,
    header_tenant: uuid.UUID | None = Depends(_tenant_header),
    tenant_slugs: list[str] = Depends(_tenant_slugs_header),
) -> uuid.UUID:
    """Tenant scope per SPEC-W43 C1 (gateway tenant binding).

    Tenant context comes from the gateway-injected X-Tenant-Slugs header
    (comma-separated slugs/uuids from the validated JWT). An explicit
    ?tenant= (or X-Tenant-ID) selector is honored ONLY when it exactly
    matches one of the header entries — otherwise 403. Without an explicit
    selector a single-entry header unambiguously selects that tenant; a
    multi-tenant principal must name one (400 otherwise).

    Standalone dev escape: with OPENDESK_TRUST_DIRECT_TENANT=1 (never set in
    compose) the legacy behavior is restored — ?tenant=<uuid-or-slug> or
    X-Tenant-ID selects the tenant directly.

    J-14 fail-closed rule (SPEC-W42) still holds: a request whose tenant
    context cannot be resolved NEVER reaches a tenant-scoped query with
    app.tenant_id unset — missing scope is 401, an ungranted selector is
    403, an unknown slug is 404, identity outage with no cache is 502.
    """
    if tenant_slugs:
        explicit = tenant
        if explicit is None and header_tenant is not None:
            explicit = str(header_tenant)
        if explicit is not None:
            if explicit not in tenant_slugs:
                raise HTTPException(
                    status.HTTP_403_FORBIDDEN,
                    "tenant selector not granted to the authenticated "
                    "principal (must match X-Tenant-Slugs)",
                )
            return await _resolve_tenant_value(request, explicit)
        if len(tenant_slugs) > 1:
            raise HTTPException(
                status.HTTP_400_BAD_REQUEST,
                "principal has multiple tenants; pass ?tenant= naming one "
                "of the X-Tenant-Slugs entries",
            )
        return await _resolve_tenant_value(request, tenant_slugs[0])

    # No gateway header: only the documented dev escape permits direct
    # tenant selection; otherwise there is no authenticated tenant context.
    if not _trust_direct(request):
        raise HTTPException(
            status.HTTP_401_UNAUTHORIZED,
            "tenant context required: X-Tenant-Slugs header (injected by "
            "the gateway from the validated JWT)",
        )
    if tenant is not None:
        return await _resolve_tenant_value(request, tenant)
    if header_tenant is None:
        raise HTTPException(
            status.HTTP_401_UNAUTHORIZED,
            "tenant scope required: ?tenant=<uuid-or-slug> query param or "
            "X-Tenant-ID header",
        )
    return header_tenant


def _bind_body_tenant(request: Request, tenant_id: uuid.UUID) -> uuid.UUID:
    """SPEC-W43 C1 for request bodies (POST /v1/conversations): body
    tenant_id is honored only when it exactly matches an X-Tenant-Slugs
    entry (else 403); without the gateway header the dev escape governs."""
    slugs = _parse_tenant_slugs(request.headers.get("x-tenant-slugs"))
    if slugs:
        if str(tenant_id) not in slugs:
            raise HTTPException(
                status.HTTP_403_FORBIDDEN,
                "body tenant_id not granted to the authenticated principal "
                "(must match X-Tenant-Slugs)",
            )
        return tenant_id
    if not _trust_direct(request):
        raise HTTPException(
            status.HTTP_401_UNAUTHORIZED,
            "tenant context required: X-Tenant-Slugs header (injected by "
            "the gateway from the validated JWT)",
        )
    return tenant_id


def _state(request: Request) -> Any:
    return request.app.state


@router.post("/v1/conversations", status_code=status.HTTP_201_CREATED)
async def create_conversation(
    body: models.ConversationCreate, request: Request
) -> models.Conversation:
    # SPEC-W43 C1: body.tenant_id is bound to the gateway-injected
    # X-Tenant-Slugs header (exact match) — no unauthenticated tenant
    # selection (dev escape: OPENDESK_TRUST_DIRECT_TENANT=1).
    tenant_id = _bind_body_tenant(request, body.tenant_id)
    db = _state(request).db
    row = await db.create_conversation(
        tenant_id, body.site_slug, body.channel, body.contact_phone
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
    turn, created = await _persist_turn(
        _state(request),
        conversation_id,
        tenant_id,
        body.role,
        body.text,
        tool_calls=body.tool_calls,
        audio_url=body.audio_url,
        idempotency_key=idempotency_key,
    )

    # SPEC-W3 §3: Idempotency-Key replay — return the original turn with
    # 200 and do NOT re-publish sink/Dapr/enriched events (exactly-once
    # semantics for the caller).
    if not created:
        response.status_code = status.HTTP_200_OK
    return models.TurnCreated(turn=turn)


def _spawn_background(st: Any, coro: Any, *, name: str) -> None:
    """Track a fire-and-forget task on app state (W46-F P5/P10).

    Same registry pattern as the incident IDP / helpdesk escalation tasks:
    the set keeps a strong reference until completion and the done callback
    logs unexpected failures (never raised onto the request path).
    """
    tasks = getattr(st, "background_tasks", None)
    if tasks is None:
        tasks = set()
        st.background_tasks = tasks
    task = asyncio.create_task(coro, name=name)
    tasks.add(task)

    def _done(t: asyncio.Task) -> None:
        tasks.discard(t)
        if t.cancelled():
            return
        exc = t.exception()
        if exc is not None:
            st.log.error("background task failed", task=name, error=str(exc))

    task.add_done_callback(_done)


async def _llm_enrich_and_publish(
    st: Any,
    *,
    turn_id: uuid.UUID,
    text: str,
    tenant_id: uuid.UUID,
    conversation_id: uuid.UUID,
    enriched: dict[str, Any],
) -> None:
    """W46-F P10: post-response LLM NER → turn write-back → enriched publish.

    Runs ONLY when INTEL_LLM=on (the caller persists the turn lexicon-only).
    llm_extract never raises (None on failure → lexicon-only enriched event);
    the DB write-back and the sink publish are best-effort with error logs,
    exactly like the previous inline publish.
    """
    ner = await intel.llm_extract(
        text, st.cfg, client=getattr(st, "intel_client", None)
    )
    if ner is not None:
        enriched["intent"] = ner.get("intent")
        enriched["entities"] = ner.get("entities") or {}
        try:
            await st.db.update_turn_intel(
                turn_id,
                tenant_id,
                intent=enriched["intent"],
                entities=enriched["entities"],
            )
        except Exception as exc:  # noqa: BLE001 — best-effort write-back
            st.log.error("turn intel write-back failed", error=str(exc),
                         conversation_id=str(conversation_id))
    try:
        await st.intel_sink.publish(enriched)
    except Exception as exc:  # noqa: BLE001 — best-effort like the raw sink
        st.log.error("enriched turn publish failed", error=str(exc),
                     conversation_id=str(conversation_id))


async def _persist_turn(
    st: Any,
    conversation_id: uuid.UUID,
    tenant_id: uuid.UUID,
    role: str,
    text: str,
    tool_calls: list[dict[str, Any]] | None = None,
    audio_url: str | None = None,
    idempotency_key: str | None = None,
) -> tuple[models.Turn, bool]:
    """Enrich + persist + fan out one turn; returns (turn, created).

    Shared by the REST turns endpoint and the SPEC-W12 USSD inbound hook so
    every channel gets identical enrichment, event publication and incident
    classification. created=False (Idempotency-Key replay) skips ALL side
    effects, exactly like the REST path.
    """

    # Call intelligence (SPEC-W3 §4, innovation 3): lexicon sentiment always.
    # W46-F P10: when INTEL_LLM=on the LLM NER call runs POST-RESPONSE in a
    # background task (shared app-lifetime httpx client) — never on the turn
    # path; the turn is persisted lexicon-only and updated when NER lands.
    if st.cfg.intel_llm:
        _sent = intel.analyze_sentiment(text)
        enrichment = {
            "sentiment": _sent["score"],
            "sentiment_label": _sent["label"],
            "intent": None,
            "entities": None,
        }
    else:
        enrichment = await intel.enrich_turn(text, st.cfg)

    # Conversation context for the event subject + incident IDP. Fetched
    # BEFORE the insert because the outbox payload is built inside the turn
    # transaction (SPEC-W43 Y-08).
    try:
        conv = await st.db.get_conversation(conversation_id, tenant_id)
    except NotFoundError:
        raise HTTPException(
            status.HTTP_404_NOT_FOUND, f"conversation {conversation_id} not found"
        ) from None
    site_slug = conv["site_slug"]
    conv_channel = conv["channel"]
    contact_phone = conv["contact_phone"]

    # SPEC-W34 GF3: this Dapr path bypasses the Fluvio pii-redact
    # smartmodule, so phone/email PII is redacted BEFORE publishing — the
    # event lands directly in Iceberg bronze.transcripts. The conversation
    # DB keeps the original text; only the published event is redacted.
    event_holder: dict[str, Any] = {}

    def _build_turn_event(turn_row: Any) -> dict[str, Any]:
        event = events.conversation_turn_event(
            conversation_id=conversation_id,
            tenant_id=tenant_id,
            site_slug=site_slug,
            role=turn_row["role"],
            text=redact.redact_text(turn_row["text"]),
            ts=turn_row["ts"],
            audio_url=audio_url,
            redacted=True,
        )
        event_holder["event"] = event
        return event

    try:
        row, created, outbox_id = await st.db.add_turn(
            conversation_id, tenant_id, role, text, tool_calls,
            sentiment=enrichment["sentiment"],
            intent=enrichment["intent"],
            entities=enrichment["entities"],
            idempotency_key=idempotency_key,
            outbox=(st.cfg.transcripts_topic, _build_turn_event),
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

    # Idempotency-Key replay — no re-publish of sink/Dapr/enriched events.
    if not created:
        return turn, False

    # 1)+2) W46-F P5: the raw transcript sink publish and the Dapr CloudEvent
    #    publish fan out CONCURRENTLY (previously 2 sequential network RTTs on
    #    the request path). Per-publish error handling is unchanged — each
    #    leg logs and degrades independently.
    raw = {
        "conversationId": str(conversation_id),
        "tenantId": str(tenant_id),
        "role": turn.role,
        "text": turn.text,
        "ts": turn.ts.isoformat(),
    }
    # CloudEvent to Kafka via Dapr pubsub `pubsub-kafka` (always, SPEC §4).
    # SPEC-W43 Y-08: the event was already persisted to
    # conversation_outbox in the SAME tx as the turn insert. The inline
    # publish marks the row sent on success; on failure the row stays
    # unsent and the OutboxRelay republishes with backoff — a crash or
    # broker outage can never silently lose a transcript event.
    event = event_holder["event"]
    sink_res, dapr_res = await asyncio.gather(
        st.sink.publish(raw),
        st.dapr.publish_event(st.cfg.transcripts_topic, event),
        return_exceptions=True,
    )
    if isinstance(sink_res, Exception):
        st.log.error("transcript sink publish failed", error=str(sink_res),
                     conversation_id=str(conversation_id))
    if isinstance(dapr_res, Exception):
        st.log.error("dapr transcript publish failed; outbox relay will retry",
                     error=str(dapr_res), conversation_id=str(conversation_id))
    elif outbox_id is not None:
        # W46-F P5: outbox_mark_sent off the response path (background task,
        # not awaited). The row is durable; a lost/crashed mark just means
        # the relay republishes — at-least-once semantics are unchanged.
        _spawn_background(
            st,
            st.db.outbox_mark_sent(outbox_id, tenant_id),
            name=f"outbox-mark-sent-{outbox_id}",
        )

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
    if st.cfg.intel_llm:
        # W46-F P10: INTEL_LLM=on — the LLM NER call + turn write-back +
        # enriched publish run post-response in a background task (shared
        # app-lifetime httpx client). The enriched event is still published
        # exactly once per persisted turn, now carrying the NER results
        # (lexicon-only intent/entities=None when the LLM call fails).
        _spawn_background(
            st,
            _llm_enrich_and_publish(
                st,
                turn_id=turn.id,
                text=text,
                tenant_id=tenant_id,
                conversation_id=conversation_id,
                enriched=enriched,
            ),
            name=f"intel-ner-{turn.id}",
        )
    else:
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

    # 5) SPEC-W45 UC helpdesk automation: a human-escalation USER turn opens
    #    a real helpdesk ticket on booking-service (POST /v1/helpdesk/tickets
    #    with X-Internal-Token, tenant-bound). Non-blocking background task
    #    like the incident IDP; Idempotency-Key replays returned above, so
    #    this fires exactly once per persisted turn (and at most once per
    #    conversation per process — HelpdeskAutomation dedupes).
    if turn.role == "user":
        helpdesk_auto = getattr(st, "helpdesk", None)
        if helpdesk_auto is not None:
            tasks = getattr(st, "background_tasks", None)
            if tasks is None:
                tasks = set()
                st.background_tasks = tasks
            helpdesk_auto.maybe_escalate(
                tenant_id=tenant_id,
                conversation_id=conversation_id,
                turn_id=turn.id,
                text=turn.text,
                intent=turn.intent,
                channel=conv_channel,
                contact_phone=contact_phone,
                background_tasks=tasks,
            )

    return turn, True


# ---------------------------------------------------------------------------
# SPEC-W12: USSD inbound hook (contract §1/§2)
# ---------------------------------------------------------------------------


@router.post("/v1/ussd/turns")
async def ussd_turn(body: ussd.UssdTurnRequest, request: Request) -> dict[str, Any]:
    """Synchronous USSD callback hook invoked by messaging-gateway via Dapr.

    One Africa's Talking callback in → one user turn appended to the
    session's conversation (deterministic uuid5(tenant, sessionId) key,
    channel="ussd") → the reply text out in the response body; the gateway
    renders ``CON ``/``END `` (see app/ussd.py for the full contract).

    Tenant scope comes from the invoke body (service-to-service call, like
    the Dapr pubsub deliveries — no X-Tenant-ID header on Dapr invoke).
    USER turns pass through the unchanged _persist_turn path, so the
    SPEC-W11 incident classifier applies verbatim (ussd is mapped web-like
    in the IDP); the agent reply turn is never classified (existing rule).
    """
    st = _state(request)
    if not st.cfg.ussd_enabled:
        raise HTTPException(status.HTTP_503_SERVICE_UNAVAILABLE, "ussd channel disabled")

    tenant_id = body.tenant_id
    conv_id = ussd.session_conversation_id(tenant_id, body.session_id)

    # Get-or-create the session conversation (idempotent on the
    # deterministic key — retried/duplicate first callbacks are safe).
    try:
        await st.db.get_conversation(conv_id, tenant_id)
    except NotFoundError:
        await st.db.create_conversation(
            tenant_id, body.site_slug, ussd.CHANNEL, body.phone_number,
            conversation_id=conv_id,
        )

    idem = ussd.idempotency_key(body)
    await _persist_turn(
        st, conv_id, tenant_id, "user", ussd.user_turn_text(body),
        idempotency_key=idem,
    )

    reply, continue_session, selected = ussd.build_reply(
        body, st.cfg.ussd_text_mode_reply
    )
    payload = ussd.response_payload(
        conversation_id=conv_id,
        reply=reply,
        continue_session=continue_session,
        body=body,
        selected=selected,
    )
    # W46-F P5: record the reply as an agent turn (deduped on the same
    # callback key, mirroring the telegram/whatsapp bridge's user-turn →
    # agent-turn pair) in a BACKGROUND task AFTER the response — the USSD
    # 1s turn budget no longer pays a second enrich+insert+fan-out. Order
    # is preserved: the user turn committed above, so seq ordering is
    # unchanged; the idempotency key keeps AT callback replays exact-once.
    # Failures are logged by the background-task done callback (previously
    # a failed agent persist 500'd an already-answered callback).
    _spawn_background(
        st,
        _persist_turn(
            st, conv_id, tenant_id, "agent", reply,
            idempotency_key=idem + ":reply",
        ),
        name=f"ussd-agent-reply-{conv_id}",
    )
    return payload
