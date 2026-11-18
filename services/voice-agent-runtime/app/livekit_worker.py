"""LiveKit Agents worker (SPEC §11), pinned to livekit-agents 0.10.x.

Pipeline: silero VAD -> faster-whisper STT (in-process, lazy) ->
OpenAI-compatible LLM (livekit-plugins-openai against LLM_BASE_URL, default
Ollama) -> FallbackTTS provider chain (SPEC-W10; piper-only by default,
which is byte-identical to the legacy direct PiperTTS path).

Room convention: room name `site-{slug}` binds the session to a tenant's
public site; the server (never the model) resolves the tenant from the slug
(SPEC §1 tenant-safe tool resolution).

Scaling (VOICE-SCALING §2): the worker prewarms each idle job process
(whisper model load + piper warmup synthesis, PRELOAD_MODELS=true) via
WorkerOptions.prewarm_fnc, keeps AGENT_IDLE_PROCESSES processes warm and
gates admission on an explicit psutil CPU load_fnc / LOAD_THRESHOLD. Slow
tools get filler ack lines + a hard timeout (VOICE-SCALING §5) via
app/async_tools.py.

Run:  python -m app.livekit_worker start   (via livekit agents CLI)

Version-sensitivity note: the `stt.STT`/`tts.TTS` bridge classes below use
the livekit-agents 0.10.x extension points (`STT.recognize`,
`TTS.synthesize` + `SynthesizedAudio`), verified against the real 0.10.2
package. If a different release reshapes those, the bridges in this file are
the single place to adjust — the pipeline stage implementations in
app/pipeline/ are version-agnostic.
"""

from __future__ import annotations

import asyncio
import inspect
import re
import threading
import time
import uuid
from typing import Any, AsyncIterable, Optional

from livekit import agents, rtc
from livekit.agents import llm as lk_llm
from livekit.agents import stt as lk_stt
from livekit.agents import tts as lk_tts
from livekit.agents import utils as lk_utils
from livekit.agents.pipeline import VoicePipelineAgent
from livekit.plugins import openai as lk_openai
from livekit.plugins import silero

from . import emergency, metrics, sip
from . import dead_mic, greeting_prerender
from .async_tools import (
    SLOW_TOOLS,
    VERIFICATION_TOOLS,
    AsyncToolRunner,
    ToolAckPolicy,
)
from .config import Settings, load_settings
from .dapr_client import DaprClient, DaprError
from .events import new_cloudevent, session_lifecycle_data
from .logging import configure_logging, get_logger
from .agent_definition import merge_definition
from .multilang import MultilangState, normalize_language, resolve_tts_voice
from .pipeline.stt import FasterWhisperSTT
from .pipeline.tts import TTSInterface
from .prompts import (
    EMERGENCY_LOCATION_FIRST_ADDENDUM,
    build_greeting,
    build_system_prompt,
)
from .session_state import SessionState
from .tenant_context import TenantContext, fetch_tenant_context
from .tools import ToolLayer
from .tts_providers.chain import FallbackTTS, build_fallback_tts

log = get_logger("livekit-worker")

ROOM_PREFIX = "site-"

# Prewarmed pipeline stages (VOICE-SCALING §2): populated by the worker
# `prewarm_fnc` in each warm job process so the first call on that process
# does not pay the whisper load / piper cold-start cost.
_PREWARMED: dict[str, Any] = {}

PREWARM_PHRASE = "Hello, thank you for calling. How can I help you today?"


# ---------------------------------------------------------------------------
# SPEC-W70 A1: endpointing / turn-taking tuning.
#
# The new VOICE_* knobs are wired into VoicePipelineAgent and silero.VAD.load
# via SIGNATURE INTROSPECTION: each candidate kwarg is kept only when the
# pinned livekit-agents==0.10.* callable actually declares it. A 0.10.x patch
# that lacks a knob (or a stub in tests) therefore degrades to framework
# defaults instead of crashing the worker with TypeError; every dropped key
# is logged ONCE at info level, and the applied set is logged once per label
# at info level too (startup observability: operators can confirm the knobs
# actually landed on the pinned SDK).
# ---------------------------------------------------------------------------
_DROPPED_KWARGS_LOGGED: set[str] = set()
_APPLIED_KWARGS_LOGGED: set[str] = set()

# SPEC-W71 P1: process-wide silero VAD cache (see _load_vad). Keyed by the
# tuned (post-introspection) kwargs dict; a threading.Lock guards both the
# lookup and the load so concurrent job threads race-safe.
_VAD_CACHE: dict[tuple, Any] = {}
_VAD_CACHE_LOCK = threading.Lock()


def _vad_cache_clear() -> None:
    """Test hook: drop all cached VAD instances."""
    with _VAD_CACHE_LOCK:
        _VAD_CACHE.clear()


def filter_supported_kwargs(
    func: Any, kwargs: dict[str, Any], *, label: str
) -> dict[str, Any]:
    """Keep only the kwargs ``func``'s signature accepts.

    A callable with a ``**kwargs`` catch-all accepts everything; a callable
    whose signature cannot be introspected at all gets the kwargs passed
    through unchanged with a warning (same "keep the worker bootable"
    philosophy as the WorkerOptions TypeError fallback below)."""
    try:
        params = inspect.signature(func).parameters
    except (TypeError, ValueError):
        log.warning(
            "signature introspection unavailable; passing kwargs through",
            label=label,
        )
        return dict(kwargs)
    if any(p.kind == inspect.Parameter.VAR_KEYWORD for p in params.values()):
        return dict(kwargs)
    accepted = {k: v for k, v in kwargs.items() if k in params}
    applied_marker = f"{label}.__applied__"
    if applied_marker not in _APPLIED_KWARGS_LOGGED:
        _APPLIED_KWARGS_LOGGED.add(applied_marker)
        log.info(
            "voice tuning knobs applied",
            label=label,
            keys=sorted(accepted),
        )
    for key in sorted(set(kwargs) - set(accepted)):
        marker = f"{label}.{key}"
        if marker not in _DROPPED_KWARGS_LOGGED:
            _DROPPED_KWARGS_LOGGED.add(marker)
            log.info(
                "voice tuning knob unsupported by this livekit-agents build; "
                "dropping (framework default applies)",
                label=label,
                key=key,
            )
    return accepted


