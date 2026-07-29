# Incidents & Emergency Lane Runbook (Wave 11)

Emergency-grade communication: Incident Data Packets (IDP), the incidents
REST API + signed dispatch, IoT webhook ingest, the voice emergency lane,
spoken-AI disclosure packs, and widget GPS consent. Implements the backlog
from `docs/ECALL-ANALYSIS.md` (SPEC-W11).

---

## 1. Incident Data Packet (IDP)

Canonical schema: `docs/schemas/incident-data-packet.json` (JSON Schema
draft 2020-12). Every emergency-flavored interaction produces one IDP:

```json
{
  "incident_id": "uuid",
  "schema_version": "1.0",
  "tenant_id": "uuid",
  "captured_at": "rfc3339",
  "channel": "voice|whatsapp|telegram|web|sms|webhook",
  "location": {"lat": 0.0, "lng": 0.0, "accuracy_m": 0,
               "source": "gps|address|caller_id|manual", "address_text": ""} | null,
  "callback_number": "+234…|null",
  "incident_type": "crime|medical|fire|crash|utility_fault|security|other",
  "severity": "critical|high|medium|low",
  "people_involved": 0,
  "hazards": ["weapons", "injuries", "fire", "gas", "traffic"],
  "narrative_summary": "≤500 chars",
  "reference_number": "INC-2026-000123",
  "contact_id": "uuid|null",
  "conversation_id": "uuid|null"
}
```

Usage:
- **Emitted by** conversation-service (`app/incidents.py`) when a user turn
  classifies as emergency (lexicon-first EN + PCM scorer, threshold
  `INCIDENT_MIN_SCORE`). Emission is a CloudEvent
  `com.opendesk.incidents.IDPCreated` on Kafka topic `opendesk.incidents`,
  non-blocking, idempotent per conversation+turn; failures are logged,
  never raised.
- **Reference numbers**: `INC-{YYYY}-{seq:06d}` per tenant.
- **Consumed by** booking-service (consumer group `booking-incidents`),
  which persists the full IDP into `incidents.payload` (idempotent on
  `incident_id`) and triggers auto-dispatch + outreach.
- Validate any hand-built IDP against the schema before sending; unknown
  additive keys are tolerated downstream.

## 2. Incidents REST API (booking-service)

All endpoints use the standard platform auth (RLS-scoped tenant context;
management endpoints require the `manage_bookings` role).

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/incidents?status=&from=&to=` | Admin list (filter by status / time range) |
| `GET` | `/v1/incidents/{id}` | Fetch one incident (full IDP payload) |
| `POST` | `/v1/incidents/{id}/dispatch` | Deliver the IDP to all active tenant dispatch endpoints |
| `POST` | `/v1/incidents/dispatch-endpoints` | Register a dispatch endpoint `{url, secret}` |
| `GET` | `/v1/incidents/dispatch-endpoints` | List tenant dispatch endpoints |
| `DELETE` | `/v1/incidents/dispatch-endpoints` | Remove a dispatch endpoint |
| `POST` | `/v1/incidents/ingest` | Internal (Dapr-invoked by messaging-gateway): construct + persist a full IDP from a webhook partial, then auto-dispatch |

Incident statuses: `new → dispatched → acknowledged → closed`.

### Signed dispatch delivery

Each dispatch POSTs the IDP JSON to every active tenant endpoint with:

```
X-OpenDesk-Signature: hex(HMAC-SHA256(secret, raw_body))
X-OpenDesk-Incident: {incident_id}
```

A per-endpoint delivery ledger row (`incident_deliveries`) records status,
attempts, last status code and last error; retries reuse the outbound
webhook Temporal workflow (payload type `incident`). Auto-dispatch on
incident creation is on by default (`INCIDENT_AUTO_DISPATCH=true`).

Receiver-side verification sample (Python):

```python
import hashlib
import hmac
import json

def verify_opendesk_incident(secret: str, raw_body: bytes, headers) -> dict:
    """Verify a signed OpenDesk incident dispatch and return the IDP."""
    expected = hmac.new(secret.encode(), raw_body, hashlib.sha256).hexdigest()
    presented = headers.get("X-OpenDesk-Signature", "")
    if not hmac.compare_digest(expected, presented):
        raise PermissionError("bad incident signature")
    idp = json.loads(raw_body)
    incident_id = headers.get("X-OpenDesk-Incident")
    assert idp.get("incident_id") == incident_id, "incident id mismatch"
    return idp
