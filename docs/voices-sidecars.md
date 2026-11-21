# Voice TTS sidecars — MMS, XTTS, CosyVoice & LongCat (SPEC-W10 Part B / SPEC-W72 / SPEC-W102)

Four standalone FastAPI sidecars extend Agora TTS beyond piper:

| Sidecar | Port | Purpose | Mock default | Real inference |
|---|---|---|---|---|
| `mms-tts` | 5800 | Nigerian/African languages via Meta MMS | `MMS_MOCK=1` | CPU OK |
| `xtts-tts` | 5810 | Brand-voice cloning via Coqui XTTS-v2 | `XTTS_MOCK=1` | **GPU required (≥6 GB VRAM)** |
| `cosyvoice-tts` | 5820 | Consent-gated cloning via CosyVoice-2 (Apache-2.0) | `COSYVOICE_MOCK=1` | **GPU required (≥8 GB VRAM)** |
| `longcat-tts` | 5830 | Consent-gated cloning via LongCat-AudioDiT (MIT code + weights) — **en/zh only** | `LONGCAT_MOCK=1` | **GPU required (≥8 GB VRAM)** |

All ship in **mock mode by default**: deterministic sine-based wav output
(16-bit PCM mono; 22.05 kHz for mms/xtts, 24 kHz for cosyvoice/longcat) with
language-/voice-keyed pitch and duration proportional to text length. The
full voice pipeline (runtime providers, admin preview, enrollment flows) is
e2e-testable with **zero model downloads and no GPU**. Heavy deps
(torch/transformers/Coqui TTS/CosyVoice/LongCat AudioDiT) are imported
lazily and only when mock mode is off.

## Run

```bash
docker compose -f docker-compose.yml -f infra/compose/voices.compose.yml up -d
```

Point the voice runtime at them (already the defaults):

```bash
MMS_TTS_URL=http://mms-tts:5800
XTTS_TTS_URL=http://xtts-tts:5810
COSYVOICE_TTS_URL=http://cosyvoice-tts:5820
LONGCAT_TTS_URL=http://longcat-tts:5830
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

### cosyvoice-tts :5820 (SPEC-W72)

```
POST   /tts         {"text": "...", "voice_id": "...", "language": "en"} → 200 audio/wav
POST   /voices      {"name": "...", "sample_base64": "...",
                     "consent_basis": "...", "source": "...",
                     "license"?, "languages"?} → 201 {"voice_id", "voice"}
GET    /voices      → {"voices": [{voice_id, name, languages, source,
                                   license, consent_basis, enrolled_at}]}
DELETE /voices/{id} → {"deleted": "<id>"}  (404 if unknown)
GET    /healthz     → {"status": "ok", "mock": true, "voices": N, ...}
```

- **Consent is FAIL-CLOSED**: `POST /voices` with a missing, empty, or
  whitespace-only `consent_basis` → **422**, and nothing is persisted (the
  registry re-asserts it, defense in depth). Every `voices.json` record
  carries `{voice_id, name, languages, source, license, consent_basis,
  enrolled_at}`; record each voice in `docs/VOICES.md`.
- `language` ∈ CosyVoice-2 base-model languages `en, zh, ja, ko, yue`
  (+ cross-lingual) — 400 otherwise. **pcm/yo/ha/ig are not covered by the
  base model**; the Naija-TTS pcm fine-tune corpus is the candidate path
  (pre-use license gate — see `docs/voice-licensing.md`). Fine-tuned
  languages can be recorded per voice via the enrollment `languages` field.
- Same bounds as xtts-tts: unknown `voice_id` → 404; invalid base64 / tiny
  samples (<1000 bytes decoded) → 400; >50 MB decoded → 413; text 1–5000
  chars. Registry persists to `{VOICES_DIR}/voices.json` +
  `{VOICES_DIR}/samples/{voice_id}.wav` (default `/data`, the
  `cosyvoice-data` volume). Mock output is 24 kHz 16-bit mono.
- CosyVoice-2 is **Apache-2.0 (commercial-safe)** — the recommended cloning
  path for owned Nigerian voices (vs. the non-commercial XTTS-v2 weights).

### longcat-tts :5830 (SPEC-W102)

```
POST   /tts         {"text": "...", "voice_id": "...", "language": "en"} → 200 audio/wav
POST   /voices      {"name": "...", "sample_base64": "...",
                     "consent_basis": "...", "source": "...",
                     "prompt_text"?, "license"?, "languages"?} → 201 {"voice_id", "voice"}
GET    /voices      → {"voices": [{voice_id, name, languages, source,
                                   license, consent_basis, prompt_text, enrolled_at}]}
