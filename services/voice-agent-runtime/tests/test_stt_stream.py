"""SPEC-W48-C streaming STT tests (/v1/stt/stream).

The recognizer behind the real STTInterface seam is a test-only stub
(energy -> "speech"); faster-whisper model download is NOT required. The
production default remains FasterWhisperSTT (wired in create_app). The
LiveKit batch bridge capabilities stay streaming=False — asserted here.
"""

from __future__ import annotations

import asyncio
import math
import struct

import numpy as np
import pytest
from starlette.testclient import TestClient
from starlette.websockets import WebSocketDisconnect

from app.config import Settings
from app.control_plane import create_app
from app.pipeline.stt_stream import (
    STREAMING_STT_CAPABILITIES,
    BackpressureError,
    StreamingSTTEngine,
)

SAMPLE_RATE = 16000
BYTES_PER_S = SAMPLE_RATE * 2  # mono PCM16


def tone_pcm(seconds: float, freq: float = 440.0, amplitude: float = 0.3) -> bytes:
    """Synthetic speech-like PCM16 tone (enough energy to read as speech)."""
    n = int(seconds * SAMPLE_RATE)
    out = bytearray(n * 2)
    for i in range(n):
        sample = int(amplitude * 32767 * math.sin(2 * math.pi * freq * i / SAMPLE_RATE))
        struct.pack_into("<h", out, i * 2, sample)
    return bytes(out)


def silence_pcm(seconds: float) -> bytes:
    return b"\x00\x00" * int(seconds * SAMPLE_RATE)


class StubRecognizer:
    """Test-only STTInterface impl: 'speech' when the chunk has tone-level
    energy, '' for silence. delay_s simulates slow inference (backpressure
    test)."""

    def __init__(self, delay_s: float = 0.0) -> None:
        self.calls = 0
        self._delay_s = delay_s

    async def transcribe_pcm(
        self,
        pcm_s16le: bytes,
        *,
        sample_rate: int,
        channels: int = 1,
        language: str | None = None,
    ) -> str:
        self.calls += 1
        if self._delay_s:
            await asyncio.sleep(self._delay_s)
        if not pcm_s16le:
            return ""
        samples = np.frombuffer(pcm_s16le, dtype=np.int16).astype(np.float32)
        rms = float(np.sqrt(np.mean(samples * samples))) / 32768.0
        return "speech" if rms > 0.01 else ""


def _engine(recognizer=None, **kw) -> StreamingSTTEngine:
    kw.setdefault("window_s", 1.0)
    kw.setdefault("hop_s", 0.25)
    kw.setdefault("silence_ms", 700)
    return StreamingSTTEngine(recognizer or StubRecognizer(), **kw)


async def _collect(engine: StreamingSTTEngine) -> list[dict]:
    return [event async for event in engine.events()]


# ---------------------------------------------------------------------------
# Engine (rolling window + endpointing), driving the real STTInterface seam.
# ---------------------------------------------------------------------------


async def test_interim_events_arrive_before_final():
    engine = _engine()
    engine.feed(tone_pcm(0.5) + silence_pcm(0.75))
    engine.finish()
    events = await _collect(engine)

    kinds = [e["type"] for e in events]
    assert kinds.count("interim") >= 2  # one per 0.25s hop
    assert kinds.index("interim") < kinds.index("final")
    # 0.5s tone + 0.75s silence at 0.25s hop => final on the 5th hop
    # (0.75s trailing silence >= 700ms threshold), no trailing flush final
    # because the segment endpointed.
    assert kinds.count("final") == 1


async def test_final_transcript_non_empty_on_tone_input():
    engine = _engine()
    engine.feed(tone_pcm(0.5) + silence_pcm(0.75))
    engine.finish()
    events = await _collect(engine)

    final = next(e for e in events if e["type"] == "final")
    assert final["text"] == "speech"
    # Segment spans the tone; end_ts excludes the trailing silence tail.
    assert final["start_ts"] == 0.0
    assert final["end_ts"] == pytest.approx(0.5)
    interims = [e for e in events if e["type"] == "interim"]
    assert all("ts" in e for e in interims)
    assert [e["ts"] for e in interims] == sorted(e["ts"] for e in interims)


async def test_silence_only_stream_endpoints_nothing_until_flush():
    engine = _engine()
    engine.feed(silence_pcm(1.0))
    events = await engine.process_pending()
    assert events and all(e["type"] == "interim" for e in events)

    engine.finish()
    events += await engine.process_pending()
    flush = await engine.flush_final()
    assert flush is not None and flush["type"] == "final"
    assert flush["text"] == ""


async def test_disconnect_flush_emits_trailing_segment_as_final():
    # No silence tail: the stream ends mid-utterance and the trailing
    # segment must still surface as a final.
    engine = _engine()
    engine.feed(tone_pcm(0.6))
    engine.finish()
    events = await _collect(engine)
    finals = [e for e in events if e["type"] == "final"]
    assert len(finals) == 1
    assert finals[0]["text"] == "speech"
    assert finals[0]["end_ts"] == pytest.approx(0.6)


