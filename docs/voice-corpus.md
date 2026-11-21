# Voice corpus — commissioned native-speaker data pipeline (SPEC-W103)

The **fine-tune readiness scaffold** for owned Nigerian voices. It collects,
validates, consents, and exports native-speaker corpora (pcm/yo/ha/ig/en) so
that a future TTS fine-tune — the documented CosyVoice-2 Naija-TTS track, or
a LongCat-AudioDiT spike — starts from clean, consented, exportable data
instead of a folder of untracked wavs.

> **Honest scope**: this is **data infrastructure only**. There is no model
> code, no GPU path, and no mock mode — the service never synthesizes
> anything. A fine-tune remains a separate project (data volume, GPU budget,
> tokenizer/text-encoder work for tone languages, native-speaker eval loop).
> What this service guarantees is that when that project starts, the corpus
> is consented, QA'd, and one `GET /v1/export` away from a training
> manifest.

## Why a corpus service (and not ad-hoc files)

| Concern | Ad-hoc folder | voice-corpus |
|---|---|---|
| Consent (NDPA) | untracked | **fail-closed at enrollment** (422 without `consent_basis`; registry re-asserts) |
| Revocation | hope | `DELETE /v1/speakers/{id}` **really deletes** audio + records |
| Speaker privacy | filenames leak names | **pseudonyms only**; `consent_reference` points at the signed artefact |
| Audio truth | client-claimed metadata | duration/rate/channels **probed from the wav itself** |
| Tone readiness | invisible | per-utterance `has_tone_marks` + per-language coverage in `/v1/stats` |
| Training handoff | manual munging | `jsonl` / `ljspeech` manifest export |

## Contracts

```
POST   /v1/speakers     {"pseudonym": "...", "languages": ["yo"],
                         "consent_basis": "...",          # FAIL-CLOSED (422 if blank)
                         "consent_reference"?: "DMS-...", "source": "..."} → 201
GET    /v1/speakers     → {"speakers": [...]}
GET    /v1/speakers/{id} → 200 | 404
DELETE /v1/speakers/{id} → {"deleted_speaker", "deleted_utterances",
                            "deleted_bytes"}  (404 unknown; real deletion)

POST   /v1/utterances   {"speaker_id": "...", "language": "yo",
                         "transcript": "Ẹ káàrọ̀", "audio_base64": "..."} → 201
                        404 unknown speaker · 400 unknown language / bad
                        base64 / non-wav · 413 decoded >10 MB ·
                        422 duration outside [0.5, 30]s or blank transcript
GET    /v1/utterances?speaker_id=&language=
DELETE /v1/utterances/{id} → {"deleted"}  (404 unknown)

GET    /v1/stats        → {speakers, utterances, seconds, per_language: {
                          <lang>: {utterances, seconds, speakers,
                                   tone_mark_coverage_pct}}}
GET    /v1/export?language=&format=jsonl|ljspeech
                        jsonl: {"audio_path","text","speaker_id","language",
                                "duration_s","sample_rate"} per line
                        ljspeech: <audio_relpath>|<text>|<speaker_id>
GET    /healthz
```

Records: speakers `{speaker_id, pseudonym, languages, consent_basis,
consent_reference, source, created_at}`; utterances `{utterance_id,
speaker_id, language, transcript, has_tone_marks, duration_s, sample_rate,
channels, audio_relpath, created_at}`. Everything persists atomically (tmp +
`os.replace`) to `{CORPUS_DIR}/registry.json`; audio lives under
`{CORPUS_DIR}/audio/{speaker_id}/{utterance_id}.wav`.

## Tone-mark QA (why it matters)

Yoruba and Igbo are **tonal** — tone changes word meaning, and a TTS model
fine-tuned on tone-less transcripts will mispronounce. The service NFC-
normalizes every transcript and detects tone diacritics (acute, grave,
macron, circumflex, caron — including precomposed forms via NFD
decomposition). **Dot-below letters are correctly NOT tone marks**: Igbo
ị/ọ/ụ and Yoruba ẹ/ọ/ṣ mark *vowel quality* (phonemes), not tone. The
per-language `tone_mark_coverage_pct` in `/v1/stats` is the readiness
signal: commissioning guidelines for tonal languages should require
tone-marked transcripts, and a low percentage is a flag before any
fine-tune spend.

## Readiness guideline

≈ **10+ hours of transcribed, consented, studio-clean speech per language**
(with multiple speakers, and tone-marked transcripts for tonal languages) is
the working rule of thumb before a fine-tune spike is worth its GPU budget —
a guideline from industry practice, not a guarantee; fewer hours can still
be useful for a smoke-test run. Track progress with `/v1/stats`.

## Operations

- Compose: opt-in profile in
  `services/voice-agent-runtime/docker-compose.fragment.yml` —
  `docker compose ... --profile corpus up -d voice-corpus` (port **5840**,
  volume `voice-corpus-data`). Deliberately **not** on the `voice` profile,
  **not** a watchdog target, and **no** worker env: nothing in the realtime
  voice path consumes it at serving time.
- Env: `CORPUS_DIR` (default `/data`), `CORPUS_LANGUAGES`
  (default `pcm,yo,ha,ig,en` — allowlist for both speaker languages and
  utterance language), `PORT` (default 5840).
- Local dev: `pip install -r requirements.txt &&
  CORPUS_DIR=/tmp/corpus python -m app.main`.
- License note: the corpus service code follows the repo license; the
  *recordings* are commissioned works — track each speaker's
  `consent_basis` + `consent_reference` (and record them in
  `docs/VOICES.md` when a voice is enrolled into a serving sidecar).
