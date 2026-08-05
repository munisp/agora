# Voice TTS sidecars — MMS & XTTS (SPEC-W10 Part B)

Two standalone FastAPI sidecars extend Agora TTS beyond piper:

| Sidecar | Port | Purpose | Mock default | Real inference |
|---|---|---|---|---|
| `mms-tts` | 5800 | Nigerian/African languages via Meta MMS | `MMS_MOCK=1` | CPU OK |
| `xtts-tts` | 5810 | Brand-voice cloning via Coqui XTTS-v2 | `XTTS_MOCK=1` | **GPU required (≥6 GB VRAM)** |

Both ship in **mock mode by default**: deterministic sine-based wav output
(22.05 kHz, 16-bit PCM mono) with language-/voice-keyed pitch and duration
proportional to text length. The full voice pipeline (runtime providers,
admin preview, enrollment flows) is e2e-testable with **zero model downloads
and no GPU**. Heavy deps (torch/transformers/Coqui TTS) are imported lazily
and only when mock mode is off.

## Run

```bash
docker compose -f docker-compose.yml -f infra/compose/voices.compose.yml up -d
```

Point the voice runtime at them (already the defaults):

```bash
MMS_TTS_URL=http://mms-tts:5800
XTTS_TTS_URL=http://xtts-tts:5810
```

## HTTP contracts

### mms-tts :5800

```
POST /tts     {"text": "Wetin dey happen?", "lang": "pcm"}  → 200 audio/wav
GET  /voices  → {"voices": [{"id", "languages", "gender", "labels"}, ...]}
GET  /healthz → {"status": "ok", "mock": true, "supported_langs": [...], ...}
```

- `lang` ∈ `eng, pcm, yor, ibo, hau` (400 otherwise; 422 on empty/overlong text, max 5000 chars).
- Voice catalog is static; `id` is the MMS language code; language mappings:
  `eng→en`, `pcm→pcm`, `yor→yo`, `ibo→ig`, `hau→ha`.

### xtts-tts :5810

```
POST   /tts         {"text": "...", "voice_id": "...", "language": "en"} → 200 audio/wav
POST   /voices      {"name": "Brand", "sample_base64": "..."} → 201 {"voice_id", "voice"}
GET    /voices      → {"voices": [{"id","name","languages","gender","labels","created_at"}]}
DELETE /voices/{id} → {"deleted": "<id>"}  (404 if unknown)
GET    /healthz     → {"status": "ok", "mock": true, "voices": N, ...}
```

- `language` ∈ XTTS-v2's 17 languages: `en, es, fr, de, it, pt, pl, tr, ru, nl, cs, ar, zh-cn, ja, ko, hu, hi` (400 otherwise). **pcm/yo/ha/ig are not supported by XTTS-v2 — route those to mms-tts.**
- Unknown `voice_id` → 404. Invalid base64 / tiny samples (<1000 bytes decoded) → 400; >50 MB decoded → 413.
- Registry persists to `{VOICES_DIR}/voices.json` + `{VOICES_DIR}/samples/{voice_id}.wav` (default `/data`, the `xtts-data` volume). Enrollment sample: 6–30 s clean wav.

### curl examples

```bash
# MMS
curl -s http://localhost:5800/healthz | jq .
curl -s http://localhost:5800/voices | jq .
curl -s -X POST http://localhost:5800/tts \
  -H 'content-type: application/json' \
  -d '{"text":"How far? Wetin dey happen for Lagos today?","lang":"pcm"}' \
  -o pcm.wav && ffplay pcm.wav

# XTTS
SAMPLE_B64=$(base64 -w0 speaker.wav)
curl -s -X POST http://localhost:5810/voices \
  -H 'content-type: application/json' \
  -d "{\"name\":\"Brand Voice\",\"sample_base64\":\"$SAMPLE_B64\"}" | jq .
# → {"voice_id":"<id>", ...}
curl -s -X POST http://localhost:5810/tts \
  -H 'content-type: application/json' \
  -d '{"text":"Welcome to our store.","voice_id":"<id>","language":"en"}' -o brand.wav
curl -s http://localhost:5810/voices | jq .
curl -s -X DELETE http://localhost:5810/voices/<id> | jq .
```

