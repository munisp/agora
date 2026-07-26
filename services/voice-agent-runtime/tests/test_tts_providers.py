"""SPEC-W10 Part A tests: TTS provider clients (httpx mocks), the FallbackTTS
chain (ordering, per-provider circuit breakers, voice routing), TTS_VOICE_MAP
parsing/precedence in multilang, and the control-plane voice endpoints."""

from __future__ import annotations

import io
import json
import wave
from types import SimpleNamespace

import httpx
import pytest

from app import metrics
from app.config import Settings
from app.multilang import (
    parse_tts_voice_map,
    resolve_tts_voice,
    voice_for_language,
)
from app.tts_providers.azure import AZURE_VOICES, AzureTTS
from app.tts_providers.base import Voice, split_voice_spec
from app.tts_providers.chain import (
    FallbackTTS,
    build_fallback_tts,
    parse_chain,
)
from app.tts_providers.mms import MmsTTS
from app.tts_providers.spitch import SpitchTTS
from app.tts_providers.xtts import XttsTTS


def _wav(rate: int = 22050, frames: int = 64) -> bytes:
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)
        wf.setframerate(rate)
        wf.writeframes(b"\x01\x00" * frames)
    return buf.getvalue()


WAV = _wav()


def _mock_client(handler) -> httpx.AsyncClient:
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


class FakeProvider:
    """Scripted TTSProvider double: outcomes is a list of bytes|Exception."""

    def __init__(self, name: str, outcomes=None, default_voice: str = "dv"):
        self.name = name
        self.default_voice = default_voice
        self.outcomes = list(outcomes or [WAV])
        self.calls: list[tuple[str, str, str]] = []
        self._available = True
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
        return self._available

    async def aclose(self) -> None:
        pass


# ---------------------------------------------------------------------------
# Chain: ordering, routing, piper last resort
# ---------------------------------------------------------------------------
async def test_chain_single_provider_success_pcm():
    chain = FallbackTTS({"piper": FakeProvider("piper")}, ["piper"])
    pcm = await chain.synthesize_pcm("hello")
    assert pcm == b"\x01\x00" * 64
    assert chain.sample_rate == 22050


async def test_chain_empty_text_returns_empty():
    chain = FallbackTTS({"piper": FakeProvider("piper")}, ["piper"])
    assert await chain.synthesize_pcm("   ") == b""


async def test_chain_fallback_order_and_metrics():
    registry = metrics.reset_registry()
    primary = FakeProvider("mms", [ConnectionError("down"), ConnectionError("down")])
    secondary = FakeProvider("piper")
    chain = FallbackTTS(
        {"mms": primary, "piper": secondary}, ["mms", "piper"]
    )
    wav = await chain.synthesize("hi")
    assert wav == WAV
    assert len(primary.calls) == 1
    assert len(secondary.calls) == 1
    rendered = registry.render()
    assert 'tts_provider_failures_total{provider="mms"} 1' in rendered


async def test_provider_qualified_voice_routes_to_pinned_provider_first():
    mms = FakeProvider("mms")
    piper = FakeProvider("piper")
    chain = FallbackTTS({"mms": mms, "piper": piper}, ["piper", "mms"])
    await chain.synthesize("sannu", voice="mms:pcm", language="pcm")
    assert mms.calls == [("sannu", "pcm", "pcm")]
    assert piper.calls == []


async def test_pinned_provider_failure_falls_through_with_default_voice():
    mms = FakeProvider("mms", [RuntimeError("boom")])
    piper = FakeProvider("piper", default_voice="en_US-lessac-medium")
    chain = FallbackTTS({"mms": mms, "piper": piper}, ["piper", "mms"])
    wav = await chain.synthesize("sannu", voice="mms:pcm")
    assert wav == WAV
    # Pinned provider got the qualified id; fallback got ITS default voice.
    assert mms.calls[0][1] == "pcm"
    assert piper.calls[0][1] == "en_US-lessac-medium"


