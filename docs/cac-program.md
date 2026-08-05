# CAC Program — End-to-End Overview (Waves 12–14)

The **CAC App** is Agora's customer-acquisition-cost program: acquire
customers through every Nigerian channel that matters (USSD, SMS, WhatsApp,
QR/promo links, referrals), attribute each one to its first touch, measure
blended CAC and payback in realtime and batch, and reward the referrers and
agents who drive growth — all under Nigerian compliance guards (NCC 2442
DND, quiet hours, NDPA consent).

This page is the map. Each wave's deep-dive doc is linked from its section.

## The pipeline at a glance

```
 ACQUIRE                ATTRIBUTE               MEASURE                  REWARD
 ┌──────────────────┐   ┌────────────────────┐  ┌──────────────────────┐  ┌───────────────────────┐
 │ USSD (W12)       │   │ Lead entity + 24h  │  │ cac.events Kafka     │  │ Referrals (W14):      │
 │ SMS failover     │──▶│ dedup (W13)        │─▶│ funnel events (W13)  │─▶│ contact/agent/staff   │
 │ QR / promo / UTM │   │ First-touch channel│  │ Realtime rollups:    │  │ refer a phone, verify │
 │ Geo campaigns    │   │ + campaign + LGA   │  │ analytics-pipeline   │  │ on signup/first txn   │
 │ (W8, guarded W14)│   │ (W13)              │  │ (W13)                │  │ Commissions (W14):    │
 └──────────────────┘   └────────────────────┘  │ Lakehouse gold       │  │ rules engine →        │
        │                                       │ tables (W13)         │  │ double-entry ledger → │
        ▼                                       │ Admin dashboards     │  │ Paystack payouts +    │
 COMPLIANCE GUARDS (W12): NCC 2442 DND           │ (W13)                │  │ nightly recon (W14)   │
 suppression + quiet-hours deferral for ALL     └──────────────────────┘  └───────────────────────┘
 marketing sends; transactional never blocked
```

## Wave 12 — Channels & compliance guards

The acquisition surface and the rules every outbound message obeys.

