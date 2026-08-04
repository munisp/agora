# Field Service (work orders & dispatch) — SPEC-W19 Agent B

Enterprise app `field-service`: work orders with a validated status machine,
team-member dispatch with paced push notification, a dispatch board and a
today view. Backend: booking-service `internal/workorders/` (self-contained
package, anti-collision contract). UI: admin-web `/app/{orgSlug}/apps/field-service`.

| Area | Artifact |
| --- | --- |
| Model + state machine + validation | `services/booking-service/internal/workorders/workorders.go` |
| Store (RLS, board/today, auto-assign, outbox) | `services/booking-service/internal/workorders/store.go` |
| Handlers + RegisterRoutes + tenant context | `services/booking-service/internal/workorders/handlers.go` |
| CloudEvents / metering / push envelope | `services/booking-service/internal/workorders/events.go` |
| Tests (embedded-postgres, RLS, matrix) | `internal/workorders/*_test.go` |
| Admin UI | `apps/admin-web/app/app/[orgSlug]/apps/field-service/`, `apps/admin-web/components/apps/field-service/` |

## Entitlement

- **app_id: `field-service`** — the INTEGRATOR gates the route group via
  `internal/appgate` (W18) and wires it into the identity app catalog.

## Data model

`work_orders` (RLS: `ENABLE` + `FORCE ROW LEVEL SECURITY`, `tenant_isolation`
policy on `app.tenant_id`, bootstrap DDL idempotent + `pg_policies`-guarded,
mirroring `internal/devices/store.go`):

| Column | Type | Notes |
| --- | --- | --- |
| `id` | uuid PK `gen_random_uuid()` | |
| `tenant_id` | uuid NOT NULL | RLS key |
| `contact_id` | uuid NULL | customer being served |
| `booking_id` | uuid NULL | originating booking |
| `title` / `description` | text | title required (≤300 B), description ≤8000 B |
| `status` | text CHECK | `created\|assigned\|en_route\|on_site\|completed\|cancelled` |
| `assignee_id` | uuid NULL | references `team_members.id` |
| `scheduled_start` / `scheduled_end` | timestamptz NULL | end ≥ start |
| `gps_lat` / `gps_lng` / `gps_accuracy` | double NULL | lat/lng set together, W16 bounds |
| `checklist` | jsonb DEFAULT `[]` | `[{label, done}]`, ≤100 items |
| `proof` | jsonb DEFAULT `{}` | `{notes, photos[]}` |
| `field_capture_id` | text NULL | links the W16 `field_captures.client_id` anchor |
| `created_at` / `updated_at` / `completed_at` | timestamptz | |

### State machine

```
created → assigned → en_route → on_site → completed
   any non-terminal state → cancelled
assigned → assigned   (re-dispatch edge, dispatch endpoint only)
```

Terminal states (`completed`, `cancelled`) have no outgoing edges. Illegal
transitions → **409**. Completion gate (also **409**): every checklist item
`done` (an empty checklist passes vacuously) **and** non-empty
`proof.notes`. `completed_at` is stamped at the transition.

## Endpoints

Base: booking-service, `/v1/field-service` (tenant via `X-Tenant-Slug`).
AuthZ (integrator wires, contract §3): reads → `view_analytics`, writes →
`manage_bookings`. RegisterRoutes applies the variadic middleware
group-wide — recommended integrator shape: method-aware composition of the
existing `require()` (GET/HEAD → `view_analytics`, else `manage_bookings`).

