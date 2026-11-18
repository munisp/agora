"""STT stage: interface + in-process faster-whisper implementation.

The default model is lazy-loaded on first transcription (env WHISPER_MODEL,
default `base`); see README for pre-download / offline notes.

SPEC-W70 A2 (decode quality + multilingual routing):
- ``condition_on_previous_text=False`` is hard-set on every transcribe call:
  whisper's default (True) feeds the previous decode back as a prompt, which
  causes hallucination loops / repetition on noisy Nigerian telephony audio
  and slows the decode.
- ``WHISPER_MODEL_MULTILINGUAL`` (default empty = off) names an optional
  second model (e.g. ``large-v3-turbo``) used ONLY when the effective
  language (forced ``language=`` or the last auto-detection) is non-English;
  English stays on the fast default model. Both models stay resident in a
  per-model-size cache with per-model load locks. Routing is the pure
  function :func:`resolve_stt_model`.
"""

from __future__ import annotations

import asyncio
import threading
from typing import Any, Protocol

import numpy as np

from .. import metrics
from ..logging import get_logger
from ..multilang import normalize_language, pidgin_proxy

log = get_logger("stt")


class STTInterface(Protocol):
    async def transcribe_pcm(
        self,
        pcm_s16le: bytes,
        *,
        sample_rate: int,
        channels: int = 1,
        language: str | None = None,
        initial_prompt: str | None = None,
    ) -> str:
        """Transcribe signed-16-bit little-endian PCM audio to text."""
        ...


def resolve_stt_model(
    forced_language: str | None,
    last_detected: str | None,
    default_model: str,
    multilingual_model: str | None,
) -> str:
    """Pick the whisper model_size for a transcription (SPEC-W70 A2).

    Pure function. The multilingual model is returned ONLY when it is set
    AND the effective language — forced ``language=`` first, else the last
    auto-detection — is non-English. Nigerian Pidgin (pcm) proxies to
    English (no whisper pcm model exists), so it stays on the fast default
    model; an unknown/empty effective language also stays on the default.
    """
    multilingual = (multilingual_model or "").strip()
    if not multilingual:
        return default_model
    effective = pidgin_proxy(
        normalize_language(forced_language) or normalize_language(last_detected)
    )
    if effective and effective != "en":
        return multilingual
    return default_model


