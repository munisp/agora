# Voice-quality conformance gates (SPEC-W11 Part E)

`services/voice-agent-runtime/eval/quality_gates.py` turns voice quality from
a vibes-based review into a release gate: it consumes eval-harness artifacts
and **SessionMetrics JSON exports** (the `quality` object that
`app/metrics.py: SessionMetrics.quality_payload` attaches to the SessionEnded
CloudEvent) and enforces four gates. Every gate prints PASS / FAIL / SKIP with
evidence lines and a JSON report lands in `eval/quality_report.json`; the
process exits non-zero when any gate FAILs. SKIP marks inputs that were not
provided and never fails the run.

```bash
# full run against a live stack (make up && make seed first)
scripts/eval-quality-gate.sh

# gates only, offline / CI fixture mode
SKIP_HARNESS=true METRICS_JSON=/tmp/session_metrics.json \
  TTS_WAVS=/tmp/preview.wav scripts/eval-quality-gate.sh

# direct
python3 services/voice-agent-runtime/eval/quality_gates.py \
  --metrics session_metrics.json --stt-results stt_hypotheses.json \
  --tts-wav preview.wav
```

## Gate reference

| Gate | What it measures | Threshold (env, default) | Input |
|------|------------------|--------------------------|-------|
| LATENCY | mouth-to-ear p95 across sessions | `QUALITY_P95_MS` = 2500 | `--metrics` session-metrics export |
| STT_WER | mean word-error rate per language | `QUALITY_MAX_WER` = 0.25 | `eval/corpora/*.yaml` + `--stt-results` |
| TTS | synthesized-wav sanity (duration > 0, silence ratio < 0.8) | fixed | `--tts-wav` file(s) |
| MOS_PROXY | composite 1–5 score from latency + interruptions + WER | `QUALITY_MIN_MOS` = 3.5 | same as LATENCY + STT_WER |

### LATENCY — mouth-to-ear p95

Mouth-to-ear (caller stops speaking → agent audio starts) is derived from the
existing latency metric fields, per session, in this precedence order:

1. explicit `mouth_to_ear_ms` per-turn sample list (richest export);
2. per-stage sample lists `stt_latencies_ms` + `llm_latencies_ms` +
   `tts_latencies_ms`, summed elementwise;
3. aggregate `avg_{stt,llm,tts}_latency_ms` fields;
4. today's `quality_payload` shape (`avg_llm_latency_ms` only) plus
   configurable allowances `QUALITY_STT_ALLOWANCE_MS` (350) and
   `QUALITY_TTS_ALLOWANCE_MS` (450) — an explicit, evidence-line-flagged
   **lower-bound estimate** until per-stage session exports land (the
   Prometheus histograms `voice_{stt,llm,tts}_latency_seconds` already exist
   for dashboards; this gate reads the per-session JSON export).

p95 uses the nearest-rank method over all derived samples.

### STT_WER — per-language word-error rate

Corpus manifests live in `eval/corpora/` (format + contribution guide:
`eval/corpora/README.md`). Audio is **not committed**; the manifest documents
each expected path. Hypotheses come from `--stt-results` JSON
(`{sample_id: transcript}`) produced by running the runtime's STT over the
corpus audio, or from the manifest's own `hypothesis` field (committed
self-test fixtures only). WER uses **jiwer** when installed, otherwise the
minimal word-Levenshtein implementation inside `quality_gates.py` — no hard
dependency. Each language's mean WER must be ≤ `QUALITY_MAX_WER`.

### TTS — automated sanity + manual intelligibility protocol

Automated (stdlib `wave` only): each wav passed via `--tts-wav` must decode,
have duration > 0 and a silence ratio < 0.8 (100 ms chunks, silent when peak
amplitude < 2 % of full scale). Use the runtime's TTS preview endpoint to
synthesize the wav.

Manual protocol (required before release; the automated gate covers sanity
only):

1. Synthesize three fixed prompts per enabled voice/locale: a greeting, a
   price answer, an emergency instruction ("Fire service don dey come. Stay
   for outside." for pcm).
2. Two listeners who did not pick the prompts transcribe them blind.
3. Pass when both transcriptions are semantically exact (numbers, names,
   negations correct) and neither listener reports artifacts
   (clipping, robotic cadence, wrong language switch).
4. Record the outcome in the release notes next to the gate report.

### MOS_PROXY — composite score (documented formula)

```
lat_pen = 1.0 * clamp((p95_ms - 1000) / 2000, 0, 1)   # 0 at ≤1.0 s, full point at ≥3.0 s
int_pen = 0.5 * clamp(interruption_rate / 0.15, 0, 1) # ½ point when ≥15 % of turns interrupted
wer_pen = 2.0 * clamp(wer / 0.40, 0, 1)               # 2 points at WER ≥ 0.40
MOS     = clamp(5 - lat_pen - int_pen - wer_pen, 1, 5)
```

`interruption_rate = Σ interruptions / Σ turns` across the exported sessions
(treated as 0 with an evidence note when the export predates interruption
tracking). Note the deliberate headroom requirement: sitting exactly on every
individual threshold (p95 = 2500, WER = 0.25) yields MOS ≈ 3.0, i.e. a system
that merely meets each gate in isolation still fails the composite — healthy
deployments need margin, which is what MOS ≈ user-perceived quality rewards.

## CI wiring

`.github/workflows/ci.yml` is **not** edited by this change — the automation
token lacks workflow scope, so a maintainer must add the lane manually:

```yaml
  quality-gates:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-python@v5
        with:
          python-version: ${{ env.PYTHON_VERSION }}
      - run: pip install pyyaml   # jiwer optional; builtin fallback otherwise
      - name: offline fixture gates
        run: SKIP_HARNESS=true scripts/eval-quality-gate.sh
```

The offline fixture lane needs no services: it runs the gates against the
committed corpus fixtures (STT_WER active) and skips LATENCY/MOS/TTS for lack
of exports. For the full lane, add a service job that boots the voice stack,
captures SessionEnded `quality` objects into `metrics.json`, synthesizes a
preview wav, and runs
`METRICS_JSON=metrics.json TTS_WAVS=preview.wav scripts/eval-quality-gate.sh`.

## Tuning the defaults

- `QUALITY_P95_MS` (2500): mouth-to-ear budget. VOICE-SCALING treats ~2.5 s as
  the ceiling for turn-taking to feel conversational; tighten to 2000 for
  tenants on local GPUs, loosen only with a recorded justification.
- `QUALITY_MAX_WER` (0.25): per-language ceiling. 0.25 is the "usable but
  degraded" line; once real corpora land (see corpora README), track the
  trend per language and tighten toward 0.15 for `en`.
- `QUALITY_MIN_MOS` (3.5): composite floor. 3.5 ≈ "fair" on the 1–5 MOS
  scale — below it callers reliably perceive the agent as sluggish/error-prone.
- `QUALITY_STT_ALLOWANCE_MS` / `QUALITY_TTS_ALLOWANCE_MS` (350/450): only used
  for exports lacking per-stage latency fields. Set from your observed
  `voice_stt_latency_seconds` / `voice_tts_latency_seconds` histogram medians,
  or (better) export per-stage session latencies so the allowances retire.
