"""Mock-mode contract tests for the LongCat-TTS sidecar (SPEC-W102 C1).

Run:  pip install fastapi httpx pytest soundfile numpy
      LONGCAT_MOCK=1 pytest tests/ -q
The audiodit/transformers/torch stack is NOT required — it is imported
lazily, only when LONGCAT_MOCK=0 (GPU inference, env-blocked in CI).
"""

import base64
import io
import json
import os
import tempfile

# Module-level app creation reads these at import time; point the registry at
# a throwaway dir and force mock mode before importing app.main.
os.environ["LONGCAT_MOCK"] = "1"
os.environ.setdefault("VOICES_DIR", tempfile.mkdtemp(prefix="longcat-test-import-"))

import numpy as np
import pytest
import soundfile as sf
from fastapi.testclient import TestClient

from app.main import create_app
from app.registry import LONGCAT_LANGUAGES, VoiceRegistry


def _sample_b64(seed_text: str = "consenting speaker sample audio ") -> str:
    # >1000 bytes of deterministic pseudo-audio (content is not validated in
    # mock mode; real mode feeds it to CosyVoice-2 as the zero-shot reference).
    repeats = 1200 // len(seed_text) + 2
    return base64.b64encode((seed_text * repeats).encode("utf-8")).decode("ascii")


def _enroll_payload(tag: str = "Brand Voice") -> dict:
    return {
        "name": tag,
        "sample_base64": _sample_b64(),
        "consent_basis": "signed voice-release form 2026-11-16 (docs/VOICES.md)",
        "source": "staff-consented studio recording",
        "license": "proprietary (consent on file)",
    }


@pytest.fixture()
def client(tmp_path, monkeypatch):
    monkeypatch.setenv("LONGCAT_MOCK", "1")
    monkeypatch.setenv("VOICES_DIR", str(tmp_path))
    return TestClient(create_app())


def _decode_wav(payload: bytes):
    data, rate = sf.read(io.BytesIO(payload), dtype="int16")
    info = sf.info(io.BytesIO(payload))
    return data, rate, info


class TestHealthz:
    def test_healthz(self, client):
        resp = client.get("/healthz")
        assert resp.status_code == 200
        body = resp.json()
        assert body["status"] == "ok"
        assert body["mock"] is True
        assert body["service"] == "longcat-tts"
        assert "en" in body["supported_languages"]


class TestConsentFailClosed:
    """SPEC-W102 C1: enrollment without a non-empty consent_basis
    is rejected 422 BEFORE anything is persisted."""

    def test_missing_consent_basis_422(self, client, tmp_path):
        payload = _enroll_payload()
        del payload["consent_basis"]
        resp = client.post("/voices", json=payload)
        assert resp.status_code == 422
        assert client.get("/voices").json()["voices"] == []
        assert not (tmp_path / "voices.json").exists()

    def test_empty_consent_basis_422(self, client):
        payload = _enroll_payload()
        payload["consent_basis"] = ""
        resp = client.post("/voices", json=payload)
        assert resp.status_code == 422

    def test_whitespace_consent_basis_422(self, client):
        payload = _enroll_payload()
        payload["consent_basis"] = "   \n\t "
        resp = client.post("/voices", json=payload)
        assert resp.status_code == 422

    def test_missing_source_422(self, client):
        payload = _enroll_payload()
        del payload["source"]
        resp = client.post("/voices", json=payload)
        assert resp.status_code == 422