DELETE /voices/{id} → {"deleted": "<id>"}  (404 if unknown)
GET    /healthz     → {"status": "ok", "mock": true, "voices": N, ...}
```

- **Consent is FAIL-CLOSED** (same as cosyvoice-tts): `POST /voices` with a
  missing, empty, or whitespace-only `consent_basis` → **422**, nothing
  persisted (registry re-asserts, defense in depth).
- `language` ∈ LongCat-AudioDiT's benchmarked languages **`en, zh`** — 400
  otherwise. **pcm/yo/ha/ig are NOT supported** (this is not a
  Nigerian-language path; keep `spitch`/`azure`/`mms` for those). Per-voice
  recorded `languages` may not exceed the base set either.
- **Zero-shot cloning needs the sample transcript**: enroll with
  `prompt_text` (what the reference sample says). Voices enrolled WITHOUT it
  honestly fall back to plain TTS — the sidecar never fakes cloning. The
  wire field is optional so the enroll shape matches the other sidecars when
  unused; the runtime provider forwards it when given.
- Same bounds as cosyvoice-tts: unknown `voice_id` → 404; invalid base64 /
  tiny samples (<1000 bytes decoded) → 400; >50 MB decoded → 413; text
  1–5000 chars. Registry persists to `{VOICES_DIR}/voices.json` +
  `{VOICES_DIR}/samples/{voice_id}.wav` (default `/data`, the
  `longcat-data` volume). Mock output is 24 kHz 16-bit mono.
- LongCat-AudioDiT is **MIT for BOTH code and weights (commercial-safe)** —
  Meituan's waveform-latent diffusion TTS with SOTA zero-shot similarity
  (SIM 0.818 ZH / 0.786 EN on the 3.5B-param benchmark). 1B checkpoint
  ≈2.5 GB; real inference wants ≥8 GB VRAM.

## Qwen3-TTS GPU server (SPEC-W74 — nari-labs/nari-qwen3-tts)

> **⚠ HARDWARE: the nari Qwen3-TTS container REQUIRES an NVIDIA H100**
> (single H100 SXM tested; NVIDIA Container Toolkit + a CUDA 13.0-compatible
> driver). There is NO CPU or consumer-GPU mode and no mock mode — do not add
> `qwen3` to `TTS_PROVIDER_CHAIN` on a host without an H100.

> **⚠ LANGUAGE: the engine has been tested with ENGLISH as the primary
> language ONLY.** This is **NOT a Nigerian-language path** — pcm/yo/ha/ig
> stay on `cosyvoice` (owned voices), `mms` (dev/eval), or `spitch` (hosted).

[nari-qwen3-tts](https://github.com/nari-labs/nari-qwen3-tts) is a
high-performance single-H100 serving implementation of Qwen3-TTS 1.7B
CustomVoice (**Apache-2.0** — see `docs/voice-licensing.md`). The published
image `ghcr.io/nari-labs/nari-qwen3-tts:latest` IS the sidecar — no shim:
the voice runtime's `qwen3` provider talks to it directly with the
OpenAI-shaped API:

```
POST /v1/audio/speech  {"model": "Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice",
                        "input": "...", "voice": "ryan", "language": "english",
                        "response_format": "wav", "stream": false} → audio/wav
GET  /health           liveness
GET  /ready            readiness — held FALSE until model load + CUDA-graph
                       capture + a warm-up TTS request have ALL completed
                       (slow boot: minutes; the compose healthcheck uses
                       start_period 600s for exactly this reason)
GET  /v1/models        model catalog
```

Enable it (opt-in `voice-gpu` profile — service defined in
`services/voice-agent-runtime/docker-compose.fragment.yml`):

```bash
docker compose -f docker-compose.yml \
  -f services/voice-agent-runtime/docker-compose.fragment.yml \
  --profile voice --profile voice-gpu up -d qwen3-tts
