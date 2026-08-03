# SPEC-W14 — Referral & Commission Engine (CAC App, Wave 3 of 5)

Wave 14. Four builders, strict ownership. Same protocol (/tmp workspace, additive rsync,
md5-verify FROM /mnt, real test tails, Go /tmp/sdk/go/bin/go GOPROXY=goproxy.cn,direct).

## Cross-agent contracts (bind everyone)

1. **Referral entity**: `{referral_id uuid, tenant_id, referrer_type:"contact|agent|staff",
   referrer_id text, referee_phone text, campaign_id uuid null, status
   "pending|verified|converted|paid|rejected", bounty_rule_id uuid null, created_at,
   verified_at, paid_at}`. Dedupe: one open referral per (tenant, referee_phone).
2. **Commission rule** (tenant-editable): `{rule_id uuid, tenant_id, name, trigger:
   "signup_verified|first_booking|first_txn|sale", beneficiary:"referrer|agent|staff",
   amount_type:"flat|percent", amount_ngn int (kobo) | bps int, cap_ngn null,
   active bool, priority int}`. Multiple rules may fire; evaluation order = priority asc.
3. **Commission ledger** (Postgres, TigerBeetle-compatible scheme — TB adapter seam):
   double-entry rows `{entry_id uuid, tenant_id, journal_id uuid, account_code int,
   debit_ngn bigint default 0, credit_ngn bigint default 0, ref_type, ref_id, created_at}`,
   codes: 300 commission_payable (liability), 301 commission_expense, 302 agent_float,
   303 house_clearing. Every commission posting = balanced pair (debit 301 / credit 300;
   payout: debit 300 / credit 302-or-303). `journal_id` groups a pair. Idempotent on
   (ref_type, ref_id, account_code). Interface `Ledger` in code with Postgres impl +
   documented TigerBeetle adapter seam (comment + interface only — no TB client code).
4. **Payout**: `{payout_id uuid, tenant_id, beneficiary_id, amount_ngn, status
   "queued|processing|paid|failed", provider:"paystack|flutterwave", provider_ref,
   failure_reason}`. Execution via payments-service Dapr invoke (Paystack transfer shape
   ASSUMPTION, annotated); Wave-5 webhook retry pattern NOT reused — payouts are
   Temporal activities with their own retry (3 attempts, backoff) then "failed".
5. **Recon**: nightly Temporal cron workflow `CommissionReconWorkflow` (schedule
   commission-recon-nightly, 02:30 Africa/Lagos): compares ledger payouts vs provider
   transfer status (mockable provider client), mismatches → outbox alert row
   (kind commission_recon_alert) + metered notification.
6. **Events**: Kafka `cac.events` (exists, W12) — emit FunnelEvent `first_txn`/`converted`
   hooks per SPEC-W13 §2 when commissions verify a referral (coordination with W13 leads:
   referral verify → booking leads status converted via the SAME leads service, internal
   endpoint — Agent A reads SPEC-W13 and reuses its leads pkg).
7. **Env**: `COMMISSIONS_ENABLED=true`, `PAYOUT_PROVIDER=paystack`,
   `PAYOUT_MIN_NGN=100`, `RECON_CRON="30 2 * * *"`.

## Agent A — booking-service: referrals + commissions domain
Owns:
- services/booking-service/internal/referrals/ (NEW: model, RLS store — incidents pattern,
  rules engine, ledger (Postgres impl + Ledger interface + TB seam comment), service)
- services/booking-service internal wiring (ADDITIVE: server.go routes, main.go, config.go)
- services/booking-service tests (embedded-postgres)
- docs/referrals-commissions.md (NEW)
Requirements: contracts §1–§3; REST: referrals CRUD + POST /v1/referrals/{id}/verify
(fires rules → balanced postings → referral verified; idempotent), rules CRUD
(manage_bookings), GET /v1/commissions/ledger?from&to, GET /v1/commissions/balance/{beneficiary}
(sum 300 credits − debits). Rule evaluation: pure function, table-driven tests incl.
percent+bps math (integer kobo, no float), cap, priority order, inactive skip.
Known-vector tests for ledger balance. go build/vet/test green.

