# voice-agent-runtime

OpenDesk open-source voice stack (SPEC §11): LiveKit Agents worker +
faster-whisper STT + OpenAI-compatible LLM (Ollama/vLLM) + Piper TTS, plus a
FastAPI control plane and an optional ElevenLabs adapter backend.

- Control plane port: **7006** (SPEC §3). Dapr sidecar at `daprd-voice:3500`.
- Two processes share the image: the control plane (`python -m app.main`)
  and the LiveKit Agents worker (`python -m app.livekit_worker start`).

## Pipeline

silero VAD → faster-whisper STT (in-process, lazy load) → LLM via
OpenAI-compatible endpoint → Piper TTS (HTTP sidecar or subprocess).
Each stage sits behind a small interface (`app/pipeline/{stt,llm,tts}.py`)
so components are swappable; the LiveKit-specific bridges live in
`app/livekit_worker.py` (the single place pinned to livekit-agents 0.10.x
internals).

## Tools & safety

The agent exposes exactly six tools: `get_business_info`, `get_availability`,
`book_appointment`, `lookup_appointment`, `reschedule_appointment`,
`cancel_appointment`.

- Read-only tools call booking-service public endpoints via Dapr service
  invocation (`GET /v1.0/invoke/booking/method/public/sites/{slug}/context|availability`,
  `GET .../v1/bookings` with `X-Tenant-Slug`).
- Mutating tools publish CloudEvents commands to Kafka topic
  `opendesk.booking.commands` via Dapr pubsub `pubsub-kafka`
  (types `com.opendesk.booking.command.{BookAppointment,RescheduleAppointment,CancelAppointment}`,
  `subject` = tenant slug, `tenantid` ext = tenant UUID, the CloudEvent id is
  reused as `data.idempotency_key`).
- **Phone-confirmation policy**: book/lookup/reschedule/cancel refuse without
  a confirmed phone in session state. Server-enforced two-step: the first
  call with a new number returns `confirmation_required`; the model reads the
  number back, the caller confirms, and the repeated call with the same
  number proceeds. booking-service re-enforces the policy server-side
  (`ErrPhoneRequired`).
- Conversation lifecycle events (`SessionStarted`, `SessionEnded`,
  `ToolInvoked`) are published to `opendesk.conversation.events` via Dapr.
- Tenant context (terminology/timezone/currency/locale + catalog + knowledge
  snippets from `knowledge` app-id `GET /v1/context?tenant=&q=`) is fetched
  at session bootstrap and injected into the system prompt
  (dynamic-variables approach, SPEC §11). Tenant resolution is always
  server-side from the site slug — never from the model.

## Control plane

| Method | Path | Body | Description |
|---|---|---|---|
| GET | `/healthz` | — | liveness |
| POST | `/voice/session` | `{site_slug, participant_name?}` | LiveKit access token (room `site-{slug}`) or ElevenLabs signed URL |
| POST | `/voice/chat` | `{site_slug, message, conversation_id?}` | text-in/text-out through the same tool layer |
| POST | `/voice/elevenlabs/tools` | ElevenLabs tool webhook payload | only when `AGENT_BACKEND=elevenlabs` |

## Backends

`AGENT_BACKEND=livekit` (default) runs the fully open-source stack.
`AGENT_BACKEND=elevenlabs` (`app/elevenlabs_adapter.py`) outsources the voice
orchestration to ElevenLabs ConvAI: `/voice/session` returns a signed URL and
hosted tool calls are passed through `/voice/elevenlabs/tools` into the same
ToolLayer, so the phone policy and Dapr command flow are unchanged.

## Env vars

