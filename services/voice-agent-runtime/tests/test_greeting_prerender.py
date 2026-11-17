"""SPEC-W73 A2 tests: pre-rendered greetings (app/greeting_prerender.py).

Proofs (f)-(i) from SPEC-W73 A4, all offline-deterministic — real FallbackTTS
chain (the one carrying the TtsLruCache) over scripted fake providers; no
LiveKit server, no models, no network:

(f) prerender -> session agent.say(greeting) performs NO synthesis
    (spy on the provider's synthesize; cache-hit proof through the real
    PiperTTSNode session path);
(g) one tenant failing prerender -> others still warm, metric labelled
    error;
(h) disabled -> zero synth calls (and empty tenant list is a no-op);
(i) boot timeout path proceeds with the partial warm;
plus: voice-key identity (per-tenant session voice), concurrency bound,
text collection via the real build_greeting seam, env parsing, and the
prewarm_fnc wiring (spawned job processes warm their own chain).
"""

from __future__ import annotations

import asyncio
import io
import time
import wave

import pytest

from app import greeting_prerender, metrics
from app.config import Settings, load_settings
from app.greeting_prerender import (
    boot_prerender,
    collect_greeting_texts,
    parse_tenant_slugs,
    prerender_greetings,
    session_greeting_voice,
)
from app.prompts import build_greeting
from app.tenant_context import TenantContext
from app.tts_providers.base import Voice
from app.tts_providers.chain import FallbackTTS

try:  # pragma: no cover - import shim (real SDK present in the worker image)
    import app.livekit_worker as lw
except ModuleNotFoundError:  # pragma: no cover - import shim
    import test_live_tts_chain  # noqa: F401 - installs the livekit stubs

    import app.livekit_worker as lw


@pytest.fixture(autouse=True)
def _clean_state():
    metrics.reset_registry()
    lw._PREWARMED.clear()
    yield
    metrics.reset_registry()
    lw._PREWARMED.clear()


def _wav_bytes(rate: int = 22050, frames: int = 64) -> bytes:
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)
        wf.setframerate(rate)
        wf.writeframes(b"\x01\x00" * frames)
    return buf.getvalue()


WAV = _wav_bytes()


class SpyProvider:
    """Scripted TTSProvider double with a synthesis spy + optional latency."""

    def __init__(
        self,
        name: str = "piper",
        default_voice: str = "en_US-lessac-medium",
        fail_on: str = "",
        delay_s: float = 0.0,
    ) -> None:
        self.name = name
        self.default_voice = default_voice
        self.fail_on = fail_on
        self.delay_s = delay_s
        self.calls: list[tuple[str, str, str]] = []
        self.in_flight = 0
        self.max_in_flight = 0

    async def synthesize(self, text: str, voice: str, language: str) -> bytes:
        self.calls.append((text, voice, language))
        self.in_flight += 1
        self.max_in_flight = max(self.max_in_flight, self.in_flight)
        try:
            if self.delay_s:
                await asyncio.sleep(self.delay_s)
            if self.fail_on and self.fail_on in text:
                raise RuntimeError("synth exploded (scripted)")
            return WAV
        finally:
            self.in_flight -= 1

    async def list_voices(self):
        return [Voice(id=self.default_voice, languages=["en"])]

    async def available(self) -> bool:
        return True

    async def aclose(self) -> None:
        pass


def _ctx(slug: str, name: str, **over) -> TenantContext:
    base = dict(
        site_slug=slug,
        tenant_id=f"t-{slug}",
        tenant_slug=slug,
        display_name=name,
        locale="en-US",
    )
    base.update(over)
    return TenantContext(**base)


def _rendered() -> str:
    return metrics.render()