async def test_feed_backpressure_raises_past_buffer_bound():
    engine = _engine(max_buffer_s=1.0)
    engine.feed(silence_pcm(0.6))  # 0.6s < 1.0s bound
    with pytest.raises(BackpressureError):
        engine.feed(silence_pcm(0.6))  # 1.2s undrained > bound


async def test_feed_rejects_odd_byte_frames():
    engine = _engine()
    with pytest.raises(ValueError):
        engine.feed(b"\x00\x00\x00")


def test_streaming_capabilities_declaration():
    assert STREAMING_STT_CAPABILITIES == {"streaming": True, "interim_results": True}
    engine = _engine()
    assert engine.capabilities == STREAMING_STT_CAPABILITIES


def test_batch_livekit_bridge_capabilities_untouched():
    """The W48-C capability flip applies ONLY to the new streaming
    endpoint; the batch LiveKit bridge keeps declaring non-streaming."""
    from app.livekit_worker import WhisperSTTNode
    from app.pipeline.stt import FasterWhisperSTT

    node = WhisperSTTNode(FasterWhisperSTT())
    assert node.capabilities.streaming is False
    assert node.capabilities.interim_results is False


# ---------------------------------------------------------------------------
# WebSocket endpoint: auth (K15 session-secret), event flow, backpressure.
# ---------------------------------------------------------------------------


def _app_and_session(recognizer=None, **setting_over):
    settings = Settings(**setting_over)
    app = create_app(settings, stt_stream_recognizer=recognizer or StubRecognizer())
    session = app.state.session_store.get_or_create(None, "demo")
    return app, session


def _stream_url(session) -> str:
    return f"/v1/stt/stream?conversation_id={session.conversation_id}"


def test_ws_happy_path_interim_then_final():
    app, session = _app_and_session()
    client = TestClient(app)
    with client.websocket_connect(
        _stream_url(session), headers={"X-Session-Secret": session.session_secret}
    ) as ws:
        ready = ws.receive_json()
        assert ready["type"] == "ready"
        assert ready["capabilities"] == {
            "streaming": True,
            "interim_results": True,
        }
        ws.send_bytes(tone_pcm(0.5) + silence_pcm(0.75))
        events: list[dict] = []
        while True:
            event = ws.receive_json()
            events.append(event)
            if event["type"] == "final":
                break
    kinds = [e["type"] for e in events]
    assert kinds.index("interim") < kinds.index("final")
    final = events[-1]
    assert final["text"] == "speech"
    assert final["start_ts"] == 0.0
    assert final["end_ts"] == pytest.approx(0.5)


def test_ws_rejects_missing_credentials():
    app, session = _app_and_session()
    client = TestClient(app)
    with pytest.raises(WebSocketDisconnect) as exc_info:
        with client.websocket_connect(_stream_url(session)):
            pass
    assert exc_info.value.code == 1008


def test_ws_rejects_wrong_secret():
    app, session = _app_and_session()
    client = TestClient(app)
    with pytest.raises(WebSocketDisconnect) as exc_info:
        with client.websocket_connect(
            _stream_url(session), headers={"X-Session-Secret": "forged-secret"}
        ):
            pass
    assert exc_info.value.code == 1008


def test_ws_rejects_unknown_conversation_with_valid_secret():
    """A valid secret must not bind the socket to a different session."""
    app, session = _app_and_session()
    client = TestClient(app)
    with pytest.raises(WebSocketDisconnect) as exc_info:
        with client.websocket_connect(
            "/v1/stt/stream?conversation_id=does-not-exist",
            headers={"X-Session-Secret": session.session_secret},
        ):
            pass
    assert exc_info.value.code == 1008


def test_ws_accepts_session_secret_query_param_fallback():
    app, session = _app_and_session()
    client = TestClient(app)
    url = _stream_url(session) + f"&session_secret={session.session_secret}"
    with client.websocket_connect(url) as ws:
        assert ws.receive_json()["type"] == "ready"


def test_ws_backpressure_closes_1008_on_overflow():
    # Slow recognizer (0.4s per chunk) + a 0.1s input bound: flooding the
    # socket overflows the undrained-input bound -> close 1008 + reason.
    app, session = _app_and_session(
        recognizer=StubRecognizer(delay_s=0.4), stt_stream_max_buffer_s=0.1
    )
    client = TestClient(app)
    with client.websocket_connect(
        _stream_url(session), headers={"X-Session-Secret": session.session_secret}
    ) as ws:
        assert ws.receive_json()["type"] == "ready"
        chunk = silence_pcm(0.05)
        with pytest.raises(WebSocketDisconnect) as exc_info:
            for _ in range(40):  # 2s of audio against a 0.1s bound
                ws.send_bytes(chunk)
            while True:
                ws.receive_json()
    assert exc_info.value.code == 1008
    assert exc_info.value.reason