async def test_bare_voice_spec_is_piper_legacy():
    piper = FakeProvider("piper")
    mms = FakeProvider("mms")
    chain = FallbackTTS({"piper": piper, "mms": mms}, ["piper", "mms"])
    await chain.synthesize("hi", voice="en_GB-alan-medium")
    assert piper.calls[0][1] == "en_GB-alan-medium"
    assert mms.calls == []


async def test_piper_implicit_last_resort():
    mms = FakeProvider("mms", [ConnectionError("down")])
    piper = FakeProvider("piper")
    # "piper" not in the configured order — appended implicitly by the ctor.
    chain = FallbackTTS({"mms": mms, "piper": piper}, ["mms"])
    wav = await chain.synthesize("hi")
    assert wav == WAV
    assert chain.order == ["mms", "piper"]
    assert len(piper.calls) == 1


async def test_factory_appends_piper_last_resort():
    settings = Settings(tts_provider_chain="mms")
    chain = build_fallback_tts(settings)
    assert chain.order == ["mms", "piper"]
    assert set(chain.providers) == {"mms", "piper"}
    await chain.aclose()


async def test_factory_default_chain_is_piper_only():
    settings = Settings()
    assert settings.tts_provider_chain == "piper"
    chain = build_fallback_tts(settings)
    assert chain.order == ["piper"]
    assert list(chain.providers) == ["piper"]
    await chain.aclose()


async def test_unknown_provider_pin_raises_value_error():
    chain = FallbackTTS({"piper": FakeProvider("piper")}, ["piper"])
    with pytest.raises(ValueError, match="unknown TTS provider"):
        await chain.synthesize("hi", provider="nope")


async def test_all_providers_failed_raises():
    metrics.reset_registry()
    chain = FallbackTTS(
        {
            "mms": FakeProvider("mms", [RuntimeError("x")]),
            "piper": FakeProvider("piper", [RuntimeError("y")]),
        },
        ["mms", "piper"],
    )
    with pytest.raises(RuntimeError, match="all TTS providers failed"):
        await chain.synthesize("hi")


def test_parse_chain_dedupes_and_drops_unknown():
    assert parse_chain("azure,mms, piper,azure, bogus") == ["azure", "mms", "piper"]
    assert parse_chain("") == ["piper"]
    assert parse_chain("bogus") == ["piper"]


def test_split_voice_spec():
    assert split_voice_spec("mms:pcm") == ("mms", "pcm")
    assert split_voice_spec("azure:en-NG-EzinneNeural") == (
        "azure",
        "en-NG-EzinneNeural",
    )
    assert split_voice_spec("en_US-lessac-medium") == (None, "en_US-lessac-medium")
    assert split_voice_spec("") == (None, "")
    assert split_voice_spec("mms:") == (None, "mms:")  # malformed -> bare


# ---------------------------------------------------------------------------
# Chain: per-provider circuit breaker (mirror of FallbackLLM semantics)
# ---------------------------------------------------------------------------
async def test_breaker_opens_skips_and_half_open_probes():
    t = [0.0]
    failing = FakeProvider(
        "mms",
        [ConnectionError("x"), ConnectionError("x"), ConnectionError("x"), WAV],
    )
    backup = FakeProvider("piper")
    chain = FallbackTTS(
        {"mms": failing, "piper": backup},
        ["mms", "piper"],
        failure_threshold=3,
        cooldown_s=60.0,
        clock=lambda: t[0],
    )
    for _ in range(3):
        await chain.synthesize("hi")
    breaker = chain.breakers["mms"]
    assert breaker.is_open
    assert len(failing.calls) == 3

    # Circuit open: primary skipped entirely.
    await chain.synthesize("hi")
    assert len(failing.calls) == 3

    # After cooldown: one half-open probe; success closes the circuit.
    t[0] += 61.0
    await chain.synthesize("hi")
    assert len(failing.calls) == 4
    assert not breaker.is_open


async def test_failed_probe_reopens_circuit():
    t = [0.0]
    failing = FakeProvider("mms", [ConnectionError("x")] * 10)
    backup = FakeProvider("piper")
    chain = FallbackTTS(
        {"mms": failing, "piper": backup},
        ["mms", "piper"],
        failure_threshold=3,
        cooldown_s=60.0,
        clock=lambda: t[0],
    )
    for _ in range(3):
        await chain.synthesize("hi")
    t[0] += 61.0
    await chain.synthesize("hi")  # probe fails -> re-open
    assert chain.breakers["mms"].is_open
    calls_before = len(failing.calls)
    await chain.synthesize("hi")
    assert len(failing.calls) == calls_before  # skipped again


