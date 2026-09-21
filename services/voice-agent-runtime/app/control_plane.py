"""FastAPI control plane (port 7006, SPEC §11).

- GET  /healthz
- POST /voice/session {site_slug} -> LiveKit access token (livekit backend)
         or ElevenLabs signed URL (elevenlabs backend).
         SPEC-W45 K15(a): the room is PER-SESSION unique
         (site-{slug}-{uuid4hex}) and the grant is scoped to that room
         only — no shared "party line" room (OOS-01).
- POST /voice/chat {site_slug, message, conversation_id?, session_secret?}
         -> text-in/text-out through the same tool layer (no audio).
         SPEC-W45 K15(b): resume requires session_secret (returned at
         creation); conversation_id alone starts a FRESH session.
         SPEC-W45 K15(c): channel_identity (provider-verified, e.g.
         WhatsApp wa_id) is pinned as the verified caller phone ONLY when
         the request carries a valid X-Internal-Token (in-cluster callers;
         the gateway strips client-supplied tokens).
- POST /voice/elevenlabs/tools -> ElevenLabs tool webhook passthrough
         (mounted only when AGENT_BACKEND=elevenlabs)
- GET  /voice/voices -> aggregated TTS provider/voice catalog (SPEC-W10)
- POST /voice/tts-preview {text, language?, provider?, voice?} -> audio/wav
         through the same FallbackTTS chain (SPEC-W10)
- POST /voice-admin/voices/enroll {name, sample_base64, tenant} -> {voice_id}
         SPEC-W45 K15(d) + verifier F-2: XTTS brand-voice enrollment moved
         off the public /voice/* prefix; guarded by _require_admin_access —
         EITHER X-Internal-Token == VOICE_ADMIN_INTERNAL_TOKEN (service-to-
         service; fail-closed 503 unset / 401 wrong) OR the gateway-injected
         X-User-Roles carrying staff|admin|platform-admin (human path via
         the APISIX api-voice-admin route: OIDC + staff role gate + K1
         header injection). CODER-F removes the public route and adds the
         jwt-auth APISIX /api/voice-admin/* route for staff.
- POST /voice-admin/escalations/{conversation_id}/staff-token -> {room, token}
         SPEC-W45 K15(e): on-demand staff join token for an escalation
         room — replaces publishing the token on the events topic.
         Same _require_admin_access dual guard as enrollment (F-2: the
         admin-web bookings-client reaches it via the gateway too).
- GET  /metrics -> Prometheus text exposition (voice_* inference series)
"""

from __future__ import annotations

import asyncio
import base64
import binascii
import hmac
import json
import uuid
from datetime import timedelta
from typing import Any

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import PlainTextResponse, Response, StreamingResponse
from pydantic import BaseModel, Field

from . import metrics, sip
from .avatar import PROVIDER_NONE, create_provider, resolve_provider_name
from .chat import ChatService
from .config import Settings, load_settings
from .dapr_client import DaprClient
from .elevenlabs_adapter import ElevenLabsBackend
from .escalation import LiveKitEscalation, escalation_room_name
from .livekit_worker import ROOM_PREFIX
from .logging import configure_logging, get_logger
from .multilang import resolve_tts_voice
from .pipeline.llm import build_llm
from .session_state import SessionStore
from .tts_providers.chain import build_fallback_tts

log = get_logger("control-plane")


class SessionRequest(BaseModel):
    site_slug: str = Field(min_length=1)
    participant_name: str | None = None


class ChatRequest(BaseModel):
    site_slug: str = Field(min_length=1)
    message: str = Field(min_length=1)
    conversation_id: str | None = None
    # SPEC-W45 K15(b): resume credential issued at session creation
    # (returned as `session_secret` in every chat response). A
    # conversation_id presented WITHOUT the matching secret NEVER resumes
    # confirmed/verified phone, history or escalation state — the server
    # starts a fresh session instead.
    session_secret: str | None = None
    # SPEC-W3 §3: stream=true switches the response to text/event-stream.
    stream: bool = False
    # Wave 5 #8 A/B prompt testing: candidate persona for eval/ab_test.py.
    # Honored ONLY when EVAL_PERSONA_OVERRIDE=true on the server (off by
    # default — on a public endpoint this would be a prompt-injection hole).
    persona_override: str | None = None
    # SPEC-W6 Part A: omnichannel inbound — which channel the message
    # arrived on ("web" default for existing callers; "whatsapp"/"telegram"
    # from the messaging-gateway bridge). Additive only; threaded into
    # session metadata + turn logging, no behavioral change.
    channel: str = "web"
    # SPEC-W45 K15(c): provider-verified channel identity (WhatsApp wa_id).
    # Honored ONLY when the request also carries a valid X-Internal-Token
    # (in-cluster messaging-gateway bridge; APISIX strips client-supplied
    # tokens) AND channel names a provider-verified source. Otherwise
    # silently ignored — the caller faces the normal OTP gate.
    channel_identity: str | None = None


