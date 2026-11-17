"""SPEC-W48-C: streaming STT over a rolling window.

The batch path (``FasterWhisperSTT.transcribe_pcm`` /
``WhisperSTTNode`` with ``STTCapabilities(streaming=False,
interim_results=False)``) is untouched. This module adds the STREAMING
capability set for the new ``/v1/stt/stream`` WebSocket endpoint only:

- the client streams 16 kHz mono PCM16 frames;
- every ``hop_s`` of new audio the last ``window_s`` of audio is
  re-transcribed and an ``{type:"interim", text, ts}`` event is emitted;
- when trailing silence reaches ``silence_ms`` (energy-based endpointing)
  the accumulated segment is transcribed once and emitted as
  ``{type:"final", text, start_ts, end_ts}``;
- a trailing segment is flushed as a final when the stream ends.

The recognizer is the REAL ``STTInterface`` seam from
``app/pipeline/stt.py`` (production: ``FasterWhisperSTT``, lazy in-process
model). Tests inject a stub recognizer implementing the same interface —
no model download required.

Backpressure: undrained input is bounded at ``max_buffer_s`` seconds of
audio (default 8s); ``feed`` raises ``BackpressureError`` past the bound
and the endpoint closes the socket with code 1008 + reason.

SPEC-W73 (WS-C): the rolling-window parameters are env-tunable. They are
read from ``os.environ`` at ENGINE CONSTRUCTION (``StreamingSTTEngine
.__init__``), never at import time:

- ``STT_STREAM_WINDOW_S``      -> ``window_s``      (default 1.0)
- ``STT_STREAM_HOP_S``         -> ``hop_s``         (default 0.25)
- ``STT_STREAM_SILENCE_MS``    -> ``silence_ms``    (default 700)
- ``STT_STREAM_MAX_BUFFER_S``  -> ``max_buffer_s``  (default 8.0)

Precedence: explicit constructor kwargs > env > the defaults above
(byte-identical to the pre-W73 hardcoded behavior). Bounds are validated
with a clear ``ValueError``: ``window_s > 0``, ``hop_s > 0``,
``hop_s <= window_s``, ``silence_ms >= 0``, ``max_buffer_s > 0``.
``SILENCE_RMS_THRESHOLD`` and ``MAX_SEGMENT_S`` remain fixed constants.
"""

from __future__ import annotations

import asyncio
import os
from collections import deque
from typing import Any, AsyncIterator

import numpy as np

from ..logging import get_logger
from .stt import STTInterface

log = get_logger("stt-stream")

# Capability declaration for the streaming endpoint ONLY. The LiveKit
# batch bridge keeps STTCapabilities(streaming=False, interim_results=False).
STREAMING_STT_CAPABILITIES: dict[str, bool] = {
    "streaming": True,
    "interim_results": True,
}

#: RMS (normalized to int16 full scale) below which a hop counts as silence.
SILENCE_RMS_THRESHOLD = 0.01

#: Force-endpoint a segment that runs this long without a silence gap so a
#: pathological non-stop stream cannot grow the segment buffer unboundedly.
MAX_SEGMENT_S = 30.0

#: SPEC-W73 (WS-C) defaults for the env-tunable rolling-window parameters.
#: These are byte-identical to the pre-W73 hardcoded constructor defaults;
#: env overrides (read at engine construction, never at import) sit between
#: explicit constructor kwargs and these values.
DEFAULT_WINDOW_S = 1.0
DEFAULT_HOP_S = 0.25
DEFAULT_SILENCE_MS = 700
DEFAULT_MAX_BUFFER_S = 8.0

ENV_WINDOW_S = "STT_STREAM_WINDOW_S"
ENV_HOP_S = "STT_STREAM_HOP_S"
ENV_SILENCE_MS = "STT_STREAM_SILENCE_MS"
ENV_MAX_BUFFER_S = "STT_STREAM_MAX_BUFFER_S"


def _env_float(name: str, default: float) -> float:
    raw = os.environ.get(name)
    if raw is None or not raw.strip():
        return default
    try:
        return float(raw)
    except ValueError:
        raise ValueError(f"{name} must be a number, got {raw!r}") from None


def _env_int(name: str, default: int) -> int:
    raw = os.environ.get(name)
    if raw is None or not raw.strip():
        return default
    try:
        return int(raw)
    except ValueError:
        raise ValueError(f"{name} must be an integer, got {raw!r}") from None


class BackpressureError(RuntimeError):
    """Raised by feed() when undrained input exceeds the buffer bound."""


