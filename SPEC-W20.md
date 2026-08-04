# SPEC-W20 — Enterprise Apps Batch 2 (CRM-360, Surveys/VoC, Lending, Workforce)

Four app builders + one integration step. Delivery protocol identical to SPEC-W12
§Delivery ($HOME workspaces; additive rsync; md5-verify FROM /mnt; real tails).
This completes the 8-app enterprise program (W19 delivered helpdesk,
field-service, loyalty-wallet, campaign-studio).

## Anti-collision architecture (binds everyone — same as W19)
All four backends live in booking-service as SELF-CONTAINED packages
internal/<app>/. To prevent four-way clobbering of server.go/main.go/config.go:
- App builders MUST NOT touch internal/httpapi/server.go, cmd/server/main.go,
  internal/config/config.go, go.mod/go.sum, or any file outside their ownership.
- Each package exposes `RegisterRoutes(r chi.Router, d *Deps, mw ...func(http.Handler) http.Handler)`
  and a `NewStore(pool *pgxpool.Pool) *Store` constructor. The canonical shape to
  mirror is now the W19 packages — inspect internal/helpdesk/ (routes+handlers+store
  split), internal/workorders/ (Deps.Resolver + package-local tenant middleware),
  and internal/loyalty/ (mirrored ledger, mounted inside the httpapi /v1 group).
  The INTEGRATOR (separate step, after all four land) wires Deps + routes +
  config envs + appgate flags, reusing the W19 wiring (requireReadWrite,
  appGateChain) verbatim.
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
   Interest in basis points int.
3. AuthZ: existing middleware perms (manage_bookings for writes, view_analytics
   for reads) — the integrator wires; builders code handlers against the
   TenantFromContext helper idiom used by W19 packages (inspect workorders).
4. Metering: where an app action is billable, emit a metering row mirroring
   internal/referrals/metering.go idiom — surveys: survey_response_received;
   lending: loan_disbursed. crm-360 and workforce: NO metering (internal-ops
   apps) — document that decision in the package doc + docs page.
5. Events: CloudEvents via the existing outbox/publisher idiom, graceful no-op
   when disabled — crm-360: note/pin/tag changes → opendesk.crm.events.v1;
   surveys: sent/answered → opendesk.surveys.events.v1; lending: application
   decided/disbursed/repaid → opendesk.lending.events.v1; workforce:
   shift assigned, leave decided → opendesk.workforce.events.v1.
