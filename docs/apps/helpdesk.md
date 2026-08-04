# Helpdesk — SLA Ticketing (SPEC-W19 Agent A)

Enterprise app `helpdesk`: multi-channel support tickets with per-priority
SLA policies, first-response / resolve clocks, an event timeline,
least-open-tickets auto-assignment, CSAT capture, usage metering and
CloudEvents lifecycle emission.

- Backend: `services/booking-service/internal/helpdesk/` (self-contained
  package per the W19 anti-collision contract; exposes `RegisterRoutes` +
  `NewStore`/`DialStore`)
- UI: `apps/admin-web/app/app/[orgSlug]/apps/helpdesk/` + shared components
  in `apps/admin-web/components/apps/helpdesk/`
- Entitlement (integrator): appgate `app_id = "helpdesk"` on the whole
  `/v1/helpdesk` route group; perms — `view_analytics` for GET,
  `manage_bookings` for POST/PATCH

---

## 1. Data model

All tables are tenant-scoped with `ENABLE` + `FORCE ROW LEVEL SECURITY` and
the `tenant_isolation` policy (`tenant_id = current_setting('app.tenant_id')`),
bootstrapped idempotently by `Store.ensureSchema` (devices idiom). Every
tenant query runs inside `withTenant` (`SET LOCAL app.tenant_id`).

### sla_policies

| Column | Type | Notes |
| --- | --- | --- |
| `id` | uuid PK | `gen_random_uuid()` |
| `tenant_id` | uuid | |
| `name` | text | required |
| `priority` | text | `low \| normal \| high \| urgent` (one policy per tier) |
| `first_response_minutes` | int | > 0 |
| `resolve_minutes` | int | > 0 |
| `active` | bool | default true; only active policies auto-attach |
| `created_at` / `updated_at` | timestamptz | |

### tickets

| Column | Type | Notes |
| --- | --- | --- |
| `id` | uuid PK | |
| `tenant_id` | uuid | |
| `contact_id` / `conversation_id` | uuid null | optional links |
| `subject` | text | required (≤ 500 bytes) |
| `channel` | text | default `web` (free-form, ≤ 100 bytes, lower-cased) |
| `priority` | text | default `normal` |
| `status` | text | `open \| pending \| resolved \| closed`, default `open` |
| `assignee_id` | uuid null | team_members id |
| `sla_policy_id` | uuid null | effective policy |
| `due_first_response_at` / `due_resolve_at` | timestamptz null | computed from policy, clock base `created_at` |
| `first_response_at` | timestamptz null | first staff note or status change |
| `resolved_at` | timestamptz null | set entering `resolved`, cleared on reopen |
| `csat_rating` / `csat_comment` / `csat_at` | int 1-5 / text / timestamptz | CSAT capture |
| `created_at` / `updated_at` | timestamptz | |

### ticket_events

One timeline row per mutation, written **in the same transaction** as the
ticket change: `id, tenant_id, ticket_id, kind
(created|assigned|status_changed|note|first_response|resolved|reopened),
actor, payload jsonb, ts`.

### Behavior notes

- **Policy auto-attach**: on create, an explicit `sla_policy_id` wins;
  otherwise the active policy matching the ticket priority attaches. On a
  priority change the policy switches to the new tier's active policy (an
  explicit same-tier policy is kept; no tier policy → policy + dues cleared).
  `sla_policy_id: null` in PATCH detaches the policy and clears the dues.
- **SLA clock**: `due_* = created_at + policy minutes`. Recomputation on
  priority/policy change keeps the original clock — a tightened policy can
  honestly show an already-breached ticket.
- **Auto-assignment**: `assignee_id: "auto"` picks the active team member
  with the fewest `open|pending` tickets (ties by name). No active members →
  `409`. team_members is the core booking table (read-only here, same RLS).
- **Reopen**: `resolved|closed → open|pending` clears `resolved_at` and
  writes a `reopened` event; `first_response_at` is sticky forever.
- **CSAT**: only on `resolved|closed` tickets (`409` otherwise).

---

## 2. Endpoints

Base: `/v1/helpdesk` (tenant via `X-Tenant-Slug` header + tenant
middleware; BFF path `/api/bookings/v1/helpdesk/...`).

### Tickets

```
GET  /v1/helpdesk/tickets?status=&priority=&assignee_id=&channel=&q=
POST /v1/helpdesk/tickets
GET  /v1/helpdesk/tickets/{id}            → {ticket, events}
PATCH /v1/helpdesk/tickets/{id}
PATCH /v1/helpdesk/tickets/{id}/csat
```

Create:

```sh
curl -X POST "$BASE/v1/helpdesk/tickets" \
  -H "X-Tenant-Slug: acme" -H "Authorization: Bearer $JWT" \
  -d '{"subject":"POS terminal offline","channel":"voice","priority":"high"}'
# → 201 {"ticket": {...}}  (+ created event; policy auto-attached by priority;
#    ticket_created CloudEvent enqueued)
```

PATCH (assign / status / note / priority / policy):