class StreamingSTTEngine:
    """Rolling-window chunked STT over a recognizer behind STTInterface.

    Single-event-loop design: ``feed`` (from the socket receiver) and
    ``process_pending`` (from the sender loop) run on the same loop, so no
    locks are needed. All timestamps are seconds of audio consumed since
    stream start.
    """

    def __init__(
        self,
        recognizer: STTInterface,
        *,
        sample_rate: int = 16000,
        window_s: float | None = None,
        hop_s: float | None = None,
        silence_ms: int | None = None,
        max_buffer_s: float | None = None,
    ) -> None:
        # SPEC-W73 (WS-C): None means "not explicitly passed" -> fall back
        # to the env var (read HERE, at construction, never at import) and
        # then to the module default. Explicit kwargs always win.
        if window_s is None:
            window_s = _env_float(ENV_WINDOW_S, DEFAULT_WINDOW_S)
        if hop_s is None:
            hop_s = _env_float(ENV_HOP_S, DEFAULT_HOP_S)
        if silence_ms is None:
            silence_ms = _env_int(ENV_SILENCE_MS, DEFAULT_SILENCE_MS)
        if max_buffer_s is None:
            max_buffer_s = _env_float(ENV_MAX_BUFFER_S, DEFAULT_MAX_BUFFER_S)

        if sample_rate <= 0:
            raise ValueError(f"sample_rate must be positive, got {sample_rate}")
        if window_s <= 0:
            raise ValueError(f"window_s must be > 0, got {window_s}")
        if hop_s <= 0:
            raise ValueError(f"hop_s must be > 0, got {hop_s}")
        if hop_s > window_s:
            raise ValueError(
                f"hop_s must be <= window_s, got hop_s={hop_s} > window_s={window_s}"
            )
        if silence_ms < 0:
            raise ValueError(f"silence_ms must be >= 0, got {silence_ms}")
        # W73 deviation (orchestrator-sanctioned): the buffer bound stays the
        # pre-W73 `> 0` check, NOT `>= hop_s` — a tighter-than-hop buffer is
        # a legitimate backpressure configuration (feed() overflows sooner)
        # and is exercised by the WS 1008 backpressure path.
        if max_buffer_s <= 0:
            raise ValueError(f"max_buffer_s must be > 0, got {max_buffer_s}")
        self._recognizer = recognizer
        self._sample_rate = sample_rate
        self._bytes_per_s = sample_rate * 2  # mono PCM16
        self._window_s = window_s
        self._hop_s = hop_s
        self._silence_ms = silence_ms
        self._max_buffer_s = max_buffer_s
        self._window_bytes = max(1, int(window_s * self._bytes_per_s))
        self._hop_bytes = max(2, int(hop_s * self._bytes_per_s))
        self._silence_s = max(0, silence_ms) / 1000.0
        self._max_buffer_bytes = max(2, int(max_buffer_s * self._bytes_per_s))

        self._pending: deque[bytes] = deque()  # fed, not yet consumed
        self._pending_bytes = 0
        self._hop_buf = bytearray()  # consumed, awaiting a full hop
        self._window = bytearray()  # rolling window (last window_s)
        self._segment = bytearray()  # current utterance since last final
        self._segment_start_s = 0.0
        self._consumed_s = 0.0
        self._trailing_silence_s = 0.0
        self._spoke = False  # any speech-energy hop in the current segment
        self._finished = False

    @property
    def capabilities(self) -> dict[str, bool]:
        return dict(STREAMING_STT_CAPABILITIES)

    @property
    def window_s(self) -> float:
        """Resolved rolling-window length (kwargs > env > default)."""
        return self._window_s

    @property
    def hop_s(self) -> float:
        """Resolved hop between interim transcriptions."""
        return self._hop_s

    @property
    def silence_ms(self) -> int:
        """Resolved trailing-silence endpointing threshold (ms)."""
        return self._silence_ms

    @property
    def max_buffer_s(self) -> float:
        """Resolved undrained-input backpressure bound."""
        return self._max_buffer_s

    @property
    def finished(self) -> bool:
        return self._finished

    @property
    def drained(self) -> bool:
        """True when every fed byte has been folded into a hop."""
        return not self._pending and not self._hop_buf

    def feed(self, pcm_s16le: bytes) -> None:
        """Enqueue one client frame. Raises BackpressureError when more than
        max_buffer_s of audio is waiting to be consumed."""
        if self._finished:
            raise BackpressureError("stream already finished")
        if len(pcm_s16le) % 2:
            raise ValueError("PCM16 frames must have an even byte length")
        if self._pending_bytes + len(pcm_s16le) > self._max_buffer_bytes:
            raise BackpressureError(
                "input buffer overflow: more than "
                f"{self._max_buffer_bytes / self._bytes_per_s:.0f}s of "
                "undrained audio"
            )
        self._pending.append(bytes(pcm_s16le))
        self._pending_bytes += len(pcm_s16le)

    def finish(self) -> None:
        """Signal end of input; remaining audio is flushed on next process."""
        self._finished = True

    @staticmethod
    def _rms(pcm_s16le: bytes | bytearray) -> float:
        if not pcm_s16le:
            return 0.0
        samples = np.frombuffer(bytes(pcm_s16le), dtype=np.int16).astype(np.float32)
        return float(np.sqrt(np.mean(samples * samples))) / 32768.0

    def _next_hop(self) -> bytes | None:
        """Fold pending frames into the hop buffer; return one full hop.

        After finish(), a trailing partial hop is returned once so no fed
        audio is ever stranded.
        """
        while self._pending and len(self._hop_buf) < self._hop_bytes:
            chunk = self._pending.popleft()
            self._pending_bytes -= len(chunk)
            self._hop_buf += chunk
        if len(self._hop_buf) >= self._hop_bytes:
            hop = bytes(self._hop_buf[: self._hop_bytes])
            del self._hop_buf[: self._hop_bytes]
            return hop
        if self._finished and self._hop_buf and not self._pending:
            hop = bytes(self._hop_buf)
            self._hop_buf.clear()
            return hop
        return None

    async def _transcribe(self, pcm: bytes | bytearray) -> str:
        try:
            return await self._recognizer.transcribe_pcm(
                bytes(pcm), sample_rate=self._sample_rate, channels=1
            )
        except Exception as exc:  # noqa: BLE001 - never kill the stream
            log.warning("stream transcription failed", error=str(exc)[:200])
            return ""

    async def process_pending(self) -> list[dict[str, Any]]:
        """Consume all complete hops currently available; return events."""
        events: list[dict[str, Any]] = []
        while True:
            hop = self._next_hop()
            if hop is None:
                break
            events.extend(await self._process_hop(hop))
        return events

    async def _process_hop(self, hop: bytes) -> list[dict[str, Any]]:
        hop_s = len(hop) / self._bytes_per_s
        self._window += hop
        if len(self._window) > self._window_bytes:
            del self._window[: len(self._window) - self._window_bytes]
        self._segment += hop
        self._consumed_s += hop_s

        if self._rms(hop) < SILENCE_RMS_THRESHOLD:
            self._trailing_silence_s += hop_s
        else:
            self._trailing_silence_s = 0.0
            self._spoke = True

        interim_text = await self._transcribe(self._window)
        events: list[dict[str, Any]] = [
            {"type": "interim", "text": interim_text, "ts": round(self._consumed_s, 3)}
        ]

        segment_s = self._consumed_s - self._segment_start_s
        endpointed = (
            self._spoke and self._trailing_silence_s >= self._silence_s
        ) or segment_s >= MAX_SEGMENT_S
        if endpointed and self._segment:
            end_ts = max(
                self._segment_start_s, self._consumed_s - self._trailing_silence_s
            )
            events.append(await self._emit_final(end_ts))
        return events

    async def _emit_final(self, end_ts: float) -> dict[str, Any]:
        text = await self._transcribe(self._segment)
        event = {
            "type": "final",
            "text": text,
            "start_ts": round(self._segment_start_s, 3),
            "end_ts": round(end_ts, 3),
        }
        self._segment.clear()
        self._window.clear()
        self._spoke = False
        self._trailing_silence_s = 0.0
        self._segment_start_s = self._consumed_s
        return event

    async def flush_final(self) -> dict[str, Any] | None:
        """Endpoint the trailing segment on stream end (disconnect flush)."""
        if not self._segment:
            return None
        return await self._emit_final(self._consumed_s)

    async def events(self) -> AsyncIterator[dict[str, Any]]:
        """Convenience async generator: drains hops until finish() + drained,
        then yields the flush final. The control plane drives
        process_pending/flush_final directly; this is for embedders/tests."""
        while True:
            for event in await self.process_pending():
                yield event
            if self._finished and self.drained:
                final = await self.flush_final()
                if final is not None:
                    yield final
                return
            await asyncio.sleep(0.005)