class TestVoiceRegistryCRUD:
    def test_enroll_returns_voice_id_and_consent_metadata(self, client):
        resp = client.post("/voices", json=_enroll_payload())
        assert resp.status_code == 201
        body = resp.json()
        assert body["voice_id"]
        voice = body["voice"]
        assert set(voice) >= {
            "voice_id", "name", "languages", "source",
            "license", "consent_basis", "prompt_text", "enrolled_at",
        }
        assert voice["voice_id"] == body["voice_id"]
        assert voice["name"] == "Brand Voice"
        assert voice["consent_basis"].startswith("signed voice-release")
        assert voice["source"] == "staff-consented studio recording"
        assert voice["license"] == "proprietary (consent on file)"
        assert voice["languages"] == list(LONGCAT_LANGUAGES)

    def test_enroll_records_prompt_text(self, client):
        """SPEC-W102: the sample transcript is stored verbatim — LongCat's
        cloning concatenates prompt_text + gen_text, so it is the cloning
        conditioning (empty = honest plain-TTS fallback on the real path)."""
        payload = {**_enroll_payload(), "prompt_text": "This is the speaker."}
        voice = client.post("/voices", json=payload).json()["voice"]
        assert voice["prompt_text"] == "This is the speaker."
        # Absent by default: recorded as "" (never invented).
        voice2 = client.post("/voices", json=_enroll_payload("NoTx")).json()["voice"]
        assert voice2["prompt_text"] == ""

    def test_enroll_custom_languages(self, client):
        payload = {**_enroll_payload(), "languages": ["en", "pcm"]}
        voice = client.post("/voices", json=payload).json()["voice"]
        assert voice["languages"] == ["en", "pcm"]

    def test_enroll_persists_sample_and_json(self, client, tmp_path):
        voice_id = client.post("/voices", json=_enroll_payload()).json()["voice_id"]
        raw = json.loads((tmp_path / "voices.json").read_text(encoding="utf-8"))
        entry = raw["voices"][0]
        assert entry["voice_id"] == voice_id
        assert entry["consent_basis"]
        sample = tmp_path / "samples" / f"{voice_id}.wav"
        assert sample.exists() and sample.stat().st_size > 1000

    def test_list_voices_shape(self, client):
        client.post("/voices", json=_enroll_payload("One"))
        client.post("/voices", json=_enroll_payload("Two"))
        voices = client.get("/voices").json()["voices"]
        assert len(voices) == 2
        for voice in voices:
            assert set(voice) >= {
                "voice_id", "name", "languages", "source",
                "license", "consent_basis", "enrolled_at",
            }
            assert "en" in voice["languages"]

    def test_enroll_invalid_base64(self, client):
        payload = _enroll_payload()
        payload["sample_base64"] = "!!!not-b64!!!"
        resp = client.post("/voices", json=payload)
        assert resp.status_code == 400
        assert "base64" in resp.json()["detail"]

    def test_enroll_tiny_sample_rejected(self, client):
        payload = _enroll_payload()
        payload["sample_base64"] = base64.b64encode(b"too small").decode()
        resp = client.post("/voices", json=payload)
        assert resp.status_code == 400
        assert "too small" in resp.json()["detail"]

    def test_enroll_missing_fields(self, client):
        assert client.post("/voices", json={"name": "x"}).status_code == 422
        assert client.post("/voices", json={"sample_base64": "eA=="}).status_code == 422

    def test_delete_removes_entry_and_sample(self, client, tmp_path):
        voice_id = client.post("/voices", json=_enroll_payload("Bye")).json()["voice_id"]
        resp = client.request("DELETE", f"/voices/{voice_id}")
        assert resp.status_code == 200
        assert resp.json()["deleted"] == voice_id
        assert not (tmp_path / "samples" / f"{voice_id}.wav").exists()
        ids = {v["voice_id"] for v in client.get("/voices").json()["voices"]}
        assert voice_id not in ids

    def test_delete_unknown_voice_404(self, client):
        resp = client.delete("/voices/does-not-exist")
        assert resp.status_code == 404

    def test_registry_persists_across_instances(self, client, tmp_path):
        voice_id = client.post("/voices", json=_enroll_payload("Persisted")).json()["voice_id"]
        # A fresh registry over the same dir must see the enrolled voice.
        reloaded = VoiceRegistry(tmp_path)
        entry = reloaded.get(voice_id)
        assert entry is not None and entry["name"] == "Persisted"
        assert entry["consent_basis"]

    def test_registry_add_rejects_blank_consent_directly(self, tmp_path):
        """Defense in depth: the registry itself refuses blank consent even
        if a caller bypasses the HTTP validation layer."""
        registry = VoiceRegistry(tmp_path)
        with pytest.raises(Exception, match="consent_basis"):
            registry.add("x", b"y" * 2000, consent_basis="  ", source="test")
        assert registry.list() == []


class TestTTS:
    def _enroll(self, client, tag: str) -> str:
        payload = _enroll_payload(f"V-{tag}")
        payload["sample_base64"] = _sample_b64(tag)
        return client.post("/voices", json=payload).json()["voice_id"]

    def test_tts_valid_wav(self, client):
        voice_id = self._enroll(client, "tts")
        resp = client.post(
            "/tts", json={"text": "Welcome to OpenDesk.", "voice_id": voice_id, "language": "en"}
        )
        assert resp.status_code == 200
        assert resp.headers["content-type"] == "audio/wav"
        assert resp.content[:4] == b"RIFF" and resp.content[8:12] == b"WAVE"
        data, rate, info = _decode_wav(resp.content)
        assert rate == 24000 and info.channels == 1 and info.subtype == "PCM_16"
        assert np.abs(data).max() > 1000

    def test_tts_voice_specific_output(self, client):
        v1 = self._enroll(client, "one")
        v2 = self._enroll(client, "two")
        payload = {"text": "same sentence", "language": "en"}
        a = client.post("/tts", json={**payload, "voice_id": v1}).content
        b = client.post("/tts", json={**payload, "voice_id": v2}).content
        assert a != b  # voice_id-keyed pitch

    def test_tts_deterministic(self, client):
        voice_id = self._enroll(client, "det")
        payload = {"text": "repeat after me", "voice_id": voice_id, "language": "en"}
        assert client.post("/tts", json=payload).content == client.post("/tts", json=payload).content

    def test_tts_duration_scales_with_text(self, client):
        voice_id = self._enroll(client, "dur")
        short = client.post("/tts", json={"text": "hi", "voice_id": voice_id, "language": "en"}).content
        long = client.post(
            "/tts", json={"text": "a much longer utterance " * 12, "voice_id": voice_id, "language": "en"}
        ).content
        assert len(_decode_wav(long)[0]) > len(_decode_wav(short)[0]) * 3

    def test_tts_unknown_voice_404(self, client):
        resp = client.post("/tts", json={"text": "hi", "voice_id": "nope", "language": "en"})
        assert resp.status_code == 404

    def test_tts_unsupported_language_400(self, client):
        voice_id = self._enroll(client, "lang")
        resp = client.post("/tts", json={"text": "hi", "voice_id": voice_id, "language": "pcm"})
        assert resp.status_code == 400
        assert "unsupported language" in resp.json()["detail"]

    def test_tts_validation(self, client):
        assert client.post("/tts", json={"text": "", "voice_id": "x", "language": "en"}).status_code == 422
        assert client.post("/tts", json={"voice_id": "x", "language": "en"}).status_code == 422
        assert client.post("/tts", json={"text": "x" * 5001, "voice_id": "x", "language": "en"}).status_code == 422
