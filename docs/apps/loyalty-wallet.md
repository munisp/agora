# Loyalty & Wallet (`loyalty-wallet`)

SPEC-W19 Agent C — points programs, wallets, tiers and redemption.

- Backend: `services/booking-service/internal/loyalty/` (self-contained package;
  `RegisterRoutes` + `NewStore` per the W19 anti-collision contract — the
  integrator wires Deps/routes/config).
- UI: `apps/admin-web/app/app/[orgSlug]/apps/loyalty-wallet/` +
  `apps/admin-web/components/apps/loyalty-wallet/`.
- Entitlement: appgate `app_id = loyalty-wallet` (integrator gates the whole
  `/v1/loyalty` route group via the variadic middleware of `RegisterRoutes`).

## Model

- **loyalty_programs** — `{id, tenant_id, name, active, earn_rules jsonb,
  tiers jsonb, cap_per_day bigint DEFAULT 0, created_at, updated_at}`.
  `earn_rules`: `[{event: booking_completed|first_txn|referral_converted,
  points int}]` (unique per event, points > 0).
  `tiers`: `[{name, min_points, benefits}]`. `cap_per_day` caps points one
  contact can earn per **UTC day** across all accruals; `0` = uncapped;
  over-cap accruals are **clamped, not rejected**. Accruals resolve earn
  rules from the most recently updated **active** program.
- **loyalty_wallets** — `{tenant_id, contact_id, balance, lifetime_earned,
  lifetime_redeemed, tier, updated_at}`, PK `(tenant_id, contact_id)`.
  Created lazily on first accrual. `tier` = highest tier whose
  `min_points <= lifetime_earned` (`""` when none qualifies); recomputed on
  every accrual. Redemption never demotes (tier follows lifetime_earned).
- **loyalty_ledger** — points double-entry journal, a **mirror** of the W14
  `referrals.Ledger` pattern (`internal/referrals/ledger.go`). Direct reuse
  of `referrals.PostgresLedger` is impossible: its `validateJournal` and the
  `commission_ledger` CHECK constraint pin account codes 300..303 (kobo),
  while loyalty posts codes 400/401 (points) — and editing referrals is
  forbidden. The interface shape (`Post` / `PostBalanced` / `Balance` /
  `Entries`), the balanced-journal invariants and the idempotency anchor
  `UNIQUE (tenant_id, ref_type, ref_id, account_code)` are mirrored 1:1.
  - **400 `loyalty_points_issued`** — liability to the contact
    (`beneficiary_id = contact_id`). Spendable balance =
    `Balance(400, contact)` = credits − debits; `wallets.balance` is the
    in-tx maintained cache of that sum.
  - **401 `loyalty_points_redeemed`** — house-side flow account
    (`beneficiary_id = ""`).
  - Accrue journal: DEBIT 401 (house) / CREDIT 400 (contact),
    `ref_type=loyalty_accrual`, `ref_id = event:ref_id`.
  - Redeem journal: DEBIT 400 (contact) / CREDIT 401 (house),
    `ref_type=loyalty_redeem`, `ref_id = caller ref_id (or minted)`.

All three tables are FORCE-RLS `tenant_isolation` (embedded idempotent
`ensureSchema`, pg_policies-guarded — the devices/store.go idiom).

## Endpoints (mounted under `/v1` by the integrator)

Permissions: reads = `view_analytics`, writes = `manage_bookings`
(integrator wires via `Deps.Require`).

### Programs

```bash
# List (newest-updated first)
curl -H "X-Tenant-Slug: acme" $GW/v1/loyalty/programs

# Create → 201 {program}
curl -X POST -H "X-Tenant-Slug: acme" -H "content-type: application/json" \
  $GW/v1/loyalty/programs -d '{
    "name": "Club Rewards",
    "active": true,
    "earn_rules": [{"event":"booking_completed","points":50},
                   {"event":"first_txn","points":100}],
    "tiers": [{"name":"silver","min_points":100,"benefits":"priority support"},
              {"name":"gold","min_points":200}],
    "cap_per_day": 0
  }'

# Patch (partial; validated against the MERGED row)
curl -X PATCH -H "X-Tenant-Slug: acme" -H "content-type: application/json" \
  $GW/v1/loyalty/programs/{program_id} -d '{"active": false, "cap_per_day": 500}'
```

### Wallet view

```bash
# → {wallet, entries, ledger_balance}; 404 when the contact has no wallet yet.
# entries = the contact's 400-account ledger rows; optional ?from=&to=
# (YYYY-MM-DD). ledger_balance is the ledger-derived cross-check of
# wallet.balance.
curl -H "X-Tenant-Slug: acme" $GW/v1/loyalty/wallets/{contact_id}
```

### Accrue

