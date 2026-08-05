# WhatsApp & Telegram channels — SPEC-W5 Agent C

WhatsApp Cloud API and Telegram Bot API as first-class inbound/outbound
channels, wired **channel → conversation bridge → voice agent → tools**.
Outbound rides the paced notification lane (`PacedSend`) — it does not
short-circuit the pacer.

## Components

| Component | Path | Notes |
|---|---|---|
| messaging-gateway | `services/messaging-gateway` (Go, :7011) | Webhook verification + signature checks, idempotent ingress, per-tenant channel config, Dapr invoke to conversation bridge |
| conversation bridge | `services/conversation-service/app/bridge.py` | `POST /v1/channels/{whatsapp,telegram}/messages`: map (channel, sender) → conversation, append user turn, run `/voice/chat`, persist + return reply |
| notification-worker | `internal/provider/{whatsapp,telegram}.go`, `internal/workflows/paced.go` | `PacedSend` kinds `whatsapp` / `telegram` |
| tenant config | booking-service `channel_configs` | `GET/PUT /v1/channels/{channel}/config` (RLS; `manage_bookings`) |

## Infra deltas (additive only)

- **APISIX** — routes are already covered by the catch-all `/webhooks/*`
  route in `infra/apisix/apisix.yaml` (public, upstream `messaging:7011`);
  **no file change needed** (decision recorded here per SPEC-W5 §Deliverables).
- **Kafka** — no new topics. Replies go through the existing paced-send
  command topic `opendesk.notifications.outbox` (notification-worker's
  `notifyoutbox` consumer, forward-compatible).
- **Dapr** — `infra/dapr/components/bindings.whatsapp.yaml` and
  `bindings.telegram.yaml` input bindings (declarative; the app polls
  `webhooks/messaging-gateway/{channel}` only when `MESSAGING_BINDINGS=true`,
  otherwise APISIX proxies directly — both supported).

## Env vars

| Var | Default | Purpose |
|---|---|---|
| `WHATSAPP_ACCESS_TOKEN` | — | Meta Cloud API token (sends + media download) |
| `WHATSAPP_PHONE_NUMBER_ID` | — | Cloud API phone-number id |
| `WHATSAPP_APP_SECRET` | — | enables `X-Hub-Signature-256` HMAC verification |
| `WHATSAPP_VERIFY_TOKEN` | `opendesk-dev-verify` | Meta GET-challenge token |
| `WHATSAPP_API_BASE` | `https://graph.facebook.com/v19.0` | override for tests |
| `TELEGRAM_BOT_TOKEN` | — | Bot API token (sends + getFile/download) |
| `TELEGRAM_SECRET_TOKEN` | — | enables `X-Telegram-Bot-Api-Secret-Token` verification |
| `TELEGRAM_API_BASE` | `https://api.telegram.org` | override for tests |
| `CHANNEL_SITE_MAP` | `{}` | JSON route map, see below |
| `MESSAGING_BINDINGS` | `false` | also expose Dapr-binding endpoint paths |
| `MESSAGING_EVENT_TIMEOUT_SECONDS` | `10` | Dapr publish timeout on the ingest path |
| `VOICE_AGENT_URL` | `http://voice-agent-runtime:7006` | conversation bridge → agent base |
| `CHANNEL_MEDIA_MAX_BYTES` | `8388608` | voice-note download cap |
| `CHANNEL_SESSION_IDLE_MINUTES` | `30` | new conversation after idle gap |

## CHANNEL_SITE_MAP (tenant routing)

The route key is `whatsapp:<phone_number_id>` / `telegram:<bot_token>`:

```json
{
  "whatsapp:771234567890123": {"site_slug": "acme-salon", "tenant_id": "<uuid>"},
  "telegram:8321456789:AAH...": {"site_slug": "acme-salon", "tenant_id": "<uuid>"}
}
```

Unknown routes are ACKed with 200 and dropped (`{"accepted": false,
"reason": "unknown_route"}`) so Meta/Telegram never retry-storm us.

## Webhook registration

- **WhatsApp** (Meta App → WhatsApp → Configuration): callback URL
  `https://<gateway>/webhooks/messaging-gateway/whatsapp`, verify token =
  `WHATSAPP_VERIFY_TOKEN`. The gateway answers the GET challenge and verifies
  `X-Hub-Signature-256` (HMAC-SHA256 over the raw body with
  `WHATSAPP_APP_SECRET`) on POSTs when the secret is set.
