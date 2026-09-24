"""TTS stage: interface + Piper implementation.

Two modes (env PIPER_MODE):
- `http` (default): POST {PIPER_HTTP_URL}/speak {"text", "voice"} expecting
  audio/wav back. The companion sidecar in ./sidecar implements exactly this
  contract (see docker-compose.fragment.yml).
- `subprocess`: runs the local `piper` binary (env PIPER_BIN) with the voice
  model from PIPER_MODEL_DIR (env PIPER_VOICE, e.g. en_US-lessac-medium);
  see README for model download.

Both return signed-16-bit mono PCM at `sample_rate` (default 22050 Hz).
"""

from __future__ import annotations

import asyncio
import io
import os
import re
import tempfile
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
        """Sentence-level streaming synthesis (SPEC-W46 P4).

        Full-utterance cache hits ship the whole buffer immediately;
        otherwise each sentence chunk is synthesized (and cached) in turn,
        so the first audio frame is available after the FIRST chunk's
        synthesis instead of the full utterance. Chunk PCM is concatenated
        into the full-utterance cache entry for the next repeat.
        """
        text = text.strip()
        if not text:
            return
        cache_key = (self.voice, text, "pcm")
        cached = self._cache.get(cache_key)
        if cached is not None:
            metrics.session_tts()
            yield cached
            return
        chunks = split_sentences(text)
        if len(chunks) <= 1:
            pcm = await self.synthesize_pcm(text)
            if pcm:
                yield pcm
            return
        parts: list[bytes] = []
        for chunk in chunks:
            pcm = await self.synthesize_pcm(chunk)
            if pcm:
                parts.append(pcm)
                yield pcm
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
        return _wav_to_pcm(await self._subprocess_wav(text, self.voice))

    async def _subprocess_wav(self, text: str, voice: str) -> bytes:
        model = os.path.join(self.model_dir, f"{voice}.onnx")
        config = os.path.join(self.model_dir, f"{voice}.onnx.json")

        # Run the blocking subprocess in a thread to keep the event loop free.
        def _exec() -> bytes:
            import subprocess

            with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as tmp:
                out_path = tmp.name
            try:
                cmd = [
                    self.piper_bin,
                    "--model",
                    model,
                    "--config",
                    config,
                    "--output_file",
                    out_path,
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
                with open(out_path, "rb") as fh:
                    return fh.read()
            finally:
                try:
                    os.unlink(out_path)
                except OSError:
                    pass

        return await asyncio.to_thread(_exec)
