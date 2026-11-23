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
import os
import inspect
import re
import threading
import time
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

from . import dtmf, outbound, outbound_script, emergency, ivr_menu, metrics, sip
from . import visitor as visitor_mod
from . import dead_mic, greeting_prerender
from .async_tools import (
    SLOW_TOOLS,
    VERIFICATION_TOOLS,
    AsyncToolRunner,
    ToolAckPolicy,
)
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
from . import pronunciation, tts_normalize
from .codeswitch import instruction as codeswitch_instruction
from .multilang import (
    MultilangState,
    normalize_language,
    resolve_pack_voice,
    resolve_tts_voice,
)
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
from .translation import (
    LLMTranslator,
    build_translator,
    detect_language,
    digit_words_to_digits,
    native_digits,
    process_inbound_text,
    process_outbound_text,
)
from .tts_providers.base import split_voice_spec
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
        # SPEC-W73 A2: job processes are spawned, so pre-render the configured tenants' greetings into THIS
        # process' chain (empty tenant list / failure = no-op).
        greeting_prerender.boot_prerender(settings, build_tts=lambda: tts)
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
    # Leaving English/Pidgin for Yorùbá, Hausa or Igbo needs much more: Pidgin lines were taken for Igbo at 0.78 and
    # 0.95 (the receptionist then answered a Pidgin caller in Igbo), while real switches came back at 0.99-1.00.
    MIN_CONFIDENCE_FROM_ENGLISH = 0.97

    def __init__(self, naija: NaijaSpeechSTT, whisper: Any, start: str, allowed: list[str], on_switch: Any) -> None:
        self._naija, self._whisper = naija, whisper
        self.language = start or "en"
        # The call's own language: English said inside a call that STARTED in Yoruba/Hausa/Igbo is code-switching
        # (details given in English); a caller who started in English/Pidgin and moved away returns home with it.
        self._home = self.language
        self._english_side = start if start in _ENGLISH_FAMILY else "en"
        self._allowed = [c for c in allowed if c != "ful"] or ["en"]
        self._on_switch = on_switch
        self.last_detected_language: str | None = self.language

    def _target(self, detected: str) -> str | None:
        if detected in _ENGLISH_FAMILY:
            return None if self.language in _ENGLISH_FAMILY else self._english_side
        return detected if detected != self.language else None

    # Code-switching: in a Yoruba/Hausa/Igbo call, callers often give dates, names, numbers and emails in English. Such a
    # turn is transcribed with whisper (MMS turned "yes, that's correct" into "go oton" and phone digits into nonsense)
    # but the call stays in the caller's language unless they ask to change to English.
    CODE_SWITCH_MIN_S = 1.2
    CODE_SWITCH_MIN_CONFIDENCE = 0.6
    _ASKS_FOR_ENGLISH = re.compile(r"\b(speak|talk|continue|switch|change)\b.{0,20}\benglish\b|\bin english\b", re.I)
    # A caller asking in words ("can I switch to Hausa? I feel more comfortable speaking Hausa") gets that language
    # for certain; detecting it from the sound alone missed requests made in the new language.
    # The Pidgin recogniser spells phonetically: "please kan i swish to housa i feel more komfortabol", "switch to ebol".
    _LANG_WORD = r"((?:hau|hou)sa\w*|ausa\w*|yor[uo]\w*|yorùbá|igb\w*|ibo\w*|ebo\w*|ebu\w*|ebl\w*|pidg\w*|pij\w*)"
    _ASKS_FOR_LANGUAGE = re.compile(
        r"\b(?:switch|swich|swish|swits?h|change|chenj|speak|spik|talk|tok|continue|go)\b.{0,25}?\b" + _LANG_WORD + r"\b"
        r"|\b[ck]omf\w*\s+(?:speaking\s+|speakin\s+|in\s+|en\s+|with\s+)?" + _LANG_WORD + r"\b", re.I)

    @staticmethod
    def _language_code(word: str) -> str | None:
        w = word.lower()
        if w.startswith(("ha", "ho", "au")):
            return "ha"
        if w.startswith("yo"):
            return "yo"
        if w.startswith(("ig", "ib", "eb")):
            return "ig"
        if w.startswith(("pi", "pij")):
            return "pcm"
        return None

    # A language the caller asked for in words sticks: an English-sounding half of the same sentence ("…I feel more
    # comfortable speaking Hausa") once flipped the call straight back to Pidgin. Only another spoken request moves it.
    _pinned = False

    def _follow_request(self, text: str) -> None:
        m = self._ASKS_FOR_LANGUAGE.search(text or "")
        if not m:
            return
        code = self._language_code(m.group(1) or m.group(2) or "")
        if code and code != self.language and code in self._allowed:
            self._pinned = code not in _ENGLISH_FAMILY
            self._switch(code, 1.0)

    async def _identify(self, pcm: bytes, sample_rate: int, channels: int) -> tuple[str, float]:
        try:
            out = await self._naija.identify(pcm, sample_rate=sample_rate, channels=channels, candidates=self._allowed)
        except Exception as exc:  # noqa: BLE001 - keep the current language
            log.warning("language identification failed", error=str(exc)[:200])
            return "", 0.0
        detected = str(out.get("language") or "")
        return detected, float((out.get("scores") or {}).get(detected, 0.0))

    def _switch(self, target: str, confidence: float) -> None:
        log.info("caller switched language", previous=self.language, language=target, confidence=confidence)
        self.language = target
        if target in _ENGLISH_FAMILY:
            self._english_side = target
        try:
            self._on_switch(target)
        except Exception as exc:  # noqa: BLE001
            log.warning("language switch hook failed", error=str(exc)[:200])

    async def _maybe_switch(self, pcm: bytes, sample_rate: int, channels: int) -> bool:
        """Follow a language switch. Returns True when this turn is English said inside a Nigerian-language call
        (code-switching): transcribe it in English without changing the call's language."""
        seconds = len(pcm) / (2 * max(1, channels) * max(1, sample_rate))
        in_nigerian = self.language not in _ENGLISH_FAMILY and (self._home not in _ENGLISH_FAMILY or self._pinned)
        if len(self._allowed) < 2 or seconds < (self.CODE_SWITCH_MIN_S if in_nigerian else self.MIN_SWITCH_S):
            return False
        detected, confidence = await self._identify(pcm, sample_rate, channels)
        if in_nigerian and detected in _ENGLISH_FAMILY:
            return confidence >= self.CODE_SWITCH_MIN_CONFIDENCE
        if seconds < self.MIN_SWITCH_S:
            return False
        target = self._target(detected)
        needed = (self.MIN_CONFIDENCE_FROM_ENGLISH
                  if self.language in _ENGLISH_FAMILY and target not in _ENGLISH_FAMILY else self.MIN_CONFIDENCE)
        if target and confidence >= needed:
            self._switch(target, confidence)
        return False

    async def transcribe_pcm(self, pcm: bytes, *, sample_rate: int = 16000, channels: int = 1,
                             language: str | None = None, initial_prompt: str | None = None) -> str:
        english_turn = await self._maybe_switch(pcm, sample_rate, channels)
        extra = {"initial_prompt": initial_prompt} if initial_prompt else {}
        if english_turn:
            text = await self._whisper.transcribe_pcm(pcm, sample_rate=sample_rate, channels=channels, language="en", **extra)
            if self._ASKS_FOR_ENGLISH.search(text or ""):
                self._pinned = False
                self._switch(self._english_side, 1.0)
            else:
                log.info("english turn inside a nigerian-language call", language=self.language)
                self._follow_request(text)
            self.last_detected_language = self.language
            return text
        self.last_detected_language = self.language
        text = None
        if self.language in call_language.NIGERIAN:
            try:
                text = await self._naija.transcribe_pcm(pcm, sample_rate=sample_rate, channels=channels,
                                                        language=self.language)
            except Exception as exc:  # noqa: BLE001 - keep hearing the caller
                log.warning("naija speech recognition failed; using whisper", language=self.language,
                            error=str(exc)[:200])
        if text is None:
            text = await self._whisper.transcribe_pcm(pcm, sample_rate=sample_rate, channels=channels, language="en",
                                                      **extra)
        self._follow_request(text)
        self.last_detected_language = self.language
        return text


