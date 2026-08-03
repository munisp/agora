# Commission Payouts & Nightly Recon (SPEC-W14, Agent B)

Lifecycle and operations for referral/commission cash-outs: the
`CommissionPayoutWorkflow`, the payout provider seam, and the nightly
`CommissionReconWorkflow`. Domain model, rules engine, ledger internals and
the REST surface are Agent A's — see `docs/referrals-commissions.md`.

## Payout lifecycle (contract §4)

```
queued ──▶ processing ──▶ paid
   │             │
   └─────────────┴──▶ failed (failure_reason set)
```

- A payout row (`commission_payouts`, RLS tenant-scoped) is created
  `queued` by the commissions path: `{payout_id, tenant_id, beneficiary_id,
  amount_ngn (kobo), status, provider, provider_ref, failure_reason}`.
- `CommissionPayoutWorkflow` (workflow ID `commission-payout-{payout_id}`,
  reject-duplicate ⇒ idempotent starts) executes it:
  1. `CommissionPayoutTransfer` activity — enforces `PAYOUT_MIN_NGN`,
     CAS `queued → processing` with the deterministic provider reference
     (`cpay_<sha256(payout_id)[:24]>`), calls the provider. Retried **3
     attempts, exponential backoff** (1s → 30s cap) per contract §4 — the
     Wave-5 webhook retry pattern is deliberately *not* reused.
  2. On success: `CommissionPayoutMarkPaid` — marks `paid` (+`paid_at`),
     writes the `commission_payout` usage-metering outbox row **in the same
     transaction**, then posts the balanced ledger pair **debit 300
     commission_payable / credit 302 agent_float** (idempotent on
     `(ref_type, ref_id, account_code)`, ref_type `commission_payout`).
  3. After exhausted retries: `CommissionPayoutMarkFailed` on a
     disconnected context with the error as `failure_reason`. A payout that
     is already `paid` is never flipped to `failed` — post-paid divergence
     is recon's job.

Idempotency: the provider reference is deterministic, so a crashed
activity attempt retries safely at the provider; a paid payout
short-circuits a replayed transfer activity.

## Provider seam (contract §4, PAYOUT_MOCK=1 default)

`referrals.PayoutProvider` — `Transfer` + `TransferStatus`:

| Setting | Provider |
|---|---|
| `PAYOUT_MOCK` unset/`1` (default) | deterministic `MockProvider` (no network) |
| `PAYOUT_MOCK=0`, `PAYOUT_PROVIDER=paystack` (default) | Paystack-shape HTTP client |
| `PAYOUT_MOCK=0`, `PAYOUT_PROVIDER=flutterwave` | flutterwave shim over the same assumed envelope |

**ASSUMPTION (annotated):** the Paystack transfer wire shape
(`POST /transfer {source:"balance", amount:<kobo>, recipient, reason,
reference}` → `data.{reference, transfer_code, status}`;
`GET /transfer/{reference}` → `data.status`) is an assumption —
payments-service has no Paystack transfer endpoint today (`/v1/payouts`
is a Mojaloop rail). Per contract §4 ("execution via payments-service Dapr
invoke"), deploy with `PAYOUT_PROVIDER_BASE_URL` pointed at the
payments-service Dapr invoke gateway
(`http://daprd-booking:3500/v1.0/invoke/payments/method`); if the adopted
wire shape differs, only `payouts.go` changes.

Mock test hooks (deterministic): beneficiary `mock-fail` declines,
`mock-pending` stays pending; a status lookup of a reference the mock
never issued returns `failed` (drives recon mismatch tests).

## Nightly recon (contract §5)

Temporal Schedule **`commission-recon-nightly`**, cron `RECON_CRON`
(default `30 2 * * *`) in **Africa/Lagos**, bootstrapped idempotently at
boot (`temporalclient.EnsureCommissionReconSchedule`; an existing —
possibly operator-paused — schedule is left untouched). Each run:

1. `CommissionReconFetchCandidates` — cross-tenant scan (annotated, like
   the outbox dispatcher) of payouts `processing` or `paid` within 72h.
2. One `CommissionReconCheckTransfer` activity per payout compares ledger
   status with provider transfer status:

   | ledger | provider | result |
   |---|---|---|
   | paid | success | consistent |
   | paid | failed/reversed/pending | `ledger_paid_provider_not_successful` |
   | processing | success | `ledger_processing_provider_successful` |
   | processing | failed/reversed | `ledger_processing_provider_failed` |
   | processing | pending | consistent (in flight) |

3. Every mismatch writes **both**: an outbox alert row (CloudEvent
   `com.opendesk.notifications.CommissionReconAlert`, `kind:
   commission_recon_alert`, to `opendesk.notifications.outbox` — the
   notification-worker consumer is forward-compatible and acknowledges
   unknown types, so the row is a durable alert + Kafka signal today) and
   a metered usage row (`commission_recon_alert` on
   `opendesk.usage.events`).

A failing check is logged and skipped — recon must be self-healing; the
payout is retried the next night. Manual re-drive:
`StartCommissionRecon` (unique workflow ID per run).

## Env (contract §7)

| Var | Default | Meaning |
|---|---|---|
| `COMMISSIONS_ENABLED` | off | gates worker registration + schedule bootstrap |
| `PAYOUT_MOCK` | `1` | deterministic mock provider |
| `PAYOUT_PROVIDER` | `paystack` | real rail when mock off |
| `PAYOUT_PROVIDER_BASE_URL` / `PAYOUT_PROVIDER_SECRET` | — | rail endpoint (Dapr invoke gateway in deploy) / API secret |
| `PAYOUT_MIN_NGN` | `100` | minimum payout, **whole naira** (ASSUMPTION: naira, ×100 ⇒ kobo floor of 10000) |
| `RECON_CRON` | `30 2 * * *` | recon schedule cron (Africa/Lagos) |

## REST surface

- `GET /v1/payouts?status=&limit=` (view_analytics) — the tenant payout
  queue, newest first (Agent C's admin payouts page). Served by
  `httpapi/payouts.go` on Agent B's `PayoutStore`; 503 when
  `COMMISSIONS_ENABLED != true`. Referral CRUD / verify, rules CRUD,
  ledger and balances are Agent A's endpoints (see
  `docs/referrals-commissions.md`).

## Coordination notes (post-integration)

- **Shared shapes**: `Payout`, `BalancedPosting` (with the BeneficiaryID
  extension — payout postings set it so per-beneficiary 300 balances
  decrease), `Ledger`, account codes 300–303 and `RefTypePayout` come from
  Agent A's `model.go` / `ledger.go` (+ `internal/store/referrals.go`,
  which bootstraps the same `commission_payouts` DDL — interchangeable
  with `PayoutStore.ensureSchema`, both idempotent). main.go wires
  `referrals.NewPostgresLedger(st)` (the TigerBeetle swap point).
- **Agent D**: the `commission_payout` metering row is emitted here
  (transactionally with mark-paid — the only place it can be atomic); D's
  metering item must not duplicate it. `commission_recon_alert` metering
  is also here per §5 "metered notification".
- **Metering topic**: both metering rows ride the existing outbox
  dispatcher (same pattern as `geo_campaign_message` /
  `incident_alert_message`).
