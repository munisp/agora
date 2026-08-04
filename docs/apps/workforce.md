# Workforce (shifts, time, leave) — SPEC-W20 Agent D

Enterprise app `workforce`: agent shift planning with an overlap guard,
clock-in/clock-out time tracking (one open entry per agent, optional GPS),
leave requests with approve/decline decisions, plus coverage and
utilization reporting. Backend: booking-service `internal/workforce/`
(self-contained package, anti-collision contract). UI: admin-web
`/app/{orgSlug}/apps/workforce`.

| Area | Artifact |
| --- | --- |
| Model + status machines + validation | `services/booking-service/internal/workforce/workforce.go` |
| Store (RLS, overlap/clock guards, coverage, utilization, outbox) | `services/booking-service/internal/workforce/store.go` |
| Handlers + RegisterRoutes + tenant context | `services/booking-service/internal/workforce/handlers.go` |
| CloudEvents (shift assigned, leave decided) | `services/booking-service/internal/workforce/events.go` |
| Tests (embedded-postgres, RLS, guards, handlers) | `internal/workforce/*_test.go` |
| Admin UI | `apps/admin-web/app/app/[orgSlug]/apps/workforce/`, `apps/admin-web/components/apps/workforce/` |

## Entitlement

- **app_id: `workforce`** — the INTEGRATOR gates the route group via
  `internal/appgate` (W18) and wires it into the identity app catalog.

## Agents are team members

`agent_id` everywhere references the core `team_members` table — the same
lookup helpdesk auto-assignment resolves against. Shift creation, clock-in
and leave filing validate the agent is an **active** team member of the
tenant (else **400**). `GET /v1/workforce/team-members` exposes the
read-only picker projection (mirrors the helpdesk assignee picker).

## Data model

All three tables are RLS-scoped (`ENABLE` + `FORCE ROW LEVEL SECURITY`,
`tenant_isolation` policy on `app.tenant_id`, bootstrap DDL idempotent +
`pg_policies`-guarded, mirroring `internal/devices/store.go`).

`shifts`:

| Column | Type | Notes |
| --- | --- | --- |
| `id` | uuid PK `gen_random_uuid()` | |
| `tenant_id` | uuid NOT NULL | RLS key |
| `agent_id` | uuid NOT NULL | `team_members.id` (validated active) |
| `starts_at` / `ends_at` | timestamptz NOT NULL | `CHECK ends_at > starts_at` |
| `role` | text NULL | optional label (≤120 B) |
| `status` | text CHECK | `scheduled\|confirmed\|completed\|no_show\|cancelled` |
| `created_at` / `updated_at` | timestamptz | |

`time_entries`:

| Column | Type | Notes |
| --- | --- | --- |
| `id` | uuid PK | |
| `tenant_id` / `agent_id` | uuid NOT NULL | |
| `clock_in_at` | timestamptz NOT NULL DEFAULT `now()` | |
| `clock_out_at` | timestamptz NULL | NULL = open entry |
| `method` | text CHECK | `web\|field_pwa` |
| `gps_lat` / `gps_lng` | double NULL | set together, W16 bounds |

`CREATE UNIQUE INDEX ux_time_entries_open ON time_entries (tenant_id, agent_id)
WHERE clock_out_at IS NULL` — one open entry per agent is enforced at the
database level, so concurrent clock-ins cannot race past the API guard.

`leave_requests`:

| Column | Type | Notes |
| --- | --- | --- |
| `id` | uuid PK | |
| `tenant_id` / `agent_id` | uuid NOT NULL | |
| `kind` | text CHECK | `annual\|sick\|unpaid` |
| `starts_on` / `ends_on` | date NOT NULL | `CHECK ends_on >= starts_on` |
| `status` | text CHECK | `pending\|approved\|declined` |
| `reason` | text NULL | ≤2000 B |
| `decided_by` | text NULL | **JWT sub** of the approver/decliner |
| `decided_at` | timestamptz NULL | stamped at the decision |
| `created_at` / `updated_at` | timestamptz | |

### Status machines

