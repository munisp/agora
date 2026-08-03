# Runbook — Wave 12: Channels & Compliance Guards (USSD, NG SMS, DND/quiet hours, consent, KYC, Flutterwave)

End-to-end ops artifact for SPEC-W12. Components and owners:

| Area | Service | Key artifacts |
| --- | --- | --- |
| USSD + NG SMS aggregators | messaging-gateway (Go, :7011) | `internal/channel/ussd.go`, `internal/provider/{africastalking,termii,ebulksms,failover}.go`, `docs/channels-ussd.md` |
| USSD conversation mapping | conversation-service (Python, :7007) | `app/ussd.py`, `POST /v1/ussd/turns` in `app/routes.py` |
| DND 2442 + quiet hours | notification-worker (:7003) | `internal/pacer/guards.go`, `internal/store/dnd.go`, `docs/dnd-quiet-hours.md` |
| Consent registry | identity-service (:7001) | `internal/consent/`, `docs/compliance-ndpa.md` |
| KYC (BVN/NIN) | kyc-service (Go, :7013, Dapr app-id `kyc`) | `services/kyc-service/`, `docs/kyc.md` |
| Flutterwave | payments-service (Rust, :7004) | `src/flutterwave.rs` |

---

## 1. USSD request/reply contract (messaging-gateway ↔ conversation-service)

