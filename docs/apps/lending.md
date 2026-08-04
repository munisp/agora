# Lending (`lending`)

SPEC-W20 Agent C — micro-loans: products → applications → KYC-gated
decision → disbursement (intent) → idempotent repayments → PAR30 portfolio.

- Backend: `services/booking-service/internal/lending/` (self-contained
  package; `RegisterRoutes` + `NewStore`/`DialStore` per the W20
  anti-collision contract — the integrator wires Deps/routes/config).
- UI: `apps/admin-web/app/app/[orgSlug]/apps/lending/` +
  `apps/admin-web/components/apps/lending/`.
- Entitlement: appgate `app_id = lending` (integrator gates the whole
  `/v1/lending` route group via the variadic middleware of
  `RegisterRoutes`).

## Model

All five tables are FORCE-RLS `tenant_isolation` (embedded idempotent
`ensureSchema`, pg_policies-guarded — the devices/store.go idiom).

- **loan_products** — `{id, tenant_id, name, active, principal_min_kobo,
  principal_max_kobo, term_days, interest_bps, fee_flat_kobo DEFAULT 0,
  created_at, updated_at}`. CHECKs: `0 < min <= max`, `term_days > 0`,
  `interest_bps ∈ [0,10000]`, `fee >= 0`.
- **loan_applications** — `{id, tenant_id, contact_id, product_id,
  principal_kobo, status, score int null, decline_reason null, decided_by
  null, decided_at null, created_at, updated_at}`. Status machine:

  ```
  draft → submitted → under_review → approved → disbursed → repaid
                        │              │            └──────┐
                        └───────────────┴──→ defaulted ←───┘
  ```

  `declined`, `repaid`, `defaulted` are terminal. `→disbursed` happens
  ONLY via the disburse endpoint, `→repaid` ONLY via the repay flow
  (outstanding hits zero). Default marking is **operator-driven** (PATCH →
  `defaulted` from `submitted`/`under_review`/`approved`/`disbursed`) —
  flips the active loan account to `defaulted` in the same tx. There is
  **no automatic default cron** (follow-up).
- **loan_accounts** — `{id, tenant_id, application_id UNIQUE, contact_id,
  principal_kobo, interest_kobo, fee_kobo, outstanding_kobo, disbursed_at,
  due_at, status active|repaid|defaulted, updated_at}`. At disbursement:
  `interest = principal*interest_bps/10000` (integer division),
  `outstanding = principal + interest + fee`, `due_at = now + term_days`.
- **repayments** — `{id, tenant_id, loan_id, amount_kobo, ref_id, paid_at,
  UNIQUE(tenant_id, loan_id, ref_id)}`. `amount_kobo` is the **applied**
  (clamped) amount.
- **lending_ledger** — kobo double-entry journal, a **mirror** of the W19
  `loyalty_ledger` idiom (itself a mirror of W14 `referrals.Ledger`),
  instantiated package-locally (referrals/loyalty are never edited).
  Idempotency anchor `UNIQUE (tenant_id, ref_type, ref_id, account_code)`.
  - **500 `loan_principal_disbursed`** — borrower principal
    (`beneficiary_id = contact_id`): CREDIT at disbursement, DEBIT as
    repayments arrive.
  - **501 `loan_repayment_received`** — house-side flow account
    (`beneficiary_id = ""`).
  - Disburse journal: DEBIT 501 / CREDIT 500, `ref_type=loan_disbursement`,
    `ref_id = application_id`. Repay journal: DEBIT 500 / CREDIT 501,
    `ref_type=loan_repayment`, `ref_id = caller ref_id`.
  - The ledger mirrors **cash movement** (principal out, repayments in) —
    not the interest/fee schedule; `loan_accounts.outstanding_kobo` is the
    schedule-side cache.

## Money rules

kobo int64 everywhere; interest in basis points int;
`interest = principal*bps/10000` (integer division, floors). Repayments
are idempotent on `UNIQUE(tenant_id, loan_id, ref_id)` — a replay answers
200 with the **same stored body** (`replayed: true`, nothing written).
Overpay is **clamped to outstanding**: the stored repayment records only
the applied amount; the response notes `requested_kobo`, `clamped: true`
— the overpay is never recorded. Disbursement is idempotent via the
application status guard: a replayed disburse returns 200 with the
existing loan account (`replayed: true`) and re-emits **nothing** (no
event, no intent, no metering — money movement is never re-intended).

