"""SPEC-W10 final integration: the LIVE call path (app/livekit_worker.py)
builds and drives the FallbackTTS provider chain instead of a direct
PiperTTS.

Covered here:
(a) default config (TTS_PROVIDER_CHAIN="piper", TTS_VOICE_MAP unset) builds a
    piper-only chain that is byte-identical to the legacy PiperTTS path
    (construction params, voice resolution, PCM bytes, sample rate);
(b) the live path (build_voice_agent) wires a FallbackTTS built from the
    configured chain into the LiveKit TTS node;
(c) whisper pcm/yo detection routes to the provider-qualified TTS_VOICE_MAP
    voice on the live path, with chain fallback to piper on provider failure,
    while the locale/prompt behavior (pcm -> en proxy) stays unchanged;
(d) prewarming exercises the first configured provider AND piper.

The LiveKit Agents SDK is optional in the unit-test env; when absent, the
modules app.livekit_worker imports are stubbed (same posture as
tests/conftest.py's livekit.api stub) so the worker wiring is testable.
"""

from __future__ import annotations

import io
import json
import sys
import types
import wave
from types import SimpleNamespace

import httpx
import pytest


def _install_livekit_stubs() -> None:
    """Minimal stand-ins for the livekit SDK surface app.livekit_worker
    imports. Only used when the real SDK is not installed."""

    class _FunctionContext:
        def __init__(self, *a, **k) -> None:
            pass

    def _ai_callable(**_kw):
        def _deco(fn):
            return fn

        return _deco

    class _ChatMessage:
        def __init__(self, role: str, text: str) -> None:
            self.role = role
            self.content = [text]

    class _ChatContext:
        def __init__(self) -> None:
            self.messages: list = []

        def append(self, *, role: str, text: str):
            self.messages.append(_ChatMessage(role, text))
            return self

    class _STTCapabilities:
        def __init__(self, *, streaming: bool, interim_results: bool) -> None:
            self.streaming = streaming
            self.interim_results = interim_results

    class _STT:
        def __init__(self, *, capabilities) -> None:
            self.capabilities = capabilities

    class _SpeechEventType:
        FINAL_TRANSCRIPT = "final_transcript"

    class _SpeechData:
        def __init__(self, *, text: str, language: str) -> None:
            self.text = text
            self.language = language

    class _SpeechEvent:
        def __init__(self, *, type, alternatives) -> None:  # noqa: A002
            self.type = type
            self.alternatives = alternatives

    class _TTSCapabilities:
        def __init__(self, *, streaming: bool) -> None:
            self.streaming = streaming

    class _TTS:
        def __init__(self, *, capabilities, sample_rate: int, num_channels: int) -> None:
            self.capabilities = capabilities
            self.sample_rate = sample_rate
            self.num_channels = num_channels

    class _SynthesizedAudio:
        def __init__(self, *, request_id: str, segment_id: str, frame) -> None:
            self.request_id = request_id
            self.segment_id = segment_id
            self.frame = frame

    class _AudioFrame:
        def __init__(self, *, data, sample_rate, num_channels, samples_per_channel) -> None:
            self.data = data
            self.sample_rate = sample_rate
            self.num_channels = num_channels
            self.samples_per_channel = samples_per_channel

    class _VoicePipelineAgent:
        def __init__(self, **kwargs) -> None:
            self.kwargs = kwargs

        @property
        def chat_ctx(self):
            return self.kwargs["chat_ctx"]

        async def say(self, text, allow_interruptions=False):  # noqa: ARG002
            return None

        def start(self, room) -> None:  # noqa: ARG002
            pass

        def on(self, event, cb=None):  # noqa: ARG002
            pass

    class _WorkerOptions:
        def __init__(self, **kwargs) -> None:
            self.__dict__.update(kwargs)

    llm_mod = types.ModuleType("livekit.agents.llm")
    llm_mod.FunctionContext = _FunctionContext
    llm_mod.ai_callable = _ai_callable
    llm_mod.ChatContext = _ChatContext

    stt_mod = types.ModuleType("livekit.agents.stt")
    stt_mod.STT = _STT
    stt_mod.STTCapabilities = _STTCapabilities
    stt_mod.SpeechEvent = _SpeechEvent
    stt_mod.SpeechEventType = _SpeechEventType
    stt_mod.SpeechData = _SpeechData

    tts_mod = types.ModuleType("livekit.agents.tts")
    tts_mod.TTS = _TTS
    tts_mod.TTSCapabilities = _TTSCapabilities
    tts_mod.SynthesizedAudio = _SynthesizedAudio

    utils_mod = types.ModuleType("livekit.agents.utils")
    utils_mod.AudioBuffer = list
    utils_mod.merge_frames = lambda buffer: buffer

    pipeline_mod = types.ModuleType("livekit.agents.pipeline")
    pipeline_mod.VoicePipelineAgent = _VoicePipelineAgent

    agents_mod = types.ModuleType("livekit.agents")
    agents_mod.llm = llm_mod
    agents_mod.stt = stt_mod
    agents_mod.tts = tts_mod
    agents_mod.utils = utils_mod
    agents_mod.pipeline = pipeline_mod
    agents_mod.WorkerOptions = _WorkerOptions
    agents_mod.JobContext = object
    agents_mod.cli = SimpleNamespace(run_app=lambda opts: None)

    rtc_mod = types.ModuleType("livekit.rtc")
    rtc_mod.AudioFrame = _AudioFrame

    openai_mod = types.ModuleType("livekit.plugins.openai")
    openai_mod.LLM = lambda **kw: SimpleNamespace(**kw)

    silero_mod = types.ModuleType("livekit.plugins.silero")
    silero_mod.VAD = SimpleNamespace(load=lambda **kw: object())

    plugins_mod = types.ModuleType("livekit.plugins")
    plugins_mod.openai = openai_mod
    plugins_mod.silero = silero_mod

    livekit_mod = types.ModuleType("livekit")
    livekit_mod.agents = agents_mod
    livekit_mod.rtc = rtc_mod
    livekit_mod.plugins = plugins_mod

    for name, mod in {
        "livekit": livekit_mod,
        "livekit.agents": agents_mod,
        "livekit.agents.llm": llm_mod,
        "livekit.agents.stt": stt_mod,
        "livekit.agents.tts": tts_mod,
        "livekit.agents.utils": utils_mod,
        "livekit.agents.pipeline": pipeline_mod,
        "livekit.rtc": rtc_mod,
        "livekit.plugins": plugins_mod,
        "livekit.plugins.openai": openai_mod,
        "livekit.plugins.silero": silero_mod,
    }.items():
        sys.modules.setdefault(name, mod)


