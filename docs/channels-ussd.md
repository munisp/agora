# USSD channel (Wave 12, Agent A)

Feature-phone access to the CAC assistant over Africa's Talking USSD, plus
the Nigeria SMS aggregator failover chain (Africa's Talking → Termii →
eBulksSMS). Implemented in `services/messaging-gateway`
(`internal/channel/ussd.go`, `internal/httpapi/ussd.go`,
`internal/provider/{ebulksms,failover}.go`).

> **ASSUMPTIONS (no live keys this wave).** Every aggregator HTTP shape
> below — the eBulksSMS request/response, and the USSD callback field set
> beyond contract §1 — is coded from public docs and annotated in source.
> Verify against live accounts before production traffic.

## USSD session contract (SPEC-W12 §1)

Africa's Talking invokes the gateway once per subscriber interaction:

```
POST /webhooks/ussd
Content-Type: application/x-www-form-urlencoded

sessionId=…&serviceCode=*384*123%23&phoneNumber=%2B2348012345678&text=1*2
```

| Field | Meaning |
|---|---|
| `sessionId` | Aggregator session id; the gateway's state key. |
| `serviceCode` | The dialed code (e.g. `*384*123#`); routes to the tenant via `CHANNEL_SITE_MAP` key `ussd:<serviceCode>`. |
| `phoneNumber` | Subscriber MSISDN (E.164). |
| `text` | **Cumulative** input for the session (`1*2*3`); **empty on the first request**. The last `*`-separated segment is the current selection. |

The response is `text/plain` and **must** start with `CON ` (session
continues) or `END ` (session terminates):

```
CON Welcome:
1. Book appointment
2. Talk to an agent
```

Session state TTL is **180s**. A callback for a missing/expired/unknown
`sessionId` starts a new session (the menu renders again); `END` deletes
the state. `400` is only returned for a garbage form (missing
`sessionId`/`serviceCode`/`phoneNumber`); internal failures are logged and
answered `END Service unavailable. Please try again later.` so the
subscriber is never stuck.

## Request/reply contract with conversation-service (DELIVERED by Agent D)

Every callback is forwarded **synchronously** to conversation-service over
the *same Dapr invoke path the other inbound channels use*
(`CONVERSATION_URL` override, else
`http://127.0.0.1:{DAPR_HTTP_PORT}/v1.0/invoke/conversation-service/method`):

```
POST {conversation-base}/v1/ussd/turns
{ "tenant_id": "<uuid>", "site_slug": "...", "session_id": "...",
  "service_code": "*384*123#", "phone_number": "+234…",
  "text": "<cumulative 1*2*3 as received, '' on first request>",
  "menu": [{"key","label","action"}, …] | omitted }
→ 200 { "conversation_id": "<uuid5(tenant, session_id)>",
        "reply": "<text>", "continue": true|false,
        "mode": "menu"|"text", "selection": "<last selection>",
        "action": "<selected menu action|''>" }
```

Field set mirrors conversation-service `app/ussd.py` `UssdTurnRequest`
exactly — keep both in sync when evolving the contract. conversation-service
maps `session_id` to the deterministic conversation key
`uuid5(tenant, session_id)`, appends the user turn (channel `ussd`,
incident classifier unchanged, ussd treated web-like), and returns the
reply text in the invoke response body. **messaging-gateway renders the
wire format**: `CON <reply>` when `continue` is true, `END <reply>`
otherwise. Empty `reply` or any invoke error → the fallback END line
above; `END` also deletes the session state.

## Menu mode vs pass-through text mode

On a new session the gateway fetches the tenant payload from the identity
packs summary endpoint (`IDENTITY_URL` override, else Dapr invoke app-id
`identity`): `GET /v1/tenants/{slug}` and reads `pack.ussd.menu` —
`[{key,label,action}]` — cached in the session and attached to every turn.

- **Menu present** → conversation-service drives the low-literacy numeric
  menu state machine (`1`/`2`/… select by item `key`, `0` = back,
  `00` = main menu, invalid input re-renders the menu; single-level menus
  end the session on selection).
- **No menu (or menu fetch fails)** → pass-through text mode: the raw last
  selection becomes the user turn; conversation-service answers with the
  configurable text-mode acknowledgement (`continue: true` — the gateway's
  180s session TTL bounds the conversation).

> **KNOWN GAP (flagged):** identity-service's pack `Summary` does not yet
> pass a pack `ussd:` block through `GET /v1/tenants/{slug}` — so today
> every tenant resolves to pass-through mode. The gateway parses
> `pack.ussd.menu` as soon as identity forwards it; no gateway change
> needed. ASSUMPTION: the `CHANNEL_SITE_MAP` `site_slug` doubles as the
> identity tenant slug (single-site NG tenants).

