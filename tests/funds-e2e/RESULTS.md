# tests/funds-e2e — RESULTS

Status: **EXECUTED BY INDEPENDENT VERIFIER (V-Harness-R1, wave W41 repair round) — OVERALL PASS: 34/34 checks, exit 0.**

History (honest note): the R0 run of this harness (2026-08-16) **FAILED
19/34** — two harness bugs (missing `conversation/knowledge/kyc` DB
pre-creation; billing `DATABASE_URL` rejected by sqlx's url crate) plus ONE
REAL SERVICE BUG: billing-engine + payments-service pinned axum 0.7.9 while
their route tables used axum 0.8 `{param}` syntax, so every path-parameter
route 404'd. R1 (this file) verifies the claimed repairs: harness.py now
pre-creates all 6 DBs and uses a percent-encoded socket DSN
(`sqlx_dsn_for()`), and both Rust services' routes are rewritten to axum
0.7 `:param` syntax (new `tests/route_smoke.rs` in both crates). **All 34
assertions now pass on the committed tree, unpatched.**

## Environment (V-Harness-R1 run, sandbox local 2026-08-17)

| | |
|---|---|
| Host | sandbox, 2 CPU / 4 GB RAM |
| Python | 3.12.12 (pgserver 0.1.4, psycopg 3.x) |
| Go | go1.23.4 linux/amd64 (/tmp/go, GOPROXY=https://goproxy.cn,direct) |
| Rust | rustc/cargo 1.97.1 stable (rustup minimal), cmake via pip (rdkafka-sys) |
| Repo | /tmp/mirror — byte copy of the /mnt mirror (`OPENDESK_REPO` default = harness.py parents[2]); md5 of routes.rs x2 + harness.py verified identical to /mnt |
| XDG_RUNTIME_DIR | /tmp/xdg (0700) |

Binaries (all real builds from the committed sources, debug profile):
`identity-service` 17 219 492 B (md5 74b77dc1...), `booking-service`
45 733 987 B (md5 0cb6e1a1...) via `go build ./cmd/server`
(GOFLAGS=-mod=readonly). `billing-engine` 150 320 528 B (md5 ea116f20...),
`cargo build --locked` 5m17s. `payments-service` 108 793 408 B
(md5 9f1c80db...), `cargo build --locked` 2m14s. Exported via
`IDENTITY_BIN/BOOKING_BIN/BILLING_BIN/PAYMENTS_BIN`.

Command:

```bash
XDG_RUNTIME_DIR=/tmp/xdg FUNDS_E2E_PERF_ITERS=50 \
IDENTITY_BIN=... BOOKING_BIN=... BILLING_BIN=... PAYMENTS_BIN=... \
python3 tests/funds-e2e/harness.py --workdir /tmp/funds-e2e-r1
# exit code: 0
```

## Run R1 — committed harness.py verbatim: 34/34 PASS, exit 0

Wall time 4.31 s, 264 timed HTTP calls. Per-assertion results (verbatim from
`funds-e2e-summary.json`):

```
PASS service up: identity /healthz
PASS service up: booking /healthz
PASS service up: billing /healthz
PASS service up: payments /healthz
PASS identity: POST /v1/tenants -> 201   (id=c2034021-3aa1-4d26-bdce-444935880782, slug=e2e-38df848b)
PASS identity: GET /v1/tenants/{slug} returns id
PASS booking: GET /healthz -> 200
PASS booking: GET /public/sites/{slug} -> 200 (Dapr-free tenant ctx fallback)
PASS booking: GET /public/sites/{slug}/context -> 200 with 1 offering
PASS booking: GET /public/sites/{slug}/offerings -> 200 with seeded offering
PASS billing: PUT /v1/rate-cards/{t} -> 200          [R0: 404 — axum route bug, NOW FIXED]
PASS billing: POST /v1/invoices/generate -> 201   (id=15f4efbd-2225-44d2-97d7-a0f77ab3cf5a)
PASS billing: generated invoice subtotal == 3000 cents (30 calls x 100)   [R0: subtotal=0 cascade]
PASS billing: POST /v1/invoices/{id}/issue -> 200 status=issued           [R0: 404, NOW FIXED]
PASS billing: POST /v1/invoices/{id}/payment-link -> 200 mode=static (EMVCo payload, BILLING_STATIC_ACCOUNT)   [R0: 404, NOW FIXED]
PASS billing: restarted with PAYSTACK_SECRET_KEY, /healthz ok
PASS billing: webhook with WRONG signature -> 401   (negative control — HMAC really enforced)
PASS billing: webhook with REAL HMAC-SHA512 -> 200 status=paid            [R0: 409 cascade, NOW PAID]
PASS billing: invoice now paid   (GET /v1/invoices/{id} 200, status=paid) [R0: 404, NOW FIXED]
PASS billing: invoice_issued ledger transfer posted (code 200, deterministic uuid5 id)   rows=1
PASS billing: invoice_paid ledger transfer posted (code 202, deterministic uuid5 id)     rows=1
PASS billing: billing_outbox InvoicePaid row committed (same-tx durability, RS-001)      rows=1
PASS billing: webhook REPLAY -> 200 already_paid   (50 identical-byte replays)
PASS billing: replay caused NO second ledger posting   rows=1 (SQL count)
PASS billing: replay caused NO second outbox row       rows=1 (SQL count)
PASS payments: POST /v1/deposits (hold) -> 201   (deposit_id=0340cead-c7be-577a-baec-39f66f12fc59)
PASS payments: hold REPLAY same key -> same deposit_id (no double-hold)
PASS payments: POST /v1/deposits/{id}/capture -> 200                      [R0: 404, NOW FIXED]
PASS payments: capture REPLAY -> identical result, no double-post         [R0: 404, NOW FIXED]
PASS payments: GET /v1/accounts/{t}/balance -> 200 with accounts          [R0: 404, NOW FIXED]
PASS RLS: app_billing_login with WRONG app.tenant_id sees 0 invoices
PASS RLS: app_billing_login with '' app.tenant_id sees 0 invoices (W40-6 fail-closed)
PASS RLS: app_billing_login with GUC unset sees 0 invoices (fail-closed)
PASS RLS: app_billing_login with CORRECT app.tenant_id sees only its invoices (rows=13)
```

Exit code: **0**. 34/34. `[harness] OK: 34/34 checks passed, 264 timed calls, 4.31s`.

## Timings (input to tests/perf/aggregate.py)

`/tmp/funds-e2e-r1/timings/funds-e2e-timings.json` — 264 calls, wall 4.31 s.
Unlike R0, every budgeted line is now measured on the SUCCESS path:
invoice_generate n=50 p50=3.0 ms p99=12.6 ms (rated against a REAL rate
card); paystack webhook n=51 p50=2.4 ms p99=12.3 ms (1 real paid transition
+ 50 replays; the 401 negative control is a separate call name and is NOT in
these samples); deposit hold n=51 p50=1.3 ms p99=4.4 ms; deposit capture
n=100 p50=1.7 ms p99=2.6 ms (real 200 captures, not 404 fast-paths).
Aggregator: ATOMIC PASS, 4/5 measured; booking create NOT-MEASURED at HTTP
(Dapr-bound, EXTERNAL_BLOCKED) with the V-Go store bench (0.86 ms/op)
consumed as the bench note. See tests/perf/RESULTS.md.

## Adversarial confirmations (V-Harness-R1)

(a) Route fix is IN the binaries: `strings` on the built billing-engine
    shows the registered route blob `/v1/rate-cards/:tenant_id`,
    `/v1/invoices/:id[/issue|/void|/payment-link|/qr]`; the only `{...}`
    occurrences are doc/error-message strings, not route registrations.
    payments-service binary shows `/v1/deposits/:id/capture` and
    `/v1/accounts/:tenant_id/balance`; zero `{param}` route strings. Source
    side: `grep '\.route(' src/routes.rs` shows `:param` for every
    parameterized route in both services; md5 of both routes.rs and
    harness.py identical between the /mnt mirror and the /tmp build tree.
(b) Wrong-signature negative control really returns **401** (live, run R1)
    before the correctly-signed webhook returns 200 `{"status":"paid"}` —
    the HMAC is enforced, not stubbed.
(c) RLS legs run as `app_billing_login` (harness.py connects with
    `dsn_for(srv, "billing", user="app_billing_login", ...)`); the role is
    `LOGIN ... IN ROLE app_billing` with `app_billing NOLOGIN NOINHERIT`
    (05-app-roles.sql:90-94) and no SUPERUSER/BYPASSRLS anywhere in the
    script. All four probes passed live.
(d) Replay idempotency is asserted by SQL ROW COUNTS, not HTTP status:
    post-replay `SELECT count(*)` on `ledger_transfers` (deterministic
    uuid5 ids `billing-issued:{id}`/`billing-paid:{id}`) and
    `billing_outbox` must stay exactly 1 after 50 identical-byte webhook
    replays — observed rows=1/rows=1.

## Known environment limits (honest scope, unchanged from R0)

* Booking create / availability at HTTP level: Dapr-bound
  (resolver.go:108) — EXTERNAL_BLOCKED in sandbox, never mocked;
  store-level coverage via tests/race + the W41-5 benchmark (consumed by
  tests/perf/aggregate.py).
* Payments uses `LEDGER_IMPL=sim` (TigerBeetle EXTERNAL_BLOCKED); billing
  uses the durable postgres ledger and its rows ARE asserted in Postgres.
* Paystack live API is unusable in sandbox; static EMV payment-link mode is
  exercised and webhook signature verification is fully real (local
  HMAC-SHA512, negative + positive control both live-verified).
* Rust binaries are debug-profile builds; timings are indicative sandbox
  numbers, not production-representative.
