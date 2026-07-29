# STT WER corpora (SPEC-W11 Part E)

This directory holds the manifests for the STT word-error-rate (WER) gate in
`../quality_gates.py`. **Audio files are never committed** — manifests record
the path each audio file must have (relative to this directory, conventionally
`audio/<id>.wav`), and contributors supply the audio out-of-band (see below).

## Manifest format

`corpus.yaml` (and any additional `corpora.d/*.yaml` you add):

```yaml
version: 1
samples:
  - id: en-lagos-01            # unique
    lang: en                   # per-language gate key (en, pcm, yo, ...)
    transcript: "reference transcript, normalized spelling"
    audio: audio/en-lagos-01.wav   # NOT committed — documented path
    hypothesis: "..."          # optional; only for committed self-test fixtures
```

The gate resolves the STT hypothesis per sample in this order:

1. `--stt-results hypotheses.json` (`{"<sample id>": "<stt output>"}`) — the
   normal path for real corpora: run the runtime's STT over each `audio` file
   and capture the transcript;
2. the manifest's own `hypothesis` field — reserved for the tiny committed
   fixture samples (`fixture-*`) that let the gate self-test in CI with no
   audio and no STT provider.

Samples with neither are reported as SKIP (they never fail the gate).

WER backend: [jiwer](https://pypi.org/project/jiwer/) when installed
(`pip install jiwer`), otherwise the minimal word-Levenshtein implementation
inside `quality_gates.py` (same normalization: lowercase, strip punctuation,
collapse whitespace). Both are applied per language; the gate compares each
language's mean WER against `QUALITY_MAX_WER` (default 0.25).

## Capture specs for contributed audio

- mono PCM wav, 16 kHz, 16-bit (downsampled phone captures are acceptable —
  note the chain in the PR);
- 3–15 seconds per utterance, one speaker, quiet-enough room (no music/TV);
- reference transcript double-checked by a second listener;
- no personal data beyond what the script says — use scripted utterances
  (bookings, opening hours, prices), never real call recordings.

## Sourcing Nigerian-accented English and Pidgin (pcm) samples

These are the accents the deployment tenants actually speak, so generic
LibriSpeech-style corpora are not a substitute.

- **Record in-house first**: team members and tenant staff reading a short
  script (greeting, booking request, price question, cancellation, emergency
  phrase). 5–10 utterances per speaker, 3+ speakers per language, mixed
  gender/age. This is the highest-signal, zero-licensing-risk source.
- **NaijaVoices** (naijavoices.com, Lagos) — consented Nigerian speech data
  collection; check their current licensing/participation terms.
- **Mozilla Common Voice** — check for `pcm`/Nigerian English segments in
  current releases (CC0); coverage is thin, so treat as a supplement.
- **Community radio / podcast snippets** — only with explicit written
  permission; re-record instead when in doubt.
- For Pidgin, prefer naturally code-switched lines (English + pcm mixed), as
  that is how callers actually speak; mark them `lang: pcm`.

Keep per-language sample counts roughly balanced so one accent can't dominate
the per-language means, and version real corpora as
`corpora.d/<name>.yaml` rather than editing the self-test `corpus.yaml`.
