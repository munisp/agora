# mms-tts — Meta MMS text-to-speech sidecar

FastAPI sidecar serving Nigerian/African languages via Meta's
`facebook/mms-tts-{eng,pcm,yor,ibo,hau}` checkpoints (SPEC-W10 Part B).
Consumed by `voice-agent-runtime` (`app/tts_providers/mms.py`).

## HTTP contract

| Endpoint | Body | Response |
|---|---|---|
| `POST /tts` | `{"text": str, "lang": "eng\|pcm\|yor\|ibo\|hau"}` | `audio/wav` bytes |
| `GET /voices` | — | `{"voices": [{"id", "languages", "gender", "labels"}]}` |
| `GET /healthz` | — | `{"status": "ok", "mock": bool, ...}` |

Port: **5800**. See `docs/voices-sidecars.md` for the full runbook.

## Mock vs real

- `MMS_MOCK=1` (**default**): deterministic sine-based wav (22.05 kHz 16-bit
  mono), pitch keyed by language, duration proportional to text length. No
  model downloads; torch/transformers are never imported.
- `MMS_MOCK=0`: real VITS inference. Models are lazy-loaded per language on
  first request and gated by `MMS_LANGS` (default `eng,pcm`) — requesting a
  language outside the allow-list returns a 400 without downloading anything.
  Checkpoints (~100–250 MB each) are cached under `HF_HOME=/models` (mount the
  `mms-models` volume).

CPU inference is viable for MMS (short utterances synthesize in ~1–3 s on a
modern CPU); no GPU required. The Dockerfile installs CPU-only torch wheels.

## Develop / test

```bash
pip install fastapi httpx pytest soundfile numpy
MMS_MOCK=1 pytest tests/ -q          # contract tests (no torch needed)
MMS_MOCK=1 python -m app.main        # serve on :5800
```

## License

MMS checkpoints are **CC-BY-NC-4.0** (non-commercial). See
`docs/voices-sidecars.md` before any commercial deployment.
