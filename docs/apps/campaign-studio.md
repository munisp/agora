# Campaign Studio (SPEC-W19 Agent D)

Segments, journeys and enrollment — lifecycle messaging orchestration on the
existing paced notification plane. Backend: `booking-service`
`internal/campaignstudio` (self-contained package per the W19 anti-collision
architecture). Admin UI: `/app/{org}/apps/campaign-studio`.

**Entitlement app_id:** `campaign-studio` (integrator wires appgate gating on
`/v1/studio/*`).

## Model

PostgreSQL, every table `ENABLE` + `FORCE ROW LEVEL SECURITY` with the
`tenant_isolation` policy (bootstrap DDL is embedded and idempotent, mirroring
the W16 devices store):

| Table | Shape |
|---|---|
| `studio_segments` | `id uuid PK, tenant_id, name, definition jsonb {filters:[{field,op,value}]}, approx_count bigint, created_at, updated_at` |
| `studio_journeys` | `id uuid PK, tenant_id, name, status draft\|active\|paused\|archived, trigger_kind segment\|manual\|event, segment_id null, steps jsonb, created_at, updated_at` |
| `studio_enrollments` | `id uuid PK, tenant_id, journey_id, contact_id, step_idx int, state active\|completed\|exited, enrolled_at, last_step_at, exited_reason null, UNIQUE(tenant_id, journey_id, contact_id)` |
| `studio_step_events` | `id uuid PK, tenant_id, journey_id, enrollment_id, step_idx, kind, payload jsonb, created_at` (audit + per-step stats) |

### Segment filters

`definition.filters[]` — AND semantics. Operators:
`eq | neq | in | gte | lte | contains`. Fields (whitelist — anything else is
rejected, values are always bound parameters, never interpolated):

- Contact fields (read `contacts`): `name`, `phone`, `email`, `source`,
  `external_id`
- Lead fields (read `leads` via an `EXISTS` phone join — leads carry no
  contact FK, the join key is `contacts.phone = leads.phone_e164`):
  `lead_status`, `lead_channel`, `lead_campaign_id`, `lead_created_at`

### Journey steps

`steps[]` — validated on write:

- `wait` — `wait_hours >= 0` (only field allowed)
- `send` — `kind sms|push_marketing|ussd` + `template` (`{name}` token,
  ≤ 4096 bytes)
- `branch` — `condition` (same filter shape as segments, evaluated on the
  contact's attributes; **false → enrollment exits** with reason
  `branch_condition_false`)
- optional `ab_variant: A|B` on any step

Status machine: `draft → active → paused ↔ active → archived` (draft may
also archive directly; archived is terminal). Structural edits (name /
trigger / segment / steps) are accepted only while `draft` or `paused`.

## Endpoints (`/v1/studio`)

Reads require `view_analytics`, writes `manage_bookings` (integrator wires
via `Deps.RequireRead` / `Deps.RequireWrite`).

```bash
# Segments
curl    $B/v1/studio/segments                                   # list
curl -X POST $B/v1/studio/segments -d '{"name":"CRM","definition":{"filters":[{"field":"source","op":"eq","value":"twenty"}]}}'
curl -X PATCH $B/v1/studio/segments/{id} -d '{"definition":{...}}'
curl -X POST $B/v1/studio/segments/{id}/count                   # → {"count":N,"truncated":false,"ceiling":100000}

# Journeys
curl    "$B/v1/studio/journeys?status=active"                   # list (filter optional)
curl -X POST $B/v1/studio/journeys -d '{"name":"Winback","trigger_kind":"manual","steps":[{"type":"send","kind":"sms","template":"Hi {name}"},{"type":"wait","wait_hours":24},{"type":"branch","condition":{"filters":[{"field":"lead_status","op":"eq","value":"qualified"}]}}]}'
curl    $B/v1/studio/journeys/{id}
curl -X PATCH $B/v1/studio/journeys/{id} -d '{"status":"active"}'   # status machine
curl -X PATCH $B/v1/studio/journeys/{id} -d '{"steps":[...]}'       # draft|paused only

# Enrollment (idempotent per journey+contact; journey must be active)
curl -X POST $B/v1/studio/journeys/{id}/enroll -d '{"contact_ids":["<uuid>",...]}'
# → {"enrolled":N,"existing":M}

# Execution — operator/CRON-triggered (journey must be active)
curl -X POST $B/v1/studio/journeys/{id}/step -d '{"limit":200}'
# → {"scanned":N,"advanced":N,"completed":N,"exited":N,"skipped":N,
#    "wait_not_due":N,"sends_queued":N,"sends_deferred":false,
#    "dispatch":"started","workflow_id":"studio-send-<batch>"}

# Stats
curl $B/v1/studio/journeys/{id}/stats
# → {"stats":{"enrolled":N,"active":N,"completed":N,"exited":N,
#    "per_step":[{"step_idx":0,"type":"send","active":N,"passed":N,"sent":N,"suppressed":N,"skipped":N,"failed":N,"exited":N}]}}
```

