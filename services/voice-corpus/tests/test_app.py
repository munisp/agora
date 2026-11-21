"""SPEC-W103 C1 tests: voice-corpus service — consent fail-closed speaker
registry, utterance QA gates (wav probe, duration bounds, language allowlist),
tone-mark detection, stats, training-manifest export, and real deletion."""

from __future__ import annotations

import base64
import io
import wave

import pytest
from fastapi.testclient import TestClient

from app.main import create_app, has_tone_marks, probe_wav
from app.registry import CorpusRegistry, RegistryError


def _wav(seconds: float = 1.0, rate: int = 16000, channels: int = 1) -> bytes:
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(channels)
        wf.setsampwidth(2)
        wf.setframerate(rate)
        wf.writeframes(b"\x01\x00" * int(rate * seconds) * channels)
    return buf.getvalue()


def _b64(raw: bytes) -> str:
    return base64.b64encode(raw).decode("ascii")


@pytest.fixture()
def client(tmp_path, monkeypatch):
    monkeypatch.setattr("app.main.CORPUS_DIR", str(tmp_path))
    with TestClient(create_app()) as c:
        yield c


def _speaker(client, pseudonym="speaker-001", languages=None, **overrides):
    payload = {
        "pseudonym": pseudonym,
        "languages": languages or ["yo"],
        "consent_basis": "signed commission agreement 2026-11-21",
        "consent_reference": "DMS-CONSENT-1042",
        "source": "commissioned studio recording",
    }
    payload.update(overrides)
    resp = client.post("/v1/speakers", json=payload)
    assert resp.status_code == 201, resp.text
    return resp.json()


def _utterance(client, speaker_id, language="yo", transcript="Ẹ káàrọ̀", seconds=1.0):
    resp = client.post(
        "/v1/utterances",
        json={
            "speaker_id": speaker_id,
            "language": language,
            "transcript": transcript,
            "audio_base64": _b64(_wav(seconds)),
        },
    )
    assert resp.status_code == 201, resp.text
    return resp.json()


# ---------------------------------------------------------------------------
class TestHealthz:
    def test_healthz(self, client):
        data = client.get("/healthz").json()
        assert data["status"] == "ok"
        assert data["speakers"] == 0
        assert "yo" in data["languages"]


# ---------------------------------------------------------------------------
class TestSpeakerConsent:
    def test_missing_consent_basis_422(self, client):
        resp = client.post(
            "/v1/speakers",
            json={"pseudonym": "x", "languages": ["yo"], "source": "studio"},
        )
        assert resp.status_code == 422

    def test_empty_consent_basis_422(self, client):
        resp = client.post(
            "/v1/speakers",
            json={
                "pseudonym": "x",
                "languages": ["yo"],
                "consent_basis": "",
                "source": "studio",
            },
        )
        assert resp.status_code == 422

    def test_whitespace_consent_basis_422(self, client):
        resp = client.post(
            "/v1/speakers",
            json={
                "pseudonym": "x",
                "languages": ["yo"],
                "consent_basis": "   ",
                "source": "studio",
            },
        )
        assert resp.status_code == 422

    def test_missing_source_422(self, client):
        resp = client.post(
            "/v1/speakers",
            json={
                "pseudonym": "x",
                "languages": ["yo"],
                "consent_basis": "signed",
            },
        )
        assert resp.status_code == 422

    def test_unknown_language_rejected_422(self, client):
        resp = client.post(
            "/v1/speakers",
            json={
                "pseudonym": "x",
                "languages": ["xx"],
                "consent_basis": "signed",
                "source": "studio",
            },
        )
        assert resp.status_code == 422
        assert "unsupported language" in resp.text

    def test_registry_add_rejects_blank_consent_directly(self, tmp_path):
        reg = CorpusRegistry(tmp_path)
        with pytest.raises(RegistryError, match="consent_basis"):
            reg.add_speaker("x", ["yo"], consent_basis="  ", source="s")

    def test_nothing_persisted_on_rejection(self, client, tmp_path):
        client.post(
            "/v1/speakers",
            json={
                "pseudonym": "x",
                "languages": ["yo"],
                "consent_basis": "",
                "source": "studio",
            },
        )
        assert not (tmp_path / "registry.json").exists()


