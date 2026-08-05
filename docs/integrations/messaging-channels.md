# Messaging channels (Nigeria)

Agora's default notification channels are email (Dapr `bindings.smtp`)
and SMS via Twilio (`bindings.twilio.sms`). For Nigerian deployments, SMS
delivery through Twilio is expensive and unreliable (DND filtering, sender-id
registration), so the stack ships a **messaging-gateway** service that fronts
the providers that actually deliver in Nigeria — **Termii**, **Africa's
Talking** and the **WhatsApp Cloud API** — plus per-tenant channel routing
in the notification-worker.

```
notification-worker ──Dapr output binding──► messaging-gateway ──► Termii
                    (bindings-termii,        :7011                 Africa's Talking
                     bindings-africastalking,                      WhatsApp Cloud API
                     bindings-whatsapp)
```

The gateway owns all provider credentials, the retry policy (2 retries on
5xx/429, no retry on 4xx), the provider error mapping (4xx → `400` with the
provider body, persistent 5xx → `502`) and the per-provider metrics
(`messaging_gateway_sends_total{provider,result}` on `/metrics`). Logs never
include the message body (PII).

## Provider setup

### Termii (SMS)

1. Sign up at <https://termii.com> and verify the business account.
2. Dashboard → **API** to copy your API key → `TERMII_API_KEY`.
3. Register a sender id (Dashboard → **Sender ID**; Nigerian carriers require
   registration, approval typically takes a few days). Set it as
   `TERMII_SENDER_ID` (default `OpenDesk`).
4. Fund the wallet; the gateway uses the `generic` channel with `type:
   plain` against `POST https://v2.api.termii.com/api/sms/send`.

### Africa's Talking (SMS)

1. Sign up at <https://account.africastalking.com> and create an app.
2. **Sandbox first**: switch the app to the sandbox, use username `sandbox`
   (`AT_USERNAME=sandbox`) and the sandbox API key (`AT_API_KEY`). Point
   `AT_BASE_URL=https://api.sandbox.africastalking.com` and test against the
   sandbox simulator numbers — no real SMS is sent.
3. Go live: create a production app, copy its username + API key, leave
   `AT_BASE_URL` at the default `https://api.africastalking.com`.
4. Optionally register an alphanumeric sender id (`AT_FROM`); Nigerian
   sender ids require carrier approval, without one messages go out on a
   shared route.
5. The gateway posts form-encoded `username/to/message/from` to
   `POST /version1/messaging` with the `apiKey` header.

### WhatsApp Cloud API

1. Create a Meta developer app at <https://developers.facebook.com> and add
   the **WhatsApp** product.
2. In *WhatsApp → API Setup* you get a **test phone number** and a temporary
   token — add up to 5 recipient numbers (each must verify an OTP) and test
   immediately. Copy the **phone number id** (not the display number) into
   `WHATSAPP_PHONE_NUMBER_ID` and the token into `WHATSAPP_TOKEN`.
3. Production: add a real business number, create a **permanent token** via
   a system user, and submit message templates for approval.
4. Free-form text only works inside the 24h customer-service window; outside
   it you must send an approved **template** (`POST /v1/whatsapp/send` with
   `{"to": "...", "template": "<name>"}`).
5. The gateway posts to
   `POST https://graph.facebook.com/v21.0/{phone_number_id}/messages` with a
   bearer token.

## Dapr binding mechanics

Each provider is a `bindings.http` component pointing at the gateway
(`infra/dapr/components/bindings.{termii,africastalking,whatsapp}.yaml`):

```yaml
metadata:
  name: bindings-termii
spec:
  type: bindings.http
  metadata:
    - name: url
      value: "http://messaging-gateway:7011/v1/termii/sms"
scopes:
  - notification
```

The `scopes: [notification]` means the component only loads on the
notification-worker's daprd sidecar (`DAPR_APP_ID=notification`) — the
gateway itself needs **no sidecar**. The worker invokes the binding with
operation `post` (the `bindings.http` operation set is HTTP verbs, not
`create`) and data `{"to": "+234…", "message": "…"}`; Dapr forwards the data
as the request body to the gateway URL.

## Channel routing

`notification-worker` picks the provider per send:

| Env var | Default | Meaning |
|---|---|---|
| `MESSAGING_CHANNELS` | `email:smtp,sms:twilio` | Fleet defaults per channel |
| `TENANT_CHANNEL_MAP` | — | JSON per-tenant overrides |

```json
{"acme-ng": {"sms": "termii"}, "lagos-clinic": {"sms": "whatsapp"}, "default": {"sms": "africastalking"}}
```

- `sms` providers: `twilio`, `termii`, `africastalking`, `whatsapp`.
  `email`: `smtp`.
- Resolution: tenant entry → `"default"` entry → `MESSAGING_CHANNELS`.
  Unknown tenants fall back to the defaults; invalid entries fail fast at
  worker boot.
- The binding invoke name is `bindings-<provider>`; `smtp`/`twilio` keep the
  existing native bindings, the Nigeria providers hit the gateway.
- Routing applies to every workflow-driven send (confirmations, reminders,
  no-show, waitlist claims, industry-pack messages) because all of them flow
  through the paced `NotifyPaced` → `notify` path with the tenant slug.

## Nigeria notes

- **DND (Do-Not-Disturb)**: Nigerian carriers block generic-route SMS to DND
  numbers. Termii's `dnd` channel / transactional routes and registered
  sender ids mitigate this; WhatsApp bypasses SMS DND entirely and is often
  the most reliable OTP/reminder channel.
- **Sender ids** must be pre-registered per carrier (MTN/Airtel/Glo/9mobile)
  on both Termii and Africa's Talking — plan days of lead time.
- **Phone numbers**: the gateway passes `to` through unchanged; send
  E.164 (`+2348012345678`). Termii also accepts local `234…` format.
- **Cost**: Termii/AT bill per SMS segment in NGN; keep templates under
  160 GSM characters to avoid multi-segment billing.
- **Future**: `POST /v1/ussd/session` — USSD session handling for
  feature-phone journeys (Termii / Africa's Talking USSD gateways) is
  intentionally not implemented yet.