## Scoring (honest: NOT a credit bureau score)

Naive rule-based 0–100, computed on submit (POST with
`status:"submitted"` or PATCH `draft→submitted`):

| signal | weight |
| --- | --- |
| contact tenure (days since first known activity: `contacts.created_at` when a schema carries it, else first booking) | +3 per started 30-day month, cap 30 |
| completed bookings | +4 each, cap 40 |
| prior repaid loans of this contact | +10 each, cap 30 |

External sources (contacts/bookings) are read **defensively** — a missing
table/column contributes 0, never a 500.

## Endpoints (mounted at `/v1/lending` on the ROOT router)

Permissions: reads = `view_analytics`, writes = `manage_bookings`
(integrator wires method-aware via the variadic middleware). Tenant
resolution: `X-Tenant-Slug` header (package middleware, workorders idiom).

### Products

```bash
curl -H "X-Tenant-Slug: acme" "$GW/v1/lending/products?all=true"

curl -X POST -H "X-Tenant-Slug: acme" -H "content-type: application/json" \
  $GW/v1/lending/products -d '{
    "name": "Trader Cash", "principal_min_kobo": 100000,
    "principal_max_kobo": 5000000, "term_days": 30,
    "interest_bps": 1500, "fee_flat_kobo": 50000
  }'   # → 201 {product}

curl -X PATCH -H "X-Tenant-Slug: acme" -H "content-type: application/json" \
  $GW/v1/lending/products/{id} -d '{"active": false, "interest_bps": 2000}'
```

### Applications

```bash
curl -H "X-Tenant-Slug: acme" "$GW/v1/lending/applications?status=under_review"

# Create (principal validated against the product band; the product must
# be active). status "submitted" computes the score immediately;
# "draft" (default) defers scoring to the submit PATCH.
curl -X POST -H "X-Tenant-Slug: acme" -H "content-type: application/json" \
  $GW/v1/lending/applications -d '{
    "contact_id": "<uuid>", "product_id": "<uuid>",
    "principal_kobo": 2000000, "status": "submitted"
  }'   # → 201 {application} (score set)

# Walk the machine
curl -X PATCH ... $GW/v1/lending/applications/{id} -d '{"status":"under_review"}'

# Approve — KYC gate (two shapes, see below)
curl -X PATCH ... $GW/v1/lending/applications/{id} -d '{
  "status": "approved", "decided_by": "ops-ada",
  "kyc": {"subject_phone": "+234801…", "id_type": "bvn", "id_value": "…"}
}'
# …or, when no KYC service is configured:
curl -X PATCH ... $GW/v1/lending/applications/{id} -d '{
  "status": "approved", "kyc_override": true,
  "kyc_reason": "branch-verified ID card", "decided_by": "ops-ada"
}'

# Decline (reason required)
curl -X PATCH ... $GW/v1/lending/applications/{id} -d '{
  "status": "declined", "decline_reason": "thin file", "decided_by": "ops-ada"
}'

# Operator-driven default marking (flips the active loan too)
curl -X PATCH ... $GW/v1/lending/applications/{id} -d '{"status":"defaulted"}'
```

Errors: illegal transition → 409; decline without reason → 400; approve
failing the KYC gate → 409 `kyc check required to approve: …`.

### Disburse / repay / loans / portfolio

```bash
# Disburse (approved → disbursed). Idempotent: a replay → 200 same loan,
# {"replayed": true}. Emits LoanDisbursed + DisbursementIntent + meters
# loan_disbursed (non-replay only).
curl -X POST -H "X-Tenant-Slug: acme" \
  $GW/v1/lending/applications/{id}/disburse

# Browse the book / resolve a loan from its application
curl -H "X-Tenant-Slug: acme" "$GW/v1/lending/loans?status=active&application_id=<uuid>"

# Loan schedule view
curl -H "X-Tenant-Slug: acme" $GW/v1/lending/loans/{id}
# → {loan, application, repayments[], total_kobo, days_past_due}

# Repay (idempotent on ref_id; overpay clamped to outstanding)
curl -X POST -H "X-Tenant-Slug: acme" -H "content-type: application/json" \
  $GW/v1/lending/loans/{id}/repay -d '{"amount_kobo": 3000000, "ref_id": "rcpt-42"}'
# → {repayment, loan, requested_kobo, clamped, replayed, loan_repaid}

# Portfolio
curl -H "X-Tenant-Slug: acme" $GW/v1/lending/portfolio
# → {portfolio: {total_outstanding_kobo, active_count, repaid_count,
#    defaulted_count, par30, par30_outstanding_kobo, computed_at}}
```

