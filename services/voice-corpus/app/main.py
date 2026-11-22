"""voice-corpus — commissioned native-speaker corpus ingest + QA + export
(SPEC-W103 C1). This is the fine-tune READINESS scaffold: honest data
infrastructure for the CosyVoice-2 Naija-TTS track and any future LongCat
spike. It contains NO model code, NO GPU path, and NO mock mode — it never
synthesizes anything; it stores, validates, and exports consented audio.

Consent is FAIL-CLOSED (mirrors the cosyvoice/longcat sidecars): POST
/v1/speakers without a non-empty ``consent_basis`` -> 422, and the registry
re-asserts before any bytes are written. Speakers are pseudonyms only
(NDPA); consent revocation (DELETE /v1/speakers/{id}) deletes the audio
files, not just a flag.

Endpoints:
    GET    /healthz
    POST   /v1/speakers {pseudonym, languages, consent_basis,
                         consent_reference?, source} -> 201 {speaker}
    GET    /v1/speakers
    GET    /v1/speakers/{id}
    DELETE /v1/speakers/{id} -> {deleted_speaker, deleted_utterances, deleted_bytes}
    POST   /v1/utterances {speaker_id, language, transcript, audio_base64} -> 201
    GET    /v1/utterances?speaker_id=&language=
    DELETE /v1/utterances/{id} -> {deleted}
    GET    /v1/stats
    GET    /v1/export?language=&format=jsonl|ljspeech
"""

from __future__ import annotations

import base64
import binascii
import io
import os
import unicodedata
import wave

from fastapi import FastAPI, HTTPException, Query
from fastapi.responses import PlainTextResponse
from pydantic import BaseModel, Field, field_validator

from .registry import (
    CorpusRegistry,
    EvalRunNotFound,
    EvalSampleNotFound,
    RegistryError,
    SpeakerNotFound,
    UtteranceNotFound,
)

CORPUS_DIR = os.environ.get("CORPUS_DIR", "/data")
CORPUS_LANGUAGES = tuple(
    p.strip().lower()
    for p in os.environ.get("CORPUS_LANGUAGES", "pcm,yo,ha,ig,en").split(",")
    if p.strip()
)
PORT = int(os.environ.get("PORT", "5840"))

MIN_DURATION_S = 0.5
MAX_DURATION_S = 30.0
MAX_AUDIO_BYTES = 10 * 1024 * 1024  # 10 MB decoded ceiling
MAX_TRANSCRIPT_CHARS = 2000

# SPEC-W104 C3 readiness thresholds. Guideline values from industry
# practice for a small single-language TTS fine-tune — NOT a guarantee of
# output quality; a "ready" corpus can still yield a poor model.
MIN_HOURS = 10.0
MIN_SPEAKERS = 3
MIN_TONE_COVERAGE_PCT = 80.0
# Tone enforcement: Yoruba ONLY. Igbo tone is inconsistently marked in
# practical orthography, Hausa standard orthography marks no tone, and pcm
# has no standard tone orthography — enforcing there would punish correct
# corpora. Coverage is still REPORTED for every language in /v1/stats.
TONE_ENFORCED = ("yo",)


# Combining diacritics that signal TONE in Latin orthographies: grave,
# acute, circumflex, macron, caron, double grave, inverted breve. Dot-below
# (U+0323) is deliberately EXCLUDED — in Igbo/Yoruba orthography it marks
# vowel quality (ị ọ ụ / ẹ ọ), not tone.
_TONE_MARKS = {0x0300, 0x0301, 0x0302, 0x0304, 0x030C, 0x030F, 0x0311}


def has_tone_marks(text: str) -> bool:
    """True when the transcript carries explicit tone information.

    Detection: any character whose NFD decomposition contains a tone
    diacritic from ``_TONE_MARKS``. This catches both precomposed forms
    (á à é è — NFD: base + combining) and base+combining sequences
    (Yoruba ẹ́ = ẹ + U+0301, ọ̀ = ọ + U+0300), while NOT flagging
    dot-below vowel-quality letters (Igbo ị ọ ụ, Yoruba ẹ ọ ṣ — those are
    phonemes, not tone marks). Yoruba and Igbo are tonal — a corpus without
    tone marks is a readiness RISK for fine-tuning, so this is recorded per
    utterance and surfaced in /v1/stats.
    """
    for ch in text:
        if ord(ch) in _TONE_MARKS:
            return True  # standalone combining mark (defensive)
        for part in unicodedata.normalize("NFD", ch):
            if ord(part) in _TONE_MARKS:
                return True
    return False


