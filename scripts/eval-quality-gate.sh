#!/usr/bin/env bash
# OpenDesk voice-quality gate (SPEC-W11 Part E).
# Runs the eval harness (scenario replay + judge) and then the quality gates
# (latency p95 / STT WER / TTS sanity / MOS-proxy). Exits non-zero when the
# harness or any gate fails — CI-ready.
#
# Prereqs for a full run: `make up` (voice runtime on :7006) plus a session-
# metrics export and, for the STT/TTS gates, hypotheses/wav artifacts.
# SKIP_HARNESS=true runs the gates only (offline fixture mode).
#
# Env overrides:
#   VOICE_BASE_URL   voice runtime base URL      (default http://localhost:7006)
#   EVAL_ARGS        extra args for eval.py      (default "--no-judge")
#   SKIP_HARNESS     true -> skip eval.py        (default false)
#   METRICS_JSON     SessionMetrics export path  (optional; LATENCY/MOS skip without it)
#   STT_RESULTS      STT hypotheses JSON path    (optional; per-sample override)
#   TTS_WAVS         space-separated wav paths   (optional; TTS gate skips without it)
#   CORPUS           corpus manifest path        (default eval/corpora/corpus.yaml)
#   QUALITY_P95_MS / QUALITY_MAX_WER / QUALITY_MIN_MOS   gate thresholds
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EVAL_DIR="$REPO_ROOT/services/voice-agent-runtime/eval"

VOICE_BASE_URL="${VOICE_BASE_URL:-http://localhost:7006}"
EVAL_ARGS="${EVAL_ARGS:---no-judge}"
SKIP_HARNESS="${SKIP_HARNESS:-false}"
METRICS_JSON="${METRICS_JSON:-}"
STT_RESULTS="${STT_RESULTS:-}"
TTS_WAVS="${TTS_WAVS:-}"
CORPUS="${CORPUS:-$EVAL_DIR/corpora/corpus.yaml}"

echo "== 1/2 eval harness (eval.py) =="
if [[ "$SKIP_HARNESS" == "true" ]]; then
  echo "SKIP_HARNESS=true — scenario replay skipped (gates only)"
else
  # shellcheck disable=SC2086
  python3 "$EVAL_DIR/eval.py" --base-url "$VOICE_BASE_URL" $EVAL_ARGS
fi

echo "== 2/2 quality gates (quality_gates.py) =="
gate_args=(--corpus "$CORPUS")
[[ -n "$METRICS_JSON" ]] && gate_args+=(--metrics "$METRICS_JSON")
[[ -n "$STT_RESULTS" ]] && gate_args+=(--stt-results "$STT_RESULTS")
if [[ -n "$TTS_WAVS" ]]; then
  # shellcheck disable=SC2086
  gate_args+=(--tts-wav $TTS_WAVS)
fi
python3 "$EVAL_DIR/quality_gates.py" "${gate_args[@]}"

echo "EVAL + QUALITY GATES PASSED"
