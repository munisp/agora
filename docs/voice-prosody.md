# Voice prosody layer (SPEC-W106)

Chain-level DSP addressing the Nigerian-language quality complaints:
**"too fast"**, **"choppy"** joins, and **pitch artifacts** from
sample-rate mismatches. It lives in the voice runtime's `FallbackTTS`
chain, so it applies uniformly to every provider (piper, mms, xtts,
spitch, cosyvoice, qwen3, longcat) without touching any sidecar.

**Default: OFF.** With `TTS_PROSODY` unset the layer is not wired and
pre-W106 audio is **bit-identical** (the house opt-in idiom, same as
`VOICEPRINTS` / `INTEL_LLM`).

## Controls

```bash
TTS_PROSODY=on
TTS_PROSODY_MAP="pcm:0.95:120,yo:0.95:120,ha:0.97:100,ig:0.95:120"
```

Map entries are `lang:rate[:join_pad_ms]`:

| Control | Meaning | Effect |
|---|---|---|
| `rate` | speech speed (output duration = input ÷ rate) | `0.95` ≈ 5% slower — the "too fast" fix. Clamped to [0.5, 2.0]; **stay within [0.85, 1.15]** |
| `join_pad_ms` | silence inserted **between** streamed sentence chunks | lets prosodic boundaries breathe — the "choppy" fix |

A `"*"` entry is the fallback for languages without their own policy;
languages with no matching policy are untouched.

## What it does (and honestly doesn't)

- **Time-stretch**: overlap-add (OLA) with Hann/COLA framing *plus*
  lightweight waveform-similarity alignment (WSOLA-lite — per-frame
  ±hop/2 alignment against the synthesized tail). Pitch is preserved (a
  220 Hz tone stretched 1.5× stays 220 Hz within 5% — covered by tests).
  **Honest limit**: this is not phase-vocoder/commercial-grade TSM —
  clean for small adjustments; artifacts grow beyond ~15% change. Do not
  use it to force a voice to a foreign cadence; use it for the last few
  percent.
- **Join pads**: real silence between sentence chunks in the streamed
  call path (never before the first chunk; included in the cached
  full-utterance entry so replays are identical).
- **Sample-rate normalization**: when the layer is ON, provider audio at
  a foreign rate (e.g. 24 kHz cosyvoice/longcat on a 22.05 kHz chain) is
  linear-interp resampled to the chain rate — fixing the case that was
  previously warn-only ("audio may be pitched"). Honest limit: linear
  interpolation, not an anti-aliased resampler; adequate for the sidecar
  rate spread.

## Tuning guidance — these are STARTING POINTS, not researched values

The example values (`0.95`, `100–120 ms`) are deliberately conservative
defaults for pcm/yo/ha/ig based on the "too fast / choppy" complaint
shape. **Tune them with the W104 eval loop**: synthesize fixed eval
sentences per language at 2–3 candidate rates, collect native-speaker
scores (`/v1/eval/runs`), and ship the MOS winner. Per-language cadence
is a human judgment, not a constant.

Scope notes:

- The layer applies to the **call path** (`synthesize_pcm` /
  `stream_pcm`). The admin `/v1/tts-preview` endpoint returns
  **unprocessed provider audio** — preview shows the engine, the chain
  shapes the call.
- The full fix for Nigerian-language quality remains the W103→W104
  corpus→fine-tune track (owned voices) with Spitch as today's production
  default; prosody is the immediate quality patch, not the destination.