# ---------------------------------------------------------------------------
class TestSpeakers:
    def test_enroll_records_consent_metadata(self, client):
        speaker = _speaker(client)
        assert speaker["speaker_id"].startswith("spk-")
        assert speaker["pseudonym"] == "speaker-001"
        assert speaker["consent_basis"] == "signed commission agreement 2026-11-21"
        assert speaker["consent_reference"] == "DMS-CONSENT-1042"
        assert speaker["source"] == "commissioned studio recording"
        assert speaker["languages"] == ["yo"]

    def test_default_consent_reference_empty_not_invented(self, client):
        speaker = _speaker(client, consent_reference="")
        assert speaker["consent_reference"] == ""

    def test_list_and_get(self, client):
        speaker = _speaker(client)
        listed = client.get("/v1/speakers").json()["speakers"]
        assert [s["speaker_id"] for s in listed] == [speaker["speaker_id"]]
        got = client.get(f"/v1/speakers/{speaker['speaker_id']}")
        assert got.status_code == 200
        assert got.json()["pseudonym"] == "speaker-001"

    def test_get_unknown_404(self, client):
        assert client.get("/v1/speakers/spk-nope").status_code == 404

    def test_delete_unknown_404(self, client):
        assert client.delete("/v1/speakers/spk-nope").status_code == 404


