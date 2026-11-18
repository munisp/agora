"""SPEC-W72 A2 tests: CosyVoice-2 TTS provider client (mocked httpx), chain
registration (PROVIDER_NAMES + factory), and config parsing
(COSYVOICE_TTS_URL default / env override)."""

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
    build_fallback_tts,
    build_provider,
    parse_chain,
)
from app.tts_providers.cosyvoice import CosyVoiceTTS


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
async def test_cosyvoice_synthesize_shape():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, content=WAV)

    cosy = CosyVoiceTTS("http://cosyvoice:5820/", client=_mock_client(handler))
    wav = await cosy.synthesize("hello", "brand-1", "en")
    assert wav == WAV
    # base_url trailing slash stripped
    assert seen["url"] == "http://cosyvoice:5820/tts"
    assert seen["body"] == {"text": "hello", "voice_id": "brand-1", "language": "en"}


async def test_cosyvoice_synthesize_requires_voice():
    cosy = CosyVoiceTTS("http://cosyvoice", client=_mock_client(lambda r: httpx.Response(500)))
    with pytest.raises(RuntimeError, match="voice_id"):
        await cosy.synthesize("hi", "", "en")

    # default_voice satisfies the requirement; empty language defaults to "en"
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, content=WAV)

    cosy = CosyVoiceTTS(
        "http://cosyvoice", default_voice="dv", client=_mock_client(handler)
    )
    assert await cosy.synthesize("hi", "", "") == WAV
    assert seen["body"]["voice_id"] == "dv"
    assert seen["body"]["language"] == "en"


async def test_cosyvoice_synthesize_empty_text_short_circuits():
    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return httpx.Response(200, content=WAV)

    cosy = CosyVoiceTTS("http://cosyvoice", client=_mock_client(handler))
    assert await cosy.synthesize("   ", "v1", "en") == b""
    assert calls == []


async def test_cosyvoice_enroll_forwards_consent():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = json.loads(request.content)
        return httpx.Response(201, json={"voice_id": "voice-7"})

    cosy = CosyVoiceTTS("http://cosyvoice", client=_mock_client(handler))
    voice_id = await cosy.enroll_voice(
        "Acme Brand",
        "QUJD",
        consent_basis="signed release 2026-11-16",
        source="staff recording",
    )
    assert voice_id == "voice-7"
    assert seen["body"] == {
        "name": "Acme Brand",
        "sample_base64": "QUJD",
        "consent_basis": "signed release 2026-11-16",
        "source": "staff recording",
    }


async def test_cosyvoice_enroll_sidecar_rejection_propagates():
    """Fail-closed consent: the sidecar 422s a blank consent_basis and the
    client surfaces it (no swallowing)."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(422, json={"detail": "consent_basis required"})

    cosy = CosyVoiceTTS("http://cosyvoice", client=_mock_client(handler))
    with pytest.raises(httpx.HTTPStatusError) as exc_info:
        await cosy.enroll_voice("Acme", "QUJD", consent_basis="", source="x")
    assert exc_info.value.response.status_code == 422


async def test_cosyvoice_enroll_missing_voice_id_raises():
    cosy = CosyVoiceTTS(
        "http://cosyvoice",
        client=_mock_client(lambda r: httpx.Response(200, json={"nope": 1})),
    )
    with pytest.raises(RuntimeError, match="missing voice_id"):
        await cosy.enroll_voice("Acme", "QUJD", consent_basis="c", source="s")


async def test_cosyvoice_delete_and_error_status():
    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "DELETE":
            return httpx.Response(200, json={"deleted": "voice-7"})
        return httpx.Response(404)

    cosy = CosyVoiceTTS("http://cosyvoice", client=_mock_client(handler))
    await cosy.delete_voice("voice-7")
    with pytest.raises(httpx.HTTPStatusError):
        await cosy.synthesize("hi", "voice-7", "en")


async def test_cosyvoice_list_voices_maps_consent_records():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "voices": [
                    {
                        "voice_id": "v1",
                        "name": "Brand",
                        "languages": ["en"],
                        "source": "staff recording",
                        "license": "proprietary",
                        "consent_basis": "signed release",
                        "enrolled_at": "2026-11-16T00:00:00+00:00",
                    },
                    {"broken": True},
                ]
            },
        )

    cosy = CosyVoiceTTS("http://cosyvoice", client=_mock_client(handler))
    voices = await cosy.list_voices()
    assert [v.id for v in voices] == ["v1"]
    assert voices[0].languages == ["en"]
    assert voices[0].labels["source"] == "staff recording"
    assert voices[0].labels["license"] == "proprietary"


async def test_cosyvoice_available_probe():
    ok = CosyVoiceTTS(
        "http://cosyvoice", client=_mock_client(lambda r: httpx.Response(200, json={"voices": []}))
    )
    assert await ok.available() is True
    down = CosyVoiceTTS(
        "http://cosyvoice",
        client=_mock_client(lambda r: (_ for _ in ()).throw(httpx.ConnectError("x"))),
    )
    assert await down.available() is False


# ---------------------------------------------------------------------------
# Chain registration
# ---------------------------------------------------------------------------
def test_cosyvoice_in_provider_names():
    assert "cosyvoice" in PROVIDER_NAMES


def test_parse_chain_accepts_cosyvoice():
    assert parse_chain("cosyvoice,piper") == ["cosyvoice", "piper"]
    assert parse_chain("cosyvoice:mylabel") == ["piper"]  # not a chain name


def test_build_provider_cosyvoice_uses_settings_url():
    settings = Settings(cosyvoice_tts_url="http://cv:9999")
    provider = build_provider("cosyvoice", settings)
    assert isinstance(provider, CosyVoiceTTS)
    assert provider.name == "cosyvoice"
    assert provider.base_url == "http://cv:9999"


async def test_factory_chain_with_cosyvoice():
    settings = Settings(tts_provider_chain="cosyvoice")
    chain = build_fallback_tts(settings)
    assert chain.order == ["cosyvoice", "piper"]
    assert isinstance(chain.providers["cosyvoice"], CosyVoiceTTS)
    await chain.aclose()


def test_split_voice_spec_cosyvoice():
    assert split_voice_spec("cosyvoice:abc123") == ("cosyvoice", "abc123")


# ---------------------------------------------------------------------------
# Config parsing
# ---------------------------------------------------------------------------
def test_cosyvoice_tts_url_default():
    assert Settings().cosyvoice_tts_url == "http://cosyvoice-tts:5820"


def test_cosyvoice_tts_url_from_env(monkeypatch):
    monkeypatch.setenv("COSYVOICE_TTS_URL", "http://cosyvoice-tts:5820")
    assert load_settings().cosyvoice_tts_url == "http://cosyvoice-tts:5820"
    monkeypatch.setenv("COSYVOICE_TTS_URL", "http://custom:9999")
    assert load_settings().cosyvoice_tts_url == "http://custom:9999"