# ---------------------------------------------------------------------------
# Voice map parsing + precedence (multilang)
# ---------------------------------------------------------------------------
def test_parse_tts_voice_map_valid():
    m = parse_tts_voice_map(
        '{"pcm": "mms:pcm", "en-NG": "azure:en-NG-EzinneNeural", "yo": "mms:yor"}'
    )
    assert m == {
        "pcm": "mms:pcm",
        "en-ng": "azure:en-NG-EzinneNeural",
        "yo": "mms:yor",
    }


def test_parse_tts_voice_map_tolerant():
    assert parse_tts_voice_map("") == {}
    assert parse_tts_voice_map("not json") == {}
    assert parse_tts_voice_map("[1,2]") == {}
    # malformed provider prefix / empty voice id dropped; bare ids kept
    m = parse_tts_voice_map({"pcm": "mms:", "yo": "MMS:yor", "en": "plain-voice"})
    assert m == {"yo": "mms:yor", "en": "plain-voice"}


def test_resolve_tts_voice_pcm_not_proxied():
    m = {"pcm": "mms:pcm"}
    assert resolve_tts_voice("pcm", m, {}, "en_US-lessac-medium") == "mms:pcm"


def test_resolve_tts_voice_region_tag():
    m = {"en-ng": "azure:en-NG-EzinneNeural"}
    assert (
        resolve_tts_voice("en-NG", m, {}, "en_US-lessac-medium")
        == "azure:en-NG-EzinneNeural"
    )
    # bare "en" does NOT match the en-ng entry
    assert resolve_tts_voice("en", m, {}, "en_US-lessac-medium") == "en_US-lessac-medium"


def test_resolve_tts_voice_primary_subtag_fallback():
    m = {"yo": "mms:yor"}
    assert resolve_tts_voice("yo-NG", m, {}, "en_US-lessac-medium") == "mms:yor"


def test_resolve_tts_voice_tts_map_precedes_piper_map():
    tts_map = {"pcm": "mms:pcm"}
    piper_map = {"en": "en_GB-alan-medium"}
    # pcm would proxy to en under the piper map; the TTS map wins first.
    assert (
        resolve_tts_voice("pcm", tts_map, piper_map, "en_US-lessac-medium")
        == "mms:pcm"
    )
    # language with no TTS-map entry still uses the piper map
    assert (
        resolve_tts_voice("en", tts_map, piper_map, "en_US-lessac-medium")
        == "en_GB-alan-medium"
    )


def test_resolve_tts_voice_unset_is_byte_identical():
    piper_map = {"es": "es_ES-sharvard-medium"}
    for lang in ("en", "es", "pcm", "fr", ""):
        assert resolve_tts_voice(lang, {}, piper_map, "en_US-lessac-medium") == (
            voice_for_language(lang, piper_map, "en_US-lessac-medium")
        )


def test_resolve_tts_voice_tenant_override():
    ctx = SimpleNamespace(tts_voice="azure:en-NG-AbeoNeural")
    assert (
        resolve_tts_voice("pcm", {"pcm": "mms:pcm"}, {}, "d", ctx=ctx)
        == "azure:en-NG-AbeoNeural"
    )
    # defensive: ctx without the attribute / empty value is ignored
    assert resolve_tts_voice("pcm", {"pcm": "mms:pcm"}, {}, "d", ctx=object()) == "mms:pcm"
    ctx_blank = SimpleNamespace(tts_voice="   ")
    assert resolve_tts_voice("pcm", {"pcm": "mms:pcm"}, {}, "d", ctx=ctx_blank) == "mms:pcm"


