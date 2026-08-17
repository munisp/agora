# tests/funds-e2e — funds flow end-to-end vs REAL Postgres + REAL binaries

One pgserver cluster (embedded full PostgreSQL 16, unix socket), four REAL
service binaries, REAL HMAC-SHA512 webhook signatures, RLS adversarial
probes with the least-privilege app role. **No mocks anywhere; Dapr is
never faked.**

## Services booted (harness-local ports)

| service | port | key env |
|---|---|---|
| identity-service (Go) | 17001 | `DATABASE_URL`->pgserver `identity`, `INDUSTRIES_DIR=<repo>/industries` |
| booking-service (Go) | 17002 | `DATABASE_URL`->pgserver `booking`, `AUTHZ_DISABLED=true`, `CONSUMER_ENABLED=false` |
| billing-engine (Rust) | 17012 | `DATABASE_URL`->pgserver `billing`, `BILLING_INTERNAL_TOKEN=<random hex/run>`, `BILLING_STATIC_ACCOUNT=OPENDESK/0123456789`, `BILLING_MERCHANT_NAME='OPENDESK DEMO'`, `KAFKA_CONSUMER_ENABLED=false` |
| payments-service (Rust) | 17004 | `LEDGER_IMPL=sim`, `MOJALOOP_ALLOW_SIM=true`, `KAFKA_CONSUMER_ENABLED=false` |

Keycloak/Permify/Dapr/Temporal endpoints are pointed at dead loopback ports
so their best-effort side effects fail fast and are logged — by design they
never fail the request path (see per-service source comments cited below).

## Booking scope — verified Dapr-freeness (source citations)

Checked against `services/booking-service/internal/httpapi/server.go`,
`public.go`, and `internal/bookingops/resolver.go`:

| path | Dapr needed? | evidence |
|---|---|---|
| `GET /healthz` | no | store ping only |
| `GET /public/sites/{slug}` | tolerated | `public.go` `publicSite`: invoke failure → logged + empty tenant ctx |
| `GET /public/sites/{slug}/context` | tolerated | `publicContext`: invoke failure → `{slug}` fallback ctx |
| `GET /public/sites/{slug}/offerings` | **no** | pure store query |
| `GET /public/sites/{slug}/availability` | **YES** | `publicAvailability` → `Resolver.BySlug` → `dapr.InvokeService` (resolver.go:108), no static fallback |
| `POST /public/sites/{slug}/bookings` | **YES** | `publicCreateBooking` → `Resolver.BySlug` → Dapr |
| all `/v1/*` tenant routes | **YES** | `X-Tenant-Slug` middleware → same resolver |

Dapr (and Temporal, which would run the saga after a booking create) is
EXTERNAL_BLOCKED in this sandbox and is **never mocked**, so the booking
HTTP scope of this harness is exactly the four Dapr-free paths above. The
public `sites` row + one offering are seeded via SQL (superuser) as harness
fixture data — normally the TenantOnboardingWorkflow seeds the site via
`/internal/sites`. Booking-create correctness at the store layer is covered
by `go test -race` (tests/race) and the store benchmark (W41-5).

## Billing two-phase payment flow (verified constraint)

`payments_qr::paystack_initialize` hardcodes `https://api.paystack.co` and
`paystack_webhook` returns 503 when `PAYSTACK_SECRET_KEY` is unset
(`src/routes.rs`). Therefore a single billing-engine process cannot both
mint a payment link offline and accept webhooks. The harness:

1. **Phase A (static mode, no secret):** rate-card PUT, fixture usage rows
   (usage ingest is Kafka-only; consumer disabled), invoice generate (201,
   asserts subtotal == rated usage), issue (200), payment-link (200,
   `mode=static`, EMVCo payload with `BILLING_STATIC_ACCOUNT`).
2. **Phase B (webhook mode):** restarts the same binary with
   `PAYSTACK_SECRET_KEY=<random per-run test secret>`; invoice state
   persists in Postgres. Negative control: wrong signature → 401. Then a
   REAL `HMAC-SHA512(secret, body)` hex digest in `x-paystack-signature` →
   200 `{"status":"paid"}`; invoice `paid`; superuser SQL asserts:
   * `ledger_transfers` contains the deterministic invoice_paid transfer
     (`uuid v5("billing-issued:…"/"billing-paid:…")`, codes 200/202), and
   * exactly ONE `billing_outbox` InvoicePaid row — the outbox row commits
     IN THE SAME TRANSACTION as the paid transition (RS-001, routes.rs).
