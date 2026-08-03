# Referrals, Commissions & Ledger (Wave 14, CAC App wave 3)

booking-service owns the referral & commission write path: referral capture
with one-open-per-phone dedupe, tenant-editable commission rules, a
double-entry commission ledger with a TigerBeetle adapter seam, the
idempotent verify flow (rules → balanced postings → referral verified),
payouts via Temporal (Paystack/Flutterwave transfer + nightly recon), and
the funnel hooks (`converted` / `first_txn`) onto Kafka `cac.events`.
Implements SPEC-W14 (contracts §1–§7). Sibling: `docs/leads-attribution.md`
(Wave 13).

Ownership inside the wave: **Agent A** — referral entity, rules engine,
ledger (Postgres impl + Ledger interface + TB seam), verify flow, REST
endpoints, `cac.events` hooks, this doc. **Agent B** — payout store +
execution activities, payout & recon Temporal workflows
(`internal/referrals/payouts.go`, `payout_workflow.go`, `recon_workflow.go`,
`internal/temporalclient/commission.go`). **Agent C** — admin-web UI.
**Agent D** — usage metering events.

---

## 1. Referral entity (contract §1)

Table `referrals` (RLS `tenant_isolation`, like every tenant table):

```
referral_id uuid PK, tenant_id uuid,
referrer_type text           -- contact|agent|staff
referrer_id  text,           -- free-form referrer identity (contact/agent/staff id)
referee_phone text,
campaign_id  uuid null,      -- may reference campaigns (no FK by design)
status       text            -- pending|verified|converted|paid|rejected
bounty_rule_id uuid null,    -- first (highest-priority) rule that fired at verify
created_at, verified_at, paid_at
```

**Dedupe (one open referral per phone):** a partial unique index
`UNIQUE (tenant_id, referee_phone) WHERE status IN ('pending','verified')`
makes the insert `ON CONFLICT ... DO NOTHING`; a duplicate returns the
EXISTING open referral unchanged (`created=false`, HTTP 200). "Open" =
pending|verified; converted/paid/rejected are closed, so a new referral for
the same phone can be opened once the previous one closed (mirrors the W13
leads first-touch dedupe posture).

**Status machine:**

```
pending ──verify(signup_verified)──────────────▶ verified
pending ──verify(first_booking|first_txn|sale)─▶ converted   (revenue triggers)
pending|verified ──reject──────────────────────▶ rejected
verified|converted ──paid out──────────────────▶ paid        (reserved, see §4)
```

`paid` exists per contract but **W14 payouts settle per-beneficiary
balances** (§4), not individual referrals — no code path flips a single
referral to `paid` in this wave; `paid_at` is reserved for a future
per-referral payout link. Documented assumption.

## 2. Commission rules (contract §2)

Table `commission_rules` — tenant-editable via REST (`manage_bookings` for
writes, `view_analytics` for reads):

```
rule_id uuid PK, tenant_id uuid, name text,
trigger     text   -- signup_verified|first_booking|first_txn|sale
beneficiary text   -- referrer|agent|staff
amount_type text   -- flat|percent
amount_ngn  bigint -- kobo (flat) — integer, no float
bps         int    -- basis points (percent)
cap_ngn     bigint null -- kobo, null = uncapped
active      bool, priority int
```

The rules engine is a **pure function**
(`referrals.EvaluateRules(rules, trigger, baseKobo, referral)`) — no I/O,
no clock — with table-driven tests (`rules_test.go`):

- every ACTIVE rule whose trigger matches fires, in `priority` asc order
  (ties: created_at, then id — deterministic);
- **flat:** `amount = amount_ngn`; **percent:** `amount = baseKobo * bps /
  10000` (integer floor, e.g. 123456 kobo × 250 bps = 3086 kobo);
- **cap:** `amount = min(amount, cap_ngn)` when set;
- awards computing to `<= 0` are skipped (e.g. a percent rule on a
  `signup_verified` verify whose base is 0);
- the rule has no beneficiary_id column, so the beneficiary resolves from
  the referral: `referrer` → the referrer (any type); `agent` / `staff` →
  the referrer only when `referrer_type` matches (the rule simply does not
  fire otherwise).

## 3. Double-entry commission ledger (contract §3)

Table `commission_ledger`:

```
entry_id uuid PK, tenant_id uuid, journal_id uuid,
account_code int     -- 300|301|302|303
beneficiary_id text  -- '' = house side (documented extension, see below)
debit_ngn bigint, credit_ngn bigint,   -- kobo; CHECK: exactly one side > 0
ref_type text, ref_id text, created_at,
UNIQUE (tenant_id, ref_type, ref_id, account_code)   -- idempotency
```