def _agent_tuning_kwargs(settings: Settings) -> dict[str, Any]:
    """VoicePipelineAgent turn-taking knobs (SPEC-W70 A1).

    Names verified against the livekit_agents-0.10.2 wheel
    (``VoicePipelineAgent.__init__``: ``min_endpointing_delay``,
    ``preemptive_synthesis`` — 0.10.x has no max-endpointing knob).
    """
    return {
        "min_endpointing_delay": settings.voice_min_endpointing_delay,
        "preemptive_synthesis": settings.voice_preemptive_synthesis,
    }


def _load_vad(settings: Settings) -> Any:
    """silero.VAD.load with the tuned min_silence_duration (SPEC-W70 A1),
    cached process-wide (SPEC-W71 P1).

    build_voice_agent runs per job; without the cache the silero model was
    re-loaded on EVERY session. One VAD instance per tuned-kwargs
    combination is kept per process (thread-safe lazy singleton; the load
    runs under the lock so concurrent builds are race-safe). Warm
    processes skip the load entirely."""
    kwargs = filter_supported_kwargs(
        silero.VAD.load,
        {"min_silence_duration": settings.voice_vad_min_silence_s},
        label="silero.VAD.load",
    )
    key = tuple(sorted(kwargs.items()))
    with _VAD_CACHE_LOCK:
        vad = _VAD_CACHE.get(key)
        if vad is None:
            vad = silero.VAD.load(**kwargs)
            _VAD_CACHE[key] = vad
    return vad


def _build_stt(settings: Settings) -> FasterWhisperSTT:
    return FasterWhisperSTT(
        model_size=settings.whisper_model,
        device=settings.whisper_device,
        compute_type=settings.whisper_compute_type,
        # SPEC-W70 A2: optional multilingual accuracy tier (empty = off).
        multilingual_model=settings.whisper_model_multilingual,
        # SPEC-W71 P3: CTranslate2 thread tuning (0 = auto / 1 worker).
        cpu_threads=settings.whisper_cpu_threads,
        num_workers=settings.whisper_num_workers,
    )


def _build_tts(settings: Settings) -> FallbackTTS:
    """Live-call TTS: the SPEC-W10 FallbackTTS provider chain.

    Default config (TTS_PROVIDER_CHAIN="piper") builds a piper-only chain
    whose PiperTTS stage is constructed with exactly the legacy settings, so
    runtime behavior is byte-identical to the pre-W10 direct PiperTTS path.
    """
    return build_fallback_tts(settings)


async def _warmup_tts(tts: Any) -> None:
    """Prewarm-hook TTS warmup synthesis.

    Piper-only chain (default): one ``synthesize_pcm`` pass — exactly the
    pre-W10 piper warmup. Multi-provider chains exercise the FIRST configured
    provider AND piper (when present but not first), so a warm process has no
    cold start on either the primary or the implicit last-resort path.
    """
    order = tts.order if isinstance(tts, FallbackTTS) else []
    if order and order[0] != "piper" and tts.provider("piper") is not None:
        await tts.synthesize(PREWARM_PHRASE, provider=order[0])
        await tts.synthesize(PREWARM_PHRASE, provider="piper")
    else:
        await tts.synthesize_pcm(PREWARM_PHRASE)


def make_prewarm_fnc(settings: Settings):
    """WorkerOptions.prewarm_fnc (livekit-agents 0.10.x: runs synchronously in
    each warm job process before it accepts jobs).

    Eagerly loads the whisper model and runs the TTS warmup synthesis (one
    piper pass on the default piper-only chain; first provider + piper on
    multi-provider chains) so a warm process' first call has no dead air.
    Failures degrade to the old lazy-load behaviour (logged, never fatal).
    """

    def _prewarm(proc) -> None:  # noqa: ARG001 - proc userdata unused; module cache suffices
        if not settings.preload_models:
            return
        try:
            stt = _build_stt(settings)
            stt.preload_sync()
            _PREWARMED["stt"] = stt
            log.info("prewarm: whisper model loaded", model=settings.whisper_model)
        except Exception as exc:  # noqa: BLE001 - degrade to lazy load
            log.warning("prewarm: whisper load failed (lazy fallback)", error=str(exc)[:200])
        try:
            tts = _build_tts(settings)
            asyncio.run(_warmup_tts(tts))
            _PREWARMED["tts"] = tts
            # SPEC-W73 A2: 0.10.2 spawns job processes (worker.py
            # mp.get_context("spawn")), so the supervisor's boot prerender
            # never reaches the processes that serve sessions — prerender
            # the configured tenants' greetings into THIS process' chain
            # (empty tenant list / failure = no-op, boot continues).
            greeting_prerender.boot_prerender(settings, build_tts=lambda: tts)
            log.info(
                "prewarm: tts warmup synthesis done",
                chain=settings.tts_provider_chain,
                voice=settings.piper_voice,
            )
        except Exception as exc:  # noqa: BLE001 - degrade to lazy load
            log.warning("prewarm: tts warmup failed (lazy fallback)", error=str(exc)[:200])

    return _prewarm


def cpu_load_fnc() -> float:
    """WorkerOptions.load_fnc: explicit CPU-based load in [0, 1] (psutil).

    Verified against livekit-agents 0.10.2 (`WorkerOptions.load_fnc: Callable
    [[], float]`; the stock default is psutil-based too — this makes the
    contract explicit and tunable via LOAD_THRESHOLD).
    """
    try:
        import psutil

        return psutil.cpu_percent(interval=0.1) / 100.0
    except Exception:  # noqa: BLE001 - never break job admission
        return 0.0


# ---------------------------------------------------------------------------
# Bridges: app pipeline stages -> livekit-agents 0.10 node types
# ---------------------------------------------------------------------------
class WhisperSTTNode(lk_stt.STT):
    """Bridge FasterWhisperSTT into the LiveKit pipeline.

    Wave 5 #3: when whisper auto-detects the spoken language it is reported
    on the SpeechEvent and forwarded to ``on_language`` (the per-session
    MultilangState hook that swaps the piper voice + locale instruction)."""

    def __init__(self, impl: FasterWhisperSTT, on_language=None) -> None:
        super().__init__(
            capabilities=lk_stt.STTCapabilities(
                streaming=False, interim_results=False
            )
        )
        self._impl = impl
        self._on_language = on_language

    async def recognize(
        self, buffer: lk_utils.AudioBuffer, *, language: str | None = None
    ) -> lk_stt.SpeechEvent:
        frame = lk_utils.merge_frames(buffer)
        text = await self._impl.transcribe_pcm(
            bytes(frame.data),
            sample_rate=frame.sample_rate,
            channels=frame.num_channels,
            language=language,
        )
        detected = language or self._impl.last_detected_language or ""
        if detected and self._on_language is not None:
            try:
                self._on_language(detected)
            except Exception as exc:  # noqa: BLE001 - never break STT
                log.warning("language hook failed", error=str(exc)[:200])
        return lk_stt.SpeechEvent(
            type=lk_stt.SpeechEventType.FINAL_TRANSCRIPT,
            alternatives=[lk_stt.SpeechData(text=text, language=detected)],
        )


