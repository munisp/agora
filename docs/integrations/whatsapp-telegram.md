# Omnichannel inbound: WhatsApp + Telegram (SPEC-W6 Part A)

The messaging-gateway (`:7011`) terminates inbound provider webhooks
directly and bridges messages into the platform: it resolves the tenant via
`CHANNEL_SITE_MAP`, resolves-or-creates a conversation in
conversation-service, records the user turn (idempotency-keyed), calls the
voice runtime buffered chat path (`POST /voice/chat`), records the
assistant turn, and replies via the same-channel provider.

```
Meta/Telegram ──webhook──► APISIX /webhooks/* ──► messaging-gateway ──► conversation-service
  (verify token / shared secret)                  :7011                ──► voice-agent-runtime
                                                       └──────────────► WhatsApp / Telegram (reply)
```

**Reliability contract:** webhook handlers always answer `200` fast —
Meta and Telegram retry-storm on non-200. The only non-200 answers are
`403` for a bad WhatsApp verify token or Telegram shared secret. All
internal failures (conversation-service down, LLM error, provider send
failure) are logged and swallowed into `200`. Turn writes use
`Idempotency-Key: <channel>:<message_id>` (and `…:reply` for the assistant
turn), so provider webhook retries never duplicate turns.

## Webhook endpoints (public, via APISIX route `webhooks-messaging`)

| Endpoint | Purpose | Auth |
|---|---|---|
| `GET /webhooks/whatsapp` | Meta verification handshake | `hub.verify_token` = `WHATSAPP_VERIFY_TOKEN` → echoes `hub.challenge`, else 403 |
| `POST /webhooks/whatsapp` | Meta Cloud API messages | always 200; `statuses[]` receipts and non-text messages ignored |
| `POST /webhooks/telegram` | Telegram Bot API updates | `X-Telegram-Bot-Api-Secret-Token` = `TELEGRAM_WEBHOOK_SECRET` when set, else 403 |
| `POST /v1/telegram/send` | Outbound parity endpoint `{to, message}` | same as other `/v1/*` sends |

Base URL in dev through APISIX: `http://localhost:9080` (so the webhook URL
Meta/Telegram call is e.g. `https://<your-host>/webhooks/whatsapp`).

## Environment

| Var | Purpose |
|---|---|
| `WHATSAPP_VERIFY_TOKEN` | Random string you invent; Meta sends it back during verification |
| `TELEGRAM_BOT_TOKEN` | BotFather token (also enables outbound `sendMessage`) |
| `TELEGRAM_BOT_USERNAME` | Bot username — the Telegram key in `CHANNEL_SITE_MAP` (single bot per gateway deployment) |
| `TELEGRAM_WEBHOOK_SECRET` | Optional shared secret; pass as `secret_token` in `setWebhook` |
| `CHANNEL_SITE_MAP` | Channel identity → tenant/site routing JSON (below) |
| `CONVERSATION_URL` / `VOICE_RUNTIME_URL` | Direct-base overrides (tests / sidecar-less dev, e.g. `http://conversation:7007`). Empty = Dapr sidecar invoke on `DAPR_HTTP_PORT` (default 3500) |

### `CHANNEL_SITE_MAP` format

WhatsApp is keyed by the **phone number id** from the payload
(`entry[].changes[].value.metadata.phone_number_id`), Telegram by
`TELEGRAM_BOT_USERNAME`:

```json
{
  "whatsapp:109876543210987": {"site_slug": "acme-ng", "tenant_id": "11111111-2222-3333-4444-555555555555"},
  "telegram:acme_ng_bot":     {"site_slug": "acme-ng", "tenant_id": "11111111-2222-3333-4444-555555555555"}
}
```

Unmapped identities are logged and dropped (webhook still answers 200).

## Meta (WhatsApp Cloud API) setup

1. Follow `docs/integrations/messaging-channels.md` → *WhatsApp Cloud API*
   to get `WHATSAPP_TOKEN` + `WHATSAPP_PHONE_NUMBER_ID`.
2. Pick a verify token: `openssl rand -hex 16` → `WHATSAPP_VERIFY_TOKEN`.
3. Meta app → **WhatsApp → Configuration → Webhook**:
   - Callback URL: `https://<your-host>/webhooks/whatsapp`
   - Verify token: the value from step 2 → **Verify and save** (Meta issues
     the `GET` handshake; the gateway echoes `hub.challenge`).
4. Subscribe to the **`messages`** webhook field (skip `statuses`-only
   subscriptions unless you want the extra traffic — they are ignored).
5. Add the phone number id to `CHANNEL_SITE_MAP` as shown above.
6. Test: send a WhatsApp message to the business number; the reply should
   arrive in the same chat and both turns appear under the conversation in
   conversation-service.

## Telegram setup

1. Talk to [@BotFather](https://t.me/BotFather) → `/newbot` → copy the token
   into `TELEGRAM_BOT_TOKEN` and the bot username (without `@`) into
   `TELEGRAM_BOT_USERNAME`.
2. Pick a webhook secret: `openssl rand -hex 16` → `TELEGRAM_WEBHOOK_SECRET`.
3. Register the webhook:
   ```bash
   curl "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/setWebhook" \
     -d url="https://<your-host>/webhooks/telegram" \
     -d secret_token="$TELEGRAM_WEBHOOK_SECRET"
   ```
4. Verify: `curl "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/getWebhookInfo"`.
5. Add `telegram:<bot_username>` to `CHANNEL_SITE_MAP`.
6. Test: DM the bot; the reply comes from the voice runtime in the same
   chat.

## Troubleshooting

- **403 on Meta verification** — `hub.verify_token` ≠ `WHATSAPP_VERIFY_TOKEN`,
  or the env var is empty (the gateway refuses to verify when unset).
- **403 on every Telegram update** — the `secret_token` passed to
  `setWebhook` ≠ `TELEGRAM_WEBHOOK_SECRET`. Re-run `setWebhook`.
- **200 but nothing happens** — check gateway logs for
  `inbound message dropped: no CHANNEL_SITE_MAP entry`: the WhatsApp key
  must be the *phone number id* (not the display number) and the Telegram
  key must match `TELEGRAM_BOT_USERNAME` exactly.
- **Turn recorded but no reply** — look for `agent reply failed` /
  `channel reply failed` in the gateway logs: voice runtime unreachable
  (set `CONVERSATION_URL`/`VOICE_RUNTIME_URL` when running without a Dapr
  sidecar) or provider send failing (outside the 24h WhatsApp window a
  template is required — free-form replies are then rejected by Meta).
- **Duplicate-looking messages** — provider retries are expected; turn
  writes are deduped by conversation-service on the
  `Idempotency-Key: <channel>:<message_id>` header.
- **Health** — `GET /healthz` on the gateway; send metrics at
  `GET /metrics` (`messaging_gateway_sends_total{provider="telegram",…}`).