def probe_wav(audio: bytes) -> tuple[float, int, int]:
    """Decode wav headers with stdlib wave -> (duration_s, rate, channels).

    Raises ValueError on anything that is not a decodable wav — the client
    never gets to assert duration/rate; the file itself is the truth.
    """
    with wave.open(io.BytesIO(audio), "rb") as wf:
        frames = wf.getnframes()
        rate = wf.getframerate()
        channels = wf.getnchannels()
        if rate <= 0:
            raise ValueError("wav has no sample rate")
        return frames / float(rate), rate, channels


class SpeakerRequest(BaseModel):
    pseudonym: str = Field(min_length=1, max_length=64)
    languages: list[str] = Field(min_length=1, max_length=8)
    # FAIL-CLOSED consent gate: missing field -> 422 (pydantic); empty or
    # whitespace-only -> 422 (validator below). No documented consent basis,
    # no corpus record — mirrors the cosyvoice/longcat sidecars.
    consent_basis: str
    consent_reference: str = Field(default="", max_length=256)
    source: str = Field(min_length=1, max_length=256)

    @field_validator("consent_basis")
    @classmethod
    def _consent_basis_not_blank(cls, value: str) -> str:
        value = (value or "").strip()
        if not value:
            raise ValueError("consent_basis must be a non-empty consent reference")
        return value

    @field_validator("languages")
    @classmethod
    def _languages_known(cls, value: list[str]) -> list[str]:
        out = []
        for lang in value:
            lang = (lang or "").strip().lower()
            if lang not in CORPUS_LANGUAGES:
                raise ValueError(
                    f"unsupported language {lang!r} (CORPUS_LANGUAGES={CORPUS_LANGUAGES})"
                )
            if lang not in out:
                out.append(lang)
        return out


class UtteranceRequest(BaseModel):
    speaker_id: str = Field(min_length=1, max_length=64)
    language: str = Field(min_length=1, max_length=16)
    transcript: str = Field(min_length=1, max_length=MAX_TRANSCRIPT_CHARS)
    audio_base64: str = Field(min_length=1)

    @field_validator("transcript")
    @classmethod
    def _transcript_normalized(cls, value: str) -> str:
        value = unicodedata.normalize("NFC", value).strip()
        if not value:
            raise ValueError("transcript must be non-empty")
        return value


# SPEC-W104 C2 — eval loop request models.
class EvalRunRequest(BaseModel):
    language: str = Field(min_length=1, max_length=16)
    # Which model produced the samples under review (e.g.
    # "cosyvoice2-pcm-ft-r3"). Never invented — required, non-blank.
    model_ref: str = Field(min_length=1, max_length=256)
    notes: str = Field(default="", max_length=1000)

    @field_validator("model_ref")
    @classmethod
    def _model_ref_not_blank(cls, value: str) -> str:
        value = (value or "").strip()
        if not value:
            raise ValueError("model_ref must name the model that made the samples")
        return value


class EvalSampleRequest(BaseModel):
    text: str = Field(min_length=1, max_length=MAX_TRANSCRIPT_CHARS)
    audio_base64: str = Field(min_length=1)


class EvalScoreRequest(BaseModel):
    # Native-speaker reviewer pseudonym — required so scores are attributable
    # (and so a reviewer's scores can be discounted if trust breaks).
    reviewer: str = Field(min_length=1, max_length=64)
    score: int
    notes: str = Field(default="", max_length=1000)

    @field_validator("score")
    @classmethod
    def _score_in_range(cls, value: int) -> int:
        if not (1 <= value <= 5):
            raise ValueError("score must be an integer in [1, 5]")
        return value