class FasterWhisperSTT:
    """faster-whisper (CTranslate2) STT, loaded in-process on first use.

    SPEC-W70 A2: models are cached per model_size in ``_models`` (with a
    per-size asyncio lock for the async path and a threading guard for lock
    creation), so the default model and the optional multilingual model can
    both stay resident. ``multilingual_model`` (WHISPER_MODEL_MULTILINGUAL,
    empty = off) is lazy-loaded on the first non-English transcription and
    preloaded by ``preload_sync`` only when the knob is set.
    """

    def __init__(
        self,
        model_size: str = "base",
        device: str = "auto",
        compute_type: str = "int8",
        multilingual_model: str = "",
        # SPEC-W71 P3: CTranslate2 thread tuning (WHISPER_CPU_THREADS /
        # WHISPER_NUM_WORKERS), passed to every WhisperModel construction.
        # cpu_threads=0 keeps the CTranslate2 auto default.
        cpu_threads: int = 0,
        num_workers: int = 1,
    ) -> None:
        self._model_size = model_size
        self._device = device
        self._compute_type = compute_type
        self._multilingual_model = (multilingual_model or "").strip()
        self._cpu_threads = cpu_threads
        self._num_workers = num_workers
        # Per-model-size cache + locks (SPEC-W70 A2).
        self._models: dict[str, Any] = {}
        self._locks: dict[str, asyncio.Lock] = {}
        self._locks_guard = threading.Lock()
        # Wave 5 #3: language whisper auto-detected on the last transcription
        # (None when the caller forced `language=` or nothing ran yet); the
        # LiveKit STT bridge forwards it to the per-session MultilangState.
        self.last_detected_language: str | None = None

    # Backward-compatible view of the DEFAULT model slot (tests reach for
    # `._model` directly to inject a fake model).
    @property
    def _model(self):
        return self._models.get(self._model_size)

    @_model.setter
    def _model(self, value) -> None:
        if value is None:
            self._models.pop(self._model_size, None)
        else:
            self._models[self._model_size] = value

    def _lock_for(self, model_size: str) -> asyncio.Lock:
        with self._locks_guard:
            return self._locks.setdefault(model_size, asyncio.Lock())

    def _load_sync(self, model_size: str):
        # Import deferred: ctranslate2 is heavy and optional at import time.
        from faster_whisper import WhisperModel

        device = self._device
        if device == "auto":
            try:
                import ctranslate2

                device = "cuda" if ctranslate2.get_cuda_device_count() > 0 else "cpu"
            except Exception:  # noqa: BLE001
                device = "cpu"
        log.info(
            "loading whisper model",
            model=model_size,
            device=device,
            compute_type=self._compute_type,
        )
        # SPEC-W71 P3: cpu_threads (0 = CTranslate2 auto) and num_workers are
        # real faster-whisper WhisperModel ctor kwargs (CTranslate2 thread
        # tuning), applied to the default AND the multilingual model alike.
        return WhisperModel(
            model_size,
            device=device,
            compute_type=self._compute_type,
            cpu_threads=self._cpu_threads,
            num_workers=self._num_workers,
        )

    async def _ensure_model(self, model_size: str | None = None):
        size = model_size or self._model_size
        model = self._models.get(size)
        if model is not None:
            return model
        async with self._lock_for(size):
            model = self._models.get(size)
            if model is None:
                model = await asyncio.to_thread(self._load_sync, size)
                self._models[size] = model
        return model

    def preload_sync(self) -> None:
        """Eagerly load the model (worker prewarm hook runs synchronously).

        SPEC-W70 A2: also preloads the multilingual model, but ONLY when
        WHISPER_MODEL_MULTILINGUAL is set (and prewarm only calls this when
        PRELOAD_MODELS is on)."""
        if self._models.get(self._model_size) is None:
            self._models[self._model_size] = self._load_sync(self._model_size)
        if self._multilingual_model and self._models.get(self._multilingual_model) is None:
            log.info(
                "preloading multilingual whisper model",
                model=self._multilingual_model,
            )
            self._models[self._multilingual_model] = self._load_sync(
                self._multilingual_model
            )

    async def preload(self) -> None:
        """Async variant of preload_sync (control-plane boot, tests)."""
        await self._ensure_model()

    @staticmethod
    def _pcm_to_float32(
        pcm_s16le: bytes, sample_rate: int, channels: int
    ) -> np.ndarray:
        audio = (
            np.frombuffer(pcm_s16le, dtype=np.int16).astype(np.float32) / 32768.0
        )
        if channels > 1:
            audio = audio.reshape(-1, channels).mean(axis=1)
        if sample_rate != 16000:
            # Linear resample to whisper's 16 kHz (dev-grade; a polyphase
            # resampler would be nicer but adds a dependency).
            ratio = 16000 / sample_rate
            x_old = np.arange(len(audio))
            x_new = np.arange(0, len(audio) - 1, 1 / ratio)
            audio = np.interp(x_new, x_old, audio).astype(np.float32)
        return audio

    async def transcribe_pcm(
        self,
        pcm_s16le: bytes,
        *,
        sample_rate: int,
        channels: int = 1,
        language: str | None = None,
        # SPEC-W82 A4.4: per-tenant STT hint — a whisper initial_prompt
        # seeded with tenant/offering/staff names biases the decode towards
        # the right spellings on accented telephony audio. None (the
        # default) preserves the pre-W82 call EXACTLY (the kwarg is only
        # forwarded when set, so fakes without the parameter still work).
        initial_prompt: str | None = None,
    ) -> str:
        # SPEC-W70 A2: route to the multilingual model (when configured) for
        # non-English turns; English stays on the fast default model.
        model_size = resolve_stt_model(
            language,
            self.last_detected_language,
            self._model_size,
            self._multilingual_model,
        )
        model = await self._ensure_model(model_size)
        audio = self._pcm_to_float32(pcm_s16le, sample_rate, channels)
        if len(audio) < 1600:  # < 100 ms
            return ""

        def _run() -> str:
            # SPEC-W82 A4.4: only forward initial_prompt when actually set —
            # keeps the pre-W82 transcribe() call byte-identical (and test
            # fakes that lack the parameter working) when no hint exists.
            extra_kwargs = (
                {"initial_prompt": initial_prompt} if initial_prompt else {}
            )
            segments, info = model.transcribe(
                audio,
                language=language,
                beam_size=1,
                **extra_kwargs,
                vad_filter=False,  # silero VAD already gates the audio upstream
                # SPEC-W70 A2: hard-set False — the whisper default (True)
                # feeds the previous decode back as a prompt, which causes
                # hallucination loops / repetition on noisy Naija audio and
                # slows the decode. Behavior fix, deliberately NOT a knob.
                condition_on_previous_text=False,
                # SPEC-W71 P2: decode fast-path. temperature=0.0 disables
                # faster-whisper's multi-temperature fallback re-decode
                # (each fallback is a FULL extra decode pass on
                # compression-ratio/logprob failures); greedy single-pass is
                # the right default for gated telephony turns.
                temperature=0.0,
                # SPEC-W71 P2: segment timestamps are unused (the upstream
                # silero VAD already gates speech) — skipping them avoids
                # timestamp-token constraint decoding on every segment.
                without_timestamps=True,
            )
            # Only trust auto-detection; a forced language= is not a detection.
            if language is None:
                from ..multilang import detect_language_from_info

                self.last_detected_language = detect_language_from_info(info) or None
            return " ".join(seg.text.strip() for seg in segments).strip()

        try:
            with metrics.get_registry().stt_latency.time():
                return await asyncio.to_thread(_run)
        finally:
            metrics.session_stt()  # per-session quality accumulator