```bash
# Create (201)
curl -X POST $API/v1/field-service/work-orders -H "X-Tenant-Slug: acme" -H "Authorization: Bearer $JWT" -d '{
  "title": "Fix AC unit — Lekki office",
  "description": "Compressor suspected",
  "contact_id": null, "booking_id": null,
  "scheduled_start": "2030-01-02T09:00:00Z",
  "gps": {"lat": 6.5244, "lng": 3.3792, "accuracy": 12},
  "checklist": [{"label": "Inspect unit", "done": false}],
  "field_capture_id": "9f2b4d2a-…"
}'

# List (200) — filters: status, assignee, q (title/description ILIKE),
# from/to (RFC3339 over scheduled_start)
curl "$API/v1/field-service/work-orders?status=assigned&assignee=<tm-uuid>&q=ac" -H "X-Tenant-Slug: acme" …
# → {"work_orders": [...]}

# Get (200) → {"work_order": {...}}
curl $API/v1/field-service/work-orders/{id} -H "X-Tenant-Slug: acme" …

# Patch (200) — partial; status changes run the state machine + completion
# gate. gps:null clears the fix. Setting assignee_id on a created order
# dispatches it without notification.
curl -X PATCH $API/v1/field-service/work-orders/{id} … -d '{
  "status": "completed",
  "checklist": [{"label": "Inspect unit", "done": true}],
  "proof": {"notes": "Compressor replaced, cooling verified"}
}'

# Dispatch (200) — assignee_id is a team_members uuid or "auto"
# (least-open-orders active member, mirrors the helpdesk auto rule;
# 409 when no active member). notify=true enqueues the paced push (below).
curl -X POST $API/v1/field-service/work-orders/{id}/dispatch … -d '{"assignee_id":"auto","notify":true}'
# → {"work_order": {...}, "notified": true}

# Board (200) — grouped by status with assignee names; ALWAYS all six keys.
curl $API/v1/field-service/board -H "X-Tenant-Slug: acme" …
# → {"board": {"created": [...], "assigned": [{...wo, "assignee_name":"Ada"}], …}}

# Today (200) — scheduled_start within the tenant-local day (tenant tz);
# optional ?assignee=<uuid>
curl "$API/v1/field-service/today?assignee=<tm-uuid>" -H "X-Tenant-Slug: acme" …
# → {"work_orders": [...], "day_start": "…", "day_end": "…"}
```

Errors: `400` invalid input (body carries `{"error": …}`), `404`
missing/cross-tenant, `409` transition/gate/no-assignee violations.

## Events (CloudEvents, outbox)

Lifecycle events on topic **`opendesk.fsm.events.v1`** (contract §5),
published post-commit via the transactional outbox (best-effort, mirrors
`internal/referrals/metering.go`):

- `com.opendesk.fsm.WorkOrderAssigned` — `data: {tenant_id, work_order_id,
  title, assignee_id, scheduled_start?, ts}` (dispatch AND re-dispatch).
- `com.opendesk.fsm.WorkOrderCompleted` — `data: {tenant_id, work_order_id,
  title, assignee_id?, completed_at}`.

Empty `WORKORDERS_FSM_EVENTS_TOPIC` → graceful no-op.

## Metering

One usage record per completion on **`opendesk.usage.events`** (contract §4),
type `com.opendesk.usage.UsageRecord`:

```json
{"tenant_id": "…", "metric": "workorder_completed", "value": 1, "ts": "…",
 "meta": {"work_order_id": "…", "assignee_id": "…", "checklist_items": 3}}
```

Empty `WORKORDERS_USAGE_TOPIC` → metering disabled. Emitted only on the
actual →completed transition, so replays cannot double-meter.

## Dispatch push notification (W16 contract)

`notify=true` on dispatch enqueues a CloudEvent to the notifications outbox
topic (**`opendesk.notifications.outbox`**, env `WORKORDERS_NOTIFICATIONS_TOPIC`;
empty → `notified:false`, dispatch still succeeds — topic-gated graceful).
The envelope mirrors the W16 push contract (notification-worker
`internal/workflows/paced.go` — shape duplicated per service boundary):

```jsonc
// CloudEvent
{
  "type": "com.opendesk.notifications.PacedSend",
  "source": "booking-service", "subject": "acme", "tenantid": "…",
  "data": {                       // ← PacedSendRequest (paced.go)
    "kind": "push_notification",  // TRANSACTIONAL: never DND-suppressed, never quiet-hours deferred
    "push": {                     // ← PacedPushNotificationSend (paced.go)
      "tenant_slug": "acme",
      "contact_id": "<assignee team_members.id>",
      "title": "Work order dispatched",
      "body": "You have a new work order: Fix AC unit (scheduled 2030-01-02 09:00 UTC)",
      "app": "field",             // device fetch restricted to the field app
      "data": {"kind": "dispatch", "work_order_id": "…", "assignee_name": "Ada", "work_order_ref": "Fix AC unit"}
    }
  }
}
```

**ASSUMPTIONS (explicit):**

