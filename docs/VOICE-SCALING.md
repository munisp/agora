# Voice Scaling: Article Insights → Agora Implementation Plan

> **STATUS: IMPLEMENTED** ✅ (all P0 + P1 items)
> - P0 prewarming + load gating: `services/voice-agent-runtime/app/livekit_worker.py` (`PRELOAD_MODELS`, `AGENT_IDLE_PROCESSES`, `LOAD_THRESHOLD`, psutil `load_fnc`)
> - P0 async tools + filler + 4s timeout: `app/async_tools.py` (+ SSE ack frames in chat path)
> - P1 inference metrics: `app/metrics.py` → `/metrics` (voice_stt/llm/tts_latency, tokens, tool calls, active sessions)
> - P1 LLM fallback chain + circuit breaker: `app/pipeline/llm.py` (`LLM_FALLBACK_*`, Ollama→MiniMax)
> - P1 LiveKit prod DaemonSet (5h drain) + worker HPA (0.5 up / lazy down): `deploy/k3s/livekit-server.yaml`, `voice-worker*.yaml`
> - P1 Concurrency Ceilings dashboard: `infra/observability/dashboards/concurrency-ceilings.json`
> - P1 load-test harness: `tests/load/voice_ramp.py` (`make loadtest-voice`)
> - P1 capacity planning + cost calculator: `docs/runbooks/capacity-planning.md`
> - P1 outbound CPS pacer + sender rotation (Redis token bucket): `services/notification-worker/internal/pacer/`
> - P1 DB pool sizing: `PG_MAX_CONNS` in booking-service + runbook formulas
> - Telephony SIP trunk config remains the only deferred item (arrives with the SIP feature work — **shipped in Wave 5**, see §4 and [telephony.md](telephony.md)).


Source: "Scaling Voice Agents: Concurrency at Every Layer" — Mahimai Raja J (Medium, Voice AI Mastery).
Core thesis: **a voice agent is not one system with one capacity number — it is a stack of independent layers, each with its own concurrency ceiling. Real capacity = the lowest ceiling. Scaling the wrong layer is wasted money.**

Agora already runs the exact stack the article targets (self-hosted LiveKit + agents + self-hosted models), so every layer maps to real components in this repo.

---

## Layer-by-layer mapping

### 0. The unit of load is a *concurrent call*, not a request
One call simultaneously holds: 1 SFU room + 1 worker job process + 1 STT stream + LLM rate share + 1 TTS stream + (optionally) 1 SIP channel + per-turn downstream calls (booking availability, knowledge RAG, identity context).
**Agora action**: adopt per-layer capacity planning (table in §7) as a standing ops artifact; every capacity discussion names a layer.

### 1. Media plane (SFU) — `livekit` service in root compose
Article: SFU is CPU/bandwidth-bound (not RAM), one pod per node (host networking, UDP range), rooms mesh across nodes, **drain grace period ~5h** on rollouts, TURN for restrictive NATs, multi-region for latency.
Agora gap: compose runs LiveKit with `--dev` on one node — fine for dev, undocumented for prod.
**Implement**:
- `deploy/k3s/livekit-server.yaml`: hostNetwork Deployment (1/node, DaemonSet), `terminationGracePeriodSeconds: 18000`, UDP nodePort range, TURN env notes. (P1)
- ADR-0008 addition: SFU rollout budget = old+new capacity during drain. (P2)

### 2. Worker plane — `voice-worker` service
Article: per-job subprocess isolation (LiveKit Agents gives this free), **`load_fnc`/`load_threshold` (0.7)** gating, 4c/8GB ≈ 10–25 jobs, **CPU-bound not RAM**, compute-optimized instances (never burstable), **`num_idle_processes` prewarming** to kill cold-start dead air, 10min+ termination grace, **autoscaler must scale UP at 0.5 (below the 0.7 worker threshold), down lazily**.
Agora gaps (real): whisper model is lazy-loaded at call time (cold-start dead air on first call), no load gating config, no prewarming, no HPA.
**Implement**:
- voice-agent-runtime `livekit_worker.py`: set `num_idle_processes` (env `AGENT_IDLE_PROCESSES=2`), explicit `load_fnc`/`load_threshold` (env-tunable, defaults CPU/0.7), model preload at worker boot instead of first call. (P0 — biggest cheap UX win)
- `deploy/k3s/voice-worker-hpa.yaml`: scale-up at avg CPU 50%, scale-down stabilization ≥15min, `terminationGracePeriodSeconds: 900`. (P1)
- Runbook: "never burstable instances" note. (P2)

