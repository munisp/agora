# tests/perf — RESULTS (measured baseline vs budgets)

Generated: 2026-08-17T03:52:27Z by
`tests/perf/aggregate.py` — do not hand-edit; re-run the aggregator.

Budget source: /tmp/ws/docs/performance-budgets.md (7/7 budget rows parsed; merged over SPEC-W42 fallbacks)
Timings inputs: /tmp/funds-sim/timings/funds-e2e-timings.json, /tmp/funds-sim2/timings/funds-e2e-timings.json
Bench inputs: (none)

Method: p50/p99 nearest-rank percentiles plus sustained throughput
(rps = N / summed measured wall of the N serially-issued timed calls),
each compared against its budget dimension. A line is PASS only when every
measured dimension passes. NOT-MEASURED lines are listed, not excused —
each names where the gap is documented. Booking-create lines (B1/B2) are
MEASURED-at-HTTP only when the harness actually exercised the booking write
path (SPEC-W42: IDENTITY_BASE_URL direct-GET tenant resolution, degraded
no-Dapr/no-Temporal posture — see tests/funds-e2e/README.md).

| # | metric | budgets | measured | verdict |
|---|---|---|---|---|
| B1 | booking create, authenticated (POST /v1/bookings) | p50 <= 300 ms; p99 <= 1500 ms; >= 25 rps | n=51 min=3.1 ms max=38.0 ms | p50=5.1 ms <= 300 PASS; p99=38.0 ms <= 1500 PASS; rps=118.5 >= 25 PASS | **PASS** |
| B2 | public booking create (POST /public/sites/{slug}/bookings) | p50 <= 300 ms; p99 <= 1500 ms; >= 25 rps | n=51 min=3.7 ms max=20.3 ms | p50=4.8 ms <= 300 PASS; p99=20.3 ms <= 1500 PASS; rps=166.8 >= 25 PASS | **PASS** |
| B3 | invoice generate (POST /v1/invoices/generate) | p50 <= 300 ms; p99 <= 1500 ms; >= 25 rps | n=51 min=4.6 ms max=27.2 ms | p50=7.0 ms <= 300 PASS; p99=27.2 ms <= 1500 PASS; rps=113.1 >= 25 PASS | **PASS** |
| B4 | invoice issue (POST /v1/invoices/{id}/issue) | p50 <= 300 ms; p99 <= 1500 ms; >= 25 rps | n=2 min=7.1 ms max=9.8 ms | p50=7.1 ms <= 300 PASS; p99=9.8 ms <= 1500 PASS; rps=118.4 >= 25 PASS | **PASS** |
| B5 | paystack webhook (POST /webhooks/paystack) | p50 <= 150 ms; p99 <= 500 ms; >= 100 rps | n=53 min=2.3 ms max=26.7 ms | p50=2.4 ms <= 150 PASS; p99=26.7 ms <= 500 PASS; rps=288.3 >= 100 PASS | **PASS** |
| B6 | deposit hold (POST /v1/deposits) | p50 <= 100 ms; p99 <= 300 ms; >= 100 rps | n=53 min=1.3 ms max=20.4 ms | p50=2.4 ms <= 100 PASS; p99=20.4 ms <= 300 PASS; rps=275.5 >= 100 PASS | **PASS** |
| B7 | deposit capture (POST /v1/deposits/{id}/capture) | p50 <= 100 ms; p99 <= 300 ms; >= 100 rps | n=102 min=1.3 ms max=25.8 ms | p50=1.8 ms <= 100 PASS; p99=2.9 ms <= 300 PASS; rps=477.2 >= 100 PASS | **PASS** |

**ATOMIC overall verdict: PASS** (7/7
budget lines had measurements).

## Environment caveat

These numbers come from a constrained sandbox (2 CPU / 4 GB, embedded
PostgreSQL on a local unix socket, services built in debug mode for the
Rust binaries, loopback HTTP, no TLS, no API gateway; the throughput figure
is SEQUENTIAL single-client throughput, not a concurrency load test — see
tests/load/ for that). They are an INDICATIVE baseline that the flow works
and where the time goes — they are not production-representative. Re-run on
production-shaped hardware before using them for capacity decisions (see
docs/runbooks/capacity-planning.md).

---

> REGENERATION NOTE: `aggregate.py` OVERWRITES this file wholesale
> (`out.write_text(doc)`) — the aggregate table above is the only part it
> produces. Everything below the `---` separator (the V-Harness verifier
> caveat and the `## Go benchmark input` section with the verbatim
> BenchmarkCreateBookingTx/BenchmarkListBookings lines) is hand-maintained
> verifier evidence and MUST be re-appended after every aggregator re-run.

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

## W42 executed evidence (fresh verifier V-W42, G4, 2026-08-17)

The aggregate table ABOVE is the W42 regeneration: command
`python3 tests/perf/aggregate.py --timings
/tmp/funds-sim/timings/funds-e2e-timings.json --timings
/tmp/funds-sim2/timings/funds-e2e-timings.json --out
tests/perf/RESULTS.md` -> exit 0, `ATOMIC: PASS; measured=7/7`.
Budget source: `docs/performance-budgets.md` (7/7 rows parsed, merged over
SPEC-W42 fallbacks). Timings come from the two executed W42 sim-mode
funds-e2e runs (run A: full suite 42/42 PASS, 23 timed calls; run B:
`FUNDS_E2E_PERF_ITERS=50`, 42/42 PASS, 366 timed calls — see
tests/funds-e2e/RESULTS.md, W42 EXECUTED section).

