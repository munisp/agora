# LongCat-TTS sidecar (SPEC-W102)

Consent-gated voice-cloning TTS over Meituan **LongCat-AudioDiT**
(https://github.com/meituan-longcat/LongCat-AudioDiT) — a waveform-latent
diffusion TTS with SOTA zero-shot voice cloning (Seed-bench SIM 0.818
Seed-ZH / 0.786 EN for the 3.5B variant). **MIT-licensed for BOTH code and
weights** — commercial-safe, like CosyVoice-2 (docs/voice-licensing.md).

Mirrors `services/cosyvoice-tts` one-for-one (same HTTP contract, same
consent posture) so the voice runtime's `longcat` chain provider is a
drop-in sibling of `cosyvoice`.

    POST   /tts           {"text", "voice_id", "language"}   -> audio/wav
    POST   /voices        {"name", "sample_base64",
                           "consent_basis", "source",
                           "license"?, "languages"?,
                           "prompt_text"?}                   -> {"voice_id"}
    GET    /voices        -> {"voices": [...]}
    DELETE /voices/{id}   -> {"deleted": voice_id}
    GET    /healthz       -> {"status": "ok", ...}

## Honest capability notes

- **Languages: English + Chinese ONLY** (the model's benchmarked coverage).
  This is NOT a Nigerian-language (pcm/yo/ha/ig) path — Spitch/MMS remain
  those routes; `pcm` etc. are rejected 400. (A possible pcm/yo fine-tune
  is a HARD-GATED experiment scaffold only — see
  `docs/longcat-pcm-yo-spike.md` and `scripts/spike_prepare.py`,
  SPEC-W105.)
- **Cloning needs the transcript.** Zero-shot cloning concatenates
  `prompt_text + gen_text` and feeds the enrolled sample as prompt audio.
  Enroll WITHOUT `prompt_text` and the real path falls back to plain
  (uncloned) TTS — the voice record says so verbatim; nothing is faked.
- Output is 24 kHz mono PCM16 WAV in both modes.

## Modes

- `LONGCAT_MOCK=1` (default, this image): deterministic sine wav, pitch
  keyed to `voice_id` — no GPU, no model download. CI/dev posture.
- `LONGCAT_MOCK=0`: real inference. Lazy-loads `audiodit.AudioDiTModel`
  (`LONGCAT_MODEL_DIR`, default `meituan-longcat/LongCat-AudioDiT-1B`, ~2.5GB
  from HuggingFace) on first use. **Requires a CUDA GPU with >=8GB VRAM**
  (1B variant); CPU is not viable. Build the CUDA variant per the Dockerfile
  block. Knobs: `LONGCAT_STEPS` (16), `LONGCAT_CFG` (4.0),
  `LONGCAT_GUIDANCE` (`apg`), `LONGCAT_FRAMES_PER_SEC` (25).

## Consent (FAIL-CLOSED)

`POST /voices` without a non-empty `consent_basis` is rejected 422 before
any bytes are written; the registry re-asserts it (defense in depth). Every
voice record carries `{voice_id, name, languages, source, license,
consent_basis, prompt_text, enrolled_at}` (docs/VOICES.md).

## Run

```bash
docker build -t opendesk/longcat-tts:dev services/longcat-tts
docker run -p 5830:5830 opendesk/longcat-tts:dev          # mock
# real (GPU): build the CUDA variant, then
# docker run --gpus all -e LONGCAT_MOCK=0 -p 5830:5830 opendesk/longcat-tts:dev
```

Tests: `LONGCAT_MOCK=1 pip install fastapi httpx pytest soundfile numpy && pytest tests/ -q`
