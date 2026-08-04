# SPEC-W19 — Enterprise Apps Batch 1 (Helpdesk, Field Service, Loyalty, Campaign Studio)

Four app builders + one integration step. Delivery protocol identical to SPEC-W12
§Delivery ($HOME workspaces; additive rsync; md5-verify FROM /mnt; real tails).

## Anti-collision architecture (binds everyone — learned from W17/W18)
All four backends live in booking-service as SELF-CONTAINED packages
internal/<app>/. To prevent four-way clobbering of server.go/main.go/config.go:
- App builders MUST NOT touch internal/httpapi/server.go, cmd/server/main.go,
  internal/config/config.go, go.mod/go.sum, or any file outside their ownership.
- Each package exposes `RegisterRoutes(r chi.Router, d *Deps, mw ...func(http.Handler) http.Handler)`
  and a `NewStore(pool *pgxpool.Pool) *Store` constructor (mirror how W16 devices/
  fieldcapture packages are shaped — inspect them). The INTEGRATOR (separate step,
  after all four land) wires Deps + routes + config envs + appgate flags.
- Config: each app documents its envs in its package doc comment; builders add
  NO config code. Integrator adds envs with safe defaults (apps functional with
  zero config).
- UI routes live under apps/admin-web/app/app/[orgSlug]/apps/<app_id>/ (the W18
  portal's nav_route convention — NO org-nav edits, NO components/apps/ shared
  file edits; per-app components go in components/apps/<app_id>/).
- Every backend route group gets entitlement gating by the INTEGRATOR via
  internal/appgate (W18) with app_id per catalog — builders just document which
  app_id gates their routes.

## Shared contracts
1. RLS: every table FORCE RLS tenant_isolation, mirroring internal/devices/store.go
   DDL idiom (embedded idempotent ensureSchema, pg_policies-guarded).
2. IDs: uuid PKs (gen_random_uuid()), human refs where useful. Money in kobo int64.
3. AuthZ: use existing middleware perms (manage_bookings for writes, view_analytics
   for reads) — the integrator wires; builders code handlers against a small
   TenantFromContext helper consistent with devices/handlers.go (inspect it).
4. Metering: where an app action is billable, emit a metering row mirroring
   internal/referrals/metering.go idiom (inspect it) — helpdesk: ticket_resolved;
   field-service: workorder_completed; loyalty: points_redeemed; studio: journey_enrolled.
5. Events: emit CloudEvents on meaningful lifecycle (ticket created/resolved →
   opendesk.helpdesk.events.v1; workorder assigned/completed → opendesk.fsm.events.v1;
   points issued/redeemed → opendesk.loyalty.events.v1; journey enrolled/completed →
   opendesk.studio.events.v1) via the existing outbox/publisher idiom in booking
   (inspect how referrals or leads publish). Graceful no-op when disabled.
