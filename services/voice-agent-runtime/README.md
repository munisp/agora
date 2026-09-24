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
- **Phone-confirmation policy**: book_appointment refuses without
  a confirmed phone in session state. Server-enforced two-step: the first
  call with a new number returns `confirmation_required`; the model reads the
  number back, the caller confirms, and the repeated call with the same
  number proceeds. booking-service re-enforces the policy server-side
  (`ErrPhoneRequired`).
- **Verified-session policy (SPEC-W45 K15(c))**: lookup/reschedule/cancel
  additionally require a VERIFIED phone — read-back confirmation is not
  enough. Verification is OTP via the booking customer portal
  (`request_verification_code` → booking `POST /public/sites/{slug}/portal/request`
  sends an SMS/email code → `verify_caller_code` → `.../portal/verify`), or a
  channel-verified identity pinned by the messaging-gateway (WhatsApp wa_id,
  internal-token-authorized). Fail-closed (`verification_unavailable`) when
  `BOOKING_URL`/`VOICE_BOOKING_INTERNAL_TOKEN` are unset. The SIP
  carrier-asserted pre-confirmation bypass is REMOVED (OOS-03): the caller ID
  is a claimed/unverified hint only.
- **Session resume (SPEC-W45 K15(b))**: each session is issued a
  `session_secret` (uuid4) at creation, returned in every `/voice/chat`
  response. Resuming a conversation requires `conversation_id` +
  `session_secret`; a `conversation_id` alone always starts a FRESH session
  (no confirmed/verified phone, history or escalation state leaks).
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
| POST | `/voice/session` | `{site_slug, participant_name?}` | LiveKit access token (PER-SESSION unique room `site-{slug}-{uuid4hex}`, grant scoped to that room — K15(a)) or ElevenLabs signed URL |
| POST | `/voice/chat` | `{site_slug, message, conversation_id?, session_secret?, channel?, channel_identity?}` | text-in/text-out through the same tool layer; resume requires `session_secret` (K15(b)); `channel_identity` (wa_id) honored only with a valid `X-Internal-Token` (K15(c)) |
| POST | `/voice/elevenlabs/tools` | ElevenLabs tool webhook payload | only when `AGENT_BACKEND=elevenlabs` |
| POST | `/voice-admin/voices/enroll` | `{name, sample_base64, tenant}` | XTTS brand-voice enrollment (K15(d): moved off `/voice/*`); guarded by `_require_admin_access` — see the **Admin-access guard (F-2)** note below |
| POST | `/voice-admin/escalations/{conversation_id}/staff-token` | — | K15(e): on-demand staff join token for the escalation room (replaces publishing it on the events topic); same `_require_admin_access` guard |

**Admin-access guard (SPEC-W45 K15(d)/(e), verifier F-2).** Both
`/voice-admin/*` endpoints accept EITHER of two credentials (`401` when
neither applies):

1. **`X-Internal-Token` == `VOICE_ADMIN_INTERNAL_TOKEN`** — the
   service-to-service path (e.g. messaging-gateway), compared in constant
   time. Fail-closed `503` when the env is unset and no other auth applies.
   The APISIX gateway strips client-supplied `x-internal-token` (global
   rule 1), so only in-cluster callers can ever present it — this remains
   the **strong path**.
2. **`X-User-Roles` containing `staff`, `admin` or `platform-admin`** — the
   human path via the APISIX `api-voice-admin` route
   (`/api/voice-admin/*` → `/voice-admin/*`): OIDC `bearer_only` + a
   staff-role gate + SPEC-W44 K1 injection of `X-User-Roles` from the
   verified JWT. admin-web (`voices-client.tsx`, `bookings-client.tsx`)
   calls these endpoints through the gateway; the gateway strips client
   `X-Internal-Token` and injects nothing in its place, so a token-only
   guard 401'd every staff call (defect F-2).

