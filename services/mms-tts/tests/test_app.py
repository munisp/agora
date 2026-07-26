"""Mock-mode contract tests for the MMS-TTS sidecar.

Run:  pip install fastapi httpx pytest soundfile numpy
      MMS_MOCK=1 pytest tests/ -q
Heavy deps (torch/transformers) are NOT required — real-mode imports are lazy.
"""

import io
import os

os.environ["MMS_MOCK"] = "1"

import numpy as np
import pytest
import soundfile as sf
from fastapi.testclient import TestClient

from app.main import create_app

client = TestClient(create_app())


def _decode_wav(payload: bytes):
    data, rate = sf.read(io.BytesIO(payload), dtype="int16")
    info = sf.info(io.BytesIO(payload))
    return data, rate, info


class TestHealthz:
    def test_healthz_shape(self):
        resp = client.get("/healthz")
        assert resp.status_code == 200
        body = resp.json()
        assert body["status"] == "ok"
        assert body["mock"] is True
        assert set(body["supported_langs"]) == {"eng", "pcm", "yor", "ibo", "hau"}


class TestVoices:
    def test_contract_shape(self):
        resp = client.get("/voices")
        assert resp.status_code == 200
        voices = resp.json()["voices"]
        assert len(voices) == 5
        for voice in voices:
            assert set(voice) >= {"id", "languages", "gender", "labels"}
            assert isinstance(voice["languages"], list) and voice["languages"]
            assert isinstance(voice["labels"], list) and voice["labels"]
        ids = {v["id"] for v in voices}
        assert ids == {"eng", "pcm", "yor", "ibo", "hau"}

    def test_language_mappings(self):
        voices = {v["id"]: v for v in client.get("/voices").json()["voices"]}
        assert voices["yor"]["languages"] == ["yo"]
        assert voices["ibo"]["languages"] == ["ig"]
        assert voices["hau"]["languages"] == ["ha"]


class TestTTS:
    @pytest.mark.parametrize("lang", ["eng", "pcm", "yor", "ibo", "hau"])
    def test_valid_wav_all_langs(self, lang):
        resp = client.post("/tts", json={"text": "How far, wetin dey happen?", "lang": lang})
        assert resp.status_code == 200
        assert resp.headers["content-type"] == "audio/wav"
        assert resp.content[:4] == b"RIFF"
        assert resp.content[8:12] == b"WAVE"
        data, rate, info = _decode_wav(resp.content)
        assert rate == 22050
        assert info.channels == 1
        assert info.subtype == "PCM_16"
        assert len(data) > 0
        # Audible signal, not silence.
        assert np.abs(data).max() > 1000

    def test_deterministic(self):
        payload = {"text": "deterministic output please", "lang": "pcm"}
        a = client.post("/tts", json=payload).content
        b = client.post("/tts", json=payload).content
        assert a == b

    def test_lang_keyed_difference(self):
        eng = client.post("/tts", json={"text": "same text", "lang": "eng"}).content
        yor = client.post("/tts", json={"text": "same text", "lang": "yor"}).content
        assert eng != yor

    def test_duration_proportional_to_text_length(self):
        short = client.post("/tts", json={"text": "hi", "lang": "eng"}).content
        long = client.post("/tts", json={"text": "a much longer sentence " * 10, "lang": "eng"}).content
        short_n = len(_decode_wav(short)[0])
        long_n = len(_decode_wav(long)[0])
        assert long_n > short_n * 3

    def test_unknown_lang_rejected(self):
        resp = client.post("/tts", json={"text": "hello", "lang": "fra"})
        assert resp.status_code == 400
        assert "unsupported lang" in resp.json()["detail"]

    def test_empty_text_rejected(self):
        resp = client.post("/tts", json={"text": "", "lang": "eng"})
        assert resp.status_code == 422

    def test_missing_fields_rejected(self):
        assert client.post("/tts", json={"lang": "eng"}).status_code == 422
        assert client.post("/tts", json={"text": "hello"}).status_code == 422
        assert client.post("/tts", json={}).status_code == 422

    def test_overlong_text_rejected(self):
        resp = client.post("/tts", json={"text": "x" * 5001, "lang": "eng"})
        assert resp.status_code == 422


class TestRealModeGuards:
    def test_real_mode_lang_not_enabled_clear_error(self, monkeypatch):
        from app import synth

        monkeypatch.setenv("MMS_MOCK", "0")
        monkeypatch.setenv("MMS_LANGS", "eng")
        real_client = TestClient(create_app())
        # yor is a valid lang but not enabled in real mode; error must not
        # attempt a model download.
        resp = real_client.post("/tts", json={"text": "hello", "lang": "yor"})
        assert resp.status_code == 400
        assert "MMS_LANGS" in resp.json()["detail"]

    def test_bad_mms_langs_fails_fast(self, monkeypatch):
        monkeypatch.setenv("MMS_MOCK", "0")
        monkeypatch.setenv("MMS_LANGS", "eng,klingon")
        with pytest.raises(ValueError, match="unsupported"):
            create_app()
