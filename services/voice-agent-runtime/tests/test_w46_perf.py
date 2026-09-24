"""SPEC-W46 W46-E performance tests (voice-agent-runtime).

Covers:
- P1/P2: tenant-context TTL cache (shared, per-slug lock, stale-while-error,
  mutation isolation) + gathered identity/knowledge legs;
- P-02: direct booking base URL with daprd transport-failure fallback;
- P9: _emit_tool_event fire-and-forget (tool path never awaits the publish);
- P4: PiperTTS LRU cache + sentence-level stream_pcm; FallbackTTS cache +
  chunked streaming.

All fakes run on httpx.MockTransport / in-memory coroutines — no network.
"""

from __future__ import annotations

import asyncio
import io
import wave

import httpx
import pytest

from app import tenant_context
from app.config import Settings
from app.dapr_client import DaprClient, DaprError
from app.pipeline.tts import PiperTTS, TtsLruCache, split_sentences
from app.session_state import SessionState
from app.tenant_context import (
    TenantContext,
    clear_tenant_context_cache,
    fetch_tenant_context,
)
from app.tools import ToolLayer
from app.tts_providers.chain import FallbackTTS

from conftest import FakeDapr


@pytest.fixture(autouse=True)
def _clean_ctx_cache():
    clear_tenant_context_cache()
    yield
    clear_tenant_context_cache()


def _wav_bytes(rate: int = 22050, frames: int = 64) -> bytes:
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)
        wf.setframerate(rate)
        wf.writeframes(b"\x01\x00" * frames)
    return buf.getvalue()


# ------------------------------------------------------------- P1/P2: cache
class CtxDapr:
    """Scriptable invoke_get for the tenant-context bootstrap legs."""

    def __init__(self) -> None:
        self.calls: list[tuple[str, str]] = []
        self.fail = False

    async def invoke_get(self, app_id, method, *, params=None, headers=None):
        self.calls.append((app_id, method))
        if self.fail:
            raise DaprError("booking down")
        if app_id == "booking":
            return {
                "site": {
                    "tenant_slug": "acme",
                    "tenant_id": "t1",
                    "display_name": "Acme",
                },
                "tenant": {"slug": "acme", "timezone": "UTC"},
                "offerings": [],
                "team_members": [],
            }
        if app_id == "identity":
            return {"id": "t1", "timezone": "Africa/Lagos", "currency": "NGN"}
        if app_id == "knowledge":
            return {"snippets": [{"content": "open 9-5"}]}
        raise AssertionError(f"unexpected app {app_id}")


def _settings(**kw) -> Settings:
    kw.setdefault("knowledge_snippet_count", 3)
    return Settings(**kw)


async def test_tenant_context_cached_per_slug():
    dapr = CtxDapr()
    settings = _settings()
    first = await fetch_tenant_context(dapr, settings, "demo")
    second = await fetch_tenant_context(dapr, settings, "demo")
    # One booking leg total; identity + knowledge legs ran once too.
    assert [c for c in dapr.calls if c[0] == "booking"] == [
        ("booking", "public/sites/demo/context")
    ]
    assert first.tenant_slug == second.tenant_slug == "acme"
    assert second.timezone == "Africa/Lagos"  # identity leg applied
    assert second.knowledge_snippets == ["open 9-5"]  # knowledge leg applied


async def test_tenant_context_cache_returns_isolated_copies():
    dapr = CtxDapr()
    settings = _settings()
    ctx1 = await fetch_tenant_context(dapr, settings, "demo")
    ctx1.agent_persona = "MUTATED"
    ctx1.offerings.append({"id": "x"})
    ctx2 = await fetch_tenant_context(dapr, settings, "demo")
    assert ctx2.agent_persona == ""  # cache not poisoned by caller mutation
    assert ctx2.offerings == []


async def test_tenant_context_stale_while_error():
    dapr = CtxDapr()
    settings = _settings(tenant_ctx_ttl_s=120, tenant_ctx_stale_s=300)
    ok = await fetch_tenant_context(dapr, settings, "demo")
    # Expire the fresh window but stay inside the stale window.
    entry = tenant_context._ctx_cache["demo"]
    entry.expires_at = 0.0
    dapr.fail = True
    stale = await fetch_tenant_context(dapr, settings, "demo")
    assert stale.tenant_slug == ok.tenant_slug  # served stale, no raise
    assert len([c for c in dapr.calls if c[0] == "booking"]) == 2  # refresh tried