class PiperTTSNode(lk_tts.TTS):
    """Bridge a TTSInterface stage into the LiveKit pipeline (chunked
    synthesis). Since SPEC-W10 the impl is the FallbackTTS provider chain
    (piper-only by default).

    SPEC-W46 P4: when the impl exposes ``stream_pcm`` (PiperTTS and the
    FallbackTTS chain both do) and ``chunked`` is on (VOICE_TTS_CHUNKED,
    default true), sentence-level chunks are yielded as they synthesize —
    the first audio frame ships after the first chunk instead of the full
    utterance. ``chunked=False`` restores the pre-W46 full-buffer path.
    """

    def __init__(self, impl: TTSInterface, *, chunked: bool = True) -> None:
        super().__init__(
            capabilities=lk_tts.TTSCapabilities(streaming=False),
            sample_rate=impl.sample_rate,
            num_channels=1,
        )
        self._impl = impl
        self._chunked = chunked

    def synthesize(self, text: str, **kwargs) -> AsyncIterable[lk_tts.SynthesizedAudio]:
        return self._stream(text)

    def _audio(self, pcm: bytes) -> lk_tts.SynthesizedAudio:
        frame = rtc.AudioFrame(
            data=pcm,
            sample_rate=self._impl.sample_rate,
            num_channels=1,
            samples_per_channel=len(pcm) // 2,
        )
        return lk_tts.SynthesizedAudio(
            request_id=str(uuid.uuid4()),
            segment_id=str(uuid.uuid4()),
            frame=frame,
        )

    async def _stream(self, text: str) -> AsyncIterable[lk_tts.SynthesizedAudio]:
        stream_pcm = getattr(self._impl, "stream_pcm", None)
        if self._chunked and stream_pcm is not None:
            async for pcm in stream_pcm(text):
                if pcm:
                    yield self._audio(pcm)
            return
        pcm = await self._impl.synthesize_pcm(text)
        if not pcm:
            return
        yield self._audio(pcm)


# ---------------------------------------------------------------------------
# Function context: the 6 tools with EXACT names (SPEC §11) + the K15(c) OTP
# verification tools (SPEC-W81 A1)
# ---------------------------------------------------------------------------
class ReceptionistFunctions(lk_llm.FunctionContext):
    """The 6 tools + OTP verification + escalation, exposed to the LLM.

    Slow tools run through AsyncToolRunner (VOICE-SCALING §5): the filler ack
    line is spoken via `speaker` if the call outlasts the grace window, and a
    hard timeout resolves to a spoken apology instead of dead air.

    SPEC-W81 A1: request_verification_code / verify_caller_code close the
    voice-path gap — the system prompt (app/prompts.py) already instructs
    the model to drive the OTP flow, and the chat/ElevenLabs paths already
    expose both tools through ToolLayer.dispatch; these wrappers delegate
    to the SAME ToolLayer methods so the LiveKit path enforces the same
    K15(c) verified-session gate on lookup/reschedule/cancel.
    """

    def __init__(
        self,
        tool_layer: ToolLayer,
        *,
        runner: AsyncToolRunner | None = None,
        ack_policy: ToolAckPolicy | None = None,
        speaker=None,
    ) -> None:
        super().__init__()
        self._tools = tool_layer
        self._runner = runner or AsyncToolRunner()
        self._ack_policy = ack_policy
        self._speaker = speaker

    def set_speaker(self, speaker) -> None:
        """Late-bind the ack TTS speaker (needs the constructed agent)."""
        self._speaker = speaker

    async def _run_slow(self, tool: str, call) -> Any:
        return await self._runner.run(
            tool, call, ack_policy=self._ack_policy, speaker=self._speaker
        )

    @lk_llm.ai_callable(
        description="Get business information: catalog (offerings with ids, durations, prices), team members, timezone, currency and terminology."
    )
    async def get_business_info(self) -> str:
        import json

        result = await self._tools.get_business_info()
        return json.dumps(result, ensure_ascii=False)

    @lk_llm.ai_callable(
        description="Get open appointment slots for an offering with a team member in a time range."
    )
    async def get_availability(
        self,
        offering_id: str,
        team_member_id: str,
        from_iso: str,
        to_iso: str,
    ) -> str:
        import json

        result = await self._run_slow(
            "get_availability",
            lambda: self._tools.get_availability(
                offering_id=offering_id,
                team_member_id=team_member_id,
                from_iso=from_iso,
                to_iso=to_iso,
            ),
        )
        return json.dumps(result, ensure_ascii=False)

    @lk_llm.ai_callable(
        description="Book an appointment. Requires the caller's phone number (phone-confirmation policy: the first call returns confirmation_required; read the number back, and call again once the caller confirms)."
    )
    async def book_appointment(
        self,
        offering_id: str,
        team_member_id: str,
        starts_at: str,
        phone: str,
        contact_name: str = "",
        email: str = "",
    ) -> str:
        import json

        result = await self._run_slow(
            "book_appointment",
            lambda: self._tools.book_appointment(
                offering_id=offering_id,
                team_member_id=team_member_id,
                starts_at=starts_at,
                phone=phone,
                contact_name=contact_name or None,
                email=email or None,
            ),
        )
        return json.dumps(result, ensure_ascii=False)

    @lk_llm.ai_callable(
        description=(
            "Send a one-time verification code to the caller's phone "
            "number (SMS/email via the booking portal). REQUIRED before "
            "looking up, rescheduling or cancelling an existing booking. "
            "After calling this, ask the caller to read you the code and "
            "submit it with verify_caller_code."
        )
    )
    async def request_verification_code(self, phone: str) -> str:
        import json

        result = await self._run_slow(
            "request_verification_code",
            lambda: self._tools.request_verification_code(phone=phone),
        )
        return json.dumps(result, ensure_ascii=False)

    @lk_llm.ai_callable(
        description=(
            "Verify the one-time code the caller read back. On success "
            "the session is verified and lookup/reschedule/cancel may run."
        )
    )
    async def verify_caller_code(self, phone: str, code: str) -> str:
        import json

        result = await self._run_slow(
            "verify_caller_code",
            lambda: self._tools.verify_caller_code(phone=phone, code=code),
        )
        return json.dumps(result, ensure_ascii=False)

    @lk_llm.ai_callable(
        description="Look up the caller's upcoming appointments by phone number."
    )
    async def lookup_appointment(self, phone: str) -> str:
        import json

        result = await self._run_slow(
            "lookup_appointment",
            lambda: self._tools.lookup_appointment(phone=phone),
        )
        return json.dumps(result, ensure_ascii=False)

    @lk_llm.ai_callable(
        description="Reschedule an existing booking to a new start time. Requires the caller's phone number."
    )
    async def reschedule_appointment(
        self, booking_id: str, starts_at: str, phone: str
    ) -> str:
        import json

        result = await self._run_slow(
            "reschedule_appointment",
            lambda: self._tools.reschedule_appointment(
                booking_id=booking_id, starts_at=starts_at, phone=phone
            ),
        )
        return json.dumps(result, ensure_ascii=False)

    @lk_llm.ai_callable(
        description="Cancel an existing booking. Requires the caller's phone number."
    )
    async def cancel_appointment(
        self, booking_id: str, phone: str, reason: str = ""
    ) -> str:
        import json

        result = await self._run_slow(
            "cancel_appointment",
            lambda: self._tools.cancel_appointment(
                booking_id=booking_id, phone=phone, reason=reason or None
            ),
        )
        return json.dumps(result, ensure_ascii=False)

    @lk_llm.ai_callable(
        description=(
            "Save the caller's location to their contact record (emergency / "
            "location-first flow). Pass the spoken address in address_text; "
            "pass lat and lng only when the caller gave exact coordinates."
        )
    )
    async def capture_location(
        self,
        address_text: str = "",
        # NOTE: Optional[float], not `float | None` — livekit-agents 0.10.x
        # function registration crashes on PEP 604 unions (issubclass TypeError).
        lat: Optional[float] = None,
        lng: Optional[float] = None,
    ) -> str:
        import json

        result = await self._tools.capture_location(
            address_text=address_text or None, lat=lat, lng=lng
        )
        return json.dumps(result, ensure_ascii=False)

    @lk_llm.ai_callable(
        description=(
            "Escalate the conversation to a human staff member (warm handoff). "
            "Creates a LiveKit escalation room, notifies staff and confirms to "
            "the caller. Use when the caller asks for a human, is distressed, "
            "or the request cannot be resolved."
        )
    )
    async def request_human(self, reason: str = "") -> str:
        import json

        result = await self._tools.request_human(reason=reason or None)
        return json.dumps(result, ensure_ascii=False)


