"""TTS stage: interface + Piper implementation.

Two modes (env PIPER_MODE):
- `http` (default): POST {PIPER_HTTP_URL}/speak {"text", "voice"} expecting
  audio/wav back. The companion sidecar in ./sidecar implements exactly this
  contract (see docker-compose.fragment.yml).
- `subprocess`: runs the local `piper` binary (env PIPER_BIN) with the voice
  model from PIPER_MODEL_DIR (env PIPER_VOICE, e.g. en_US-lessac-medium);
  see README for model download. SPEC-W71 P5: piper is invoked with
  `--output-raw` (raw s16le mono on stdout at the voice's configured
  `audio.sample_rate`, read from the voice `.onnx.json` and cached per
  voice) — no temp file on the hot path.

Both return signed-16-bit mono PCM at `sample_rate` (default 22050 Hz).
"""

from __future__ import annotations

import asyncio
import io
import os
import re
import time
import wave
from collections import OrderedDict
from typing import AsyncIterator, Protocol

import httpx

from .. import metrics
from ..logging import get_logger

log = get_logger("tts")


class TtsLruCache:
    """Bounded per-process LRU for synthesized audio (SPEC-W46 P4).

    Keyed ``(voice, text, format)`` by the callers; ``maxsize <= 0``
    disables caching. Repeated utterances (greetings, read-backs, menu
    lines) are served from memory instead of re-synthesized every turn.
    """

    def __init__(self, maxsize: int = 256) -> None:
        self.maxsize = maxsize
        self._items: "OrderedDict[tuple[str, str, str], bytes]" = OrderedDict()

    def get(self, key: tuple[str, str, str]) -> bytes | None:
        if self.maxsize <= 0:
            return None
        try:
            value = self._items.pop(key)
        except KeyError:
            return None
        self._items[key] = value  # most-recently-used
        return value

    def set(self, key: tuple[str, str, str], value: bytes) -> None:
        if self.maxsize <= 0 or not value:
            return
        self._items[key] = value
        self._items.move_to_end(key)
        while len(self._items) > self.maxsize:
            self._items.popitem(last=False)

    def clear(self) -> None:
        self._items.clear()


# Sentence boundary for chunk-level streaming: a chunk ends at terminal
# punctuation (or a newline); the final fragment without punctuation is
# one chunk. Abbreviations are NOT special-cased — a false split only
# costs an extra synthesis call, never wrong audio ordering.
_SENTENCE_RE = re.compile(r"[^.!?…\n]+(?:[.!?…]+[\"')\]]*\s*|\n+|$)")


def split_sentences(text: str) -> list[str]:
    """Split `text` into speakable sentence-level chunks (order preserved)."""
    chunks = [m.group(0).strip() for m in _SENTENCE_RE.finditer(text.strip())]
    return [c for c in chunks if c]


class TTSInterface(Protocol):
    sample_rate: int

    async def synthesize_pcm(self, text: str) -> bytes:
        """Synthesize text to signed-16-bit mono PCM at `sample_rate`."""
        ...


def _wav_to_pcm(wav_bytes: bytes) -> tuple[bytes, int]:
    with wave.open(io.BytesIO(wav_bytes), "rb") as wf:
        channels = wf.getnchannels()
        sampwidth = wf.getsampwidth()
        rate = wf.getframerate()
        frames = wf.readframes(wf.getnframes())
    if sampwidth != 2:
        raise RuntimeError(f"unsupported piper wav sample width: {sampwidth}")
    if channels != 1:
        # Downmix interleaved channels by simple averaging.
        import numpy as np

        audio = np.frombuffer(frames, dtype=np.int16)
        audio = audio.reshape(-1, channels).mean(axis=1).astype(np.int16)
        frames = audio.tobytes()
    return frames, rate