Account codes (TigerBeetle-compatible chart):

| code | account             | used for                                        |
|-----:|---------------------|-------------------------------------------------|
| 300  | commission_payable  | liability owed to a beneficiary                 |
| 301  | commission_expense  | house expense side of an accrual                |
| 302  | agent_float         | payout settlement via agent float               |
| 303  | house_clearing      | payout settlement via house clearing            |

Every posting is a **balanced pair** (one `journal_id`, sum debits == sum
credits) — enforced by `Ledger.Post`/`PostBalanced` before any row lands:

- **accrual** (verify): DEBIT 301 (house) / CREDIT 300 (beneficiary),
  `ref_type=commission_accrual`, `ref_id=<referral_id>:<rule_id>`,
  `journal_id = UUIDv5(referral|rule)` (deterministic);
- **payout** (Agent B): DEBIT 300 (beneficiary) / CREDIT 302 or 303,
  `ref_type=commission_payout`, `ref_id=<payout_id>`.

**Idempotency:** the unique key `(ref_type, ref_id, account_code)` +
`ON CONFLICT DO NOTHING` makes every replay a strict no-op (verify replays,
retried Temporal activities).

**`beneficiary_id` extension:** the contract row set has no beneficiary
column, but `GET /v1/commissions/balance/{beneficiary}` must sum 300-account
credits−debits *per beneficiary* — so the table carries `beneficiary_id`
(`''` = house side). Payout postings MUST set it on the debit side or the
balance never decreases (flagged to Agent B; `BalancedPosting` has the
field).

**Ledger interface + TigerBeetle seam** (`internal/referrals/ledger.go`):

```go
type Ledger interface {
    Post(ctx, tenantID, journalID, entries []LedgerEntry) error
    PostBalanced(ctx, p BalancedPosting) error
    Balance(ctx, tenantID, accountCode, beneficiaryID) (int64, error)
    Entries(ctx, tenantID, from, to) ([]LedgerEntry, error)
}
```

`PostgresLedger` is the impl wired in `main.go`. The TB adapter seam is
interface + documentation only (no TB client code): account codes → four TB
accounts per tenant, one journal → a `linked` TB transfer chain, transfer id
= UUIDv5(ref_type|ref_id|account_code) for the same idempotency semantics,
`Balance` = TB `lookup_accounts`. Amounts are already integer kobo. Swapping
is a one-line `main.go` change; the verify path's accrual-+-status-flip
atomicity moves to an outbox-row + relay under TB (same at-least-once +
idempotent-consumer posture as `cac.events`).

## 4. Payouts (contract §4 — Agent B)