# ---------------------------------------------------------------------------
# (f) prerender -> session agent.say(greeting) performs NO synthesis
# ---------------------------------------------------------------------------
async def test_f_prerendered_greeting_is_cache_hit_on_session_path():
    settings = Settings()
    provider = SpyProvider()
    chain = FallbackTTS({"piper": provider}, ["piper"], sample_rate=22050)
    chain.voice = "en_US-lessac-medium"  # build_voice_agent sets this
    greeting = build_greeting(_ctx("acme", "Acme"))

    warmed = await prerender_greetings(
        settings,
        lambda: chain,
        {"acme": greeting},
        tenant_voices={"acme": "en_US-lessac-medium"},
    )
    assert warmed is chain
    assert len(provider.calls) == 1  # the prerender synthesis itself
    assert 'voice_greeting_prerender_total{result="ok"} 1' in _rendered()

    # Session path: agent.say(build_greeting(ctx)) -> PiperTTSNode._stream ->
    # chain.stream_pcm -> full-utterance cache hit, ZERO provider synthesis.
    node = lw.PiperTTSNode(warmed, chunked=True)
    frames = [audio async for audio in node.synthesize(build_greeting(_ctx("acme", "Acme")))]
    assert frames  # audio still ships
    assert len(provider.calls) == 1  # cache-hit proof: NO new synthesis


async def test_f_voice_mismatch_is_cache_miss_key_identity():
    """The prerendered key is (voice, text, format): a different voice must
    NOT hit (guards the cache-key identity requirement)."""
    settings = Settings()
    provider = SpyProvider()
    chain = FallbackTTS({"piper": provider}, ["piper"], sample_rate=22050)
    chain.voice = "en_US-lessac-medium"
    await prerender_greetings(
        settings, lambda: chain, {"acme": "Hello there."},
        tenant_voices={"acme": "en_US-lessac-medium"},
    )
    assert len(provider.calls) == 1
    chain.voice = "en_GB-alba-medium"  # a different session voice
    await chain.synthesize_pcm("Hello there.")
    assert len(provider.calls) == 2  # miss: voice is part of the key


# ---------------------------------------------------------------------------
# (g) one tenant failing prerender -> others still warm, metric labelled error
# ---------------------------------------------------------------------------
async def test_g_one_tenant_failure_is_nonfatal_others_stay_warm():
    settings = Settings()
    provider = SpyProvider(fail_on="Broken")
    chain = FallbackTTS({"piper": provider}, ["piper"], sample_rate=22050)
    chain.voice = "en_US-lessac-medium"

    warmed = await prerender_greetings(
        settings,
        lambda: chain,
        {"acme": build_greeting(_ctx("acme", "Acme")),
         "broken": build_greeting(_ctx("broken", "Broken"))},
    )
    rendered = _rendered()
    assert 'voice_greeting_prerender_total{result="ok"} 1' in rendered
    assert 'voice_greeting_prerender_total{result="error"} 1' in rendered

    # The healthy tenant is a cache hit; the failed one synthesizes cold.
    calls_after = len(provider.calls)
    await chain.synthesize_pcm(build_greeting(_ctx("acme", "Acme")))
    assert len(provider.calls) == calls_after  # warm
    provider.fail_on = ""  # provider recovers
    await chain.synthesize_pcm(build_greeting(_ctx("broken", "Broken")))
    assert len(provider.calls) == calls_after + 1  # cold, today's behavior


# ---------------------------------------------------------------------------
# (h) disabled -> zero synth calls; empty tenant list -> no-op
# ---------------------------------------------------------------------------
def test_h_disabled_is_zero_synth():
    provider = SpyProvider()
    chain = FallbackTTS({"piper": provider}, ["piper"], sample_rate=22050)
    settings = Settings(
        voice_greeting_prerender_enabled=False,
        voice_greeting_prerender_tenants="acme",
    )
    assert boot_prerender(settings, lambda: chain) is None
    assert provider.calls == []  # zero synth calls
    assert "voice_greeting_prerender_total{" not in _rendered()  # no samples


def test_h_empty_tenant_list_is_noop():
    provider = SpyProvider()
    chain = FallbackTTS({"piper": provider}, ["piper"], sample_rate=22050)
    settings = Settings(voice_greeting_prerender_tenants="  , ,")
    assert boot_prerender(settings, lambda: chain) is None
    assert provider.calls == []


