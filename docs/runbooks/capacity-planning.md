# Capacity Planning Runbook — Voice Stack

Companion to `docs/VOICE-SCALING.md` ("Scaling Voice Agents: Concurrency at
Every Layer" — Mahimai Raja J). This is the standing ops artifact for voice
capacity: **every capacity discussion names a layer.** A voice agent is not
one system with one capacity number — it is a stack of independent layers,
each with its own concurrency ceiling. **Real capacity = the lowest ceiling.
Scaling the wrong layer is wasted money.**

The unit of load is a **concurrent call**, not a request. One call
simultaneously holds: 1 SFU room + 1 worker job process + 1 STT stream +
LLM rate share + 1 TTS stream + (optionally) 1 SIP channel + per-turn
downstream calls (booking availability, knowledge RAG, identity context).

Live view: Grafana → **Agora — Concurrency Ceilings**
(`infra/observability/dashboards/concurrency-ceilings.json`).

---

## 1. Per-layer worksheet (pre-filled with Agora components)

Fill the **Ceiling now** column from load tests (`tests/load/voice_ramp.py`)
and the Concurrency Ceilings dashboard. The demand column is per *N
concurrent calls*.

> **W41 measurement note:** no placeholder cells remain, and none were filled
> with invented numbers. Voice-layer ceilings are **not yet measured**
> (harness: `tests/load/voice_ramp.py`, §5 — no W41 execution); the
> funds/booking API paths now have a measured baseline via the W41 perf
> harness — see §1a and `tests/perf/RESULTS.md`.

| # | Layer | Agora component | Ceiling unit | Demand per concurrent call | Ceiling now | Lift lever |
|---|-------|--------------------|--------------|----------------------------|-------------|------------|
| 1 | Media plane (SFU) | LiveKit DaemonSet (`deploy/k3s/livekit-server.yaml`), 1 pod/node, hostNetwork | concurrent **rooms** per node; node CPU/bandwidth | 1 room + 1–2 participant streams | not yet measured (harness: `tests/load/voice_ramp.py`, §5 — no W41 execution) (planning start: ~50–100 calls/node on 4 dedicated cores, UDP hostPorts 50000-50100) | add `livekit=true` nodes (rooms mesh across nodes); budget old+new capacity during 5h drain rollouts |
| 2 | Worker plane | `voice-worker` Deployment + HPA (`deploy/k3s/voice-worker*.yaml`) | concurrent **job processes** per pod | 1 job (1 warm subprocess) | not yet measured (harness: `tests/load/voice_ramp.py`, §5 — no W41 execution) (planning start: 10–25 jobs per 4cpu/8Gi pod; HPA 2–20 pods) | raise `maxReplicas` + add compute-optimized nodes (**never burstable t3-class**); `AGENT_IDLE_PROCESSES` prewarming |
| 3a | Inference — STT | faster-whisper in voice-agent-runtime (`WHISPER_MODEL=base`) | concurrent **streams** per GPU/CPU box | 1 stream while caller speaks (~40% of call) | not yet measured (harness: `tests/load/voice_ramp.py`, §5 — no W41 execution) | bigger GPU / smaller model / dedicated STT tier |
| 3b | Inference — LLM | Ollama `qwen3:8b` (`LLM_BASE_URL`); MiniMax-M2 adapter as API mode | **rate** (RPM/TPM); self-hosted: `OLLAMA_NUM_PARALLEL` slots | ~2–4 completions/turn, ~8–12 turns/call | not yet measured (harness: `tests/load/voice_ramp.py`, §5 — no W41 execution) (planning start: `OLLAMA_NUM_PARALLEL` = GPU VRAM-dependent, 4–8 typical) | raise `OLLAMA_NUM_PARALLEL`, add GPU, or fallback adapter (Ollama→MiniMax on 429/timeout) |
| 3c | Inference — TTS | Piper sidecar (`PIPER_URL`); ElevenLabs adapter optional | concurrent **synth streams** | 1 stream per agent turn | not yet measured (harness: `tests/load/voice_ramp.py`, §5 — no W41 execution) (planning note: Piper is light — ~dozens of streams per core) | scale piper replicas; ElevenLabs adapter when enabled (reintroduces API rate cap) |
| 4 | Telephony | LiveKit SIP (roadmap — waitlist backfill outbound) | carrier **channel count** (hard cap) + **CPS** (start rate) | 1 SIP channel per PSTN call | not yet provisioned | procurement: more channels/trunks/numbers, regional origination; pace outbound with `OutboundPacer` (`OUTBOUND_CPS`, default 1/s) — CPS binds campaigns first |
| 5a | Application — tools | Dapr-invoked booking availability / knowledge RAG / identity context | downstream rps at p99 ≤ turn budget | 2–5 downstream calls per turn | booking paths: see `tests/perf/RESULTS.md` (W41 measured baseline); knowledge/identity downstreams not yet measured | cache (Redis availability cache ✓), async tools + filler audio, hard 4s timeout → spoken fallback |
| 5b | Application — DB | Postgres pgx pools per Go service | pool **connections** (see §3) | per-turn queries × N calls | defaults in §3 | size for peak (formula §3), not average |
| 5c | Application — search | OpenSearch (knowledge RAG) | concurrent queries at p99 latency | 0–2 searches per turn (industry-pack dependent) | not yet measured (harness: `tests/load/voice_ramp.py`, §5 — no W41 execution) | replicas/shards; cache embeddings |
| 6 | Gateway | APISIX (`apisix_http_status`, `apisix_bandwidth`) | rps + bandwidth | signalling + webhooks only (RTP bypasses gateway) | rarely binding | scale gateway replicas |

**Rule-of-thumb scaling order** (cheapest/most-likely-first):

1. **Worker pods** (HPA already does this — check `maxReplicas` headroom).
2. **Inference** — usually the *real* ceiling: Ollama slots/GPU first, then
   whisper streams, then piper.
3. **DB pools / downstream latency** — the quiet killer; check pool waits
   before adding app replicas.
4. **SFU nodes** — media plane scales cleanly per node; add before node CPU
   saturates.
5. **SIP channels/CPS** — procurement lead-time item; order *weeks* ahead of
   any outbound campaign.
6. **Gateway/OpenSearch** — almost never the binding layer; verify before
   spending.

---

## 1a. Funds/booking API baseline + headroom (W41)

The voice worksheet above is unmeasured; the **funds and booking API paths**
now have a measurement loop (SPEC-W41 W41-5): `tests/funds-e2e/harness.py`
times every public call against real service binaries + real (embedded)
Postgres, and `tests/perf/aggregate.py` computes p50/p99 + throughput
(N ≥ 50 per hot call) into `tests/perf/RESULTS.md`. Budgets are defined in
`docs/performance-budgets.md`.

| Operation | Budget (p50 / p99 / rps) | Measured baseline |
|---|---|---|
| Booking create (`POST /v1/bookings`) | see `docs/performance-budgets.md` B1 | see `tests/perf/RESULTS.md` (W41 measured baseline) |
| Public booking (`POST /public/sites/{slug}/bookings`) | B2 | see `tests/perf/RESULTS.md` (W41 measured baseline) |
| Invoice generate / issue | B3 / B4 | see `tests/perf/RESULTS.md` (W41 measured baseline) |
| Paystack webhook | B5 | see `tests/perf/RESULTS.md` (W41 measured baseline) |
| Deposit hold / capture | B6 / B7 | see `tests/perf/RESULTS.md` (W41 measured baseline) |

**Headroom formulas** (recompute per release from the fresh baseline):

```
latency_headroom    = budget_p99 / measured_p99        # >1 = inside budget
throughput_headroom = measured_rps / peak_expected_rps # >1 = inside budget
```

Planning rule: when either headroom drops **below 2×** for a path, treat that
path's layer as the next binding constraint in §1 (usually 5b DB pools —
formula in §3) and provision before the next demand step. Sandbox numbers are
indicative only; re-measure on staging per release (environment caveats:
`docs/performance-budgets.md` §3). Do **not** transcribe baseline numbers
into this table — `tests/perf/RESULTS.md` is the single source of truth and
is appended per run.