* **B1/B2 booking create: now MEASURED-at-HTTP** (n=51 each) — the W42
  harness exercises the real booking write path via IDENTITY_BASE_URL
  direct-GET tenant resolution (no Dapr). B1: p50=5.1 ms, p99=38.0 ms,
  rps=118.5 (budget 300/1500 ms, 25 rps) PASS. B2: p50=4.8 ms,
  p99=20.3 ms, rps=166.8 PASS. The W41 NOT-MEASURED gap is closed at HTTP
  level; the go-bench note below remains as store-level context.
* **B3-B7 all PASS** with n>=2..102 (table above); B7 deposit capture
  rps=477.2 sequential single-client.
* **REAL-LEDGER (TB) timings: ABSENT — documented gap.** The W42 TB-mode
  run fails at build time (tb-fixture E0369; payments-service tb-live
  2xE0369 at src/ledger/tigerbeetle.rs:241,305 — see
  tests/funds-e2e/RESULTS.md W42 section), so no tigerbeetle-mode timings
  file exists to aggregate. B6/B7 numbers above are sim-ledger only.
  Exit-code contract honored: aggregator exit 0 reflects sim-mode
  measurements only.

Environment identical to the funds-e2e W42 runs (2 CPU / 4 GB sandbox,
embedded PostgreSQL 16, debug Rust builds, loopback HTTP — indicative
baseline only, see Environment caveat above).


---

## W42 R1 (repair round) — TB real-ledger perf leg (verifier W42-gate-G2+G4-R1, 2026-08-17)

Source timings: /tmp/funds-tb/timings/funds-e2e-timings.json from the TB-mode harness run
(`TB_BINARY=/tmp/tigerbeetle FUNDS_E2E_PERF_ITERS=50`, ledger_mode="tigerbeetle", 370 timed calls,
harness wall 23.16s warm / 934.76s cold incl. builds). Aggregator:
`python3 tests/perf/aggregate.py --timings /tmp/funds-tb/timings/funds-e2e-timings.json`.
B6/B7 now have REAL-LEDGER numbers (real TigerBeetle 0.16.28 single-replica --development
underneath, payments-service built --features tb-live).

| # | metric | measured (TB real ledger) | verdict |
|---|---|---|---|
| B1 | booking create, authenticated | n=50 p50=3.9 ms p99=8.1 ms rps=255.2 | PASS |
| B2 | public booking create | n=50 p50=3.9 ms p99=5.9 ms rps=250.3 | PASS |
| B3 | invoice generate | n=50 p50=3.1 ms p99=5.6 ms rps=305.2 | PASS |
| B4 | invoice issue | n=1 p50=8.9 ms p99=8.9 ms rps=112.7 | PASS |
| B5 | paystack webhook | n=51 p50=1.6 ms p99=8.7 ms rps=554.6 | PASS |
| B6 | deposit hold (REAL TB pending transfer) | n=51 p50=2.5 ms p99=3.1 ms rps=391.7 | PASS |
| B7 | deposit capture (REAL TB linked post+split batch) | n=50 p50=2.4 ms p99=3.8 ms rps=404.7 | PASS |

Aggregator verdict line: `ATOMIC: PASS; measured=7/7` — all seven budgets measured and passing
in TB real-ledger mode. Caveat: the 50 B7 samples are first-time captures of fresh holds; the
capture-REPLAY path 502s in TB mode (see tests/funds-e2e/RESULTS.md W42 R1 section — separate
functional defect, not a perf regression). Same sandbox caveats as the R0 section apply
(2 CPU / 4 GB, debug builds, sequential single-client throughput).


---

## W42 R2 — perf re-aggregation on the repaired TB-mode run

    python3 tests/perf/aggregate.py --timings /tmp/funds-tb-r2/timings/funds-e2e-timings.json
    ATOMIC: PASS; measured=7/7   (exit 0)

| # | metric | budgets | measured | verdict |
|---|---|---|---|---|
| B1 | booking create, authenticated | p50<=300ms; p99<=1500ms; >=25rps | n=50 p50=4.0ms p99=10.5ms rps=233.8 | **PASS** |
| B2 | public booking create | p50<=300ms; p99<=1500ms; >=25rps | n=50 p50=3.9ms p99=20.3ms rps=231.9 | **PASS** |
| B3 | invoice generate | p50<=300ms; p99<=1500ms; >=25rps | n=50 p50=3.1ms p99=6.8ms rps=301.5 | **PASS** |
| B4 | invoice issue | p50<=300ms; p99<=1500ms; >=25rps | n=1 p50=6.9ms p99=6.9ms rps=145.3 | **PASS** |
| B5 | paystack webhook | p50<=150ms; p99<=500ms; >=100rps | n=51 p50=1.6ms p99=8.9ms rps=555.4 | **PASS** |
| B6 | deposit hold (real ledger) | p50<=100ms; p99<=300ms; >=100rps | n=51 p50=2.6ms p99=3.6ms rps=376.7 | **PASS** |
| B7 | deposit capture (real ledger) | p50<=100ms; p99<=300ms; >=100rps | n=100 p50=2.3ms p99=2.6ms rps=433.0 | **PASS** |

Unlike R1, the capture-REPLAY call is no longer a failure excluded from perf: replay now returns 200
(is_idempotent_replay fix) and its 50 samples are measured inside B7 (n=100 = 50 captures + 50 replays).
B6 includes hold + 50 hold replays (n=51). Same sandbox caveats as R0/R1 apply.
