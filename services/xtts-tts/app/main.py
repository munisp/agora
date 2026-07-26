"""XTTS-TTS sidecar — voice-cloning TTS (SPEC-W10 Part B, consumed by Agent A).

    POST   /tts           {"text", "voice_id", "language"} -> audio/wav
    POST   /voices        {"name", "sample_base64"}        -> {"voice_id"}
    GET    /voices        -> {"voices": [{id, name, languages, gender, labels, created_at}]}
    DELETE /voices/{id}   -> {"deleted": voice_id}
    GET    /healthz       -> {"status": "ok", ...}

Config (env):
    XTTS_MOCK=1       (default) deterministic sine wav with a voice-specific
                      pitch derived from voice_id — no GPU, no model download.
                      Set XTTS_MOCK=0 for real Coqui XTTS-v2 inference
                      (requires a >=6GB VRAM GPU; CPU is not viable — see
                      docs/voices-sidecars.md and the Dockerfile CUDA block).
    VOICES_DIR=/data  voice registry (voices.json) + enrolled samples volume.
    PORT=5810         listen port when run as `python -m app.main`.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import io
import os
import sys
import tempfile
import threading
from pathlib import Path

import numpy as np
import soundfile as sf
from fastapi import FastAPI, HTTPException
from fastapi.responses import Response
from pydantic import BaseModel, Field

from .registry import (
    MAX_SAMPLE_BYTES,
    MIN_SAMPLE_BYTES,
    XTTS_LANGUAGES,
    VoiceNotFound,
    VoiceRegistry,
)

MOCK_SAMPLE_RATE = 22050
MAX_TEXT_LEN = 5000
MIN_DURATION_S = 0.4
MAX_DURATION_S = 60.0


class TTSRequest(BaseModel):
    text: str = Field(min_length=1, max_length=MAX_TEXT_LEN)
    voice_id: str = Field(min_length=1, max_length=64)
    language: str = Field(min_length=2, max_length=8)


class EnrollRequest(BaseModel):
    name: str = Field(min_length=1, max_length=100)
    sample_base64: str = Field(min_length=1)


def mock_synthesize(text: str, voice_id: str, language: str) -> bytes:
    """Deterministic sine wav; pitch is derived from a hash of voice_id so
    each enrolled voice sounds distinct, and duration scales with text."""
    digest = hashlib.sha256(f"xtts:{voice_id}".encode("utf-8")).digest()
    pitch = 120.0 + (int.from_bytes(digest[:2], "big") % 121)  # 120..240 Hz
    lang_shift = (sum(language.encode("utf-8")) % 11) - 5  # small stable detune
    pitch += lang_shift

    duration = min(MAX_DURATION_S, max(MIN_DURATION_S, len(text) / 12.0))
    n = int(MOCK_SAMPLE_RATE * duration)
    t = np.arange(n, dtype=np.float32) / MOCK_SAMPLE_RATE

    text_seed = int.from_bytes(
        hashlib.sha256(f"{voice_id}:{language}:{text}".encode("utf-8")).digest()[:4], "big"
    )
    rng = np.random.default_rng(text_seed)

    vibrato = 4.0 * np.sin(2 * np.pi * 4.5 * t)
    phase = 2 * np.pi * np.cumsum(pitch + vibrato) / MOCK_SAMPLE_RATE
    signal = 0.5 * np.sin(phase) + 0.3 * np.sin(2 * phase) + 0.1 * np.sin(3 * phase)
    signal += 0.02 * rng.standard_normal(n).astype(np.float32)

    fade = min(256, n // 4)
    envelope = np.ones(n, dtype=np.float32)
    if fade > 0:
        envelope[:fade] = np.linspace(0.0, 1.0, fade)
        envelope[-fade:] = np.linspace(1.0, 0.0, fade)
    signal *= envelope

    peak = float(np.max(np.abs(signal))) or 1.0
    buf = io.BytesIO()
    sf.write(buf, (signal / peak * 0.8).astype(np.float32), MOCK_SAMPLE_RATE,
             format="WAV", subtype="PCM_16")
    return buf.getvalue()


class XTTSModel:
    """Lazy wrapper around Coqui TTS XTTS-v2.

    The `TTS` package and the ~1.8 GB checkpoint are imported/downloaded only
    on first use, and only when XTTS_MOCK=0. GPU is required in practice.
    """

    def __init__(self) -> None:
        self._tts = None
        self._lock = threading.Lock()

    def _load(self):
        with self._lock:
            if self._tts is None:
                from TTS.api import TTS  # lazy heavy import

                self._tts = TTS("tts_models/multilingual/multi-dataset/xtts_v2")
            return self._tts

    def synthesize(self, text: str, sample_wav: Path, language: str) -> bytes:
        tts = self._load()
        with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as tmp:
            out_path = tmp.name
        try:
            tts.tts_to_file(
                text=text,
                speaker_wav=str(sample_wav),
                language=language,
                file_path=out_path,
            )
            data, rate = sf.read(out_path, dtype="float32", always_2d=False)
        finally:
            Path(out_path).unlink(missing_ok=True)
        buf = io.BytesIO()
        sf.write(buf, data, int(rate), format="WAV", subtype="PCM_16")
        return buf.getvalue()


def create_app() -> FastAPI:
    mock = os.environ.get("XTTS_MOCK", "1") != "0"
    voices_dir = os.environ.get("VOICES_DIR", "/data")
    try:
        registry = VoiceRegistry(voices_dir)
    except OSError:
        # /data is the container default (root + volume). For local dev as a
        # non-root user, fall back to a local dir — explicit VOICES_DIR
        # misconfiguration still fails loudly.
        if "VOICES_DIR" in os.environ:
            raise
        voices_dir = os.path.abspath("./xtts-data")
        print(f"[xtts-tts] /data not writable; using local {voices_dir}", file=sys.stderr)
        registry = VoiceRegistry(voices_dir)
    model = None if mock else XTTSModel()

    app = FastAPI(title="opendesk-xtts-tts", version="1.0.0")

    @app.get("/healthz")
    def healthz() -> dict:
        return {
            "status": "ok",
            "service": "xtts-tts",
            "mock": mock,
            "voices_dir": str(registry.dir),
            "voices": len(registry.list()),
            "supported_languages": list(XTTS_LANGUAGES),
        }

    @app.get("/voices")
    def list_voices() -> dict:
        return {"voices": registry.list()}

    @app.post("/voices", status_code=201)
    def enroll(req: EnrollRequest) -> dict:
        try:
            sample = base64.b64decode(req.sample_base64, validate=True)
        except (binascii.Error, ValueError) as exc:
            raise HTTPException(
                status_code=400, detail=f"sample_base64 is not valid base64: {exc}"
            ) from exc
        if len(sample) < MIN_SAMPLE_BYTES:
            raise HTTPException(
                status_code=400,
                detail=f"decoded sample too small ({len(sample)} bytes); "
                       f"provide a 6-30s wav of the consenting speaker",
            )
        if len(sample) > MAX_SAMPLE_BYTES:
            raise HTTPException(
                status_code=413,
                detail=f"decoded sample too large ({len(sample)} bytes; max {MAX_SAMPLE_BYTES})",
            )
        entry = registry.add(req.name, sample)
        return {"voice_id": entry["id"], "voice": entry}

    @app.delete("/voices/{voice_id}")
    def delete_voice(voice_id: str) -> dict:
        try:
            entry = registry.delete(voice_id)
        except VoiceNotFound as exc:
            raise HTTPException(status_code=404, detail=f"unknown voice_id '{voice_id}'") from exc
        return {"deleted": entry["id"]}

    @app.post("/tts", response_class=Response)
    def tts(req: TTSRequest) -> Response:
        voice = registry.get(req.voice_id)
        if voice is None:
            raise HTTPException(status_code=404, detail=f"unknown voice_id '{req.voice_id}'")
        language = req.language.lower()
        if language not in XTTS_LANGUAGES:
            raise HTTPException(
                status_code=400,
                detail=f"unsupported language '{req.language}' for XTTS-v2; "
                       f"supported: {', '.join(XTTS_LANGUAGES)}",
            )
        if mock:
            wav = mock_synthesize(req.text, req.voice_id, language)
        else:
            assert model is not None
            wav = model.synthesize(req.text, registry.sample_path(req.voice_id), language)
        return Response(content=wav, media_type="audio/wav")

    return app


app = create_app()


def main() -> None:  # pragma: no cover - process entrypoint
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=int(os.environ.get("PORT", "5810")))


if __name__ == "__main__":  # pragma: no cover
    main()