Africa's Talking posts form fields `sessionId, serviceCode, phoneNumber,
text` to messaging-gateway. `text` is the **cumulative** input (`1*2*3`),
**empty on the first request** of a session. The wire response is
`text/plain` prefixed `CON ` (continue) or `END ` (terminate). Session
state TTL is **180s** (gateway-owned Dapr state, component `statestore`,
scope `messaging`).

For each callback the gateway synchronously invokes conversation-service
(Dapr invoke, app-id `conversation`):

```
POST /v1/ussd/turns
{
  "tenant_id":    "<uuid>",
  "site_slug":    "acme",
  "session_id":   "ATUid_…",
  "service_code": "*384*123#",
  "phone_number": "+2348012345678",
  "text":         "1*2",                 // cumulative; "" on first request
  "menu": [ {"key":"1","label":"Book appointment","action":"book"}, … ]  // or null
}
```

`menu` is the tenant pack's `ussd.menu` (fetched by the gateway from the
identity packs summary endpoint). **Its presence selects the mode:**
menu mode when present, pass-through text mode when null/absent.

Response `200` (the invoke body the gateway renders):

```
{
  "conversation_id": "<uuid5(tenant, sessionId)>",
  "reply":    "1. Book appointment\n2. Check booking status\n0. Back\n00. Main menu",
  "continue": true,        // true → gateway answers "CON <reply>", false → "END <reply>"
  "mode":     "menu",      // "menu" | "text"
  "selection":"2",          // LAST selection parsed from the cumulative text
  "action":   "book"        // pack action of the selected item ("" otherwise)
}
```

Guarantees on the conversation-service side (`app/ussd.py` +
`app/routes.py::ussd_turn`):

- **Deterministic conversation key**: `uuid5(NAMESPACE_URL,
  "opendesk:ussd:{tenant_id}:{sessionId}")`. One USSD session = one
  conversation, no lookup state. Get-or-create is `INSERT … ON CONFLICT
  (id) DO NOTHING`, so duplicate/parallel first callbacks are safe.
- **One user turn per callback**, text = cumulative input parsed to the
  **LAST selection** (menu mode: the human-readable label, e.g.
  `Book appointment (ussd menu option 2)`; text mode: raw last segment;
  first request: `(ussd session started via *384*123#)`).
- **Channel `ussd`** is persisted on the conversation (channel enum +
  Postgres CHECK widened at startup via `ensure_ussd_channel`,
  contract §2). `contact_phone` = `phoneNumber` (feeds the incident IDP
  `callback_number`).
- **Idempotency**: key `ussd:{sessionId}:{text}` (+`:reply` for the agent
  turn). Retried AT callbacks with identical text append nothing twice.
- **Reply recorded as an agent turn** (mirrors the telegram/whatsapp
  bridge's user→agent turn pair). Agent/system turns are **never**
  incident-classified (existing rule); user turns pass the SPEC-W11
  emergency classifier **unchanged**, with `ussd` mapped web-like in the
  IDP channel enum (`incidents._CHANNEL_MAP: ussd→web`).

### Menu mode (low-literacy navigation)

Single-level numeric state machine driven by conversation-service:

| Input | Behaviour | `continue` |
| --- | --- | --- |
| *(empty, first request)* | render numbered menu + `0. Back` / `00. Main menu` footer | `true` (CON) |
| `1`…`9` matching a key | confirmation `<label>. Thank you — … confirmation SMS shortly.` | `false` (END) |
| `0` (back) / `00` (main menu) | re-render menu (single level: identical) | `true` |
| anything else | `Invalid selection. Please try again.` + menu | `true` |

### Text mode (no pack menu)

The raw last selection is appended as the user turn; the reply is the
configurable acknowledgement `USSD_TEXT_MODE_REPLY` (default "Thank you.
Your message has been received; an agent will respond shortly.") with
`continue=true`. Rich agent replies ride the existing bridge path
(voice runtime `/voice/chat`) if the gateway chains it — out of scope for
the synchronous USSD contract.

### Ops checks

```bash
# hook health (local dev, direct base):
curl -s localhost:7007/v1/ussd/turns -H 'content-type: application/json' -d '{
  "tenant_id":"<uuid>","site_slug":"acme","session_id":"s1",
  "service_code":"*384#","phone_number":"+2348012345678","text":"",
  "menu":[{"key":"1","label":"Book appointment","action":"book"}]}'

# kill switch:
USSD_ENABLED=false   # POST /v1/ussd/turns answers 503
```

---

## 2. NG SMS aggregator chain (messaging-gateway)

Env (contract §8): `AT_API_KEY/AT_USERNAME`, `TERMII_API_KEY`,
`EBULK_API_KEY/EBULK_USERNAME`, `SMS_PROVIDER_CHAIN` (default
`africastalking,termii,ebulksms`). Ordered failover: on 5xx/timeout the
next provider in the chain is tried; each provider has a circuit breaker
and a price-tier annotation (at=1.0, termii=1.0, ebulks=0.85) used for
reporting only. Metering event `sms_send` carries a `provider` label.
Provider request/response shapes are **ASSUMPTIONS** (no live keys in the
repo) — verify against provider docs before production traffic.

---

## 3. DND 2442 + quiet hours (notification-worker)

Every paced send kind is classified (contract §3):

- **marketing** (`geo_campaign`, `promo`, `broadcast`, `drip`): DND-suppressed
  (per-tenant opt-out → global 2442 list) and quiet-hours deferred
  (`workflow.Sleep` until the window opens; tenant tz, default
  Africa/Lagos, default window `20:00-08:00`, overnight supported).
- **transactional** (booking confirmations, reminders, `incident_alert`,
  `otp`): neither. `incident_alert` keeps the existing Priority lane.

Suppressed sends complete as `suppressed_dnd` and count
`notifications_suppressed_total{reason}`. Env: `DND_ENFORCEMENT`
(default true), `QUIET_HOURS_DEFAULT`, `QUIET_HOURS_OVERRIDES` (JSON
per-channel). Admin API: `POST /v1/dnd/import`,
`DELETE /v1/dnd/{phone}`, `GET /v1/dnd/check?phone=`.

---

## 4. Consent registry + erasure (identity-service)

`ConsentRecord`: `{consent_id, tenant_id, data_subject_id (phone_e164 or
contact uuid), purpose, captured_ts, captured_channel, captured_locale,
erasure_ts}`. Capture is idempotent on `(tenant, subject, purpose)`.
Service-to-service gate: `GET /internal/consents/check?subject=&purpose=`
(tenant header, no auth middleware). Erasure (`POST /v1/consents/erasure`)
sets `erasure_ts` and publishes CloudEvent
`com.opendesk.consent.ErasureRequested` to topic
`opendesk.consent.erasure.v1` — tombstone-only; downstream consumers
anonymize their own data-subject records (consumer contract:
`docs/compliance-ndpa.md`).

---

## 5. KYC service (BVN/NIN)

Go service on :7013, Dapr app-id `kyc`. `POST /v1/kyc/resolve`
`{tenant_id, subject_phone, id_type:"bvn"|"nin", id_value}` →
`{status:"verified"|"mismatch"|"pending", reference, latency_ms}`.

- **Consent-gated**: calls identity `GET /internal/consents/check
  ?subject=&purpose=kyc` first; no consent → `403`.
- `KYC_MOCK=1` (default): deterministic mock — `id_value` all digits and
  length ≥ 10 → `verified`.
- Audit table `kyc_audit` (tenant RLS: who/what/when/result); result
  published as `com.opendesk.kyc.Resolved` on `opendesk.kyc.resolved.v1`.
- p95 target ≤ 8s (see `docs/kyc.md`).

Compose: `kyc` + `daprd-kyc` blocks added (port 7013). The DB `kyc` must
exist — add it to `infra/postgres/init-scripts/00-create-dbs.sql` if not
present (owner: Agent C migration path).

---

## 6. Flutterwave (payments-service)

Mirrors the Paystack module: `POST /v1/payments/flutterwave/initialize`,
webhook `POST /webhooks/flutterwave` validated by header `verif-hash` ==
`FLUTTERWAVE_SECRET_HASH` (constant-time compare). Same outbox/ledger
codes as the Paystack path. Env: `FLUTTERWAVE_SECRET_KEY`,
`FLUTTERWAVE_SECRET_HASH`. Rust changes were **statically reviewed only**
(no toolchain in the wave environment) — `cargo check` runs in CI.

---

## 7. Infra changes this wave

- **Kafka topics** (`infra/kafka/create-topics.sh`, additive):
  `opendesk.consent.erasure.v1`, `opendesk.kyc.resolved.v1`, `cac.events`.
  Re-run the one-shot job after broker bring-up:
  `BOOTSTRAP=kafka:9092 bash infra/kafka/create-topics.sh`.
- **Dapr state**: the generic Redis `statestore` already exists, so **no
  dedicated `state.ussd.yaml`** was created; its `scopes` gained
  `messaging` (gateway USSD session state, 180s TTL keys).
- **docker-compose.yml** (additive only): `kyc` + `daprd-kyc` services;
  `EBULK_*` + `SMS_PROVIDER_CHAIN` on messaging-gateway; `DND_*` +
  `QUIET_HOURS_*` on notification; `FLUTTERWAVE_*` on payments.

## 8. Data-residency note (NDPA 2023)

All Wave-12 personal data stays in-region by deployment: USSD session
state (Redis), conversation turns (Postgres `conversation` DB, RLS per
tenant), consent records + KYC audit (Postgres `identity`/`kyc` DBs) run
inside the same compose/cluster boundary — only the **provider edge**
(Africa's Talking, Termii, eBulksms, Flutterwave) leaves the platform,
and only with the minimum payload (destination MSISDN + message text /
hashed webhook signature). The NDPA retention profile
(`infra/privacy/ndpa-profile.env`, 180-day turn retention) applies
unchanged to `channel='ussd'` conversations: the existing sweeper
hard-deletes their turns on the same schedule, and GDPR/NDPA erasure
(`contact_phone` tombstone) covers USSD callers automatically because the
conversation carries their MSISDN in `contact_phone`.
