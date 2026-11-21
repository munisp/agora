# voice-corpus (SPEC-W103)

Commissioned native-speaker corpus **registry + QA + training-manifest
export** — the fine-tune READINESS scaffold for owned Nigerian voices. It
serves both the CosyVoice-2 Naija-TTS fine-tune track and any future
LongCat-AudioDiT spike.

**This is honest data infrastructure**: no model code, no GPU path, no mock
mode — the service never synthesizes anything. A fine-tune itself remains a
separate project (data volume, GPU budget, native-speaker eval); this
service makes the corpus for it collectible, consented, and exportable.

## What it does

- **Fail-closed consent** (mirrors the cosyvoice/longcat sidecars):
  enrolling a speaker without a non-empty `consent_basis` → **422**, and the
  registry re-asserts before any bytes are written.
- **NDPA posture**: speakers are **pseudonyms only** (never legal names);
  `consent_reference` points at the signed artefact; consent revocation is
  `DELETE /v1/speakers/{id}` which **really deletes** the audio files and
  all records (returns `{deleted_speaker, deleted_utterances,
  deleted_bytes}`) — no tombstones.
- **QA at ingest**: audio must be a decodable wav (duration/rate/channels
  are probed from the file itself, never trusted from the client), duration
  bounded to 0.5–30 s, decoded size capped at 10 MB, language allowlisted by
  `CORPUS_LANGUAGES` (default `pcm,yo,ha,ig,en`).
- **Tone-mark detection**: Yoruba and Igbo are tonal. Every transcript is
  NFC-normalized and scanned for tone diacritics (acute/grave/macron/…;
  dot-below vowel-quality letters like Igbo ị ọ ụ are correctly NOT tone
  marks). Coverage is surfaced per language in `/v1/stats` — a low
  percentage on a tonal language is a readiness warning for fine-tuning.
- **Training-manifest export**: `GET /v1/export?language=yo&format=jsonl`
  (or `ljspeech` `path|text|speaker_id`). Consented by construction — only
  consented, non-deleted records can exist in the store.

## Run

```bash
docker compose -f services/voice-agent-runtime/docker-compose.fragment.yml \
  --profile corpus up -d voice-corpus
```

Local dev (no docker):

```bash
pip install -r requirements.txt
CORPUS_DIR=/tmp/corpus python -m app.main   # :5840
```

## API summary

```
POST   /v1/speakers     {pseudonym, languages, consent_basis,
                         consent_reference?, source} → 201 (422 blank consent)
GET    /v1/speakers     · GET /v1/speakers/{id} · DELETE /v1/speakers/{id}
POST   /v1/utterances   {speaker_id, language, transcript, audio_base64} → 201
                        (404 speaker · 400 language/base64/wav · 413 size · 422 duration/transcript)
GET    /v1/utterances?speaker_id=&language= · DELETE /v1/utterances/{id}
GET    /v1/stats        per-language {utterances, seconds, speakers,
                                      tone_mark_coverage_pct}
GET    /v1/export?language=&format=jsonl|ljspeech
GET    /healthz
```

See `docs/voice-corpus.md` for the full contract and the readiness
guideline (≈10+ hours per language before a fine-tune spike is a useful
rule of thumb — a guideline, not a guarantee).
