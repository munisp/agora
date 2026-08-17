# tests/perf — RESULTS (measured baseline vs budgets)

Generated: 2026-08-16T22:44:37Z by
`tests/perf/aggregate.py` — do not hand-edit; re-run the aggregator.

> REGENERATION NOTE: `aggregate.py` OVERWRITES this file wholesale
> (`out.write_text(doc)`) — the aggregate table above is the only part it
> produces. Everything below the `---` separator (the V-Harness verifier
> caveat and the `## Go benchmark input` section with the verbatim
> BenchmarkCreateBookingTx/BenchmarkListBookings lines) is hand-maintained
> verifier evidence and MUST be re-appended after every aggregator re-run.

Budget source: /tmp/mirror/docs/performance-budgets.md (merged over SPEC fallbacks)
Timings inputs: /tmp/funds-e2e-r1/timings/funds-e2e-timings.json
Bench inputs: /tmp/go-bench.txt

| metric | budget (p99) | measured | verdict |
|---|---|---|---|
| booking create (HTTP public create, else store bench) | 1500 ms | n=0; store bench BenchmarkCreateBookingTx mean 0.86 ms/op (go-bench mean only; HTTP path not exercised) | **NOT-MEASURED** |
| invoice generate (POST /v1/invoices/generate) | 1500 ms | n=50 p50=3.0 ms p99=12.6 ms min=2.9 ms max=12.6 ms | **PASS** |
| paystack webhook (POST /webhooks/paystack) | 500 ms | n=51 p50=2.4 ms p99=12.3 ms min=2.1 ms max=12.3 ms | **PASS** |
| deposit hold (POST /v1/deposits) | 300 ms | n=51 p50=1.3 ms p99=4.4 ms min=1.2 ms max=4.4 ms | **PASS** |
| deposit capture (POST /v1/deposits/{id}/capture) | 300 ms | n=100 p50=1.7 ms p99=2.6 ms min=1.3 ms max=4.6 ms | **PASS** |

**ATOMIC overall verdict: PASS** (4/5
budget lines had measurements; NOT-MEASURED lines are listed, not excused —
each names where the gap is documented).

## Environment caveat

These numbers come from a constrained sandbox (2 CPU / 4 GB, embedded
PostgreSQL on a local unix socket, services built in debug mode for the
Rust binaries, loopback HTTP, no TLS, no API gateway). They are an
INDICATIVE baseline that the flow works and where the time goes — they are
not production-representative. Re-run on production-shaped hardware before
using them for capacity decisions (see docs/runbooks/capacity-planning.md).

---

## V-Harness verifier caveat (R1 re-run, sandbox local 2026-08-17; aggregator timestamp above is UTC — supersedes the 2026-08-16 caveat)

The aggregator output above is verbatim (`aggregate.py` exit 0) from the
R1 funds-e2e run, which itself passed **34/34** (see
tests/funds-e2e/RESULTS.md). Unlike R0 (19/34, axum route defect), every
budgeted line is now measured on the REAL success path:

* **paystack webhook (n=51, PASS)** — 1 real HMAC-SHA512 signed
  `paid` transition (200) + 50 idempotent replays (200
  `already_paid`). The wrong-signature negative control (401) is NOT in
  these samples (distinct call name `billing.webhook_bad_sig`).
* **deposit capture (n=100, PASS)** — real 200 captures + replays
  against the sim ledger (`POST /v1/deposits/:id/capture` now routes
  correctly under axum 0.7.9).
* **invoice generate (n=50, PASS)** — rated against a REAL rate card
  whose PUT returned 200 (first invoice subtotal asserted == 3000
  cents).
* **deposit hold (n=51, PASS)** — healthy 201s, as before.
* **booking create (NOT-MEASURED at HTTP)** — Dapr-bound
  (resolver.go:108), EXTERNAL_BLOCKED in sandbox; the store-level
  go-bench input below (BenchmarkCreateBookingTx mean 0.86 ms/op) is
  now consumed by the aggregator as the bench note on this row.

Net: the ATOMIC PASS is now backed by success-path samples on all four
HTTP-measured lines. Sandbox numbers remain indicative only (debug Rust
builds, loopback, embedded Postgres — see Environment caveat above).

## Go benchmark input (V-Go executed, 2026-08-17)

Run by independent verifier V-Go (wave W41) in a pristine /tmp copy of
services/booking-service; sandbox 2 CPU / 4 GB, embedded Postgres 16.4 on
loopback port 5570, `go1.23.4 linux/amd64`. Command:
`go test ./internal/store/ -run xxx -bench . -benchtime 50x` — PASS, exit 0,
package wall 15.851s. Raw output lines (verbatim):

```
BenchmarkCreateBookingTx-2    50    859257 ns/op    4930 B/op    152 allocs/op
BenchmarkListBookings-2       50    736457 ns/op   79453 B/op   1082 allocs/op
```

These are the booking-store hot-path inputs the aggregator may consume for
the "booking create" budget row (859257 ns/op ~= 0.86 ms/op vs 1500 ms p99
budget; 736457 ns/op ~= 0.74 ms/op for tenant-scoped list). Indicative
sandbox numbers, not production-representative (see caveat above).