### Step semantics (`POST /journeys/{id}/step`)

Advances up to `limit` (default 200, env `STUDIO_STEP_BATCH`) active
enrollments by **one step** each, in one transaction
(`FOR UPDATE SKIP LOCKED` — concurrent operator+CRON callers cannot
double-process):

- `wait` → due when `last_step_at + wait_hours <= now`, else left in place
  (`wait_not_due`)
- `send` → the paced-send payload is queued **and the enrollment advances
  in the same transaction**, then one `StudioSendWorkflow` is started
  post-commit for the batch. A retried step call finds the enrollments
  already advanced → no double-queue (idempotent). Workflow-start failure
  returns `502` with the advancement honestly reported.
- `branch` → condition evaluated on contact attrs; true advances, false
  exits.
- Advancing past the last step → `completed` (+ `journey_completed` event).
- When no Temporal starter is configured (Temporal down at boot), due send
  enrollments are left in place and the response carries
  `sends_deferred: true` — the next dispatched call picks them up.

**Temporal-cron follow-up (honest note):** journey execution is
operator/CRON-triggered via the step endpoint. Wiring a Temporal cron
schedule (or external scheduler) that calls `POST /journeys/{id}/step`
periodically for every active journey — plus segment-trigger auto-enrollment
(`trigger_kind: segment`) — is an OPS FOLLOW-UP, not part of this wave.

### Paced-send contract used (notification-worker `NotifyPaced`)

Marketing kinds only — DND (NCC 2442 + tenant opt-out) is enforced
activity-side, quiet-hours (default 20:00–08:00 Africa/Lagos,
`QUIET_HOURS_DEFAULT`/`QUIET_HOURS_OVERRIDES`, captured at step time) are
deferred workflow-side by `StudioSendWorkflow` (mirror of
`geo.quiet.go`/`workflows.GuardedPacedSend`):

| Step kind | Paced kind | Payload |
|---|---|---|
| `sms` | `geo_campaign` | `{kind:"geo_campaign", geo_campaign:{tenant_slug, campaign_id:<journey id>, channel:"sms", phone, name, text}}` — the notification-worker's only SMS marketing route (`SendGeoCampaignMessage`) |
| `push_marketing` | `push_marketing` | `{kind:"push_marketing", push:{tenant_slug, contact_id, phone, title:<journey name>, body, data:{journey_id, enrollment_id}}}` — `SendPushNotification` fan-out; `phone` lets the DND guard check the phone-keyed registries |
| `ussd` | — | **No outbound USSD binding exists**: ussd steps advance and count as `skipped` (limitation) |

Outcomes are recorded per enrollment (`send_sent` / `send_suppressed` /
`send_failed` step events) by the `StudioRecordSendOutcome` activity and
surface in the stats endpoint. Contacts without a phone skip SMS steps
(`send_skipped`, reason `missing_phone`); contacts that vanished exit with
reason `contact_missing`.

## Events & metering

- **CloudEvents** topic `opendesk.studio.events.v1` (via the transactional
  outbox, post-commit best-effort): `com.opendesk.studio.JourneyEnrolled`,
  `com.opendesk.studio.JourneyCompleted` — data
  `{tenant_id, journey_id, contact_id, enrollment_id, ts}`.
  ⚠️ **Integrator:** declare the topic in `infra/kafka/create-topics.sh`
  (broker auto-create is OFF) — flagged as a foreign-file change.
