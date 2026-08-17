# tests/funds-e2e — funds flow end-to-end vs REAL Postgres + REAL binaries

One pgserver cluster (embedded full PostgreSQL 16, unix socket), four REAL
service binaries, REAL HMAC-SHA512 webhook signatures, RLS adversarial
probes with the least-privilege app role, and — since SPEC-W42 — the REAL
booking write path (via `IDENTITY_BASE_URL`) and an optional REAL
TigerBeetle ledger. **No mocks anywhere; Dapr is never faked.**

## Services booted (harness-local ports)

| service | port | key env |
|---|---|---|
| identity-service (Go) | 17001 | `DATABASE_URL`->pgserver `identity`, `INDUSTRIES_DIR=<repo>/industries` |
| booking-service (Go) | 17002 | `DATABASE_URL`->pgserver `booking`, `AUTHZ_DISABLED=true`, `CONSUMER_ENABLED=false`, `IDENTITY_BASE_URL=http://127.0.0.1:17001` |
| billing-engine (Rust) | 17012 | `DATABASE_URL`->pgserver `billing`, `BILLING_INTERNAL_TOKEN=<random hex/run>`, `BILLING_STATIC_ACCOUNT=OPENDESK/0123456789`, `BILLING_MERCHANT_NAME='OPENDESK DEMO'`, `KAFKA_CONSUMER_ENABLED=false` |
| payments-service (Rust) | 17004 | sim mode: `LEDGER_IMPL=sim`, `MOJALOOP_ALLOW_SIM=true`, `KAFKA_CONSUMER_ENABLED=false` — or TB mode below |

Keycloak/Permify/Dapr/Temporal endpoints are pointed at dead loopback ports
so their best-effort side effects fail fast and are logged — by design they
never fail the request path (see per-service source comments cited below).

## Booking write path (SPEC-W42 — now covered, honestly degraded)

Booking-service ≥ W42 supports `IDENTITY_BASE_URL` (Coder G): when set,
`TenantResolver.BySlug` issues a DIRECT HTTP GET
`{IDENTITY_BASE_URL}/v1/tenants/{slug}` instead of Dapr service invocation.
The harness sets it to the REAL in-harness identity-service, so the booking
write path runs end-to-end with **no Dapr, no Temporal, no Redis**:

| path | exercised | notes |
|---|---|---|
| `GET /healthz`, `GET /public/sites/{slug}` `[/context]` `[/offerings]` | yes | as before |
| `POST /v1/bookings` | **yes** | `Authorization: Bearer <token>` + `X-Tenant-Slug`; `AUTHZ_DISABLED=true` makes the Permify check a pass-through |
| `POST /public/sites/{slug}/bookings` | **yes** | anonymous; tenant resolved from the published site row + `IDENTITY_BASE_URL` |
| `GET /public/sites/{slug}/availability` | still no | resolver-independent availability is exercised through create's `checkSlot`; the GET path itself is not asserted here |

Seeding (SQL as superuser — the same fixture-data pattern the harness
already used for `sites`/`offerings`; normally the TenantOnboardingWorkflow
does this via Temporal): one published `sites` row, one offering, one team
member, and all-week `availability_rules` rows (the create path enforces
availability via `bookingops.checkSlot`).

Asserted per create call (both authed and public):

1. response `201`, body `status=pending`;
2. the booking row commits **atomically with its outbox row(s)** and stays
   `status=pending` — no Temporal means no saga confirms it (honest
   degraded mode; nothing fakes confirmation);
3. outbox row(s) for the booking stay `sent_at IS NULL` — the outbox
   dispatcher runs but its Dapr publish fails against the dead port, so
   rows are retained for retry (honest degraded mode; asserted via SQL);
4. replaying the same `idempotency_key` returns the ORIGINAL booking and
   `SELECT count(*) ... WHERE idempotency_key=...` stays exactly 1.

Both create calls are timed (`booking.create_authed`,
`booking.public_create_booking`) and repeated with fresh keys over
staggered slots when `FUNDS_E2E_PERF_ITERS>1` — these feed the B1/B2
budget lines in `tests/perf/aggregate.py` (MEASURED-at-HTTP; the store-level
`BenchmarkCreateBookingTx` remains a bench note, not the gate).