- **Telegram**: `setWebhook` to
  `https://<gateway>/webhooks/messaging-gateway/telegram` with
  `secret_token` = `TELEGRAM_SECRET_TOKEN`; the gateway verifies the
  `X-Telegram-Bot-Api-Secret-Token` header when configured.

Both POST endpoints **always answer 200 fast** on well-formed payloads
(processing continues inline but the response shape never leaks internal
errors); 401/400 only for failed verification or garbage bodies.

## Reply flow (paced, never short-circuited)

1. messaging-gateway verifies + dedupes (`message_id`, Redis
   `statestore-messaging`, 24h TTL) and Dapr-invokes the conversation bridge
   with `ChannelMessage{channel, sender_id, site_slug, tenant_id, text,
   media_id, message_id}`.
2. Bridge maps `(channel, sender_id)` → conversation (`channel_conversations`
   upsert; idle > 30 min ⇒ new conversation; conversation row carries
   `channel`, `external_user_id`), appends the user turn, calls the voice
   agent's `/voice/chat` (`site_slug`, `session_id=channel:sender`), and
   persists the reply as the agent turn.
3. The gateway publishes one CloudEvent `com.opendesk.notifications.PacedSend`
   to `opendesk.notifications.outbox` whose data is a `PacedSendRequest`:
   - **text** → `kind: "whatsapp"` with `whatsapp.to = E.164 phone`, or
     `kind: "telegram"` with `telegram.chat_id`.
   - **voice note** (WhatsApp `audio` / Telegram `voice`): the bridge
     downloads the media (capped), transcribes it via voice-runtime
     `POST /voice/stt` (whisper, multipart — new control-plane endpoint), and
     if a transcript is produced, the gateway sends a *preview* PacedSend:
     "🎤 Voice note: \"<transcript excerpt>\" — reply processing…"; the real
     reply follows as the normal kind. If the STT preview send was emitted,
     the reply is suppressed when empty.
4. notification-worker's `notifyoutbox` consumer starts one
   `PacedSendWorkflow` per event (workflow id `paced-send-<cloudevent id>`;
   already-started ⇒ ACK = idempotent), which paces and delivers through the
   channel provider.

Failure policy: provider send failures are **not** retried by the paced
workflow today (same as other kinds); the bridge/user turns are already
durable, and ingest ACKs are independent of downstream delivery.

## Providers (notification-worker)

`internal/provider/whatsapp.go` — Cloud API `POST
/{phone-number-id}/messages` (`messaging_product=whatsapp`, `text.body` ≤4096
chars, preview_url disabled). `internal/provider/telegram.go` — Bot API
`sendMessage` (`disable_web_page_preview`). Both:

- 2 attempts (1 retry, 100ms backoff), transport/5xx/429 retried, 4xx not;
- token redacted from errors and logs (never logged in full);
- `whatsapp` requires `WHATSAPP_ACCESS_TOKEN` + `WHATSAPP_PHONE_NUMBER_ID`;
  `telegram` requires `TELEGRAM_BOT_TOKEN`. Both providers must be configured
  for their kinds or sends fail fast.

Classification (SPEC-W5 contract): both kinds are **transactional** —
they carry the AI receptionist's direct replies, so DND/quiet-hours do not
suppress or defer them.

## Per-tenant channel config (booking-service)

`channel_configs(tenant_id, channel, enabled, config jsonb)` with RLS
(`SET LOCAL app.tenant_id`), upsert semantics on PUT:

```
GET /v1/channels/whatsapp/config   → {"channel":"whatsapp","enabled":false,"config":{}}
PUT /v1/channels/telegram/config   {"enabled": true, "config": {"greeting": "..."}}
```

Permission `manage_bookings`; channel must be `whatsapp|telegram`; config ≤16KB.

## Voice notes (STT endpoint)

`POST /voice/stt` on voice-agent-runtime: multipart `file` (webm/ogg/mp4/amr
etc.), whisper-only, hard timeout 4s, 503 when STT isn't local whisper,
413 over `STT_MAX_UPLOAD_MB` (default 25MB). Best-effort: bridge continues
with a placeholder when transcription fails.

## Tests & verification

- Go: `go test ./...` in messaging-gateway (webhook verify, dedupe, route
  map, PacedSend publish, handler paths) and notification-worker (providers,
  paced classification, consumer idempotency).
- Python: `pytest` in conversation-service (bridge mapping, idle-gap new
  conversation, turns, reply persistence) and voice-agent-runtime (STT
  endpoint).
- Idempotency proven at three layers: ingress dedupe (message_id), workflow
  start dedupe (deterministic workflow id), conversation upsert
  (unique `(tenant_id, channel, external_user_id)`).
