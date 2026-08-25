# payments-service

OpenDesk payments service (SPEC §9). Ledger-centric: all money movement is
double-entry against a TigerBeetle-compatible `LedgerClient` trait
(ADR-0007 fallback: default build ships an in-memory sim ledger).

- Port: **7004** (SPEC §3). Dapr sidecar expected at `daprd-payments:3500`.
- Stack: Rust 2021, axum 0.7, tokio, reqwest; sqlx (Postgres) only for the durable
  `payout_attempts` reconciliation table (P-01/C3) — the ledger stays the source of truth.
- **NGN-only** (P-13): holds and payouts with `currency != "NGN"` are rejected with
  400 until multi-currency lands.
- **Auth (P-09, contract C1)**: tenant-scoped routes accept either a valid
  `X-Internal-Token` (constant-time compare against `PAYMENTS_INTERNAL_TOKEN`)
  or the gateway-injected `X-Tenant-Slugs` header whose list must contain the
  request's `tenant_id` (else 403). `/activities/*` and `/v1/internal/*`
  require the internal token. With no token configured, no gateway header and
  no dev escape, money routes fail closed with 503.
  `OPENDESK_TRUST_DIRECT_TENANT=1` is the documented dev escape (standalone,
  no gateway) — never set it in compose/production.
- **Idempotency (P-12, C5)**: money-moving endpoints (`/v1/deposits`,
  `/v1/refunds`, `/v1/no-show-fee`, `/v1/payouts`,
  `/v1/payments/flutterwave/initialize`) REQUIRE a non-empty `idempotency_key`
  (400 when absent). Capture is idempotent by construction (transfer id
  derived from the deposit id).

## Ledger model (SPEC §9)

Accounts per tenant: `tenant:{id}:deposits`, `tenant:{id}:revenue`; platform
accounts: `platform:fees`, `platform:clearing`, `platform:payouts`.
Amounts are minor units (cents). All transfers are idempotent by transfer id.

| Code | Meaning | Flow |
|---|---|---|
| 100 | deposit hold (pending) | `platform:clearing → tenant:{id}:deposits` (pending) |
| 101 | capture | posts hold, splits `deposits → revenue` (net) + `deposits → platform:fees` (fee) |
| 102 | refund | voids pending hold, or `revenue → platform:clearing` after capture |
| 103 | no-show fee | like capture, charges `amount_cents` of the hold, releases remainder |
| 104 | payout | `tenant:{id}:revenue → platform:payouts` (Mojaloop rail) |

`LEDGER_IMPL=sim` selects the in-memory double-entry ledger
(`src/ledger/sim.rs`) with TigerBeetle semantics (pending/posted/voided,
idempotent ids, `debits_must_not_exceed_credits` on liability accounts).
`LEDGER_IMPL=tigerbeetle` requires building with `--features tb-live`
(see `src/ledger/tigerbeetle.rs`; ADR-0007). SPEC-W34 GF11: `LEDGER_IMPL`
is **mandatory** — the service refuses to start when it is unset/unknown
(no silent sim fallback in a money path).

## REST API

| Method | Path | Body | Notes |
|---|---|---|---|
| GET | `/healthz` | — | liveness |
| POST | `/v1/deposits` | `{tenant_id, booking_id?, amount_cents, currency?, idempotency_key}` | hold (code 100); key REQUIRED; auto-provisions tenant accounts (P-10) |
| POST | `/v1/deposits/{id}/capture` | `{tenant_id, amount_cents?}` | capture (101), full when amount omitted (resolved via hold lookup, C4) |
| POST | `/v1/refunds` | `{tenant_id, deposit_id?, amount_cents, reason?, idempotency_key}` | void pending hold or post-capture refund (102); key REQUIRED; `amount_cents` on a pending hold must be 0 or the exact hold amount (P-11: partial => 400, never a silent full void) |
| POST | `/v1/no-show-fee` | `{tenant_id, deposit_id, amount_cents, booking_id?, idempotency_key}` | charge fee from hold (103); key REQUIRED |
| GET | `/v1/accounts/{tenant_id}/balance` | — | account snapshots (tenant-bound, P-09) |
| POST | `/v1/payouts` | `{tenant_id, amount_cents, currency, payee:{party_id_type, party_identifier}, idempotency_key}` | ledger-first two-phase payout (104): pending hold → rail → post/void (P-01/C3); key REQUIRED |
| POST | `/v1/internal/accounts/provision` | `{tenant_id}` | explicit idempotent account provisioning (internal token, P-10) |
| POST | `/activities/hold-deposit` | `{tenant_id, booking_id, amount_cents, currency?}` | Temporal `HoldDeposit` activity (SPEC §6); internal token |
| POST | `/activities/void-hold` | `{tenant_id, deposit_id? \| booking_id?}` | Temporal `VoidHold` compensation; internal token |

Idempotency: money-moving endpoints REQUIRE an explicit `idempotency_key`
(P-12/C5); transfer ids are derived deterministically from it, so retries are
safe. Temporal activity endpoints keep saga-deterministic ids and are
internal-token gated.

## Events & commands

