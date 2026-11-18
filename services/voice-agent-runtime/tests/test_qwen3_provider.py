"""SPEC-W74 A1 tests: Qwen3-TTS (nari) GPU-tier provider client (offline,
httpx.MockTransport), chain registration (PROVIDER_NAMES + factory), config
parsing, and chain failover engagement.

All HTTP is mocked — live GPU (H100) validation is on the pre-deploy
register (SPEC-W74 Honest register).
"""

from __future__ import annotations

import io
import json
import wave

import httpx
import pytest

from app.config import Settings, load_settings
from app.tts_providers.base import split_voice_spec
from app.tts_providers.chain import (
    PROVIDER_NAMES,
    FallbackTTS,
    build_fallback_tts,
    build_provider,
    parse_chain,
)
from app.tts_providers.qwen3 import Qwen3TTS


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


# ---------------------------------------------------------------------------
# Provider client (httpx.MockTransport)
# ---------------------------------------------------------------------------
async def test_qwen3_synthesize_request_shape_exact():
    """OpenAI-shaped POST /v1/audio/speech with the exact nari key set."""
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["url"] = str(request.url)
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, content=WAV, headers={"content-type": "audio/wav"})

    qwen = Qwen3TTS("http://qwen3:8000/", client=_mock_client(handler))
    wav = await qwen.synthesize("Hello there.", "ryan", "english")
    assert wav == WAV  # success bytes passthrough
    assert seen["method"] == "POST"
    # base_url trailing slash stripped
    assert seen["url"] == "http://qwen3:8000/v1/audio/speech"
    assert seen["body"] == {
        "model": "Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice",
        "input": "Hello there.",
        "voice": "ryan",
        "language": "english",
        "response_format": "wav",
        "stream": False,
    }


async def test_qwen3_synthesize_defaults_fill_voice_and_language():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, content=WAV)

    qwen = Qwen3TTS(
        "http://qwen3:8000",
        model="m1",
        default_voice="sohee",
        default_language="english",
        client=_mock_client(handler),
    )
    assert await qwen.synthesize("hi", "", "") == WAV
    assert seen["body"]["voice"] == "sohee"
    assert seen["body"]["language"] == "english"
    assert seen["body"]["model"] == "m1"


async def test_qwen3_synthesize_empty_text_short_circuits():
    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return httpx.Response(200, content=WAV)

    qwen = Qwen3TTS("http://qwen3", client=_mock_client(handler))
    assert await qwen.synthesize("   ", "ryan", "english") == b""
    assert calls == []


async def test_qwen3_non_200_raises():
    qwen = Qwen3TTS(
        "http://qwen3:8000",
        client=_mock_client(lambda r: httpx.Response(503, json={"detail": "not ready"})),
    )
    with pytest.raises(httpx.HTTPStatusError) as exc_info:
        await qwen.synthesize("hi", "ryan", "english")
    assert exc_info.value.response.status_code == 503


async def test_qwen3_timeout_raises():
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ReadTimeout("slow gpu", request=request)

    qwen = Qwen3TTS("http://qwen3:8000", client=_mock_client(handler))
    with pytest.raises(httpx.ReadTimeout):
        await qwen.synthesize("hi", "ryan", "english")


async def test_qwen3_chain_failover_engages():
    """A qwen3 failure must fail over through FallbackTTS to the next
    provider (recording fallback proves the hop)."""
    qwen = Qwen3TTS(
        "http://qwen3:8000",
        client=_mock_client(lambda r: httpx.Response(500, text="boom")),
    )

    class _RecordingFallback:
        name = "fake"
        default_voice = "fv"

        def __init__(self) -> None:
            self.calls: list[tuple[str, str, str]] = []

        async def synthesize(self, text: str, voice: str, language: str) -> bytes:
            self.calls.append((text, voice, language))
            return WAV

        async def list_voices(self):
            return []

        async def available(self) -> bool:
            return True

        async def aclose(self) -> None:
            pass

    fallback = _RecordingFallback()
    chain = FallbackTTS(
        {"qwen3": qwen, "fake": fallback},
        ["qwen3", "fake"],
        failure_threshold=3,
        cooldown_s=60.0,
    )
    wav = await chain.synthesize("Hello.")
    assert wav == WAV
    assert fallback.calls == [("Hello.", "fv", "")]
    assert chain.breakers["qwen3"]._failures == 1
    await chain.aclose()


async def test_qwen3_available_ready_probe():
    ok = Qwen3TTS(
        "http://qwen3:8000",
        client=_mock_client(lambda r: httpx.Response(200, json={"ready": True})),
    )
    assert await ok.available() is True
    booting = Qwen3TTS(
        "http://qwen3:8000",
        client=_mock_client(lambda r: httpx.Response(503, json={"ready": False})),
    )
    assert await booting.available() is False
    down = Qwen3TTS(
        "http://qwen3:8000",
        client=_mock_client(
            lambda r: (_ for _ in ()).throw(httpx.ConnectError("x"))
        ),
    )
    assert await down.available() is False