- **USSD channel** — Africa's Talking USSD callback flow (`CON`/`END`
  responses, 180s session TTL) plus the Nigeria SMS aggregator failover
  chain (Africa's Talking → Termii): [channels-ussd.md](channels-ussd.md).
- **DND 2442 suppression & quiet hours** — the compliance contract every
  marketing send obeys: per-tenant opt-out then the global NCC 2442 list
  suppress *marketing* kinds (`geo_campaign`, `promo`, `broadcast`, `drip`);
  quiet hours (default **20:00–08:00 Africa/Lagos**, per-channel overrides)
  defer them with a durable workflow sleep; transactional traffic
  (confirmations, reminders, `incident_alert`, `otp`, …) is never
  suppressed or deferred: [dnd-quiet-hours.md](dnd-quiet-hours.md).
- Ops: [runbook-wave12.md](runbook-wave12.md).

## Wave 13 — Growth core: leads, attribution, funnels, CAC dashboards

Every acquisition becomes a **lead** with first-touch attribution; every
funnel step is an event; CAC is computed realtime and batch-verified.

- **Leads, attribution & campaign spend** (booking-service write path):
  lead entity with 24h dedup key, first-touch channel/campaign/LGA
  attribution, the `new → contacted → qualified → converted|lost` status
  machine emitting `cac.events` funnel events:
  [leads-attribution.md](leads-attribution.md).
- **QR / promo landing attribution** — how a QR scan, UTM link or
  `?promo=` share link becomes first-touch attribution at the edge:
  [qr-attribution.md](qr-attribution.md).
- **Realtime CAC analytics API** (analytics-pipeline): Kafka-consuming
  rollups behind the admin BFF: [cac-analytics-api.md](cac-analytics-api.md).
- **Lakehouse gold tables** — batch-verified CAC in Iceberg via
  `cac_analytics.py`: [cac-lakehouse.md](cac-lakehouse.md).
- **Admin CAC dashboards** — blended CAC, payback, by-channel tables:
  [cac-dashboards.md](cac-dashboards.md).

## Wave 14 — Referral & commission engine

Growth loops with real money: referrals create attributed leads, commission
rules pay referrers/agents/staff through a double-entry ledger and payout
providers, and every naira is reconciled nightly.

- **Referrals & commissions domain** (booking-service `internal/referrals`):
  referral entity (one open referral per tenant+referee phone), tenant-
  editable commission rules (`signup_verified|first_booking|first_txn|sale`
  triggers, flat-NGN or bps with caps, priority ordering), Postgres
  double-entry ledger (300 commission_payable / 301 commission_expense /
  302 agent_float / 303 house_clearing) with a documented TigerBeetle
  adapter seam, idempotent `POST /v1/referrals/{id}/verify` that fires the
  rules engine and posts balanced entries:
  [referrals-commissions.md](referrals-commissions.md).
- **Payouts & nightly recon** (Temporal): payout workflow (queue →
  provider transfer with 3-attempt backoff retry → paid + balanced
  300-debit/302-credit posting, else failed with reason) and the
  `CommissionReconWorkflow` cron (02:30 Africa/Lagos) comparing ledger
  payouts against provider transfer status, mismatching into
  `commission_recon_alert` outbox alerts:
  [commission-payouts.md](commission-payouts.md).
- **Growth admin UI** — referrals list/create, rules editor, ledger view,
  payouts queue, per-agent leaderboard:
  [growth-dashboards.md](growth-dashboards.md).
- **Compliance adoption for campaigns** — booking-service's
  `GeoCampaignWorkflow` now defers marketing sends through quiet hours and
  records `suppressed_dnd` outcomes (mirroring notification-worker's
  guards, service-boundary duplicated): see
  [dnd-quiet-hours.md](dnd-quiet-hours.md) §Coordination and
  `services/booking-service/internal/geo/quiet.go`.

## Cross-cutting invariants

- **Metering** — billable units are usage outbox rows on
  `opendesk.usage.events`, written transactionally with (or immediately
  after, best-effort) the action they meter so billing can never drift:
  `geo_campaign_message` (per campaign recipient actually sent —
  DND-suppressed sends are NOT metered), `incident_alert_message`,
  `referral_verified` (per non-idempotent referral verification),
  `commission_payout` (per paid payout, emitted same-tx at the paid
  transition), `commission_recon_alert` (per nightly recon mismatch).
- **Events** — funnel events are CloudEvents on Kafka `cac.events`
  (`com.opendesk.cac.FunnelEvent`); referral verification emits
  `first_txn`/`converted` hooks through the SAME leads service as W13, so
  referred customers appear in CAC dashboards with referral attribution.
- **Compliance** — marketing sends are DND-suppressed and quiet-hours
  deferred everywhere (W12 guards, adopted by geo campaigns in W14);
  transactional and priority traffic is never delayed.
- **Money math** — commission amounts are integer kobo (no floats);
  percent rules use basis points; every ledger posting is a balanced
  double-entry pair grouped by `journal_id`, idempotent on
  `(ref_type, ref_id, account_code)`.

## Configuration quick reference

| Env | Default | Wave | Meaning |
|---|---|---|---|
| `DND_ENFORCEMENT` | `true` | W12 | Master switch for DND suppression of marketing sends. |
| `QUIET_HOURS_DEFAULT` | `20:00-08:00` | W12 | Quiet-hours window, `HH:MM-HH:MM` tenant-local (default Africa/Lagos). |
| `QUIET_HOURS_OVERRIDES` | — | W12 | JSON per-channel windows, e.g. `{"sms":"22:00-06:00"}`. |
| `GEO_CAMPAIGN_BATCH` | `50` | W8/W14 | Geo campaign audience batch size. |
| `COMMISSIONS_ENABLED` | `true` | W14 | Referral/commission engine switch. |
| `PAYOUT_PROVIDER` | `paystack` | W14 | Payout provider (`paystack\|flutterwave`). |
| `PAYOUT_MIN_NGN` | `100` | W14 | Minimum payout amount. |
| `RECON_CRON` | `30 2 * * *` | W14 | Nightly commission recon schedule (02:30 Africa/Lagos). |

## Doc index

| Doc | Wave | Scope |
|---|---|---|
| [channels-ussd.md](channels-ussd.md) | W12 | USSD + SMS failover acquisition channels |
| [dnd-quiet-hours.md](dnd-quiet-hours.md) | W12 | DND 2442 + quiet-hours compliance guards |
| [runbook-wave12.md](runbook-wave12.md) | W12 | Wave-12 operations runbook |
| [leads-attribution.md](leads-attribution.md) | W13 | Leads, dedup, first-touch attribution, funnel events |
| [qr-attribution.md](qr-attribution.md) | W13 | QR/promo/UTM landing attribution |
| [cac-analytics-api.md](cac-analytics-api.md) | W13 | Realtime CAC rollups API |
| [cac-lakehouse.md](cac-lakehouse.md) | W13 | Batch CAC gold tables |
| [cac-dashboards.md](cac-dashboards.md) | W13 | Admin CAC dashboards |
| [referrals-commissions.md](referrals-commissions.md) | W14 | Referral entity, rules engine, ledger |
| [commission-payouts.md](commission-payouts.md) | W14 | Payout workflow + nightly recon |
| [growth-dashboards.md](growth-dashboards.md) | W14 | Growth admin UI |
| [compliance-ndpa.md](compliance-ndpa.md) | cross | NDPA consent basis for lead/marketing data |