| Var | Default | Description |
|---|---|---|
| `PORT` | `7006` | control plane port |
| `LOG_LEVEL` | `info` | structlog level (JSON logs) |
| `DAPR_HOST` / `DAPR_HTTP_PORT` | `daprd-voice` / `3500` | Dapr sidecar |
| `DAPR_PUBSUB_NAME` | `pubsub-kafka` | pubsub component |
| `BOOKING_APP_ID` / `IDENTITY_APP_ID` / `KNOWLEDGE_APP_ID` | `booking` / `identity` / `knowledge` | Dapr app-ids |
| `BOOKING_COMMANDS_TOPIC` | `opendesk.booking.commands` | booking commands topic |
| `CONVERSATION_EVENTS_TOPIC` | `opendesk.conversation.events` | lifecycle/tool events topic |
| `LIVEKIT_URL` / `LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET` | `ws://livekit:7880` / `devkey` / `secret` | LiveKit server (dev keys, SPEC §11) |
| `LLM_BASE_URL` | `http://ollama:11434/v1` | OpenAI-compatible endpoint (Ollama; vLLM pluggable) |
| `LLM_MODEL` | `qwen3:8b` | model name (open-weights default, SPEC-W3 §0) |
| `LLM_API_KEY` | `ollama` | optional pass-through to the OpenAI-compatible client; ignored by Ollama, required by hosted providers (e.g. MiniMax) |
| `WHISPER_MODEL` | `base` | faster-whisper model size |
| `WHISPER_DEVICE` / `WHISPER_COMPUTE_TYPE` | `auto` / `int8` | ctranslate2 device/precision |
| `PIPER_MODE` | `http` | `http` (sidecar) or `subprocess` (local binary) |
| `PIPER_HTTP_URL` | `http://piper:5500` | piper sidecar URL (`POST /speak` → wav) |
| `PIPER_VOICE` | `en_US-lessac-medium` | voice model name |
| `PIPER_BIN` / `PIPER_MODEL_DIR` | `piper` / `/voices` | subprocess mode: binary + model dir |
| `PIPER_SAMPLE_RATE` | `22050` | expected PCM rate |
| `AGENT_BACKEND` | `livekit` | `livekit` or `elevenlabs` |
| `ELEVENLABS_API_KEY` / `ELEVENLABS_AGENT_ID` | _(unset)_ | elevenlabs backend |
| `KNOWLEDGE_SNIPPET_COUNT` / `KNOWLEDGE_QUERY` | `3` / `opening hours services pricing` | bootstrap grounding |
| `PHONE_CONFIRMATION_REQUIRED` | `true` | phone-confirmation policy toggle |
| `COPILOT_MODE` | `true` | whisper-copilot: post suggested replies to the escalation room data channel after `request_human` |
| `PLUGIN_ALLOWED_HOSTS` | `booking,knowledge,identity` | SSRF allowlist for pack `customTools` |
| `VOICEPRINTS` | `off` | consent gate for the voice-biometrics scaffold |
| `VOICEPRINT_THRESHOLD` | `0.75` | cosine-similarity verify threshold |
| `TENANT_PHONE_MAP` | `{}` | SIP inbound (Wave 5 #1): JSON `{"+1555…":"tenant-slug"}` dialed-number→tenant map (dev-mode; production = `phone_numbers` table, see docs/telephony.md) |
| `SIP_DEFAULT_SITE` | _(unset)_ | fallback site slug for unmapped dialed numbers (empty = reject) |
| `AGENTS_REGISTRY_URL` | `http://conversation:7007` | SPEC-W38: conversation-service agents registry base URL (internal); empty disables registry resolution |
| `AGENTS_CACHE_TTL_S` | `30` | SPEC-W38: in-process cache TTL for agent resolve/get lookups (hits and misses) |
| `PIPER_VOICE_MAP` | `{}` | multilingual (Wave 5 #3): JSON `{"es":"es_ES-sharvard-medium"}` language→piper voice; unmapped languages fall back to `PIPER_VOICE` |
| `EVAL_PERSONA_OVERRIDE` | `false` | A/B eval (Wave 5 #8): allow `persona_override` on POST /voice/chat — eval only, prompt-injection surface otherwise |
| `HF_HOME` | `/models` (image) | whisper model cache dir |

## Local models

- **Ollama**: `docker compose --profile voice up ollama ollama-init` pulls
  `qwen3:8b` (override with `LLM_MODEL`).
- **Whisper**: faster-whisper downloads `WHISPER_MODEL` from HuggingFace on
  first transcription into `HF_HOME` (mounted volume `whisper-models`); no
  build-time download.
- **Piper voices**: the `piper-init` compose service runs
  `python -m piper.download_voices $PIPER_VOICE --download-dir /voices`
  (idempotent). Models are `{voice}.onnx` + `{voice}.onnx.json` in the
  `piper-voices` volume. For `PIPER_MODE=subprocess`, download the same files
  into `PIPER_MODEL_DIR` on the host.

## Run

```bash
# control plane (dev)
pip install -e .
python -m app.main

# LiveKit worker (dev; needs a reachable LiveKit server)
python -m app.livekit_worker dev

# full voice profile via compose fragment
docker compose -f infra/docker-compose.core.yml \
  -f services/voice-agent-runtime/docker-compose.fragment.yml \
  --profile voice up --build

# smoke: text chat through the tool layer (no audio)
curl -s localhost:7006/voice/chat -H 'content-type: application/json' \
  -d '{"site_slug":"demo","message":"What services do you offer?"}'
```

## Concurrency & scaling (VOICE-SCALING)

Per-layer concurrency disciplines from `docs/VOICE-SCALING.md`:

- **Worker prewarming (P0)**: `PRELOAD_MODELS=true` (default) makes the
  worker's `prewarm_fnc` eagerly load the whisper model and run one piper
  warmup synthesis in every warm job process — no first-call dead air.
  `AGENT_IDLE_PROCESSES=2` keeps that many job processes warm
  (`num_idle_processes`). Prewarm failures degrade to lazy loading.
- **Load gating (P0)**: the worker advertises an explicit CPU-based
  `load_fnc` (psutil) and stops accepting jobs above `LOAD_THRESHOLD=0.7`.
- **Async tools with filler (P0)**: slow Dapr tools (`get_availability`,
  `book_appointment`, `reschedule_appointment`, `cancel_appointment`,
  `lookup_appointment`, `knowledge_search`) speak a per-industry ack line
  (tenant `terminology["tool_ack"]` override → industry default →
  "Let me check that for you…") when the call outlasts a 400 ms grace
  window (`TOOL_ACK_GRACE_MS`); the SSE chat path emits an immediate
  `{"ack": "..."}` event instead. Every tool call is bounded by
  `TOOL_TIMEOUT_SECONDS=4` — on timeout the agent speaks an apology
  ("I'm having trouble reaching our booking system…") instead of dead air;
  timeouts never raise into the pipeline.
- **Inference metrics (P1)**: hand-rolled Prometheus exposition at
  `GET /metrics`: `voice_stt_latency_seconds`, `voice_llm_latency_seconds`,
  `voice_llm_tokens_total{kind=prompt|completion}`,
  `voice_tts_latency_seconds`, `voice_tool_calls_total{tool,result}`,
  `voice_active_sessions`.
- **Worker metrics (W44/F15-09)**: the LiveKit worker process
  (`python -m app.livekit_worker start`) serves the SAME hand-rolled registry
  on `VOICE_WORKER_METRICS_PORT` (default **9464**, `0` disables) so
  Prometheus can scrape the per-call series recorded in job processes — the
  control-plane `/metrics` on 7006 is a different process and never sees
  them. The worker supervisor and every job process bind the port with
  `SO_REUSEPORT` where available, so a scrape returns one process' view of
  the shared registry; add a Prometheus scrape job targeting this port
  (see infra/prometheus — job `voice-worker`).
- **LLM fallback chain (P1)**: when `LLM_FALLBACK_BASE_URL` (+
  `LLM_FALLBACK_MODEL`/`LLM_FALLBACK_API_KEY`) is set, a primary failure —
  connection error, 429, 5xx, or a call exceeding `LLM_TIMEOUT=20`s — retries
  that call against the fallback endpoint. A circuit breaker
  (`LLM_CB_FAILURES=3`, `LLM_CB_COOLDOWN_S=60`) routes around a flapping
  primary, then probes it. Covers the chat/tool-loop paths (buffered + SSE);
  the LiveKit worker's `livekit-plugins-openai` LLM node cannot hot-swap
  endpoints mid-process, so the worker path relies on the primary endpoint
  (see the note in `app/livekit_worker.py`).

## Notes / simplifications

- Session state + chat history are in-memory (dev-grade); swap
  `SessionStore` for the Dapr Redis state store in production.
- Whisper resampling to 16 kHz is linear (dev-grade); swap in a polyphase
  resampler if audio quality matters.
- The LiveKit bridges (`app/livekit_worker.py`) use livekit-agents 0.10.x
  extension points (`stt.STT._recognize_impl`, `tts.TTS.synthesize`,
  `VoicePipelineAgent`); if a different 0.10.x patch release reshapes those
  internals, that file is the single adjustment point.

## Model routing (SPEC-W3 §0)

All LLM access goes through the OpenAI-compatible env triple
`LLM_BASE_URL` / `LLM_MODEL` / `LLM_API_KEY` — swapping models is pure
configuration, no code changes. Open weights are the default.

| Profile | LLM_BASE_URL | LLM_MODEL | LLM_API_KEY | Use |
|---|---|---|---|---|
| **default** | `http://ollama:11434/v1` | `qwen3:8b` | `ollama` (ignored) | local Ollama, pulled by `ollama-init`; good quality/cost balance |
| quality | `http://ollama:11434/v1` | `qwen3:32b` | `ollama` | higher-quality local model; pull manually (`ollama pull qwen3:32b`), needs ~20GB VRAM/RAM |
| long-context | `https://api.minimax.io/v1` | `MiniMax-M2` | your MiniMax key | hosted long-context path; `LLM_API_KEY` is passed through to the client. A local MiniMax-M2 via vLLM/Ollama works the same way by pointing `LLM_BASE_URL` at it |

The same env family configures the conversation-service call-intelligence
LLM (`INTEL_LLM_*`) and the eval harness judge, so one Ollama serves
everything out of the box.

## Warm handoff & whisper-copilot (innovation 1)

Tool `request_human(reason?)`: creates LiveKit room
`escalation-{conversation_id}` via livekit-api, mints a staff join token and
publishes `com.opendesk.conversation.EscalationRequested`
(`{conversation_id, tenant_id, site_slug, room, join_token_staff, reason}`)
to `opendesk.conversation.events` via Dapr — the dashboard listens for the
banner. The caller hears a spoken confirmation. When LiveKit is unreachable
the event still goes out and the caller flow is unaffected. Afterwards,
whisper-copilot mode (`COPILOT_MODE=true`) keeps the agent engaged: every
reply is also posted as a `copilot_suggestion` to the escalation room's
`copilot` data channel for the operator (best-effort).

## Multi-agent crews (innovation 6)

Packs may declare `agents: [{id, name, persona, intents}]` (see
`industries/salon.yaml`, `industries/clinic.yaml`; validated by
identity-service's pack loader and passed through in the tenant pack
summary). Each chat turn is scored against the agents' intent keywords
(deterministic, embedding-free); the best match becomes
`session.active_agent` and its persona is swapped into the system prompt
(re-rendered per turn) with fallback to the base persona when nothing
matches.

## Plugin tools (innovation 15 MVP)

Pack `customTools` become real function tools executing declarative HTTP
calls with `{{var}}` template substitution, guarded by the
`PLUGIN_ALLOWED_HOSTS` SSRF allowlist. See `docs/plugins.md` (MVP semantics +
WASM-sandbox phase-2 design) and the example in
`industries/consultancy.yaml`.

## Voice biometrics (innovation 2) — SCAFFOLD

`app/voiceprint.py` defines the enrollment/verification API:
`VoiceprintStore` protocol (+ in-memory dev impl), an import-guarded
Resemblyzer encoder (optional `resemblyzer` package), cosine-similarity
verification against `VOICEPRINT_THRESHOLD`, and `enroll_voiceprint` /
`verify_voiceprint` functions gated by the `VOICEPRINTS` consent env
(default **off**; enrollment additionally requires explicit caller consent).
**Not wired into the audio pipeline yet** — no audio is ever captured or
embedded by the running agent. Pipeline integration (post-STT utterance
sampling + a persistent store) is the next step.

## Eval harness (innovation 5)

`eval/` replays `eval/scenarios/*.yaml` against `/voice/chat`, asserts
expected tool calls and scores turns with an LLM judge; see `eval/README.md`
and `make eval`. `eval/ab_test.py` (Wave 5 #8) A/B-tests persona variants
(`make ab-test`, requires `EVAL_PERSONA_OVERRIDE=true`).

## SIP telephony inbound (Wave 5 #1)

Inbound PSTN calls land via LiveKit SIP: an inbound trunk claims the
provisioned numbers and a callee dispatch rule creates one room per dialed
number (`call-{number}`). `app/sip.py` detects `call-*` rooms / SIP
participants, resolves the tenant from the dialed number
(`TENANT_PHONE_MAP`, dev-mode static JSON; production = `phone_numbers`
table), and attaches the carrier-asserted caller ID as the session's
confirmed phone — the two-step read-back confirmation is bypassed for SIP
calls only (policy documented in `app/sip.py`). Provisioning:
`deploy/livekit-sip/setup.sh` + docs/telephony.md.

## Agents registry & agent definitions (SPEC-W38)

Voice agents are first-class tenant-scoped entities owned by
conversation-service (agent-as-product). On an inbound SIP call the dialed
number is resolved **registry-first**:

1. `app/agents_registry.py` calls
   `GET {AGENTS_REGISTRY_URL}/v1/agents/resolve?phone=<E.164>` (internal,
   not exposed via APISIX; 2s timeout) and caches hits **and** misses for
   `AGENTS_CACHE_TTL_S` (default 30s). Any 404/network/timeout/malformed
   payload **fails open** to step 2 — dev mode keeps working when the
   registry is down.
2. Legacy `TENANT_PHONE_MAP` → `SIP_DEFAULT_SITE` (unchanged).

Resolution outcomes are counted as
`voice_agent_resolution_total{source=registry|env_map|default}`.

A resolved agent carries a declarative **definition** JSONB
(`app/agent_definition.py`, all keys optional):
`persona`, `voice{provider,voice_id,language}`, `instructions`,
`context_budget_tokens`, `tool_allowlist`, `knowledge_packs`,
`ops_rules{max_call_seconds,escalation_phone}`. The worker merges it over
the bootstrapped tenant context (merge order: **env defaults < industry
pack < agent definition**) before the prompt is built:

- `persona` replaces the pack `agentPersona` in the system prompt;
- `instructions` is appended as its own `AGENT INSTRUCTIONS` block;
- `context_budget_tokens` truncates knowledge snippets (~4 chars/token);
- `tool_allowlist` (non-empty) filters `ToolLayer.schemas()` and blocks
  dispatch of non-allowlisted tools;
- `voice.voice_id` (piper) overrides the session's default TTS voice.

## Multilingual receptionist (Wave 5 #3)

Whisper auto-detects the caller's language per utterance;
`app/multilang.py` switches the turn language (tenant identity locale is the
default, pack `languages: [en, es]` bounds the switch set), injects a
per-turn "respond in {language}" instruction into the system prompt, and
swaps the piper voice via `PIPER_VOICE_MAP` with graceful fallback to
`PIPER_VOICE`. Pack `languages` is validated at the voice runtime's pack
consumption point (`tenant_context._apply_pack`) because identity passes
pack fields through unchecked.
