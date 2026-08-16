# Performance Budgets — Funds & Booking Hot Paths (SPEC-W41 W41-5)

Latency and throughput budgets for the revenue-critical HTTP paths. Budgets
are **normative gates**, not measurements: the measured baseline lives in
`tests/perf/RESULTS.md` (filled by the W41 perf aggregation run — see
§Methodology). SLO alignment: the customer-API budgets below are the
per-route decomposition of `docs/slo-dashboards.md` SLO 1
(p50 ≤ 300 ms, p99 ≤ 1.5 s).

## 1. Budget table

| # | Operation | Endpoint | p50 budget | p99 budget | Throughput budget | Measured baseline |
|---|-----------|----------|-----------:|-----------:|------------------:|-------------------|
| B1 | Booking create (authenticated) | `POST /v1/bookings` (booking-service) | ≤ 300 ms | ≤ 1.5 s | ≥ 25 rps | see `tests/perf/RESULTS.md` (W41 measured baseline) |
| B2 | Public booking create (anonymous, via published site) | `POST /public/sites/{slug}/bookings` (booking-service) | ≤ 300 ms | ≤ 1.5 s | ≥ 25 rps | see `tests/perf/RESULTS.md` (W41 measured baseline) |
| B3 | Invoice generate | `POST /v1/invoices/generate` (billing-engine) | ≤ 300 ms | ≤ 1.5 s | ≥ 25 rps | see `tests/perf/RESULTS.md` (W41 measured baseline) |
| B4 | Invoice issue | `POST /v1/invoices/{id}/issue` (billing-engine) | ≤ 300 ms | ≤ 1.5 s | ≥ 25 rps | see `tests/perf/RESULTS.md` (W41 measured baseline) |
| B5 | Paystack webhook handling (HMAC verify + ledger post + outbox, one tx) | `POST /webhooks/paystack` (billing-engine) | ≤ 150 ms | ≤ 500 ms | ≥ 100 rps | see `tests/perf/RESULTS.md` (W41 measured baseline) |
| B6 | Deposit hold | `POST /v1/deposits` (payments-service) | ≤ 100 ms | ≤ 300 ms | ≥ 100 rps | see `tests/perf/RESULTS.md` (W41 measured baseline) |
| B7 | Deposit capture | `POST /v1/deposits/{id}/capture` (payments-service) | ≤ 100 ms | ≤ 300 ms | ≥ 100 rps | see `tests/perf/RESULTS.md` (W41 measured baseline) |

Rationale for the tighter tiers:

* **B5 (webhook ≤ 500 ms p99):** provider webhooks have delivery timeouts and
  aggressive retry schedules; a slow handler amplifies into duplicate
  deliveries. The handler must stay cheap: verify HMAC, post ledger + outbox
  in one transaction, return 200. (Idempotency under replay is a correctness
  gate covered by `tests/funds-e2e`, not a latency budget.)
* **B6/B7 (hold/capture ≤ 300 ms p99):** these sit on the interactive
  booking-confirm path where the caller is waiting; the ledger client is
  in-process (`sim`) or a single LAN hop (TigerBeetle), so there is no
  justification for multi-hundred-ms latency.
* **B1–B4** inherit the platform customer-API SLO (SLO 1) unchanged.

## 2. Methodology

1. **HTTP-level timings — `tests/funds-e2e/harness.py`.** The funds E2E
   harness boots real service binaries against a real (embedded, pgserver)
   Postgres and times every public call it makes, including the idempotent
   replay calls. Run it with `python tests/funds-e2e/harness.py --workdir
   <dir>` (the harness's only flag; CI passes a per-runner workdir).
2. **Aggregation — `tests/perf/aggregate.py`.** Repeats the hot calls
   (N ≥ 50 iterations each), computes p50/p99 and sustained throughput, and
   writes the comparison-against-budget table to `tests/perf/RESULTS.md`.
   A run **fails** if any measured p99 exceeds its budget above.
3. **Store-level micro-benchmarks — Go `testing.B`.**
   `services/booking-service/internal/store/bench_test.go`
   (`BenchmarkCreateBookingTx`, `BenchmarkListBookings`) runs against
   embedded-postgres and isolates DB-transaction cost from HTTP/middleware
   overhead; run on demand with
   `go test -bench=. -benchmem ./internal/store/ -args ...` (see the file's
   header comment; skipped under `-short` like the other store tests).
4. **Continuous monitoring.** Budgets above are release-gate numbers. The
   steady-state production signal is the Prometheus/Grafana path defined in
   `docs/slo-dashboards.md` (gateway-side latency is live today; per-service
   histograms are still pending instrumentation — that doc tracks status
   honestly).

## 3. Environment caveats (read before comparing numbers)

* The W41 measured baseline (`tests/perf/RESULTS.md`) is produced in the
  **sandbox/CI environment**: embedded Postgres (pgserver) on shared,
  burstable compute (≈2 CPU / 4 GB), `LEDGER_IMPL=sim` for payments, Kafka
  consumers disabled. These numbers are **indicative, not
  prod-representative** — embedded PG I/O, no network hop between services,
  and no connection-pool contention make them optimistic in some dimensions
  and noisy in others.
* Budgets gate a release only when re-measured on **staging-like
  infrastructure** (real Postgres, TigerBeetle ledger, production-shaped
  payloads). Re-run `tests/perf/aggregate.py` against staging **per release**
  and append the run to `tests/perf/RESULTS.md`; never overwrite a prior
  run's evidence — append with a date + environment header.
* If the sandbox baseline already violates a budget, that is a real signal
  (sandbox is the *easy* environment for these paths): investigate before
  shipping; do not relax the budget to fit a regression.
* Throughput budgets assume the dev default connection pools
  (`docs/runbooks/capacity-planning.md` §3); capacity headroom arithmetic
  against the measured baseline lives in
  `docs/runbooks/capacity-planning.md` §1a.