class PiperTTS:
    def __init__(
        self,
        *,
        mode: str = "http",
        http_url: str = "http://piper:5500",
        voice: str = "en_US-lessac-medium",
        piper_bin: str = "piper",
        model_dir: str = "/voices",
        sample_rate: int = 22050,
        timeout_s: float = 30.0,
        cache_size: int = 256,
    ) -> None:
        self.mode = mode
        self.http_url = http_url.rstrip("/")
        self.voice = voice
        self.piper_bin = piper_bin
        self.model_dir = model_dir
        self.sample_rate = sample_rate
        self._timeout = timeout_s
        self._client: httpx.AsyncClient | None = None
        # SPEC-W46 P4: LRU keyed (voice, text, format) — repeated phrases
        # (greetings, read-backs) skip synthesis entirely.
        self._cache = TtsLruCache(cache_size)
        # SPEC-W71 P5: per-voice sample-rate cache (from the voice
        # .onnx.json config) for the raw-stdout subprocess path.
        self._voice_rates: dict[str, int] = {}

    async def aclose(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    def _http(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = httpx.AsyncClient(timeout=httpx.Timeout(self._timeout))
        return self._client

    async def synthesize_pcm(self, text: str) -> bytes:
        text = text.strip()
        if not text:
            return b""
        cache_key = (self.voice, text, "pcm")
        cached = self._cache.get(cache_key)
        if cached is not None:
            metrics.session_tts()  # cache hit still counts as a served utterance
            return cached
        try:
            with metrics.get_registry().tts_latency.time():
                if self.mode == "subprocess":
                    pcm, rate = await self._synthesize_subprocess(text)
                else:
                    pcm, rate = await self._synthesize_http(text)
        finally:
            metrics.session_tts()  # per-session quality accumulator
        if rate != self.sample_rate:
            log.warning(
                "piper sample rate mismatch; audio may be pitched",
                expected=self.sample_rate,
                got=rate,
            )
        self._cache.set(cache_key, pcm)
        return pcm

    async def stream_pcm(self, text: str) -> AsyncIterator[bytes]:
        """Sentence-level streaming synthesis (SPEC-W46 P4, SPEC-W71 P4/P9).

        Full-utterance cache hits ship the whole buffer immediately;
        otherwise each sentence chunk is synthesized (and cached) in turn,
        so the first audio frame is available after the FIRST chunk's
        synthesis instead of the full utterance. Chunk PCM is concatenated
        into the full-utterance cache entry for the next repeat.

        SPEC-W71 P4 (depth-1 prefetch): while chunk N is awaited/yielded,
        chunk N+1's synthesis is already in flight as an asyncio task —
        multi-sentence latency drops from sum(chunk times) toward
        ~max(chunk times) + first chunk. Preserved semantics: chunks still
        populate the full-utterance cache entry in ORIGINAL order (parts
        are appended in iteration order, never completion order),
        empty-chunk skipping is unchanged, and a prefetch failure raises at
        exactly the iteration where the sequential path would have raised
        it. An early exit (generator close) cancels the pending prefetch so
        no task leaks.

        SPEC-W71 P9: time-to-first-yield is observed on
        ``voice_tts_first_chunk_seconds`` (every path, cache hits included).
        """
        text = text.strip()
        if not text:
            return
        started = time.perf_counter()
        first_yield = True

        def _mark_first_chunk() -> None:
            nonlocal first_yield
            if first_yield:
                first_yield = False
                metrics.get_registry().tts_first_chunk.observe(
                    time.perf_counter() - started
                )

        cache_key = (self.voice, text, "pcm")
        cached = self._cache.get(cache_key)
        if cached is not None:
            metrics.session_tts()
            _mark_first_chunk()
            yield cached
            return
        chunks = split_sentences(text)
        if len(chunks) <= 1:
            pcm = await self.synthesize_pcm(text)
            if pcm:
                _mark_first_chunk()
                yield pcm
            return
        parts: list[bytes] = []
        next_task: asyncio.Task | None = asyncio.ensure_future(
            self.synthesize_pcm(chunks[0])
        )
        try:
            for i in range(len(chunks)):
                task, next_task = next_task, None
                if i + 1 < len(chunks):
                    # Prefetch depth 1: chunk i+1 synthesizes while chunk i
                    # is awaited and yielded.
                    next_task = asyncio.ensure_future(
                        self.synthesize_pcm(chunks[i + 1])
                    )
                pcm = await task  # this chunk's error surfaces at ITS index
                if pcm:
                    parts.append(pcm)
                    _mark_first_chunk()
                    yield pcm
        finally:
            if next_task is not None:
                # Early exit (consumer closed the generator): never leak the
                # prefetch task; a completed-but-unconsumed prefetch error is
                # retrieved so asyncio does not log "never retrieved".
                if not next_task.done():
                    next_task.cancel()
                elif not next_task.cancelled():
                    next_task.exception()
        if parts:
            self._cache.set(cache_key, b"".join(parts))

    async def synthesize_wav(self, text: str, voice: str | None = None) -> bytes:
        """RIFF wav bytes for `text` (defaults to the configured voice).

        SPEC-W10: consumed by the tts_providers chain's piper adapter. Same
        fetch paths as synthesize_pcm, without the PCM conversion.
        """
        text = text.strip()
        if not text:
            return b""
        voice = (voice or "").strip() or self.voice
        cache_key = (voice, text, "wav")
        cached = self._cache.get(cache_key)
        if cached is not None:
            return cached
        if self.mode == "subprocess":
            wav = await self._subprocess_wav(text, voice)
        else:
            wav = await self._http_wav(text, voice)
        self._cache.set(cache_key, wav)
        return wav

    async def _synthesize_http(self, text: str) -> tuple[bytes, int]:
        return _wav_to_pcm(await self._http_wav(text, self.voice))

    async def _http_wav(self, text: str, voice: str) -> bytes:
        resp = await self._http().post(
            f"{self.http_url}/speak", json={"text": text, "voice": voice}
        )
        resp.raise_for_status()
        return resp.content

    async def _synthesize_subprocess(self, text: str) -> tuple[bytes, int]:
        return await self._subprocess_pcm(text, self.voice)

    def _voice_sample_rate(self, voice: str) -> int:
        """Sample rate for `voice` from its piper ``.onnx.json`` config
        (``audio.sample_rate``), cached per voice (SPEC-W71 P5).

        The raw-stdout path carries no WAV header, so the rate MUST come
        from the voice config. Falls back to ``self.sample_rate`` (with a
        warning) when the config is missing or unparseable."""
        cached = self._voice_rates.get(voice)
        if cached is not None:
            return cached
        rate = 0
        config_path = os.path.join(self.model_dir, f"{voice}.onnx.json")
        try:
            import json

            with open(config_path, "r", encoding="utf-8") as fh:
                cfg = json.load(fh)
            rate = int((cfg.get("audio") or {}).get("sample_rate") or 0)
        except Exception as exc:  # noqa: BLE001 - degrade to default rate
            log.warning(
                "piper voice config unreadable; using default sample rate",
                voice=voice,
                error=str(exc)[:200],
            )
        if rate <= 0:
            rate = self.sample_rate
        self._voice_rates[voice] = rate
        return rate

    async def _subprocess_pcm(self, text: str, voice: str) -> tuple[bytes, int]:
        """SPEC-W71 P5: piper synthesis straight from stdout — no temp file
        on the hot path.

        Flag verified against the piper-tts 1.8.0 wheel
        (``piper/__main__.py`` argparse): ``--output-raw`` (alias
        ``--output_raw``) streams raw signed-16-bit little-endian mono audio
        to stdout at the voice's configured sample rate, and piper reads the
        text from stdin when no ``--input-file`` is given (same as the
        pre-W71 temp-file path). (Code-level fallback note: piper also
        accepts ``--output_file -`` for WAV-to-stdout; the raw path is the
        proven single path used here.)
        """
        model = os.path.join(self.model_dir, f"{voice}.onnx")
        config = os.path.join(self.model_dir, f"{voice}.onnx.json")
        rate = self._voice_sample_rate(voice)

        # Run the blocking subprocess in a thread to keep the event loop free.
        def _exec() -> bytes:
            import subprocess

            cmd = [
                self.piper_bin,
                "--model",
                model,
                "--config",
                config,
                "--output-raw",
            ]
            proc = subprocess.run(
                cmd,
                input=text.encode("utf-8"),
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                timeout=self._timeout,
                check=False,
            )
            if proc.returncode != 0:
                raise RuntimeError(
                    f"piper exited {proc.returncode}: {proc.stderr.decode(errors='replace')[:512]}"
                )
            return proc.stdout

        return await asyncio.to_thread(_exec), rate

    async def _subprocess_wav(self, text: str, voice: str) -> bytes:
        """RIFF wav bytes for `text` (consumed by the chain's piper adapter).

        SPEC-W71 P5: the WAV container is assembled in memory around the
        raw stdout PCM — no temp file is created on any path."""
        pcm, rate = await self._subprocess_pcm(text, voice)
        buf = io.BytesIO()
        with wave.open(buf, "wb") as wf:
            wf.setnchannels(1)
            wf.setsampwidth(2)
            wf.setframerate(rate)
            wf.writeframes(pcm)
        return buf.getvalue()
