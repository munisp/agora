"""naija-speech: Nigerian-language speech for live calls, on the ghana GPU (GB10).

- POST /tts  {"text", "language", "voice"?, "sample_rate"?} -> audio/wav (16-bit mono at sample_rate)
    ig / yo / ha / en : YarnGPT2 (saheedniyi/YarnGPT2, Apache-2.0) — a small speech LM served by vLLM, decoded with
                        WavTokenizer. The reply is split into sentences and all sentences are generated in ONE vLLM
                        batch (one at a time is ~1x real time; a 4-sentence batch ran 3.3x real time).
    pcm / ful         : Meta MMS-TTS (facebook/mms-tts-pcm, mms-tts-ful; CC-BY-NC 4.0 — see README).
    Audio is resampled to the caller's rate (the call pipeline plays one fixed rate; any other rate sounds pitched).
- POST /asr?language=ig|yo|ha|pcm|ful  body: audio/wav -> {"text"}
    Meta MMS-1B-all (facebook/mms-1b-all, CC-BY-NC 4.0) with the language's adapter. Whisper hears none of Igbo,
    Pidgin or Fulfulde, and Yoruba/Hausa poorly; MMS on real FLEURS recordings is close to word-perfect for Igbo/Hausa.
- GET /healthz, GET /voices

Published on the Tailscale address only (like agora-xtts); no tenant data is stored.
"""
from __future__ import annotations

import io
import os
import re
import threading
import time
from typing import Optional

import numpy as np
import soundfile as sf
import torch
import torchaudio.functional as AF
import uvicorn
from fastapi import FastAPI, HTTPException, Query, Request
from fastapi.responses import Response
from pydantic import BaseModel, Field

YARN_LANG = {"ig": "igbo", "yo": "yoruba", "ha": "hausa", "en": "english"}
YARN_VOICES = {
    "igbo": ["igbo_female2", "igbo_male2", "igbo_female1"],
    "yoruba": ["yoruba_female2", "yoruba_male2", "yoruba_female1"],
    "hausa": ["hausa_female1", "hausa_female2", "hausa_male2", "hausa_male1"],
    "english": ["idera", "chinenye", "jude", "emma", "umar", "joke", "zainab", "osagie", "remi", "tayo"],
}
MMS_TTS = {"pcm": "facebook/mms-tts-pcm", "ful": "facebook/mms-tts-ful"}
ASR_LANG = {"ig": "ibo", "yo": "yor", "ha": "hau", "pcm": "pcm", "ful": "ful"}
WAVTOK_CKPT = os.environ.get("WAVTOKENIZER_CKPT", "/models/wavtokenizer_large_speech_320_24k.ckpt")
GPU_SHARE = float(os.environ.get("YARN_GPU_MEMORY", "0.08"))
GAP_S = 0.18  # pause between sentences
MAX_CHARS = 1200

_SENT = re.compile(r"(?<=[.!?…])\s+|\n+")


def sentences(text: str) -> list[str]:
    """Sentence chunks for one batch; very long sentences are split at commas so no chunk runs past ~200 chars."""
    out: list[str] = []
    for s in _SENT.split(text.strip()):
        s = s.strip()
        while len(s) > 200:
            cut = s.rfind(",", 0, 200)
            cut = cut if cut > 40 else 200
            out.append(s[: cut + 1].strip())
            s = s[cut + 1 :].strip()
        if s:
            out.append(s)
    return out


def wav_bytes(audio: np.ndarray, rate: int) -> bytes:
    buf = io.BytesIO()
    sf.write(buf, np.clip(audio, -1.0, 1.0), rate, format="WAV", subtype="PCM_16")
    return buf.getvalue()


def resample(audio: np.ndarray, src: int, dst: int) -> np.ndarray:
    if src == dst or audio.size == 0:
        return audio
    return AF.resample(torch.from_numpy(audio.astype(np.float32)), src, dst).numpy()