# ---------------------------------------------------------------------------
# Emergency priority lane (SPEC-W11 Part C)
# ---------------------------------------------------------------------------
class EmergencyTurnHook:
    """Live-turn emergency detector wired to ``user_speech_committed``.

    Runs ``emergency.is_emergency`` on every committed user turn. On the
    FIRST hit of the session (EmergencyState latches):

    (a) ``session.emergency = True`` (latched flag on session state);
    (b) ``voice_emergency_sessions_total`` increments and the active
        SessionMetrics gets its additive ``emergency`` flag (flows into the
        SessionEnded quality payload);
    (c) the EXISTING warm-handoff escalation path (ToolLayer.request_human,
        app/escalation.py underneath) is triggered immediately with
        reason ``"emergency"``. First-claim on prewarmed agents is inherent:
        worker prewarming (VOICE-SCALING §2) is shared across sessions, so
        the escalation-room creation + staff notification starts with no
        cold-start penalty — nothing extra to prewarm for emergencies;
    (d) the location-first prompt addendum (prompts.py) is appended to the
        chat context: the agent confirms the exact location FIRST, then
        reassures and keeps the caller on the line.

    Never raises: a detector failure must not break the call.
    """

    def __init__(
        self,
        *,
        tool_layer: ToolLayer,
        session: SessionState,
        agent: Any | None = None,
    ) -> None:
        self._tools = tool_layer
        self._session = session
        self._agent = agent
        self.state = emergency.EmergencyState()

    def __call__(self, message: Any = None, *_args: Any, **_kwargs: Any) -> None:
        try:
            text = emergency.message_text(message)
            if not text:
                return
            hit = self.state.observe(text)
            if hit is None:  # no emergency, or already triggered this session
                return
            severity, hazards = hit
            # (a) latch the session flag.
            self._session.emergency = True
            self._session.touch()
            # (b) metrics: registry counter + per-session quality flag.
            metrics.emergency_session()
            metrics.session_emergency()
            log.warning(
                "emergency detected on live call",
                conversation_id=self._session.conversation_id,
                severity=severity,
                hazards=hazards,
            )
            # (d) location-first instruction for the next model turn.
            self._inject_addendum()
            # (c) warm handoff via the existing escalation path.
            self._schedule_escalation()
        except Exception as exc:  # noqa: BLE001 - never break the call
            log.error("emergency hook failed", error=str(exc)[:200])

    def _inject_addendum(self) -> None:
        chat_ctx = getattr(self._agent, "chat_ctx", None)
        if chat_ctx is None:
            return
        try:
            chat_ctx.append(role="system", text=EMERGENCY_LOCATION_FIRST_ADDENDUM)
        except Exception as exc:  # noqa: BLE001 - addendum is best-effort
            log.warning("emergency addendum injection failed", error=str(exc)[:200])

    def _schedule_escalation(self) -> None:
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            log.error("emergency escalation skipped: no running event loop")
            return
        loop.create_task(self._escalate())

    async def _escalate(self) -> None:
        if self._session.escalation_room:
            # Already escalated (e.g. the caller asked for a human first) —
            # the emergency flag/metric above still stand.
            return
        try:
            await self._tools.request_human(reason="emergency")
        except Exception as exc:  # noqa: BLE001 - logged; call continues
            log.error(
                "emergency warm handoff failed",
                conversation_id=self._session.conversation_id,
                error=str(exc)[:200],
            )


# ---------------------------------------------------------------------------
# SPEC-W71 P6: mid-call phrase cache warming.
#
# After the session has started (and the greeting is queued — warming NEVER
# blocks it), a fire-and-forget task synthesizes the phrases the agent is
# most likely to speak mid-call: the tool-ack/filler lines (derived from
# the REAL ToolAckPolicy for this tenant — overrides, per-industry
# defaults, the DEFAULT_ACK fallback) plus the standard closers. They land
# in the TTS LRU (chain-level + piper-level), so the filler line spoken
# while a slow tool runs is served from memory. Warm failures only log.
# ---------------------------------------------------------------------------
TTS_WARM_CLOSERS: tuple[str, ...] = (
    "Is there anything else I can help you with?",
    "Thank you for calling. Goodbye.",
)