async def test_tenant_context_error_without_stale_raises():
    dapr = CtxDapr()
    dapr.fail = True
    with pytest.raises(DaprError):
        await fetch_tenant_context(dapr, _settings(), "demo")


async def test_identity_and_knowledge_legs_run_concurrently():
    """Each leg blocks until the OTHER leg has started: sequential execution
    would deadlock; asyncio.gather lets both complete."""

    class DeadlockDapr(CtxDapr):
        def __init__(self) -> None:
            super().__init__()
            self.identity_started = asyncio.Event()
            self.knowledge_started = asyncio.Event()

        async def invoke_get(self, app_id, method, *, params=None, headers=None):
            if app_id == "identity":
                self.identity_started.set()
                await asyncio.wait_for(self.knowledge_started.wait(), 1.0)
            if app_id == "knowledge":
                self.knowledge_started.set()
                await asyncio.wait_for(self.identity_started.wait(), 1.0)
            return await super().invoke_get(
                app_id, method, params=params, headers=headers
            )

    dapr = DeadlockDapr()
    ctx = await asyncio.wait_for(
        fetch_tenant_context(dapr, _settings(), "demo"), timeout=3.0
    )
    assert ctx.tenant_slug == "acme"


# ------------------------------------------------------- P-02: direct base
def _mock_dapr(handler, **kw) -> DaprClient:
    client = DaprClient("http://daprd:3500", **kw)
    client._client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    return client


