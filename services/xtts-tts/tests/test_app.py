"""Mock-mode contract tests for the XTTS-TTS sidecar.

Run:  pip install fastapi httpx pytest soundfile numpy
      XTTS_MOCK=1 pytest tests/ -q
The Coqui TTS package is NOT required — it is imported lazily, only when
XTTS_MOCK=0.
"""

import base64
import io
import os
import tempfile

# Module-level app creation reads these at import time; point the registry at
# a throwaway dir and force mock mode before importing app.main.
os.environ["XTTS_MOCK"] = "1"
os.environ.setdefault("VOICES_DIR", tempfile.mkdtemp(prefix="xtts-test-import-"))

import numpy as np
import pytest
import soundfile as sf
from fastapi.testclient import TestClient

from app.main import create_app
from app.registry import VoiceRegistry


def _sample_b64(seed_text: str = "consenting speaker sample audio ") -> str:
    # >1000 bytes of deterministic pseudo-audio (content is not validated in
    # mock mode; real mode feeds it to XTTS as the reference wav).
    repeats = 1200 // len(seed_text) + 2
    return base64.b64encode((seed_text * repeats).encode("utf-8")).decode("ascii")


@pytest.fixture()
def client(tmp_path, monkeypatch):
    monkeypatch.setenv("XTTS_MOCK", "1")
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
        assert "en" in body["supported_languages"]


class TestVoiceRegistryCRUD:
    def test_enroll_returns_voice_id(self, client):
        resp = client.post("/voices", json={"name": "Brand Voice", "sample_base64": _sample_b64()})
        assert resp.status_code == 201
        body = resp.json()
        assert body["voice_id"]
        assert body["voice"]["name"] == "Brand Voice"
        assert body["voice"]["gender"] == "cloned"

    def test_enroll_persists_sample_and_json(self, client, tmp_path):
        voice_id = client.post(
            "/voices", json={"name": "A", "sample_base64": _sample_b64()}
        ).json()["voice_id"]
        assert (tmp_path / "voices.json").exists()
        sample = tmp_path / "samples" / f"{voice_id}.wav"
        assert sample.exists() and sample.stat().st_size > 1000

    def test_list_voices_shape(self, client):
        client.post("/voices", json={"name": "One", "sample_base64": _sample_b64("a ")})
        client.post("/voices", json={"name": "Two", "sample_base64": _sample_b64("b ")})
        voices = client.get("/voices").json()["voices"]
        assert len(voices) == 2
        for voice in voices:
            assert set(voice) >= {"id", "name", "languages", "gender", "labels", "created_at"}
            assert "en" in voice["languages"]

    def test_enroll_invalid_base64(self, client):
        resp = client.post("/voices", json={"name": "Bad", "sample_base64": "!!!not-b64!!!"})
        assert resp.status_code == 400
        assert "base64" in resp.json()["detail"]

    def test_enroll_tiny_sample_rejected(self, client):
        tiny = base64.b64encode(b"too small").decode()
        resp = client.post("/voices", json={"name": "Tiny", "sample_base64": tiny})
        assert resp.status_code == 400
        assert "too small" in resp.json()["detail"]

    def test_enroll_missing_fields(self, client):
        assert client.post("/voices", json={"name": "x"}).status_code == 422
        assert client.post("/voices", json={"sample_base64": "eA=="}).status_code == 422

    def test_delete_removes_entry_and_sample(self, client, tmp_path):
        voice_id = client.post(
            "/voices", json={"name": "Bye", "sample_base64": _sample_b64()}
        ).json()["voice_id"]
        resp = client.request("DELETE", f"/voices/{voice_id}")
        assert resp.status_code == 200
        assert resp.json()["deleted"] == voice_id
        assert not (tmp_path / "samples" / f"{voice_id}.wav").exists()
        ids = {v["id"] for v in client.get("/voices").json()["voices"]}
        assert voice_id not in ids

    def test_delete_unknown_voice_404(self, client):
        resp = client.delete("/voices/does-not-exist")
        assert resp.status_code == 404

    def test_registry_persists_across_instances(self, client, tmp_path):
        voice_id = client.post(
            "/voices", json={"name": "Persisted", "sample_base64": _sample_b64()}
        ).json()["voice_id"]
        # A fresh registry over the same dir must see the enrolled voice.
        reloaded = VoiceRegistry(tmp_path)
        entry = reloaded.get(voice_id)
        assert entry is not None and entry["name"] == "Persisted"


class TestTTS:
    def _enroll(self, client, tag: str) -> str:
        return client.post(
            "/voices", json={"name": f"V-{tag}", "sample_base64": _sample_b64(tag)}
        ).json()["voice_id"]

    def test_tts_valid_wav(self, client):
        voice_id = self._enroll(client, "tts")
        resp = client.post(
            "/tts", json={"text": "Welcome to OpenDesk.", "voice_id": voice_id, "language": "en"}
        )
        assert resp.status_code == 200
        assert resp.headers["content-type"] == "audio/wav"
        assert resp.content[:4] == b"RIFF" and resp.content[8:12] == b"WAVE"
        data, rate, info = _decode_wav(resp.content)
        assert rate == 22050 and info.channels == 1 and info.subtype == "PCM_16"
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
        resp = client.post("/tts", json={"text": "hi", "voice_id": voice_id, "language": "yo"})
        assert resp.status_code == 400
        assert "unsupported language" in resp.json()["detail"]

    def test_tts_validation(self, client):
        assert client.post("/tts", json={"text": "", "voice_id": "x", "language": "en"}).status_code == 422
        assert client.post("/tts", json={"voice_id": "x", "language": "en"}).status_code == 422
        assert client.post("/tts", json={"text": "x" * 5001, "voice_id": "x", "language": "en"}).status_code == 422