---

## 2. Worked example — 500 concurrent calls (article's scenario)

Article conclusion: ~20–50 workers suffice; the binding constraints are
**inference caps and SIP channels**, not CPU. Filled with Agora
components:

| Layer | Arithmetic for N=500 | What binds |
|---|---|---|
| SFU | 500 rooms ≈ 5–10 LiveKit nodes (50–100 rooms/node) + 1 surge node for drain budget | node CPU/bandwidth — plan old+new during rollouts |
| Workers | 500 jobs ÷ ~15 jobs/pod ≈ **34 pods** (HPA max 20 default → **raise to 40**) | pod count; 34 × 4cpu ≈ 136 cores of compute-optimized capacity |
| Whisper STT | ~40% duty cycle → ~200 concurrent streams ≈ 2–4 GPUs (or heavy CPU tier) | GPU stream count |
| Ollama LLM | 500 calls × ~2 active completions/turn window; at 8 slots/GPU ≈ **8–16 GPU slots** (`OLLAMA_NUM_PARALLEL`) | **the usual ceiling** — GPU count or fallback to MiniMax-M2 |
| Piper TTS | 500 streams ÷ ~dozens/core → 2–4 piper replicas | cheap, rarely binds |
| SIP | 500 PSTN calls = 500 channels; campaign start rate ≤ carrier CPS | **procurement** — channels and CPS are bought, not deployed |
| Postgres pools | 500 calls × 2 peak mid-call turns ≈ 1000 short queries/s across booking/identity | pool `MaxConns` + `max_connections` — see §3 |
| APISIX | signalling only; 500 WS upgrades + webhooks — trivial | not binding |

