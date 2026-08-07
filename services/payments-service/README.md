# payments-service

OpenDesk payments service (SPEC §9). Ledger-centric: all money movement is
double-entry against a TigerBeetle-compatible `LedgerClient` trait
(ADR-0007 fallback: default build ships an in-memory sim ledger).

- Port: **7004** (SPEC §3). Dapr sidecar expected at `daprd-payments:3500`.
- Stack: Rust 2021, axum 0.7, tokio, reqwest (no sqlx — payments is ledger-centric).

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
| POST | `/v1/deposits` | `{tenant_id, booking_id?, amount_cents, currency?, idempotency_key?}` | hold (code 100) |
| POST | `/v1/deposits/{id}/capture` | `{tenant_id, amount_cents?}` | capture (101), full when amount omitted |
| POST | `/v1/refunds` | `{tenant_id, deposit_id?, amount_cents, reason?, idempotency_key?}` | void pending hold or post-capture refund (102) |
| POST | `/v1/no-show-fee` | `{tenant_id, deposit_id, amount_cents, booking_id?}` | charge fee from hold (103) |
| GET | `/v1/accounts/{tenant_id}/balance` | — | account snapshots |
| POST | `/v1/payouts` | `{tenant_id, amount_cents, currency, payee:{party_id_type, party_identifier}, idempotency_key?}` | Mojaloop quote→transfer, then ledger payout (104) |
| POST | `/activities/hold-deposit` | `{tenant_id, booking_id, amount_cents, currency?}` | Temporal `HoldDeposit` activity (SPEC §6) |
| POST | `/activities/void-hold` | `{tenant_id, deposit_id? \| booking_id?}` | Temporal `VoidHold` compensation |

Idempotency: pass `idempotency_key`; without one, ids are derived
deterministically from `booking_id` / `deposit_id` so retries are safe.

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
headers included). Payout ordering: rail first (deterministic `transferId`
from idempotency key), then ledger transfer; a ledger failure after a
committed rail transfer is logged CRITICAL for operator reconciliation.

## Env vars

| Var | Default | Description |
|---|---|---|
| `PORT` | `7004` | HTTP listen port |
| `RUST_LOG` | `info` | tracing filter (JSON logs) |
| `LEDGER_IMPL` | — (required; GF11) | `sim` \| `tigerbeetle` (latter needs `--features tb-live`); startup fails when unset |
| `TB_ADDRESSES` | `tigerbeetle:3000` | TigerBeetle replica addresses |
| `TB_CLUSTER_ID` | `0` | TigerBeetle cluster id (SPEC §9: cluster 0) |
| `PLATFORM_FEE_BPS` | `250` | platform fee in basis points on capture/no-show |
| `KAFKA_BROKERS` | `kafka:9092` | Kafka bootstrap servers |
| `KAFKA_GROUP_ID` | `payments-service` | consumer group |
| `PAYMENTS_COMMANDS_TOPIC` | `opendesk.payments.commands` | commands topic |
| `DLQ_TOPIC` | `opendesk.dlq` | dead-letter topic for failed commands (GF11; offset commits only after the DLQ copy is durable) |
| `KAFKA_CONSUMER_ENABLED` | `true` | start the commands consumer |
| `DAPR_HOST` | `daprd-payments` | Dapr sidecar host (SPEC §3) |
| `DAPR_HTTP_PORT` | `3500` | Dapr sidecar HTTP port |
| `DAPR_PUBSUB_NAME` | `pubsub-kafka` | Dapr pubsub component |
| `PAYMENTS_EVENTS_TOPIC` | `opendesk.payments.events` | events topic |
| `MOJALOOP_ENDPOINT` | `http://mojaloop:8444` | mojaloop-simulator base URL |

## Run

```bash
cargo run                      # dev (sim ledger)
cargo test                     # sim ledger invariant/unit tests
cargo build --features tb-live # live TigerBeetle client
docker build -t opendesk/payments-service .
```

Graceful shutdown on SIGINT/SIGTERM; Kafka consumer drains via a shutdown
watch channel before exit.