try:  # pragma: no cover - import shim (real SDK present in the worker image)
    import app.livekit_worker as lw
except ModuleNotFoundError:  # pragma: no cover - import shim
    _install_livekit_stubs()
    import app.livekit_worker as lw

from app import metrics  # noqa: E402
from app.config import Settings  # noqa: E402
from app.multilang import resolve_tts_voice, voice_for_language  # noqa: E402
from app.pipeline.tts import PiperTTS  # noqa: E402
from app.tenant_context import TenantContext  # noqa: E402
from app.tts_providers.base import Voice  # noqa: E402
from app.tts_providers.chain import FallbackTTS  # noqa: E402
from conftest import FakeDapr  # noqa: E402


def _wav(rate: int = 22050, frames: int = 64) -> bytes:
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)
        wf.setframerate(rate)
        wf.writeframes(b"\x01\x00" * frames)
    return buf.getvalue()


WAV = _wav()
PCM = b"\x01\x00" * 64


class FakeProvider:
    """Scripted TTSProvider double (same pattern as tests/test_tts_providers)."""

    def __init__(self, name: str, outcomes=None, default_voice: str = "dv"):
        self.name = name
        self.default_voice = default_voice
        self.outcomes = list(outcomes or [WAV])
        self.calls: list[tuple[str, str, str]] = []
        self._voices = [Voice(id=default_voice, languages=["en"])]

    async def synthesize(self, text: str, voice: str, language: str) -> bytes:
        self.calls.append((text, voice, language))
        outcome = self.outcomes.pop(0) if self.outcomes else WAV
        if isinstance(outcome, Exception):
            raise outcome
        return outcome

    async def list_voices(self):
        return list(self._voices)

    async def available(self) -> bool:
        return True

    async def aclose(self) -> None:
        pass


class _FakeAgent:
    """VoicePipelineAgent double: records constructor kwargs."""

    def __init__(self, **kwargs) -> None:
        self.kwargs = kwargs
        self.said: list[str] = []

    @property
    def chat_ctx(self):
        return self.kwargs["chat_ctx"]

    async def say(self, text, allow_interruptions=False):  # noqa: ARG002
        self.said.append(text)

    def start(self, room) -> None:  # noqa: ARG002
        pass

    def on(self, event, cb=None):  # noqa: ARG002
        pass