## Ledger modes (payments-service)

### Sim mode (default, `TB_BINARY` unset)

`LEDGER_IMPL=sim` — the in-memory sim ledger. The deterministic-id /
idempotency semantics under test, not durability. Unchanged from W41.

### Real-ledger mode (`TB_BINARY=/path/to/tigerbeetle`, SPEC-W42)

The harness drives a REAL TigerBeetle 0.16.28 server (the version pinned by
the client crate `tigerbeetle-unofficial 0.8.0+0.16.28` — client/server
wire protocol must match):

1. `tigerbeetle format --cluster=0 --replica=0 --replica-count=1
   <workdir>/0_0.tigerbeetle` then `tigerbeetle start
   --addresses=$TB_ADDRESS` (default `127.0.0.1:3000`) `--development`;
   waits for the TCP port.
2. Builds payments with `cargo build --locked --features tb-live`
   (`PAYMENTS_BIN` still honored — it MUST be a tb-live build; the service
   fails closed at boot otherwise). The tb-live build needs libclang
   (bindgen) and downloads the Zig toolchain at build time (network).
3. Pre-creates the five ledger accounts (tenant `deposits`/`revenue` +
   platform `fees`/`clearing`/`payouts`, ledger 1, ids = uuid v5
   URL-namespace of the account name — exactly `ledger::account_id`)
   through a **throwaway fixture crate the harness generates into the
   workdir** (`<workdir>/tb-fixture/`, never the repo) linking the SAME
   pinned client crate. The service intentionally exposes no HTTP endpoint
   for account creation; the sim ledger auto-creates accounts on hold while
   real TigerBeetle correctly refuses transfers to unknown accounts.
4. Boots payments with `LEDGER_IMPL=tigerbeetle TB_ADDRESSES=127.0.0.1:3000
   TB_CLUSTER_ID=0 PLATFORM_FEE_BPS=250`.

The FULL assertion suite runs in this mode too, plus real-ledger
assertions:

* the hold is visible as PENDING on the real ledger (`credits_pending`
  delta == hold amount on `tenant:{id}:deposits`);
* capture MOVES funds: balance deltas from `GET /v1/accounts/{t}/balance`
  match the capture response amounts exactly — deposits `credits_posted
  += post`, `credits_pending -= post`, `debits_posted += revenue+fee`,
  revenue `credits_posted += revenue` (with fee_bps=250: 5000 hold ->
  4875 revenue + 125 platform fee);
* capture REPLAY leaves every balance counter byte-identical (TigerBeetle
  answers `exists` for the replayed deterministic ids — no double-post).

**Single-replica `--development` semantics:** no replication, no storage
fault tolerance, NTP/clock checks relaxed. This mode proves client/server
correctness of the money path (incl. the W42 transfer-code fix: post/void
legs carry the hold's own code 100 or real TB rejects
`pending_transfer_has_different_code` and rolls the linked batch back) — it
is NOT an HA or durability certification.

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

## Payments flow (both ledger modes)