```bash
# Idempotent on ref_id+event (ledger ref). Applies the active program's earn
# rule, enforces cap_per_day (clamps; capped=true), recomputes tier.
# → 200 {wallet, awarded, applied, capped, event}
# Replay of the same ref_id+event → 200 {applied: false, awarded: 0, wallet}.
curl -X POST -H "X-Tenant-Slug: acme" -H "content-type: application/json" \
  $GW/v1/loyalty/accrue -d '{
    "contact_id": "<uuid>",
    "event": "booking_completed",
    "ref_id": "booking-12345"
  }'
```

Errors: 400 unknown/unawarded event, missing fields, or no active program.

### Redeem

```bash
# → 200 {wallet, redeemed, applied, ref_id}
# Insufficient balance → 409 {"error":"insufficient_points","balance":N}
# ref_id is OPTIONAL but recommended: it anchors idempotent retries. Without
# it a fresh redemption id is minted, so a retried submission redeems TWICE.
curl -X POST -H "X-Tenant-Slug: acme" -H "content-type: application/json" \
  $GW/v1/loyalty/redeem -d '{
    "contact_id": "<uuid>",
    "points": 40,
    "reason": "voucher #123",
    "ref_id": "redeem-678"
  }'
```

### Leaderboard

```bash
# metric = lifetime_earned (default) | balance | lifetime_redeemed; limit ≤ 100
# → {entries: [{rank, contact_id, balance, lifetime_earned, lifetime_redeemed, tier}]}
curl -H "X-Tenant-Slug: acme" "$GW/v1/loyalty/leaderboard?metric=lifetime_earned&limit=20"
```

## Events (contract §5)

Lifecycle CloudEvents on topic **`opendesk.loyalty.events.v1`**
(`LOYALTY_EVENTS_TOPIC`; empty disables). Emitted best-effort via the
transactional outbox, on the non-idempotent path only:

- `com.opendesk.loyalty.PointsIssued` — data `{tenant_id, contact_id, event,
  ref_id, points, program_id, balance_after, tier, ts}`.
- `com.opendesk.loyalty.PointsRedeemed` — data `{tenant_id, contact_id,
  ref_id, reason, points, balance_after, ts}`.

## Metering (contract §4)

`points_redeemed` — one `com.opendesk.usage.UsageRecord` (value 1, meta
`{contact_id, points, ref_id}`) on the shared usage topic
(`USAGE_EVENTS_TOPIC`, default `opendesk.usage.events`), emitted once per
non-idempotent redemption.

## Config envs (integrator)

| Env | Default | Purpose |
| --- | --- | --- |
| `LOYALTY_EVENTS_TOPIC` | `opendesk.loyalty.events.v1` | lifecycle topic; empty disables emission |
| `USAGE_EVENTS_TOPIC` | `opendesk.usage.events` | shared metering topic; empty disables metering |
| `DATABASE_URL` | — | `DialStore` fallback pool (maxConns 4, devices idiom) |

App is functional with zero config. Integrator wires `Deps{Store,
TenantFromContext, Require, EventsTopic, UsageTopic}` and passes the appgate
`loyalty-wallet` entitlement middleware to `RegisterRoutes`.

**As delivered (W19 integrator):** `RegisterRoutes` mounts `/loyalty` —
the integrator calls it INSIDE httpapi's existing `/v1` route group (chi
panics on a second `Mount("/v1")`), so the group inherits httpapi's
tenant middleware directly; `TenantFromContext`/`Require` are attached to
the Deps in `httpapi.NewRouter` (they read package-private context keys)
and the appgate middleware is passed as the variadic gate as sketched.
Metering topic is wired from the existing `cfg.UsageEventsTopic`.

## Admin UI (`/apps/loyalty-wallet`)

- **Programs** tab — program cards + editor: friendly earn-rules form
  (event select + points rows), tiers form (name / min lifetime points /
  benefits), daily cap, active toggle; raw-JSON fallback for both jsonb
  blobs. Client-side validation mirrors the backend.
- **Wallets** tab — lookup by contact id; balance/lifetime/tier cards with a
  ledger cross-check warning; accrue + redeem action forms (manage roles
  only; 409 surfaces the current balance); per-contact ledger table.
- **Leaderboard** tab — rank by lifetime earned / balance / redeemed.

BFF paths: `/api/bookings/v1/loyalty/...` (tenant via `x-tenant-slug`).
Role guard: page readable by owner/admin/analyst (`view_analytics`); writes
need owner/admin/staff (`manage_bookings`) — enforced again by the backend.

## Limitations / honest notes

- **cap_per_day uses the UTC day**, not the tenant timezone (ledger
  `created_at >= date_trunc('day', now())`).
- A zero-award accrual (cap exhausted) posts **no journal** — replays
  recompute the same zero, so responses stay consistent, but capped-away
  points leave no audit row.
- Accrual resolves rules from the most recently updated **active** program;
  running multiple active programs concurrently is supported but only the
  latest one awards.
- `POST /redeem` without `ref_id` is NOT retry-safe (documented above).
- Concurrent accruals serialize on the wallet row lock; the cap check is
  race-safe per contact.
- Points are not money: no FX/expiry/transfer semantics yet.