def test_settings_from_env(monkeypatch):
    monkeypatch.setenv("TTS_PROVIDER_CHAIN", "azure,mms,piper")
    monkeypatch.setenv("TTS_VOICE_MAP", '{"pcm": "mms:pcm"}')
    monkeypatch.setenv("AZURE_SPEECH_KEY", "k")
    monkeypatch.setenv("AZURE_SPEECH_REGION", "westeurope")
    from app.config import load_settings

    s = load_settings()
    assert s.tts_provider_chain == "azure,mms,piper"
    assert s.tts_voice_map == {"pcm": "mms:pcm"}
    assert s.azure_speech_key == "k"
    assert s.azure_speech_region == "westeurope"
    assert s.mms_tts_url == "http://mms-tts:5800"
    assert s.xtts_tts_url == "http://xtts-tts:5810"
    assert s.spitch_base_url == "https://api.spitch.app"


# ---------------------------------------------------------------------------
# Provider clients (httpx.MockTransport)
# ---------------------------------------------------------------------------
async def test_mms_synthesize_shape_and_retry():
    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        if len(calls) == 1:
            return httpx.Response(500, text="cold model")
        assert request.url.path == "/tts"
        assert json.loads(request.content) == {"text": "sannu", "lang": "pcm"}
        return httpx.Response(200, content=WAV)

    mms = MmsTTS("http://mms-tts:5800", client=_mock_client(handler))
    wav = await mms.synthesize("sannu", "pcm", "pcm")
    assert wav == WAV
    assert len(calls) == 2  # one retry on 5xx


async def test_mms_lang_mapping_and_no_retry_on_4xx():
    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return httpx.Response(400, text="bad lang")

    mms = MmsTTS("http://mms", client=_mock_client(handler))
    with pytest.raises(httpx.HTTPStatusError):
        await mms.synthesize("kilonshe", "", "yo")
    assert len(calls) == 1
    assert json.loads(calls[0].content)["lang"] == "yor"  # yo -> yor


async def test_mms_list_voices_and_available():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "voices": [
                    {
                        "id": "pcm",
                        "languages": ["pcm"],
                        "gender": "female",
                        "labels": {"name": "Pidgin"},
                    },
                    {"broken": True},
                ]
            },
        )

    mms = MmsTTS("http://mms", client=_mock_client(handler))
    voices = await mms.list_voices()
    assert [v.id for v in voices] == ["pcm"]
    assert voices[0].as_dict()["labels"] == {"name": "Pidgin"}
    assert await mms.available() is True

    down = MmsTTS(
        "http://mms",
        client=_mock_client(lambda r: (_ for _ in ()).throw(httpx.ConnectError("x"))),
    )
    assert await down.available() is False


async def test_xtts_synthesize_and_enroll():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content)
        if request.url.path == "/tts":
            seen["tts"] = body
            return httpx.Response(200, content=WAV)
        if request.url.path == "/voices" and request.method == "POST":
            seen["enroll"] = body
            return httpx.Response(200, json={"voice_id": "brand-1"})
        return httpx.Response(404)

    xtts = XttsTTS("http://xtts:5810", client=_mock_client(handler))
    wav = await xtts.synthesize("hello", "brand-1", "en")
    assert wav == WAV
    assert seen["tts"] == {"text": "hello", "voice_id": "brand-1", "language": "en"}

    voice_id = await xtts.enroll_voice("Acme Brand", "QUJD")
    assert voice_id == "brand-1"
    assert seen["enroll"] == {"name": "Acme Brand", "sample_base64": "QUJD"}


async def test_xtts_synthesize_requires_voice():
    xtts = XttsTTS("http://xtts", client=_mock_client(lambda r: httpx.Response(500)))
    with pytest.raises(RuntimeError, match="voice_id"):
        await xtts.synthesize("hi", "", "en")


async def test_azure_ssml_request_shape():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["key"] = request.headers.get("Ocp-Apim-Subscription-Key")
        seen["fmt"] = request.headers.get("X-Microsoft-OutputFormat")
        seen["ct"] = request.headers.get("Content-Type")
        seen["body"] = request.content.decode()
        return httpx.Response(200, content=WAV)

    azure = AzureTTS(
        api_key="secret", region="westeurope", client=_mock_client(handler)
    )
    assert await azure.available() is True
    wav = await azure.synthesize("How far <boss>?", "en-NG-AbeoNeural", "en-NG")
    assert wav == WAV
    assert seen["url"] == (
        "https://westeurope.tts.speech.microsoft.com/cognitiveservices/v1"
    )
    assert seen["key"] == "secret"
    assert seen["fmt"] == "riff-24khz16bit-mono-pcm"
    assert seen["ct"] == "application/ssml+xml"
    assert "name='en-NG-AbeoNeural'" in seen["body"]
    assert "How far &lt;boss&gt;?" in seen["body"]  # SSML escaped


