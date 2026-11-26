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


def test_faster_whisper_imports():
    """The receptionist's ears: the STT backend must import in the shipped image (it lacked `requests`)."""
    import faster_whisper  # noqa: F401


# ---------------------------------------------------------------------------
# SPEC-W73 C2: env-tunable rolling-window parameters. Env is read at ENGINE
# CONSTRUCTION (StreamingSTTEngine.__init__), never at import. Precedence:
# explicit kwargs > env > module defaults (byte-identical to pre-W73
# behavior). All tests isolate env via monkeypatch -> order-independent.
# ---------------------------------------------------------------------------

STT_STREAM_ENV_VARS = (
    "STT_STREAM_WINDOW_S",
    "STT_STREAM_HOP_S",
    "STT_STREAM_SILENCE_MS",
    "STT_STREAM_MAX_BUFFER_S",
)


@pytest.fixture
def clean_stt_env(monkeypatch):
    """Guarantee none of the STT_STREAM_* vars leak in from the ambient env."""
    for name in STT_STREAM_ENV_VARS:
        monkeypatch.delenv(name, raising=False)
    return monkeypatch


def test_defaults_unchanged_without_env_or_kwargs(clean_stt_env):
    """Pre-W73 behavior: no kwargs, no env -> the original hardcoded
    defaults, byte-identical derived buffers."""
    engine = StreamingSTTEngine(StubRecognizer())
    assert engine.window_s == 1.0
    assert engine.hop_s == 0.25
    assert engine.silence_ms == 700
    assert engine.max_buffer_s == 8.0
    # Derived internals match the pre-W73 constructor math exactly.
    assert engine._window_bytes == int(1.0 * BYTES_PER_S)
    assert engine._hop_bytes == int(0.25 * BYTES_PER_S)
    assert engine._silence_s == 0.7
    assert engine._max_buffer_bytes == int(8.0 * BYTES_PER_S)


def test_env_override_applied_at_construction(clean_stt_env):
    clean_stt_env.setenv("STT_STREAM_WINDOW_S", "2.0")
    clean_stt_env.setenv("STT_STREAM_HOP_S", "0.5")
    clean_stt_env.setenv("STT_STREAM_SILENCE_MS", "900")
    clean_stt_env.setenv("STT_STREAM_MAX_BUFFER_S", "4.0")
    engine = StreamingSTTEngine(StubRecognizer())
    assert engine.window_s == 2.0
    assert engine.hop_s == 0.5
    assert engine.silence_ms == 900
    assert engine.max_buffer_s == 4.0
    assert engine._window_bytes == int(2.0 * BYTES_PER_S)
    assert engine._hop_bytes == int(0.5 * BYTES_PER_S)
    assert engine._silence_s == 0.9
    assert engine._max_buffer_bytes == int(4.0 * BYTES_PER_S)


def test_env_read_at_construction_not_import(clean_stt_env):
    """Changing env AFTER construction must not mutate a live engine, and a
    later engine must pick up the new value."""
    engine_before = StreamingSTTEngine(StubRecognizer())
    clean_stt_env.setenv("STT_STREAM_HOP_S", "0.5")
    assert engine_before.hop_s == 0.25
    engine_after = StreamingSTTEngine(StubRecognizer())
    assert engine_after.hop_s == 0.5


def test_kwargs_beat_env(clean_stt_env):
    clean_stt_env.setenv("STT_STREAM_WINDOW_S", "2.0")
    clean_stt_env.setenv("STT_STREAM_HOP_S", "0.5")
    clean_stt_env.setenv("STT_STREAM_SILENCE_MS", "900")
    clean_stt_env.setenv("STT_STREAM_MAX_BUFFER_S", "4.0")
    engine = StreamingSTTEngine(
        StubRecognizer(), window_s=1.0, hop_s=0.25, silence_ms=700, max_buffer_s=8.0
    )
    assert engine.window_s == 1.0
    assert engine.hop_s == 0.25
    assert engine.silence_ms == 700
    assert engine.max_buffer_s == 8.0
    # Partial kwargs: only the unset parameter falls back to env.
    partial = StreamingSTTEngine(StubRecognizer(), window_s=3.0)
    assert partial.window_s == 3.0
    assert partial.hop_s == 0.5  # from env
    assert partial.silence_ms == 900  # from env
    assert partial.max_buffer_s == 4.0  # from env