Table `commission_payouts` (bootstrapped identically by A's store bootstrap
and B's `PayoutStore.ensureSchema` — interchangeable, both idempotent):

```
payout_id uuid PK, tenant_id uuid, beneficiary_id text,
amount_ngn bigint,             -- kobo
status text,                   -- queued|processing|paid|failed
provider text,                 -- paystack|flutterwave (mock via PAYOUT_MOCK)
provider_ref text, failure_reason text, created_at, paid_at
```

`PayoutWorkflow`: create payout → provider transfer → finalize; failed
transfers alert on Slack. `CommissionReconWorkflow` runs nightly
(`RECON_CRON`, default `30 2 * * *` = 02:30 Africa/Lagos): queued > 24h and
processing > 1h are flagged/alerted; paid rows are checked with the provider
by `provider_ref`. See `internal/referrals/payout_workflow.go` /
`recon_workflow.go` (Agent B).

## 5. REST API (booking-service, `/v1`)

| Method & path                              | Permission        | Notes |
|--------------------------------------------|-------------------|-------|
| `POST /v1/referrals`                       | `manage_bookings` | create; §1 dedupe → 200 + existing |
| `GET  /v1/referrals?status=`               | `view_analytics`  | list (newest first) |
| `GET  /v1/referrals/{id}`                  | `view_analytics`  | one |
| `POST /v1/referrals/{id}/verify`           | `manage_bookings` | fires rules → postings → verified/converted; idempotent |
| `POST /v1/referrals/{id}/reject`           | `manage_bookings` | pending\|verified → rejected (409 on terminal) |
| `POST /v1/commissions/rules`               | `manage_bookings` | create rule |
| `GET  /v1/commissions/rules`               | `view_analytics`  | list rules (priority order) |
| `PUT  /v1/commissions/rules/{id}`          | `manage_bookings` | update (incl. `active` toggle) |
| `DELETE /v1/commissions/rules/{id}`        | `manage_bookings` | delete rule |
| `GET  /v1/commissions/ledger?from&to`      | `view_analytics`  | ledger rows, oldest first |
| `GET  /v1/commissions/balance/{beneficiary}` | `view_analytics` | payable balance in kobo (account 300 credits − debits) |

Verify body: `{"trigger": "signup_verified|first_booking|first_txn|sale",
"base_amount_ngn": <kobo, integer>}` — the base percent/bps rules are
computed against (0 for `signup_verified`). Response:
`{"referral": {...}, "already_verified": bool, "awards": [...]}`; a replay
on a verified/converted/paid referral returns 200 with
`already_verified=true` and posts/emits nothing.

There is intentionally **no hard DELETE for referrals**: reject is the
audit-preserving delete. **Gap (flagged):** no HTTP route lists payouts yet
(B's `PayoutStore.ListPayouts` has no handler — Agent C's payouts queue page
needs one; owner: Agent B's files).

`COMMISSIONS_ENABLED=false` (contract §7) → all endpoints above answer 503
(service not wired).

## 6. Events + W13 coordination (contract §6)

Topic `cac.events`, event_type `cac.funnel`, `FunnelEvent` payload per
SPEC-W13 §2. On a successful (non-replay) verify, after the durable commit:

1. **Lead conversion hook** — if the `referee_phone` matches an open lead
   of the tenant, the lead is walked to `converted` through the **leads
   service** (`leads.Service.Transition`, i.e. `new → contacted → qualified
   → converted` as needed; the leads service emits its own per-step
   FunnelEvents). Wave-13 exported no by-phone lookup, so the resolution
   uses the additive store seam `store.FindOpenLeadByPhone` (documented in
   `internal/store/referrals.go`) — leads rows are only mutated by the
   leads service itself. *Flagged:* W13's exported `Transition` needs a
   leadID; the by-phone resolution is the one store-level addition.
2. **Referral funnel hook** — one FunnelEvent with `entity_type=customer`,
   `entity_id=referee_phone`, `channel=referral`, `event_name=converted`
   (or `first_txn` when the trigger is `first_txn`), `amount_ngn` = verify
   base in NGN, idempotency key `referral:<referral_id>:<event_name>`.

Both hooks are **best-effort post-commit** (same posture as W13
`leads.emit`): the referral + postings are durable first; a hook failure is
logged for reconciliation, not rolled back.

Usage metering (`referral_verified`, `commission_payout` on
`usage.events`) is **Agent D's** (SPEC-W14 §Agent D) and is emitted from
D's metering path, not here.

## 7. Configuration (contract §7)

| Env                | Default        | Used by |
|--------------------|----------------|---------|
| `COMMISSIONS_ENABLED` | `true`      | A: endpoints 503 when false |
| `PAYOUT_PROVIDER`  | `paystack`     | B: payout provider |
| `PAYOUT_MIN_NGN`   | `100`          | B: minimum payout amount (naira) |
| `RECON_CRON`       | `30 2 * * *`   | B: recon schedule (Africa/Lagos 02:30) |
| `PAYOUT_MOCK`      | —              | B: mock provider for tests/dev |
| `CAC_EVENTS_TOPIC` | `cac.events`   | funnel hooks (W13, shared) |

## 8. Failure modes

- **RLS everywhere**: all four tables enable + force `tenant_isolation`
  (bootstrap guarded by `pg_policies` checks, like incidents/leads);
  `SET LOCAL app.tenant_id` per transaction.
- **Replay-safety**: referral dedupe (§1), verify short-circuit on
  non-pending rows (row lock), ledger unique key (§3), deterministic
  journal ids + event idempotency keys.
- **Unbalanced journal**: rejected before any write
  (`ErrUnbalancedJournal`) — programming error, never retried.
- **Hook failures** (cac.events enqueue, lead conversion): logged
  `... — reconcile`; the durable path never rolls back (same as W13).

## 9. Tests

- `internal/referrals/rules_test.go` — pure rules engine, table-driven
  (flat/percent/bps integer math, cap, priority, inactive, beneficiary
  resolution, zero-amount skip, determinism) + validation guards.
- `internal/referrals/service_test.go` — embedded Postgres (port 5548):
  end-to-end verify (rules → balanced postings → status → funnel hook),
  idempotent replay, revenue-trigger conversion + `first_txn` amount,
  referee-lead conversion walk + event set, reject flow, create dedupe,
  ledger known vectors (50000 accrued − 20000 paid = 30000; replays
  deduped).
- `internal/store/referrals_test.go` — dedupe, verify tx idempotency,
  reject transitions, rules CRUD + ordering, ledger store known vectors,
  lead-by-phone seam.
- `internal/httpapi/referrals_routes_test.go` — route wiring behind the
  tenant middleware.