async def test_direct_base_used_for_registered_app_only():
    hits = {"direct": 0, "daprd": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.host == "booking":
            hits["direct"] += 1
            return httpx.Response(200, json={"via": "direct"})
        hits["daprd"] += 1
        return httpx.Response(200, json={"via": "daprd"})

    client = _mock_dapr(handler, direct_bases={"booking": "http://booking:7002"})
    assert await client.invoke_get("booking", "v1/bookings") == {"via": "direct"}
    assert await client.invoke_get("identity", "v1/tenants/acme") == {"via": "daprd"}
    assert hits == {"direct": 1, "daprd": 1}


async def test_direct_transport_error_falls_back_to_daprd():
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.host == "booking":
            raise httpx.ConnectError("unreachable", request=request)
        return httpx.Response(200, json={"via": "daprd"})

    client = _mock_dapr(handler, direct_bases={"booking": "http://booking:7002"})
    assert await client.invoke_get("booking", "v1/bookings") == {"via": "daprd"}


async def test_direct_http_error_raises_without_fallback():
    hits = {"daprd": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.host == "booking":
            return httpx.Response(500, text="boom")
        hits["daprd"] += 1
        return httpx.Response(200, json={"via": "daprd"})

    client = _mock_dapr(handler, direct_bases={"booking": "http://booking:7002"})
    with pytest.raises(DaprError):
        await client.invoke_get("booking", "v1/bookings")
    assert hits["daprd"] == 0  # app-level errors are not re-issued via daprd


async def test_no_direct_base_stays_on_daprd():
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.host == "daprd"
        return httpx.Response(200, json={"via": "daprd"})

    client = _mock_dapr(handler, direct_bases={"booking": ""})
    assert await client.invoke_get("booking", "v1/bookings") == {"via": "daprd"}


# ------------------------------------------------- P9: fire-and-forget emit
def _ctx() -> TenantContext:
    return TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")


async def test_emit_tool_event_does_not_block_tool_path():
    """The ToolInvoked publish is scheduled in the background: the tool
    returns even while the publish is still in flight, and the event lands
    once the publisher unblocks."""

    class GatedDapr(FakeDapr):
        def __init__(self) -> None:
            super().__init__()
            self.gate = asyncio.Event()

        async def publish_best_effort(self, pubsub, topic, event, kind=""):
            await self.gate.wait()
            return await super().publish_best_effort(pubsub, topic, event, kind=kind)

    dapr = GatedDapr()
    layer = ToolLayer(
        dapr=dapr,
        settings=Settings(),
        ctx=_ctx(),
        session=SessionState(conversation_id="c1", site_slug="demo"),
    )
    result = await asyncio.wait_for(layer.dispatch("get_business_info", {}), 1.0)
    assert "offerings" in result  # tool answered
    assert dapr.best_effort == []  # publish still gated — path did not await it
    dapr.gate.set()
    await asyncio.sleep(0)
    await asyncio.sleep(0)
    assert [e["data"]["tool"] for (_p, _t, e) in dapr.best_effort] == [
        "get_business_info"
    ]


# ------------------------------------------------------ P4: TTS cache/chunk
def _piper(handler, **kw) -> PiperTTS:
    kw.setdefault("mode", "http")
    kw.setdefault("http_url", "http://piper:5500")
    piper = PiperTTS(**kw)
    piper._client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    return piper


def test_tts_lru_cache_eviction_and_disable():
    cache = TtsLruCache(2)
    cache.set(("v", "a", "pcm"), b"a")
    cache.set(("v", "b", "pcm"), b"b")
    cache.get(("v", "a", "pcm"))  # touch: b becomes oldest
    cache.set(("v", "c", "pcm"), b"c")
    assert cache.get(("v", "b", "pcm")) is None
    assert cache.get(("v", "a", "pcm")) == b"a"
    off = TtsLruCache(0)
    off.set(("v", "a", "pcm"), b"a")
    assert off.get(("v", "a", "pcm")) is None


def test_split_sentences():
    assert split_sentences("Hello there. How are you? Fine!") == [
        "Hello there.",
        "How are you?",
        "Fine!",
    ]
    assert split_sentences("no punctuation at all") == ["no punctuation at all"]
    assert split_sentences("") == []


async def test_piper_pcm_cache_avoids_resynthesis():
    posts: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        import json as _json

        posts.append(_json.loads(request.content))
        return httpx.Response(
            200, content=_wav_bytes(), headers={"content-type": "audio/wav"}
        )

    piper = _piper(handler)
    first = await piper.synthesize_pcm("hello world")
    second = await piper.synthesize_pcm("hello world")
    assert first == second
    assert len(posts) == 1  # second call served from the LRU
    await piper.synthesize_pcm("different text")
    assert len(posts) == 2
    # Voice is part of the cache key.
    piper.voice = "en_US-other-voice"
    await piper.synthesize_pcm("hello world")
    assert len(posts) == 3


async def test_piper_stream_pcm_chunks_then_full_cache():
    posts: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        import json as _json

        posts.append(_json.loads(request.content)["text"])
        return httpx.Response(
            200, content=_wav_bytes(), headers={"content-type": "audio/wav"}
        )

    piper = _piper(handler)
    chunks = [pcm async for pcm in piper.stream_pcm("One. Two! Three?")]
    assert len(chunks) == 3  # first frame after the first sentence
    assert posts == ["One.", "Two!", "Three?"]
    # Repeat: full-utterance cache hit, single buffer, no new synthesis.
    again = [pcm async for pcm in piper.stream_pcm("One. Two! Three?")]
    assert again == [b"".join(chunks)]
    assert posts == ["One.", "Two!", "Three?"]


class _FakeProvider:
    name = "fake"

    def __init__(self) -> None:
        self.calls: list[str] = []

    @property
    def default_voice(self) -> str:
        return "fake-voice"

    async def synthesize(self, text: str, voice: str, language: str) -> bytes:
        self.calls.append(text)
        return _wav_bytes()

    async def list_voices(self):
        return []

    async def available(self) -> bool:
        return True

    async def aclose(self) -> None:
        pass


async def test_chain_stream_pcm_chunks_and_caches():
    provider = _FakeProvider()
    chain = FallbackTTS({"fake": provider}, ["fake"], cache_size=8)
    chunks = [pcm async for pcm in chain.stream_pcm("One. Two.")]
    assert len(chunks) == 2
    assert provider.calls == ["One.", "Two."]
    again = [pcm async for pcm in chain.stream_pcm("One. Two.")]
    assert again == [b"".join(chunks)]
    assert provider.calls == ["One.", "Two."]  # full-utterance cache hit
    # synthesize_pcm is cached too.
    await chain.synthesize_pcm("solo utterance")
    await chain.synthesize_pcm("solo utterance")
    assert provider.calls.count("solo utterance") == 1