@pytest.mark.parametrize(
    "kwargs",
    [
        {"window_s": 0.0},  # window must be > 0
        {"window_s": -1.0},
        {"hop_s": 0.0},  # hop must be > 0
        {"hop_s": -0.25},
        {"window_s": 0.5, "hop_s": 1.0},  # hop must be <= window
        {"silence_ms": -1},  # silence must be >= 0
        {"max_buffer_s": 0.0},  # buffer must be > 0 (pre-W73 bound kept: a
        {"max_buffer_s": -1.0},  # tighter-than-hop buffer is legitimate)
    ],
)
def test_bounds_violations_raise_value_error(clean_stt_env, kwargs):
    with pytest.raises(ValueError):
        StreamingSTTEngine(StubRecognizer(), **kwargs)


def test_env_bounds_violation_raises_value_error(clean_stt_env):
    """Invalid env values fail loudly at construction, not silently."""
    clean_stt_env.setenv("STT_STREAM_HOP_S", "2.0")  # > default window 1.0
    with pytest.raises(ValueError, match="hop_s"):
        StreamingSTTEngine(StubRecognizer())
    clean_stt_env.setenv("STT_STREAM_HOP_S", "0.25")
    clean_stt_env.setenv("STT_STREAM_MAX_BUFFER_S", "0")  # must be > 0
    with pytest.raises(ValueError, match="max_buffer_s"):
        StreamingSTTEngine(StubRecognizer())


def test_env_unparseable_value_raises_value_error(clean_stt_env):
    clean_stt_env.setenv("STT_STREAM_WINDOW_S", "not-a-number")
    with pytest.raises(ValueError, match="STT_STREAM_WINDOW_S"):
        StreamingSTTEngine(StubRecognizer())


async def test_env_tuned_engine_still_streams_end_to_end(clean_stt_env):
    """Functional proof: an env-tuned engine (larger hop + silence floor)
    endpoints with the tuned parameters, not the defaults."""
    clean_stt_env.setenv("STT_STREAM_HOP_S", "0.5")
    clean_stt_env.setenv("STT_STREAM_SILENCE_MS", "1000")
    engine = StreamingSTTEngine(StubRecognizer())
    assert engine.hop_s == 0.5
    engine.feed(tone_pcm(0.5) + silence_pcm(1.0))
    engine.finish()
    events = await _collect(engine)
    kinds = [e["type"] for e in events]
    # 0.5s hops over 1.5s of audio -> 3 interims; endpoint fires once the
    # 1.0s trailing-silence floor is reached (2nd silent hop).
    assert kinds == ["interim", "interim", "interim", "final"]
    final = events[-1]
    assert final["text"] == "speech"
    assert final["end_ts"] == pytest.approx(0.5)


# ---------------------------------------------------------------------------
# SPEC-W75 (WS-B): on_hop_timing hook -> voice_stt_stream_hop_seconds.
# The hook observes the elapsed wall-clock seconds of EVERY recognizer
# call (interim hops AND final-segment transcriptions); it is keyword-only,
# defaults to None (zero behavior change), and its exceptions are swallowed
# so a metrics hook never breaks the stream. ADDITIVE section: every test
# above is byte-identical to pre-W75.
# ---------------------------------------------------------------------------


async def test_hop_timing_hook_receives_positive_float_per_recognizer_call():
    """B4(a): scripted PCM through the engine with the stub recognizer;
    the hook fires exactly once per recognizer call (interim hops + the
    final-segment transcription), always with a positive float."""
    timings: list[float] = []
    stub = StubRecognizer()
    engine = _engine(stub, on_hop_timing=timings.append)
    engine.feed(tone_pcm(0.5) + silence_pcm(0.75))
    engine.finish()
    events = await _collect(engine)

    kinds = [e["type"] for e in events]
    assert kinds.count("interim") >= 2
    assert kinds.count("final") == 1
    # One timing per recognizer call, interim hops and final alike.
    assert stub.calls >= 3
    assert len(timings) == stub.calls
    assert all(isinstance(t, float) and t > 0 for t in timings)


class _RecordingLogger:
    """Captures structlog-style ``warning(event, **kw)`` calls; the engine
    module's logger is swapped for this via monkeypatch (the app logs
    through structlog's PrintLogger, so caplog cannot see it)."""

    def __init__(self) -> None:
        self.warnings: list[str] = []

    def warning(self, event: str, **_kw) -> None:
        self.warnings.append(event)


