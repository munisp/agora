"""SPEC-W71 tests: 10 voice-agent performance improvements.

Covers:
- P1: process-wide silero VAD cache (same object on repeated loads,
  race-safe under concurrent builds, keyed by tuned kwargs);
- P2: whisper decode fast-path kwargs (temperature=0.0,
  without_timestamps=True) on the transcribe call;
- P3: WHISPER_CPU_THREADS / WHISPER_NUM_WORKERS passed to every
  WhisperModel construction + env parsing (incl. bad values);
- P4: TTS chunk prefetch (overlap, ordering, cache entry in original
  order, error propagation at the sequential point, prefetch cancel on
  early exit);
- P5: subprocess TTS via piper --output-raw stdout (command shape, no
  temp file, sample rate from the voice .onnx.json cached per voice,
  in-memory WAV wrap);
- P6: mid-call phrase cache warming (warm set derived from the real
  ToolAckPolicy, scheduling does not block, cache ends populated,
  failure-tolerant);
- P7: VOICE_LLM_MAX_TOKENS on our own OpenAI-compatible client paths
  (mock client create() kwargs; 0 disables);
- P8: chat-history windowing boundary matrix (orphaned-tool-call
  protection, system retention, disabled-when-0) + the agent hook;
- P9: per-stage latency metrics present in the registry and observed on
  the instrumented paths.
"""

from __future__ import annotations

import asyncio
import io
import json
import subprocess
import sys
import tempfile
import threading
import time
import types
import wave
from types import SimpleNamespace

import httpx
import pytest

from app import metrics
from app.async_tools import DEFAULT_ACK, SLOW_TOOLS, ToolAckPolicy
from app.config import Settings, load_settings
from app.pipeline.llm import OpenAICompatibleLLM, build_llm, run_tool_loop_stream
from app.pipeline.stt import FasterWhisperSTT
from app.pipeline.tts import PiperTTS, _wav_to_pcm
from app.tenant_context import TenantContext

try:  # pragma: no cover - import shim (real SDK present in the worker image)
    import app.livekit_worker as lw
except ModuleNotFoundError:  # pragma: no cover - import shim
    import test_live_tts_chain  # noqa: F401 - installs the livekit stubs

    import app.livekit_worker as lw


@pytest.fixture(autouse=True)
def _clean_state():
    metrics.reset_registry()
    lw._vad_cache_clear()
    lw._PREWARMED.clear()
    yield
    lw._vad_cache_clear()
    lw._PREWARMED.clear()


def _ctx(**over) -> TenantContext:
    base = dict(
        site_slug="acme",
        tenant_id="t1",
        tenant_slug="acme",
        display_name="Acme",
        locale="en-US",
    )
    base.update(over)
    return TenantContext(**base)


def _wav_bytes(rate: int = 22050, frames: int = 64) -> bytes:
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)
        wf.setframerate(rate)
        wf.writeframes(b"\x01\x00" * frames)
    return buf.getvalue()


