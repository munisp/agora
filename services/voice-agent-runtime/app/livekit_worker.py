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
import functools
import re
import uuid
from typing import Any, AsyncIterable, Iterator, Optional

from livekit import agents, rtc
from livekit.agents import llm as lk_llm
from livekit.agents import stt as lk_stt
from livekit.agents import tts as lk_tts
from livekit.agents import utils as lk_utils
from livekit.agents.pipeline import VoicePipelineAgent
from livekit.plugins import openai as lk_openai
from livekit.plugins import silero

from . import emergency, metrics, sip
from .async_tools import AsyncToolRunner, ToolAckPolicy
from .config import Settings, load_settings
from .dapr_client import DaprClient, DaprError
from . import call_language
from .call_activity import announce_activity, announce_captions, announce_language, caption
from .pipeline.naija_stt import NaijaSpeechSTT
from .lk_tool_loop import ToolLoopLLM
from .transcripts import TranscriptRecorder, tool_turn_text
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


def _build_stt(settings: Settings) -> FasterWhisperSTT:
    return FasterWhisperSTT(
        model_size=settings.whisper_model,
        device=settings.whisper_device,
        compute_type=settings.whisper_compute_type,
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


def _openai_client(settings: Settings) -> Any:
    import httpx
    import openai

    from .pipeline.llm import http_client_with_body

    timeout = httpx.Timeout(settings.llm_timeout_s, connect=10.0)
    extra = getattr(settings, "llm_extra_body", None) or {}
    return openai.AsyncClient(
        base_url=settings.llm_base_url,
        api_key=settings.llm_api_key,
        timeout=timeout,
        max_retries=0,
        http_client=http_client_with_body(extra, timeout) if extra else None,
    )


def _worker_llm(settings: Settings) -> Any:
    # The plugin refuses to start without api_key even when handed a client.
    # ToolLoopLLM: the 0.10 pipeline drops tool calls made in the answer round (a second lookup = silence).
    return ToolLoopLLM(lk_openai.LLM(model=settings.llm_model, api_key=settings.llm_api_key, client=_openai_client(settings)))


async def _warmup_then_release(tts: Any) -> None:
    """Warm the sidecars, then drop the HTTP clients. Prewarm runs in its own asyncio.run() loop, which closes
    afterwards; clients kept from it made every later call fail with "Event loop is closed" (piper then fell
    back to 16 kHz MMS: pitched audio and failed frame capture). Providers re-create clients lazily."""
    try:
        await _warmup_tts(tts)
    finally:
        aclose = getattr(tts, "aclose", None)
        if aclose is not None:
            await aclose()


def _prewarm(settings: Settings, proc) -> None:  # noqa: ARG001 - proc userdata unused; module cache suffices
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
        asyncio.run(_warmup_then_release(tts))
        _PREWARMED["tts"] = tts
        log.info(
            "prewarm: tts warmup synthesis done",
            chain=settings.tts_provider_chain,
            voice=settings.piper_voice,
        )
    except Exception as exc:  # noqa: BLE001 - degrade to lazy load
        log.warning("prewarm: tts warmup failed (lazy fallback)", error=str(exc)[:200])


def make_prewarm_fnc(settings: Settings):
    """WorkerOptions.prewarm_fnc (livekit-agents 0.10.x: runs synchronously in
    each warm job process before it accepts jobs).

    Eagerly loads the whisper model and runs the TTS warmup synthesis (one
    piper pass on the default piper-only chain; first provider + piper on
    multi-provider chains) so a warm process' first call has no dead air.
    Failures degrade to the old lazy-load behaviour (logged, never fatal).
    """
    # A module-level function bound with functools.partial: job processes start with "spawn", which pickles the
    # prewarm function, and a nested closure can't be pickled (every job failed with "Can't get local object").
    return functools.partial(_prewarm, settings)


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
_ENGLISH_FAMILY = frozenset({"en", "pcm"})


class SwitchingLanguageSTT:
    """Speech recognition that follows the caller between languages mid-call (English, Pidgin, Yorùbá, Hausa, Igbo).

    Every caller turn of at least MIN_SWITCH_S seconds is checked with spoken-language identification (MMS-LID on the
    GPU host: on real speech 30/30 correct for full sentences, ~80% on 2.5 s clips — so short turns never switch).
    A switch needs MIN_CONFIDENCE and is reported through `on_switch(code)` (voice, prompt and the caller's screen
    follow). English and Pidgin are one family here: identification cannot tell them apart reliably.
    The turn is then transcribed with MMS for a Nigerian language, whisper (pinned) for English."""

    MIN_SWITCH_S = 3.0
    MIN_CONFIDENCE = 0.65  # Igbo came back right at 0.72 on a clean clip

    def __init__(self, naija: NaijaSpeechSTT, whisper: Any, start: str, allowed: list[str], on_switch: Any) -> None:
        self._naija, self._whisper = naija, whisper
        self.language = start or "en"
        self._english_side = start if start in _ENGLISH_FAMILY else "en"
        self._allowed = [c for c in allowed if c != "ful"] or ["en"]
        self._on_switch = on_switch
        self.last_detected_language: str | None = self.language

    def _target(self, detected: str) -> str | None:
        if detected in _ENGLISH_FAMILY:
            return None if self.language in _ENGLISH_FAMILY else self._english_side
        return detected if detected != self.language else None

    async def _maybe_switch(self, pcm: bytes, sample_rate: int, channels: int) -> None:
        seconds = len(pcm) / (2 * max(1, channels) * max(1, sample_rate))
        if seconds < self.MIN_SWITCH_S or len(self._allowed) < 2:
            return
        try:
            out = await self._naija.identify(pcm, sample_rate=sample_rate, channels=channels, candidates=self._allowed)
        except Exception as exc:  # noqa: BLE001 - keep the current language
            log.warning("language identification failed", error=str(exc)[:200])
            return
        detected = str(out.get("language") or "")
        confidence = float((out.get("scores") or {}).get(detected, 0.0))
        target = self._target(detected)
        if target and confidence >= self.MIN_CONFIDENCE:
            log.info("caller switched language", previous=self.language, language=target, confidence=confidence)
            self.language = target
            if target in _ENGLISH_FAMILY:
                self._english_side = target
            try:
                self._on_switch(target)
            except Exception as exc:  # noqa: BLE001
                log.warning("language switch hook failed", error=str(exc)[:200])

    async def transcribe_pcm(self, pcm: bytes, *, sample_rate: int = 16000, channels: int = 1,
                             language: str | None = None) -> str:
        await self._maybe_switch(pcm, sample_rate, channels)
        self.last_detected_language = self.language
        if self.language in call_language.NIGERIAN:
            try:
                return await self._naija.transcribe_pcm(pcm, sample_rate=sample_rate, channels=channels,
                                                        language=self.language)
            except Exception as exc:  # noqa: BLE001 - keep hearing the caller
                log.warning("naija speech recognition failed; using whisper", language=self.language,
                            error=str(exc)[:200])
        return await self._whisper.transcribe_pcm(pcm, sample_rate=sample_rate, channels=channels, language="en")


class ChosenLanguageSTT:
    """Speech recognition for a call whose caller chose a Nigerian language: MMS on the GPU host with that language's
    adapter; whisper is the fallback if the service fails (so the call never goes deaf). Reports the chosen language
    as "detected" so nothing downstream flips the call back to English."""

    def __init__(self, naija: NaijaSpeechSTT, whisper: Any, language: str) -> None:
        self._naija, self._whisper, self._language = naija, whisper, language
        self.last_detected_language: str | None = language

    async def transcribe_pcm(self, pcm: bytes, *, sample_rate: int = 16000, channels: int = 1,
                             language: str | None = None) -> str:
        try:
            return await self._naija.transcribe_pcm(pcm, sample_rate=sample_rate, channels=channels,
                                                    language=self._language)
        except Exception as exc:  # noqa: BLE001 - keep hearing the caller
            log.warning("naija speech recognition failed; using whisper", language=self._language, error=str(exc)[:200])
            return await self._whisper.transcribe_pcm(pcm, sample_rate=sample_rate, channels=channels)


# Nigerian-language replies: the naija voices make one sentence at about real time, so a short sentence followed by a
# long one left a gap of several seconds. Grouping sentences into chunks of at least this many characters lets the
# service generate each chunk in one GPU batch (~3x real time): every chunk is ready before the previous one ends.
NAIJA_MIN_CHUNK_CHARS = 60


def _naija_tts(node: "PiperTTSNode") -> Any:
    from livekit.agents import tokenize as lk_tokenize

    return lk_tts.StreamAdapter(
        tts=node, sentence_tokenizer=lk_tokenize.basic.SentenceTokenizer(min_sentence_len=NAIJA_MIN_CHUNK_CHARS)
    )


_YES = re.compile(r"\b(yes|yeah|yep|correct|that'?s right|right|exactly|e correct|na so|ehen|bẹ́ẹ̀ni|beeni|bẹẹni|haka ne|haka|ee|eh|ọ dị mma)\b", re.I)


def _message_plain(msg: Any) -> str:
    content = getattr(msg, "content", "")
    if isinstance(content, list):
        content = " ".join(str(c) for c in content if isinstance(c, str))
    return str(content or "")


def read_back_confirmed(messages: list[Any], phone: str) -> bool:
    """True when the receptionist's last message read this phone number back and the caller's reply after it said
    yes. The model often reads the number back BEFORE its first booking call; without this the booking tool asked the
    caller to confirm a second time (callers hung up thinking they were booked)."""
    digits = re.sub(r"\D", "", phone or "")
    if len(digits) < 7:
        return False
    last_agent = next((m for m in reversed(messages) if getattr(m, "role", "") == "assistant" and _message_plain(m).strip()), None)
    if last_agent is None:
        return False
    i = messages.index(last_agent)
    replies = [_message_plain(m) for m in messages[i + 1:] if getattr(m, "role", "") == "user"]
    return (digits[-7:] in re.sub(r"\D", "", _message_plain(last_agent))) and any(_YES.search(r) for r in replies)


_TAG = re.compile(r"</?[a-zA-Z][^>]{0,80}>")
_MARKDOWN = re.compile(r"[*_#`~|]+")


def speakable(text: str) -> str:
    """What the voice should say: the model sometimes writes HTML ("</li><li>") or markdown, which was read aloud."""
    out = _MARKDOWN.sub("", _TAG.sub(" ", text or ""))
    return re.sub(r"\s{2,}", " ", out).strip()


class WhisperSTTNode(lk_stt.STT):
    """Bridge FasterWhisperSTT into the LiveKit pipeline.

    Wave 5 #3: when whisper auto-detects the spoken language it is reported
    on the SpeechEvent and forwarded to ``on_language`` (the per-session
    MultilangState hook that swaps the piper voice + locale instruction)."""

    # Whisper's language guess is unreliable on short clips: "Nine o'clock is fine." (1.5 s) came back as German
    # ("Nein, der Klo"). Below this length the call's current language is used instead of a guess.
    SHORT_CLIP_S = 3.0

    def __init__(self, impl: FasterWhisperSTT, on_language=None, current_language=None) -> None:
        super().__init__(
            capabilities=lk_stt.STTCapabilities(
                streaming=False, interim_results=False
            )
        )
        self._impl = impl
        self._on_language = on_language
        self._current_language = current_language

    async def recognize(
        self, buffer: lk_utils.AudioBuffer, *, language: str | None = None
    ) -> lk_stt.SpeechEvent:
        frame = lk_utils.merge_frames(buffer)
        if language is None and self._current_language is not None:
            seconds = frame.samples_per_channel / max(1, frame.sample_rate)
            if seconds < self.SHORT_CLIP_S:
                language = self._current_language() or None
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
        caption("caller", text)
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
        spoken = speakable(text)
        caption("receptionist", spoken)  # live caption on the caller's screen, as the sentence starts
        return self._stream(spoken)

    # LiveKit's AudioSource buffers about a second; one frame per sentence (several seconds) was refused with
    # "InvalidState - failed to capture frame" and the caller heard the answer cut off. Ship 100 ms frames instead.
    FRAME_MS = 100

    def _audio_frames(self, pcm: bytes) -> Iterator[lk_tts.SynthesizedAudio]:
        rate = self._impl.sample_rate
        step = max(1, rate * self.FRAME_MS // 1000) * 2  # bytes per frame (16-bit mono)
        pcm = pcm[: len(pcm) - (len(pcm) % 2)]
        request_id, segment_id = str(uuid.uuid4()), str(uuid.uuid4())
        for start in range(0, len(pcm), step):
            chunk = pcm[start:start + step]
            frame = rtc.AudioFrame(data=chunk, sample_rate=rate, num_channels=1, samples_per_channel=len(chunk) // 2)
            yield lk_tts.SynthesizedAudio(request_id=request_id, segment_id=segment_id, frame=frame)

    async def _stream(self, text: str) -> AsyncIterable[lk_tts.SynthesizedAudio]:
        stream_pcm = getattr(self._impl, "stream_pcm", None)
        if self._chunked and stream_pcm is not None:
            async for pcm in stream_pcm(text):
                for audio in self._audio_frames(pcm):
                    yield audio
            return
        pcm = await self._impl.synthesize_pcm(text)
        for audio in self._audio_frames(pcm):
            yield audio


# ---------------------------------------------------------------------------
# Function context: the 6 tools with EXACT names (SPEC §11)
# ---------------------------------------------------------------------------
class ReceptionistFunctions(lk_llm.FunctionContext):
    """The 6 tools + escalation, exposed to the LLM.

    Slow tools run through AsyncToolRunner (VOICE-SCALING §5): the filler ack
    line is spoken via `speaker` if the call outlasts the grace window, and a
    hard timeout resolves to a spoken apology instead of dead air.
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
        description="Book an appointment. Call this as soon as you have the slot and the caller's phone number — do NOT read the number back before this first call. The first call returns confirmation_required: then read the number back ONCE, and call again with the same number when the caller confirms."
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

        # Already read back and confirmed on the call? Then this first call counts as the confirmed one.
        try:
            from livekit.agents.pipeline import AgentCallContext

            call = AgentCallContext.get_current()
            msgs = list(getattr(getattr(call, "llm_stream", None), "chat_ctx", None).messages)
            if read_back_confirmed(msgs, phone):
                self._tools._session.pending_phone = (phone or "").strip()
        except Exception as exc:  # noqa: BLE001 - fall back to the normal two-step confirmation
            log.debug("read-back check skipped", error=str(exc)[:160])

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
    caller_language: str = "",
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

    # A Nigerian language the caller chose on the call page: the call starts in it; the caller may switch later.
    chosen = caller_language if caller_language in call_language.NIGERIAN and settings.naija_speech_url else ""
    base_prompt = system_prompt
    system_prompt += call_language.prompt_instruction(chosen)
    chat_ctx = lk_llm.ChatContext().append(role="system", text=system_prompt)
    runner = AsyncToolRunner(
        timeout_s=settings.tool_timeout_s,
        ack_grace_ms=settings.tool_ack_grace_ms,
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
    tts_impl.language = ""  # job processes are reused: never inherit the previous call's language
    if chosen:
        session.active_language = chosen
        tts_impl.voice = "naija:auto"  # pins the naija provider; "auto" = the service picks the language's best voice
        tts_impl.language = chosen
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

    # Mid-call language switching (Nigerian-language service configured): voice, prompt and the caller's screen follow.
    language_listeners: list[Any] = []

    def _switch_language(code: str) -> None:
        session.active_language = code
        if code in call_language.NIGERIAN:
            tts_impl.voice, tts_impl.language = "naija:auto", code
        else:
            tts_impl.voice, tts_impl.language = _voice_spec(code if code != "pcm" else "en"), ""
        prompt = base_prompt + call_language.prompt_instruction(code)
        try:
            msgs = agent.chat_ctx.messages
            if msgs and msgs[0].role == "system":
                msgs[0].content = prompt if isinstance(msgs[0].content, str) else [prompt]
        except Exception as exc:  # noqa: BLE001
            log.warning("language prompt swap failed", error=str(exc)[:200])
        for listener in list(language_listeners):
            try:
                listener(code)
            except Exception as exc:  # noqa: BLE001
                log.warning("language listener failed", error=str(exc)[:200])

    switching_stt = (
        SwitchingLanguageSTT(NaijaSpeechSTT(settings.naija_speech_url), stt_impl, chosen or "en",
                             call_language.allowed(settings.call_languages), _switch_language)
        if settings.naija_speech_url and caller_language is not None and sip_call is None else None
    )

    agent = VoicePipelineAgent(
        vad=silero.VAD.load(),
        stt=(WhisperSTTNode(switching_stt) if switching_stt is not None
             else WhisperSTTNode(stt_impl, on_language=_on_language,
                                 current_language=lambda: normalize_language(ml_state.active_language) or "en")),
        # NOTE (VOICE-SCALING §3): the livekit-plugins-openai LLM node cannot
        # hot-swap endpoints mid-process, so the LLM fallback chain
        # (app/pipeline/llm.py FallbackLLM) covers the chat/tool-loop paths;
        # the worker node relies on the primary endpoint + job-level retry.
        # Own client: the plugin's default read timeout is 5 s, which cut off every reply from a CPU-hosted model
        # (Ollama logged "cancel task" while still reading the prompt). LLM_TIMEOUT governs it instead.
        llm=_worker_llm(settings),
        tts=_naija_tts(PiperTTSNode(tts_impl, chunked=settings.tts_chunked)) if switching_stt is not None
        else PiperTTSNode(tts_impl, chunked=settings.tts_chunked),
        chat_ctx=chat_ctx,
        fnc_ctx=fnc_ctx,
        allow_interruptions=True,
    )
    agent.agora_call_language = chosen  # read by the greeting in entrypoint
    agent.agora_language_listeners = language_listeners  # entrypoint tells the caller's screen about switches
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
    return agent, ctx, session



_TRANSCRIPTS: "TranscriptRecorder | None" = None


def _message_text(msg: Any) -> str:
    content = getattr(msg, "content", msg)
    if isinstance(content, list):
        content = " ".join(str(c) for c in content if isinstance(c, str))
    return speakable(str(content or ""))


def _record_voice_transcript(agent: Any, settings: Settings, tenant_ctx: Any, session: Any, conversation_id: str,
                             site_slug: str, *, channel: str) -> None:
    """Store the call's words for the business's Conversations page: what the caller said (STT, committed turns),
    what the receptionist said (greeting included) and the tools it used. Fire-and-forget; never affects the call."""
    global _TRANSCRIPTS
    if _TRANSCRIPTS is None:
        _TRANSCRIPTS = TranscriptRecorder(getattr(settings, "conversation_url", ""))
    rec = _TRANSCRIPTS
    if not rec.enabled:
        return
    key = f"voice:{conversation_id}"

    def _send(turns: list[dict[str, Any]]) -> None:
        rec.record(key, turns, tenant_id=str(getattr(tenant_ctx, "tenant_id", "") or ""), site_slug=site_slug,
                   channel=channel, contact_phone=session.verified_phone or session.confirmed_phone or session.claimed_phone)

    def _said(role: str):
        return lambda msg, *_a, **_k: (lambda t: _send([{"role": role, "text": t}]) if t else None)(_message_text(msg))

    def _tools(called, *_a, **_k) -> None:
        turns = []
        for fn in called or []:
            info = getattr(fn, "call_info", fn)
            name = getattr(getattr(info, "function_info", None), "name", None) or getattr(info, "name", "tool")
            args = getattr(info, "arguments", None) or {}
            turns.append({"role": "tool", "text": tool_turn_text({"tool": name, "arguments": args}),
                          "tool_calls": [{"tool": name, "arguments": args}]})
        if turns:
            _send(turns)

    try:
        agent.on("user_speech_committed", _said("user"))
        agent.on("agent_speech_committed", _said("agent"))
        agent.on("function_calls_finished", _tools)
        llm = getattr(agent, "_llm", None)
        if hasattr(llm, "on_extra_tools"):
            llm.on_extra_tools(finished=_tools)  # second and later lookups for the same question
    except Exception as exc:  # noqa: BLE001 - event surface may shift; the call itself is unaffected
        log.warning("voice transcript hooks unavailable", error=str(exc)[:200])

def keep_listening_after_reconnect(agent: Any, room: Any) -> None:
    """A caller whose connection blips does a full reconnect (LiveKit logs CLIENT_REQUEST_LEAVE, then a new session
    with the same identity). livekit-agents 0.10 keeps listening to the OLD participant object, which never gets a
    microphone track again, so the receptionist went deaf for the rest of the call (seen 2026-11-16, twice, a few
    minutes into calls). Point the listener at the new participant and subscribe to its microphone."""

    def _relink(participant: Any) -> None:
        listener = getattr(agent, "_human_input", None)
        old = getattr(listener, "_participant", None)
        if listener is None or old is None or participant is old:
            return
        if getattr(participant, "identity", None) != getattr(old, "identity", None):
            return
        listener._participant = participant
        try:
            listener._subscribe_to_microphone()
        except Exception as exc:  # noqa: BLE001 - the track events retry the subscription
            log.warning("re-subscribe after reconnect failed", error=str(exc)[:200])
        log.info("caller reconnected; listening to the new connection", identity=participant.identity)

    def _trace(event: str):
        def handler(*args: Any) -> None:
            names = [getattr(a, "identity", None) or getattr(a, "sid", None) or type(a).__name__ for a in args]
            listener = getattr(agent, "_human_input", None)
            linked = getattr(getattr(listener, "_participant", None), "identity", None)
            log.info("room event", room_event=event, args=names, linked=linked,
                     listening_to=getattr(getattr(listener, "_subscribed_track", None), "sid", None))
        return handler

    try:
        room.on("participant_connected", _relink)
        # Diagnostics for mid-call reconnects: what the worker sees when a caller drops and rejoins.
        for event in ("participant_disconnected", "track_published", "track_subscribed", "track_unsubscribed",
                      "reconnecting", "reconnected"):
            room.on(event, _trace(event))
    except Exception as exc:  # noqa: BLE001
        log.warning("reconnect hook unavailable", error=str(exc)[:200])


async def _caller_language(ctx: agents.JobContext, timeout_s: float = 8.0) -> str:
    """The language the web caller chose (token metadata, app/call_language.py); "" = English / not chosen."""
    try:
        participant = await asyncio.wait_for(ctx.wait_for_participant(), timeout=timeout_s)
        return call_language.from_metadata(getattr(participant, "metadata", ""))
    except Exception as exc:  # noqa: BLE001 - no choice = the default (English) call
        log.info("caller language not read", error=str(exc)[:120])
        return ""


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
            settings, dapr, site_slug, conversation_id, sip_call=sip_call,
            caller_language=await _caller_language(ctx) if sip_call is None else "",
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
    _record_voice_transcript(agent, settings, tenant_ctx, session, conversation_id, site_slug,
                             channel="phone" if sip_call is not None else "voice")
    try:
        # Turn = one committed user utterance (livekit-agents 0.10.x event).
        agent.on("user_speech_committed", lambda *_a, **_k: metrics.session_turn())
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
    # The caller's screen: talking / typing (tool in use) / listening.
    announce_activity(agent, ctx.room)
    keep_listening_after_reconnect(agent, ctx.room)
    announce_language(agent, ctx.room)
    announce_captions(ctx.room)
    # Greet the caller so the conversation opens naturally. SPEC-W11 Part C
    # §5: build_greeting prepends/appends the pack's spoken-AI-disclosure /
    # recording-consent lines when the tenant context carries a `disclosure`
    # block; absent the block the greeting is byte-identical to before.
    await agent.say(
        call_language.greeting(getattr(agent, "agora_call_language", ""), getattr(tenant_ctx, "display_name", ""),
                               build_greeting(tenant_ctx)),
        allow_interruptions=True,
    )

    @ctx.room.on("disconnected")
    def _on_disconnected(*args) -> None:  # noqa: ARG001
        import asyncio

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