## Mock vs real enablement

### mms-tts

| | Mock (default) | Real |
|---|---|---|
| Enable | `MMS_MOCK=1` | `MMS_MOCK=0` |
| Hardware | any | CPU OK (~1–3 s per short utterance) |
| Models | none | `facebook/mms-tts-{lang}` lazy per language, gated by `MMS_LANGS` (default `eng,pcm`); ~100–250 MB per checkpoint, cached in the `mms-models` volume (`HF_HOME=/models`) |
| Enable all languages | — | `MMS_LANGS=eng,pcm,yor,ibo,hau` |

Requesting a language outside `MMS_LANGS` in real mode returns a clear 400
**without downloading anything**.

### xtts-tts

| | Mock (default) | Real |
|---|---|---|
| Enable | `XTTS_MOCK=1` | `XTTS_MOCK=0` |
| Hardware | any | **CUDA GPU, ≥6 GB VRAM — CPU not viable** |
| Models | none | Coqui XTTS-v2 checkpoint ~1.8 GB, downloaded on first synthesis (persist via `TTS_HOME` on the volume or pre-bake at build) |
| Image | default `Dockerfile` (python:3.11-slim) | CUDA variant: `pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime` + `requirements-gpu.txt` (block at the bottom of `services/xtts-tts/Dockerfile`) + compose `deploy` GPU reservation (commented in `infra/compose/voices.compose.yml`) |

GPU sizing:

| GPU | VRAM | XTTS-v2 |
|---|---|---|
| RTX 3060 12GB / T4 16GB | 12–16 GB | comfortable |
| RTX 4060 / A10G | 8–24 GB | comfortable |
| 6 GB cards | 6 GB | minimum, tight |
| CPU only | — | **not viable** (minutes per utterance) |

## Licenses — READ BEFORE COMMERCIAL USE

> **⚠ MMS (facebook/mms-tts-*) checkpoints are licensed CC-BY-NC-4.0 —
> NON-COMMERCIAL use only.** Serving MMS-generated audio in a commercial
> product (paid SaaS voice agents) is not permitted under that license.
> Commercial deployments must either obtain separate terms from Meta or use a
> commercially licensed provider for these languages (Azure en-NG, Spitch, or
> licensed piper voices). Treat the mms-tts sidecar as a dev/eval path unless
> licensing is resolved.

> **⚠ XTTS-v2 weights are under the Coqui Public Model License — also
> NON-COMMERCIAL.** Same implication: do not ship cloned-voice output in a
> commercial offering without a commercial license. Additionally, voice
> cloning requires documented **speaker consent** (NDPA compliance) — the
> admin enrollment flow requires a consent checkbox, and samples are stored
> on the `xtts-data` volume (treat as personal data: access control +
> deletion via `DELETE /voices/{id}`).

The sidecar *code* in this repo follows the repo's license; the restrictions
above apply to the downloaded model weights only.

## Operations

- Compose override: `infra/compose/voices.compose.yml` (restart
  `unless-stopped`, HTTP healthchecks via `/healthz`, volumes `mms-models` /
  `xtts-data`, joins the root `opendesk` network).
- Local dev (no docker): `pip install fastapi uvicorn soundfile numpy httpx`
  then `MMS_MOCK=1 python -m app.main` / `XTTS_MOCK=1 VOICES_DIR=./data python -m app.main`
  from each service dir.
- Tests: `pytest tests/ -q` in each service (mock mode; torch/TTS not needed —
  lazy imports). See `services/mms-tts/README.md` and `services/xtts-tts/README.md`.