def tts_warm_phrases(ack_policy: ToolAckPolicy | None) -> list[str]:
    """The mid-call TTS warm set: every ack line the given ToolAckPolicy can
    produce for the slow tools, plus the standard closers. Deduped,
    order-stable."""
    phrases: list[str] = []
    if ack_policy is not None:
        for tool in sorted(SLOW_TOOLS):
            ack = ack_policy.ack_for(tool)
            if ack and ack.strip():
                phrases.append(ack.strip())
    phrases.extend(TTS_WARM_CLOSERS)
    seen: set[str] = set()
    out: list[str] = []
    for phrase in phrases:
        if phrase not in seen:
            seen.add(phrase)
            out.append(phrase)
    return out


async def _warm_tts_cache(impl: Any, phrases: list[str]) -> None:
    """Background TTS LRU warming. Failure-tolerant by contract: a warm
    failure only logs — the live call is unaffected."""
    for phrase in phrases:
        try:
            await impl.synthesize_pcm(phrase)
        except Exception as exc:  # noqa: BLE001 - warm failure only logs
            log.warning("tts warm failed", phrase=phrase[:60], error=str(exc)[:200])
    log.info("tts warm pass complete", phrases=len(phrases))


def schedule_tts_warmup(agent: Any, ack_policy: ToolAckPolicy | None) -> bool:
    """Schedule the mid-call TTS warm pass (asyncio.create_task). Call AFTER
    agent.start + the greeting, so warming never delays the caller's first
    audio. Returns False (and logs) instead of raising when there is no
    warmable impl or the warm set is empty.

    The impl is reached via the agent's public ``tts`` node
    (VoicePipelineAgent.tts, verified in the 0.10.2 wheel) and the
    PiperTTSNode's wrapped chain."""
    try:
        node = getattr(agent, "tts", None)
        impl = getattr(node, "_impl", None)  # PiperTTSNode wraps the chain
        synth = getattr(impl, "synthesize_pcm", None)
        phrases = tts_warm_phrases(ack_policy)
        if synth is None or not phrases:
            log.warning("tts warmup skipped: no synthesizable impl or empty warm set")
            return False
        asyncio.create_task(_warm_tts_cache(impl, phrases))
        return True
    except Exception as exc:  # noqa: BLE001 - never break session startup
        log.warning("tts warmup scheduling failed", error=str(exc)[:200])
        return False


# ---------------------------------------------------------------------------
# SPEC-W71 P8: chat-history windowing. chat_ctx grows unbounded within a
# call, inflating every turn's TTFT; it is trimmed in place after each
# agent reply commits (agent_speech_committed), at SAFE boundaries only.
# ---------------------------------------------------------------------------
def _msg_role(message: Any) -> str:
    role = getattr(message, "role", None)
    if role is None and isinstance(message, dict):
        role = message.get("role")
    return str(role or "")


def trim_chat_messages(messages: list, max_messages: int) -> list:
    """Bound a chat-context message list at SAFE boundaries (pure function;
    works on livekit ChatMessage and plain test doubles alike).

    Retains the leading system message, then the newest messages up to
    ``max_messages`` total — walking the cut point BACKWARD (toward older
    messages) to a user-or-system boundary, so an assistant message
    carrying ``tool_calls`` is never separated from its tool-result
    messages (the retained window never starts mid-tool-exchange).
    ``max_messages <= 0`` disables trimming. Returns a NEW list; callers
    assign in place (``msgs[:] = ...``)."""
    if max_messages <= 0 or len(messages) <= max_messages:
        return list(messages)
    head = list(messages[:1]) if messages and _msg_role(messages[0]) == "system" else []
    body = list(messages[len(head):])
    keep = max_messages - len(head)
    if keep <= 0:
        return list(head)
    cut = len(body) - keep
    while cut > 0 and _msg_role(body[cut]) not in ("user", "system"):
        cut -= 1
    return head + body[cut:]


class ChatHistoryTrimHook:
    """SPEC-W71 P8: trim the agent chat_ctx after each committed agent reply
    (``agent_speech_committed`` — event name verified against the
    livekit-agents 0.10.2 wheel). Never raises; disabled when the cap is
    <= 0."""

    def __init__(self, agent: Any, max_messages: int) -> None:
        self._agent = agent
        self._max = max_messages

    def __call__(self, *_args: Any, **_kwargs: Any) -> None:
        if self._max <= 0:
            return
        try:
            chat_ctx = getattr(self._agent, "chat_ctx", None)
            if chat_ctx is None:
                return
            msgs = chat_ctx.messages
            trimmed = trim_chat_messages(msgs, self._max)
            if len(trimmed) < len(msgs):
                msgs[:] = trimmed
                log.info(
                    "chat history trimmed", kept=len(trimmed), cap=self._max
                )
        except Exception as exc:  # noqa: BLE001 - never break the call
            log.warning("chat history trim failed", error=str(exc)[:200])


# ---------------------------------------------------------------------------
# SPEC-W71 P9: turn-total latency (voice_turn_total_seconds).
# ---------------------------------------------------------------------------
class TurnLatencyHook:
    """voice_turn_total_seconds = user_speech_committed -> first agent audio
    (``agent_started_speaking``, the pipeline's first agent-audio-frame
    event; both event names verified against the livekit-agents 0.10.2
    wheel).

    A filler ack line spoken while a slow tool runs IS agent audio and
    legitimately ends the caller's perceived turn wait; the greeting fires
    agent_started_speaking without a preceding user turn and is ignored.
    """

    def __init__(self, clock=time.monotonic) -> None:
        self._clock = clock
        self._turn_started: float | None = None

    def on_user_committed(self, *_args: Any, **_kwargs: Any) -> None:
        self._turn_started = self._clock()

    def on_agent_speaking(self, *_args: Any, **_kwargs: Any) -> None:
        started, self._turn_started = self._turn_started, None
        if started is None:
            return
        metrics.get_registry().turn_total.observe(self._clock() - started)


