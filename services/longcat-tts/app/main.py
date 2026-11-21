"""LongCat-TTS sidecar — consent-gated voice-cloning TTS (SPEC-W102 C1),
mirroring the services/cosyvoice-tts structure and conventions.

    POST   /tts           {"text", "voice_id", "language"}   -> audio/wav
    POST   /voices        {"name", "sample_base64",
                           "consent_basis", "source",
                           "license"?, "languages"?}         -> {"voice_id"}
    GET    /voices        -> {"voices": [{voice_id, name, languages, source,
                                          license, consent_basis, enrolled_at}]}
    DELETE /voices/{id}   -> {"deleted": voice_id}
    GET    /healthz       -> {"status": "ok", ...}

Config (env):
    LONGCAT_MOCK=1  (default) deterministic sine wav with a voice-specific
                    pitch derived from voice_id — no GPU, no model download.
                    Set LONGCAT_MOCK=0 for real LongCat-AudioDiT zero-shot
                    inference (meituan-longcat/LongCat-AudioDiT, MIT — code
                    AND weights; requires a >=8GB VRAM GPU for the 1B
                    variant — see the Dockerfile CUDA block and
                    docs/voices-sidecars.md).
    LONGCAT_MODEL_DIR   model id/path (default meituan-longcat/LongCat-AudioDiT-1B)
    LONGCAT_STEPS       diffusion steps (default 16)
    LONGCAT_CFG         guidance strength (default 4.0)
    LONGCAT_GUIDANCE    cfg|apg (default apg — adaptive projection guidance)
    LONGCAT_FRAMES_PER_SEC  latent frames per audio second for the duration
                            estimate (default 25)
    VOICES_DIR=/data  voice registry (voices.json) + enrolled samples volume.
    PORT=5830         listen port when run as `python -m app.main`.

Consent is FAIL-CLOSED: POST /voices without a non-empty ``consent_basis``
is rejected with 422 before any bytes are written. Every voices.json record
carries the consent metadata (see app/registry.py and docs/VOICES.md).
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import io
import os
import sys
import threading
from pathlib import Path
from typing import List, Optional

import numpy as np
import soundfile as sf
from fastapi import FastAPI, HTTPException
from fastapi.responses import Response
from pydantic import BaseModel, Field, field_validator

from .registry import (
    LONGCAT_LANGUAGES,
    MAX_SAMPLE_BYTES,
    MIN_SAMPLE_BYTES,
    VoiceNotFound,
    VoiceRegistry,
)

MOCK_SAMPLE_RATE = 24000  # matches LongCat-AudioDiT output rate
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
    # FAIL-CLOSED consent gate: missing field -> 422 (pydantic); empty or
    # whitespace-only -> 422 (validator below). No documented consent basis,
    # no enrollment.
    consent_basis: str
    # Provenance of the reference sample (e.g. "staff-consented recording",
    # "naija-tts-finetune"); recorded verbatim on the voice record.
    source: str = Field(min_length=1, max_length=200)
    license: str = Field(default="", max_length=200)
    languages: Optional[List[str]] = None
    # Transcript of the reference sample. LongCat's zero-shot cloning
    # concatenates prompt_text + gen_text for the text encoder, so cloning
    # REQUIRES it for best fidelity; empty = plain (uncloned) TTS on the
    # real path — recorded verbatim on the voice record, never faked.
    prompt_text: str = Field(default="", max_length=2000)

    @field_validator("consent_basis")
    @classmethod
    def _consent_basis_not_blank(cls, value: str) -> str:
        value = (value or "").strip()
        if not value:
            raise ValueError("consent_basis must be a non-empty consent reference")
        return value


def mock_synthesize(text: str, voice_id: str, language: str) -> bytes:
    """Deterministic sine wav; pitch is derived from a hash of voice_id so
    each enrolled voice sounds distinct, and duration scales with text."""
    digest = hashlib.sha256(f"longcat:{voice_id}".encode("utf-8")).digest()
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


class LongCatModel:
    """Lazy wrapper around Meituan LongCat-AudioDiT (MIT — code + weights).

    The ``audiodit`` package (auto-registers with transformers) and the
    ~2.5GB 1B checkpoint are imported/downloaded only on first use, and only
    when LONGCAT_MOCK=0. GPU is required in practice (>=8GB VRAM for the 1B
    variant; CPU inference is not viable). Zero-shot cloning feeds the
    enrolled sample as prompt_audio; the text encoder sees
    ``prompt_text + gen_text`` — with NO recorded transcript the clone
    conditioning is impossible, so the honest fallback is plain TTS
    (voice stays the model default; the mock keeps per-voice pitch either
    way, documented in the README).
    """

    def __init__(self) -> None:
        self._model = None
        self._tokenizer = None
        self._lock = threading.Lock()

    def _load(self):
        with self._lock:
            if self._model is None:
                import torch  # lazy heavy import
                from transformers import AutoTokenizer  # lazy heavy import

                from audiodit import AudioDiTModel  # lazy heavy import

                model_dir = os.environ.get(
                    "LONGCAT_MODEL_DIR", "meituan-longcat/LongCat-AudioDiT-1B"
                )
                model = AudioDiTModel.from_pretrained(model_dir)
                device = "cuda" if torch.cuda.is_available() else "cpu"
                model.to(device)
                model.vae.to_half()  # VAE runs fp16 (matches upstream)
                model.eval()
                self._device = device
                self._model = model
                self._tokenizer = AutoTokenizer.from_pretrained(
                    model.config.text_encoder_model
                )
            return self._model

    def synthesize(
        self, text: str, sample_wav: Path, language: str, prompt_text: str = ""
    ) -> bytes:
        import numpy as np  # lazy
        import soundfile as sf  # lazy
        import torch  # lazy

        model = self._load()
        steps = int(os.environ.get("LONGCAT_STEPS", "16"))
        cfg = float(os.environ.get("LONGCAT_CFG", "4.0"))
        guidance = os.environ.get("LONGCAT_GUIDANCE", "apg")
        fps = float(os.environ.get("LONGCAT_FRAMES_PER_SEC", "25"))

        gen_text = text.strip()
        if prompt_text:
            enc_text = f"{prompt_text.strip()} {gen_text}"
            audio, _sr = sf.read(str(sample_wav), dtype="float32", always_2d=False)
            if getattr(audio, "ndim", 1) > 1:
                audio = audio.mean(axis=1)
            if _sr != 24000:  # resample to the model's rate (linear fallback)
                import math

                n = int(len(audio) * 24000 / _sr)
                xp = np.linspace(0, len(audio) - 1, num=max(n, 1))
                audio = np.interp(xp, np.arange(len(audio)), audio).astype(np.float32)
            prompt_wav = torch.from_numpy(np.asarray(audio)).unsqueeze(0).unsqueeze(0)
            prompt_frames = max(1, int(len(audio) / 24000 * fps))
        else:
            enc_text = gen_text
            prompt_wav = None
            prompt_frames = 0
        inputs = self._tokenizer([enc_text], padding="longest", return_tensors="pt")
        gen_frames = max(1, int(len(gen_text) / 12.0 * fps))
        with torch.no_grad():
            kwargs = dict(
                input_ids=inputs.input_ids,
                attention_mask=inputs.attention_mask,
                duration=prompt_frames + gen_frames,
                steps=steps,
                cfg_strength=cfg,
                guidance_method=guidance,
            )
            if prompt_wav is not None:
                kwargs["prompt_audio"] = prompt_wav
            output = model(**kwargs)
        wav = output.waveform.squeeze().detach().cpu().numpy().astype(np.float32)
        buf = io.BytesIO()
        sf.write(buf, wav, 24000, format="WAV", subtype="PCM_16")
        return buf.getvalue()


def create_app() -> FastAPI:
    mock = os.environ.get("LONGCAT_MOCK", "1") != "0"
    voices_dir = os.environ.get("VOICES_DIR", "/data")
    try:
        registry = VoiceRegistry(voices_dir)
    except OSError:
        # /data is the container default (root + volume). For local dev as a
        # non-root user, fall back to a local dir — explicit VOICES_DIR
        # misconfiguration still fails loudly.
        if "VOICES_DIR" in os.environ:
            raise
        voices_dir = os.path.abspath("./longcat-data")
        print(f"[longcat-tts] /data not writable; using local {voices_dir}", file=sys.stderr)
        registry = VoiceRegistry(voices_dir)
    model = None if mock else LongCatModel()

    app = FastAPI(title="opendesk-longcat-tts", version="1.0.0")

    @app.get("/healthz")
    def healthz() -> dict:
        return {
            "status": "ok",
            "service": "longcat-tts",
            "mock": mock,
            "voices_dir": str(registry.dir),
            "voices": len(registry.list()),
            "supported_languages": list(LONGCAT_LANGUAGES),
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
        entry = registry.add(
            req.name,
            sample,
            consent_basis=req.consent_basis,
            source=req.source,
            license=req.license,
            languages=req.languages,
            prompt_text=req.prompt_text,
        )
        return {"voice_id": entry["voice_id"], "voice": entry}

    @app.delete("/voices/{voice_id}")
    def delete_voice(voice_id: str) -> dict:
        try:
            entry = registry.delete(voice_id)
        except VoiceNotFound as exc:
            raise HTTPException(status_code=404, detail=f"unknown voice_id '{voice_id}'") from exc
        return {"deleted": entry["voice_id"]}

    @app.post("/tts", response_class=Response)
    def tts(req: TTSRequest) -> Response:
        voice = registry.get(req.voice_id)
        if voice is None:
            raise HTTPException(status_code=404, detail=f"unknown voice_id '{req.voice_id}'")
        language = req.language.lower()
        if language not in LONGCAT_LANGUAGES:
            raise HTTPException(
                status_code=400,
                detail=f"unsupported language '{req.language}' for CosyVoice-2; "
                       f"supported: {', '.join(LONGCAT_LANGUAGES)}",
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

    uvicorn.run(app, host="0.0.0.0", port=int(os.environ.get("PORT", "5830")))


if __name__ == "__main__":  # pragma: no cover
    main()