PAR30 = outstanding of **active** loans whose `due_at` is >30 days in the
past ÷ total outstanding of active loans. `par30` is **null** (not 0%)
when there is no outstanding — an honest empty state.

**Contract addition (documented deviation):** `GET /v1/lending/loans`
(collection list with `status`/`application_id`/`contact_id` filters) is
an addition to the SPEC endpoint list — `GET /loans/{id}` alone gives no
way to discover loan ids, which the loan-detail UI requires. Same filter
idiom as the other W19 list endpoints.

## KYC gate (approve)

- `LENDING_KYC_URL` **set** → the handler calls
  `POST {LENDING_KYC_URL}/v1/kyc/resolve` (the kyc-service, SPEC-W12 —
  consent-gated BVN/NIN resolution) with the operator-supplied
  `{subject_phone, id_type, id_value}` and requires `status: "verified"`.
  Non-verified / unreachable / non-200 → 409. The service reference is
  recorded in the decision event (`kyc.mode: "service"`).
- `LENDING_KYC_URL` **empty** → approval requires an explicit
  `{kyc_override: true, kyc_reason: "…"}`; the override + reason are
  recorded in the decision event payload (`kyc.mode: "override"`). This is
  the honest fallback for deployments without a KYC provider.

## Events (topic `opendesk.lending.events.v1`, contract §5)

CloudEvents via the transactional outbox, best-effort post-commit,
graceful no-op when the topic is empty:

| type | fires |
| --- | --- |
| `com.opendesk.lending.ApplicationDecided` | →approved / →declined (payload carries `decision`, `score`, `decided_by`, `decline_reason` and the `kyc` decision record) |
| `com.opendesk.lending.LoanDisbursed` | non-idempotent disbursement |
| `com.opendesk.lending.LoanRepaid` | repayment that zeroes outstanding |
| `com.opendesk.lending.DisbursementIntent` | non-idempotent disbursement — **integration point for the payments/TigerBeetle rail** (see below) |

## Metering (contract §4)

`loan_disbursed` — one usage record per **non-idempotent** disbursement on
the shared usage topic (`com.opendesk.usage.UsageRecord`, value 1,
principal + ids in meta). A replayed disburse never double-meters.

## Real money movement (OUT of scope — integration point)

This package never moves money. Disbursement emits
`com.opendesk.lending.DisbursementIntent` on the lending events topic with
`{intent: "loan_disbursement_payout", amount_kobo, currency: "NGN",
contact_id, loan_id, application_id, ref_id = application_id}`. The
payments/TigerBeetle rail's consumer subscribes to that intent, performs
the actual payout (idempotent on `ref_id`) and owns settlement +
reconciliation. Repayments are operator-recorded receipts (cash/transfer
reference in `ref_id`) — incoming payment-webhook automation is a
follow-up.

## Config envs (for the integrator; apps functional with zero config)

| env | default | effect |
| --- | --- | --- |
| `LENDING_EVENTS_TOPIC` | `opendesk.lending.events.v1` | lifecycle + intent topic; empty disables events |
| `USAGE_EVENTS_TOPIC` | `opendesk.usage.events` | metering topic; empty disables `loan_disbursed` metering |
| `LENDING_KYC_URL` | _(empty)_ | kyc-service base URL for the approve gate; empty → approvals require `kyc_override` + reason |
| `DATABASE_URL` | — | `DialStore` fallback pool (same idiom as W16 devices) |

Wiring: `lending.RegisterRoutes(r, &lending.Deps{Store, Resolver, Logger,
EventsTopic, UsageTopic, KYCURL}, appGateChain...)` on the ROOT router;
method-aware permission middleware (GET → `view_analytics`, else
`manage_bookings`); appgate `app_id = lending`. Kafka topic
`opendesk.lending.events.v1` is created by the integrator
(`infra/kafka/create-topics.sh`).

## Limitations / follow-ups

- Naive scoring only (documented weights) — no credit bureau integration.
- No automatic default marking cron (operator-driven PATCH only).
- No amortization schedule — flat interest, single bullet repayment total.
- Repayments are operator-recorded; no inbound payment-webhook automation.
- Repayments on `defaulted` loans (recoveries) are rejected (409) —
  recovery accounting is a follow-up.
- Currency is assumed NGN/kobo end-to-end (single-currency book).