# ---------------------------------------------------------------------------
# Session wiring
# ---------------------------------------------------------------------------
async def _publish_lifecycle(
    dapr: DaprClient,
    settings: Settings,
    ctx: TenantContext,
    type_: str,
    conversation_id: str,
    *,
    quality: dict[str, Any] | None = None,
    agent_id: str | None = None,
) -> None:
    event = new_cloudevent(
        type_=type_,
        subject=ctx.tenant_slug,
        tenant_uuid=ctx.tenant_id,
        data=session_lifecycle_data(
            conversation_id=conversation_id,
            channel="voice",
            site_slug=ctx.site_slug,
            quality=quality,
            agent_id=agent_id,
        ),
    )
    await dapr.publish_best_effort(
        settings.dapr_pubsub, settings.conversation_events_topic, event, kind=type_
    )


# SPEC-W45 K15(a): web session rooms are PER-SESSION unique —
# ``site-{slug}-{uuid4hex}`` (control_plane.py) — so no two callers ever
# share a "party line" room (OOS-01). The trailing 32-hex session suffix is
# stripped to recover the tenant slug; legacy ``site-{slug}`` rooms (and
# any non-conforming name) pass through unchanged.
_SESSION_ROOM_RE = re.compile(r"^(?P<slug>.+)-[0-9a-f]{32}$")


def site_slug_from_room(room_name: str) -> str:
    base = room_name[len(ROOM_PREFIX):] if room_name.startswith(ROOM_PREFIX) else room_name
    m = _SESSION_ROOM_RE.match(base)
    return m.group("slug") if m else base


async def build_voice_agent(
    settings: Settings,
    dapr: DaprClient,
    site_slug: str,
    conversation_id: str,
    sip_call: "sip.InboundCallContext | None" = None,
) -> tuple[VoicePipelineAgent, TenantContext, SessionState]:
    """Bootstrap tenant context and assemble the VoicePipelineAgent.

    Wave 5 #1: ``sip_call`` (from app/sip.bootstrap_inbound_call) attaches
    the carrier-asserted caller ID as the session's CLAIMED (unverified)
    phone before the prompt is rendered, so the receptionist can greet with
    the number on file — but the K15(c) OTP gate still applies before any
    booking lookup/change (the carrier-asserted bypass was removed).
    """
    ctx = await fetch_tenant_context(dapr, settings, site_slug)
    # SPEC-W38 F1/F2: when the SIP bootstrap resolved an agent record via
    # the agents registry, merge its declarative definition over the
    # pack/env context BEFORE the prompt is built and the tool layer is
    # assembled (merge order: env < pack < definition).
    agent_record = getattr(sip_call, "agent_record", None) if sip_call is not None else None
    if agent_record is not None and agent_record.definition is not None:
        merge_definition(ctx, agent_record.definition)
    session = SessionState(conversation_id=conversation_id, site_slug=site_slug)
    # SPEC-W38 F3: retain the registry-resolved agent id on the session so
    # the SessionEnded lifecycle event can carry it as `agentId` for the
    # conversation-service capture consumer.
    if agent_record is not None:
        session.resolved_agent_id = (getattr(agent_record, "id", "") or "").strip() or None
    caller_phone = ""
    if sip_call is not None:
        # K15(c): claimed (unverified) hint only — never confirmed.
        sip.attach_caller_id(session, sip_call.caller_phone)
        caller_phone = session.claimed_phone or ""
    tool_layer = ToolLayer(dapr=dapr, settings=settings, ctx=ctx, session=session)
    system_prompt = build_system_prompt(
        ctx, conversation_id=conversation_id, caller_phone=caller_phone or None
    )

    chat_ctx = lk_llm.ChatContext().append(role="system", text=system_prompt)
    runner = AsyncToolRunner(
        timeout_s=settings.tool_timeout_s,
        ack_grace_ms=settings.tool_ack_grace_ms,
        # SPEC-W81 A1: the OTP tools do a booking-portal HTTP round-trip
        # (portal client timeout = http_timeout_s, 15s default) that can
        # outlast the 4s global tool timeout — give ONLY those tools the
        # longer budget so the portal answer reaches the caller instead of
        # a generic timeout apology. The global timeout is untouched.
        timeout_overrides={
            tool: settings.verification_tool_timeout_s
            for tool in VERIFICATION_TOOLS
        },
    )
    fnc_ctx = ReceptionistFunctions(
        tool_layer,
        runner=runner,
        ack_policy=ToolAckPolicy.from_context(ctx),
    )

    # Reuse the prewarmed stages when the worker prewarm hook populated them.
    stt_impl = _PREWARMED.get("stt") or _build_stt(settings)
    tts_impl = _PREWARMED.get("tts") or _build_tts(settings)

    # Wave 5 #3 multilingual receptionist: tenant locale sets the default
    # language; whisper detection switches it per turn, swapping the TTS
    # voice and re-rendering the system prompt with the per-turn locale
    # instruction. SPEC-W10: voice selection goes through resolve_tts_voice —
    # provider-qualified TTS_VOICE_MAP entries ("mms:pcm") route to that
    # provider on the FallbackTTS chain, with PIPER_VOICE_MAP + the default
    # piper voice as the legacy fallback (byte-identical when TTS_VOICE_MAP
    # is unset).
    ml_state = MultilangState.from_context(ctx)
    session.active_language = ml_state.active_language

    def _voice_spec(language: str) -> str:
        return resolve_tts_voice(
            language,
            settings.tts_voice_map,
            settings.piper_voice_map,
            settings.piper_voice,
        )

    tts_impl.voice = _voice_spec(ml_state.active_language)
    # SPEC-W38 F2: definition.voice overrides the session's default voice
    # (per-turn language switching still applies on top). Only piper /
    # provider-less voices are applied here — other providers go through the
    # chain's own voice maps.
    definition = getattr(ctx, "agent_definition", None)
    if (
        definition is not None
        and definition.voice.voice_id
        and definition.voice.provider in ("", "piper")
    ):
        tts_impl.voice = definition.voice.voice_id

    def _on_language(detected: str) -> None:
        switched = ml_state.observe(detected)
        if settings.tts_voice_map and normalize_language(detected):
            # Provider-qualified maps follow the RAW detection: pcm/yo/ha/ig
            # route to their mapped provider voice even when the locale stays
            # put (pcm is proxied to English at the locale/prompt level).
            tts_impl.voice = _voice_spec(detected)
        elif switched:
            tts_impl.voice = _voice_spec(ml_state.active_language)
        if not switched:
            return
        session.active_language = ml_state.active_language
        new_prompt = build_system_prompt(
            ctx,
            conversation_id=conversation_id,
            language=ml_state.active_language,
            caller_phone=session.confirmed_phone or session.claimed_phone,
        )
        try:
            msgs = agent.chat_ctx.messages
            if msgs and msgs[0].role == "system":
                msgs[0].content = [new_prompt]
        except Exception as exc:  # noqa: BLE001 - prompt stays at default
            log.warning("locale instruction swap failed", error=str(exc)[:200])

    agent = VoicePipelineAgent(
        vad=_load_vad(settings),
        stt=WhisperSTTNode(stt_impl, on_language=_on_language),
        # NOTE (VOICE-SCALING §3): the livekit-plugins-openai LLM node cannot
        # hot-swap endpoints mid-process, so the LLM fallback chain
        # (app/pipeline/llm.py FallbackLLM) covers the chat/tool-loop paths;
        # the worker node relies on the primary endpoint + job-level retry.
        llm=lk_openai.LLM(
            model=settings.llm_model,
            base_url=settings.llm_base_url,
            api_key=settings.llm_api_key,
        ),
        tts=PiperTTSNode(tts_impl, chunked=settings.tts_chunked),
        chat_ctx=chat_ctx,
        fnc_ctx=fnc_ctx,
        allow_interruptions=True,
        # SPEC-W70 A1: endpointing / preemptive-synthesis knobs, filtered by
        # signature introspection against the pinned 0.10.* class.
        **filter_supported_kwargs(
            VoicePipelineAgent.__init__,
            _agent_tuning_kwargs(settings),
            label="VoicePipelineAgent",
        ),
    )
    # Voice-path filler: speak the ack line through the agent's TTS while a
    # slow tool call is in flight (cancelled when the tool answers fast).
    fnc_ctx.set_speaker(lambda text: agent.say(text, allow_interruptions=True))

    # SPEC-W11 Part C emergency lane: inspect every committed user turn; the
    # hook latches session.emergency, counts the session, triggers the warm
    # handoff and injects the location-first addendum on the first hit. A
    # second listener (turn counting) is registered in entrypoint — the
    # emitter supports multiple handlers per event.
    try:
        agent.on(
            "user_speech_committed",
            EmergencyTurnHook(tool_layer=tool_layer, session=session, agent=agent),
        )
    except Exception as exc:  # noqa: BLE001 - detection off, call unaffected
        log.warning("emergency hook registration failed", error=str(exc)[:200])
    # SPEC-W71 P8: chat-history windowing — trim the chat_ctx in place after
    # each committed agent reply, at safe boundaries only (disabled when
    # VOICE_LLM_HISTORY_MAX <= 0).
    if settings.voice_llm_history_max > 0:
        try:
            agent.on(
                "agent_speech_committed",
                ChatHistoryTrimHook(agent, settings.voice_llm_history_max),
            )
        except Exception as exc:  # noqa: BLE001 - trimming off, call unaffected
            log.warning("history trim hook registration failed", error=str(exc)[:200])
    return agent, ctx, session