# runtime: TTS_PROVIDER_CHAIN=qwen3,piper (QWEN3_TTS_URL default
# http://qwen3-tts:8000 already wired in the fragment)
```

Serving profiles (`QWEN3_TTS_PROFILE`, default `balanced` in the fragment):
`ttfa` (time-to-first-audio optimized, smaller initial codec chunks),
`balanced` (default container profile), `throughput` (larger codec chunks +
batches under load). Published numbers: sub-50 ms p95 TTFA at 10 RPS on one
H100. `HF_TOKEN` is required for the first model pull; model files and
compiled kernels are cached in the `qwen3-cache` volume
(`QWEN3_TTS_MODEL_CACHE_DIR`, default `/home/nari/.cache`).

Watchdog note for ops: `qwen3` is deliberately **not** a default target of
`VOICE_WATCHDOG_TARGETS` in the voice-watchdog service (GPU-tier, opt-in —
the probe semantics would need a warm H100 to be meaningful).

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

# CosyVoice (consent_basis mandatory — fail-closed)
SAMPLE_B64=$(base64 -w0 speaker.wav)
curl -s -X POST http://localhost:5820/voices \
  -H 'content-type: application/json' \
  -d "{\"name\":\"Owned Naija Voice\",\"sample_base64\":\"$SAMPLE_B64\",\"consent_basis\":\"signed voice-release form 2026-11-16\",\"source\":\"staff-consented studio recording\",\"license\":\"proprietary (consent on file)\"}" | jq .
# → {"voice_id":"<id>", "voice": {...consent metadata...}}
curl -s -X POST http://localhost:5820/tts \
  -H 'content-type: application/json' \
  -d '{"text":"Welcome to our store.","voice_id":"<id>","language":"en"}' -o cosy.wav
curl -s http://localhost:5820/voices | jq .
curl -s -X DELETE http://localhost:5820/voices/<id> | jq .
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

### cosyvoice-tts

| | Mock (default) | Real |
|---|---|---|
| Enable | `COSYVOICE_MOCK=1` | `COSYVOICE_MOCK=0` |
| Hardware | any | **CUDA GPU, ≥8 GB VRAM — CPU not viable** |
| Models | none | CosyVoice2-0.5B checkpoint ~2 GB (`COSYVOICE_MODEL_DIR`, persist on a volume or pre-bake at build) |
| Image | default `Dockerfile` (python:3.11-slim) | CUDA variant: `pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime` + `requirements-gpu.txt` (block at the bottom of `services/cosyvoice-tts/Dockerfile`) + `--gpus all` |

Real mode does zero-shot cloning from the enrolled ~15 s reference sample
(`inference_zero_shot`). CosyVoice is installed from source
(`git+https://github.com/FunAudioLLM/CosyVoice`, Apache-2.0) — it is not on
PyPI.

### longcat-tts

| | Mock (default) | Real |
|---|---|---|
| Enable | `LONGCAT_MOCK=1` | `LONGCAT_MOCK=0` |
| Hardware | any | **CUDA GPU, ≥8 GB VRAM — CPU not viable** |
| Models | none | `meituan-longcat/LongCat-AudioDiT-1B` HF snapshot ≈2.5 GB (`LONGCAT_MODEL_DIR`, persist on a volume or pre-bake at build) |
| Image | default `Dockerfile` (python:3.11-slim) | CUDA variant: `pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime` + `requirements-gpu.txt` (block at the bottom of `services/longcat-tts/Dockerfile`) + `--gpus all` |

Real mode uses the HuggingFace-native `audiodit` package
(`AudioDiTModel.from_pretrained`, `model.vae.to_half()`, tokenizer from
`model.config.text_encoder_model`; installed from source,
`git+https://github.com/meituan-longcat/LongCat-AudioDiT.git`) — MIT for
code AND weights. Zero-shot cloning concatenates the enrolled
`prompt_text` + target text and conditions on the enrolled sample as
`prompt_audio`; voices without `prompt_text` synthesize plain TTS honestly.
Output is 24 kHz 16-bit mono. Diffusion knobs: `LONGCAT_STEPS` (16),
`LONGCAT_CFG` (4.0), `LONGCAT_GUIDANCE` (`apg`).

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

> **✓ CosyVoice-2 (code + weights) is Apache-2.0 — COMMERCIAL-SAFE** and the
> recommended path for owned Nigerian voices. Consent still applies: the
> cosyvoice-tts sidecar enforces it **fail-closed at enrollment**
> (`consent_basis` mandatory, 422 otherwise) and every voice record carries
> the consent metadata — see `docs/voice-licensing.md` and `docs/VOICES.md`.

> **✓ LongCat-AudioDiT (code + weights) is MIT — COMMERCIAL-SAFE**, the
> strongest license posture in the sidecar fleet. Scope is **en/zh only**
> (benchmarked), so it complements rather than replaces the Nigerian-language
> paths. Same fail-closed consent enforcement at enrollment; cloning requires
> the sample transcript (`prompt_text`) — never implied otherwise.

The sidecar *code* in this repo follows the repo's license; the restrictions
above apply to the downloaded model weights only.

## Operations

- Compose override: `infra/compose/voices.compose.yml` (restart
  `unless-stopped`, HTTP healthchecks via `/healthz`, volumes `mms-models` /
  `xtts-data`, joins the root `opendesk` network). The cosyvoice-tts and
  longcat-tts sidecars
  are wired in `services/voice-agent-runtime/docker-compose.fragment.yml`
  (volumes `cosyvoice-data` / `longcat-data`, **mock default** — set
  `COSYVOICE_MOCK=0` / `LONGCAT_MOCK=0` only
  with a GPU + the CUDA Dockerfile variant).
- Local dev (no docker): `pip install fastapi uvicorn soundfile numpy httpx`
  then `MMS_MOCK=1 python -m app.main` / `XTTS_MOCK=1 VOICES_DIR=./data python -m app.main`
  from each service dir.
- Tests: `pytest tests/ -q` in each service (mock mode; torch/TTS not needed —
  lazy imports). See `services/mms-tts/README.md` and `services/xtts-tts/README.md`.
