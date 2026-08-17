# tests/perf — performance budget aggregation

`aggregate.py` turns raw measurements into the budget verdict table
(`RESULTS.md`):

* **HTTP timings** — every public call in tests/funds-e2e/harness.py is
  timed with `time.perf_counter` and dumped to
  `<workdir>/timings/funds-e2e-timings.json`. Run the harness with
  `FUNDS_E2E_PERF_ITERS=50` (or higher) so the hot calls have N>=50 samples.
* **Go benchmarks (optional)** — `services/booking-service/internal/store`
  benchmarks (W41-5 `bench_test.go`): copy the repo to /tmp, then
  `cd services/booking-service && go test -bench=. -benchmem -run='^$' ./internal/store/ | tee /tmp/booking-bench.txt`
  (requires network for the embedded-postgres binary download; skip-tagged
  like the existing store tests under `-short`).

## Run

```bash
python3 tests/perf/aggregate.py \
  --timings /tmp/funds-e2e/timings/funds-e2e-timings.json \
  --bench /tmp/booking-bench.txt \
  --out tests/perf/RESULTS.md
echo $?   # 0 = no measured budget line exceeded
```

Budgets are read from `docs/performance-budgets.md` when present;
otherwise the SPEC-W41 fallbacks apply (booking create p99<=1.5 s, invoice
generate p99<=1.5 s, paystack webhook p99<=500 ms, deposit hold/capture
p99<=300 ms) and the source is labeled in RESULTS.md.

Verdicts are per line (PASS / FAIL / NOT-MEASURED with the gap named) plus
one ATOMIC overall verdict. Sandbox numbers are indicative only — see the
environment caveat in RESULTS.md.