# ---------------------------------------------------------------------------
class TestUtteranceQA:
    def test_enroll_ok_probes_real_audio_facts(self, client):
        speaker = _speaker(client)
        utt = _utterance(client, speaker["speaker_id"], seconds=2.0)
        assert utt["speaker_id"] == speaker["speaker_id"]
        assert utt["language"] == "yo"
        assert utt["duration_s"] == pytest.approx(2.0, abs=0.01)
        assert utt["sample_rate"] == 16000
        assert utt["channels"] == 1
        assert utt["has_tone_marks"] is True  # "Ẹ káàrọ̀" carries tone marks
        assert utt["audio_relpath"].startswith("audio/")

    def test_plain_transcript_has_no_tone_marks(self, client):
        speaker = _speaker(client, languages=["en"])
        utt = _utterance(
            client, speaker["speaker_id"], language="en", transcript="Good morning"
        )
        assert utt["has_tone_marks"] is False

    def test_unknown_speaker_404(self, client):
        resp = client.post(
            "/v1/utterances",
            json={
                "speaker_id": "spk-nope",
                "language": "yo",
                "transcript": "Bawo ni",
                "audio_base64": _b64(_wav()),
            },
        )
        assert resp.status_code == 404

    def test_unsupported_language_400(self, client):
        speaker = _speaker(client)
        resp = client.post(
            "/v1/utterances",
            json={
                "speaker_id": speaker["speaker_id"],
                "language": "xx",
                "transcript": "hello",
                "audio_base64": _b64(_wav()),
            },
        )
        assert resp.status_code == 400

    def test_invalid_base64_400(self, client):
        speaker = _speaker(client)
        resp = client.post(
            "/v1/utterances",
            json={
                "speaker_id": speaker["speaker_id"],
                "language": "yo",
                "transcript": "Bawo ni",
                "audio_base64": "!!!not-base64!!!",
            },
        )
        assert resp.status_code == 400

    def test_non_wav_audio_400(self, client):
        speaker = _speaker(client)
        resp = client.post(
            "/v1/utterances",
            json={
                "speaker_id": speaker["speaker_id"],
                "language": "yo",
                "transcript": "Bawo ni",
                "audio_base64": _b64(b"definitely not a wav file"),
            },
        )
        assert resp.status_code == 400

    def test_duration_too_short_422(self, client):
        speaker = _speaker(client)
        resp = client.post(
            "/v1/utterances",
            json={
                "speaker_id": speaker["speaker_id"],
                "language": "yo",
                "transcript": "Bawo",
                "audio_base64": _b64(_wav(0.1)),
            },
        )
        assert resp.status_code == 422

    def test_duration_too_long_422(self, client):
        speaker = _speaker(client)
        resp = client.post(
            "/v1/utterances",
            json={
                "speaker_id": speaker["speaker_id"],
                "language": "yo",
                "transcript": "long take",
                "audio_base64": _b64(_wav(31.0, rate=8000)),
            },
        )
        assert resp.status_code == 422

    def test_oversized_audio_413(self, client, monkeypatch):
        monkeypatch.setattr("app.main.MAX_AUDIO_BYTES", 100)
        speaker = _speaker(client)
        resp = client.post(
            "/v1/utterances",
            json={
                "speaker_id": speaker["speaker_id"],
                "language": "yo",
                "transcript": "Bawo ni",
                "audio_base64": _b64(_wav(1.0)),
            },
        )
        assert resp.status_code == 413

    def test_blank_transcript_422(self, client):
        speaker = _speaker(client)
        resp = client.post(
            "/v1/utterances",
            json={
                "speaker_id": speaker["speaker_id"],
                "language": "yo",
                "transcript": "   ",
                "audio_base64": _b64(_wav()),
            },
        )
        assert resp.status_code == 422

    def test_list_filters(self, client):
        s1 = _speaker(client, pseudonym="s1", languages=["yo", "en"])
        s2 = _speaker(client, pseudonym="s2", languages=["pcm"])
        u1 = _utterance(client, s1["speaker_id"], language="yo")
        u2 = _utterance(client, s1["speaker_id"], language="en", transcript="Hello")
        u3 = _utterance(client, s2["speaker_id"], language="pcm", transcript="How body")
        by_lang = client.get("/v1/utterances?language=yo").json()["utterances"]
        assert [u["utterance_id"] for u in by_lang] == [u1["utterance_id"]]
        by_speaker = client.get(
            f"/v1/utterances?speaker_id={s1['speaker_id']}"
        ).json()["utterances"]
        assert {u["utterance_id"] for u in by_speaker} == {
            u1["utterance_id"],
            u2["utterance_id"],
        }
        assert u3["language"] == "pcm"


# ---------------------------------------------------------------------------
class TestToneMarks:
    @pytest.mark.parametrize(
        "text,expected",
        [
            ("Ẹ káàrọ̀, bawo ni?", True),   # Yoruba with tone marks
            ("Ọ̀rẹ́ mi ṣé dáa?", True),      # precomposed + combining
            ("Good morning", False),         # plain ASCII
            ("Sannu da safe", False),        # Hausa without marks
            ("Kedu ka ị mere", False),       # Igbo dotted vowel ị (no tone)
        ],
    )
    def test_detection(self, text, expected):
        assert has_tone_marks(text) is expected

    def test_probe_wav_rejects_garbage(self):
        with pytest.raises(Exception):
            probe_wav(b"not a wav")


# ---------------------------------------------------------------------------
class TestStats:
    def test_per_language_stats(self, client):
        s1 = _speaker(client, pseudonym="s1", languages=["yo", "en"])
        _utterance(client, s1["speaker_id"], language="yo", seconds=2.0)
        _utterance(
            client, s1["speaker_id"], language="en", transcript="Hello there",
        )
        data = client.get("/v1/stats").json()
        assert data["speakers"] == 1
        assert data["utterances"] == 2
        yo = data["per_language"]["yo"]
        assert yo["utterances"] == 1
        assert yo["seconds"] == pytest.approx(2.0, abs=0.05)
        assert yo["speakers"] == 1
        assert yo["tone_mark_coverage_pct"] == 100.0
        en = data["per_language"]["en"]
        assert en["tone_mark_coverage_pct"] == 0.0
        assert data["per_language"]["pcm"]["tone_mark_coverage_pct"] is None