# ---------------------------------------------------------------------------
# (i) boot timeout path proceeds with the partial warm
# ---------------------------------------------------------------------------
def test_i_boot_timeout_proceeds_with_partial_warm(monkeypatch):
    fast = SpyProvider(delay_s=0.0)
    settings = Settings(
        voice_greeting_prerender_tenants="acme,slowcoach",
        voice_greeting_prerender_timeout_s=0.1,
    )
    greeting_fast = build_greeting(_ctx("acme", "Acme"))
    greeting_slow = build_greeting(_ctx("slowcoach", "Slowcoach"))

    class _Mixed(SpyProvider):
        async def synthesize(self, text, voice, language):
            if "Slowcoach" in text:
                await asyncio.sleep(5.0)  # hangs past the boot budget
            return await super().synthesize(text, voice, language)

    provider = _Mixed()
    chain = FallbackTTS({"piper": provider}, ["piper"], sample_rate=22050)
    chain.voice = "en_US-lessac-medium"

    async def _fake_collect(settings_, slugs, **kw):
        texts = {"acme": greeting_fast, "slowcoach": greeting_slow}
        return texts, {s: "en_US-lessac-medium" for s in texts}

    monkeypatch.setattr(greeting_prerender, "collect_greeting_texts", _fake_collect)

    started = time.perf_counter()
    tts = boot_prerender(settings, lambda: chain)
    elapsed = time.perf_counter() - started

    assert elapsed < 2.0  # boot proceeded; no 5s wait, no crash
    assert tts is chain  # partial warm returned, not discarded
    assert 'voice_greeting_prerender_total{result="timeout"} 1' in _rendered()

    # Partial warm is real: the fast tenant's greeting is already cached.
    async def _check_partial():
        calls = len(provider.calls)
        await chain.synthesize_pcm(greeting_fast)
        assert len(provider.calls) == calls  # cache hit

    asyncio.run(_check_partial())


# ---------------------------------------------------------------------------
# Supporting behavior
# ---------------------------------------------------------------------------
def test_parse_tenant_slugs():
    assert parse_tenant_slugs("") == []
    assert parse_tenant_slugs(" acme, ,beau,acme ,") == ["acme", "beau"]


async def test_concurrency_bound_respected():
    settings = Settings(voice_greeting_prerender_concurrency=1)
    provider = SpyProvider(delay_s=0.02)
    chain = FallbackTTS({"piper": provider}, ["piper"], sample_rate=22050)
    texts = {f"t{i}": f"Hello tenant {i}." for i in range(4)}
    await prerender_greetings(settings, lambda: chain, texts)
    assert len(provider.calls) == 4
    assert provider.max_in_flight == 1  # strictly serialized


async def test_concurrent_warm_with_concurrency():
    settings = Settings(voice_greeting_prerender_concurrency=4)
    provider = SpyProvider(delay_s=0.05)
    chain = FallbackTTS({"piper": provider}, ["piper"], sample_rate=22050)
    texts = {f"t{i}": f"Hello tenant {i}." for i in range(4)}
    started = time.perf_counter()
    await prerender_greetings(settings, lambda: chain, texts)
    assert time.perf_counter() - started < 0.15  # overlapped, not 4x0.05 serial
    assert len(provider.calls) == 4


async def test_collect_greeting_texts_real_seam_and_voice_identity():
    """Text collection goes through fetch_tenant_context + build_greeting,
    and the collected voice mirrors build_voice_agent's resolution."""
    settings = Settings()
    ctxs = {"acme": _ctx("acme", "Acme"), "beau": _ctx("beau", "Beau")}

    async def _fake_fetch(dapr, settings_, slug):
        return ctxs[slug]

    texts, voices = await collect_greeting_texts(
        settings, ["acme", "beau"], fetch_ctx=_fake_fetch
    )
    assert texts == {s: build_greeting(c) for s, c in ctxs.items()}
    assert voices == {"acme": "en_US-lessac-medium", "beau": "en_US-lessac-medium"}
    assert session_greeting_voice(settings, ctxs["acme"]) == settings.piper_voice