Cost for 500 concurrent calls is dominated by **inference + telephony**;
compute (workers) is the smallest line item — see §4.

---

## 3. DB connection pool sizing

> A pool sized for *average* injects **dead air** at peak — the caller hears
> silence while the query waits for a connection, and dashboards look green
> because CPU and error rate are fine.

**Formula (per service):**

```
pool_max_conns ≈ peak_concurrent_calls × peak_turns_per_call_hitting_this_service
                 × queries_per_turn ÷ target_query_concurrency_headroom
```

Simplified rule of thumb: `pool_max_conns ≈ peak_calls × peak_mid-call_turns`,
then divide across service replicas. Keep `sum(all services' MaxConns)` under
Postgres `max_connections` with ~20% headroom for migrations/jobs.

**Agora defaults (env-documented):**

| Service | Env | Default | Rationale |
|---|---|---|---|
| booking-service | `PG_MAX_CONNS` | **20** | ✅ implemented (`store.New(ctx, dsn, maxConns)` — pgxpool `ParseConfig`+`NewWithConfig`). Hottest path: availability lookup + booking write per turn. |
| identity-service | `PG_MAX_CONNS` | 10 (recommended) | ⚠️ pool config **not exposed** — `store.New` calls `pgxpool.New` directly (pgx v5 default `max(4, NumCPU)`). Tenant context is read once per call; default 4 is acceptable at small scale, revisit with the same pattern as booking-service when identity joins the mid-call path. |
| crm-sync-service | `PG_MAX_CONNS` | 10 (recommended) | ⚠️ not exposed (same `pgxpool.New` pattern). Off-call-path (event sync); pgx default is fine until sync batch sizes grow. |
| notification-worker | `PG_MAX_CONNS` | 10 (recommended) | ⚠️ pool config **not exposed** — `cmd/worker/main.go` calls `store.New` → `pgxpool.New` directly (pgx v5 default `max(4, NumCPU)`); the civic-ledger/DND activities query Postgres via pgx (`internal/store`). Same pattern as identity-service: expose `PG_MAX_CONNS` with the booking-service pattern. |

Monitoring: Grafana → Concurrency Ceilings → "Postgres connections per
database" (`pg_stat_activity_count` vs `pg_settings_max_connections`; needs
postgres_exporter scraped — empty panel = monitoring gap, not headroom).

---

## 4. Cost-per-call-hour calculator

