# DND 2442 Suppression & Quiet Hours (SPEC-W12, Agent B)

Nigerian compliance guards for outbound notifications: the NCC **2442
do-not-disturb** registry plus tenant opt-outs suppress *marketing* sends,
and *quiet hours* defer them to the next allowed window. Transactional
traffic is never suppressed or deferred.

## Classification (contract §3)

Every paced send kind (`workflows.PacedSend*`) is classified in an explicit
table in `internal/pacer/guards.go` (`kindClasses`, `pacer.ClassifyKind`):

| Class | Kinds | DND suppression | Quiet hours |
|---|---|---|---|
| `marketing` | `geo_campaign`, `promo`, `broadcast`, `drip` | yes | deferred |
| `transactional` | `confirmation`, `reminder`, `deposit_reminder`, `intake_reminder`, `proposal_reminder`, `noshow_followup`, `waitlist_claim`, `follow_up`, `staff_alert`, `incident_alert`, `otp` | no | no |

Unknown kinds default to **transactional** — the guards only ever apply to
kinds explicitly classified as marketing.

`incident_alert` is transactional **and** keeps its SPEC-W11 Priority
fast-lane (bypasses the CPS token bucket, still metered): the guards do not
touch the Priority path at all.

## Where the guards live

- **DND suppression — activity-side** (`internal/activities/paced.go`,
  `pacer.Guards.PreSend`). `NotifyPaced` checks the registry **before**
  acquiring a CPS token, so a suppressed marketing send consumes no pacing
  budget and never touches a channel binding. The send completes normally
  with result `{status: "suppressed_dnd", reason: ...}`
  (`workflows.PacedSendResult`) — suppression is a *completion status*, not
  an error; scheduling workflows record it as the send outcome.
- **Quiet-hours deferral — workflow-side**
  (`workflows.GuardedPacedSend`). Marketing kinds arriving inside the
  window are deferred with a durable `workflow.Sleep` until the window
  opens, then dispatched through `NotifyPaced` as usual. Transactional
  kinds and `Priority` sends pass immediately. The caller configures
  `workflow.ActivityOptions` on the context as usual, and passes the
  quiet-hours config in (built from its input or from the `QUIET_HOURS_*`
  env at schedule time) so replay stays deterministic across env changes.

Check order for DND (contract): **per-tenant opt-out first, then the global
NCC 2442 list** (`store.IsSuppressed`).

## Configuration (contract §8)

| Env | Default | Meaning |
|---|---|---|
| `DND_ENFORCEMENT` | `true` | Master switch for DND suppression. `false` passes all marketing sends (logged). |
| `QUIET_HOURS_DEFAULT` | `20:00-08:00` | Default window, `HH:MM-HH:MM` local, overnight supported. |
| `QUIET_HOURS_OVERRIDES` | — | JSON object of per-channel windows, e.g. `{"sms":"22:00-06:00"}`. |

Tenant timezone: the tenant's IANA tz, default **Africa/Lagos**. The window
is validated at boot (worker refuses to start on a malformed value). The
DND registry needs `DATABASE_URL` (it shares the `notifications` DB);
without it, `/v1/dnd` returns 503 and marketing sends pass with a warn log.

## DND registry (Postgres)

Table `dnd_numbers` (bootstrapped idempotently by `internal/store/dnd.go`,
same pattern as the webhook tables; app-level tenant isolation, RLS
optional — global rows have `tenant_id NULL`):

- **Global list** (`source = ncc2442`): the NCC 2442 registry, bulk-loaded.
- **Per-tenant opt-outs** (`source = tenant_optout`, `tenant_id` +
  `tenant_slug`): a customer asked one tenant to stop marketing messages.

Uniqueness via partial unique indexes (`phone_e164` where global;
`(tenant_id, phone_e164)` where tenant-scoped) — imports are idempotent
(`ON CONFLICT DO NOTHING`), so re-loading an updated registry snapshot is
safe.

**Phone normalization** (`store.NormalizePhone`): formatting is stripped,
`00…` folds to `+…`; matching is exact equality on the normalized form.
Load E.164 numbers; national-format numbers only match identically
formatted sends. *(Assumption — the NCC 2442 feed format was not available
in this wave.)*

## HTTP API (`internal/httpapi/dnd.go`)

Reached through APISIX at `/api/notifications/v1/dnd/*` (import restricted
to admin JWTs at the gateway, same pattern as `/v1/webhooks`).

| Route | Purpose |
|---|---|
| `POST /v1/dnd/import` | Bulk-load the global list. Body `{"phones": [...], "source": "ncc2442"}` → `{"received": n, "imported": m}` (idempotent). |
| `DELETE /v1/dnd/{phone}` | Opt-out honor: remove a number. No tenant header → global **and** all tenant lists (full re-consent); `X-Tenant-Slug` header → only that tenant's row. 404 when not listed. |
| `GET /v1/dnd/check?phone=[&tenant=slug]` | Suppression lookup in guard order → `{"suppressed": bool, "reason": "tenant_optout"\|"global_dnd"\|""}`. |

## Metering & audit

- Every suppression increments **`notifications_suppressed_total{reason}`**
  with `reason ∈ {tenant_optout, global_dnd}` — process-local counters
  (same idiom as the pacer's `granted`/`priority` counters), exposed via
  `Guards.SuppressedStats()` and emitted on every suppression log line; a
  metrics endpoint can scrape them later.
- Every guard decision (suppress / pass) is logged with kind, tenant,
  phone, reason.
- Quiet-hours deferrals are logged workflow-side with the window-open
  instant and delay.

## Failure policy

- DND store error at send time → **fail-open** (send proceeds, error
  logged), mirroring the pacer's redis fail-open: a notifications-DB outage
  must not stall time-sensitive traffic, and every pass is in the audit
  log for reconciliation.
- No `DATABASE_URL` → registry empty; guards pass with a warn log;
  `/v1/dnd` 503s.
- Malformed `QUIET_HOURS_*` → worker refuses to start (fail fast).

## Coordination notes / assumptions

- booking-service's `GeoCampaignWorkflow` (and Wave-13 promo/broadcast/drip
  senders) should call **`workflows.GuardedPacedSend`** (or replicate its
  deferral) instead of a bare `ExecuteActivity("NotifyPaced", …)`, and read
  `PacedSendResult.status` (`suppressed_dnd`) for their send ledgers. That
  adoption is a booking-service change and is **not** part of this wave
  (Agent B does not own booking-service); the suppression guard in
  `NotifyPaced` already applies to its sends today.
- `NotifyPaced` now returns `(PacedSendResult, error)` — backward
  compatible for Temporal callers that ignore the result
  (`Get(ctx, nil)`), including booking-service's stubs.
- `promo`, `broadcast`, `drip` kinds are classified but have no send
  payloads yet (Wave 13+); the guard already covers them the moment their
  payloads carry a recipient phone.