class Engines:
    """Models load once at startup; one lock per model family (vLLM batches internally; MMS calls are tiny)."""

    def __init__(self) -> None:
        self.ready = False
        self.error: Optional[str] = None
        self.yarn_lock = threading.Lock()
        self.mms_lock = threading.Lock()

    def load(self) -> None:
        try:
            from huggingface_hub import hf_hub_download
            from transformers import AutoProcessor, AutoTokenizer, VitsModel, Wav2Vec2ForCTC
            from vllm import LLM, SamplingParams
            from yarngpt.audiotokenizer import AudioTokenizerV2

            cfg = hf_hub_download("novateur/WavTokenizer-medium-speech-75token",
                                  "wavtokenizer_mediumdata_frame75_3s_nq1_code4096_dim512_kmeans200_attn.yaml")
            self.tok = AudioTokenizerV2("saheedniyi/YarnGPT2", WAVTOK_CKPT, cfg)
            self.llm = LLM(model="saheedniyi/YarnGPT2", dtype="bfloat16", gpu_memory_utilization=GPU_SHARE,
                           max_model_len=4096)
            self.sp = SamplingParams(temperature=0.1, repetition_penalty=1.1, max_tokens=3000)
            self.asr_proc = AutoProcessor.from_pretrained("facebook/mms-1b-all")
            self.asr = Wav2Vec2ForCTC.from_pretrained("facebook/mms-1b-all", torch_dtype=torch.float16).to("cuda").eval()
            self.asr_lang: Optional[str] = None
            self.vits = {k: (VitsModel.from_pretrained(m).to("cuda").eval(), AutoTokenizer.from_pretrained(m))
                         for k, m in MMS_TTS.items()}
            self.ready = True
        except Exception as exc:  # noqa: BLE001 - reported by /healthz
            self.error = f"{type(exc).__name__}: {exc}"[:500]
            raise

    # ------------------------------------------------------------------ TTS
    def yarn(self, text: str, lang: str, voice: str) -> tuple[np.ndarray, int]:
        from vllm.inputs import TokensPrompt

        chunks = sentences(text)
        prompts = [TokensPrompt(prompt_token_ids=self.tok.tokenizer.encode(
            self.tok.create_prompt(c, lang=lang, speaker_name=voice), add_special_tokens=False)) for c in chunks]
        with self.yarn_lock:
            outs = self.llm.generate(prompts, self.sp, use_tqdm=False)
            gap = np.zeros(int(24000 * GAP_S), dtype=np.float32)
            parts = []
            for o in outs:
                codes = self.tok.extract_integers(self.tok.tokenizer.decode(o.outputs[0].token_ids))
                if codes:
                    parts += [self._decode(codes).squeeze().float().numpy(), gap]
        return (np.concatenate(parts[:-1]) if parts else np.zeros(0, np.float32)), 24000

    def _decode(self, codes: list[int]) -> torch.Tensor:
        """WavTokenizer decode. On GB10 cuFFT has failed with CUFFT_INTERNAL_ERROR (seen 2026-11-16) and stayed broken
        for every later request: clear the FFT plan cache and retry once; if it fails again the CUDA context is gone,
        so exit and let Docker's restart policy bring the service back clean instead of answering 500 forever."""
        try:
            return self.tok.get_audio(codes)
        except RuntimeError as exc:
            if "cufft" not in str(exc).lower():
                raise
            torch.backends.cuda.cufft_plan_cache.clear()
            torch.cuda.empty_cache()
            try:
                return self.tok.get_audio(codes)
            except RuntimeError as exc2:
                if "cufft" in str(exc2).lower():
                    print(f"cuFFT unusable ({exc2}); exiting for a clean restart", flush=True)
                    os._exit(3)
                raise

    def mms_tts(self, text: str, lang: str) -> tuple[np.ndarray, int]:
        model, tok = self.vits[lang]
        parts = []
        with self.mms_lock, torch.no_grad():
            for c in sentences(text):
                ids = tok(c, return_tensors="pt").to("cuda")
                parts += [model(**ids).waveform.squeeze().float().cpu().numpy(),
                          np.zeros(int(model.config.sampling_rate * GAP_S), np.float32)]
        return (np.concatenate(parts[:-1]) if parts else np.zeros(0, np.float32)), model.config.sampling_rate

    # ------------------------------------------------------------------ ASR
    def transcribe(self, audio: np.ndarray, rate: int, lang: str) -> str:
        audio = resample(audio, rate, 16000)
        with self.mms_lock, torch.no_grad():
            if self.asr_lang != lang:
                self.asr_proc.tokenizer.set_target_lang(lang)
                self.asr.load_adapter(lang)
                self.asr_lang = lang
            inp = self.asr_proc(audio, sampling_rate=16000, return_tensors="pt").to("cuda")
            return self.asr_proc.decode(self.asr(inp.input_values.half()).logits[0].argmax(-1)).strip()


