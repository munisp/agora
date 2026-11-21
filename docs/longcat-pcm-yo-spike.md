# LongCat pcm/yo spike — experiment plan with HARD GATES (SPEC-W105)

> **Status: SCAFFOLD.** This document is the plan and the gate contract for
> a possible LongCat-AudioDiT Nigerian-language fine-tune spike. Nothing in
> the repo trains the model, and this document **forbids the training run**
> until every gate below passes. LongCat's production role today is the
> en/zh premium brand-voice tier (see `docs/voices-sidecars.md`); Nigerian
> languages stay on Spitch/Azure/MMS and the CosyVoice track.

## Why a spike at all

LongCat-AudioDiT is MIT for code **and** weights — the strongest license in
the TTS fleet — with SOTA zero-shot similarity on en/zh. If a Nigerian
fine-tune worked, the platform would own a commercially-clean, self-hosted,
cloning-grade Nigerian voice engine. That is option value worth preparing
for — **not** evidence the fine-tune will succeed.

## Honest risk register (first-class outcomes, not failures)

1. **Tone architecture** — Yoruba is tonal; the en/zh text encoder has no
   explicit tone representation. Fine-tuning alone may not teach tone. If
   the spike produces tone-deaf Yoruba, the correct verdict is "encoder
   extension required" — deeper research, not more data.
2. **Tokenizer** — the en/zh tokenizer never saw pcm/yo text. Gate G3
   measures this (fertility); a ratio >1.5 means a tokenizer-extension
   milestone exists before any training.
3. **Underperformance vs CosyVoice** — the Apache-2.0 CosyVoice-2 track is
   the documented primary path. If CosyVoice pcm hits the eval bar, the
   LongCat spike is likely *unnecessary*; if it fails, the failure is
   probably pipeline-shaped (data/eval), which would fail the LongCat spike
   too. Either way the cheaper track goes first.
4. **Fallback verdict** — if gates fail or results disappoint, LongCat
   simply remains the en/zh premium tier. That outcome loses nothing.

## HARD GATES (all must pass before any GPU hour is spent)

- **G1 — data readiness.** `GET /v1/readiness/pcm` **and**
  `GET /v1/readiness/yo` on the voice-corpus service both return
  `ready: true` (W104 gate: ≥10 hours, ≥3 speakers, ≥80% tone-mark coverage
  for yo — `docs/voice-corpus.md`).
- **G2 — pipeline validation.** The CosyVoice-2 pcm fine-tune has completed
  and its W104 eval run reports **mos ≥ 3.5 over ≥10 scores across ≥3
  native-speaker reviewers** (`GET /v1/eval/runs/{run_id}`). Rationale: if
  the cheaper, architecturally-safer track cannot reach a listenable bar
  with this corpus and eval loop, the LongCat spike is **cancelled** — the
  constraint is the pipeline, not the model.
- **G3 — tokenizer coverage.** `scripts/spike_prepare.py` has been run
  against the corpus export and reviewed:
  - `fertility_ratio ≤ 1.5` → proceed to a LoRA-style fine-tune attempt;
  - `fertility_ratio > 1.5` → a **tokenizer-extension milestone** (vocab
    merge for pcm/yo orthography incl. tone diacritics) is inserted before
    any training run;
  - heuristic `oov_pct` is recorded for the file (it quantifies non-ASCII
    load, e.g. tone-marked vowels).

## Preparation tooling (shipped, CPU-only, zero GPU claims)

`services/longcat-tts/scripts/spike_prepare.py`:

```bash
# 1) export the corpus (voice-corpus service)
curl "$CORPUS/v1/export?language=yo&format=jsonl" > corpus-yo.jsonl

# 2) build the LongCat-style manifest + coverage report
LONGCAT_TOKENIZER=meituan-longcat/LongCat-AudioDiT-1B \   # optional; without
python -m scripts.spike_prepare \                          # it the report is
    --input corpus-yo.jsonl \                              # heuristic-only and
    --out manifest-yo.jsonl \                              # SAYS SO verbatim
    --coverage coverage-yo.json --language yo
```

- Accepts both voice-corpus export shapes (generic and CosyVoice); the
  manifest rows are `{utt, wav, text, spk}`.
- `coverage-*.json` carries `mode: "unicode-heuristic"` always, plus a
  `tokenizer` block when the real tokenizer loaded, or an explicit
  `tokenizer_mode: "unavailable: <reason>"` otherwise — never silent,
  never faked.

## Spike execution outline (only after G1–G3)

1. Data prep via `spike_prepare` (train/valid reuse of the deterministic
   W104 split where applicable).
2. Smoke run: 1k steps on a single GPU, synthesize 20 fixed eval sentences
   (tone-minimal-pair set included — meaning flips under tone errors).
3. W104 eval loop: register the run with the exact `model_ref`, upload
   samples, collect native-speaker scores; **≥10 scores across ≥3
   reviewers** before any conclusion.
4. Decision: MOS ≥ 3.5 **and** reviewer-flagged tone correctness → iterate;
   otherwise record the verdict and stop. LongCat stays en/zh either way.
