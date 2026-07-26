# xtts-tts — Coqui XTTS-v2 voice-cloning sidecar

FastAPI sidecar for brand-voice cloning (SPEC-W10 Part B). Consumed by
`voice-agent-runtime` (`app/tts_providers/xtts.py`).

## HTTP contract

| Endpoint | Body | Response |
|---|---|---|
| `POST /tts` | `{"text", "voice_id", "language"}` | `audio/wav` bytes |
| `POST /voices` | `{"name", "sample_base64"}` | `201 {"voice_id", "voice"}` |
| `GET /voices` | — | `{"voices": [{id, name, languages, gender, labels, created_at}]}` |
| `DELETE /voices/{id}` | — | `{"deleted": voice_id}` |
| `GET /healthz` | — | `{"status": "ok", ...}` |

Port: **5810**. Voice registry: `voices.json` + `samples/*.wav` under
`VOICES_DIR` (default `/data`, mount the `xtts-data` volume).

## Mock vs real

- `XTTS_MOCK=1` (**default**): deterministic sine wav (22.05 kHz 16-bit mono)
  with a voice-specific pitch derived from the `voice_id` hash. No GPU, no
  model download; the Coqui `TTS` package is never imported.
- `XTTS_MOCK=0`: real XTTS-v2 inference. **GPU required — ≥6 GB VRAM**
  (e.g. RTX 3060 12GB, T4 16GB, A10G). **CPU inference is not viable**
  (minutes per short utterance); do not enable real mode without a GPU. Use
  the CUDA base-image variant documented at the bottom of the `Dockerfile`
  (`pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime` + `requirements-gpu.txt`)
  and pass `--gpus all` / the compose `deploy` GPU reservation.

Languages (XTTS-v2): en, es, fr, de, it, pt, pl, tr, ru, nl, cs, ar, zh-cn,
ja, ko, hu, hi. Nigerian languages (pcm/yo/ha/ig) are **not** supported by
XTTS-v2 — use the mms-tts sidecar for those.

## Enrollment notes

`sample_base64` is the base64 of a 6–30 s clean wav of the speaker. **Consent
is mandatory** (NDPA) — the admin UI requires a consent checkbox; see
`docs/voices-sidecars.md`.

## Develop / test

```bash
pip install fastapi httpx pytest soundfile numpy uvicorn
XTTS_MOCK=1 pytest tests/ -q            # contract tests (no TTS package needed)
XTTS_MOCK=1 VOICES_DIR=./data python -m app.main   # serve on :5810
```

## License

XTTS-v2 weights are under the **Coqui Public Model License** — non-commercial
use only. See `docs/voices-sidecars.md` before any commercial deployment.