- **Outbox** (SPEC §9): after each ledger op a CloudEvent
  (`com.opendesk.payments.{DepositHeld|DepositCaptured|RefundPosted|NoShowFeePosted|PayoutPosted|PaymentPosted}`)
  is published via Dapr pubsub component `pubsub-kafka` to topic
  `opendesk.payments.events` (`POST {DAPR_HOST}:{DAPR_HTTP_PORT}/v1.0/publish/pubsub-kafka/opendesk.payments.events`).
  Publication is best-effort: failures are logged and counted
  (`events_failed` counter), never rolled back — the ledger is the source of truth.
- **Commands**: consumes `opendesk.payments.commands` (ChargeDeposit, Refund,
  NoShowFee) with idempotent processing (transfer id derived from command id).

## Mojaloop adapter

`src/mojaloop.rs`: FSPIOP-style `POST {MOJALOOP_ENDPOINT}/quotes` then
`POST {MOJALOOP_ENDPOINT}/transfers` (mojaloop-simulator compatible, FSPIOP
headers included). Payout ordering is **ledger-first** (P-01, contract C3):
1. a PENDING payout transfer (`revenue → platform:payouts`) reserves the funds
   — an over-limit payout is rejected here with NO rail side effect;
2. the rail executes; ONLY an explicit `COMMITTED` from a well-formed response
   posts the payout (decode failure / missing state / `RECEIVED` / transport
   error after the transfer was sent are UNKNOWN — never defaulted to
   COMMITTED). The quote echo (amount + currency) is verified before the
   transfer is sent (P-08);
3. on rail failure/unknown the pending transfer is voided and a durable
   `payout_attempts` row is recorded (Postgres `payout_attempts` table,
   bootstrapped at startup; in-memory fallback only when no DSN is
   configured). A background reconciler
   (`PAYOUT_RECONCILER_INTERVAL_SECS`, default 30s) sweeps unknown rows,
   re-queries the rail (`GET /transfers/{id}`) and settles or fails them.
   A ledger post failure after a committed rail transfer is logged CRITICAL
   and swept by the reconciler.

## Env vars

| Var | Default | Description |
|---|---|---|
| `PORT` | `7004` | HTTP listen port |
| `RUST_LOG` | `info` | tracing filter (JSON logs) |
| `LEDGER_IMPL` | — (required; GF11) | `sim` \| `tigerbeetle` (latter needs `--features tb-live`); startup fails when unset |
| `TB_ADDRESSES` | `tigerbeetle:3000` | TigerBeetle replica addresses |
| `TB_CLUSTER_ID` | `0` | TigerBeetle cluster id (SPEC §9: cluster 0) |
| `KAFKA_BROKERS` | `kafka:9092` | Kafka bootstrap servers |
| `KAFKA_GROUP_ID` | `payments-service` | consumer group |
| `PAYMENTS_COMMANDS_TOPIC` | `opendesk.payments.commands` | commands topic |
| `DLQ_TOPIC` | `opendesk.dlq` | dead-letter topic for failed commands (GF11; offset commits only after the DLQ copy is durable) |
| `KAFKA_CONSUMER_ENABLED` | `true` | start the commands consumer |
| `DAPR_HOST` | `daprd-payments` | Dapr sidecar host (SPEC §3) |
| `DAPR_HTTP_PORT` | `3500` | Dapr sidecar HTTP port |
| `DAPR_PUBSUB_NAME` | `pubsub-kafka` | Dapr pubsub component |
| `PAYMENTS_EVENTS_TOPIC` | `opendesk.payments.events` | events topic |
| `MOJALOOP_ENDPOINT` | — (required unless sim opted in; SIM-003) | real Mojaloop rail base URL; startup fails closed when unset and `MOJALOOP_ALLOW_SIM` is not `true` |
| `MOJALOOP_ALLOW_SIM` | `false` | dev/CI opt-in: allow the payout rail to target the mojaloop-simulator (`http://mojaloop:8444` default) |
| `PLATFORM_FEE_BPS` | `250` | platform fee bps on capture/no-show; validated 0..=10000 at boot (P-05), checked arithmetic (overflow => 422) |
| `PAYMENTS_INTERNAL_TOKEN` | — (unset) | shared secret for `X-Internal-Token` (P-09/C1); unset + no gateway header + no dev escape => money routes fail closed 503 |
| `OPENDESK_TRUST_DIRECT_TENANT` | `false` | C1 dev escape: accept tenant context from request bodies without a gateway header; never set in production |
| `PAYMENTS_DATABASE_URL` | — (falls back to `DATABASE_URL`, then `PG_DSN`) | Postgres DSN for the durable `payout_attempts` table (P-01/C3); bootstrapped at boot, fail-closed when configured but unreachable; in-memory dev fallback when unset |
| `PAYOUT_RECONCILER_INTERVAL_SECS` | `30` | reconciler sweep interval for unknown payout rail outcomes |

## Run

```bash
cargo run                      # dev (sim ledger)
cargo test                     # sim ledger invariant/unit tests
cargo build --features tb-live # live TigerBeetle client
docker build -t opendesk/payments-service .
```

Graceful shutdown on SIGINT/SIGTERM; Kafka consumer drains via a shutdown
watch channel before exit.