class TTSRequest(BaseModel):
    text: str = Field(min_length=1, max_length=MAX_CHARS)
    language: str = Field(min_length=2, max_length=5)
    voice: Optional[str] = None
    sample_rate: int = Field(default=24000, ge=8000, le=48000)


def create_app(engines: Engines) -> FastAPI:
    app = FastAPI(title="agora naija-speech")

    def need_ready() -> None:
        if not engines.ready:
            raise HTTPException(503, engines.error or "models are loading")

    @app.get("/healthz")
    def healthz() -> dict:
        return {"status": "ok" if engines.ready else ("error" if engines.error else "loading"), "error": engines.error,
                "tts": sorted(list(YARN_LANG) + list(MMS_TTS)), "asr": sorted(ASR_LANG)}

    @app.get("/voices")
    def voices() -> dict:
        out = [{"id": v, "languages": [code], "gender": "male" if "male" in v and "female" not in v else "female",
                "labels": ["yarngpt"]} for code, lang in YARN_LANG.items() for v in YARN_VOICES[lang]]
        out += [{"id": f"mms-{k}", "languages": [k], "gender": "", "labels": ["mms"]} for k in MMS_TTS]
        return {"voices": out}

    @app.post("/tts")
    def tts(req: TTSRequest) -> Response:
        need_ready()
        code = req.language.strip().lower()
        started = time.time()
        if code in YARN_LANG:
            lang = YARN_LANG[code]
            voice = req.voice if req.voice in YARN_VOICES[lang] else YARN_VOICES[lang][0]
            audio, rate = engines.yarn(req.text, lang, voice)
            engine = "yarngpt"
        elif code in MMS_TTS:
            audio, rate = engines.mms_tts(req.text, code)
            engine = "mms"
        else:
            raise HTTPException(422, f"unsupported language {code!r}; supported: {sorted(list(YARN_LANG) + list(MMS_TTS))}")
        if audio.size == 0:
            raise HTTPException(502, "no audio produced")
        audio = resample(audio, rate, req.sample_rate)
        return Response(wav_bytes(audio, req.sample_rate), media_type="audio/wav",
                        headers={"X-Engine": engine, "X-Seconds": f"{time.time() - started:.2f}"})

    @app.post("/asr")
    async def asr(request: Request, language: str = Query(...)) -> dict:
        need_ready()
        code = language.strip().lower()
        if code not in ASR_LANG:
            raise HTTPException(422, f"unsupported language {code!r}; supported: {sorted(ASR_LANG)}")
        body = await request.body()
        if not body or len(body) > 20_000_000:
            raise HTTPException(413 if body else 422, "send one utterance as audio/wav (max 20 MB)")
        try:
            audio, rate = sf.read(io.BytesIO(body), dtype="float32")
        except Exception as exc:  # noqa: BLE001
            raise HTTPException(422, f"not a readable wav: {exc}") from exc
        if audio.ndim > 1:
            audio = audio.mean(axis=1)
        started = time.time()
        text = engines.transcribe(audio, rate, ASR_LANG[code])
        return {"text": text, "language": code, "seconds": round(time.time() - started, 2)}

    return app


def main() -> None:  # pragma: no cover - process entrypoint (vLLM spawns workers: keep under __main__)
    engines = Engines()
    engines.load()
    uvicorn.run(create_app(engines), host="0.0.0.0", port=int(os.environ.get("PORT", "5820")))


if __name__ == "__main__":
    main()