@pytest.fixture(autouse=True)
def _clean_prewarm_cache():
    lw._PREWARMED.clear()
    yield
    lw._PREWARMED.clear()


def _ctx(**over) -> TenantContext:
    base = dict(
        site_slug="acme",
        tenant_id="00000000-0000-0000-0000-000000000001",
        tenant_slug="acme",
        display_name="Acme",
        locale="en-US",
        languages=["en", "pcm"],
    )
    base.update(over)
    return TenantContext(**base)


async def _build_agent(monkeypatch, settings: Settings, ctx: TenantContext, tts_impl):
    """Run build_voice_agent with the heavy LiveKit nodes faked out; returns
    (agent_kwargs, session). The TTS impl under test is prewarmed-seeded."""
    lw._PREWARMED["tts"] = tts_impl

    async def _fake_fetch(dapr, settings_, site_slug):  # noqa: ARG001
        return ctx

    monkeypatch.setattr(lw, "fetch_tenant_context", _fake_fetch)
    monkeypatch.setattr(lw, "VoicePipelineAgent", _FakeAgent)
    monkeypatch.setattr(
        lw, "silero", SimpleNamespace(VAD=SimpleNamespace(load=lambda **kw: object()))
    )
    monkeypatch.setattr(
        lw, "lk_openai", SimpleNamespace(LLM=lambda **kw: SimpleNamespace(**kw))
    )
    agent, _tenant_ctx, session = await lw.build_voice_agent(
        settings, FakeDapr(), ctx.site_slug, "conv-1"
    )
    return agent.kwargs, session


# ---------------------------------------------------------------------------
# (a) default config -> piper-only chain, byte-identical to legacy PiperTTS
# ---------------------------------------------------------------------------
def test_default_build_tts_is_piper_only_chain():
    settings = Settings()
    assert settings.tts_provider_chain == "piper"
    assert settings.tts_voice_map == {}
    chain = lw._build_tts(settings)
    assert isinstance(chain, FallbackTTS)
    assert chain.order == ["piper"]
    assert chain.sample_rate == settings.piper_sample_rate == 22050


def test_default_chain_piper_stage_matches_legacy_construction():
    settings = Settings()
    chain = lw._build_tts(settings)
    legacy = PiperTTS(
        mode=settings.piper_mode,
        http_url=settings.piper_http_url,
        voice=settings.piper_voice,
        piper_bin=settings.piper_bin,
        model_dir=settings.piper_model_dir,
        sample_rate=settings.piper_sample_rate,
    )
    inner = chain.provider("piper")._piper
    for attr in ("mode", "http_url", "voice", "piper_bin", "model_dir", "sample_rate"):
        assert getattr(inner, attr) == getattr(legacy, attr), attr


def test_default_voice_resolution_identical_to_legacy():
    settings = Settings()
    for lang in ("en", "es", "yo", "pcm", "en-NG", ""):
        assert resolve_tts_voice(
            lang, settings.tts_voice_map, settings.piper_voice_map, settings.piper_voice
        ) == voice_for_language(lang, settings.piper_voice_map, settings.piper_voice)
    # ... also with a legacy PIPER_VOICE_MAP configured.
    mapped = Settings(piper_voice_map={"es": "es_ES-sharvard-medium"})
    for lang in ("en", "es", "pcm"):
        assert resolve_tts_voice(
            lang, mapped.tts_voice_map, mapped.piper_voice_map, mapped.piper_voice
        ) == voice_for_language(lang, mapped.piper_voice_map, mapped.piper_voice)


async def test_default_chain_pcm_bytes_and_requests_identical():
    """Same wire payload + same PCM out of the chain as legacy PiperTTS."""
    metrics.reset_registry()
    requests: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(json.loads(request.content))
        return httpx.Response(200, content=WAV, headers={"content-type": "audio/wav"})

    settings = Settings()
    legacy = PiperTTS(
        mode=settings.piper_mode,
        http_url=settings.piper_http_url,
        voice=settings.piper_voice,
        piper_bin=settings.piper_bin,
        model_dir=settings.piper_model_dir,
        sample_rate=settings.piper_sample_rate,
    )
    chain = lw._build_tts(settings)
    legacy._client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    chain.provider("piper")._piper._client = httpx.AsyncClient(
        transport=httpx.MockTransport(handler)
    )
    try:
        assert await legacy.synthesize_pcm("hello there") == PCM
        assert await chain.synthesize_pcm("hello there") == PCM
        assert requests[0] == requests[1] == {
            "text": "hello there",
            "voice": settings.piper_voice,
        }
        # Per-language voice swap (the multilang hook sets `.voice`).
        legacy.voice = "en_GB-alan-medium"
        chain.voice = "en_GB-alan-medium"
        assert await legacy.synthesize_pcm("second turn") == PCM
        assert await chain.synthesize_pcm("second turn") == PCM
        assert requests[2] == requests[3] == {
            "text": "second turn",
            "voice": "en_GB-alan-medium",
        }
    finally:
        await legacy.aclose()
        await chain.aclose()