## Agent B — booking-service: payouts + recon workflow (Temporal)
Owns:
- services/booking-service/internal/referrals/payouts.go (NEW: payout store + execution
  activities + provider client interface w/ Paystack-shape impl, PAYOUT_MOCK=1 default)
- services/booking-service/internal/referrals/recon_workflow.go + payout_workflow.go (NEW)
- services/booking-service/internal/temporalclient/ (ADDITIVE: StartCommissionRecon,
  schedule bootstrap in main — follow existing schedule/cron patterns if any, else
  Temporal Schedule API client code in main.go additive)
- tests (temporal testsuite + httptest provider fakes)
- docs/commission-payouts.md (NEW)
COORDINATION: Agent A owns the pkg dir too — A: model/store/service/rules; B: ONLY the
three files above + temporalclient additive. Shared types (Payout, Ledger) live in A's
files; B codes against contract §3/§4 shapes and imports A's pkg (develop against the
contract; if A's names differ at integration, B's files must compile against whatever
A exported — check /mnt before final rsync; A delivers first typically. If conflict,
B adapts, never edits A's files).
Requirements: contracts §4/§5; payout workflow: create payout → provider transfer activity
(retry 3, backoff) → mark paid + balanced payout posting (300 debit / 302 credit) →
failed after retries w/ reason; recon per §5 with mismatch alert outbox row +
metered notification via existing outbox pattern. go build/vet/test green.

## Agent C — admin-web: referral & commission UI
Owns:
- apps/admin-web/app/app/[orgSlug]/growth/ (NEW pages: referrals list + create,
  commission rules editor, ledger view, payouts queue + balances, per-agent leaderboard)
- apps/admin-web/components/growth/ (NEW)
- nav: ADDITIVE single link (surgical; COORDINATION: W13-D added a cac/ link — append
  after it, do not refactor nav)
- docs/growth-dashboards.md (NEW)
Requirements: mirror Wave-7 analytics/billing page patterns (server fetch + tenant header,
Permify manage_billing gate for rules/payouts, view_analytics for reads); rules editor:
trigger/beneficiary/amount_type + flat-ngn or bps + cap + priority, active toggle;
leaderboard: top referrers by verified+converted counts with paid totals; NGN formatting
helper reuse (billing page has one — reuse, don't duplicate). Existing chart lib only,
no new deps. tsc --noEmit clean.

## Agent D — integration: metering + geo-campaign guard adoption + docs
Owns:
- services/booking-service/internal/geo/workflow.go (SURGICAL: adopt the quiet-hours/DND
  semantics documented in docs/dnd-quiet-hours.md §Coordination — GeoCampaignWorkflow
  records suppressed_dnd + defers via the SAME contract; read notification-worker's
  workflows/paced.go GuardedPacedSend and mirror the semantics booking-side WITHOUT
  importing across services: duplicate the small classification + window math in
  internal/geo (self-contained, tested), keeping behavior identical)
- services/booking-service usage-metering: ADDITIVE metering events referral_verified,
  commission_payout (follow existing outbox metering rows e.g. incident_alert_message,
  geo_campaign_message)
- docs/cac-program.md (NEW: end-to-end CAC App overview linking W12–W14 docs)
- tests for the geo guard adoption (temporal testsuite: marketing deferral + suppressed)
Requirements: go build/vet/test green for booking-service AFTER A+B land (you deliver last;
rsync LAST and re-run the FULL booking-service suite; if A/B export names differ from the
contract, adapt your files only). bash -n on any scripts you touch.

## Delivery protocol: identical to SPEC-W12 §Delivery. Agent order: A and C start
immediately; B starts immediately but checks /mnt for A's exports before finalizing;
D syncs LAST (wait is acceptable — do a final full-suite run before your rsync).
