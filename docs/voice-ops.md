# Voice ops runbook — latency bench & model-server watchdog (SPEC-W73 WS-B)

Two operator-facing tools for the voice stack:

- `services/voice-agent-runtime/eval/latency_bench.py` — stage-latency bench
  (STT / LLM / TTS / E2E p50/p95/mean).
- `services/voice-agent-runtime/ops/voice_watchdog.py` — model-server
  watchdog that probes **real inference** (health endpoints can lie) and
  restarts wedged sidecars via the docker socket.

---

## 1. Latency bench

### Default (offline) mode — runs anywhere

```bash
make bench-voice                                                # 20 iterations
make bench-voice BENCH_ARGS="--iterations 100 --json bench.json --md bench.md"
# or directly:
python3 services/voice-agent-runtime/eval/latency_bench.py --iterations 20 \
    --json bench.json --md bench.md
```

The default mode opens **zero sockets** and downloads **zero models**: it
drives the real pipeline code (whisper PCM conversion + decode call,
`run_tool_loop`, piper HTTP fetch + wav decode, metrics) with the model
inference replaced by deterministic fakes injected at the documented seams
(stub model at the `_model` test seam, stub `LLMInterface`, piper
`httpx.MockTransport` with the TTS LRU cache disabled so every iteration
measures the full path).

Exit code: `0` on success, non-zero on a harness error.

### Output schema (pinned)

```json
{
  "stt": {"p50_ms": ..., "p95_ms": ..., "mean_ms": ..., "min_ms": ..., "max_ms": ..., "n": ...},
  "llm": { ... }, "tts": { ... }, "e2e": { ... },
  "meta": {"iterations": 20, "mode": "offline", "ts": "..."}
}
```

`e2e` is the per-iteration sum of the three stages (the pipeline's serial
lower bound for one user turn).

### Interpreting the numbers

- **Offline mode numbers are harness overhead**, not production latencies —
  use them for *regression drift* (a code change that slows the pipeline
  scaffolding shows up here in CI) and for validating the percentile
  plumbing.
- **Production-representative numbers need `--live`** against the running
  stack (`make up-voice` first; `--live` reads `WHISPER_MODEL`,
  `LLM_BASE_URL`, `LLM_MODEL`, `PIPER_HTTP_URL`, `PIPER_VOICE`).
- Compare `--live` results against the runtime's own W71 histograms on the
  control plane `/metrics`: `voice_stt_latency_seconds`,
  `voice_llm_latency_seconds`, `voice_llm_first_token_seconds`,
  `voice_tts_latency_seconds`, `voice_tts_first_chunk_seconds`, and the
  end-to-end `voice_turn_total_seconds`. The bench p50/p95 should sit in the
  same ballpark as the histogram quantiles; if the bench is much faster than
  the histograms, the gap is orchestration (LiveKit, VAD, endpointing), not
  the models.

> **Honest note (env-blocked here):** `--live` requires running sidecars and
> a downloaded whisper model; it was validated for structure only in CI and
> needs a one-time operator validation on a real voice host.

---

## 2. Model-server watchdog (`voice-watchdog`)

Compose healthchecks answer `/healthz`, which a sidecar can keep serving
while its inference path is wedged (worker thread deadlocked, CUDA context
lost, model unloaded). The watchdog instead issues a **tiny real inference
request** per target on an interval and counts **consecutive** failures
(any success resets the counter to zero):

| Target | Probe |
|---|---|
| `piper` | `POST /speak {"text": "ping", "voice": $PIPER_VOICE}` |
| `mms` | `POST /tts {"text": "ping", "lang": "eng"}` |
| `xtts` / `cosyvoice` | `GET /voices` → `POST /tts {"text": "ping", "voice_id": <first enrolled>, "language": "en"}` (the cloning contract 404s on unknown `voice_id`, so the voice is discovered first; an empty registry = liveness-only probe, logged) |
| `ollama` | `POST /api/generate {"model": $LLM_MODEL, "prompt": "ping", "stream": false, "options": {"num_predict": 1}}` |

On `VOICE_WATCHDOG_FAIL_THRESHOLD` consecutive failures the watchdog logs a
structured **CRITICAL** line and — only when armed — restarts the container
through the mounted docker socket (`POST /containers/<name>/restart` over
`/var/run/docker.sock`, stdlib HTTP-over-unix; the container name is the
target name).

### Config (env)

| Var | Default | Meaning |
|---|---|---|
| `VOICE_WATCHDOG_TARGETS` | `piper=...,mms=...,xtts=...,cosyvoice=...,ollama=...` (compose names) | csv of `name=url`. Trim to the sidecars you actually run — a target that isn't deployed will probe-fail forever. `mms`/`xtts` live in `infra/compose/voices.compose.yml`, not the voice fragment. |
| `VOICE_WATCHDOG_INTERVAL_S` | `30` | seconds between probe rounds |
| `VOICE_WATCHDOG_FAIL_THRESHOLD` | `3` | consecutive failures before action |
| `VOICE_WATCHDOG_PROBE_TIMEOUT_S` | `10` | per-probe timeout (a timeout counts as a failure) |
| `VOICE_WATCHDOG_DRY_RUN` | **`1`** | `1` = log-only, NEVER restarts; `0` = armed |
| `VOICE_WATCHDOG_DOCKER_SOCK` | `/var/run/docker.sock` | docker socket path |
| `LLM_MODEL` / `PIPER_VOICE` | `qwen3:8b` / `en_US-lessac-medium` | reused in the ollama/piper probe bodies |

### Rollout (dry-run first, then arm)

```bash
# 1. Dry-run soak (default DRY_RUN=1 — log-only, cannot restart anything).
docker compose -f docker-compose.yml \
  -f services/voice-agent-runtime/docker-compose.fragment.yml \
  --profile voice --profile voice-watchdog up -d voice-watchdog
docker compose logs -f voice-watchdog
# Expect one probe round every 30s; any CRITICAL line says
# "would restart container (dry-run, no action)".

# 2. Arm restarts only after the soak is clean:
VOICE_WATCHDOG_DRY_RUN=0 docker compose ... up -d voice-watchdog
```

### Safety contract

- **DRY_RUN=1 is the default everywhere** (script, compose fragment) — the
  watchdog never restarts anything unless you explicitly arm it.
- The watchdog **never targets the voice-agent runtime itself**:
  `voice-agent-runtime` / `voice-agent-worker` entries in
  `VOICE_WATCHDOG_TARGETS` are dropped at config parse (logged), and the
  docker restart helper re-refuses them (defense in depth). A supervisor
  that can restart the thing it's supervising from inside the same failure
  domain is a footgun.
- The watchdog **never exits non-zero because a target failed** — it is a
  supervisor, not a gate. A failed docker restart is logged, not raised.
- After a breach (restart or would-restart) the counter resets, so a
  persistently wedged container re-alerts every `threshold` failures instead
  of every interval.

> **Honest note (env-blocked here):** the armed (`DRY_RUN=0`) restart path
> needs a real docker socket and running sidecars; it is covered by faked
> socket-client tests (`tests/test_voice_watchdog.py` (e)–(h)) and needs a
> one-time operator validation on a docker host before relying on it.
