# billing-engine (Rust, :7012)

OpenDesk usage metering, rating/invoicing, QR payments, and dunning
(SPEC-W7 Part B). Full architecture and operator docs: [`docs/billing.md`](../../docs/billing.md).

## What it does

- **B1 Metering** — consumes CloudEvents from `opendesk.usage.events`
  (group `billing-engine`), idempotently anchored on
  `processed_events(event_id)`, into `usage_records`.
- **B2 Rating & invoicing** — per-tenant `rate_cards` (seeded free/standard/pro
  `plan_presets`), monthly invoice generation
  (`max(0, total - included_quota) * unit_price_cents`), status machine
  `draft -> issued -> paid | past_due` (+ `void`), double-entry receivables
  postings to the sim ledger (codes 200 AR-control / 201 revenue /
  202 payments-clearing).
- **B3 QR payments** — `POST /v1/invoices/{id}/payment-link` (Paystack
  initialize when `PAYSTACK_SECRET_KEY` is set, else a static EMVCo-style
  payload from `BILLING_STATIC_ACCOUNT`), `GET /v1/invoices/{id}/qr` (SVG),
  `POST /webhooks/paystack` (HMAC-SHA512 verified, idempotent paid transition
  + `com.opendesk.billing.InvoicePaid` to `opendesk.billing.events`), and the
  `DUNNING_INTERVAL_S` dunning sweep.

## API surface

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| GET | `/healthz` | — | liveness + counters |
| GET | `/metrics` | — | Prometheus text exposition (`billing_events_published_total`, `billing_events_failed_total`) |
| PUT | `/v1/rate-cards/{tenant_id}` | tenant | upsert one rate-card row |
| POST | `/v1/invoices/generate` | tenant + owner/admin | generate/replace draft for `{tenant_id, period:"YYYY-MM"}` |
| GET | `/v1/invoices?tenant_id=&status=` | tenant | list invoices |
| GET | `/v1/invoices/{id}` | tenant | invoice detail |
| POST | `/v1/invoices/{id}/issue` | tenant | draft -> issued (+ ledger) |
| POST | `/v1/invoices/{id}/void` | tenant + owner/admin | -> void |
| POST | `/v1/invoices/{id}/payment-link` | tenant | Paystack or static EMV link |
| GET | `/v1/invoices/{id}/qr` | tenant | SVG QR of the payment ref |
| POST | `/webhooks/paystack` | HMAC signature | charge.success -> paid |

ALL routes except `/healthz`, `/metrics` and `/webhooks/paystack` additionally
require the `x-internal-token` header equal to `BILLING_INTERNAL_TOKEN`
(RS-002; APISIX strips any client-supplied copy and injects the real token —
a missing/wrong token is 401 before any tenant check runs).
"tenant" auth = `X-Tenant-ID` header must equal the target tenant id.
"owner/admin" = the gateway-injected `x-user-roles` header must contain the
Keycloak realm role `owner` or `admin` (403 otherwise).

## Environment

| Var | Default | Notes |
| --- | --- | --- |
| `PORT` | `7012` | listen port |
| `DATABASE_URL` | `postgres://opendesk:opendesk@postgres:5432/billing` | per-service role supported (`BILLING_PG_USER/PASS` in compose). GF6 (SPEC-W34): `migrations/0002_rls.sql` enables FORCE RLS on all billing tables; the service sets `app.tenant_id` transaction-locally per request/tenant (`src/tenant.rs`), and the policies are fail-closed without it |
| `INTERNAL_DATABASE_URL` | unset | GF6: DSN for cross-tenant internal jobs (dunning sweep, Paystack webhook lookup) — point at the `app_billing_internal_login` role when `DATABASE_URL` uses least-privilege `app_billing_login`; unset = share the main pool (fine for the dev superuser default, which bypasses RLS) |
| `KAFKA_BROKERS` | `kafka:9092` | |
| `KAFKA_GROUP_ID` | `billing-engine` | usage consumer group |
| `USAGE_EVENTS_TOPIC` | `opendesk.usage.events` | B1 source |
| `KAFKA_CONSUMER_ENABLED` | `true` | |
| `BILLING_EVENTS_TOPIC` | `opendesk.billing.events` | B3 InvoicePaid topic (produce only). RS-001: events are written to the durable `billing_outbox` table in the same transaction as the paid commit and relayed with backoff — the topic MUST be provisioned (Kafka auto-create is OFF) |
| `BILLING_LEDGER_IMPL` | `postgres` | SIM-029: `postgres` = durable DB-backed ledger (default; boot FAILS if the ledger tables/DB are unavailable); `sim` = in-memory dev/CI opt-in (never silently selected) |
| `BILLING_INTERNAL_TOKEN` | — (required; RS-002) | boot fails when unset. Every request except `/healthz`, `/metrics`, `/webhooks/paystack` must carry `x-internal-token` equal to this value; APISIX strips client copies and injects it |
| `PAYSTACK_SECRET_KEY` | unset | when set: Paystack mode + webhook signature enforced |
| `PAYSTACK_DEFAULT_EMAIL` | `billing@opendesk.local` | fallback customer email |
| `PAYSTACK_CALLBACK_URL` | `http://localhost:9080/billing/callback` | |
| `BILLING_STATIC_ACCOUNT` | — (required in static mode; SIM-030) | static EMV merchant account; boot fails closed when PAYSTACK_SECRET_KEY is unset and this is empty |
| `BILLING_MERCHANT_NAME` | — (required in static mode; SIM-030) | static EMV merchant display name; same fail-closed rule |
| `DUNNING_INTERVAL_S` | `3600` | sweep cadence (min 60) |
| `INVOICE_DUE_DAYS` | `14` | issued -> past_due age |

## Build & test

```sh
cargo build --release
cargo test            # rating math, CRC16, HMAC-SHA512, state machine, ledger
docker build -t opendesk/billing-engine .
```

No Rust toolchain is available in the authoring environment (same constraint
as payments-service): the dependency set mirrors payments-service plus only
`sqlx 0.7` and `qrcode 0.14` per SPEC-W7, and tests are reviewed statically.

## Schema

`migrations/0001_init.sql` is embedded (`include_str!`) and applied
idempotently at boot (same bootstrap pattern as notification-worker). The
`billing` database is created by `infra/postgres/init-scripts/00-create-dbs.sql`;
least-privilege role grants (`app_billing` / `app_billing_login`) are in
`05-app-roles.sql`.
