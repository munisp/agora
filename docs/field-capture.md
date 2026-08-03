# Field Capture API (offline queue) — SPEC-W16

Server half of the field PWA / mobile offline queue (contract §4), plus the
push device-token registry (contract §1) that the same clients use after the
push-permission grant. Both live in `booking-service`:

- `internal/fieldcapture/` — POST `/v1/field/capture` (this document)
- `internal/devices/` — `/v1/devices` + GET `/internal/devices` (see §6)

BFF path prefix (APISIX): `/api/bookings`.

## 1. Offline-queue contract (contract §4)

The field client keeps an IndexedDB outbox of items captured while offline:

```json
{
  "id": "client-side row id",
  "kind": "lead_capture | checkin",
  "payload": { "…": "kind-specific" },
  "captured_at": "2025-01-15T10:30:00Z",
  "gps": { "lat": 6.5244, "lng": 3.3792, "accuracy": 12.5 }
}
```

`gps` is nullable (user denied the location prompt, or fix unavailable).
The client flushes the outbox on the `online` event and via Background Sync
where available, batching up to **100 items** per request
(`FIELD_CAPTURE_BATCH_LIMIT`).

### POST /v1/field/capture

```json
{
  "items": [
    {
      "client_id": "0f3d6c2e-…-uuid",
      "kind": "lead_capture",
      "payload": { "phone_e164": "+2348011111111", "name": "Ada", "notes": "met at stall" },
      "captured_at": "2025-01-15T10:30:00Z",
      "gps": { "lat": 6.5244, "lng": 3.3792, "accuracy": 12.5 }
    },
    {
      "client_id": "7aa1…-uuid",
      "kind": "checkin",
      "payload": { "contact_id": "b2…-uuid", "note": "site visit" },
      "captured_at": "2025-01-15T11:05:00Z",
      "gps": null
    }
  ]
}
```

Response is **always 200** when the batch itself is processable — per-item
outcomes ride in `results` (same order as `items`):

```json
{
  "results": [
    { "client_id": "0f3d…", "kind": "lead_capture", "status": "applied", "lead_id": "9c…" },
    { "client_id": "7aa1…", "kind": "checkin",      "status": "deduped", "checkin_id": "e4…" },
    { "client_id": "…",     "kind": "lead_capture", "status": "error",   "error": "lead_capture payload: phone_e164 is required" }
  ]
}
```

| status | meaning | client action |
|---|---|---|
| `applied` | side effect executed now | drop from outbox |
| `deduped` | this `client_id` was already applied; the ORIGINAL outcome (`lead_id` / `checkin_id` / `error`) is returned | drop from outbox, reconcile local ids |
| `error` | deterministic validation failure | drop after surfacing to the user — retrying unchanged will fail the same way |

## 2. Idempotency

- `client_id` is a **client-generated UUID**, minted when the item enters the
  outbox and never changed across flushes. The logical dedupe key is
  `field_capture:{client_id}` (contract §4); physically it is the
  `field_captures (tenant_id, client_id)` primary key.
- First application inserts the anchor row (`status='processing'`), applies
  the side effect, then records the outcome (`applied|error` + result JSON).
- A replay hits the anchor and returns the stored outcome — **no side effect
  is re-executed**.
- Transient apply failures (DB errors) RELEASE the anchor, so the next flush
  retries cleanly. An anchor observed stuck in `processing` (server crashed
  mid-apply) dedupes with an explicit "previous attempt incomplete" error —
  resubmit with a **new** `client_id` to force re-application.
- `kind=lead_capture` stacks on top of the W13 leads **24h first-touch
  dedupe** (`sha256(tenant|phone|channel|YYYY-MM-DD)`, channel `field`):
  the same phone captured twice in a day under different `client_id`s
  produces ONE lead row; the second item is `applied` with the existing
  lead's id.

## 3. Kinds & payloads

### lead_capture → `booking.leads` (channel `field`)

| payload field | required | notes |
|---|---|---|
| `phone_e164` | ✔ | E.164; only field with a leads column besides attribution |
| `name`, `notes` | — | preserved verbatim on the `field_captures.payload` anchor row (no leads columns today — no data loss, a CRM hook can consume them later) |
| `utm`, `campaign_id`, `lga_id`, `consent_id` | — | forwarded to the leads service (first-touch attribution) |

The created lead emits the standard `lead_created` FunnelEvent onto
`cac.events` via the outbox (field leads appear in the CAC funnel).

### checkin → `booking.field_checkins`

| payload field | required | notes |
|---|---|---|
| `contact_id` | — | contact being visited; nullable |
| `note` | — | free text |

The GPS fix rides on the **item** (`gps`), not the payload; `lat/lng/accuracy`
land on the `field_checkins` row (nullable). **Location-store finding:** the
W8 store (`internal/store/geolocations.go`, `contact_locations`) is a
last-known-position UPSERT keyed `(tenant_id, contact_id)` and exposes **no
history** — so per the spec's fallback clause, check-in history persists in
the new `field_checkins` table (RLS-enabled, tenant-scoped).

## 4. Auth & permissions

- Tenant resolution: standard `X-Tenant-Slug` (+ JWT `tenant_slugs` claim)
  middleware.
- Permission: **`manage_bookings`** (Permify: owner/admin/member=staff).
  Rationale — least-privileged *existing* fit: field capture creates leads and
  check-ins, the same write class as `POST /v1/leads` and
  `PUT /v1/contacts/{id}/location` (both `manage_bookings`); the only lighter
  existing permissions (`view_dashboard`, `view_analytics`) are read scopes,
  and the Permify schema has no field-specific write permission. No new
  permission was invented per the spec's "least-privileged existing fit".

## 5. Errors

- `400` — malformed JSON body, empty `items`, batch over the limit.
- `401/403` — standard middleware (no subject / missing `manage_bookings`).
- `503` — field-capture store not configured (dial failure at boot; the rest
  of booking-service keeps running).
- Item-level failures never change the HTTP status; see the status table in §1.

## 6. Device tokens (contract §1) — client quick reference

Full contract in `docs/push-notifications.md` (Agent A). Client surface:

```
POST   /v1/devices              manage_bookings   {"token","platform":"android|ios|web","app":"admin|field","contact_id?"}
DELETE /v1/devices/{token}      manage_bookings   unregister (URL-encode the path segment;
                                 ?token= fallback exists for web-push endpoint URLs containing '/')
GET    /v1/devices?platform=&app=  view_analytics  device inventory
GET    /internal/devices?contact_id=   service-to-service (X-Tenant-Slug, Dapr invoke
                                 from notification-worker) → bare JSON array of
                                 device tokens, [] when none — shape frozen for Agent A
```

Register is an upsert on `(tenant_id, token)`: re-registration (token
refresh, app restart) returns `200 {"created":false}` and refreshes
`contact_id`/`platform`/`app`/`last_seen_at`; first registration returns
`201 {"created":true}`.

## 7. Example flush (curl)

```bash
curl -X POST https://api.opendesk.example/api/bookings/v1/field/capture \
  -H 'Authorization: Bearer …' -H 'X-Tenant-Slug: acme' \
  -H 'Content-Type: application/json' \
  -d '{"items":[{"client_id":"0f3d6c2e-7f3a-4b6c-9d1e-2a4c6e8f0a1b",
                 "kind":"lead_capture",
                 "payload":{"phone_e164":"+2348011111111","name":"Ada"},
                 "captured_at":"2025-01-15T10:30:00Z",
                 "gps":{"lat":6.5244,"lng":3.3792,"accuracy":12.5}}]}'
```