1. `PacedPushNotificationSend.contact_id` resolves device tokens via
   booking-service `GET /internal/devices?contact_id=` — dispatch passes
   the assignee **team member id** as `contact_id`, so staff devices must
   register with `device_tokens.contact_id = team_members.id` (app
   `"field"`) for the fan-out to reach them. No team_members↔contacts
   link exists in the schema; this is the zero-migration mapping.
2. The notification-worker's `notifyoutbox` consumer handles
   `com.opendesk.notifications.SendPortalCode`; unknown types are
   acknowledged (forward-compatible, never error). **Consumer-side
   `PacedSend` delivery LANDED with the W19 integrator:** the consumer now
   has a `com.opendesk.notifications.PacedSend` case that unmarshals the
   CloudEvent data into the W16 `PacedSendRequest` shape and starts one
   `PacedSendWorkflow` per command (workflow id `paced-send-<cloudevent
   id>`, already-started tolerated on redelivery), which runs the send
   through the `NotifyPaced` activity on the notification task queue —
   dispatch push is delivered end-to-end.

## Config envs (integrator wires; app functional with zero config)

| Env | Default | Meaning |
| --- | --- | --- |
| `WORKORDERS_NOTIFICATIONS_TOPIC` | `opendesk.notifications.outbox` | dispatch push target topic; empty disables notify |
| `WORKORDERS_USAGE_TOPIC` | `opendesk.usage.events` | metering topic; empty disables metering |
| `WORKORDERS_FSM_EVENTS_TOPIC` | `opendesk.fsm.events.v1` | lifecycle events topic; empty disables events |
| `DATABASE_URL` | (existing) | `workorders.DialStore` opens a small pool (maxConns 4) |

Wiring (integrator): `workorders.NewStore(ctx, pool)` or
`workorders.DialStore(ctx, databaseURL)`, then on the ROOT router
`workorders.RegisterRoutes(r, &workorders.Deps{Store, Resolver, Logger,
NotificationsTopic, UsageTopic, FSMEventsTopic}, appgateMW, permMW…)` —
routes mount at `/v1/field-service/*` (the package resolves the tenant
itself from `X-Tenant-Slug` via `Deps.Resolver`; httpapi's tenant-middleware
context key is unexported, hence the package-local middleware).

**As delivered (W19 integrator):** the variadic chain is
`[httpapi tenantMiddleware, appgate("field-service"), method-aware perms
(GET/HEAD → view_analytics, else manage_bookings)]` — the package's own
tenant middleware still runs FIRST (RegisterRoutes contract), then
httpapi's tenantMiddleware populates the ctx values the appgate slug
extractor and the Permify `require` middleware read. `Resolver` is wired
to `bookingops.TenantResolver` (cache-backed, so the double resolution is
cheap). Store dialed via `workorders.DialStore(ctx, cfg.DatabaseURL)`.

## Admin UI

`/app/{orgSlug}/apps/field-service` — role-guarded server page (view:
owner/admin/staff/analyst; writes `canWrite`: owner/admin/staff, mirroring
manage_bookings). Tabs:

- **Dispatch board** — six status lanes with assignee names, scheduled
  times and checklist progress; a card opens the detail panel.
- **Today** — the tenant-local day's scheduled orders (table).

Detail panel: status-flow buttons (only legal transitions render; Complete
stays disabled with an explanation until the checklist is all-done and
proof notes exist — the server re-checks authoritatively), dispatch /
re-dispatch control (`auto` or a team-member id, push-notify toggle),
checklist editor (toggle/add/remove), GPS display, proof-notes editor.
BFF: browser → `/api/bookings/v1/field-service/...` → gateway →
booking-service (the generic `/api/[[...path]]` proxy attaches the Keycloak
token and `x-tenant-slug`; no BFF code added).

## Limitations / honest notes

- Dispatch push delivery needs the notifyoutbox `PacedSend` consumer case
  (ASSUMPTION 2 above); enqueue side is complete and tested.
- `/today` is computed in the tenant timezone; orders without
  `scheduled_start` never appear there (they are on the board instead).
- `proof.photos` are stored verbatim (URLs/keys); upload flows belong to
  the W16 field-capture path, not this package.
- Auto-dispatch ignores scheduled-time overlap and skills/roles — it is a
  pure least-open-orders rule (documented; matches the spec's auto clause).