# ---------------------------------------------------------------------------
# SPEC-W73 A1: dead-mic detection wiring (watchdog in app/dead_mic.py).
# ---------------------------------------------------------------------------
def start_dead_mic_monitor(
    settings: Settings, agent: Any, session: SessionState, room: Any
) -> dead_mic.DeadMicMonitor | None:
    """Create, wire and start the per-session dead-mic watchdog.

    Returns None (complete no-op) when VOICE_DEAD_MIC_ENABLED=0 or wiring
    fails. User-speech signals are the wheel-verified VoicePipelineAgent
    events ``user_started_speaking`` / ``user_speech_committed`` (0.10.2
    ``EventTypes``); the hangup path is the room's own ``disconnect``
    (verified on livekit 0.17.x ``rtc.Room``), which fires the room
    ``disconnected`` handler — the existing teardown stays the single
    SessionEnded publisher. Escalation (``session.escalation_room`` set)
    suppresses every further dead-mic utterance.
    """
    if not settings.voice_dead_mic_enabled:
        return None
    try:
        monitor = dead_mic.DeadMicMonitor(
            agent=agent,
            first_prompt_s=settings.voice_dead_mic_first_prompt_s,
            second_prompt_s=settings.voice_dead_mic_second_prompt_s,
            hangup_enabled=settings.voice_dead_mic_hangup_enabled,
            on_hangup=room.disconnect,
            is_escalated=lambda: session.escalation_room is not None,
        )
        agent.on("user_started_speaking", monitor.note_user_speech)
        agent.on("user_speech_committed", monitor.note_user_speech)
        monitor.start()
        return monitor
    except Exception as exc:  # noqa: BLE001 - monitor off, call unaffected
        log.warning("dead-mic monitor unavailable", error=str(exc)[:200])
        return None