```
shifts:  scheduled → confirmed → completed | no_show | cancelled
         scheduled → completed | no_show | cancelled   (terminal states: no outgoing edges)
leave:   pending → approved | declined                  (decisions are final — re-decide → 409)
```

### Guards (SPEC-W20)

- **Shift overlap**: creating or moving a shift that overlaps another
  **non-cancelled** shift of the same agent → **409** with
  `conflicting_shift_id`. Overlap is half-open (`starts < other.ends ∧
  ends > other.starts`), so back-to-back shifts are legal. Cancelled
  shifts never block, and a shift created directly in `cancelled` skips
  the guard.
- **One open time entry per agent**: clock-in while open → **409** with
  `open_entry_id`; clock-out with none → **404**.
- **Leave decisions** record `decided_by` from the JWT sub
  (`Deps.UserFromContext`, `X-User-Id` fallback) + `decided_at`.

## Endpoints

Base: booking-service, `/v1/workforce` (tenant via `X-Tenant-Slug`).
AuthZ (integrator wires, contract §3): reads → `view_analytics`, writes →
`manage_bookings` (recommended integrator shape: method-aware composition
of the existing `require()`).

```bash
# --- Shifts -------------------------------------------------------------
# Create (201; 409 {"error":..., "conflicting_shift_id":"..."} on overlap)
curl -X POST $API/v1/workforce/shifts -H "X-Tenant-Slug: acme" -H "Authorization: Bearer $JWT" -d '{
  "agent_id": "TM_UUID", "starts_at": "2030-03-10T09:00:00Z",
  "ends_at": "2030-03-10T17:00:00Z", "role": "front desk"}'

# List (filters: agent_id, status, from, to — overlap window semantics)
curl "$API/v1/workforce/shifts?agent_id=TM_UUID&status=scheduled" -H "X-Tenant-Slug: acme" -H "Authorization: Bearer $JWT"

# Partial update (times/role/agent re-run the overlap guard; status runs
# the state machine; re-assigning agent_id emits a fresh shift-assigned event)
curl -X PATCH $API/v1/workforce/shifts/SHIFT_UUID -H "X-Tenant-Slug: acme" -d '{"status":"confirmed"}'

# 7-day grid (from = YYYY-MM-DD or RFC3339, default today tenant-local)
curl "$API/v1/workforce/shifts/week?from=2030-03-10&agent_id=TM_UUID" -H "X-Tenant-Slug: acme"
# → {"week_start":"2030-03-10","days":[7 × YYYY-MM-DD],"shifts":[…agent_name…]}

# --- Time ---------------------------------------------------------------
# Clock in (201; 409 {"error":..., "open_entry_id":"..."} when already open)
curl -X POST $API/v1/workforce/time/clock-in -H "X-Tenant-Slug: acme" -d '{
  "agent_id": "TM_UUID", "method": "field_pwa", "gps_lat": 6.5244, "gps_lng": 3.3792}'

# Clock out (200 with the closed entry; 404 when no open entry)
curl -X POST $API/v1/workforce/time/clock-out -H "X-Tenant-Slug: acme" -d '{"agent_id":"TM_UUID"}'

# Entries (filters: agent_id, from, to on clock_in_at)
curl "$API/v1/workforce/time/entries?agent_id=TM_UUID&from=2030-03-01&to=2030-04-01" -H "X-Tenant-Slug: acme"

# --- Leave --------------------------------------------------------------
# File (201, starts pending; dates are YYYY-MM-DD)
curl -X POST $API/v1/workforce/leave -H "X-Tenant-Slug: acme" -d '{
  "agent_id": "TM_UUID", "kind": "annual", "starts_on": "2030-05-04",
  "ends_on": "2030-05-08", "reason": "family"}'

# Queue (filters: status, agent_id)
curl "$API/v1/workforce/leave?status=pending" -H "X-Tenant-Slug: acme"

# Decide (200; decided_by = JWT sub; 409 when already decided)
curl -X PATCH $API/v1/workforce/leave/LEAVE_UUID -H "X-Tenant-Slug: acme" -d '{"action":"approve"}'

# --- Reporting ----------------------------------------------------------
# Coverage: per UTC day, distinct agents scheduled vs bookings count
# (read-only join on the core bookings table; degrades to 0 when absent).
curl "$API/v1/workforce/coverage?from=2030-03-10&to=2030-03-17" -H "X-Tenant-Slug: acme"
# → {"coverage":[{"date":"2030-03-10","agents_scheduled":3,"bookings":12}, …]}

# Utilization: per agent, scheduled vs clocked hours over the range
# (open entries counted to NOW and flagged; utilization_pct null when no
# scheduled hours). from defaults to today (UTC), to = from+7d; max 62 days.
curl "$API/v1/workforce/utilization?from=2030-03-10&to=2030-03-17" -H "X-Tenant-Slug: acme"
# → {"utilization":[{"agent_id":…,"agent_name":"Ada","scheduled_hours":40,
#     "clocked_hours":36.5,"utilization_pct":91.25,"open_entries":0}], "from":…, "to":…}

# Agent picker (active team members)
curl $API/v1/workforce/team-members -H "X-Tenant-Slug: acme"
```