## Session store

- `USSD_SESSION_BACKEND=memory` (default): in-process map with lazy 180s
  TTL expiry. Fits the gateway's single-replica deployment model (same
  assumption as the in-process metrics registry).
- `USSD_SESSION_BACKEND=dapr`: Dapr state store via the raw net/http API
  (`/v1.0/state/{store}`, `ttlInSeconds` metadata, `ussd:<sessionId>` keys)
  — the same pattern as `daprc` in booking/identity/notification.
  `USSD_STATE_STORE` selects the component (default `statestore`).
  **INFRA FLAG:** `infra/dapr/components/statestore.redis.yaml` scopes do
  not include the `messaging-gateway` app-id yet (Agent D's territory).

## SMS aggregator failover chain

`POST /v1/sms/send` `{to, message, sender_id?}` sends through the ordered
chain from `SMS_PROVIDER_CHAIN` (default `africastalking,termii,ebulksms`):

- Provider 5xx / timeout (after the shared client's 2 retries) → next
  provider. Provider 4xx is a caller fault → returned immediately (400),
  no failover.
- Per-provider **circuit breaker** mirroring the voice runtime's
  `CircuitBreaker`: 3 consecutive failures open the circuit for 60s, then
  one probe; a failed probe re-opens.
- Per-provider **price-tier annotation** (relative, reporting only —
  ASSUMPTION): `africastalking=1.0`, `termii=1.0`, `ebulksms=0.85`. Logged
  at startup; never used for routing.
- Metering follows the existing counter: each provider's client records
  `messaging_gateway_sends_total{provider,result}` — the `sms_send` event
  with the `provider` label, unchanged.
- Response names the winning provider:
  `200 {"provider":"termii","provider_status":200,"provider_body":"…"}`;
  `502 {"error":"all sms providers failed"}` on chain exhaustion.

### Provider request shapes (ASSUMPTIONS — verify with live keys)

| Provider | Request |
|---|---|
| Africa's Talking | `POST {base}/version1/messaging`, form `username,to,message,from`, header `apiKey` (existing, unchanged) |
| Termii | `POST {base}/api/sms/send` `{api_key,to,from,sms,type:"plain",channel:"generic"}` (existing, unchanged) |
| eBulksSMS | `POST {base}/sendsms` `{username,apikey,sender,messagetext,flash:0,recipients}` (new) |

## Configuration

| Env | Default | Purpose |
|---|---|---|
| `SMS_PROVIDER_CHAIN` | `africastalking,termii,ebulksms` | Ordered failover chain |
| `AT_API_KEY` / `AT_USERNAME` / `AT_FROM` / `AT_BASE_URL` | — | Africa's Talking |
| `TERMII_API_KEY` / `TERMII_SENDER_ID` / `TERMII_BASE_URL` | — | Termii |
| `EBULK_API_KEY` / `EBULK_USERNAME` / `EBULK_SENDER` / `EBULK_BASE_URL` | `https://api.ebulksms.com` | eBulksSMS |
| `IDENTITY_URL` | Dapr invoke `identity` | Menu fetch base override |
| `CONVERSATION_URL` | Dapr invoke `conversation-service` | USSD turn base override |
| `USSD_SESSION_BACKEND` | `memory` | `memory` \| `dapr` |
| `USSD_STATE_STORE` | `statestore` | Dapr component (backend=dapr) |
| `USSD_SESSION_TTL_SECONDS` | `180` | Contract §1 TTL |

Tenant routing reuses `CHANNEL_SITE_MAP` with the route key
`ussd:<serviceCode>`:

```json
{"ussd:*384*123#": {"site_slug": "acme-ng", "tenant_id": "<uuid>"}}
```

## Ops notes

- Register the callback URL `https://<gateway>/webhooks/ussd` on the AT
  USSD channel (shared shortcode) or via the aggregator's callback
  configuration. It sits behind the same public APISIX `/webhooks/*`
  route as the WhatsApp/Telegram webhooks; AT callbacks carry no shared
  secret, so tenancy is established by the `serviceCode` site-map entry.
- Aggregator callbacks time out in a few seconds: the handler is bounded by
  the shared 25s webhook context but conversation-service should answer in
  ~1–2s; on slow paths the subscriber sees the fallback END line.
- USSD is not idempotent at the aggregator: a retried callback for a live
  session simply replays the current step (cumulative `text` makes the
  last-selection parse deterministic).