6. Tests: embedded-postgres harness (mirror devices/fieldcapture store_test idiom),
   RLS isolation test, handler tests, plus package unit tests. go build/vet/test
   green for the PACKAGE (full-service test is the integrator's gate).
7. UI: follow W18-B idioms — unwrap<T>() from components/apps/types.ts (READ-ONLY
   import, do not edit), role guards per growth page pattern, BFF path style
   /api/bookings/v1/<...>, honest loading/empty/error states, warm tokens.
   tsc --noEmit green (run from a $HOME copy; lockfile untouched).
8. Docs: docs/apps/<app_id>.md — model, endpoints with curl examples, events,
   metering, entitlement app_id, config envs (for the integrator), limitations.

## Agent A — helpdesk (SLA ticketing)
Backend internal/helpdesk: sla_policies {id, tenant_id, name, priority
low|normal|high|urgent, first_response_minutes int, resolve_minutes int, active};
tickets {id, tenant_id, contact_id null, conversation_id null, subject, channel,
priority, status open|pending|resolved|closed, assignee_id null, sla_policy_id null,
due_first_response_at, due_resolve_at, first_response_at, resolved_at, created_at,
updated_at}; ticket_events {id, tenant_id, ticket_id, kind created|assigned|
status_changed|note|first_response|resolved|reopened, actor, payload jsonb, ts}.
Endpoints /v1/helpdesk: GET/POST /tickets (filters status/priority/assignee/
channel, q search), GET /tickets/{id} (with events timeline), PATCH /tickets/{id}
(assign/status/note — writes ticket_events; sets first_response_at on first
staff note/status change; resolved_at on resolve; recompute due_* from policy on
priority/policy change), GET/POST/PATCH /sla-policies, GET /stats (open by
priority, breaches count, avg first-response/resolve minutes 30d), GET /breaches
(now > due_*, status not in resolved|closed). Assignment: PATCH assign accepts
assignee_id or "auto" → least-open-tickets team member (inspect team members
table from earlier waves). CSAT: PATCH /tickets/{id}/csat {rating 1-5, comment}.
UI /apps/helpdesk: queue board (status columns, priority pills, breach badges),
ticket detail drawer (timeline, assign, note, CSAT), SLA policies editor, stats
header tiles. Docs docs/apps/helpdesk.md.

## Agent B — field-service (work orders & dispatch)
Backend internal/workorders: work_orders {id, tenant_id, contact_id null,
booking_id null, title, description, status created|assigned|en_route|on_site|
completed|cancelled, assignee_id null, scheduled_start, scheduled_end,
gps_lat/gps_lng/gps_accuracy null, checklist jsonb DEFAULT '[]', proof jsonb
DEFAULT '{}', field_capture_id null, created_at, updated_at, completed_at}.
Endpoints /v1/field-service: GET/POST /work-orders, GET/PATCH /work-orders/{id}
(status transitions validated: created→assigned→en_route→on_site→completed,
any→cancelled; completed requires checklist all-done + proof.notes),
POST /work-orders/{id}/dispatch {assignee_id|auto, notify bool} — notify=true
enqueues a paced push_notification to the assignee via the W16 contract
(document the envelope you POST to the notifications topic — inspect
notification-worker workflows/paced.go payload shapes and mirror; topic-gated
graceful when notifications disabled), GET /board (grouped by status with
assignee names), GET /today?assignee=. Checklist items {label, done}.
UI /apps/field-service: dispatch board (status lanes), WO detail (status flow
buttons enforcing transitions, checklist editor, gps display, proof notes),
today view. Docs docs/apps/field-service.md.

## Agent C — loyalty-wallet (points & tiers)
Backend internal/loyalty: programs {id, tenant_id, name, active, earn_rules
jsonb [{event booking_completed|first_txn|referral_converted, points int}],
tiers jsonb [{name, min_points, benefits text}], cap_per_day int DEFAULT 0};
wallets {tenant_id, contact_id, balance int, lifetime_earned int, lifetime_redeemed
int, tier text, updated_at, PK(tenant_id, contact_id)}; ledger REUSES the W14
referrals.Ledger interface (inspect internal/referrals/ledger.go) with codes
400 loyalty_points_issued / 401 loyalty_points_redeemed — instantiate its own
PostgresLedger or mirror the pattern within the package (do NOT edit referrals).
Endpoints /v1/loyalty: GET/POST/PATCH /programs, GET /wallets/{contact_id}
(balance, tier, ledger entries via Ledger.Entries), POST /accrue {contact_id,
event, ref_id} (idempotent on ref_id+event via ledger ref; applies earn_rules;
enforces cap_per_day; updates tier), POST /redeem {contact_id, points, reason}
(insufficient → 409; cap; ledger balanced entry), GET /leaderboard.
UI /apps/loyalty-wallet: program editor (earn rules + tiers friendly form over
the jsonb), wallet lookup by contact, ledger table, leaderboard. Docs
docs/apps/loyalty-wallet.md.

## Agent D — campaign-studio (journeys & segments)
Backend internal/campaignstudio: segments {id, tenant_id, name, definition jsonb
{filters [{field, op eq|neq|in|gte|lte|contains, value}]}, approx_count int,
created_at}; journeys {id, tenant_id, name, status draft|active|paused|archived,
trigger_kind segment|manual|event, segment_id null, steps jsonb [{type wait|send|
branch, kind sms|push_marketing|ussd null, template text, wait_hours int,
condition jsonb null, ab_variant null}], created_at, updated_at}; enrollments
{id, tenant_id, journey_id, contact_id, step_idx int, state active|completed|
exited, enrolled_at, last_step_at, exited_reason null}.
Endpoints /v1/studio: GET/POST/PATCH /segments + POST /segments/{id}/count
(evaluates definition against contacts/leads — read-only queries, RLS-safe,
document perf ceiling 100k rows), GET/POST/PATCH /journeys (status machine
draft→active→paused↔active→archived; steps jsonb validated: known types/kinds,
wait_hours ≥0), POST /journeys/{id}/enroll {contact_ids[]} (idempotent per
journey+contact), POST /journeys/{id}/step (advance due enrollments one step:
wait→time check; send→enqueue paced send via the notification-worker contract
(inspect W12/W16 payload shapes; marketing kinds only — DND/quiet-hours apply
automatically; branch→evaluate condition on contact attrs), GET
/journeys/{id}/stats (enrolled/active/completed/exited, per-step counts).
Execution is operator/CRON-triggered via the step endpoint (document: Temporal
cron wiring is an ops follow-up — honest note).
UI /apps/campaign-studio: journey list + editor (steps form: type picker, kind,
template textarea, wait hours; raw JSON fallback), segment builder (filter rows),
journey stats panel. Docs docs/apps/campaign-studio.md.

## Integration step (after all four land — lead dispatches)
Wire all four packages into booking-service (Deps, RegisterRoutes, config envs
with safe defaults, appgate per app_id: helpdesk|field-service|loyalty-wallet|
campaign-studio), run FULL go build/vet/test, fix any cross-package issues,
deliver wiring diff. Then independent verification gate, then push.