Article model: **cost per concurrent-call-hour ≈ $1.15–$3.55 rented**;
compute is the smallest line item; inference + telephony dominate.
Self-hosting models converts per-minute cost into fixed GPU cost — the
Agora open-model-first design. The calculator is parametric: plug in your
own numbers in the *you* column.

### 4a. Inputs (edit for your deployment)

| Parameter | Symbol | Default | You |
|---|---|---|---|
| Concurrent calls | N | 100 | |
| Call duty cycle (agent speaking/listening) | — | 100% while active | |
| LLM tokens per call-hour (in+out, ~10 turns × 1k tok) | T_llm | ~600k tok/call-h | |
| STT audio minutes per call-hour | M_stt | ~25 min (caller share) | |
| TTS characters per call-hour | C_tts | ~15k chars | |
| GPU hourly cost (L4/A10G-class, on-demand) | G | $0.80/h | |
| Calls one GPU serves (whisper+piper CPU-offload, ollama 8 slots) | n_gpu | ~25 | |
| SIP trunk per channel-hour | S | $0.01–0.02 + per-min carrier rate | |

### 4b. Self-hosted (Agora default: qwen3 via Ollama + faster-whisper + Piper)

| Line item | Formula | @ N=100, defaults |
|---|---|---|
| GPU inference (STT+LLM+TTS) | (N ÷ n_gpu) × G | (100÷25)×$0.80 = **$3.20/h → $0.032/call-h** |
| Worker compute | (N ÷ 15 jobs/pod) × 4cpu pod ≈ core-hour price ~$0.05 | ~$1.35/h → $0.013/call-h |
| SFU nodes | (N ÷ 75 rooms/node) × ~$0.20/h | ~$0.27/h → $0.003/call-h |
| Telephony (if PSTN) | N × S | $1–2/h → $0.01–0.02/call-h |
| **Total (excl. telephony)** | | **≈ $0.05/call-hour** |

Fixed-cost caveat: self-hosted cost is *amortized*, not per-minute — at low
utilization (N ≪ n_gpu) the per-call-hour cost rises (GPU idles); at steady
volume it crushes API rates. Break-even vs API stack below ≈ 15–20%
utilization of one GPU; above that self-host wins.

### 4c. API-mode comparison (MiniMax / ElevenLabs adapters enabled)

| Line item | API rate (list, check current pricing) | @ N=100 |
|---|---|---|
| LLM — MiniMax-M2 | ~$0.30/1M in + ~$1.20/1M out (blended ≈ $0.60/1M) | 100 × 600k × $0.60/1M ≈ **$36/h → $0.36/call-h** |
| STT — hosted whisper-class | ~$0.006/min | 100 × 25 × 0.006 ≈ **$15/h → $0.15/call-h** |
| TTS — ElevenLabs | ~$0.18/1k chars (Creator tier) | 100 × 15k × $0.18/1k ≈ **$270/h → $2.70/call-h** |
| Telephony | same as self-hosted | $0.01–0.02/call-h |
| **Total (excl. telephony)** | | **≈ $3.2/call-hour** — dominated by TTS |

### 4d. Reading the table

* **~60× gap** between self-hosted (~$0.05) and full-API (~$3.2) per
  call-hour at steady volume — the article's "self-host wins at steady
  volume" made concrete with the Agora stack.
* API mode still makes sense as a **fallback adapter** (Ollama→MiniMax on
  429/timeout; Piper→ElevenLabs when enabled): you pay API rates only for
  overflow/degraded minutes — degraded latency, not dead calls.
* Telephony is a procurement line item in both worlds; for outbound
  campaigns CPS pacing (`OUTBOUND_CPS`) is the knob that protects both cost
  and spam reputation.
* Never scale the worker plane to fix an inference ceiling — check the
  Concurrency Ceilings dashboard for *which* layer saturates first, then
  spend on that layer only.

---

## 5. Load-test feedback loop

`tests/load/voice_ramp.py` (simulated callers, ramp 5→10→25→50 with holds)
prints per-layer saturation and the first-degrading layer. The test is done
when you know **which layer fails next and at what number** — write that
number into the worksheet (§1, "Ceiling now") and re-run after every change
to any ceiling lever above.