async def test_live_path_initial_voice_uses_legacy_piper_map(monkeypatch):
    """Tenant locale es-ES + PIPER_VOICE_MAP -> same voice the legacy path set."""
    settings = Settings(piper_voice_map={"es": "es_ES-sharvard-medium"})
    chain = FallbackTTS(
        {"piper": FakeProvider("piper", default_voice=settings.piper_voice)},
        ["piper"],
        sample_rate=settings.piper_sample_rate,
    )
    kwargs, session = await _build_agent(
        monkeypatch, settings, _ctx(locale="es-ES", languages=["en", "es"]), chain
    )
    assert kwargs["tts"]._impl is chain
    assert chain.voice == "es_ES-sharvard-medium"
    assert session.active_language == "es"


# ---------------------------------------------------------------------------
# (b) live path uses FallbackTTS with the configured chain
# ---------------------------------------------------------------------------
def test_build_tts_configured_chain():
    settings = Settings(
        tts_provider_chain="mms,piper", tts_voice_map={"pcm": "mms:pcm"}
    )
    chain = lw._build_tts(settings)
    assert isinstance(chain, FallbackTTS)
    assert chain.order == ["mms", "piper"]
    assert set(chain.providers) == {"mms", "piper"}
    assert chain.sample_rate == settings.piper_sample_rate


async def test_live_agent_tts_node_wraps_fallback_chain(monkeypatch):
    settings = Settings(
        tts_provider_chain="mms,piper", tts_voice_map={"pcm": "mms:pcm"}
    )
    chain = FallbackTTS(
        {
            "mms": FakeProvider("mms", default_voice="pcm"),
            "piper": FakeProvider("piper", default_voice=settings.piper_voice),
        },
        ["mms", "piper"],
        sample_rate=settings.piper_sample_rate,
    )
    kwargs, _session = await _build_agent(monkeypatch, settings, _ctx(), chain)
    node = kwargs["tts"]
    assert isinstance(node._impl, FallbackTTS)
    assert node._impl is chain
    assert node.sample_rate == settings.piper_sample_rate
    # No map entry for the default language -> default piper voice initially.
    assert chain.voice == settings.piper_voice


# ---------------------------------------------------------------------------
# (c) pcm/yo detection -> provider-qualified voice on the live path
# ---------------------------------------------------------------------------
async def test_pcm_detection_routes_mms_voice_with_chain_fallback(monkeypatch):
    metrics.reset_registry()
    settings = Settings(
        tts_provider_chain="mms,piper",
        tts_voice_map={"pcm": "mms:pcm", "yo": "mms:yor"},
    )
    mms = FakeProvider("mms", [ConnectionError("mms down")], default_voice="pcm")
    piper = FakeProvider("piper", default_voice=settings.piper_voice)
    chain = FallbackTTS(
        {"mms": mms, "piper": piper},
        ["mms", "piper"],
        sample_rate=settings.piper_sample_rate,
    )
    kwargs, session = await _build_agent(monkeypatch, settings, _ctx(), chain)

    on_language = kwargs["stt"]._on_language
    on_language("pcm")

    # Voice follows the RAW detection (provider-qualified map)...
    assert chain.voice == "mms:pcm"
    # ...while the locale/prompt behavior is unchanged: pcm stays proxied to
    # English (no locale switch, no prompt re-render).
    assert session.active_language == "en"

    pcm = await chain.synthesize_pcm("sannu, how you dey?")
    assert pcm == PCM
    # mms pinned first with the qualified voice id; piper fell back with ITS
    # default voice after the mms failure.
    assert mms.calls == [("sannu, how you dey?", "pcm", "")]
    assert piper.calls == [("sannu, how you dey?", settings.piper_voice, "")]
    rendered = metrics.get_registry().render()
    assert 'tts_provider_failures_total{provider="mms"} 1' in rendered

    # yo detection routes the same way (raw-detection routing).
    on_language("yo")
    assert chain.voice == "mms:yor"