```

### Auto-outreach

When an incident carries `callback_number`/`contact_id` and severity is
`critical` or `high`, notification-worker sends a paced `incident_alert`
(sms/whatsapp per tenant channel routing). `incident_alert` uses the
pacer's **priority fast-lane** (`priority: true` on the paced request): it
bypasses the CPS token bucket with direct dispatch while staying metered.

## 3. IoT / webhook ingest (messaging-gateway)

`POST /webhooks/incidents` — public route covered by the existing APISIX
`/webhooks/*` policy; Dapr http input binding
`infra/dapr/components/bindings.incident-webhook.yaml` points here.

Request body:

```json
{
  "tenant_slug": "acme-city",        // or "tenant_id": "uuid"
  "secret": "per-tenant shared secret",
  "incident": {
    "incident_type": "utility_fault",
    "severity": "high",
    "location": {"lat": 6.5244, "lng": 3.3792, "accuracy_m": 12,
                 "source": "gps", "address_text": ""},
    "callback_number": "+2348012345678",
    "narrative_summary": "Smart-meter reports gas leak alarm, building 4",
    "hazards": ["gas"]
  }
}
```

- `incident` is an IDP-ish partial: the gateway Dapr-invokes booking-service
  `POST /v1/incidents/ingest`, which constructs the full IDP
  (`channel: "webhook"`, fresh `incident_id`, `captured_at`,
  `schema_version: "1.0"`), persists it, and triggers auto-dispatch +
  outreach exactly like a conversational incident.
- **Secret**: per-tenant shared secrets live in the
  `INCIDENT_WEBHOOK_SECRETS` env var (JSON map of tenant slug/id → secret).
- Responses: always `200` on valid JSON (ingest is asynchronous); `400` on
  unparseable garbage.

## 4. Emergency lane (voice runtime)

- `app/emergency.py` detects emergencies on live user turns (EN + PCM
  lexicon — deliberately mirrors the conversation-service classifier; same
  class list) → `is_emergency(text) -> (bool, severity, hazards)` with a
  per-session `EmergencyState`.
- On a hit, livekit_worker (surgically):
  1. sets `session.emergency = True`,
  2. emits metric `voice_emergency_sessions_total`,
  3. triggers the existing warm-handoff escalation path immediately with
     reason `"emergency"` (prewarmed agents are already shared, so
     first-claim is inherent),
  4. injects a **location-first prompt addendum**: the agent confirms the
     exact location FIRST before anything else.
- `SessionMetrics.emergency: bool` flows into the session-ended quality
  event payload (additive JSON key).
- Tool `capture_location(address_text, lat?, lng?)` resolves the caller's
  contact (SIP caller-ID / conversation contact_phone) and Dapr-invokes
  booking-service `PUT /v1/contacts/{id}/location` (Wave-8 contract);
  returns an ack and never raises.

## 5. Disclosure packs (spoken AI / recording consent)

Packs may carry an optional `disclosure` block (schema enforced by both
`scripts/validate_pack.py` and identity-service `internal/packs/packs.go`):

```yaml
disclosure:
  spokenAiDisclosure: true   # required bool when the block is present
  recordingConsent: true     # required bool when the block is present
  text: "You are speaking with an automated police non-emergency assistant. This call may be recorded."  # optional, ≤200 chars
```

Shipped in Wave 11 on `law-enforcement`, `healthcare` and `civic-services`.
The block passes through the tenant Summary JSON as camelCase
`ctx.disclosure` (`{"spokenAiDisclosure","recordingConsent","text"}`); the
voice runtime prepends the automated-assistant line (+ optional pack text)
to the greeting when `spokenAiDisclosure` is set and appends a recording
notice when `recordingConsent` is set. When the block is absent, behavior
is byte-identical to before (no disclosure). After editing a pack,
re-register it: `scripts/validate_pack.py upsert-index industries/index.json
industries/<id>.yaml --version <v> --author opendesk`, then
`validate-index` must pass with 31 entries.

## 6. Widget GPS consent (embed.js)

Opt-in per embed via a script-tag attribute:

```html
<script src="https://<your-opendesk-host>/embed.js"
        data-site="acme" data-location-consent="true" async></script>
```

Behavior (implemented in `apps/admin-web/public/embed.js` +
`apps/admin-web/app/embed/ui-actions-bridge.tsx`):

1. After the widget iframe loads, the host page requests
   `navigator.geolocation.getCurrentPosition` **once** (10s timeout, 5min
   maximumAge).
2. Denial, timeout or missing geolocation support are silent — chat is
   never blocked and nothing is sent. Without
   `data-location-consent="true"` no permission prompt ever appears.
3. The fix is postMessaged into the iframe as
   `{type: "opendesk:location", location: {lat, lng, accuracy}}` (the
   bridge accepts it only from the direct parent frame, origin-checked
   against the referrer).
4. The bridge's fetch tap merges it as an additive `client_location`
   `{lat, lng, accuracy}` key into the JSON body of every `/voice/chat`
   request the widget originates. The server tolerates unknown keys;
   payloads without a fix pass through byte-identical.