```sh
curl -X PATCH "$BASE/v1/helpdesk/tickets/$TICKET_ID" \
  -H "X-Tenant-Slug: acme" -H "Authorization: Bearer $JWT" \
  -d '{"assignee_id":"auto","status":"pending","note":"Technician dispatched"}'
# assignee_id: "<team-member-uuid>" | "auto" | null (unassign)
# sla_policy_id: "<policy-uuid>" | null (detach)
# → 200 {"ticket": {...}} — timeline rows written same-tx; resolving meters
#   ticket_resolved + emits ticket_resolved (only on the real transition)
```

CSAT:

```sh
curl -X PATCH "$BASE/v1/helpdesk/tickets/$TICKET_ID/csat" \
  -H "X-Tenant-Slug: acme" -H "Authorization: Bearer $JWT" \
  -d '{"rating":5,"comment":"fast fix"}'
# → 200 {"ticket": {...}} | 409 if the ticket is not resolved|closed
```

### SLA policies

```
GET   /v1/helpdesk/sla-policies           → {policies: [...]}
POST  /v1/helpdesk/sla-policies           → 201 {policy}
PATCH /v1/helpdesk/sla-policies/{id}      → {policy} (partial: only sent fields)
```

```sh
curl -X POST "$BASE/v1/helpdesk/sla-policies" \
  -H "X-Tenant-Slug: acme" -H "Authorization: Bearer $JWT" \
  -d '{"name":"Urgent tier","priority":"urgent","first_response_minutes":15,"resolve_minutes":120}'
```

### Stats & breaches

```
GET /v1/helpdesk/stats
# → {"stats": {"open_by_priority": {"low":n,"normal":n,"high":n,"urgent":n},
#      "open_count": n, "breached_count": n, "resolved_30d": n,
#      "avg_first_response_minutes_30d": f|null,
#      "avg_resolve_minutes_30d": f|null, "avg_csat_30d": f|null}}

GET /v1/helpdesk/breaches
# → {"tickets": [Ticket + {breached_first_response, breached_resolve}]}
#   (now() > due_*, status NOT IN resolved|closed)
```

### Team members (assignee picker)

```
GET /v1/helpdesk/team-members             → {team_members: [{id,name,email}]}
```

---

## 3. Events (CloudEvents)

Topic `opendesk.helpdesk.events.v1` (default), via the shared transactional
outbox (same idiom as leads funnel events). **Graceful no-op when the topic
is empty.**

Envelope: `type = "com.opendesk.helpdesk.TicketEvent"`,
`source = "booking-service"`, `subject = <tenant slug>`,
extension `tenantid`. Data:

```json
{
  "event_name": "ticket_created | ticket_resolved",
  "tenant_id": "…", "ticket_id": "…", "subject": "…",
  "channel": "…", "priority": "…", "status": "…",
  "contact_id": "…?", "conversation_id": "…?", "assignee_id": "…?",
  "resolved_at": "…?", "created_at": "…"
}
```

Exactly one `ticket_created` per ticket; `ticket_resolved` only on the real
transition into `resolved` (idempotent re-patches do not re-emit).

## 4. Metering

One `ticket_resolved` usage record per resolution on
`opendesk.usage.events` (referrals metering idiom):

```json
{"type": "com.opendesk.usage.UsageRecord",
 "data": {"tenant_id": "…", "metric": "ticket_resolved", "value": 1,
          "ts": "…", "meta": {"ticket_id": "…", "priority": "…",
          "channel": "…", "assignee_id": "…?"}}}
```

Best-effort post-commit; never blocks the resolution; never double-fires on
replay.

## 5. Config envs (integrator wires; app functional with zero config)

| Env | Default | Notes |
| --- | --- | --- |
| `HELPDESK_EVENTS_TOPIC` | `opendesk.helpdesk.events.v1` | empty disables emission |
| `HELPDESK_USAGE_TOPIC` | `opendesk.usage.events` | empty disables metering |
| `HELPDESK_DB_MAX_CONNS` | `4` | dedicated pool size (DialStore) |

The integrator also wires `Deps.TenantFromContext` / `Deps.UserFromContext`
(httpapi's ctxTenant/ctxUser accessors) and the middleware chain
(tenant → appgate `"helpdesk"` → perms).

## 6. UI

`/app/{orgSlug}/apps/helpdesk` — stats header tiles (open / breaches / 30d
averages / CSAT), queue board (status columns, priority pills, breach
badges, search + priority/assignee/channel filters), ticket detail drawer
(timeline, assign/auto-assign, status flow, note composer, CSAT stars), SLA
policy editor (owner/admin). Role guard: owner/admin/staff (server-side,
growth-page pattern).

## 7. Limitations

- `channel` is free-form text (no enum yet) — filters are exact-match.
- No delete endpoint for tickets or policies (deactivate policies instead;
  ticket deletion is an ops follow-up).
- CSAT is recorded by staff on behalf of the customer; a portal-facing CSAT
  capture flow is a follow-up.
- Assignment is per team-member id; notification of assignees (paced push)
  is a follow-up via the W16 notification contract.
- List endpoints cap at 500 rows (newest first); pagination is a follow-up.