3. **Replay:** identical webhook bytes+signature → 200
   `{"status":"already_paid"}`, still exactly 1 ledger transfer, 1 outbox
   row (idempotency — `mark_paid_idempotent` + deterministic transfer ids).

## Payments flow

`POST /v1/deposits` (hold, explicit `idempotency_key`) → 201; replay with
the same key → identical `deposit_id` (no double-hold); `POST
/v1/deposits/{id}/capture` → 200; replay capture → byte-identical result,
no double-post (deterministic capture id `uuid v5("capture:{hold_id}")`,
sim.rs `capture_like` rebuild path); `GET /v1/accounts/{t}/balance` → 200.

Note `LEDGER_IMPL=sim` here is the in-memory sim ledger — the
deterministic-id/idempotency semantics under test, not durability
(TigerBeetle is EXTERNAL_BLOCKED in sandbox). Billing's ledger assertions
above use the durable `BILLING_LEDGER_IMPL=postgres` implementation.

## RLS adversarial (post-flow)

Direct connections to the `billing` DB as `app_billing_login` (role from
`05-app-roles.sql`): wrong `app.tenant_id` → 0 invoices; `app.tenant_id=''`
→ 0 (W40-6 NULLIF fail-closed); GUC unset → 0; correct tenant → only its
own rows.

## How to run

```bash
pip install pgserver==0.1.4 psycopg pytest
export XDG_RUNTIME_DIR=/tmp/xdg && mkdir -p $XDG_RUNTIME_DIR && chmod 700 $XDG_RUNTIME_DIR
# Go toolchain at /tmp/go/bin/go (or $GO_BIN / PATH); cargo for the Rust
# services. Or pre-build and export:
#   IDENTITY_BIN=... BOOKING_BIN=... BILLING_BIN=... PAYMENTS_BIN=...
python3 tests/funds-e2e/harness.py --workdir /tmp/funds-e2e
echo $?   # 0 = every check passed
```

* Go builds: `go build ./cmd/server` with `GOFLAGS=-mod=readonly`,
  `GOCACHE`/`GOMODCACHE` inside the workdir. Sandbox note: proxy.golang.org
  is unreachable from here — set `GOPROXY=https://goproxy.cn,direct`
  (verified working 2026-08-17).
* Rust builds: `cargo build --locked` with `CARGO_TARGET_DIR` inside the
  workdir — the source tree is never written to (safe on the /mnt mirror).
* Expected duration (2 CPU / 4 GB sandbox): schema+services ~1 min with
  pre-built binaries; first-time Go builds ~2-5 min each (module downloads);
  first-time cargo builds are the long pole (tens of minutes — pre-build or
  set `*_BIN`).
* Outputs: `<workdir>/funds-e2e-summary.json`, per-service logs in
  `<workdir>/logs/`, HTTP timings in
  `<workdir>/timings/funds-e2e-timings.json` (input to
  `tests/perf/aggregate.py`).
* `FUNDS_E2E_PERF_ITERS=N` (default 1) repeats the idempotent hot calls N
  times (webhook replay, hold+capture pairs, invoice generate over distinct
  periods) so the perf aggregator has N>=50 samples.

### CI mode (ubuntu runner)

```yaml
- run: pip install pgserver==0.1.4 psycopg pytest
- uses: actions/setup-go@v5
  with: {go-version: "1.23.4"}
- uses: dtolnay/rust-toolchain@stable
- run: mkdir -p /tmp/xdg && chmod 700 /tmp/xdg
- run: python3 tests/funds-e2e/harness.py --workdir $RUNNER_TEMP/funds-e2e
  env:
    XDG_RUNTIME_DIR: /tmp/xdg
```

pgserver downloads its PostgreSQL binaries once per runner (network
required); both cargo builds fit a standard runner (use
`Swatinem/rust-cache` to keep the lane under ~20 min).

## Results

See [RESULTS.md](RESULTS.md).