> **TRUST-BOUNDARY WARNING (R1-adjacent residual):** path 2 trusts the
> APISIX header boundary — global rule 1 strips any client-supplied
> `x-user-roles` and the route re-injects the claim-derived value after
> OIDC verification, so external clients cannot spoof it. An **in-cluster
> caller that bypasses the gateway** (direct ClusterIP call) could still
> spoof `X-User-Roles` absent a networkPolicy pinning this service's
> ingress to APISIX. That residual is accepted for the human path; the
> token path remains the strong path for anything sensitive.

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
| `VOICE_TENANT_CTX_TTL_S` / `VOICE_TENANT_CTX_STALE_S` | `120` / `300` | W46 P1/P2: per-slug tenant-context cache fresh window / stale-while-error window (refresh failure serves the stale entry; no entry = error propagates as before) |
| `BOOKING_BASE_URL` | `http://booking:7002` | W46 P-02: direct booking-service base for tool/tenant invokes (daprd invoke fallback on transport failure; empty = pure daprd). Distinct from `BOOKING_URL` (OTP verifier) |
| `VOICE_TTS_CACHE_SIZE` | `256` | W46 P4: TTS LRU size keyed (voice, text, format); `0` disables |
| `VOICE_TTS_CHUNKED` | `true` | W46 P4: sentence-level streaming TTS (first audio frame after the first chunk); `false` restores the full-buffer path |
| `PHONE_CONFIRMATION_REQUIRED` | `true` | phone-confirmation policy toggle |
| `BOOKING_URL` | _(unset)_ | K15(c): booking-service base URL for the portal OTP endpoints (`/public/sites/{slug}/portal/request|verify`). Unset = verification unavailable, mutating tools fail closed |
| `VOICE_BOOKING_INTERNAL_TOKEN` | _(unset)_ | K15(c): `X-Internal-Token` sent on the booking portal OTP calls (K2 pattern; must match booking-side internal token) |
| `VOICE_ADMIN_INTERNAL_TOKEN` | _(unset)_ | K15(d)/(e) + F-2: `X-Internal-Token` half of the `/voice-admin/*` admin-access guard (enrollment, escalation staff-token mint; the other half is the gateway-injected staff-grade `X-User-Roles` — see the guard note above) and for channel-identity pinning on `/voice/chat`. Unset = 503 fail-closed on the token path when no other auth applies |
| `PHONE_HASH_SALT` | _(unset)_ | K15(f): HMAC-SHA256 key for caller-phone hashes in ToolInvoked/capture_location events (W28 scheme: tenant-bound, digits-normalized). Unset = phone omitted from events entirely (never plaintext) |
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
`escalation-{conversation_id}` via livekit-api and publishes
`com.opendesk.conversation.EscalationRequested`
(`{conversation_id, tenant_id, site_slug, room, reason, staff_token_endpoint}`)
to `opendesk.conversation.events` via Dapr — the dashboard listens for the
banner. **SPEC-W45 K15(e): the staff join token is NO LONGER on the event**
(the topic is fan-out — any consumer could hijack the room and listen to the
caller). Staff mint a token on demand via the internal
`POST /voice-admin/escalations/{conversation_id}/staff-token` endpoint
(`X-Internal-Token`, or the gateway-injected staff-grade `X-User-Roles` —
the admin-web bookings-client calls it via `/api/voice-admin/*`, F-2), or
receive it in-room via a targeted LiveKit data
message (`LiveKitEscalation.deliver_staff_token`,
`destination_identities=[staff]`) — staff participant only. The caller hears
a spoken confirmation. When LiveKit is unreachable
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
CLAIMED (unverified) phone — a prompt hint and the emergency
location-capture contact key. **SPEC-W45 K15(c): the carrier-asserted
pre-confirmation bypass is REMOVED (OOS-03)** — caller ID is spoofable, so
SIP callers face the same OTP verification gate as every other channel
before any booking lookup/change. Provisioning:
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