async def test_default_config_detection_keeps_legacy_voice_swap(monkeypatch):
    """TTS_VOICE_MAP unset: detection switches voice exactly like pre-W10."""
    settings = Settings(piper_voice_map={"es": "es_ES-sharvard-medium"})
    chain = FallbackTTS(
        {"piper": FakeProvider("piper", default_voice=settings.piper_voice)},
        ["piper"],
        sample_rate=settings.piper_sample_rate,
    )
    kwargs, session = await _build_agent(
        monkeypatch, settings, _ctx(languages=["en", "es"]), chain
    )
    assert chain.voice == settings.piper_voice
    on_language = kwargs["stt"]._on_language
    on_language("es")
    assert session.active_language == "es"
    assert chain.voice == "es_ES-sharvard-medium"
    # pcm detection with no map: locale proxied to en (a switch from es) and
    # the voice falls back to the default piper voice — unchanged legacy
    # behavior (voice_for_language pidgin proxy).
    on_language("pcm")
    assert session.active_language == "en"
    assert chain.voice == settings.piper_voice


# ---------------------------------------------------------------------------
# (d) prewarming: first configured provider AND piper
# ---------------------------------------------------------------------------
async def test_warmup_tts_piper_only_single_pass():
    """Default chain: exactly one synthesize pass (legacy piper warmup)."""
    piper = FakeProvider("piper", default_voice="en_US-lessac-medium")
    chain = FallbackTTS({"piper": piper}, ["piper"], sample_rate=22050)
    await lw._warmup_tts(chain)
    assert [c[0] for c in piper.calls] == [lw.PREWARM_PHRASE]


async def test_warmup_tts_first_provider_and_piper():
    """Multi-provider chain: warmup exercises the first provider AND piper."""
    mms = FakeProvider("mms", default_voice="pcm")
    piper = FakeProvider("piper", default_voice="en_US-lessac-medium")
    chain = FallbackTTS(
        {"mms": mms, "piper": piper}, ["mms", "piper"], sample_rate=22050
    )
    await lw._warmup_tts(chain)
    assert [c[0] for c in mms.calls] == [lw.PREWARM_PHRASE]
    assert [c[0] for c in piper.calls] == [lw.PREWARM_PHRASE]


def test_prewarm_fnc_warms_chain_and_caches(monkeypatch):
    """WorkerOptions.prewarm_fnc end-to-end with a chain (preload preserved)."""

    class _FakeSTT:
        def __init__(self) -> None:
            self.loaded = False

        def preload_sync(self) -> None:
            self.loaded = True

    mms = FakeProvider("mms", default_voice="pcm")
    piper = FakeProvider("piper", default_voice="en_US-lessac-medium")
    chain = FallbackTTS(
        {"mms": mms, "piper": piper}, ["mms", "piper"], sample_rate=22050
    )
    fake_stt = _FakeSTT()
    monkeypatch.setattr(lw, "_build_stt", lambda settings: fake_stt)
    monkeypatch.setattr(lw, "_build_tts", lambda settings: chain)

    prewarm = lw.make_prewarm_fnc(
        Settings(preload_models=True, tts_provider_chain="mms,piper")
    )
    prewarm(object())

    assert fake_stt.loaded
    assert [c[0] for c in mms.calls] == [lw.PREWARM_PHRASE]
    assert [c[0] for c in piper.calls] == [lw.PREWARM_PHRASE]
    assert lw._PREWARMED["stt"] is fake_stt
    assert lw._PREWARMED["tts"] is chain


def test_prewarm_fnc_piper_only_default(monkeypatch):
    """Default chain prewarm: one piper pass, identical to legacy warmup."""
    piper = FakeProvider("piper", default_voice="en_US-lessac-medium")
    chain = FallbackTTS({"piper": piper}, ["piper"], sample_rate=22050)
    monkeypatch.setattr(lw, "_build_stt", lambda settings: object())
    monkeypatch.setattr(lw, "_build_tts", lambda settings: chain)

    prewarm = lw.make_prewarm_fnc(Settings(preload_models=True))
    prewarm(object())

    assert [c[0] for c in piper.calls] == [lw.PREWARM_PHRASE]
    assert lw._PREWARMED["tts"] is chain
