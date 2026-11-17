"""SPEC-W70 tests: voice latency + Nigerian-language fluency optimization.

Covers:
- A1: VOICE_* endpointing/VAD/preemptive-synthesis knob parsing (defaults,
  env overrides, bad-value fallback) + the signature-introspection kwargs
  filter (stub VoicePipelineAgent / silero.VAD.load — livekit-agents 0.10.*
  is not installed in this env, per the suite's stub posture), including a
  regression pin built from the REAL livekit-agents 0.10.2 parameter names;
- A2: resolve_stt_model pure routing function, per-model-size whisper cache
  (lazy multilingual load, preload hook), condition_on_previous_text=False
  asserted on the transcribe call via a recording fake model;
- A3: SPOKEN STYLE block presence in build_system_prompt (all languages,
  before the locale instruction, <= 8 bullet lines);
- A4: pcm locale instruction emits natural Nigerian Pidgin (the pcm->en
  proxy still stands for STT/voice resolution — covered in test_multilang).
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from app.config import Settings, load_settings
from app.multilang import locale_instruction
from app.pipeline.stt import FasterWhisperSTT, resolve_stt_model
from app.prompts import SPOKEN_STYLE_BLOCK, build_system_prompt
from app.tenant_context import TenantContext

try:  # pragma: no cover - import shim (real SDK present in the worker image)
    import app.livekit_worker as lw
except ModuleNotFoundError:  # pragma: no cover - import shim
    import test_live_tts_chain  # noqa: F401 - installs the livekit stubs

    import app.livekit_worker as lw


def _ctx(**kw) -> TenantContext:
    base = dict(site_slug="acme", tenant_id="t1", tenant_slug="acme", locale="en-US")
    base.update(kw)
    return TenantContext(**base)


# ---------------------------------------------------------------------------
# A1: config knob parsing (defaults improve latency with zero config)
# ---------------------------------------------------------------------------
class TestVoiceKnobConfig:
    def test_defaults_improve_latency_out_of_the_box(self):
        s = Settings()
        assert s.voice_min_endpointing_delay == pytest.approx(0.3)
        assert s.voice_vad_min_silence_s == pytest.approx(0.35)
        assert s.voice_preemptive_synthesis is True
        # A2 knob is opt-in (empty = off).
        assert s.whisper_model_multilingual == ""

    def test_env_overrides(self, monkeypatch):
        monkeypatch.setenv("VOICE_MIN_ENDPOINTING_DELAY", "0.2")
        monkeypatch.setenv("VOICE_VAD_MIN_SILENCE_S", "0.5")
        monkeypatch.setenv("VOICE_PREEMPTIVE_SYNTHESIS", "false")
        monkeypatch.setenv("WHISPER_MODEL_MULTILINGUAL", "large-v3-turbo")
        s = load_settings()
        assert s.voice_min_endpointing_delay == pytest.approx(0.2)
        assert s.voice_vad_min_silence_s == pytest.approx(0.5)
        assert s.voice_preemptive_synthesis is False
        assert s.whisper_model_multilingual == "large-v3-turbo"

    def test_bad_values_fall_back_to_defaults(self, monkeypatch):
        # A typo in a latency knob must not crash the worker boot.
        monkeypatch.setenv("VOICE_MIN_ENDPOINTING_DELAY", "fast")
        monkeypatch.setenv("VOICE_VAD_MIN_SILENCE_S", "none")
        s = load_settings()
        assert s.voice_min_endpointing_delay == pytest.approx(0.3)
        assert s.voice_vad_min_silence_s == pytest.approx(0.35)


# ---------------------------------------------------------------------------
# A1: signature-introspection kwargs filter (stub classes, no livekit SDK)
# ---------------------------------------------------------------------------
class TestIntrospectionFilter:
    def setup_method(self):
        lw._DROPPED_KWARGS_LOGGED.clear()
        lw._APPLIED_KWARGS_LOGGED.clear()

    class _RealV0102VoicePipelineAgent:
        """Stub whose __init__ carries the REAL livekit-agents 0.10.2
        VoicePipelineAgent.__init__ parameter names, hardcoded from the
        downloaded wheel (livekit_agents-0.10.2-py3-none-any.whl,
        livekit/agents/pipeline/pipeline_agent.py). If the SDK pin changes
        or our tuning kwargs drift from the real signature, the
        all-knobs-land test below fails LOUDLY instead of the filter
        silently dropping a knob in production."""

        def __init__(
            self,
            *,
            vad,
            stt,
            llm,
            tts,
            chat_ctx=None,
            fnc_ctx=None,
            allow_interruptions=True,
            interrupt_speech_duration=0.5,
            interrupt_min_words=0,
            min_endpointing_delay=0.5,
            preemptive_synthesis=False,
            transcription=None,
            before_llm_cb=None,
            before_tts_cb=None,
            plotting=False,
            loop=None,
            will_synthesize_assistant_reply=None,
        ):
            self.min_endpointing_delay = min_endpointing_delay
            self.preemptive_synthesis = preemptive_synthesis

    def test_every_tuning_kwarg_lands_on_real_0_10_2_signature(self):
        # Regression pin (W70 fix): against the real 0.10.2 parameter names
        # every tuning kwarg we emit must LAND — none dropped.
        kwargs = lw._agent_tuning_kwargs(Settings())
        assert kwargs == {
            "min_endpointing_delay": 0.3,
            "preemptive_synthesis": True,
        }
        filtered = lw.filter_supported_kwargs(
            self._RealV0102VoicePipelineAgent.__init__,
            kwargs,
            label="RealStub",
        )
        assert filtered == kwargs  # nothing dropped on the pinned SDK
        assert lw._DROPPED_KWARGS_LOGGED == set()
        # ...and the agent is constructible with the applied kwargs.
        agent = self._RealV0102VoicePipelineAgent(
            vad=object(), stt=None, llm=None, tts=None, **filtered
        )
        assert agent.min_endpointing_delay == pytest.approx(0.3)
        assert agent.preemptive_synthesis is True

    class _StubVoicePipelineAgent:
        """Stub mirroring a livekit-agents build WITHOUT the newer knobs."""

        def __init__(self, *, vad, stt, llm, tts, min_endpointing_delay=0.5):
            self.vad = vad
            self.min_endpointing_delay = min_endpointing_delay

    def test_filter_keeps_supported_drops_rest(self):
        kwargs = lw._agent_tuning_kwargs(Settings())
        filtered = lw.filter_supported_kwargs(
            self._StubVoicePipelineAgent.__init__, kwargs, label="StubAgent"
        )
        # Only the knob the stub declares survives.
        assert filtered == {"min_endpointing_delay": 0.3}
        # ...and the stub agent is constructible with the filtered kwargs.
        agent = self._StubVoicePipelineAgent(
            vad=object(), stt=None, llm=None, tts=None, **filtered
        )
        assert agent.min_endpointing_delay == pytest.approx(0.3)

    def test_dropped_keys_logged_once(self):
        kwargs = lw._agent_tuning_kwargs(Settings())
        lw.filter_supported_kwargs(self._StubVoicePipelineAgent.__init__, kwargs, label="StubX")
        lw.filter_supported_kwargs(self._StubVoicePipelineAgent.__init__, kwargs, label="StubX")
        assert lw._DROPPED_KWARGS_LOGGED == {
            "StubX.preemptive_synthesis",
        }

    def test_applied_keys_logged_once(self):
        kwargs = lw._agent_tuning_kwargs(Settings())
        lw.filter_supported_kwargs(self._StubVoicePipelineAgent.__init__, kwargs, label="StubY")
        lw.filter_supported_kwargs(self._StubVoicePipelineAgent.__init__, kwargs, label="StubY")
        assert lw._APPLIED_KWARGS_LOGGED == {"StubY.__applied__"}

    def test_var_kwargs_callable_accepts_everything(self):
        def _catch_all(**kwargs):
            return kwargs

        kwargs = lw._agent_tuning_kwargs(Settings())
        assert lw.filter_supported_kwargs(_catch_all, kwargs, label="any") == kwargs

    def test_unintrospectable_callable_passes_through(self):
        class _NoSignature:
            @property
            def __signature__(self):
                raise ValueError("no signature available")

            def __call__(self, **kwargs):
                return kwargs

        # A callable whose signature cannot be introspected gets the kwargs
        # passed through with a warning (worker stays bootable).
        kwargs = {"min_silence_duration": 0.35}
        out = lw.filter_supported_kwargs(_NoSignature(), kwargs, label="nosig")
        assert out == kwargs

    def test_load_vad_uses_tuned_silence(self, monkeypatch):
        # SPEC-W71 P1 (sanctioned update): _load_vad is now cached
        # process-wide; clear the cache so this test observes its own load.
        lw._vad_cache_clear()
        calls = []

        class _VAD:
            @staticmethod
            def load(*, min_silence_duration=0.55):
                calls.append(min_silence_duration)
                return object()

        monkeypatch.setattr(lw, "silero", SimpleNamespace(VAD=_VAD))
        lw._load_vad(Settings(voice_vad_min_silence_s=0.35))
        assert calls == [0.35]

    def test_load_vad_drops_knob_when_signature_lacks_it(self, monkeypatch):
        # SPEC-W71 P1 (sanctioned update): see above.
        lw._vad_cache_clear()
        calls = []

        class _VAD:
            @staticmethod
            def load():  # older stub: no tuning params
                calls.append(())
                return object()

        monkeypatch.setattr(lw, "silero", SimpleNamespace(VAD=_VAD))
        lw._load_vad(Settings())  # must not raise
        assert calls == [()]


# ---------------------------------------------------------------------------
# A2: whisper model routing (pure function) + per-model cache
# ---------------------------------------------------------------------------
class TestResolveSttModel:
    def test_multilingual_unset_always_default(self):
        assert resolve_stt_model("yo", "ha", "base", "") == "base"
        assert resolve_stt_model(None, None, "base", None) == "base"

    def test_english_stays_on_default(self):
        assert resolve_stt_model("en", None, "base", "large-v3-turbo") == "base"
        assert resolve_stt_model(None, "en", "base", "large-v3-turbo") == "base"
        # Unknown/empty effective language -> default.
        assert resolve_stt_model(None, None, "base", "large-v3-turbo") == "base"

    def test_non_english_routes_to_multilingual(self):
        # Forced language wins over the last detection.
        assert resolve_stt_model("yo", None, "base", "large-v3-turbo") == "large-v3-turbo"
        assert resolve_stt_model("yo", "en", "base", "large-v3-turbo") == "large-v3-turbo"
        # No forced language: the last auto-detection decides.
        assert resolve_stt_model(None, "ha", "base", "large-v3-turbo") == "large-v3-turbo"
        assert resolve_stt_model(None, "ig", "base", "large-v3-turbo") == "large-v3-turbo"

    def test_pidgin_proxies_to_english_stays_on_default(self):
        # pcm has no whisper model; the pcm->en proxy applies to STT routing.
        assert resolve_stt_model("pcm", None, "base", "large-v3-turbo") == "base"
        assert resolve_stt_model(None, "pcm", "base", "large-v3-turbo") == "base"


class _RecordingModel:
    """Fake whisper model recording transcribe kwargs."""

    def __init__(self, language: str = "en") -> None:
        self.calls: list[dict] = []
        self._language = language

    def transcribe(self, audio, **kwargs):  # noqa: ARG002
        self.calls.append(kwargs)
        return [SimpleNamespace(text=" wetin dey happen ")], SimpleNamespace(
            language=self._language
        )


PCM_1S = b"\x00\x00" * 16000  # 1s of 16 kHz s16le


class TestMultilingualSttCache:
    async def test_condition_on_previous_text_hard_set_false(self):
        stt = FasterWhisperSTT(model_size="base")
        fake = _RecordingModel()
        stt._model = fake  # skip the real whisper load (test_metrics pattern)
        text = await stt.transcribe_pcm(PCM_1S, sample_rate=16000)
        assert text == "wetin dey happen"
        assert fake.calls[0]["condition_on_previous_text"] is False

    async def test_lazy_load_per_model_size(self, monkeypatch):
        stt = FasterWhisperSTT(model_size="base", multilingual_model="large-v3-turbo")
        loaded: list[str] = []
        monkeypatch.setattr(
            stt,
            "_load_sync",
            lambda size: loaded.append(size) or _RecordingModel(),
        )
        # English turn: only the default model loads.
        await stt.transcribe_pcm(PCM_1S, sample_rate=16000, language="en")
        assert loaded == ["base"]
        # First non-English turn lazy-loads the multilingual model.
        await stt.transcribe_pcm(PCM_1S, sample_rate=16000, language="yo")
        assert loaded == ["base", "large-v3-turbo"]
        # Cached: no reload on subsequent turns of either language.
        await stt.transcribe_pcm(PCM_1S, sample_rate=16000, language="yo")
        await stt.transcribe_pcm(PCM_1S, sample_rate=16000, language="en")
        assert loaded == ["base", "large-v3-turbo"]

    async def test_detection_drives_next_turn_routing(self, monkeypatch):
        stt = FasterWhisperSTT(model_size="base", multilingual_model="large-v3-turbo")
        loaded: list[str] = []
        monkeypatch.setattr(
            stt,
            "_load_sync",
            lambda size: loaded.append(size) or _RecordingModel(language="yo"),
        )
        # First turn (no detection yet) uses the default model; whisper
        # auto-detects Yoruba...
        await stt.transcribe_pcm(PCM_1S, sample_rate=16000)
        assert stt.last_detected_language == "yo"
        # ...so the NEXT turn routes to the multilingual model.
        await stt.transcribe_pcm(PCM_1S, sample_rate=16000)
        assert loaded == ["base", "large-v3-turbo"]

    def test_preload_loads_multilingual_only_when_knob_set(self, monkeypatch):
        loaded: list[str] = []

        def _fake_load(size):
            loaded.append(size)
            return object()

        stt = FasterWhisperSTT(model_size="base", multilingual_model="large-v3-turbo")
        monkeypatch.setattr(stt, "_load_sync", _fake_load)
        stt.preload_sync()
        assert loaded == ["base", "large-v3-turbo"]

        loaded.clear()
        stt2 = FasterWhisperSTT(model_size="base")  # knob unset
        monkeypatch.setattr(stt2, "_load_sync", _fake_load)
        stt2.preload_sync()
        assert loaded == ["base"]


# ---------------------------------------------------------------------------
# A3: SPOKEN STYLE block in the system prompt
# ---------------------------------------------------------------------------
class TestSpokenStyleBlock:
    def test_block_present_for_all_languages(self):
        prompt = build_system_prompt(_ctx(), conversation_id="c1")
        assert "SPOKEN STYLE" in prompt
        assert "naira" in prompt
        assert "no markdown" in prompt
        # Non-default language turn: block still present, BEFORE the locale
        # instruction (the language rule lands last).
        prompt = build_system_prompt(_ctx(), conversation_id="c1", language="yo")
        assert "SPOKEN STYLE" in prompt
        assert prompt.index("SPOKEN STYLE") < prompt.index("LANGUAGE (this turn)")

    def test_block_is_compact(self):
        bullets = [
            line for line in SPOKEN_STYLE_BLOCK.splitlines() if line.startswith("- ")
        ]
        assert 0 < len(bullets) <= 8


# ---------------------------------------------------------------------------
# A4: pcm locale instruction (see also test_multilang.TestPidgin)
# ---------------------------------------------------------------------------
class TestPidginLocaleInstruction:
    def test_pcm_instruction_is_natural_pidgin(self):
        instruction = locale_instruction("pcm")
        assert "Nigerian Pidgin" in instruction
        assert "keep tool arguments" in instruction

    def test_prompt_carries_pidgin_instruction(self):
        ctx = _ctx(locale="en-NG")
        prompt = build_system_prompt(ctx, conversation_id="c1", language="pcm")
        assert "The caller is speaking Nigerian Pidgin" in prompt