async def entrypoint(ctx: agents.JobContext) -> None:
    settings = load_settings()
    configure_logging(settings.log_level)
    # W44/F15-09: job processes record the per-call voice_* series into the
    # shared app/metrics.py registry — expose it for Prometheus here too
    # (first binder wins; SO_REUSEPORT lets multiple job procs share 9464).
    _start_worker_metrics(settings)
    # SPEC-W46 P-02: direct booking base with daprd fallback (same as the
    # control plane) — the per-turn tool calls skip the sidecar hop.
    dapr = DaprClient(
        settings.dapr_base_url,
        settings.http_timeout_s,
        direct_bases={settings.booking_app_id: settings.booking_base_url},
    )

    await ctx.connect()
    room_name = ctx.room.name or ""
    # Wave 5 #1 SIP inbound: `call-*` rooms (dispatch-rule.yaml) or rooms
    # joined by a SIP participant are PSTN calls. Resolve the tenant from the
    # dialed number (TENANT_PHONE_MAP) instead of the `site-{slug}` web
    # convention; the caller ID becomes the confirmed phone (app/sip.py).
    sip_call: sip.InboundCallContext | None = None
    participants = list(getattr(ctx.room, "remote_participants", {}).values())
    if sip.is_sip_room(room_name) or any(sip.is_sip_participant(p) for p in participants):
        try:
            # SPEC-W38 F1: registry-first dialed-number resolution (agents
            # registry), fail-open to the legacy TENANT_PHONE_MAP path.
            sip_call = await sip.bootstrap_inbound_call_async(
                settings, room_name, participants
            )
        except sip.SipTenantResolutionError as exc:
            log.error("sip tenant resolution failed", room=room_name, error=str(exc))
            await dapr.aclose()
            return
        site_slug = sip_call.site_slug
    else:
        site_slug = site_slug_from_room(room_name)
    conversation_id = str(uuid.uuid4())
    log.info(
        "voice session starting",
        room=room_name,
        site_slug=site_slug,
        channel="phone" if sip_call is not None else "web",
    )

    try:
        agent, tenant_ctx, session = await build_voice_agent(
            settings, dapr, site_slug, conversation_id, sip_call=sip_call
        )
    except DaprError as exc:
        log.error("session bootstrap failed", site_slug=site_slug, error=str(exc))
        await dapr.aclose()
        return

    # Per-session call-quality accumulator: activated BEFORE agent.start so
    # the pipeline tasks it spawns inherit this context (asyncio tasks copy
    # the contextvars at creation). The instrumented STT/TTS/LLM/tool call
    # sites feed it; the SessionEnded event below ships its payload.
    session_metrics = metrics.activate_session(metrics.SessionMetrics(conversation_id))
    try:
        # Turn = one committed user utterance (livekit-agents 0.10.x event).
        agent.on("user_speech_committed", lambda *_a, **_k: metrics.session_turn())
        # SPEC-W71 P9: turn-total latency (user commit -> first agent audio).
        turn_latency = TurnLatencyHook()
        agent.on("user_speech_committed", turn_latency.on_user_committed)
        agent.on("agent_started_speaking", turn_latency.on_agent_speaking)
    except Exception as exc:  # noqa: BLE001 - event surface may shift; turns stay 0
        log.warning("turn counting unavailable", error=str(exc)[:200])

    await _publish_lifecycle(
        dapr,
        settings,
        tenant_ctx,
        "com.opendesk.conversation.SessionStarted",
        conversation_id,
    )
    metrics.get_registry().active_sessions.inc()

    agent.start(ctx.room)
    # Greet the caller so the conversation opens naturally. SPEC-W11 Part C
    # §5: build_greeting prepends/appends the pack's spoken-AI-disclosure /
    # recording-consent lines when the tenant context carries a `disclosure`
    # block; absent the block the greeting is byte-identical to before.
    await agent.say(
        build_greeting(tenant_ctx),
        allow_interruptions=True,
    )
    # SPEC-W71 P6: warm the TTS LRU mid-call with the tool-ack/filler phrases
    # (derived from this tenant's ToolAckPolicy) + standard closers. Scheduled
    # AFTER session start and the greeting — fire-and-forget; failures log.
    schedule_tts_warmup(agent, ToolAckPolicy.from_context(tenant_ctx))

    # SPEC-W73 A1: dead-mic watchdog, started AFTER the greeting is queued
    # (helper above; None when disabled). Cancelled on room disconnect below.
    dead_mic_monitor = start_dead_mic_monitor(settings, agent, session, ctx.room)

    @ctx.room.on("disconnected")
    def _on_disconnected(*args) -> None:  # noqa: ARG001
        import asyncio

        if dead_mic_monitor is not None:
            dead_mic_monitor.cancel()  # no orphan watchdog past session end
        metrics.get_registry().active_sessions.dec()
        # Attach the call-quality payload (None when the session recorded no
        # signals -> the event carries no `quality` key at all).
        quality = session_metrics.quality_payload(
            escalated=session.escalation_room is not None,
            confirmed_phone=session.confirmed_phone,
        )
        asyncio.get_event_loop().create_task(
            _publish_lifecycle(
                dapr,
                settings,
                tenant_ctx,
                "com.opendesk.conversation.SessionEnded",
                conversation_id,
                quality=quality,
                agent_id=session.resolved_agent_id,
            )
        )


def build_worker_options(settings: Settings) -> "agents.WorkerOptions":
    """WorkerOptions with prewarming + CPU load gating (VOICE-SCALING §2).

    Field names verified against the livekit-agents 0.10.2 wheel
    (`num_idle_processes`, `load_fnc`, `load_threshold`, `prewarm_fnc`); the
    TypeError fallback keeps the worker bootable if a future 0.10.x patch
    reshapes the signature.
    """
    scaling_kwargs: dict[str, Any] = {
        "num_idle_processes": settings.agent_idle_processes,
        "load_fnc": cpu_load_fnc,
        "load_threshold": settings.load_threshold,
        "prewarm_fnc": make_prewarm_fnc(settings),
    }
    base_kwargs: dict[str, Any] = {
        "entrypoint_fnc": entrypoint,
        "api_key": settings.livekit_api_key,
        "api_secret": settings.livekit_api_secret,
        "ws_url": settings.livekit_url,
    }
    try:
        return agents.WorkerOptions(**base_kwargs, **scaling_kwargs)
    except TypeError as exc:
        log.warning(
            "WorkerOptions rejected scaling kwargs; falling back to minimal options",
            error=str(exc),
        )
        return agents.WorkerOptions(**base_kwargs)


def _start_worker_metrics(settings: Settings) -> None:
    """W44/F15-09: serve the app/metrics.py registry as Prometheus text on
    VOICE_WORKER_METRICS_PORT (default 9464). Best-effort: a bind failure
    (port already held by the supervisor or another job process) only logs."""
    if metrics.start_http_server(settings.worker_metrics_port):
        log.info(
            "worker /metrics endpoint listening",
            port=settings.worker_metrics_port,
        )
    elif settings.worker_metrics_port > 0:
        log.warning(
            "worker /metrics endpoint not started (port in use or disabled)",
            port=settings.worker_metrics_port,
        )


def main() -> None:
    settings = load_settings()
    configure_logging(settings.log_level)
    _start_worker_metrics(settings)
    # SPEC-W73 A2: pre-render known tenants' greetings BEFORE the worker
    # starts accepting jobs (bounded by VOICE_GREETING_PRERENDER_TIMEOUT_S;
    # timeout -> partial warm). The warmed chain is pinned for sessions
    # served by THIS process (thread executor / dev); spawned job processes
    # warm their own chains in prewarm_fnc.
    warmed_tts = greeting_prerender.boot_prerender(
        settings, build_tts=lambda: _build_tts(settings)
    )
    if warmed_tts is not None:
        _PREWARMED.setdefault("tts", warmed_tts)
    log.info(
        "starting livekit agents worker",
        backend=settings.agent_backend,
        preload_models=settings.preload_models,
        idle_processes=settings.agent_idle_processes,
        load_threshold=settings.load_threshold,
    )
    agents.cli.run_app(build_worker_options(settings))


if __name__ == "__main__":
    main()