`POST /v1/deposits` (hold, explicit `idempotency_key`) → 201; replay with
the same key → identical `deposit_id` (no double-hold); `POST
/v1/deposits/{id}/capture` → 200 (TB mode sends an explicit
`amount_cents` — the live client's fee split requires it); replay capture →
byte-identical result, no double-post (deterministic capture id
`uuid v5("capture:{hold_id}")`); `GET /v1/accounts/{t}/balance` → 200.

## RLS adversarial (post-flow)

Direct connections to the `billing` DB as `app_billing_login` (role from
`05-app-roles.sql`): wrong `app.tenant_id` → 0 invoices; `app.tenant_id=''`
→ 0 (W40-6 NULLIF fail-closed); GUC unset → 0; correct tenant → only its
own rows.

## Environment reference

| env var | default | meaning |
|---|---|---|
| `TB_BINARY` | unset | path to a tigerbeetle 0.16.28 binary; set => real-ledger mode |
| `TB_ADDRESS` | `127.0.0.1:3000` | listen address for the harness-started TigerBeetle |
| `IDENTITY_BIN` / `BOOKING_BIN` / `BILLING_BIN` / `PAYMENTS_BIN` | unset | pre-built binaries (skip harness builds); in TB mode `PAYMENTS_BIN` MUST be a `--features tb-live` build |
| `GO_BIN` / `CARGO_BIN` | autodetect | toolchains for the harness builds |
| `FUNDS_E2E_PERF_ITERS` | 1 | repeat the idempotent hot calls N times (webhook replay, hold+capture pairs, invoice generate over distinct periods, booking creates over staggered slots) so the perf aggregator has N>=50 samples |
| `FUNDS_E2E_{IDENTITY,BOOKING,BILLING,PAYMENTS}_PORT` | 17001/17002/17012/17004 | harness-local service ports |
| `FUNDS_E2E_WORKDIR` | `/tmp/funds-e2e` | workdir (also positional `--workdir`) |
| `OPENDESK_REPO` | harness parents[2] | repo root (source of init scripts / binaries) |

## How to run

```bash
pip install pgserver==0.1.4 psycopg pytest
export XDG_RUNTIME_DIR=/tmp/xdg && mkdir -p $XDG_RUNTIME_DIR && chmod 700 $XDG_RUNTIME_DIR
# Go toolchain at /tmp/go/bin/go (or $GO_BIN / PATH); cargo for the Rust
# services. Or pre-build and export:
#   IDENTITY_BIN=... BOOKING_BIN=... BILLING_BIN=... PAYMENTS_BIN=...
python3 tests/funds-e2e/harness.py --workdir /tmp/funds-e2e
echo $?   # 0 = every check passed

# REAL-LEDGER mode (SPEC-W42): needs the tigerbeetle 0.16.28 binary
# (https://github.com/tigerbeetle/tigerbeetle/releases — client/server
# wire protocol must match the pinned tigerbeetle-unofficial 0.8.0+0.16.28),
# libclang-dev, and network for the crates.io + Zig downloads:
TB_BINARY=/usr/local/bin/tigerbeetle python3 tests/funds-e2e/harness.py --workdir /tmp/funds-e2e-tb
```

* Go builds: `go build ./cmd/server` with `GOFLAGS=-mod=readonly`,
  `GOCACHE`/`GOMODCACHE` inside the workdir. Sandbox note: proxy.golang.org
  is unreachable from some environments — set `GOPROXY=https://goproxy.cn,direct`.
* Rust builds: `cargo build --locked` (sim) / `cargo build --locked
  --features tb-live` (TB mode) with `CARGO_TARGET_DIR` inside the workdir —
  the source tree is never written to (safe on the /mnt mirror).
* Expected duration (2 CPU / 4 GB sandbox): schema+services ~1 min with
  pre-built binaries; first-time Go builds ~2-5 min each (module downloads);
  first-time cargo builds are the long pole (tens of minutes — pre-build or
  set `*_BIN`). TB mode adds the tb-sys build (Zig download) once.
* Outputs: `<workdir>/funds-e2e-summary.json` (includes `ledger_mode`),
  per-service logs in `<workdir>/logs/` (incl. `tigerbeetle.log` in TB
  mode), HTTP timings in `<workdir>/timings/funds-e2e-timings.json` (input
  to `tests/perf/aggregate.py`).

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
# optional real-ledger lane:
# - run: curl -L https://github.com/tigerbeetle/tigerbeetle/releases/download/0.16.28/tigerbeetle-x86_64-linux.zip -o tb.zip && unzip tb.zip
# - run: sudo apt-get install -y libclang-dev
# - run: TB_BINARY=$PWD/tigerbeetle python3 tests/funds-e2e/harness.py --workdir $RUNNER_TEMP/funds-e2e-tb
```

pgserver downloads its PostgreSQL binaries once per runner (network
required); both cargo builds fit a standard runner (use
`Swatinem/rust-cache` to keep the lane under ~20 min; the tb-live build
adds the Zig toolchain download on cache miss).

## Results

See [RESULTS.md](RESULTS.md).