class ChosenLanguageSTT:
    """Speech recognition for a call whose caller chose a Nigerian language: MMS on the GPU host with that language's
    adapter; whisper is the fallback if the service fails (so the call never goes deaf). Reports the chosen language
    as "detected" so nothing downstream flips the call back to English."""

    def __init__(self, naija: NaijaSpeechSTT, whisper: Any, language: str) -> None:
        self._naija, self._whisper, self._language = naija, whisper, language
        self.last_detected_language: str | None = language

    async def transcribe_pcm(self, pcm: bytes, *, sample_rate: int = 16000, channels: int = 1,
                             language: str | None = None, initial_prompt: str | None = None) -> str:
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


def call_voice(code: str, *, english_voice: str, naija: bool, voice_spec: Any,
               language_voices: dict[str, str] | None = None) -> tuple[str, str]:
    """(voice spec, synthesis language) for a call language code (en, pcm, yo, ha, ig, ful, or a locale like en-NG).

    With a Nigerian English voice configured, English and Pidgin are spoken by it (YarnGPT reads Pidgin naturally; the
    MMS Pidgin voice sounds robotic). Other Nigerian languages use the service's own voice for that language."""
    code = normalize_language(code) or (code or "en").strip().lower()
    if english_voice and ":" in english_voice and code in ("en", "pcm"):
        return english_voice, "en"  # a full provider spec, e.g. "edge:en-NG-AbeoNeural"
    if english_voice and naija and code in ("en", "pcm"):
        return f"naija:{english_voice}", "en"
    if naija and code in call_language.NIGERIAN:
        # pins the naija provider; "auto" = the service picks the language's default voice
        return f"naija:{(language_voices or {}).get(code) or 'auto'}", code
    return voice_spec(code if code != "pcm" else "en"), ""


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
    # The latest receptionist line that read this number back (times may have been offered after it)...
    def _said_digits(text: str) -> str:
        # Read-backs are spoken in words ("zero eight zero two, five five five…"), so words count as digits too.
        return re.sub(r"\D", "", text) + "|" + re.sub(r"\D", "", spoken_phone(text) or "")

    read_back = next((i for i in range(len(messages) - 1, -1, -1) if getattr(messages[i], "role", "") == "assistant"
                      and digits[-7:] in _said_digits(_message_plain(messages[i]))), None)
    if read_back is None:
        return False
    # ...and the caller's answer right after it was yes.
    reply = next((_message_plain(m) for m in messages[read_back + 1:] if getattr(m, "role", "") == "user"), "")
    return bool(_YES.search(reply))


import unicodedata as _ud

# Spoken digits in English/Pidgin, Yoruba, Hausa and Igbo (accents stripped: speech recognisers drop them unevenly).
_DIGIT_WORDS = {
    **dict.fromkeys(["zero", "oh", "o"], "0"), "one": "1", "two": "2", "three": "3", "four": "4", "five": "5",
    "six": "6", "seven": "7", "eight": "8", "nine": "9",
    "odo": "0", "okan": "1", "meji": "2", "meta": "3", "merin": "4", "marun": "5", "marunun": "5", "mefa": "6",
    "meje": "7", "mejo": "8", "mesan": "9", "mesanan": "9",
    "sifiri": "0", "daya": "1", "biyu": "2", "uku": "3", "hudu": "4", "biyar": "5", "shida": "6", "bakwai": "7",
    "takwas": "8", "tara": "9",
    "efu": "0", "otu": "1", "abuo": "2", "ato": "3", "ano": "4", "ise": "5", "isii": "6", "asaa": "7", "asato": "8",
    "itoolu": "9",
}
_MULT = {"double": 2, "triple": 3}
_NG_MOBILE = re.compile(r"(?:0|234)[789][01]\d{8}")


def _plain_word(w: str) -> str:
    w = _ud.normalize("NFKD", w.lower().translate(str.maketrans("ɗƙƴ", "dky")))
    return "".join(c for c in w if c.isalnum() and not _ud.combining(c))


def spoken_phone(text: str) -> str | None:
    """A Nigerian mobile number spoken in a line ("zero eight zero six, five five five…", "double five", digits, or
    Yoruba/Hausa/Igbo number words) as 0XXXXXXXXXX, or None."""
    digits, mult = [], 1
    for raw in re.split(r"[\s,.;:!?\-–—]+", text or ""):
        if not raw:
            continue
        if raw.isdigit():
            digits.append(raw * 1); mult = 1; continue
        w = _plain_word(raw)
        if w in _MULT:
            mult = _MULT[w]; continue
        if w in _DIGIT_WORDS:
            digits.append(_DIGIT_WORDS[w] * mult); mult = 1
    m = _NG_MOBILE.search("".join(digits))
    if not m:
        return None
    n = m.group(0)
    return "0" + n[3:] if n.startswith("234") else n


def caller_phone(messages: list[Any]) -> str | None:
    """The phone number the caller most recently said, from their own words (newest first)."""
    for m in reversed(messages):
        if getattr(m, "role", "") == "user":
            found = spoken_phone(_message_plain(m))
            if found:
                return found
    return None


_MONTHS = {m: i for i, m in enumerate(["january", "february", "march", "april", "may", "june", "july", "august",
                                         "september", "october", "november", "december"], 1)}
_ORDINAL_WORDS = {"first": 1, "second": 2, "third": 3, "fourth": 4, "fifth": 5, "sixth": 6, "seventh": 7, "eighth": 8,
                  "ninth": 9, "tenth": 10, "eleventh": 11, "twelfth": 12, "thirteenth": 13, "fourteenth": 14,
                  "fifteenth": 15, "sixteenth": 16, "seventeenth": 17, "eighteenth": 18, "nineteenth": 19,
                  "twentieth": 20, "thirtieth": 30, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
                  "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13,
                  "fourteen": 14, "fifteen": 15, "sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
                  "twenty": 20, "thirty": 30}
_MONTH_RE = "(" + "|".join(_MONTHS) + ")"
_DAY_RE = r"(\d{1,2}|(?:twenty|thirty)?[- ]?(?:first|second|third|fourth|fifth|sixth|seventh|eighth|ninth|one|two|three|four|five|six|seven|eight|nine)|tenth|eleventh|twelfth|thirteenth|fourteenth|fifteenth|sixteenth|seventeenth|eighteenth|nineteenth|twentieth|thirtieth|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|twenty|thirty)"


def _day_number(word: str) -> int | None:
    word = word.strip().lower()
    if word.isdigit():
        return int(word)
    total = 0
    for part in re.split(r"[- ]+", word):
        if part not in _ORDINAL_WORDS:
            return None
        total += _ORDINAL_WORDS[part]
    return total or None


def caller_date(messages: list[Any]) -> tuple[int, int] | None:
    """(month, day) of the date the caller most recently said outright ("24 November", "November 24th", "the
    twenty-fourth of November"), newest first; None when they gave no explicit date."""
    pats = [re.compile(rf"\b{_DAY_RE}(?:st|nd|rd|th)?\s+(?:of\s+)?{_MONTH_RE}\b", re.I),
            re.compile(rf"\b{_MONTH_RE}\s+(?:the\s+)?{_DAY_RE}(?:st|nd|rd|th)?\b", re.I)]
    for m in reversed(messages):
        if getattr(m, "role", "") != "user":
            continue
        text = _message_plain(m)
        for i, pat in enumerate(pats):
            hit = pat.search(text)
            if hit:
                day_word, month_word = (hit.group(1), hit.group(2)) if i == 0 else (hit.group(2), hit.group(1))
                day = _day_number(day_word)
                if day and 1 <= day <= 31:
                    return _MONTHS[month_word.lower()], day
    return None