def _decode_audio(audio_base64: str) -> tuple[bytes, float]:
    """Shared audio QA (utterances + eval samples): base64 -> wav probe."""
    try:
        audio = base64.b64decode(audio_base64, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise HTTPException(
            status_code=400, detail="audio_base64 is not valid base64"
        ) from exc
    if len(audio) > MAX_AUDIO_BYTES:
        raise HTTPException(
            status_code=413,
            detail=f"decoded audio exceeds {MAX_AUDIO_BYTES} bytes",
        )
    try:
        duration_s, _rate, _channels = probe_wav(audio)
    except (wave.Error, EOFError, ValueError) as exc:
        raise HTTPException(
            status_code=400, detail=f"audio is not a decodable wav: {exc}"
        ) from exc
    return audio, duration_s


def create_app() -> FastAPI:
    app = FastAPI(title="opendesk-voice-corpus", version="0.1.0")
    registry = CorpusRegistry(CORPUS_DIR)

    @app.get("/healthz")
    def healthz() -> dict:
        return {
            "status": "ok",
            "speakers": len(registry.list_speakers()),
            "utterances": len(registry.list_utterances()),
            "languages": list(CORPUS_LANGUAGES),
        }

    # ------------------------------------------------------------- speakers
    @app.post("/v1/speakers", status_code=201)
    def add_speaker(req: SpeakerRequest) -> dict:
        try:
            return registry.add_speaker(
                req.pseudonym,
                req.languages,
                consent_basis=req.consent_basis,
                consent_reference=req.consent_reference,
                source=req.source,
            )
        except RegistryError as exc:
            raise HTTPException(status_code=422, detail=str(exc)) from exc

    @app.get("/v1/speakers")
    def list_speakers() -> dict:
        return {"speakers": registry.list_speakers()}

    @app.get("/v1/speakers/{speaker_id}")
    def get_speaker(speaker_id: str) -> dict:
        record = registry.get_speaker(speaker_id)
        if record is None:
            raise HTTPException(status_code=404, detail="unknown speaker_id")
        return record

    @app.delete("/v1/speakers/{speaker_id}")
    def delete_speaker(speaker_id: str) -> dict:
        try:
            return registry.delete_speaker(speaker_id)
        except SpeakerNotFound as exc:
            raise HTTPException(status_code=404, detail="unknown speaker_id") from exc

    # ------------------------------------------------------------ utterances
    @app.post("/v1/utterances", status_code=201)
    def add_utterance(req: UtteranceRequest) -> dict:
        language = req.language.strip().lower()
        if language not in CORPUS_LANGUAGES:
            raise HTTPException(
                status_code=400,
                detail=f"unsupported language {language!r} "
                f"(CORPUS_LANGUAGES={CORPUS_LANGUAGES})",
            )
        try:
            audio = base64.b64decode(req.audio_base64, validate=True)
        except (binascii.Error, ValueError) as exc:
            raise HTTPException(
                status_code=400, detail="audio_base64 is not valid base64"
            ) from exc
        if len(audio) > MAX_AUDIO_BYTES:
            raise HTTPException(
                status_code=413,
                detail=f"decoded audio exceeds {MAX_AUDIO_BYTES} bytes",
            )
        try:
            duration_s, sample_rate, channels = probe_wav(audio)
        except (wave.Error, EOFError, ValueError) as exc:
            raise HTTPException(
                status_code=400, detail=f"audio is not a decodable wav: {exc}"
            ) from exc
        if not (MIN_DURATION_S <= duration_s <= MAX_DURATION_S):
            raise HTTPException(
                status_code=422,
                detail=f"duration {duration_s:.3f}s outside "
                f"[{MIN_DURATION_S}, {MAX_DURATION_S}] seconds",
            )
        try:
            return registry.add_utterance(
                req.speaker_id,
                language,
                req.transcript,
                audio,
                has_tone_marks=has_tone_marks(req.transcript),
                duration_s=duration_s,
                sample_rate=sample_rate,
                channels=channels,
            )
        except SpeakerNotFound as exc:
            raise HTTPException(status_code=404, detail="unknown speaker_id") from exc

    @app.get("/v1/utterances")
    def list_utterances(
        speaker_id: str = Query(default=""),
        language: str = Query(default=""),
    ) -> dict:
        return {
            "utterances": registry.list_utterances(
                speaker_id=speaker_id.strip(), language=language.strip().lower()
            )
        }

    @app.delete("/v1/utterances/{utterance_id}")
    def delete_utterance(utterance_id: str) -> dict:
        try:
            return registry.delete_utterance(utterance_id)
        except UtteranceNotFound as exc:
            raise HTTPException(status_code=404, detail="unknown utterance_id") from exc

    # ------------------------------------------------------------ stats/export
    @app.get("/v1/stats")
    def stats() -> dict:
        speakers = registry.list_speakers()
        utterances = registry.list_utterances()
        per_language: dict[str, dict] = {}
        for lang in CORPUS_LANGUAGES:
            utts = [u for u in utterances if u["language"] == lang]
            speaker_ids = {u["speaker_id"] for u in utts}
            tone = sum(1 for u in utts if u["has_tone_marks"])
            per_language[lang] = {
                "utterances": len(utts),
                "seconds": round(sum(u["duration_s"] for u in utts), 1),
                "speakers": len(speaker_ids),
                "tone_mark_coverage_pct": (
                    round(100.0 * tone / len(utts), 1) if utts else None
                ),
            }
        return {
            "speakers": len(speakers),
            "utterances": len(utterances),
            "seconds": round(sum(u["duration_s"] for u in utterances), 1),
            "per_language": per_language,
        }

    @app.get("/v1/export")
    def export(
        language: str = Query(default=""),
        format: str = Query(default="jsonl"),
    ) -> PlainTextResponse:
        fmt = format.strip().lower()
        if fmt not in ("jsonl", "ljspeech"):
            raise HTTPException(
                status_code=400, detail="format must be jsonl or ljspeech"
            )
        lang = language.strip().lower()
        if lang and lang not in CORPUS_LANGUAGES:
            raise HTTPException(
                status_code=400,
                detail=f"unsupported language {lang!r} "
                f"(CORPUS_LANGUAGES={CORPUS_LANGUAGES})",
            )
        # Consented by construction: only consented, non-deleted records can
        # exist in the store (fail-closed enrollment + real deletion).
        utterances = registry.list_utterances(language=lang)
        lines = []
        if fmt == "jsonl":
            import json

            for u in utterances:
                lines.append(
                    json.dumps(
                        {
                            "audio_path": u["audio_relpath"],
                            "text": u["transcript"],
                            "speaker_id": u["speaker_id"],
                            "language": u["language"],
                            "duration_s": u["duration_s"],
                            "sample_rate": u["sample_rate"],
                        },
                        ensure_ascii=False,
                    )
                )
        else:  # ljspeech: relpath|text|speaker_id
            for u in utterances:
                lines.append(
                    f"{u['audio_relpath']}|{u['transcript']}|{u['speaker_id']}"
                )
        return PlainTextResponse("\n".join(lines) + ("\n" if lines else ""))

    # ----------------------------------------------- SPEC-W104 C1: ft export
    @app.get("/v1/export/cosyvoice")
    def export_cosyvoice(
        language: str = Query(default=""),
        valid_pct: int = Query(default=5),
    ) -> PlainTextResponse:
        """CosyVoice-2 fine-tune handoff: one jsonl line per utterance —
        {"utt", "wav", "text", "spk", "split"}. The train/valid split is
        DETERMINISTIC (md5 of the utterance id) so reruns are byte-identical
        and the validation set never drifts between data-prep runs.
        """
        if not (0 <= valid_pct <= 50):
            raise HTTPException(
                status_code=400, detail="valid_pct must be in [0, 50]"
            )
        lang = language.strip().lower()
        if lang and lang not in CORPUS_LANGUAGES:
            raise HTTPException(
                status_code=400,
                detail=f"unsupported language {lang!r} "
                f"(CORPUS_LANGUAGES={CORPUS_LANGUAGES})",
            )
        import hashlib
        import json

        lines = []
        for u in registry.list_utterances(language=lang):
            bucket = hashlib.md5(u["utterance_id"].encode("utf-8")).digest()[0] % 100
            lines.append(
                json.dumps(
                    {
                        "utt": u["utterance_id"],
                        "wav": u["audio_relpath"],
                        "text": u["transcript"],
                        "spk": u["speaker_id"],
                        "split": "valid" if bucket < valid_pct else "train",
                    },
                    ensure_ascii=False,
                )
            )
        return PlainTextResponse("\n".join(lines) + ("\n" if lines else ""))

    # ------------------------------------------------ SPEC-W104 C2: eval loop
    @app.post("/v1/eval/runs", status_code=201)
    def add_eval_run(req: EvalRunRequest) -> dict:
        language = req.language.strip().lower()
        if language not in CORPUS_LANGUAGES:
            raise HTTPException(
                status_code=400,
                detail=f"unsupported language {language!r} "
                f"(CORPUS_LANGUAGES={CORPUS_LANGUAGES})",
            )
        try:
            return registry.add_eval_run(
                language, req.model_ref, notes=req.notes
            )
        except RegistryError as exc:
            raise HTTPException(status_code=422, detail=str(exc)) from exc

    @app.get("/v1/eval/runs")
    def list_eval_runs() -> dict:
        return {"runs": registry.list_eval_runs()}

    @app.post("/v1/eval/runs/{run_id}/samples", status_code=201)
    def add_eval_sample(run_id: str, req: EvalSampleRequest) -> dict:
        audio, duration_s = _decode_audio(req.audio_base64)
        try:
            return registry.add_eval_sample(
                run_id, req.text, audio, duration_s=duration_s
            )
        except EvalRunNotFound as exc:
            raise HTTPException(status_code=404, detail="unknown run_id") from exc

    @app.post("/v1/eval/samples/{sample_id}/scores", status_code=201)
    def add_eval_score(sample_id: str, req: EvalScoreRequest) -> dict:
        try:
            return registry.add_eval_score(
                sample_id, req.reviewer, req.score, notes=req.notes
            )
        except EvalSampleNotFound as exc:
            raise HTTPException(
                status_code=404, detail="unknown sample_id"
            ) from exc
        except RegistryError as exc:
            raise HTTPException(status_code=422, detail=str(exc)) from exc

    @app.get("/v1/eval/runs/{run_id}")
    def eval_run_summary(run_id: str) -> dict:
        run = registry.get_eval_run(run_id)
        if run is None:
            raise HTTPException(status_code=404, detail="unknown run_id")
        samples = registry.list_eval_samples(run_id)
        scores = registry.list_eval_scores(run_id)
        values = [s["score"] for s in scores]
        per_reviewer: dict[str, int] = {}
        for s in scores:
            per_reviewer[s["reviewer"]] = per_reviewer.get(s["reviewer"], 0) + 1
        return {
            "run": run,
            "samples": len(samples),
            "scores": len(scores),
            # Plain mean over all scores. Guideline (docs/voice-corpus.md):
            # a MOS means nothing below n>=10 across >=3 reviewers.
            "mos": round(sum(values) / len(values), 3) if values else None,
            "min": min(values) if values else None,
            "max": max(values) if values else None,
            "per_reviewer": per_reviewer,
        }

    # ------------------------------------------- SPEC-W104 C3: readiness gate
    @app.get("/v1/readiness/{language}")
    def readiness(language: str) -> dict:
        lang = language.strip().lower()
        if lang not in CORPUS_LANGUAGES:
            raise HTTPException(
                status_code=400,
                detail=f"unsupported language {lang!r} "
                f"(CORPUS_LANGUAGES={CORPUS_LANGUAGES})",
            )
        utterances = registry.list_utterances(language=lang)
        seconds = sum(u["duration_s"] for u in utterances)
        hours = round(seconds / 3600.0, 2)
        speakers = len({u["speaker_id"] for u in utterances})
        tone = sum(1 for u in utterances if u["has_tone_marks"])
        tone_pct = (
            round(100.0 * tone / len(utterances), 1) if utterances else None
        )
        gaps: list[str] = []
        if hours < MIN_HOURS:
            gaps.append(f"hours {hours} < {MIN_HOURS}")
        if speakers < MIN_SPEAKERS:
            gaps.append(f"speakers {speakers} < {MIN_SPEAKERS}")
        if lang in TONE_ENFORCED and (
            tone_pct is None or tone_pct < MIN_TONE_COVERAGE_PCT
        ):
            gaps.append(
                f"tone_mark_coverage_pct {tone_pct} < {MIN_TONE_COVERAGE_PCT} "
                f"(tone enforced for {lang})"
            )
        return {
            "language": lang,
            "ready": not gaps,
            "gaps": gaps,
            "stats": {
                "utterances": len(utterances),
                "hours": hours,
                "speakers": speakers,
                "tone_mark_coverage_pct": tone_pct,
                "tone_enforced": lang in TONE_ENFORCED,
            },
            "thresholds": {
                "min_hours": MIN_HOURS,
                "min_speakers": MIN_SPEAKERS,
                "min_tone_coverage_pct": MIN_TONE_COVERAGE_PCT,
                "tone_enforced_languages": list(TONE_ENFORCED),
            },
        }

    return app


def main() -> None:  # pragma: no cover - process entrypoint
    import uvicorn

    uvicorn.run(
        "app.main:create_app", host="0.0.0.0", port=PORT, factory=True
    )


if __name__ == "__main__":  # pragma: no cover
    main()