async def test_collect_greeting_texts_skips_failing_tenant():
    settings = Settings()

    async def _boom(dapr, settings_, slug):
        raise ConnectionError("identity unreachable (scripted)")

    texts, voices = await collect_greeting_texts(settings, ["acme"], fetch_ctx=_boom)
    assert texts == {} and voices == {}


def test_session_greeting_voice_respects_definition_override():
    from app.agent_definition import AgentDefinition

    settings = Settings()
    ctx = _ctx("acme", "Acme")
    definition = AgentDefinition()
    definition.voice.voice_id = "en_GB-alba-medium"
    definition.voice.provider = "piper"
    ctx.agent_definition = definition
    assert session_greeting_voice(settings, ctx) == "en_GB-alba-medium"


def test_prewarm_fnc_prerenders_greetings_per_process(monkeypatch):
    """Spawned job processes do NOT inherit the supervisor's warm cache
    (0.10.2 mp spawn), so prewarm_fnc prerenders into its own chain."""

    class _FakeSTT:
        def preload_sync(self) -> None:
            pass

    provider = SpyProvider()
    chain = FallbackTTS({"piper": provider}, ["piper"], sample_rate=22050)
    monkeypatch.setattr(lw, "_build_stt", lambda settings: _FakeSTT())
    monkeypatch.setattr(lw, "_build_tts", lambda settings: chain)

    greeting = build_greeting(_ctx("acme", "Acme"))

    async def _fake_collect(settings_, slugs, **kw):
        return {"acme": greeting}, {"acme": "en_US-lessac-medium"}

    monkeypatch.setattr(greeting_prerender, "collect_greeting_texts", _fake_collect)

    settings = Settings(
        preload_models=True, voice_greeting_prerender_tenants="acme"
    )
    lw.make_prewarm_fnc(settings)(object())

    synth_texts = [c[0] for c in provider.calls]
    assert lw.PREWARM_PHRASE in synth_texts  # legacy warmup preserved
    assert greeting in synth_texts  # greeting prerendered per process
    assert lw._PREWARMED["tts"] is chain

    # Session on this process: greeting is a cache hit.
    async def _check():
        calls = len(provider.calls)
        await chain.synthesize_pcm(greeting)
        assert len(provider.calls) == calls

    asyncio.run(_check())


def test_env_parsing(monkeypatch):
    for key in (
        "VOICE_GREETING_PRERENDER_ENABLED",
        "VOICE_GREETING_PRERENDER_TENANTS",
        "VOICE_GREETING_PRERENDER_CONCURRENCY",
        "VOICE_GREETING_PRERENDER_TIMEOUT_S",
    ):
        monkeypatch.delenv(key, raising=False)
    defaults = load_settings()
    assert defaults.voice_greeting_prerender_enabled is True
    assert defaults.voice_greeting_prerender_tenants == ""  # no-op default
    assert defaults.voice_greeting_prerender_concurrency == 4
    assert defaults.voice_greeting_prerender_timeout_s == 30.0

    monkeypatch.setenv("VOICE_GREETING_PRERENDER_ENABLED", "0")
    monkeypatch.setenv("VOICE_GREETING_PRERENDER_TENANTS", "acme, beau")
    monkeypatch.setenv("VOICE_GREETING_PRERENDER_CONCURRENCY", "2")
    monkeypatch.setenv("VOICE_GREETING_PRERENDER_TIMEOUT_S", "7.5")
    tuned = load_settings()
    assert tuned.voice_greeting_prerender_enabled is False
    assert tuned.voice_greeting_prerender_tenants == "acme, beau"
    assert tuned.voice_greeting_prerender_concurrency == 2
    assert tuned.voice_greeting_prerender_timeout_s == 7.5
