# Growth dashboards — referrals & commissions (admin-web)

Wave 14 (SPEC-W14, Agent C). Admin UI for the referral & commission engine:
referral list + create + verify, tenant-editable commission rules, the
double-entry commission ledger, the payout queue with per-beneficiary
balances, and a per-referrer leaderboard.

## Routes and gating

All pages live under `app/app/[orgSlug]/growth/`; `/app/{orgSlug}/growth`
itself is an index that redirects to the first page the caller's roles allow
(referrals → rules → org overview).

| Route | Gate (server-side) | Permify equivalent | Content |
| --- | --- | --- | --- |
| `/growth/referrals` | `canViewAnalytics` (owner/admin/analyst) | `view_analytics` | Referral table, create form, verify action, referrer leaderboard |
| `/growth/rules` | `canViewBilling` (owner/billing) | `manage_billing` | Commission rules editor (CRUD + active toggle) |
| `/growth/ledger` | `canViewAnalytics` (owner/admin/analyst) | `view_analytics` | Double-entry ledger grouped by journal |
| `/growth/payouts` | `canViewBilling` (owner/billing) | `manage_billing` | Payout queue + outstanding balances per beneficiary |

- Gating mirrors the SPEC-W14 Agent C contract: `manage_billing` for
  rules/payouts (same gate helper as the billing page), `view_analytics`
  for the read views (referrals, ledger, leaderboard). Each page enforces
  its rule server-side and redirects everyone else to the org overview.
- Nav: one additive entry ("Growth", `Gift` icon) in
  `components/org-nav.tsx`, appended directly after the W13 "cac" entry and
  shown when the caller passes either gate (`canViewAnalytics ||
  canViewBilling`); no other nav changes.
- In-section navigation: `components/growth/growth-tabs.tsx` renders links
  to the four pages, filtered by the same two role flags (computed by each
  server page and passed down), so users never see a link that would bounce
  them.

## Data sources

All requests go through the app's established BFF proxy
(`app/api/[[...path]]/route.ts`) with the Keycloak access token and the
`x-tenant-slug` header attached (tenant passed as `?tenant=`), exactly like
the CAC pages.

| UI section | Endpoint | Contract |
| --- | --- | --- |
| Referral table | `GET /api/bookings/v1/referrals` | §1 list |
| Create referral | `POST /api/bookings/v1/referrals` | §1 create (`{referrer_type, referrer_id, referee_phone, campaign_id?}`) |
| Verify action | `POST /api/bookings/v1/referrals/{id}/verify` | §1 verify (fires rules → balanced postings; idempotent). Body: `{trigger, base_amount_ngn}` — `trigger` is required (`signup_verified` \| `first_booking` \| `first_txn` \| `sale`); `base_amount_ngn` is the integer-kobo revenue base percent rules on `first_txn`/`sale` are computed against (the UI collects whole naira and converts with `Math.round(x * 100)`). |
| Rules editor | `GET/POST /api/bookings/v1/commissions/rules`, `PUT/DELETE /api/bookings/v1/commissions/rules/{id}` | §2 CRUD (PUT is a full replacement — the active-toggle resends the whole rule with `active` flipped; list answers `{rules: [...]}`) |
| Ledger view | `GET /api/bookings/v1/commissions/ledger?from&to` | §3 |
| Balance cards | `GET /api/bookings/v1/commissions/balance/{beneficiary}` | §3 (sum of 300 credits − debits) |
| Payout queue, leaderboard paid totals | `GET /api/bookings/v1/payouts` | §4 list |

List responses are read through a tolerant `unwrap()` (first array-valued
own property, bare arrays included) — the same contract the CAC pages use —
so the UI works whether the backend answers with a keyed envelope
(`{referrals: [...]}` etc.) or a bare array. The balance endpoint's envelope
is not pinned by the contract; `extractBalance()` accepts a bare number or
`{balance_ngn|balance|amount_ngn|available_ngn}`.

### Soft failures

Until the Wave-14 backend ships, missing routes degrade gracefully (same
style as the CAC page's optional reads): 404s render a muted "not available
yet" note instead of failing the page; per-beneficiary balance reads and the
payouts read on the referrals page render "—" rather than fabricated zeros.

## UI structure

- `components/growth/types.ts` — contract §1–§4 types and pure helpers:
  `unwrap`, `formatNgn` (kobo → ₦ via the billing page's `formatMoney`,
  reused not duplicated), status→badge mappings, account-code labels,
  `extractBalance`, `buildLeaderboard`, `groupByJournal`.
- `components/growth/referral-create-form.tsx` — referrer type/id, referee
  phone, optional campaign id.
- `components/growth/referral-table.tsx` — status badges, timestamps,
  per-row **Verify** action for `pending` referrals.
- `components/growth/leaderboard.tsx` — top 10 referrers ranked by
  verified+converted counts with paid totals summed from paid payouts.
- `components/growth/rules-editor.tsx` — self-contained CRUD section
  (mirrors the billing page's InvoicesPanel): trigger / beneficiary /
  amount_type (flat ₦ or percent bps) / optional cap / priority / active
  toggle, edit-in-place and delete with confirm.
- `components/growth/ledger-table.tsx` — journals with per-entry
  debit/credit columns, account-code labels (300 payable, 301 expense,
  302 agent float, 303 house clearing), journal totals and a
  balanced/unbalanced badge computed client-side.
- `components/growth/payout-queue.tsx` + `balance-cards.tsx` — queue with
  provider/status/failure reason; outstanding payable per beneficiary.
- `components/growth/growth-tabs.tsx` — role-filtered sub-navigation.

## Conventions honoured

- No new dependencies: all visuals are hand-rolled with the existing
  `Card`/`Table`/`Badge`/`PageHeader`/`ErrorNote` primitives; warm
  low-saturation palette matches the CAC/analytics pages.
- **Money is integer kobo end-to-end** (contract §2: `amount_ngn int
  (kobo)`). The rules form converts whole-naira input with
  `Math.round(x * 100)`; percent rules take basis points (100 bps = 1%) as
  integers. Display reuses the billing page's `formatMoney(kobo, "NGN",
  "en-NG")` helper — imported, never duplicated. (The CAC pages format
  whole-naira fields; the W14 contracts are kobo-based, so the billing
  helper is the correct reuse here.)
- Rules editor notes the server-side evaluation semantics (priority
  ascending, inactive skipped, multiple rules may fire).

## Verification

`npx tsc --noEmit` (app tsconfig) is clean. Runtime behaviour depends on the
Wave-14 booking-service referrals/commissions API (Agents A/B) landing; all
reads degrade gracefully until then.