async def test_qwen3_available_probes_ready_path():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["method"] = request.method
        return httpx.Response(200)

    qwen = Qwen3TTS("http://qwen3:8000", client=_mock_client(handler))
    assert await qwen.available() is True
    assert (seen["method"], seen["url"]) == ("GET", "http://qwen3:8000/ready")


async def test_qwen3_list_voices_static_default():
    qwen = Qwen3TTS("http://qwen3:8000", client=_mock_client(lambda r: httpx.Response(404)))
    voices = await qwen.list_voices()
    assert [v.id for v in voices] == ["ryan"]
    assert voices[0].languages == ["english"]
    assert voices[0].labels["engine"] == "qwen3-tts"


# ---------------------------------------------------------------------------
# Chain registration
# ---------------------------------------------------------------------------
def test_qwen3_in_provider_names():
    assert "qwen3" in PROVIDER_NAMES
    assert PROVIDER_NAMES == (
        "piper", "mms", "xtts", "azure", "spitch", "cosyvoice", "qwen3"
    )


def test_parse_chain_accepts_qwen3():
    assert parse_chain("qwen3,piper") == ["qwen3", "piper"]
    assert parse_chain("qwen3:ryan") == ["piper"]  # not a chain name


def test_build_provider_qwen3_uses_settings():
    settings = Settings(
        qwen3_tts_url="http://q3:9999",
        qwen3_tts_model="m-x",
        qwen3_tts_voice="sohee",
        qwen3_tts_language="english",
        qwen3_tts_timeout_s=12.5,
    )
    provider = build_provider("qwen3", settings)
    assert isinstance(provider, Qwen3TTS)
    assert provider.name == "qwen3"
    assert provider.base_url == "http://q3:9999"
    assert provider.model == "m-x"
    assert provider.default_voice == "sohee"
    assert provider._timeout == 12.5


def test_build_provider_unknown_pin_valueerror_preserved():
    settings = Settings()
    with pytest.raises(ValueError, match="unknown TTS provider"):
        build_provider("nope", settings)


async def test_factory_chain_with_qwen3():
    settings = Settings(tts_provider_chain="qwen3")
    chain = build_fallback_tts(settings)
    assert chain.order == ["qwen3", "piper"]
    assert isinstance(chain.providers["qwen3"], Qwen3TTS)
    await chain.aclose()


async def test_unknown_provider_pin_valueerror_on_synthesize():
    qwen = Qwen3TTS("http://qwen3:8000", client=_mock_client(lambda r: httpx.Response(200, content=WAV)))
    chain = FallbackTTS({"qwen3": qwen}, ["qwen3"])
    with pytest.raises(ValueError, match="unknown TTS provider"):
        await chain.synthesize("hi", provider="nope")
    await chain.aclose()


def test_split_voice_spec_qwen3():
    assert split_voice_spec("qwen3:ryan") == ("qwen3", "ryan")


async def test_qwen3_voice_spec_routing():
    """`qwen3:ryan` pins qwen3 first and passes `ryan` as its voice id."""
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, content=WAV)

    qwen = Qwen3TTS("http://qwen3:8000", client=_mock_client(handler))
    chain = FallbackTTS({"qwen3": qwen}, ["qwen3"])
    wav = await chain.synthesize("Hello.", voice="qwen3:ryan", language="english")
    assert wav == WAV
    assert seen["body"]["voice"] == "ryan"
    await chain.aclose()


# ---------------------------------------------------------------------------
# Config parsing
# ---------------------------------------------------------------------------
def test_qwen3_settings_defaults():
    s = Settings()
    assert s.qwen3_tts_url == "http://qwen3-tts:8000"
    assert s.qwen3_tts_model == "Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice"
    assert s.qwen3_tts_voice == "ryan"
    assert s.qwen3_tts_language == "english"
    assert s.qwen3_tts_timeout_s == 30.0


def test_qwen3_settings_from_env(monkeypatch):
    monkeypatch.setenv("QWEN3_TTS_URL", "http://custom:9000")
    monkeypatch.setenv("QWEN3_TTS_MODEL", "m-env")
    monkeypatch.setenv("QWEN3_TTS_VOICE", "aiden")
    monkeypatch.setenv("QWEN3_TTS_LANGUAGE", "english")
    monkeypatch.setenv("QWEN3_TTS_TIMEOUT_S", "45")
    s = load_settings()
    assert s.qwen3_tts_url == "http://custom:9000"
    assert s.qwen3_tts_model == "m-env"
    assert s.qwen3_tts_voice == "aiden"
    assert s.qwen3_tts_language == "english"
    assert s.qwen3_tts_timeout_s == 45.0
    # Bad float falls back to default (W70 A1 _env_float contract).
    monkeypatch.setenv("QWEN3_TTS_TIMEOUT_S", "not-a-number")
    assert load_settings().qwen3_tts_timeout_s == 30.0


def test_multilang_routes_qwen3_spec_unchanged():
    """G5 multilang half: parse_tts_voice_map is provider-prefix-generic —
    `qwen3:ryan` parses with ZERO multilang changes."""
    from app.multilang import parse_tts_voice_map

    assert parse_tts_voice_map('{"en": "qwen3:ryan"}') == {"en": "qwen3:ryan"}