class TtsPreviewRequest(BaseModel):
    """SPEC-W10: admin voice preview through the FallbackTTS chain."""

    text: str = Field(min_length=1)
    language: str | None = None
    provider: str | None = None
    voice: str | None = None  # may be provider-qualified ("mms:pcm")


class VoiceEnrollRequest(BaseModel):
    """SPEC-W10: XTTS brand-voice enrollment (admin path; consent gate lives
    in the admin UI — see docs/voices.md)."""

    name: str = Field(min_length=1)
    sample_base64: str = Field(min_length=1)
    tenant: str = Field(min_length=1)


class VoiceSessionResponse(BaseModel):
    backend: str
    room: str | None = None
    url: str | None = None
    token: str | None = None
    signed_url: str | None = None
    # SPEC-W9 Part A (additive): avatar presence for this session, e.g.
    # {"provider": "tavus", "status": "joining"}. None when AVATAR_PROVIDER
    # is `none` (default) — the client renders audio-only in that case.
    avatar: dict | None = None


def create_app(settings: Settings | None = None) -> FastAPI:
    settings = settings or load_settings()
    configure_logging(settings.log_level)

    dapr = DaprClient(settings.dapr_base_url, settings.http_timeout_s)
    sessions = SessionStore()
    # Primary LLM endpoint + optional circuit-broken fallback chain
    # (LLM_FALLBACK_* envs, VOICE-SCALING §3).
    llm = build_llm(settings)
    chat_service = ChatService(settings=settings, dapr=dapr, llm=llm, sessions=sessions)
    # SPEC-W10 Part A: TTS provider fallback chain (default "piper" =
    # pre-W10 behavior). Backs the /voice/voices, /voice/tts-preview and
    # /voice/voices/enroll endpoints below.
    tts_chain = build_fallback_tts(settings)
    elevenlabs = (
        ElevenLabsBackend(settings=settings, dapr=dapr, sessions=sessions)
        if settings.agent_backend == "elevenlabs"
        else None
    )

    app = FastAPI(title="OpenDesk voice-agent-runtime", version="0.1.0")

    @app.on_event("shutdown")
    async def _shutdown() -> None:
        await dapr.aclose()
        await tts_chain.aclose()
        if elevenlabs is not None:
            await elevenlabs.aclose()

    @app.get("/healthz")
    async def healthz() -> dict[str, Any]:
        return {
            "status": "ok",
            "service": "voice-agent-runtime",
            "backend": settings.agent_backend,
        }

    @app.get("/metrics")
    async def prometheus_metrics() -> PlainTextResponse:
        """Hand-rolled Prometheus text exposition (VOICE-SCALING §3)."""
        return PlainTextResponse(
            metrics.render(), media_type="text/plain; version=0.0.4; charset=utf-8"
        )

    @app.post("/voice/session", response_model=VoiceSessionResponse)
    async def create_voice_session(req: SessionRequest) -> VoiceSessionResponse:
        if settings.agent_backend == "elevenlabs":
            assert elevenlabs is not None
            try:
                payload = await elevenlabs.get_signed_url()
            except Exception as exc:  # noqa: BLE001
                log.warning("elevenlabs signed url failed", error=str(exc))
                raise HTTPException(status_code=502, detail=str(exc)) from exc
            return VoiceSessionResponse(
                backend="elevenlabs", signed_url=payload.get("signed_url")
            )

        # LiveKit backend: mint an access token for a PER-SESSION unique
        # room `site-{slug}-{uuid4hex}` (K15(a)/OOS-01: the legacy shared
        # `site-{slug}` room let any token holder join and eavesdrop on
        # every concurrent caller of the same tenant — a "party line").
        # The grant is scoped to exactly this room (no room_list/admin,
        # no wildcard) so a leaked token cannot roam other tenants' rooms.
        from livekit import api as lk_api

        room = f"{ROOM_PREFIX}{req.site_slug}-{uuid.uuid4().hex}"
        identity = f"web-{uuid.uuid4().hex[:8]}"
        token = (
            lk_api.AccessToken(settings.livekit_api_key, settings.livekit_api_secret)
            .with_identity(identity)
            .with_name(req.participant_name or "Caller")
            .with_grants(
                lk_api.VideoGrants(
                    room_join=True,
                    room=room,
                    can_publish=True,
                    can_subscribe=True,
                )
            )
            .with_ttl(timedelta(minutes=30))
            .to_jwt()
        )
        log.info("livekit session token minted", room=room, identity=identity)

        # SPEC-W9 Part A: optional avatar presence. Fire-and-forget — the
        # provider join is a background task; misconfiguration is reported
        # synchronously via check_ready, everything else degrades in the
        # background (warning log, audio-only session). Never blocks or
        # fails session creation.
        avatar_payload: dict | None = None
        provider_name = resolve_provider_name(settings, tenant_ctx=None)
        provider = (
            create_provider(provider_name, settings)
            if provider_name != PROVIDER_NONE
            else None
        )
        if provider is not None:
            not_ready = provider.check_ready()
            if not_ready:
                log.warning(
                    "avatar provider not ready", provider=provider_name, detail=not_ready
                )
                avatar_payload = {
                    "provider": provider_name,
                    "status": "unavailable",
                    "detail": not_ready,
                }
            else:
                asyncio.create_task(provider.join_room(room, tenant_ctx=None))
                avatar_payload = {"provider": provider_name, "status": "joining"}

        return VoiceSessionResponse(
            backend="livekit",
            room=room,
            url=settings.livekit_url,
            token=token,
            avatar=avatar_payload,
        )

    # SPEC-W45 K15(c): channels whose provider cryptographically verifies
    # the sender identity (WhatsApp wa_id via the messaging-gateway).
    PROVIDER_VERIFIED_CHANNELS = frozenset({"whatsapp", "telegram"})

    def _valid_internal_token(request: Request) -> bool:
        """Constant-time check against VOICE_ADMIN_INTERNAL_TOKEN. False
        when unset (fail closed) — never raises; used for soft authorization
        (channel-identity pinning) where the public path must keep working."""
        token = settings.voice_admin_internal_token
        if not token:
            return False
        supplied = request.headers.get("X-Internal-Token", "")
        return bool(supplied) and hmac.compare_digest(supplied, token)

    # SPEC-W45 K15(d)/(e) + verifier F-2: staff-grade realm roles accepted
    # on the gateway human path (APISIX api-voice-admin route: OIDC
    # bearer_only + staff|admin|platform-admin role gate + SPEC-W44 K1
    # injection of X-User-Roles from the verified JWT).
    ADMIN_GATEWAY_ROLES = frozenset({"staff", "admin", "platform-admin"})

    def _gateway_admin_role_present(request: Request) -> bool:
        """True when the gateway-injected X-User-Roles header carries a
        staff-grade role.

        TRUST BOUNDARY (F-2, R1-adjacent residual — read before reusing):
        this header is trusted ONLY because the APISIX boundary makes it
        unspoofable by clients: global rule 1 strips any client-supplied
        x-user-roles before route plugins run, and the api-voice-admin
        route re-injects the claim-derived value after OIDC verification.
        An IN-CLUSTER caller that reaches this service directly (bypassing
        the gateway) could still spoof X-User-Roles absent a networkPolicy
        pinning ingress to APISIX — that residual is accepted for the
        human path; the X-Internal-Token path below remains the strong
        path for service-to-service calls."""
        raw = request.headers.get("X-User-Roles", "")
        roles = {r.strip().lower() for r in raw.split(",") if r.strip()}
        return bool(roles & ADMIN_GATEWAY_ROLES)

    def _require_admin_access(request: Request) -> None:
        """Hard guard for /voice-admin/* (K15(d)/(e); verifier F-2 fix).

        EITHER path authorizes:
        (1) X-Internal-Token == VOICE_ADMIN_INTERNAL_TOKEN — the
            service-to-service path (e.g. messaging-gateway), constant-time
            compare. Fail-closed 503 when the env is UNSET and no other
            auth applies (the 503 only fires on the token path; a valid
            gateway role never depends on the token being configured).
        (2) X-User-Roles containing staff|admin|platform-admin — the human
            path via the APISIX api-voice-admin route (OIDC + staff role
            gate + K1 header injection; admin-web voices-client /
            bookings-client call through the gateway). See
            _gateway_admin_role_present for the trust-boundary caveat.
        401 when neither path authorizes. (F-2 root cause: APISIX strips
        client X-Internal-Token and injects nothing in its place, so a
        token-only guard 401'd EVERY gateway-authenticated staff call.)
        """
        if _gateway_admin_role_present(request):
            return
        if not settings.voice_admin_internal_token:
            log.error(
                "voice-admin route hit but VOICE_ADMIN_INTERNAL_TOKEN is "
                "unset (fail closed)"
            )
            raise HTTPException(
                status_code=503,
                detail="voice admin not configured (VOICE_ADMIN_INTERNAL_TOKEN unset)",
            )
        supplied = request.headers.get("X-Internal-Token", "")
        if not supplied or not hmac.compare_digest(
            supplied, settings.voice_admin_internal_token
        ):
            raise HTTPException(
                status_code=401,
                detail="admin access required (X-Internal-Token or staff gateway role)",
            )

    @app.post("/voice/chat")
    async def voice_chat(req: ChatRequest, request: Request) -> Any:
        # K15(c): pin the provider-verified channel identity (wa_id) as the
        # session's verified phone — but ONLY for an internal-token-
        # authorized in-cluster caller on a provider-verified channel. A
        # public web caller cannot forge it (APISIX strips client-supplied
        # X-Internal-Token, and the value is ignored without a match).
        pin: str | None = None
        if (
            req.channel_identity
            and req.channel in PROVIDER_VERIFIED_CHANNELS
            and _valid_internal_token(request)
        ):
            pin = sip.normalize_phone(req.channel_identity) or None
            if pin is None:
                log.warning(
                    "channel_identity present but not a usable phone; ignoring",
                    channel=req.channel,
                )
        # SPEC-W3 §3 SSE streaming chat: stream=true answers with
        # text/event-stream frames `data: {"delta": "..."}` per LLM chunk
        # (through the same tool layer) and a terminal
        # `data: {"done": true, ...}` frame. The buffered path is unchanged.
        if req.stream:

            async def event_source():
                try:
                    async for event in chat_service.handle_message_stream(
                        site_slug=req.site_slug,
                        message=req.message,
                        conversation_id=req.conversation_id,
                        session_secret=req.session_secret,
                    ):
                        yield f"data: {json.dumps(event, ensure_ascii=False)}\n\n"
                except Exception as exc:  # noqa: BLE001
                    log.warning("chat stream failed", site_slug=req.site_slug, error=str(exc))
                    yield f"data: {json.dumps({'error': str(exc)})}\n\n"

            return StreamingResponse(
                event_source(),
                media_type="text/event-stream",
                headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
            )
        try:
            return await chat_service.handle_message(
                site_slug=req.site_slug,
                message=req.message,
                conversation_id=req.conversation_id,
                session_secret=req.session_secret,
                persona_override=req.persona_override,
                channel=req.channel,
                pin_verified_identity=pin,
            )
        except Exception as exc:  # noqa: BLE001
            log.warning("chat failed", site_slug=req.site_slug, error=str(exc))
            raise HTTPException(status_code=502, detail=str(exc)) from exc

    # ------------------------------------------------------------------
    # SPEC-W10 Part A: voice provider catalog, preview, brand-voice enroll.
    # Same (unauthenticated, internal-network) posture as the other control
    # plane endpoints; enrollment is an admin path at the BFF layer.
    # ------------------------------------------------------------------
    @app.get("/voice/voices")
    async def list_tts_voices() -> dict[str, Any]:
        """Aggregate the configured TTS providers with availability flags;
        unavailable providers are listed with available:false and no voices."""
        providers_out: list[dict[str, Any]] = []
        for name, provider in tts_chain.providers_in_order():
            try:
                available = await provider.available()
            except Exception:  # noqa: BLE001 - probe must never 5xx the route
                available = False
            voices: list[dict[str, Any]] = []
            if available:
                try:
                    voices = [v.as_dict() for v in await provider.list_voices()]
                except Exception as exc:  # noqa: BLE001
                    log.warning("voice catalog fetch failed", provider=name, error=str(exc)[:200])
            providers_out.append(
                {"name": name, "available": available, "voices": voices}
            )
        return {"providers": providers_out}

    @app.post("/voice/tts-preview")
    async def tts_preview(req: TtsPreviewRequest) -> Response:
        """Synthesize a preview clip through the same FallbackTTS chain the
        calls use. Voice resolution: explicit `voice` param, then
        TTS_VOICE_MAP (provider-qualified), then PIPER_VOICE_MAP, then the
        default piper voice (app/multilang.py resolve_tts_voice)."""
        voice_spec = (req.voice or "").strip() or resolve_tts_voice(
            req.language or "",
            settings.tts_voice_map,
            settings.piper_voice_map,
            settings.piper_voice,
        )
        try:
            wav = await tts_chain.synthesize(
                req.text,
                voice=voice_spec,
                language=(req.language or "").strip(),
                provider=(req.provider or "").strip().lower() or None,
            )
        except ValueError as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc
        except Exception as exc:  # noqa: BLE001
            log.warning("tts preview failed", error=str(exc)[:200])
            raise HTTPException(status_code=502, detail=str(exc)) from exc
        return Response(content=wav, media_type="audio/wav")

    # ------------------------------------------------------------------
    # SPEC-W45 K15(d): staff-only admin prefix. Enrollment moved off the
    # public /voice/* prefix (OOS-04); guarded by _require_admin_access —
    # X-Internal-Token (VOICE_ADMIN_INTERNAL_TOKEN, fail closed) for
    # in-cluster service callers OR the gateway-injected staff-grade
    # X-User-Roles for humans (verifier F-2). CODER-F drops the public
    # APISIX route and exposes /api/voice-admin/* with jwt-auth + staff
    # role for the admin UI; the internal token remains the strong
    # in-cluster path.
    # ------------------------------------------------------------------
    @app.post("/voice-admin/voices/enroll")
    async def enroll_brand_voice(
        req: VoiceEnrollRequest, request: Request
    ) -> dict[str, Any]:
        """Enroll a brand voice on the XTTS provider (requires `xtts` in
        TTS_PROVIDER_CHAIN). Consent (NDPA) is captured in the admin UI."""
        _require_admin_access(request)
        xtts = tts_chain.provider("xtts")
        if xtts is None:
            raise HTTPException(
                status_code=400,
                detail="xtts provider not enabled (add 'xtts' to TTS_PROVIDER_CHAIN)",
            )
        try:
            base64.b64decode(req.sample_base64, validate=True)
        except (binascii.Error, ValueError) as exc:
            raise HTTPException(
                status_code=400, detail="sample_base64 is not valid base64"
            ) from exc
        try:
            voice_id = await xtts.enroll_voice(req.name, req.sample_base64)
        except Exception as exc:  # noqa: BLE001
            log.warning("voice enrollment failed", tenant=req.tenant, error=str(exc)[:200])
            raise HTTPException(status_code=502, detail=str(exc)) from exc
        log.info("brand voice enrolled", tenant=req.tenant, voice_id=voice_id)
        return {"voice_id": voice_id}

    @app.post("/voice-admin/escalations/{conversation_id}/staff-token")
    async def mint_escalation_staff_token(
        conversation_id: str, request: Request
    ) -> dict[str, Any]:
        """K15(e): on-demand staff join token for an escalation room.

        Replaces publishing the token on the events topic (any consumer
        could hijack the room). The staff dashboard calls this endpoint
        (via the APISIX /api/voice-admin/* gateway route — verifier F-2:
        same dual _require_admin_access guard as enrollment) when an
        operator picks up an escalation; the token is returned to that
        caller only. Scoped to the escalation room."""
        _require_admin_access(request)
        room = escalation_room_name(conversation_id)
        token = LiveKitEscalation(settings).staff_join_token(room)
        log.info("escalation staff token minted", room=room)
        return {
            "room": room,
            "url": settings.livekit_url,
            "token": token,
        }

    if elevenlabs is not None:

        @app.post("/voice/elevenlabs/tools")
        async def elevenlabs_tools(payload: dict[str, Any]) -> dict[str, Any]:
            try:
                return await elevenlabs.handle_tool_webhook(payload)
            except Exception as exc:  # noqa: BLE001
                log.warning("elevenlabs tool webhook failed", error=str(exc))
                raise HTTPException(status_code=502, detail=str(exc)) from exc

    return app
