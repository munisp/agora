# cosyvoice-tts — CosyVoice-2 voice-cloning sidecar

FastAPI sidecar for **consent-gated** voice cloning (SPEC-W72 A1), the
Apache-2.0 (commercial-safe) counterpart to the non-commercial XTTS path.
Consumed by `voice-agent-runtime` (`app/tts_providers/cosyvoice.py`).

## HTTP contract

| Endpoint | Body | Response |
|---|---|---|
| `POST /tts` | `{"text", "voice_id", "language"}` | `audio/wav` bytes (24 kHz) |
| `POST /voices` | `{"name", "sample_base64", "consent_basis", "source", "license"?, "languages"?}` | `201 {"voice_id", "voice"}` |
| `GET /voices` | — | `{"voices": [{voice_id, name, languages, source, license, consent_basis, enrolled_at}]}` |
| `DELETE /voices/{id}` | — | `{"deleted": voice_id}` |
| `GET /healthz` | — | `{"status": "ok", ...}` |

Port: **5820**. Voice registry: `voices.json` + `samples/*.wav` under
`VOICES_DIR` (default `/data`, mount the `cosyvoice-data` volume).

## Consent is fail-closed

Every voice record carries consent metadata
(`{voice_id, name, languages, source, license, consent_basis, enrolled_at}`)
and `POST /voices` **without a non-empty `consent_basis` is rejected 422**
before any bytes are written (the registry re-asserts it, defense in depth).
Record each enrolled voice in `docs/VOICES.md`.

## Mock vs real

- `COSYVOICE_MOCK=1` (**default**): deterministic sine wav (24 kHz 16-bit
  mono) with a voice-specific pitch derived from the `voice_id` hash. No GPU,
  no model download; the `cosyvoice` package is never imported.
- `COSYVOICE_MOCK=0`: real CosyVoice-2 zero-shot cloning
  ([FunAudioLLM/CosyVoice](https://github.com/FunAudioLLM/CosyVoice),
  **Apache-2.0**) from a ~15 s reference sample. **GPU required — ≥8 GB
  VRAM**; CPU inference is not viable. Use the CUDA base-image variant
  documented at the bottom of the `Dockerfile` +
  `requirements-gpu.txt` and pass `--gpus all`.

Languages (CosyVoice-2 base model): en, zh, ja, ko, yue (+ cross-lingual).
Nigerian languages (pcm/yo/ha/ig) are **not** covered by the base model —
the **Naija-TTS** dataset (Mozilla Data Collective, ~5h55m pcm read speech)
is the fine-tune/eval corpus candidate for a Pidgin voice; it is subject to
a **pre-use license/consent gate** — see `docs/voice-licensing.md` before any
download or training. Fine-tuned languages can be recorded per voice at
enrollment via the optional `languages` field.

## Config envs

| Env | Default | Meaning |
|---|---|---|
| `COSYVOICE_MOCK` | `1` | `1` = deterministic sine mock (no GPU/model); `0` = real CosyVoice-2 inference |
| `VOICES_DIR` | `/data` | registry (`voices.json`) + enrolled samples volume |
| `PORT` | `5820` | listen port (`python -m app.main`) |
| `COSYVOICE_MODEL_DIR` | `pretrained_models/CosyVoice2-0.5B` | real mode only: CosyVoice2-0.5B checkpoint dir |

Request bounds (mirror the xtts sidecar): text 1–5000 chars; enrollment
sample 1000 B–50 MB decoded; mock utterance duration 0.4–60 s.

## Develop / test

```bash
pip install fastapi httpx pytest soundfile numpy uvicorn
COSYVOICE_MOCK=1 pytest tests/ -q            # contract tests (no CosyVoice package needed)
COSYVOICE_MOCK=1 VOICES_DIR=./data python -m app.main   # serve on :5820
```

## License

CosyVoice-2 code + weights are **Apache-2.0** — commercial-safe. The consent
contract (above) still applies to every enrolled voice; see
`docs/voice-licensing.md` and `docs/VOICES.md`.