_WEEKDAYS = ["monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"]
_ORD_WORDS = ["", "first", "second", "third", "fourth", "fifth", "sixth", "seventh", "eighth", "ninth", "tenth",
              "eleventh", "twelfth", "thirteenth", "fourteenth", "fifteenth", "sixteenth", "seventeenth", "eighteenth",
              "nineteenth", "twentieth", "twenty-first", "twenty-second", "twenty-third", "twenty-fourth",
              "twenty-fifth", "twenty-sixth", "twenty-seventh", "twenty-eighth", "twenty-ninth", "thirtieth",
              "thirty-first"]
_WEEKDAY_DATE = [
    re.compile(rf"\b(?P<wd>{'|'.join(_WEEKDAYS)}),?\s+(?:the\s+)?(?P<day>{_DAY_RE})(?P<suf>st|nd|rd|th)?\s+(?:of\s+)?(?P<mon>{_MONTH_RE})\b", re.I),
    re.compile(rf"\b(?P<wd>{'|'.join(_WEEKDAYS)}),?\s+(?P<mon>{_MONTH_RE})\s+(?:the\s+)?(?P<day>{_DAY_RE})(?P<suf>st|nd|rd|th)?\b", re.I),
]


def fix_weekday_dates(text: str, tz_name: str | None, now: Any = None) -> str:
    """Make every "<weekday> <date>" the receptionist says agree: the date is moved to that weekday's date in the
    coming two weeks. gemma said "Tuesday, November twenty-fifth" with "next Tuesday = 24 November" in its prompt."""
    from datetime import datetime, timedelta
    from zoneinfo import ZoneInfo

    tz = ZoneInfo(tz_name or "UTC")
    today = (now or datetime.now(tz)).astimezone(tz).date()

    def fix(m: "re.Match[str]") -> str:
        day = _day_number(m.group("day"))
        if not day:
            return m.group(0)
        month = _MONTHS[m.group("mon").lower()]
        wd = _WEEKDAYS.index(m.group("wd").lower())
        try:
            said = today.replace(month=month, day=day)
        except ValueError:
            return m.group(0)
        if said < today:
            said = said.replace(year=said.year + 1)
        if said.weekday() == wd or (said - today).days > 14:
            return m.group(0)
        right = next(today + timedelta(days=i) for i in range(1, 8) if (today + timedelta(days=i)).weekday() == wd)
        if right.month != month:
            return m.group(0)
        word = m.group("day")
        new = str(right.day) if word.isdigit() else _ORD_WORDS[right.day]
        if word[:1].isupper():
            new = new.capitalize()
        start, end = m.span("day")
        return m.group(0)[: start - m.start()] + new + m.group(0)[end - m.start():]

    for pat in _WEEKDAY_DATE:
        text = pat.sub(fix, text)
    return text


def date_mismatch_note(messages: list[Any], when_iso: str, tz_name: str | None) -> str | None:
    """A correction when the tool date is not the date the caller said (a Hausa call booked Wednesday 25 November
    after the caller said "Tuesday, 24 November" twice); None when they agree or the caller gave no date."""
    from datetime import datetime
    from zoneinfo import ZoneInfo

    said = caller_date(messages)
    if not said:
        return None
    try:
        dt = datetime.fromisoformat(str(when_iso).replace("Z", "+00:00"))
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=ZoneInfo(tz_name or "UTC"))
        local = dt.astimezone(ZoneInfo(tz_name or "UTC"))
    except Exception:  # noqa: BLE001 - unparseable: let the booking service judge it
        return None
    if (local.month, local.day) == said:
        return None
    try:
        wanted = local.replace(month=said[0], day=said[1])
    except ValueError:
        return None
    if wanted.date() < datetime.now(local.tzinfo).date():
        wanted = wanted.replace(year=wanted.year + 1)
    return (f"date_mismatch: the caller asked for {wanted.strftime('%A')} {wanted.day} {wanted.strftime('%B')}, "
            f"but this call used {local.strftime('%A')} {local.day} {local.strftime('%B')}. Call again with "
            f"{wanted.strftime('%Y-%m-%d')} and tell the caller {wanted.strftime('%A')} {wanted.day} "
            f"{wanted.strftime('%B')}.")


# The receptionist sometimes repeats a misheard language name back ("we can continue in Eboe").
_LANGUAGE_NAME_FIX = {"eboe": "Igbo", "ebo": "Igbo", "ebol": "Igbo", "ebul": "Igbo", "ebl": "Igbo", "igbol": "Igbo",
                      "ibo": "Igbo", "housa": "Hausa", "housar": "Hausa", "hausar": "Hausa", "ausa": "Hausa",
                      "yoroba": "Yoruba"}
_LANGUAGE_NAMES_HEARD = re.compile(r"\b(" + "|".join(_LANGUAGE_NAME_FIX) + r")\b", re.I)
_REAL_LANGUAGES = {"english", "pidgin", "yoruba", "yorùbá", "hausa", "igbo", "fulfulde"}
_CONTINUE_IN = re.compile(r"\b(continue|carry on|switch|go on)\s+(in|to|with)\s+([A-Z][\w'’-]+)")


def fix_continue_in(text: str, language: str | None) -> str:
    """"We can continue in Ebon" -> "…in Igbo": a word that is no language becomes the call's language."""
    name = call_language.NAMES.get(language or "", ("", ""))[0]
    if not name:
        return text
    return _CONTINUE_IN.sub(lambda m: m.group(0) if m.group(3).lower() in _REAL_LANGUAGES
                            else f"{m.group(1)} {m.group(2)} {name}", text)


_LEAKED_TOKEN = re.compile(r"(?:(?<=[.!?]\s)|^)[a-z]{8,}\s+(?=[A-Z])")


SWITCH_NOTE_MARK = "[language switch]"


def _is_switch_note(m: Any) -> bool:
    return getattr(m, "role", "") == "system" and SWITCH_NOTE_MARK in _message_plain(m)


def plausible_ng_phone(phone: str) -> bool:
    return bool(_NG_MOBILE.fullmatch(re.sub(r"\D", "", phone or "")))


_TAG = re.compile(r"</?[a-zA-Z][^>]{0,80}>")
_MARKDOWN = re.compile(r"[*_#`~|]+")


# Byte-fallback tokens the model sometimes emits instead of a character ("<0xC7><0xB8>jẹ́", "<0xA0> Note"): a run
# of them is the UTF-8 encoding of the real text, so it is decoded back; bytes that do not form a character
# (a lone "<0xA0>") are dropped. They showed up raw in Hausa/Yoruba captions and transcripts (2026-11-21).
_BYTE_TOKENS = re.compile(r"(?:<0x[0-9A-Fa-f]{2}>)+")


def _decode_byte_tokens(text: str) -> str:
    def repl(m: re.Match) -> str:
        raw = bytes(int(h, 16) for h in re.findall(r"<0x([0-9A-Fa-f]{2})>", m.group(0)))
        return raw.decode("utf-8", errors="ignore").replace("\u00a0", " ")
    return _BYTE_TOKENS.sub(repl, text)


def speakable(text: str) -> str:
    """What the voice should say: the model sometimes writes HTML ("</li><li>") or markdown, which was read aloud."""
    out = _MARKDOWN.sub("", _TAG.sub(" ", _decode_byte_tokens(text or "")))
    return re.sub(r"\s{2,}", " ", out).strip()