async def test_hop_timing_hook_raising_never_breaks_the_stream(monkeypatch):
    """B4(b): a hook that raises is swallowed (logged); the stream
    completes normally and LATER hops are still timed."""
    import app.pipeline.stt_stream as stt_stream_module

    recorder = _RecordingLogger()
    monkeypatch.setattr(stt_stream_module, "log", recorder)

    timings: list[float] = []
    calls = {"n": 0}

    def flaky_hook(elapsed: float) -> None:
        calls["n"] += 1
        if calls["n"] == 1:
            raise RuntimeError("metrics backend exploded")
        timings.append(elapsed)

    stub = StubRecognizer()
    engine = _engine(stub, on_hop_timing=flaky_hook)
    engine.feed(tone_pcm(0.5) + silence_pcm(0.75))
    engine.finish()
    events = await _collect(engine)

    # Stream unaffected: interims + the endpointed final arrive as usual.
    kinds = [e["type"] for e in events]
    assert kinds.count("interim") >= 2
    assert kinds.count("final") == 1
    final = events[-1]
    assert final["text"] == "speech"
    assert final["end_ts"] == pytest.approx(0.5)
    # The hook kept being CALLED for every later hop; only the first raise
    # was swallowed, so all subsequent recognizer calls were timed.
    assert calls["n"] == stub.calls
    assert len(timings) == stub.calls - 1
    assert all(t > 0 for t in timings)
    # Logged, never raised.
    assert recorder.warnings.count("stt hop-timing hook raised") == 1


async def test_hop_timing_hook_logged_only_once(monkeypatch):
    """Log-once: a hook that ALWAYS raises produces exactly one warning,
    no matter how many hops are timed."""
    import app.pipeline.stt_stream as stt_stream_module

    recorder = _RecordingLogger()
    monkeypatch.setattr(stt_stream_module, "log", recorder)

    def always_raises(elapsed: float) -> None:
        raise RuntimeError("down")

    stub = StubRecognizer()
    engine = _engine(stub, on_hop_timing=always_raises)
    engine.feed(tone_pcm(0.5) + silence_pcm(0.75))
    engine.finish()
    events = await _collect(engine)
    assert any(e["type"] == "final" for e in events)
    assert stub.calls >= 3
    assert recorder.warnings.count("stt hop-timing hook raised") == 1


async def test_default_none_hook_zero_behavior_change():
    """B4(c): no on_hop_timing kwarg -> pre-W75 behavior, byte-identical
    event stream to the established happy path."""
    engine = _engine()  # no hook
    engine.feed(tone_pcm(0.5) + silence_pcm(0.75))
    engine.finish()
    events = await _collect(engine)
    kinds = [e["type"] for e in events]
    assert kinds.count("interim") >= 2
    assert kinds.count("final") == 1
    final = events[-1]
    assert final["text"] == "speech"
    assert final["start_ts"] == 0.0
    assert final["end_ts"] == pytest.approx(0.5)


def test_metrics_registry_exposes_stt_stream_hop_histogram():
    """B4(d): the registry exposes voice_stt_stream_hop_seconds with the
    W71-consistent default buckets, rendered in the exposition."""
    from app import metrics

    registry = metrics.reset_registry()  # isolate from other tests
    hist = registry.stt_stream_hop
    assert isinstance(hist, metrics.Histogram)
    assert hist.name == "voice_stt_stream_hop_seconds"
    assert hist.buckets == metrics.DEFAULT_BUCKETS
    hist.observe(0.123)
    text = metrics.render()
    assert "# TYPE voice_stt_stream_hop_seconds histogram" in text
    assert "voice_stt_stream_hop_seconds_count 1" in text
    assert "voice_stt_stream_hop_seconds_sum 0.123000" in text


def test_ws_endpoint_wires_hop_timing_into_registry():
    """B3 proof: a stream through the real WS endpoint observes hop
    timings into the process registry's stt_stream_hop histogram."""
    from app import metrics

    registry = metrics.reset_registry()
    app, session = _app_and_session()
    client = TestClient(app)
    with client.websocket_connect(
        _stream_url(session), headers={"X-Session-Secret": session.session_secret}
    ) as ws:
        assert ws.receive_json()["type"] == "ready"
        ws.send_bytes(tone_pcm(0.5) + silence_pcm(0.75))
        while True:
            if ws.receive_json()["type"] == "final":
                break
    assert registry.stt_stream_hop._count >= 3  # interims + final