async def test_azure_unconfigured_and_catalog():
    azure = AzureTTS()
    assert await azure.available() is False
    with pytest.raises(RuntimeError, match="not configured"):
        await azure.synthesize("hi", "", "en")
    voices = await azure.list_voices()
    ids = [v.id for v in voices]
    assert ids == [v.id for v in AZURE_VOICES]
    assert "en-NG-EzinneNeural" in ids and "en-NG-AbeoNeural" in ids
    # Verified: no Azure Yoruba/Hausa voices (those route to mms/spitch).
    assert not any(v.id.startswith(("yo-", "ha-")) for v in voices)


async def test_spitch_request_shape():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["auth"] = request.headers.get("Authorization")
        seen["accept"] = request.headers.get("Accept")
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, content=WAV)

    spitch = SpitchTTS(api_key="sk", client=_mock_client(handler))
    wav = await spitch.synthesize("Bawo ni", "femi", "yo")
    assert wav == WAV
    assert seen["url"] == "https://api.spitch.app/v1/speech"
    assert seen["auth"] == "Bearer sk"
    assert seen["accept"] == "audio/wav"
    assert seen["body"] == {
        "text": "Bawo ni",
        "voice": "femi",
        "format": "wav",
        "speed": 1.0,
        "language": "yo",
    }


def test_spitch_build_request_isolated():
    spitch = SpitchTTS(api_key="sk", base_url="https://api.spitch.app/")
    url, headers, body = spitch.build_request("ekaabo", "", "yo-NG")
    assert url == "https://api.spitch.app/v1/speech"
    assert headers["Authorization"] == "Bearer sk"
    assert body["voice"] == "sade"  # default voice
    assert body["language"] == "yo"  # region stripped


async def test_spitch_unconfigured():
    spitch = SpitchTTS()
    assert await spitch.available() is False
    with pytest.raises(RuntimeError, match="SPITCH_API_KEY"):
        await spitch.synthesize("hi", "", "yo")


# ---------------------------------------------------------------------------
# Control-plane endpoints (ASGI transport; chain stubbed)
# ---------------------------------------------------------------------------
class _FakeChain:
    """Stands in for FallbackTTS at the control-plane wiring point."""

    def __init__(self, providers: dict, order: list[str] | None = None):
        self._providers = dict(providers)
        self._order = list(order or providers.keys())
        self.calls: list[dict] = []

    def providers_in_order(self):
        return [(n, self._providers[n]) for n in self._order]

    def provider(self, name: str):
        return self._providers.get(name)

    async def aclose(self) -> None:
        pass

    async def synthesize(self, text, *, voice="", language="", provider=None):
        self.calls.append(
            {"text": text, "voice": voice, "language": language, "provider": provider}
        )
        if provider == "nope":
            raise ValueError("unknown TTS provider: 'nope'")
        if text == "explode":
            raise RuntimeError("all TTS providers failed")
        return WAV


def _app(monkeypatch, chain, settings: Settings | None = None):
    from app import control_plane as cp

    monkeypatch.setattr(cp, "build_fallback_tts", lambda s: chain)
    return cp.create_app(settings or Settings())


async def _post(app, path: str, payload: dict) -> httpx.Response:
    async with httpx.AsyncClient(
        transport=httpx.ASGITransport(app=app), base_url="http://test"
    ) as client:
        return await client.post(path, json=payload)