class WhisperSTTNode(lk_stt.STT):
    """Bridge FasterWhisperSTT into the LiveKit pipeline.

    Wave 5 #3: when whisper auto-detects the spoken language it is reported
    on the SpeechEvent and forwarded to ``on_language`` (the per-session
    MultilangState hook that swaps the piper voice + locale instruction).

    SPEC-W82 A2/A4.4: ``hints`` (per-tenant VOICE_STT_HINTS entry) supplies
    a forced whisper ``language=`` and an ``initial_prompt`` (fail-open:
    None/empty keeps the pre-W82 call exactly); ``on_transcript`` is an
    optional async post-STT hook ``(text, detected) -> text`` — the W82
    inbound-translation seam. Hook failures are logged and the ORIGINAL
    transcript is used (translation never breaks STT).
    """

    # Whisper's language guess is unreliable on short clips: "Nine o'clock is fine." (1.5 s) came back as German
    # ("Nein, der Klo"). Below this length the call's current language is used instead of a guess.
    SHORT_CLIP_S = 3.0

    def __init__(
        self,
        impl: FasterWhisperSTT,
        on_language=None,
        on_transcript=None,
        hints: dict | None = None,
        current_language=None,
    ) -> None:
        super().__init__(
            capabilities=lk_stt.STTCapabilities(
                streaming=False, interim_results=False
            )
        )
        self._impl = impl
        self._on_language = on_language
        self._current_language = current_language
        self._on_transcript = on_transcript
        self._hints = hints or None

    async def recognize(
        self, buffer: lk_utils.AudioBuffer, *, language: str | None = None
    ) -> lk_stt.SpeechEvent:
        frame = lk_utils.merge_frames(buffer)
        if language is None and self._current_language is not None:
            seconds = frame.samples_per_channel / max(1, frame.sample_rate)
            if seconds < self.SHORT_CLIP_S:
                language = self._current_language() or None
        # SPEC-W82 A4.4: an explicit language= argument still wins; the
        # tenant hint only forces a language when the pipeline left it open.
        forced_language = language
        initial_prompt = None
        if self._hints:
            forced_language = forced_language or self._hints.get("language") or None
            initial_prompt = self._hints.get("initial_prompt") or None
        # initial_prompt only when a hint supplies one: recognisers without the parameter keep working.
        extra = {"initial_prompt": initial_prompt} if initial_prompt else {}
        text = await self._impl.transcribe_pcm(
            bytes(frame.data),
            sample_rate=frame.sample_rate,
            channels=frame.num_channels,
            language=forced_language,
            **extra,
        )
        detected = forced_language or self._impl.last_detected_language or ""
        if detected and self._on_language is not None:
            try:
                self._on_language(detected)
            except Exception as exc:  # noqa: BLE001 - never break STT
                log.warning("language hook failed", error=str(exc)[:200])
        caption("caller", text)  # the caller sees what they said, before any translation
        # SPEC-W82 A2: post-STT inbound-translation hook (pcm/yo/ig/ha -> en
        # before the LLM turn; the original transcript is stashed on the
        # tenant context by the hook). Fail-open: any hook failure keeps the
        # original transcript.
        if self._on_transcript is not None and text.strip():
            try:
                text = await self._on_transcript(text, detected)
            except Exception as exc:  # noqa: BLE001 - never break STT
                log.warning("transcript hook failed", error=str(exc)[:200])
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

    SPEC-W82 A3/A4.3/A4.5: ``on_text`` is an optional async pre-TTS
    transform ``text -> text`` (outbound translation -> spoken-entity
    normalization -> pronunciation lexicon). It runs BEFORE provider
    dispatch, and any failure falls back to the original text (fail-open —
    the reply is spoken untransformed rather than dropped).
    """

    def __init__(
        self, impl: TTSInterface, *, chunked: bool = True, on_text=None
    ) -> None:
        super().__init__(
            capabilities=lk_tts.TTSCapabilities(streaming=False),
            sample_rate=impl.sample_rate,
            num_channels=1,
        )
        self._impl = impl
        self._chunked = chunked
        self._on_text = on_text

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
        # SPEC-W82 A3/A4.3/A4.5: pre-TTS transform (outbound translation,
        # normalization, pronunciation) BEFORE provider dispatch; fail-open
        # to the original text on any error.
        if self._on_text is not None:
            try:
                text = await self._on_text(text)
            except Exception as exc:  # noqa: BLE001 - never break TTS
                log.warning("pre-TTS transform failed", error=str(exc)[:200])
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
        # The call's recent spoken lines (role, content), filled from the agent's committed-speech events; used to
        # see whether a phone number was already read back and confirmed before the first booking call.
        self.recent_lines: list[Any] = []
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

        note = date_mismatch_note(list(self.recent_lines), from_iso, self._tools._ctx.timezone)
        if note:
            log.info("availability date differs from the caller's; asked the model to correct it")
            return json.dumps({"slots": [], "note": note}, ensure_ascii=False)
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

        note = date_mismatch_note(list(self.recent_lines), starts_at, self._tools._ctx.timezone)
        if note:
            log.info("booking date differs from the caller's; asked the model to correct it")
            return json.dumps({"status": "not_booked", "note": note}, ensure_ascii=False)
        # The model sometimes miscounts spoken digits ("zero eight zero six…" became 080065553108): when its number is
        # not a valid Nigerian mobile, use the number the caller actually said.
        try:
            if not plausible_ng_phone(phone):
                heard = caller_phone(list(self.recent_lines))
                if heard:
                    log.info("booking phone corrected from the caller's own words")
                    phone = heard
        except Exception as exc:  # noqa: BLE001
            log.warning("caller phone check skipped", error=str(exc)[:160])
        # Already read back and confirmed on the call? Then this first call counts as the confirmed one.
        try:
            if read_back_confirmed(list(self.recent_lines), phone):
                self._tools._session.pending_phone = (phone or "").strip()
                log.info("phone already confirmed by read-back; booking without a second confirmation")
        except Exception as exc:  # noqa: BLE001 - fall back to the normal two-step confirmation
            log.warning("read-back check skipped", error=str(exc)[:160])

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
        description=(
            "Read digits the caller types on their phone keypad (phone "
            "calls only). Use after request_verification_code so the "
            "caller can type the 6-digit code instead of reading it "
            "aloud: call with expected_length 6, then submit the returned "
            "digits with verify_caller_code. The caller presses # to "
            "submit early or * to clear."
        )
    )
    async def read_keypad_entry(self, timeout_s: int = 30, expected_length: int = 0) -> str:
        import json

        result = await self._run_slow(
            "read_keypad_entry",
            lambda: self._tools.read_keypad_entry(
                timeout_s=timeout_s,
                expected_length=expected_length or None,
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
            "Save the caller's name, email and phone number to the CRM as soon as they have CONFIRMED the "
            "read-back at the start of the call, so the business can always reach them, even if they never "
            "book. Call it once, right after the caller says the details are correct."
        )
    )
    async def save_caller_details(self, name: str, phone: str, email: str = "") -> str:
        import json

        result = await self._tools.save_caller_details(name=name, phone=phone, email=email or None)
        return json.dumps(result, ensure_ascii=False)

    @lk_llm.ai_callable(
        description=(
            "Send the person a WhatsApp message confirming their appointment (service, with whom, when). Call it "
            "once, right after they confirm an appointment or you book one. Mention the WhatsApp only if this "
            "returns status sent."
        )
    )
    async def send_whatsapp_confirmation(self, offering: str, when: str, team_member: str = "") -> str:
        import json

        result = await self._tools.send_whatsapp_confirmation(offering=offering, when=when, team_member=team_member or None)
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
    stored_conversation_id: str | None = None,
    direction: str | None = None,
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
            stored_conversation_id=stored_conversation_id,
            direction=direction,
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
    dtmf_buffer: "dtmf.DtmfBuffer | None" = None,
    visitor: "visitor_mod.Visitor | None" = None,
) -> tuple[VoicePipelineAgent, TenantContext, SessionState]:
    """Bootstrap tenant context and assemble the VoicePipelineAgent.

    Wave 5 #1: ``sip_call`` (from app/sip.bootstrap_inbound_call) attaches
    the carrier-asserted caller ID as the session's CLAIMED (unverified)
    phone before the prompt is rendered, so the receptionist can greet with
    the number on file — but the K15(c) OTP gate still applies before any
    booking lookup/change (the carrier-asserted bypass was removed).

    SPEC-W86: ``dtmf_buffer`` (created by the entrypoint for PSTN calls so
    it can also register the room data-received listener — the 3-tuple
    return contract is unchanged) is attached to the ToolLayer so
    read_keypad_entry can collect keypad digits; None on non-SIP paths.
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
        if sip_call.tenant_source == "outbound":
            # 2026-11-22: we placed this call (app/outbound_script.py): who we called and why, from the dialer.
            brief = outbound_script.from_attributes(sip_call.attributes)
            session.caller_name = brief.contact_name or None
            session.outbound_purpose = brief.purpose or "general"
            # We dialed this number and the person answered on it, so it is their phone for the booking read-back
            # policy (unlike an inbound caller ID, which can be spoofed — K15(c) stays as is for inbound). This is NOT
            # identity verification: lookups/changes of existing bookings still need the one-time code.
            if caller_phone:
                session.confirmed_phone = caller_phone
            # WhatsApp confirmations: the number typed into the demo phone; the number we reached only when
            # WHATSAPP_CONFIRMATIONS=on (demo contacts carry realistic numbers that may be real people's).
            session.whatsapp_to = (sip_call.attributes or {}).get("agora.whatsapp") or (
                caller_phone if settings.whatsapp_confirmations == "on" else "") or None
            session.outbound_prompt = outbound_script.prompt_block(brief, caller_phone, whatsapp=bool(session.whatsapp_to))
            session.outbound_greeting = outbound_script.greeting(ctx, brief)
            log.info("outbound call brief", purpose=brief.purpose or "general", named=bool(brief.contact_name),
                     facts=sorted(brief.context))
    else:
        # 2026-11-21: the web caller's pre-call form (self-reported; app/visitor.py).
        visitor_mod.apply(session, visitor)
    # SPEC-W96 IVR-A: parse the keypad menu for PSTN sessions when enabled
    # ("" env -> DEFAULT_MENU; "none" -> disabled; web/non-SIP -> none).
    # Stashed on the session for the entrypoint's greeting line + buffer
    # arming; the prompt addendum rides every build_system_prompt below
    # so the mapping survives per-turn rebuilds (locale switches).
    menu = (
        ivr_menu.parse_ivr_menu(settings.ivr_menu)
        if (sip_call is not None and settings.ivr_menu_enabled)
        else ()
    )
    session.ivr_menu = menu or None
    keypad_hint = ivr_menu.prompt_addendum(menu) if menu else None
    tool_layer = ToolLayer(
        dapr=dapr,
        settings=settings,
        ctx=ctx,
        session=session,
        # SPEC-W86: PSTN keypad capture buffer (None on non-SIP paths).
        dtmf_buffer=dtmf_buffer,
    )
    system_prompt = build_system_prompt(
        ctx,
        conversation_id=conversation_id,
        caller_phone=caller_phone or None,
        keypad_menu_hint=keypad_hint,
        visitor=visitor_mod.from_session(session),
        outbound=session.outbound_prompt,
    )

    # A Nigerian language the caller chose on the call page: the call starts in it; the caller may switch later.
    chosen = caller_language if caller_language in call_language.NIGERIAN and settings.naija_speech_url else ""
    base_prompt = system_prompt
    system_prompt += call_language.prompt_instruction(chosen, think_in_english=settings.naija_think_in_english)
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
            **{
                tool: settings.verification_tool_timeout_s
                for tool in VERIFICATION_TOOLS
            },
            # SPEC-W86: read_keypad_entry WAITS on the caller's keypad for
            # up to 60s — it needs a budget above the max wait (default
            # 65s); the global 4s tool timeout stays untouched.
            "read_keypad_entry": settings.keypad_tool_timeout_s,
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

    # SPEC-W82 A1: translation layer for this session (mock-default;
    # fail-open — a turn never blocks on translation).
    translator = build_translator(settings)
    if settings.naija_think_in_english:
        # Think in English: what Yoruba/Hausa/Igbo callers say must reach the receptionist in English, read with the
        # call's context (the mock translator passed the recogniser's rough phonetic text straight through, and the
        # receptionist answered questions nobody asked).
        def _inbound_context() -> dict:
            return {"business": getattr(ctx, "display_name", ""), "industry": getattr(ctx, "industry", ""),
                    "services": [o.get("name") for o in (getattr(ctx, "offerings", None) or []) if isinstance(o, dict)],
                    "last_line": getattr(session, "last_agent_en", "")}

        translator = LLMTranslator(settings.llm_base_url, settings.llm_model, api_key=settings.llm_api_key,
                                   extra_body=settings.llm_extra_body or None, context=_inbound_context)

    def _voice_spec(language: str) -> str:
        # SPEC-W82 A4.2: the pack `voice:` per-language map slots into the
        # precedence chain ctx.tts_voice -> pack map -> TTS_VOICE_MAP ->
        # PIPER_VOICE_MAP. A pack entry naming a provider that is NOT in
        # the configured chain is ignored with a warning (never a crash);
        # resolution then falls through to the env maps.
        override = getattr(ctx, "tts_voice", None)
        if isinstance(override, str) and override.strip():
            return override.strip()
        pack_spec = resolve_pack_voice(language, getattr(ctx, "pack_voice_map", None))
        if pack_spec:
            pref, _ = split_voice_spec(pack_spec)
            if pref and isinstance(tts_impl, FallbackTTS) and pref not in tts_impl.order:
                log.warning(
                    "pack voice spec provider not in TTS chain; ignoring",
                    provider=pref,
                    spec=pack_spec,
                    language=language,
                )
            else:
                return pack_spec
        return resolve_tts_voice(
            language,
            settings.tts_voice_map,
            settings.piper_voice_map,
            settings.piper_voice,
            ctx=ctx,
        )

    # A Nigerian-accented English voice (NAIJA_ENGLISH_VOICE): English and Pidgin are spoken by it, not piper/MMS.
    english_voice = settings.naija_english_voice if (settings.naija_speech_url or ":" in settings.naija_english_voice) else ""

    def _set_voice(code: str) -> None:
        tts_impl.voice, tts_impl.language = call_voice(code, english_voice=english_voice,
                                                       naija=bool(settings.naija_speech_url), voice_spec=_voice_spec,
                                                       language_voices=settings.naija_language_voices)

    tts_impl.voice = _voice_spec(ml_state.active_language)
    # SPEC-W82 A4.2 CRITICAL FIX: FallbackTTS.language tracks the session's active language from bootstrap (provider
    # chain routing). Job processes are reused, so it is always reset here; the Nigerian English voice / chosen call
    # language below then set voice + language together.
    tts_impl.language = ml_state.active_language or ""
    if english_voice:
        _set_voice(ml_state.active_language)
    if chosen:
        session.active_language = chosen
        _set_voice(chosen)
    # SPEC-W38 F2: definition.voice overrides the session's default voice
    # (per-turn language switching still applies on top). Only piper /
    # provider-less voices are applied here — other providers go through the
    # chain's own voice maps.
    definition = getattr(ctx, "agent_definition", None)
    if (
        definition is not None
        and definition.voice.voice_id
        and definition.voice.provider in ("", "piper")
        and not english_voice  # the Nigerian English voice replaces piper for the whole deployment
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
        if english_voice and (normalize_language(detected) in ("en", "pcm")):
            _set_voice(detected)
        if not switched:
            return
        session.active_language = ml_state.active_language
        # SPEC-W82 A4.2: keep the provider chain's language routing in step
        # (closes the chain.py:113 never-set gap on switches too).
        tts_impl.language = ml_state.active_language
        new_prompt = build_system_prompt(
            ctx,
            conversation_id=conversation_id,
            language=ml_state.active_language,
            caller_phone=session.confirmed_phone or session.claimed_phone,
            keypad_menu_hint=keypad_hint,
            visitor=visitor_mod.from_session(session),
            outbound=session.outbound_prompt,
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
        _set_voice(code)
        prompt = base_prompt + call_language.prompt_instruction(code, think_in_english=settings.naija_think_in_english)
        try:
            msgs = agent.chat_ctx.messages
            if msgs and msgs[0].role == "system":
                msgs[0].content = prompt if isinstance(msgs[0].content, str) else [prompt]
            # The opening system prompt alone lost to the recent turns: after Hausa turns the model answered an Igbo
            # turn in Hausa. A note at the end of the conversation, right before the reply, is followed reliably.
            name = call_language.NAMES.get(code, (code, code))[0]
            if settings.naija_think_in_english and code in call_language.SPOKEN_BY_TRANSLATION:
                note = (f"{SWITCH_NOTE_MARK} The caller asked to continue in {name}. Your NEXT reply must be exactly "
                        f"this, and nothing else: \"{call_language.agreement_en(code)}\" Then wait for what they want. "
                        f"Never repeat it in later replies. Keep writing in plain English: it is translated into {name} "
                        f"before they hear it.")
            else:
                note = (f"{SWITCH_NOTE_MARK} The caller has switched to {name}. Begin your NEXT reply only with one "
                        f"short, warm sentence in "
                        f"{name} agreeing to continue in {name} (for example: of course, we can continue in {name}), "
                        f"then answer what they asked. From now on reply ONLY in {name}. If some words are unclear, say "
                        f"so in {name} and ask them to repeat — do not change language yourself.")
            msgs[:] = [m for m in msgs if not _is_switch_note(m)]  # one switch, one agreement
            msgs.append(lk_llm.ChatMessage.create(text=note, role="system"))
        except Exception as exc:  # noqa: BLE001
            log.warning("language prompt swap failed", error=str(exc)[:200])
        for listener in list(language_listeners):
            try:
                listener(code)
            except Exception as exc:  # noqa: BLE001
                log.warning("language listener failed", error=str(exc)[:200])

    switching_stt = (
        SwitchingLanguageSTT(NaijaSpeechSTT(settings.naija_speech_url), stt_impl, chosen or "en",
                             call_language.allowed(settings.call_languages)
                             if (settings.voice_language_switch_mode == "on"
                                 or (settings.voice_language_switch_mode == "nigerian" and chosen in call_language.NIGERIAN))
                             else [chosen or "en"], _switch_language)
        if settings.naija_speech_url and caller_language is not None and sip_call is None else None
    )

    async def _on_transcript(text: str, detected: str) -> str:
        """SPEC-W82 A2/A4.1 post-STT hook: pure pcm/yo/ig/ha transcripts are
        translated to English before the LLM turn; the original transcript
        and detection are stashed on the per-turn tenant context, the
        session active_language + chain language + voice spec follow the
        caller, and the locale instruction is swapped in. Code-switched
        pcm/en turns are NOT force-translated (mixed register instruction
        instead). Fail-open: any error keeps the original transcript.
        """
        inbound = await process_inbound_text(
            text,
            translator,
            enabled=settings.voice_translate_enabled,
            path="voice",
            # The recogniser that heard this turn was for the caller's language: trust it over spelling heuristics.
            language_hint=detected if detected in ("yo", "ha", "ig") else None,
        )
        lang = inbound.detected_language
        if not lang or lang == "en":
            return inbound.text
        ctx.detected_language = lang
        ctx.original_text = inbound.original_text
        session.active_language = lang
        # SPEC-W82 A4.2: chain language routing + per-language voice follow
        # the detected caller language (pcm included — MMS/Spitch have real
        # pcm/yo/ig/ha voices even though whisper reports pcm as en).
        tts_impl.language = lang
        tts_impl.voice = _voice_spec(lang)
        try:
            new_prompt = build_system_prompt(
                ctx,
                conversation_id=conversation_id,
                language=lang,
                caller_phone=session.confirmed_phone or session.claimed_phone,
                keypad_menu_hint=keypad_hint,
                visitor=visitor_mod.from_session(session),
                outbound=session.outbound_prompt,
            )
            if inbound.code_switch:
                ctx.code_switch = True
                ctx.code_switch_segments = list(inbound.segments)
                new_prompt = f"{new_prompt}{codeswitch_instruction()}"
            msgs = agent.chat_ctx.messages
            if msgs and msgs[0].role == "system":
                msgs[0].content = [new_prompt]
        except Exception as exc:  # noqa: BLE001 - prompt stays at default
            log.warning("locale instruction swap failed", error=str(exc)[:200])
        return inbound.text

    # SPEC-W82 A4.4: per-tenant STT hints (forced language + initial_prompt),
    # keyed by site slug first, then tenant slug. Fail-open when unset.
    stt_hints = (
        settings.voice_stt_hints.get(site_slug)
        or settings.voice_stt_hints.get(ctx.tenant_slug)
        or None
    )

    # SPEC-W82 A4.5: effective pronunciation lexicon for this tenant
    # (env "*" < env tenant < pack voice.lexicon — pack wins).
    lexicon = pronunciation.merge_lexicons(
        settings.voice_pronunciation_lexicon,
        ctx.tenant_slug,
        getattr(ctx, "pack_pronunciation_lexicon", None),
    )

    async def _prepare_tts_text(text: str) -> str:
        """SPEC-W82 A3/A4.3/A4.5 pre-TTS transform, applied in order:
        1. outbound translation en -> session active_language (A3; fail-open
           English, skipped when the reply is already non-English);
        2. Naija spoken-entity normalization (A4.3: naira amounts, phone
           digit read-back, dates, abbreviations);
        3. pronunciation lexicon (A4.5: azure -> SSML phoneme, others ->
           respell substitution; provider read from the current voice spec).
        """
        out = await process_outbound_text(
            text,
            session.active_language,
            translator,
            enabled=settings.voice_translate_enabled,
        )
        out = tts_normalize.normalize_for_tts(out)
        provider, _ = split_voice_spec(getattr(tts_impl, "voice", ""))
        out = pronunciation.apply_pronunciation(out, lexicon, provider=provider or "")
        return out

    # Calls on our naija-speech path (a chosen Nigerian language / mid-call switching) keep their own recognition and
    # voices; the W82 translation hook, STT hints and pre-TTS transforms serve every other call (phone, no naija).
    w82_hooks = switching_stt is None and not english_voice

    # Think in English, speak Yoruba/Hausa/Igbo (NAIJA_THINK_IN_ENGLISH): each English sentence group is translated
    # just before the Nigerian voice says it. Lines already in the language (the greeting and recording notice) pass
    # through. What was said, and the English it came from, are kept for the transcript.
    spoken_pairs: list = []
    session.spoken_pairs = spoken_pairs  # read by _record_voice_transcript
    speak_translator = (LLMTranslator(settings.llm_base_url, settings.llm_model, api_key=settings.llm_api_key,
                                      extra_body=settings.llm_extra_body or None)
                        if settings.naija_think_in_english else None)

    async def _speak_in_caller_language(text: str) -> str:
        # A model token leaked into speech once, between the text before and after a tool call ("...twelve noon.
        # lvisertguard I have found a slot"): a lowercase non-word opening a sentence, followed by a capital, is dropped.
        text = _LEAKED_TOKEN.sub("", text)
        text = _LANGUAGE_NAMES_HEARD.sub(lambda m: _LANGUAGE_NAME_FIX[m.group(0).lower()], text)
        text = fix_continue_in(text, session.active_language)
        try:
            text = fix_weekday_dates(text, getattr(ctx, "timezone", None))
        except Exception as exc:  # noqa: BLE001 - never block speech on this
            log.warning("weekday/date check skipped", error=str(exc)[:160])
        lang = session.active_language
        if speak_translator is None or lang not in call_language.SPOKEN_BY_TRANSLATION or not text.strip():
            return text
        if detect_language(text) != "en":
            return text
        # Numbers stay digits into translation: the model says times, dates and amounts in the language, and phone
        # numbers are then spoken digit by digit with the language's own number words. (English number words
        # inside a Yoruba/Hausa/Igbo sentence were where the voices mumbled.)
        out = native_digits((await speak_translator.translate(digit_words_to_digits(text), "en", lang)).text, lang)
        if out != text:
            spoken_pairs.append((text, out))
            session.last_agent_en = text  # the caller's next words are read against this (inbound translation)
        return out

    # What a Yoruba/Hausa/Igbo caller said, as the receptionist should understand it. The recogniser writes those
    # languages phonetically (no tone marks, words split or run together): read raw, the receptionist answered
    # questions nobody asked. It now gets the English meaning, worked out with the call's context; the caller's own
    # words are kept for the transcript (session.caller_heard: English -> what was heard).
    session.caller_heard = {}

    async def _understand_caller(text: str, detected: str) -> str:
        lang = detected if detected in ("yo", "ha", "ig") else None
        if lang is None and (detected or "") in ("", "und") and session.active_language in ("yo", "ha", "ig"):
            lang = session.active_language
        if speak_translator is None or lang is None or not text.strip():
            return text
        inbound = await process_inbound_text(text, translator, enabled=settings.voice_translate_enabled,
                                             path="voice", language_hint=lang)
        english = (inbound.text or "").strip()
        if not english or english == text:
            return text
        session.caller_heard[english] = text
        log.info("caller understood", language=lang, heard=text[:160], english=english[:160])
        return english

    agent = VoicePipelineAgent(
        vad=_load_vad(settings),
        stt=(WhisperSTTNode(switching_stt, on_transcript=_understand_caller) if switching_stt is not None
             else WhisperSTTNode(stt_impl, on_language=_on_language,
                                 on_transcript=_on_transcript if w82_hooks else None,
                                 hints=stt_hints,
                                 current_language=lambda: normalize_language(ml_state.active_language) or "en")),
        # NOTE (VOICE-SCALING §3): the livekit-plugins-openai LLM node cannot
        # hot-swap endpoints mid-process, so the LLM fallback chain
        # (app/pipeline/llm.py FallbackLLM) covers the chat/tool-loop paths;
        # the worker node relies on the primary endpoint + job-level retry.
        # Own client: the plugin's default read timeout is 5 s, which cut off every reply from a CPU-hosted model
        # (Ollama logged "cancel task" while still reading the prompt). LLM_TIMEOUT governs it instead.
        llm=_worker_llm(settings),
        tts=_naija_tts(PiperTTSNode(tts_impl, chunked=settings.tts_chunked,
                                    on_text=_speak_in_caller_language if speak_translator is not None else None))
        if (switching_stt is not None or english_voice)
        else PiperTTSNode(tts_impl, chunked=settings.tts_chunked, on_text=_prepare_tts_text if w82_hooks else None),
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
    agent.agora_call_language = chosen  # read by the greeting in entrypoint

    def _remember(role: str):
        def handler(msg: Any, *_a: Any, **_k: Any) -> None:
            from types import SimpleNamespace

            fnc_ctx.recent_lines.append(SimpleNamespace(role=role, content=_message_plain(msg)))
            del fnc_ctx.recent_lines[:-12]
        return handler

    try:
        agent.on("user_speech_committed", _remember("user"))
        agent.on("agent_speech_committed", _remember("assistant"))

        def _drop_switch_note(*_a: Any, **_k: Any) -> None:
            # The agreement has been said: drop the note, or the model opens every later reply with it again.
            try:
                msgs = agent.chat_ctx.messages
                msgs[:] = [m for m in msgs if not _is_switch_note(m)]
            except Exception as exc:  # noqa: BLE001
                log.warning("switch note cleanup skipped", error=str(exc)[:160])

        agent.on("agent_speech_committed", _drop_switch_note)
    except Exception as exc:  # noqa: BLE001
        log.warning("recent-lines hooks unavailable", error=str(exc)[:200])
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



_TRANSCRIPTS: "TranscriptRecorder | None" = None


def _message_text(msg: Any) -> str:
    content = getattr(msg, "content", msg)
    if isinstance(content, list):
        content = " ".join(str(c) for c in content if isinstance(c, str))
    return speakable(str(content or ""))


def _record_voice_transcript(agent: Any, settings: Settings, tenant_ctx: Any, session: Any, conversation_id: str,
                             site_slug: str, *, channel: str, spoken_pairs: list | None = None) -> None:
    """Store the call's words for the business's Conversations page: what the caller said (STT, committed turns),
    what the receptionist said (greeting included) and the tools it used. Fire-and-forget; never affects the call."""
    global _TRANSCRIPTS
    if _TRANSCRIPTS is None:
        _TRANSCRIPTS = TranscriptRecorder(getattr(settings, "conversation_url", ""))
    rec = _TRANSCRIPTS
    if not rec.enabled:
        return
    key = f"voice:{conversation_id}"
    session.stored_id_lookup = lambda: rec.stored_id(key)

    def _send(turns: list[dict[str, Any]]) -> None:
        rec.record(key, turns, tenant_id=str(getattr(tenant_ctx, "tenant_id", "") or ""), site_slug=site_slug,
                   channel=channel, contact_phone=session.verified_phone or session.confirmed_phone or session.claimed_phone)

    def _said(role: str):
        return lambda msg, *_a, **_k: (lambda t: _send([{"role": role, "text": t}]) if t else None)(_message_text(msg))

    def _user_said(msg, *_a, **_k) -> None:
        # A Yoruba/Hausa/Igbo turn reached the receptionist as English (see _understand_caller): the Conversations
        # page shows what the caller said, with that English as its translation.
        text = _message_text(msg)
        heard = (getattr(session, "caller_heard", None) or {}).pop((text or "").strip(), None)
        if heard:
            _send([{"role": "user", "text": heard, "translation_en": text}])
            return
        _said("user")(msg)

    def _agent_said(msg, *_a, **_k) -> None:
        # Spoken through translation: store what the caller heard, with the English it came from as its translation.
        # Only the pieces of THIS reply: an interrupted reply never commits, and its pieces once piled into the next
        # bubble (one Igbo bubble held three replies).
        if spoken_pairs:
            letters = lambda x: re.sub(r"[^a-z]", "", x.lower())  # numbers became words in the spoken copy
            said = letters(_message_plain(msg))
            mine = [(e, t) for e, t in spoken_pairs if letters(e)[:24] and letters(e)[:24] in said] or \
                list(spoken_pairs[-1:])
            spoken_pairs.clear()
            english = " ".join(e for e, _ in mine)
            heard = " ".join(t for _, t in mine)
            _send([{"role": "agent", "text": speakable(heard), "translation_en": speakable(english)}])
            return
        _said("agent")(msg)

    def _agent_interrupted(msg, *_a, **_k) -> None:
        text = _message_text(msg)
        if spoken_pairs:
            spoken_pairs.clear()  # the translated pieces of a cut-off reply must not pile into the next bubble
        if text and text.strip():
            _send([{"role": "agent", "text": text.rstrip() + " …"}])

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
        agent.on("user_speech_committed", _user_said)
        agent.on("agent_speech_committed", _agent_said)
        # livekit-agents 0.10 never commits an interrupted reply, so on phone calls (people talk over each other)
        # whole receptionist turns were missing from Conversations (2026-11-22): store what was said, marked "…".
        agent.on("agent_speech_interrupted", _agent_interrupted)
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


async def _caller_visitor(ctx: agents.JobContext, timeout_s: float = 8.0) -> "visitor_mod.Visitor | None":
    """The web caller's pre-call form (token metadata, app/visitor.py); None when they filled none."""
    try:
        participant = await asyncio.wait_for(ctx.wait_for_participant(), timeout=timeout_s)
        return visitor_mod.from_metadata(getattr(participant, "metadata", ""))
    except Exception as exc:  # noqa: BLE001 - no form = the receptionist asks as before
        log.info("caller visitor details not read", error=str(exc)[:120])
        return None


async def _caller_language(ctx: agents.JobContext, timeout_s: float = 8.0) -> str:
    """The language the web caller chose (token metadata, app/call_language.py); "" = English / not chosen."""
    try:
        participant = await asyncio.wait_for(ctx.wait_for_participant(), timeout=timeout_s)
        return call_language.from_metadata(getattr(participant, "metadata", ""))
    except Exception as exc:  # noqa: BLE001 - no choice = the default (English) call
        log.info("caller language not read", error=str(exc)[:120])
        return ""


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
        # The caller's silence only counts once the receptionist has finished: a reply here takes ~7 s to start and
        # ~10 s to speak, and lookups longer, so counting from the caller's last word made the receptionist ask
        # "can you check your microphone?" over its own answer and hang up on working callers.
        for event in ("agent_started_speaking", "agent_stopped_speaking", "function_calls_collected",
                      "function_calls_finished"):
            agent.on(event, monitor.note_user_speech)
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
    direction: str | None = None
    participants = list(getattr(ctx.room, "remote_participants", {}).values())
    # 2026-11-22: an outbound room joined before its callee (the browser demo phone joins on Answer): wait for them, so
    # the call brief is there before the opening line.
    if room_name.startswith(outbound.OUTBOUND_ROOM_PREFIX) and not participants:
        try:
            await asyncio.wait_for(ctx.wait_for_participant(), timeout=15)
        except Exception:  # noqa: BLE001 - no callee: fall through to a general outbound call
            pass
        participants = list(getattr(ctx.room, "remote_participants", {}).values())
    # SPEC-W101 C4: OUTBOUND call — the platform dialed the customer (see
    # app/outbound.py build_outbound_call_context; R3).
    out_ctx = outbound.build_outbound_call_context(room_name, participants)
    if out_ctx is not None:
        sip_call = out_ctx
        site_slug = out_ctx.site_slug
        direction = "outbound"
    elif sip.is_sip_room(room_name) or any(sip.is_sip_participant(p) for p in participants):
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
        direction=direction or "inbound",
    )

    # SPEC-W86: PSTN calls get a DTMF keypad-capture buffer (OTP entry via
    # the phone keypad); web/non-SIP rooms get None — read_keypad_entry
    # honestly answers keypad_unavailable there. The entrypoint creates
    # the buffer so it can register the room data-received listener below
    # (build_voice_agent keeps its 3-tuple return contract).
    dtmf_buffer = dtmf.DtmfBuffer() if sip_call is not None else None
    try:
        agent, tenant_ctx, session = await build_voice_agent(
            settings,
            dapr,
            site_slug,
            conversation_id,
            sip_call=sip_call,
            caller_language=await _caller_language(ctx) if sip_call is None else "",
            dtmf_buffer=dtmf_buffer,
            visitor=await _caller_visitor(ctx) if sip_call is None else None,
        )
    except DaprError as exc:
        log.error("session bootstrap failed", site_slug=site_slug, error=str(exc))
        await dapr.aclose()
        return
    if dtmf_buffer is not None:
        dtmf.attach_dtmf_listener(ctx.room, dtmf_buffer)
        # SPEC-W96 IVR-A: arm the menu interception (PSTN + enabled only;
        # session.ivr_menu is None otherwise). Menu digits are acked +
        # injected as intent hints; OTP capture (wait_for_entry) always
        # wins — the buffer never steals digits mid-wait.
        if session.ivr_menu:
            ivr_menu.attach_ivr_menu(dtmf_buffer, session.ivr_menu, agent)

    # Per-session call-quality accumulator: activated BEFORE agent.start so
    # the pipeline tasks it spawns inherit this context (asyncio tasks copy
    # the contextvars at creation). The instrumented STT/TTS/LLM/tool call
    # sites feed it; the SessionEnded event below ships its payload.
    session_metrics = metrics.activate_session(metrics.SessionMetrics(conversation_id))
    _record_voice_transcript(agent, settings, tenant_ctx, session, conversation_id, site_slug,
                             channel="phone" if sip_call is not None else "voice",
                             spoken_pairs=getattr(session, "spoken_pairs", None))
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
        direction=direction,
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
    # SPEC-W96 IVR-A: the keypad menu line is appended LAST (after any
    # disclosure lines) so the shortcuts are the caller's last heard
    # instruction; None when the menu is disabled or the call is non-SIP.
    menu_line = ivr_menu.menu_line(session.ivr_menu) if session.ivr_menu else None
    # 2026-11-21: name, email and phone are taken at the start of the call unless the caller's form gave them.
    ask_first_known = visitor_mod.from_session(session) is not None
    # 2026-11-22: a call WE placed opens with who is calling and an identity check, never "thank you for calling".
    opening = session.outbound_greeting or call_language.greeting(
        getattr(agent, "agora_call_language", ""), getattr(tenant_ctx, "display_name", ""),
        build_greeting(tenant_ctx, menu_line=menu_line, ask_details=not ask_first_known),
        ask_details=not ask_first_known)
    await agent.say(opening, allow_interruptions=True)
    # SPEC-W71 P6: warm the TTS LRU mid-call with the tool-ack/filler phrases
    # (derived from this tenant's ToolAckPolicy) + standard closers. Scheduled
    # AFTER session start and the greeting — fire-and-forget; failures log.
    schedule_tts_warmup(agent, ToolAckPolicy.from_context(tenant_ctx))

    # SPEC-W73 A1: dead-mic watchdog, started AFTER the greeting is queued
    # (helper above; None when disabled). Cancelled on room disconnect below.
    dead_mic_monitor = start_dead_mic_monitor(settings, agent, session, ctx.room)

    ended = {"done": False}

    # 2026-11-23: an outbound callee hanging up never fired the room's "disconnected" (the job simply exits), so no
    # SessionEnded went out and the call never reached the CRM or the dashboard. End the session once, on whichever
    # comes first: our own disconnect, or the last caller leaving the room.
    @ctx.room.on("participant_disconnected")
    def _on_caller_left(*args) -> None:  # noqa: ARG001
        if not getattr(ctx.room, "remote_participants", {}):
            _on_disconnected()

    @ctx.room.on("disconnected")
    def _on_disconnected(*args) -> None:  # noqa: ARG001
        import asyncio

        if ended["done"]:
            return
        ended["done"] = True
        if dead_mic_monitor is not None:
            dead_mic_monitor.cancel()  # no orphan watchdog past session end
        metrics.get_registry().active_sessions.dec()
        # Attach the call-quality payload (None when the session recorded no
        # signals -> the event carries no `quality` key at all).
        quality = session_metrics.quality_payload(
            escalated=session.escalation_room is not None,
            confirmed_phone=session.confirmed_phone,
        )
        # 2026-11-23: who we called and why ride inside `quality` (conversation-service copies that object into
        # CallQualityEnriched as is), so the web app and the CRM can both say "Outbound call — confirm appointment".
        if quality is not None:
            quality["direction"] = direction or "inbound"
            if getattr(session, "outbound_purpose", None):
                quality["purpose"] = session.outbound_purpose
            if session.caller_name:
                quality["contact_name"] = session.caller_name
            quality["channel"] = "phone" if sip_call is not None else "web"
        asyncio.get_event_loop().create_task(
            _publish_lifecycle(
                dapr,
                settings,
                tenant_ctx,
                "com.opendesk.conversation.SessionEnded",
                conversation_id,
                quality=quality,
                agent_id=session.resolved_agent_id,
                stored_conversation_id=_TRANSCRIPTS.stored_id(f"voice:{conversation_id}") if _TRANSCRIPTS else None,
                direction=direction,
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
        # A fresh pod downloads the whisper model (~480 MB for "small") inside the first process' prewarm; the 10 s
        # default killed every process mid-download ("initialization timed out") and no call could start.
        "initialize_process_timeout": float(os.environ.get("VOICE_INIT_PROCESS_TIMEOUT_S", "180")),
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