## Events (CloudEvents via the transactional outbox)

Topic **`opendesk.workforce.events.v1`** (empty `WORKFORCE_EVENTS_TOPIC`
disables emission — graceful no-op; best-effort post-commit, never blocks
the mutation):

| Type | Fires when | Key data |
| --- | --- | --- |
| `com.opendesk.workforce.ShiftAssigned` | shift created / re-assigned to a different agent | `shift_id, agent_id, starts_at, ends_at, status, role?` |
| `com.opendesk.workforce.LeaveDecided` | leave approved / declined | `leave_id, agent_id, kind, starts_on, ends_on, decision, decided_by, decided_at` |

The integrator extends `infra/kafka/create-topics.sh` with
`opendesk.workforce.events.v1` (SPEC-W20 integration step).

## Metering — deliberately NONE

Workforce is an **internal-ops app** (SPEC-W20 contract §4): shifts, clock
events and leave decisions are operational records, not billable
tenant-facing actions, so no usage records are emitted (contrast:
surveys/lending meter their billable actions). If a future billing model
prices per-agent seats, that belongs in subscription metering, not here.

## Config envs (for the integrator — safe defaults, zero config works)

| Env | Default | Empty means |
| --- | --- | --- |
| `WORKFORCE_EVENTS_TOPIC` | `opendesk.workforce.events.v1` | events disabled |

The package also needs the shared booking-service seams the integrator
already wires for W19 apps: a tenant resolver (`bookingops.TenantResolver`),
`Deps.UserFromContext` (httpapi's user accessor — JWT sub →
`leave_requests.decided_by`) and the method-aware perms middleware.

## UI (`/app/{orgSlug}/apps/workforce`)

- **Week grid** — 7 UTC-day columns, shift cards with agent/time/role/
  status, coverage line per day (agents scheduled vs bookings), prev/next
  week navigation, click-to-edit.
- **Shift dialog** — agent picker (active team members), date + start/end
  (entered local, sent as UTC ISO), role, status moves on edit. Overlap
  409s surface the API message (with the conflicting shift id) in a toast.
- **Time panel** — agent + method + optional GPS (manual or browser
  geolocation), clock in/out, recent entries with an explicit
  "open (counting)" badge.
- **Leave queue** — file a request; approve/decline pending rows; decided
  history shows decider + timestamp.
- **Utilization table** — per-agent scheduled/clocked hours, % badge
  (null → "—"), open-entry flag; from/to range picker.

Role guard: view = owner/admin/staff/analyst (`view_analytics`), writes
(shift edit, clock, leave decisions) = owner/admin/staff
(`manage_bookings`) — controls hidden when `canWrite` is false.

## Limitations / follow-ups

- Coverage/utilization bucket by **UTC day** (documented in the UI copy);
  tenant-local day bucketing is a follow-up.
- Shift reminders/notifications (push to the agent before a shift) are out
  of scope — the `ShiftAssigned` event is the integration point.
- Leave does not yet block shift creation over approved leave windows —
  surfaced as a follow-up, not enforced.
- Utilization percentages are simple clocked/scheduled ratios — no break
  deductions or rounding policy yet.