# ---------------------------------------------------------------------------
# P1: process-wide silero VAD cache
# ---------------------------------------------------------------------------
class TestP1VadCache:
    def test_two_loads_return_same_object(self, monkeypatch):
        recorder = []

        class _VAD:
            @staticmethod
            def load(*, min_silence_duration=0.55):
                recorder.append(min_silence_duration)
                return object()

        monkeypatch.setattr(lw, "silero", SimpleNamespace(VAD=_VAD))
        first = lw._load_vad(Settings())
        second = lw._load_vad(Settings())
        assert first is second  # warm processes skip the load entirely
        assert recorder == [0.35]  # silero model loaded ONCE
        # A different tuning is a different cache entry.
        third = lw._load_vad(Settings(voice_vad_min_silence_s=0.9))
        assert third is not first
        assert recorder == [0.35, 0.9]

    def test_concurrent_builds_race_safe(self, monkeypatch):
        recorder = []
        lock = threading.Lock()

        class _VAD:
            @staticmethod
            def load(*, min_silence_duration=0.55):
                time.sleep(0.01)  # widen the race window
                with lock:
                    recorder.append(min_silence_duration)
                return object()

        monkeypatch.setattr(lw, "silero", SimpleNamespace(VAD=_VAD))
        barrier = threading.Barrier(8)
        results: list = []

        def _worker():
            barrier.wait()
            results.append(lw._load_vad(Settings()))

        threads = [threading.Thread(target=_worker) for _ in range(8)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        assert len(results) == 8
        assert recorder == [0.35]  # exactly one load under the race
        assert all(r is results[0] for r in results)


# ---------------------------------------------------------------------------
# P2: whisper decode fast-path
# ---------------------------------------------------------------------------
class _RecordingModel:
    def __init__(self) -> None:
        self.calls: list[dict] = []

    def transcribe(self, audio, **kwargs):  # noqa: ARG002
        self.calls.append(kwargs)
        return [SimpleNamespace(text=" hello ")], SimpleNamespace(language="en")


PCM_1S = b"\x00\x00" * 16000  # 1s of 16 kHz s16le


class TestP2DecodeFastPath:
    async def test_transcribe_sets_fast_path_kwargs(self):
        stt = FasterWhisperSTT(model_size="base")
        fake = _RecordingModel()
        stt._model = fake  # skip the real whisper load (test_metrics pattern)
        text = await stt.transcribe_pcm(PCM_1S, sample_rate=16000)
        assert text == "hello"
        assert fake.calls[0]["temperature"] == 0.0
        assert fake.calls[0]["without_timestamps"] is True
        # Pre-existing decode levers untouched.
        assert fake.calls[0]["condition_on_previous_text"] is False
        assert fake.calls[0]["beam_size"] == 1
        assert fake.calls[0]["vad_filter"] is False


# ---------------------------------------------------------------------------
# P3: CTranslate2 thread tuning
# ---------------------------------------------------------------------------
class TestP3ThreadTuning:
    def test_ctor_kwargs_passed_to_whisper_model(self, monkeypatch):
        calls = []
        fake_fw = types.SimpleNamespace(
            WhisperModel=lambda *a, **kw: calls.append(kw) or object()
        )
        monkeypatch.setitem(sys.modules, "faster_whisper", fake_fw)
        stt = FasterWhisperSTT(
            model_size="base", device="cpu", cpu_threads=4, num_workers=2
        )
        stt._load_sync("base")
        # ...and the multilingual override construction gets them too.
        stt._load_sync("large-v3-turbo")
        assert len(calls) == 2
        for kw in calls:
            assert kw["cpu_threads"] == 4
            assert kw["num_workers"] == 2

    def test_defaults_preserve_ctranslate2_auto(self):
        s = Settings()
        assert s.whisper_cpu_threads == 0  # 0 = CTranslate2 auto
        assert s.whisper_num_workers == 1

    def test_env_parsing(self, monkeypatch):
        monkeypatch.setenv("WHISPER_CPU_THREADS", "8")
        monkeypatch.setenv("WHISPER_NUM_WORKERS", "4")
        s = load_settings()
        assert s.whisper_cpu_threads == 8
        assert s.whisper_num_workers == 4

    def test_bad_env_values_fall_back_to_defaults(self, monkeypatch):
        monkeypatch.setenv("WHISPER_CPU_THREADS", "lots")
        monkeypatch.setenv("WHISPER_NUM_WORKERS", "")
        s = load_settings()
        assert s.whisper_cpu_threads == 0
        assert s.whisper_num_workers == 1

    def test_build_stt_wires_knobs(self):
        stt = lw._build_stt(Settings(whisper_cpu_threads=6, whisper_num_workers=3))
        assert stt._cpu_threads == 6
        assert stt._num_workers == 3


# ---------------------------------------------------------------------------
# P4: TTS chunk prefetch
# ---------------------------------------------------------------------------
def _piper_with_fake_synth(synth) -> PiperTTS:
    piper = PiperTTS(mode="http")
    piper.synthesize_pcm = synth  # instance-level override
    return piper


class TestP4ChunkPrefetch:
    async def test_overlap_ordering_and_cache_entry(self):
        intervals: dict[str, tuple[float, float]] = {}

        async def synth(text: str) -> bytes:
            start = time.monotonic()
            # Uneven chunk times make the overlap observable: chunk 3's
            # prefetch must start while chunk 2 (the slow one) still runs.
            await asyncio.sleep(0.1 if text == "Two." else 0.02)
            intervals[text] = (start, time.monotonic())
            return text.encode()

        piper = _piper_with_fake_synth(synth)
        chunks = [p async for p in piper.stream_pcm("One. Two. Three.")]
        # Ordering of yielded bytes unchanged (sequential order).
        assert chunks == [b"One.", b"Two.", b"Three."]
        # Overlap: chunk N+1 synthesis started before chunk N finished.
        assert intervals["Two."][0] < intervals["One."][1]
        assert intervals["Three."][0] < intervals["Two."][1]
        # Full-utterance cache entry assembled in ORIGINAL chunk order.
        cached = piper._cache.get((piper.voice, "One. Two. Three.", "pcm"))
        assert cached == b"One.Two.Three."

    async def test_error_surfaces_at_sequential_position(self):
        async def synth(text: str) -> bytes:
            await asyncio.sleep(0.01)
            if text == "Two.":
                raise RuntimeError("boom")
            return text.encode()

        piper = _piper_with_fake_synth(synth)
        got: list[bytes] = []
        with pytest.raises(RuntimeError, match="boom"):
            async for pcm in piper.stream_pcm("One. Two. Three."):
                got.append(pcm)
        # Exactly like the sequential path: chunk 1 delivered, then the
        # chunk-2 failure raises; chunk 3 never yielded.
        assert got == [b"One."]

    async def test_empty_chunk_skipping_preserved(self):
        async def synth(text: str) -> bytes:
            await asyncio.sleep(0)
            return b"" if text == "Two." else text.encode()

        piper = _piper_with_fake_synth(synth)
        chunks = [p async for p in piper.stream_pcm("One. Two. Three.")]
        assert chunks == [b"One.", b"Three."]
        cached = piper._cache.get((piper.voice, "One. Two. Three.", "pcm"))
        assert cached == b"One.Three."

    async def test_prefetch_cancelled_on_early_exit(self):
        entered = asyncio.Event()
        release = asyncio.Event()

        async def synth(text: str) -> bytes:
            if text == "Two.":
                entered.set()
                await release.wait()  # hangs unless the prefetch is cancelled
            return text.encode()

        piper = _piper_with_fake_synth(synth)
        agen = piper.stream_pcm("One. Two. Three.")
        assert await agen.__anext__() == b"One."
        await asyncio.wait_for(entered.wait(), 1)  # prefetch in flight
        await agen.aclose()  # early exit: generator close
        release.set()
        await asyncio.sleep(0.05)
        leftovers = [
            t
            for t in asyncio.all_tasks()
            if t is not asyncio.current_task() and not t.done()
        ]
        assert leftovers == []  # no leaked prefetch task


# ---------------------------------------------------------------------------
# P5: subprocess TTS without temp files (--output-raw stdout)
# ---------------------------------------------------------------------------
class TestP5SubprocessRaw:
    def _piper(self, tmp_path) -> PiperTTS:
        (tmp_path / "en_x.onnx.json").write_text(
            json.dumps({"audio": {"sample_rate": 16000}})
        )
        return PiperTTS(mode="subprocess", model_dir=str(tmp_path), voice="en_x")

    async def test_raw_stdout_command_shape_no_temp_file(
        self, monkeypatch, tmp_path
    ):
        calls = []

        class _Proc:
            returncode = 0
            stderr = b""
            stdout = b"\x01\x00" * 8

        def fake_run(cmd, **kwargs):
            calls.append((cmd, kwargs))
            return _Proc()

        monkeypatch.setattr(subprocess, "run", fake_run)

        def _no_temp_file(*a, **kw):
            raise AssertionError("temp file must not be created (SPEC-W71 P5)")

        monkeypatch.setattr(tempfile, "NamedTemporaryFile", _no_temp_file)

        piper = self._piper(tmp_path)
        pcm, rate = await piper._synthesize_subprocess("hello")
        assert pcm == b"\x01\x00" * 8
        assert rate == 16000  # from the voice .onnx.json, not the default
        cmd, kwargs = calls[0]
        assert "--output-raw" in cmd
        assert "--output_file" not in cmd and "--output-file" not in cmd
        assert cmd[cmd.index("--model") + 1].endswith("en_x.onnx")
        assert cmd[cmd.index("--config") + 1].endswith("en_x.onnx.json")
        assert kwargs["input"] == b"hello"
        assert kwargs["stdout"] is subprocess.PIPE

    async def test_wav_path_wraps_pcm_in_memory(self, monkeypatch, tmp_path):
        class _Proc:
            returncode = 0
            stderr = b""
            stdout = b"\x02\x00" * 16

        monkeypatch.setattr(subprocess, "run", lambda *a, **kw: _Proc())
        piper = self._piper(tmp_path)
        wav = await piper._subprocess_wav("hello", "en_x")
        pcm, rate = _wav_to_pcm(wav)
        assert pcm == b"\x02\x00" * 16
        assert rate == 16000

    async def test_sample_rate_cached_per_voice(self, monkeypatch, tmp_path):
        class _Proc:
            returncode = 0
            stderr = b""
            stdout = b"\x00\x00"

        monkeypatch.setattr(subprocess, "run", lambda *a, **kw: _Proc())
        piper = self._piper(tmp_path)
        _, rate1 = await piper._synthesize_subprocess("one")
        assert rate1 == 16000
        # Config read once: delete it; the cached rate still applies.
        (tmp_path / "en_x.onnx.json").unlink()
        _, rate2 = await piper._synthesize_subprocess("two")
        assert rate2 == 16000

    async def test_missing_voice_config_falls_back_to_default_rate(
        self, monkeypatch, tmp_path
    ):
        class _Proc:
            returncode = 0
            stderr = b""
            stdout = b"\x00\x00"

        monkeypatch.setattr(subprocess, "run", lambda *a, **kw: _Proc())
        piper = PiperTTS(
            mode="subprocess", model_dir=str(tmp_path), voice="missing_voice",
            sample_rate=22050,
        )
        _, rate = await piper._synthesize_subprocess("hello")
        assert rate == 22050

    async def test_nonzero_exit_raises(self, monkeypatch, tmp_path):
        class _Proc:
            returncode = 3
            stderr = b"piper exploded"
            stdout = b""

        monkeypatch.setattr(subprocess, "run", lambda *a, **kw: _Proc())
        piper = self._piper(tmp_path)
        with pytest.raises(RuntimeError, match="piper exited 3"):
            await piper._synthesize_subprocess("hello")


# ---------------------------------------------------------------------------
# P6: mid-call phrase cache warming
# ---------------------------------------------------------------------------
class TestP6PhraseWarming:
    def test_warm_set_derived_from_real_ack_policy(self):
        policy = ToolAckPolicy.from_context(_ctx())
        phrases = lw.tts_warm_phrases(policy)
        assert phrases  # non-empty
        # Default industry: every slow tool resolves to DEFAULT_ACK.
        assert DEFAULT_ACK in phrases
        # Standard closers included; deduped.
        assert "Thank you for calling. Goodbye." in phrases
        assert len(phrases) == len(set(phrases))

    def test_warm_set_reflects_tenant_overrides(self):
        ctx = _ctx(terminology={"tool_ack": {"book_appointment": "Booking now…"}})
        policy = ToolAckPolicy.from_context(ctx)
        phrases = lw.tts_warm_phrases(policy)
        assert "Booking now…" in phrases
        # Industry default still covers the other slow tools.
        assert DEFAULT_ACK in phrases

    def test_warm_set_industry_flavour(self):
        policy = ToolAckPolicy.from_context(_ctx(industry="clinic"))
        assert "One moment while I check our scheduling system…" in (
            lw.tts_warm_phrases(policy)
        )

    async def test_schedule_populates_cache_without_blocking(self):
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(
                200, content=_wav_bytes(), headers={"content-type": "audio/wav"}
            )

        piper = PiperTTS(mode="http", http_url="http://piper:5500")
        piper._client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
        agent = SimpleNamespace(tts=SimpleNamespace(_impl=piper))
        policy = ToolAckPolicy.from_context(_ctx())

        t0 = time.monotonic()
        assert lw.schedule_tts_warmup(agent, policy) is True
        # Scheduling is fire-and-forget: returns without awaiting synthesis.
        assert time.monotonic() - t0 < 0.5
        for _ in range(100):
            await asyncio.sleep(0.01)
            if all(
                piper._cache.get((piper.voice, p, "pcm")) is not None
                for p in lw.tts_warm_phrases(policy)
            ):
                break
        for phrase in lw.tts_warm_phrases(policy):
            assert piper._cache.get((piper.voice, phrase, "pcm")) is not None

    async def test_warm_failure_only_logs(self):
        class _Boom:
            async def synthesize_pcm(self, text: str) -> bytes:
                raise RuntimeError("tts down")

        agent = SimpleNamespace(tts=SimpleNamespace(_impl=_Boom()))
        assert lw.schedule_tts_warmup(agent, ToolAckPolicy()) is True
        await asyncio.sleep(0.1)  # the task completes; no exception escapes

    def test_schedule_without_impl_or_phrases_returns_false(self):
        assert lw.schedule_tts_warmup(SimpleNamespace(), ToolAckPolicy()) is False
        assert (
            lw.schedule_tts_warmup(
                SimpleNamespace(tts=SimpleNamespace(_impl=object())), None
            )
            is False
        )


# ---------------------------------------------------------------------------
# P7: LLM response-length cap
# ---------------------------------------------------------------------------
class _FakeCompletions:
    def __init__(self) -> None:
        self.calls: list[dict] = []

    async def create(self, **kwargs):
        self.calls.append(kwargs)
        if kwargs.get("stream"):
            return object()  # stream object unused by this test
        msg = SimpleNamespace(content="ok", tool_calls=None)
        return SimpleNamespace(choices=[SimpleNamespace(message=msg)], usage=None)


class TestP7MaxTokens:
    def _llm(self, max_tokens: int) -> tuple[OpenAICompatibleLLM, _FakeCompletions]:
        llm = OpenAICompatibleLLM(
            base_url="http://x/v1", model="m", max_tokens=max_tokens
        )
        completions = _FakeCompletions()
        llm._client = SimpleNamespace(
            chat=SimpleNamespace(completions=completions)
        )
        return llm, completions

    def test_config_default_and_disable(self, monkeypatch):
        assert Settings().voice_llm_max_tokens == 300
        monkeypatch.setenv("VOICE_LLM_MAX_TOKENS", "0")
        assert load_settings().voice_llm_max_tokens == 0
        monkeypatch.setenv("VOICE_LLM_MAX_TOKENS", "many")
        assert load_settings().voice_llm_max_tokens == 300

    async def test_create_kwargs_buffered_path(self):
        llm, completions = self._llm(300)
        await llm.chat_with_tools([{"role": "user", "content": "hi"}], [])
        assert completions.calls[0]["max_tokens"] == 300

    async def test_create_kwargs_stream_path(self):
        llm, completions = self._llm(250)
        await llm.stream_with_tools([{"role": "user", "content": "hi"}], [])
        assert completions.calls[0]["max_tokens"] == 250
        assert completions.calls[0]["stream"] is True

    async def test_disabled_omits_cap(self):
        llm, completions = self._llm(0)
        await llm.chat_with_tools([{"role": "user", "content": "hi"}], [])
        assert completions.calls[0]["max_tokens"] is None

    def test_build_llm_wires_cap_to_primary_and_fallback(self):
        s = Settings(
            voice_llm_max_tokens=123,
            llm_fallback_base_url="http://fallback/v1",
        )
        chain = build_llm(s)
        assert chain._primary._max_tokens == 123
        assert chain._fallback._max_tokens == 123


# ---------------------------------------------------------------------------
# P8: chat-history windowing
# ---------------------------------------------------------------------------
def _m(role: str, tool_calls=None) -> SimpleNamespace:
    return SimpleNamespace(role=role, tool_calls=tool_calls)


class TestP8HistoryWindowing:
    def test_under_cap_unchanged(self):
        msgs = [_m("system"), _m("user"), _m("assistant")]
        assert lw.trim_chat_messages(msgs, 60) == msgs

    def test_disabled_when_zero(self):
        msgs = [_m("system")] + [_m("user"), _m("assistant")] * 100
        assert lw.trim_chat_messages(msgs, 0) == msgs

    def test_system_retained_and_newest_kept(self):
        sys_msg = _m("system")
        msgs = [sys_msg] + [_m("user"), _m("assistant")] * 40  # 81 msgs
        trimmed = lw.trim_chat_messages(msgs, 62)
        assert trimmed[0] is sys_msg
        # Cut walked back to a user boundary: body starts with a user turn.
        # Walking BACKWARD retains at most one extra message over the cap —
        # safety (no orphaned tool calls) wins over the exact cap.
        assert trimmed[1].role == "user"
        assert 62 <= len(trimmed) <= 63
        # Newest messages kept.
        assert trimmed[-1] is msgs[-1]

    def test_never_separates_tool_calls_from_results(self):
        sys_msg = _m("system")
        msgs = [
            sys_msg,
            _m("user"),                      # 1
            _m("assistant", tool_calls=["c1"]),  # 2 tool-call turn
            _m("tool"),                      # 3 result of c1
            _m("assistant"),                 # 4 spoken answer
            _m("user"),                      # 5
            _m("assistant"),                 # 6
        ]
        # Cap forces the raw cut onto the tool-result message (index 3);
        # the boundary walk must move back to the user message (index 1).
        trimmed = lw.trim_chat_messages(msgs, 4)
        assert trimmed[0] is sys_msg
        assert trimmed[1].role == "user"
        # The assistant tool_calls message and its tool result travel
        # together (both retained or both dropped — here both retained).
        roles = [m.role for m in trimmed]
        assert "tool" not in roles or any(
            m.role == "assistant" and m.tool_calls for m in trimmed
        )
        for i, m in enumerate(trimmed):
            if m.role == "tool":
                assert any(
                    t.role == "assistant" and t.tool_calls for t in trimmed[:i]
                ), "orphaned tool result: cut separated call from result"

    def test_window_never_starts_on_tool_or_assistant(self):
        msgs = [_m("system")]
        for i in range(10):
            msgs.append(_m("user"))
            msgs.append(_m("assistant", tool_calls=[f"c{i}"]))
            msgs.append(_m("tool"))
            msgs.append(_m("assistant"))
        for cap in (5, 8, 13, 20):
            trimmed = lw.trim_chat_messages(msgs, cap)
            assert trimmed[0].role == "system"
            assert trimmed[1].role in ("user", "system")

    def test_hook_trims_in_place_after_reply(self):
        msgs = [_m("system")] + [_m("user"), _m("assistant")] * 50
        agent = SimpleNamespace(chat_ctx=SimpleNamespace(messages=msgs))
        hook = lw.ChatHistoryTrimHook(agent, 60)
        hook()  # agent_speech_committed fired
        assert len(agent.chat_ctx.messages) <= 61  # cap + boundary walk
        assert agent.chat_ctx.messages[0].role == "system"
        assert agent.chat_ctx.messages[1].role == "user"

    def test_hook_disabled_when_zero(self):
        msgs = [_m("system")] + [_m("user"), _m("assistant")] * 50
        agent = SimpleNamespace(chat_ctx=SimpleNamespace(messages=msgs))
        lw.ChatHistoryTrimHook(agent, 0)()
        assert len(agent.chat_ctx.messages) == 101

    def test_hook_never_raises_without_chat_ctx(self):
        lw.ChatHistoryTrimHook(SimpleNamespace(), 10)()  # no exception


# ---------------------------------------------------------------------------
# P9: per-stage latency metrics
# ---------------------------------------------------------------------------
class TestP9Metrics:
    def test_new_series_in_registry_and_render(self):
        registry = metrics.get_registry()
        assert registry.llm_first_token.name == "voice_llm_first_token_seconds"
        assert registry.tts_first_chunk.name == "voice_tts_first_chunk_seconds"
        assert registry.turn_total.name == "voice_turn_total_seconds"
        text = metrics.render()
        assert "# TYPE voice_llm_first_token_seconds histogram" in text
        assert "# TYPE voice_tts_first_chunk_seconds histogram" in text
        assert "# TYPE voice_turn_total_seconds histogram" in text

    def test_turn_total_hook(self):
        t = [100.0]
        hook = lw.TurnLatencyHook(clock=lambda: t[0])
        hook.on_agent_speaking()  # greeting without a user turn: ignored
        hook.on_user_committed()
        t[0] += 0.7
        hook.on_agent_speaking()
        hook.on_agent_speaking()  # already consumed: no double-observe
        text = metrics.render()
        assert "voice_turn_total_seconds_count 1" in text
        assert "voice_turn_total_seconds_sum 0.700000" in text

    async def test_tts_first_chunk_observed_on_stream(self):
        async def synth(text: str) -> bytes:
            await asyncio.sleep(0.01)
            return text.encode()

        piper = _piper_with_fake_synth(synth)
        async for _ in piper.stream_pcm("One. Two."):
            pass
        text = metrics.render()
        assert "voice_tts_first_chunk_seconds_count 1" in text
        # Cache-hit path also observes (near-zero time-to-first-chunk).
        async for _ in piper.stream_pcm("One. Two."):
            pass
        assert "voice_tts_first_chunk_seconds_count 2" in metrics.render()

    async def test_llm_first_token_observed_on_stream_path(self):
        class _StreamLLM:
            async def stream_with_tools(self, messages, tools):  # noqa: ARG002
                async def _gen():
                    yield SimpleNamespace(
                        choices=[
                            SimpleNamespace(
                                delta=SimpleNamespace(content="hi", tool_calls=None)
                            )
                        ],
                        usage=None,
                    )
                    yield SimpleNamespace(
                        choices=[
                            SimpleNamespace(
                                delta=SimpleNamespace(
                                    content=" there", tool_calls=None
                                )
                            )
                        ],
                        usage=None,
                    )

                return _gen()

        events = [
            ev
            async for ev in run_tool_loop_stream(
                _StreamLLM(), messages=[{"role": "user", "content": "hi"}],
                tools=[], dispatch=None,
            )
        ]
        assert events[-1]["type"] == "final"
        assert events[-1]["text"] == "hi there"
        assert "voice_llm_first_token_seconds_count 1" in metrics.render()
