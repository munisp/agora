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

## Inbound media (WhatsApp → OCR)

SPEC-W60: inbound WhatsApp **images and PDF documents** posted to
`POST /webhooks/whatsapp` are no longer dropped — the gateway fetches the
bytes from Meta and funnels them into the same ocr-service pipeline
(`POST /v1/ocr/extract`) that booking receipt uploads use, inheriting its
magic-byte validation, 25 MB cap, ClamAV scan (W59), MinIO PII bucket and
tenant-delete erasure.

### Fetch flow (Meta 2-GET)

The webhook message carries a media **id** (plus `mime_type`, `sha256`,
`caption`, `filename`) — never the bytes. The gateway fetches with the same
`WHATSAPP_TOKEN` bearer, in two GETs:

1. `GET {WHATSAPP_BASE_URL}/{media-id}` → JSON `{url, mime_type, sha256,
   file_size}` (retriable — idempotent).
2. `GET {url}` (Meta lookaside host) → the bytes, streamed through a
   25 MB + 1 `io.LimitReader` cap with a dedicated 30s HTTP client (the 10s
   send client is too tight). Single attempt: a failure answers the webhook
   `500`, Meta redelivers, and the redelivery re-mints a fresh URL (the
   lookaside URL expires in ~5 minutes; the media id does not).

Before **any** fetch, the gateway (in order): resolves the tenant via
`CHANNEL_SITE_MAP` (unmapped `phone_number_id` ⇒ `200` drop — the cost
gate), checks the 24h redelivery dedupe heap (claimed on success only), and
applies a per-sender sliding-window throttle
(`WHATSAPP_MEDIA_RATE_PER_MINUTE`, default `10`, in-memory per replica —
mirroring the USSD `ratelimit.go` precedent; required because the APISIX
`limit-count` keys Meta's egress IPs, not individual senders, and a tripped
sender gets `200` + drop-log). Deterministic rejects
(disallowed mime, oversize, AV-infected OCR `415`) answer `200` + drop-log
so Meta never redelivery-storms them.

### Kind mapping (hard-coded)

| WhatsApp media | ocr `kind` |
|---|---|
| `image/*` (jpeg/png/webp) | `receipt` |
| `application/pdf` | `document` |

Nothing else is emittable — audio/video/stickers/gif are ignored, and the
mapping table above is the whole policy. The caption is **not** sent to OCR
(no field for it) and is never logged; logs carry ids, byte counts and
sha256 prefixes only (PII discipline).

### Why kyc_* kinds are excluded

The ocr-service `kyc_*` kinds sit behind the W58 consent gate: a
pre-existing `purpose=kyc_document` consent on the subject's phone is
required before any byte leaves for OCR, and a WhatsApp message cannot
bootstrap that consent (the public consent endpoint is a web surface;
in-chat consent capture is its own UX + legal design). KYC-over-WhatsApp is
therefore structurally closed this wave and deferred to a future "Option C"
wave together with outbound media.

### Rollout flag + fail-closed posture

- `WHATSAPP_MEDIA_ENABLED` (default `false`): when off, media messages drop
  + log exactly as pre-W60 — zero behavior change on upgrade.
- With the flag on but `OCR_SERVICE_URL` / `OCR_INTERNAL_TOKEN` unset, the
  gateway logs CRITICAL at boot and drops media per-message (fail-visible,
  never fail-silent).
- `WHATSAPP_APP_SECRET` is **production-required**: without it (and
  `WHATSAPP_MOCK` off) every inbound post fails closed with `401`. The dev
  compose default `WHATSAPP_MOCK=1` (DEV ONLY) is the only bypass;
  production overlays must leave the mock unset/0.

### Booking anchor + media inbox (SPEC-W61)

With `WHATSAPP_MEDIA_ANCHOR_ENABLED` on, the gateway POSTs a metadata row
to booking-service `POST /v1/media/inbox/ingest` (internauth, edge-DENIED
in APISIX) after OCR success. Booking resolves the sender phone
(digits-only, both-sides normalized) against tenant contacts: a unique
recent candidate booking anchors the media; anything else is **parked** for
staff review in `/app/[orgSlug]/media-inbox` — the ingest path NEVER
creates contacts or leads. Parked rows are capped at 500 per tenant
(oldest excess auto-discarded) and bounded in time by the media TTL below.
Full doc: [docs/media-inbox.md](../media-inbox.md).

| Env var | Default | Meaning |
|---|---|---|
| `WHATSAPP_MEDIA_ANCHOR_ENABLED` | `false` | Anchor rollout flag — off = the W60 path is byte-identical |
| `MEDIA_INBOX_TTL_HOURS` | `168` | Sent as `expires_in_hours` on the OCR extract call (1..8760); bounds parked PII retention |
| `MESSAGING_BOOKING_INTERNAL_TOKEN` | mapped from `BOOKING_INTERNAL_TOKEN` | Ingest internauth token (X-Internal-Token); compose wires the mapping |

### Multi-tenancy model

**One Meta app, one WABA phone number per tenant.** Inbound routing keys on
`metadata.phone_number_id` via `CHANNEL_SITE_MAP`
(`whatsapp:<phone_number_id> → {tenant_id, site_slug}`) — it is the only
tenant key present in the webhook payload. A single number shared across
tenants is therefore **unroutable** today; cross-tenant shared-number
routing (e.g. by keyword) is a future product decision, not a config
option. A `CHANNEL_SITE_MAP` typo misroutes media into the wrong tenant
bucket (cross-tenant PII leak) — the blast radius already exists for text;
review the map on every change.

### Known limitations (stated honestly)

- **Erasure gap (pre-existing, inherited — not a regression):** ocr
  `receipt`/`document` rows are not subject-bound (no subject column by
  design), so a consent `ErasureRequested` does **not** reach them — only
  `kyc_*` rows are erased that way. Tenant deletion still hard-deletes all
  rows + MinIO objects. **SPEC-W61 closes this for WhatsApp-ingested
  media** when the anchor flag is on: the `media_inbox` row binds
  `document_id ↔ sender phone`, so subject erasure tombstones the inbox row
  AND hard-erases the OCR bytes (per-document erase + TTL backstop) — see
  [docs/media-inbox.md](../media-inbox.md). Receipts uploaded through other
  paths remain subject to the original gap.
- **Kind misclassification:** a KYC slip photographed and sent as an image
  lands as `kind=receipt`, bypassing the kyc TTL/consent machinery. The
  bytes are still AV-scanned and tenant-bucketed; **escalation is a tenant
  staff workflow decision**, not something the pipeline can detect. True
  `kyc_*` intake over WhatsApp stays closed until the Option C consent
  flow.

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