### 3. Inference plane — whisper / piper / ollama (or MiniMax/ElevenLabs adapters)
Article: **usually the real ceiling.** STT/TTS cap on *concurrent streams*, LLM caps on *rate (RPM/TPM)*. Lift by: raise limits, **fallback adapters to a secondary provider**, or self-host (removes external caps, replaces with GPU capacity planning). Self-host wins at steady volume.
Agora position: already self-host-first (article's recommended end-state) — our ceiling is GPU/CPU, not a provider account. But API modes (MiniMax-M2, ElevenLabs adapter) reintroduce rate caps, and we have **no fallback chain**.
**Implement**:
- Fallback adapters in the pipeline interfaces: STT whisper→(none local, document), **LLM: Ollama primary → MiniMax-M2 secondary on 429/timeout** (env `LLM_FALLBACK_BASE_URL/MODEL`), TTS piper→ElevenLabs adapter when enabled. Retry-with-fallback = degraded latency, not dead calls. (P1)
- **Per-call inference instrumentation** (VoiceGateway-style, open-source): wrap STT/LLM/TTS calls with timers + token counters → Prometheus `voice_stt_latency_seconds`, `voice_llm_latency_seconds`, `voice_llm_tokens_total`, `voice_tts_latency_seconds` on voice-agent-runtime `/metrics`; feed the existing ai-voice Grafana dashboard. This replaces the article's vendor tool with our own. (P1)
- ADR: GPU sizing note (concurrent whisper streams per GPU, ollama parallel slots `OLLAMA_NUM_PARALLEL`). (P2)

### 4. Telephony plane — LiveKit SIP (inbound shipped, Wave 5 #1)

Inbound SIP landed in Wave 5: `deploy/livekit-sip/` (trunk + dispatch rule +
setup script), `app/sip.py` in the voice runtime, and
[telephony.md](telephony.md) (carrier checklist, provisioning, number→tenant
mapping). The sizing guidance below still applies.
Article: two ceilings set by the carrier, not you: **channel count** (hard cap) and **CPS** (start rate). CPS binds *outbound campaigns* first; pacing is one knob for both CPS and spam-reputation; scale via procurement, multiple trunks/numbers, regional origination.
Agora relevance: innovation #7 (waitlist backfill outbound) and LiveKit SIP are on the roadmap — this article is the sizing discipline for exactly that feature.
**Implement**:
- When SIP lands (`deploy/` SIP dispatch rules + trunk config): `OutboundPacer` in the WaitlistBackfill workflow — token-bucket `OUTBOUND_CPS` (default 1/s) + per-number rotation list, **before** dialing. (P1 with SIP)
- Docs: telephony capacity = procurement lead-time item; channel/CPS checklist in runbook. (P2)

### 5. Application plane — Dapr-invoked tools (booking/knowledge/identity), Postgres pools, OpenSearch
Article: the quiet killer. Per-turn downstream deps × N calls; a connection pool sized for average injects **dead air** at peak and dashboards look green. Size pools for peak; **explicit timeouts + fallbacks on every mid-call external call** (a slow dependency is worse than a failed one); cache; load-test downstreams under the same ramp; **async tools with filler audio** (`ctx.update("let me check that")`, `ctx.with_filler(...)`) — changes the experience of slow deps, not their capacity.
Agora status: Redis availability cache already shipped ✓ (article's "cache what you can"). Gaps: tool calls are synchronous (caller hears silence during availability/booking lookups), pool sizing undocumented, no downstream load test.
**Implement**:
- **Async tools + filler** in the voice tool layer: long tools (book/reschedule, knowledge search) immediately speak an ack line (pre-cached TTS clip per industry pack persona) while the Dapr invoke completes; hard timeout 4s → spoken apology fallback instead of dead air. (P0 for call quality)
- Pool sizing: pgxpool `MaxConns` env per Go service sized for peak concurrent calls (formula in runbook: peak_calls × turns_per_call_peak); asyncpg pool likewise. (P1)
- Extend `tests/e2e` with a downstream-ramp scenario (availability endpoint under 100 rps while calls run). (P2)

### 6. Capacity & cost arithmetic — adopt both models
Article's 500-call plan: ~20–50 workers, but the binding constraints are inference caps + SIP channels, not CPU. Cost model: **cost per concurrent-call-hour ≈ $1.15–$3.55 rented; compute is the smallest line item; inference + telephony dominate; self-hosted models convert per-minute cost into fixed GPU cost** (validation of Agora's open-model-first design).
**Implement**:
- `docs/runbooks/capacity-planning.md`: the per-layer worksheet (per-call demand × N) pre-filled with Agora component names + a cost-per-call-hour calculator using self-hosted rates (GPU $/hr amortized vs API rates). (P1)
- Grafana: per-layer saturation panels on one "Concurrency Ceilings" dashboard (worker load_fnc, ollama queue depth, whisper active streams, DB pool usage, APISIX rates) — the article's "watch the bottleneck move" made visible. (P1)

### 7. Load testing — know which layer fails *next*
Article: simulated callers publishing looped speech, ramp in steps, hold, watch 4 signal families (worker, inference, telephony, application); "the test is done when you know which layer fails next and at what number."
**Implement**:
- `tests/load/voice_ramp.py`: LiveKit API client spawning N simulated participants per room publishing looping audio, ramp 5→10→25→50 with holds, reading Prometheus metrics; report prints per-layer saturation + first-degrading layer. CI-adjacent (`make loadtest-voice`). (P1)

---

## Priority summary

| Priority | Item | Article layer | Effort |
|---|---|---|---|
| P0 | Model preload + `num_idle_processes` prewarming | Worker | S |
| P0 | Async tools with filler/ack + 4s timeout fallback | Application | M |
| P1 | Per-call STT/LLM/TTS metrics + Concurrency Ceilings dashboard | Inference/Ops | M |
| P1 | LLM fallback chain (Ollama→MiniMax) | Inference | S |
| P1 | HPA 0.5-up/lazy-down + 15min worker drain | Worker | S |
| P1 | LiveKit prod DaemonSet + 5h grace + TURN notes | Media | M |
| P1 | `tests/load/voice_ramp.py` | Testing | M |
| P1 | Capacity-planning runbook + cost calculator | Ops | S |
| P1* | Outbound CPS pacer (with SIP work) | Telephony | M |
| P2 | DB pool sizing formulas + downstream ramp test | Application | S |

*with SIP feature work.

**Meta-lesson for Agora**: our open-model-first stack already embodies the article's end-state recommendation (self-hosted inference = owned ceilings, fixed GPU cost). The gaps are not architectural — they're the *operational disciplines*: prewarming, async tools, per-layer metrics, load testing, and capacity arithmetic. Those are what to implement.
