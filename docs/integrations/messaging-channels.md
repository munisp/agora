# Messaging channels — Termii, Africa's Talking, WhatsApp (SPEC-W4 §4)

Nigeria-first outbound messaging: pluggable providers behind Dapr HTTP output
bindings, per-tenant channel routing (Termii / Africa's Talking / WhatsApp
Cloud API), and opt-in PII redaction of outbound bodies. Delivered by
`services/messaging-gateway` (Go, port 7011, Dapr app-id `messaging`) plus
provider/route additions to `services/notification-worker`.

## Architecture

```
notification-worker                messaging-gateway (:7011)
┌───────────────────────┐  Dapr    ┌───────────────────────────────┐
│ NotifyPaced activity  │ invoke   │ POST /v1/sms/send             │
│ kind=sms|whatsapp     │─────────▶│  {to,message,sender_id?}      │
└───────────────────────┘          │    ↓ provider chain (mock/live)│
                                   │    POST /v1.0/bindings/termii │
                                   │    POST /v1.0/bindings/…      │
                                   └───────────────────────────────┘
                                              │ Dapr HTTP bindings
                            ┌─────────────────┼──────────────────┐
                            ▼                 ▼                  ▼
                     bindings.termii   bindings.africastalking  bindings.whatsapp
                     (Termii JSON)     (AT form POST)           (Meta Cloud API)
```

The worker treats messaging-gateway as the SMS/WhatsApp "provider":
`messaging.Send` Dapr-invokes `POST /v1/sms/send` and maps the gateway's
`provider_status` to success/failure, so pacing/metering/audit all stay in
the worker. The gateway owns vendor specifics.

## Tenant channel routing (`TenantChannelResolver`)

Resolution order for `notification.sendRouting` (workflow
`resolveChannelsForTenant`):

1. **`TENANT_CHANNELS`** env JSON — highest precedence:
   `{"acme": {"sms": "termii", "whatsapp": "whatsapp"}}`
2. **Tenant row `country` via Dapr invoke identity** —
   `GET /v1.0/invoke/identity/method/v1/tenants/{slug}` reads
   `country` (and `channels` if the tenant payload carries one; both keys
   are optional and the resolver tolerates their absence).
3. **`COUNTRY_CHANNELS`** env JSON — e.g.
   `{"NG": {"sms": "africastalking", "whatsapp": "whatsapp"}}`
   (shipped default: NG → africastalking sms).
4. **Static defaults** — sms `termii`, whatsapp `whatsapp`.

The chosen **provider name is stamped on `notification.deliveries.provider`**
for billing/audit (so rows say `termii`/`africastalking`/`whatsapp`, not the
transport). Delivery **status remains pending→sent only** — Nigeria carrier
DLR webhooks are out of scope for W4 and are a documented follow-up
(`docs/ARCHITECTURE.md` notification section).

## Providers

### Termii (Dapr binding `termii`)

Component: `infra/dapr/components/bindings.termii.yaml` → `POST
https://api.ng.termii.com/api/sms/send` with body
`{api_key,to,from,sms,type:"plain",channel:"generic"}`. Live when
`TERMII_API_KEY` is set; otherwise **mock mode** (default) — no binding call,
logs the send, returns a synthetic `provider_status: 200`.

### Africa's Talking (Dapr binding `africastalking`)

Component: `bindings.africastalking.yaml` → `POST
https://api.africastalking.com/version1/messaging` with
`Content-Type: application/x-www-form-urlencoded`, `apiKey` header, body
`username,to,message,from`. Live when `AT_API_KEY` + `AT_USERNAME` set;
otherwise mock mode. Sender id from `AT_FROM` (or per-request `sender_id`).

### WhatsApp Cloud API (Dapr binding `whatsapp`)

Component: `bindings.whatsapp.yaml` → `POST
https://graph.facebook.com/v19.0/{WHATSAPP_PHONE_NUMBER_ID}/messages` with
bearer `WHATSAPP_ACCESS_TOKEN`, body
`{messaging_product:"whatsapp",to,type:"text",text:{body,preview_url:false}}`.
Live when both envs set; otherwise mock mode.

### Twilio (unchanged)

The pre-existing Twilio SMS/WhatsApp path stays exactly as before —
messaging-gateway is simply another provider option; nothing was removed.

## PII redaction (`REDACT_PII=1`)

`gateway/redact.go` — opt-in, applied to the message body **before** the
binding call, and mirrored by notification-worker's `provider.RedactPII` so
both layers can scrub independently (defense in depth; each off by default):

| Pattern | Replacement |
|---|---|
| E.164-ish phone numbers (`+2348012345678`) | `[phone]` |
| Email addresses | `[email]` |
| 11-digit BVNs | `[bvn]` |
| 11-digit NINs (same shape, label split so audits can tell) | `[nin]` |

Heuristic by design: it targets the common leak vectors (a confirmation
echoing the caller's phone, a staff note with an ID number) and errs toward
over-redaction. Redaction events are counted
(`messaging_gateway_redactions_total{kind}`) and logged at debug level with
the pattern name only — never the original text.

## Metering

The gateway meters every accepted send onto the existing usage topic
(`USAGE_TOPIC`, default `opendesk.usage.events`) as CloudEvents:

- `com.opendesk.usage.UsageRecord{metric:"sms_send", value:1,
  meta:{provider, mock, redacted}}`
- `com.opendesk.usage.UsageRecord{metric:"whatsapp_send", …}` for the
  whatsapp provider label.

These are **additive** to the worker's `email_sent`/`sms_sent`/… usage
records (worker meters its own send activity; the gateway meters vendor
dispatch). Publisher is a no-op when no Dapr sidecar is present (dev/test
friendly).

## Configuration

messaging-gateway env:

| Var | Default | Purpose |
|---|---|---|
| `PORT` | `7011` | HTTP listen |
| `TERMII_API_KEY` | — | Termii live mode when set |
| `AT_API_KEY`, `AT_USERNAME`, `AT_FROM` | — | Africa's Talking live mode |
| `WHATSAPP_ACCESS_TOKEN`, `WHATSAPP_PHONE_NUMBER_ID` | — | WhatsApp live mode |
| `REDACT_PII` | `0` | `1` scrubs bodies before vendor dispatch |
| `USAGE_TOPIC` | `opendesk.usage.events` | metering topic |
| `DAPR_HTTP_PORT` | `3500` | sidecar port |

notification-worker env (additive):

| Var | Default | Purpose |
|---|---|---|
| `MESSAGING_GATEWAY_APP_ID` | `messaging` | Dapr app-id to invoke |
| `TENANT_CHANNELS` | `{}` | per-slug channel overrides (JSON) |
| `COUNTRY_CHANNELS` | NG→africastalking | country→channel map (JSON) |
| `IDENTITY_APP_ID` | `identity` | tenant lookup for routing |

## Verification

- `go build/vet/test ./...` green in both services (go1.23.4).
- Provider/binding/mock tests: `messaging-gateway/internal/provider/*_test.go`.
- Routing tests: `notification-worker/internal/workflows/channels_test.go`
  (precedence, fallback, mixed-case country).
- Redaction tests: phone/email/BVN/NIN tables in `gateway/redact_test.go` and
  `provider/redact_test.go`.
- Manual: `curl -X POST localhost:7011/v1/sms/send -d '{"to":"+234…","message":"hi"}'`
  → `{"provider":"termii","provider_status":200,"mock":true}` with no keys set.