6. Tests: embedded-postgres harness (mirror W19 store_test idiom), RLS isolation
   test, handler tests, package unit tests. go build/vet/test green for the
   PACKAGE (full-service test is the integrator's gate).
7. UI: follow W18/W19 idioms — unwrap<T>() from components/apps/types.ts
   (READ-ONLY import, do not edit), role guards per growth page pattern, BFF
   path style /api/bookings/v1/<...>, honest loading/empty/error states, warm
   tokens. tsc --noEmit green (run from a $HOME copy; lockfile untouched).
8. Docs: docs/apps/<app_id>.md — model, endpoints with curl examples, events,
   metering (or explicit no-metering note), entitlement app_id, config envs
   (for the integrator), limitations.

## Agent A — crm-360 (unified customer profile)
Backend internal/crm360: crm_notes {id, tenant_id, contact_id, author, body,
pinned bool DEFAULT false, created_at, updated_at}; crm_tags {tenant_id,
contact_id, tag text, created_at, PK(tenant_id, contact_id, tag)}. The 360 view
is an AGGREGATION over existing domain tables — read-only joins across contacts,
bookings, helpdesk tickets, work_orders, loyalty wallets, conversations
(discover actual table/column names from the W13–W19 stores; code defensively:
each section degrades to an empty array if its source table is absent, never
500s on a missing optional source).
Endpoints /v1/crm: GET /contacts/{id}/360 (profile: contact record, tags,
open ticket count + latest 5, recent bookings latest 5, active work orders,
loyalty wallet {balance, tier} if any, consent status if resolvable), GET
/contacts/{id}/timeline?limit= (merged chronological feed across bookings,
ticket_events, work order status changes, loyalty ledger entries, crm_notes —
item {ts, kind, summary, ref_id}), GET/POST/DELETE /contacts/{id}/tags (tag
text validated: lowercase, 1..40 chars, [a-z0-9-_]), GET/POST
/contacts/{id}/notes, PATCH /notes/{id} (edit body, toggle pinned), GET
/contacts/search?q=&tag=&limit= (name/phone/email prefix search + tag filter).
UI /apps/crm-360: contact search + list, 360 profile page (sections with
honest empty states), timeline feed, notes editor (pin toggle), tag chips.
Docs docs/apps/crm-360.md.

## Agent B — surveys-voc (NPS/CSAT/VoC)
Backend internal/surveys: surveys {id, tenant_id, name, status draft|active|
paused|archived, kind nps|csat|ces|custom, questions jsonb [{id, type rating|
text|single|multi, label, options []text, required bool}], trigger_kind manual|
ticket_resolved|booking_completed, channel sms|push_marketing, created_at,
updated_at}; survey_invites {id, tenant_id, survey_id, contact_id, token text
unique, status queued|sent|answered|expired, sent_at, answered_at, created_at};
survey_responses {id, tenant_id, survey_id, invite_id null, contact_id null,
answers jsonb, score int null, submitted_at} — responses RLS'd; the public
submit path resolves tenant via invite token, NEVER via header.
Endpoints /v1/surveys: GET/POST/PATCH /surveys (status machine draft→active→
paused↔active→archived; questions validated: known types, single/multi require
≥2 options), POST /surveys/{id}/send {contact_ids[]} (creates invites with
random 128-bit hex tokens, enqueues paced sends via the W16/W19
notification-worker PacedSend CloudEvent contract — inspect notification-worker
internal/notifyoutbox/consumer.go + workflows/paced.go and mirror exactly;
channel from survey.channel; marketing kinds get DND/quiet-hours automatically),
POST /respond {token, answers} (PUBLIC — no tenant header, no JWT; token
resolves invite → tenant + survey; validates required questions + option
membership; computes score for nps/csat/ces from the first rating answer;
idempotent on invite: second submit → 409 already_answered; unknown token →
404; document rate-limit as ops concern via APISIX), GET /surveys/{id}/results
(response count, score distribution, NPS = %promoters(9-10) − %detractors(0-6)
for kind=nps, mean for csat/ces, per-question breakdown for single/multi),
GET /voc/themes?survey_id= (naive keyword frequency over text answers —
lowercase, strip stopwords, top 20 terms with counts; document as naive,
not NLP). Trigger automation (ticket_resolved → auto-send) is OUT of scope —
document as follow-up; only manual send ships.
UI /apps/surveys-voc: survey list + editor (question rows: type picker, label,
options editor, required toggle; kind picker; raw JSON fallback), send dialog
(contact id list), results dashboard (NPS gauge as number tile, distributions,
themes list). Docs docs/apps/surveys-voc.md.

## Agent C — lending (micro-loans)
Backend internal/lending: loan_products {id, tenant_id, name, active,
principal_min_kobo int64, principal_max_kobo int64, term_days int,
interest_bps int, fee_flat_kobo int64 DEFAULT 0}; loan_applications {id,
tenant_id, contact_id, product_id, principal_kobo int64, status draft|
submitted|under_review|approved|declined|disbursed|repaid|defaulted, score int
null, decline_reason null, decided_by null, decided_at null, created_at,
updated_at}; loan_accounts {id, tenant_id, application_id unique, contact_id,
principal_kobo, interest_kobo, fee_kobo, outstanding_kobo, disbursed_at,
due_at, status active|repaid|defaulted}; repayments {id, tenant_id, loan_id,
amount_kobo int64, ref_id text, paid_at, UNIQUE(tenant_id, loan_id, ref_id)};
mirrored ledger REUSING the W14/W19 ledger idiom (inspect internal/loyalty/
ledger.go — instantiate package-local, do NOT edit referrals or loyalty) with
codes 500 loan_principal_disbursed / 501 loan_repayment_received (disjoint
from referrals 300-303 and loyalty 400-401).
Scoring: naive rule-based score 0-100 from contact tenure + completed bookings
count + prior repaid loans (weights documented in code); honest note: not a
credit bureau score.
Endpoints /v1/lending: GET/POST/PATCH /products, GET/POST /applications
(principal validated against product min/max; score computed on submit), PATCH
/applications/{id} (status machine submitted→under_review→approved|declined;
decline requires reason; approve requires kyc check — call kyc-service if
KYC_URL env configured, else accept explicit {kyc_override: true, reason} and
record it in the event payload; document this honestly), POST
/applications/{id}/disburse (approved→disbursed: creates loan_account with
interest = principal*bps/10000, fee, outstanding = principal+interest+fee,
due_at = now+term_days; mirrored ledger 500 entry; emits disbursement INTENT
event for the payments/TigerBeetle rail — real money movement is OUT of scope,
document as integration point; idempotent via application status guard), POST
/loans/{id}/repay {amount_kobo, ref_id} (idempotent on ref_id → 200 replay;
amount clamped to outstanding, overpay noted in response; outstanding==0 →
status repaid + application repaid; ledger 501), GET /loans/{id} (schedule
view: principal/interest/fee/outstanding/repayments), GET /portfolio
(total outstanding, active/repaid/defaulted counts, PAR30 = outstanding of
loans >30d past due / total outstanding). Default marking is operator-driven
(PATCH application → defaulted) — no automatic cron, document as follow-up.
UI /apps/lending: products editor, applications queue (status filters, score
badge, approve/decline dialog with kyc override checkbox), loan detail
(schedule + repay form), portfolio tiles (outstanding, PAR30). Docs
docs/apps/lending.md.

## Agent D — workforce (shifts, time, leave)
Backend internal/workforce: shifts {id, tenant_id, agent_id, starts_at,
ends_at, role text null, status scheduled|confirmed|completed|no_show|
cancelled, created_at, updated_at, CHECK ends_at > starts_at}; time_entries
{id, tenant_id, agent_id, clock_in_at, clock_out_at null, method web|field_pwa,
gps_lat/gps_lng float8 null}; leave_requests {id, tenant_id, agent_id, kind
annual|sick|unpaid, starts_on date, ends_on date, status pending|approved|
declined, decided_by null, decided_at null, reason text null, CHECK ends_on >=
starts_on}. Agent ids reference the existing team members table (discover from
earlier waves — helpdesk assignment already resolves team members; mirror that
lookup).
Endpoints /v1/workforce: GET/POST/PATCH /shifts (overlap detection: creating/
moving a shift overlapping another non-cancelled shift for the same agent →
409 with the conflicting shift id), GET /shifts/week?agent_id=&from= (7-day
grid), POST /time/clock-in {agent_id, method, gps_lat?, gps_lng?} (409 if an
open entry exists for the agent), POST /time/clock-out {agent_id} (404 if no
open entry), GET /time/entries?agent_id=&from=&to=, GET/POST /leave, PATCH
/leave/{id} (approve|decline with decided_by from JWT sub), GET
/coverage?from=&to= (per day: agents scheduled vs bookings count — read-only
join, honest empty state), GET /utilization?from=&to= (per agent: scheduled
hours, clocked hours, utilization %; null clock_out entries counted to now,
flagged open).
UI /apps/workforce: week grid (shifts per agent/day), shift create/edit dialog,
clock in/out panel (gps optional), leave queue (approve/decline), utilization
table. Docs docs/apps/workforce.md.

## Integration step (after all four land — lead dispatches)
Wire all four packages into booking-service (Deps, RegisterRoutes, config envs
with safe defaults, appgate per app_id: crm-360|surveys-voc|lending|workforce)
reusing the W19 wiring patterns, run FULL go build/vet/test, fix any
cross-package issues, deliver wiring diff. Then independent verification gate,
then push. Also extend infra/kafka/create-topics.sh with the four new events
topics (opendesk.crm.events.v1, opendesk.surveys.events.v1,
opendesk.lending.events.v1, opendesk.workforce.events.v1).