# ---------------------------------------------------------------------------
class TestExport:
    def test_jsonl_shape(self, client):
        speaker = _speaker(client)
        utt = _utterance(client, speaker["speaker_id"])
        import json

        body = client.get("/v1/export?format=jsonl&language=yo").text
        rows = [json.loads(line) for line in body.strip().splitlines()]
        assert len(rows) == 1
        row = rows[0]
        assert row["audio_path"] == utt["audio_relpath"]
        assert row["text"] == "Ẹ káàrọ̀"
        assert row["speaker_id"] == speaker["speaker_id"]
        assert row["language"] == "yo"
        assert row["sample_rate"] == 16000

    def test_ljspeech_shape(self, client):
        speaker = _speaker(client)
        utt = _utterance(client, speaker["speaker_id"])
        body = client.get("/v1/export?format=ljspeech").text
        lines = body.strip().splitlines()
        assert lines == [
            f"{utt['audio_relpath']}|Ẹ káàrọ̀|{speaker['speaker_id']}"
        ]

    def test_language_filter(self, client):
        speaker = _speaker(client, languages=["yo", "pcm"])
        _utterance(client, speaker["speaker_id"], language="yo")
        _utterance(client, speaker["speaker_id"], language="pcm", transcript="How body")
        yo_body = client.get("/v1/export?language=yo").text.strip().splitlines()
        pcm_body = client.get("/v1/export?language=pcm").text.strip().splitlines()
        all_body = client.get("/v1/export").text.strip().splitlines()
        assert len(yo_body) == 1 and len(pcm_body) == 1 and len(all_body) == 2
        assert "How body" in pcm_body[0]

    def test_bad_format_400(self, client):
        assert client.get("/v1/export?format=mp3").status_code == 400

    def test_bad_language_filter_400(self, client):
        assert client.get("/v1/export?language=xx").status_code == 400


# ---------------------------------------------------------------------------
class TestDeletion:
    def test_delete_speaker_removes_audio_and_records(self, client, tmp_path):
        speaker = _speaker(client)
        utt = _utterance(client, speaker["speaker_id"])
        audio_file = tmp_path / utt["audio_relpath"]
        assert audio_file.is_file()
        result = client.delete(f"/v1/speakers/{speaker['speaker_id']}").json()
        assert result["deleted_speaker"] == speaker["speaker_id"]
        assert result["deleted_utterances"] == 1
        assert result["deleted_bytes"] > 0
        # REAL deletion: bytes are gone, not tombstoned
        assert not audio_file.exists()
        assert not (tmp_path / "audio" / speaker["speaker_id"]).exists()
        assert client.get("/v1/utterances").json()["utterances"] == []
        assert client.get(f"/v1/speakers/{speaker['speaker_id']}").status_code == 404

    def test_delete_utterance_removes_file(self, client, tmp_path):
        speaker = _speaker(client)
        utt = _utterance(client, speaker["speaker_id"])
        audio_file = tmp_path / utt["audio_relpath"]
        assert audio_file.is_file()
        assert client.delete(f"/v1/utterances/{utt['utterance_id']}").status_code == 200
        assert not audio_file.exists()
        assert client.delete(f"/v1/utterances/{utt['utterance_id']}").status_code == 404


# ---------------------------------------------------------------------------
class TestPersistence:
    def test_registry_survives_reload(self, tmp_path, monkeypatch):
        monkeypatch.setattr("app.main.CORPUS_DIR", str(tmp_path))
        with TestClient(create_app()) as c:
            speaker = _speaker(c)
            _utterance(c, speaker["speaker_id"])
        reg = CorpusRegistry(tmp_path)
        assert len(reg.list_speakers()) == 1
        assert len(reg.list_utterances()) == 1