async def test_voices_endpoint_aggregates_with_availability(monkeypatch):
    ok = FakeProvider("mms")
    down = FakeProvider("azure")
    down._available = False

    class Exploding(FakeProvider):
        async def available(self):
            raise ConnectionError("nope")

    chain = _FakeChain({"mms": ok, "azure": down, "spitch": Exploding("spitch")})
    app = _app(monkeypatch, chain)
    async with httpx.AsyncClient(
        transport=httpx.ASGITransport(app=app), base_url="http://test"
    ) as client:
        resp = await client.get("/voice/voices")
    assert resp.status_code == 200, resp.text
    providers = {p["name"]: p for p in resp.json()["providers"]}
    assert providers["mms"]["available"] is True
    assert providers["mms"]["voices"][0]["id"] == "dv"
    assert providers["azure"] == {"name": "azure", "available": False, "voices": []}
    assert providers["spitch"]["available"] is False  # probe failure contained


async def test_tts_preview_resolves_voice_map(monkeypatch):
    chain = _FakeChain({"mms": FakeProvider("mms"), "piper": FakeProvider("piper")})
    settings = Settings(tts_voice_map={"pcm": "mms:pcm"})
    app = _app(monkeypatch, chain, settings)
    resp = await _post(app, "/voice/tts-preview", {"text": "how far", "language": "pcm"})
    assert resp.status_code == 200, resp.text
    assert resp.headers["content-type"] == "audio/wav"
    assert resp.content == WAV
    assert chain.calls[0]["voice"] == "mms:pcm"


async def test_tts_preview_explicit_voice_and_provider(monkeypatch):
    chain = _FakeChain({"piper": FakeProvider("piper")})
    app = _app(monkeypatch, chain)
    resp = await _post(
        app,
        "/voice/tts-preview",
        {"text": "hi", "voice": "azure:en-NG-AbeoNeural", "provider": "piper"},
    )
    assert resp.status_code == 200
    assert chain.calls[0]["voice"] == "azure:en-NG-AbeoNeural"
    assert chain.calls[0]["provider"] == "piper"


async def test_tts_preview_errors(monkeypatch):
    chain = _FakeChain({"piper": FakeProvider("piper")})
    app = _app(monkeypatch, chain)
    # unknown provider pin -> 400
    resp = await _post(app, "/voice/tts-preview", {"text": "hi", "provider": "nope"})
    assert resp.status_code == 400
    # chain failure -> 502
    resp = await _post(app, "/voice/tts-preview", {"text": "explode"})
    assert resp.status_code == 502
    # empty text -> 422 (pydantic min_length)
    resp = await _post(app, "/voice/tts-preview", {"text": ""})
    assert resp.status_code == 422


async def test_enroll_requires_xtts(monkeypatch):
    chain = _FakeChain({"piper": FakeProvider("piper")})
    app = _app(monkeypatch, chain)
    resp = await _post(
        app,
        "/voice/voices/enroll",
        {"name": "Acme", "sample_base64": "QUJD", "tenant": "acme"},
    )
    assert resp.status_code == 400
    assert "xtts" in resp.json()["detail"]


async def test_enroll_happy_path_and_base64_guard(monkeypatch):
    class FakeXtts(FakeProvider):
        def __init__(self):
            super().__init__("xtts")
            self.enrolled = []

        async def enroll_voice(self, name, sample_base64):
            self.enrolled.append((name, sample_base64))
            return "voice-42"

    xtts = FakeXtts()
    chain = _FakeChain({"xtts": xtts, "piper": FakeProvider("piper")})
    app = _app(monkeypatch, chain)

    resp = await _post(
        app,
        "/voice/voices/enroll",
        {"name": "Acme", "sample_base64": "not base64!!", "tenant": "acme"},
    )
    assert resp.status_code == 400

    resp = await _post(
        app,
        "/voice/voices/enroll",
        {"name": "Acme", "sample_base64": "QUJD", "tenant": "acme"},
    )
    assert resp.status_code == 200, resp.text
    assert resp.json() == {"voice_id": "voice-42"}
    assert xtts.enrolled == [("Acme", "QUJD")]


def test_metrics_series_registered():
    registry = metrics.reset_registry()
    metrics.tts_provider_failure("mms")
    rendered = registry.render()
    assert "# TYPE tts_provider_failures_total counter" in rendered
    assert 'tts_provider_failures_total{provider="mms"} 1' in rendered