- **Metering** topic `opendesk.usage.events`: metric `journey_enrolled`,
  value 1 per NEW enrollment (emitted only on the non-idempotent path —
  replays never double-meter).

## Config envs (integrator — package adds no config code)

| Env | Default | Purpose |
|---|---|---|
| `STUDIO_DATABASE_URL` | falls back to `DATABASE_URL` | dedicated store pool (maxConns 4) |
| `STUDIO_STEP_BATCH` | `200` | enrollments advanced per step call |
| `STUDIO_EVENTS_TOPIC` | `opendesk.studio.events.v1` | lifecycle events (empty disables) |
| `USAGE_EVENTS_TOPIC` | existing | metering topic (empty disables) |
| `QUIET_HOURS_DEFAULT` / `QUIET_HOURS_OVERRIDES` | `20:00-08:00` / — | reused SPEC-W12 §8 quiet-hours config, captured at step time |

### Integrator wiring sketch

```go
studioStore, _ := campaignstudio.DialStore(ctx, studioDSN)
if tc != nil {
    campaignstudio.RegisterWorker(w, &campaignstudio.SendActivities{Store: studioStore, Logger: logger})
    studioStarter = campaignstudio.TemporalStarter{Client: tc.Underlying(), TaskQueue: cfg.TemporalTaskQueue}
}
campaignstudio.RegisterRoutes(r, &campaignstudio.Deps{
    Store: studioStore, Logger: logger, Starter: studioStarter,
    TenantFromContext: tenantFrom,
    RequireRead: s.require("view_analytics"), RequireWrite: s.require("manage_bookings"),
    UsageTopic: cfg.UsageEventsTopic, EventsTopic: campaignstudio.DefaultEventsTopic,
    StepBatchSize: cfg.StudioStepBatch,
} /* + appgate gating, app_id "campaign-studio" */)
```

**As delivered (W19 integrator):** booking-service DOES run an in-process
Temporal worker (shared `opendesk-main` task queue), so
`campaignstudio.RegisterWorker` IS wired in `cmd/server/main.go` — the
store is hoisted ahead of the worker block (same idiom as the W14 payout
store) and the workflow/activity registration happens when both Temporal
and the store dial succeeded. The step endpoint also works starter-only
(sends defer with `sends_deferred` when Temporal is unreachable).
`EventsTopic` is wired from `cfg.StudioEventsTopic` (`STUDIO_EVENTS_TOPIC`,
default `DefaultEventsTopic`) rather than the constant directly; the
appgate middleware (`app_id "campaign-studio"`) plus httpapi's
tenantMiddleware form the variadic group chain.

## Limitations

- **100k-row segment ceiling:** one count evaluation scans at most 100 000
  contacts (bounded `LIMIT` subquery); `truncated: true` reports the ceiling
  was hit. Narrow larger audiences with filters.
- **Lead join is by phone** (`leads.phone_e164 = contacts.phone`) — leads
  carry no contact FK; contacts without a phone never match `lead_*`
  filters.
- **ussd send steps are skipped** (no outbound USSD binding in
  notification-worker).
- **No automatic scheduler** (see the Temporal-cron follow-up above):
  nothing advances enrollments until the step endpoint is called.
- **Segment/event triggers** (`trigger_kind: segment|event`) are accepted
  and stored, but auto-enrollment from segment membership / platform events
  ships with the cron follow-up; `manual` enrollment via the API works
  today.
- **Send dispatch is at-least-once at the workflow boundary:** the
  enrollment advances transactionally before the workflow starts; a
  workflow-start failure surfaces as 502 and is NOT retried automatically
  (the send is lost; step events keep the audit). Workflow-internal send
  failures are recorded (`send_failed`) and never abort the batch.
- `gte`/`lte` on non-numeric values compares lexicographically (RFC3339
  timestamps compare correctly); SQL-side `lead_created_at` casts to
  `timestamptz`.
