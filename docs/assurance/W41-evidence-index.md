# W41 Evidence Index — Score-to-95 Remediation Wave (SPEC-W41)

Single index of every W41 artifact. Status legend:
**EXECUTED** = produced by a real run; **PENDING-EXECUTION** = skeleton
committed in-wave, filled by the fresh verifiers (SPEC-W41 Gate 3 — the
people executing did not write the code); **STATIC** = config/docs artifact;
**REPAIR-IN-PROGRESS** = execution ran but a defect was found (repair round
R1), verifier re-run pending; **PARTIAL** = partially executed, some
measurements blocked by a known defect.

> Honesty rule (SPEC-W41): RESULTS files that start PENDING-EXECUTION must
> be filled from a real execution log before the re-score. Absent execution,
> the associated rubric points stay unearned — this index does not assert
> results, it locates artifacts.

## 1. Funds / E2E (W41-2)

| Artifact | What it evidences | Status |
|---|---|---|
| `tests/funds-e2e/README.md` | How to run the harness (env, CI mode, pgserver notes) | STATIC |
| `tests/funds-e2e/harness.py` | Executable harness: one pgserver cluster (real Postgres), real identity/booking (Go) + billing-engine/payments-service (Rust) binaries, real HMAC-SHA512 webhook, idempotency replay, RLS tenant-deny | STATIC |
| `tests/funds-e2e/RESULTS.md` | Executed run: commands, key outputs, exit codes, timings | **EXECUTED** (R1 run: 34/34 assertions, exit 0, 264 timed calls, 4.31s wall; R0 FAIL on the axum route defect recorded in `tests/funds-e2e/RESULTS.md`) |

## 2. Backup / restore drill (W41-3)

| Artifact | What it evidences | Status |
|---|---|---|
| `tests/restore-drill/README.md` | How to run the drill | STATIC |
| `tests/restore-drill/drill.py` | pg_dump -Fc per DB → fresh pgserver cluster → pg_restore --no-owner --no-privileges → asserts (row counts, pg_policies, relforcerowsecurity, marker rows, app_billing_login tenant-deny) | STATIC |
| `tests/restore-drill/RESULTS.md` | Executed drill evidence | **EXECUTED** (two independent runs, 29/29 PASS each) |
| `infra/backups/backup.sh` | W41-fixed defaults: `PG_DBS` now includes notifications/billing/kyc/platform; `COMPOSE_NETWORK` default `agora_opendesk` matches compose project `agora` | STATIC (shellcheck-clean) |
| `docs/runbooks/operations.md` §6/§8 | Runbook references the executable drill + RESULTS location | STATIC |
| `docs/data-residency.md` §2.1 | Quarterly-drill requirement now names `tests/restore-drill/` | STATIC |

## 3. Performance (W41-5)

| Artifact | What it evidences | Status |
|---|---|---|
| `docs/performance-budgets.md` | p50/p99/throughput budgets B1–B7 + methodology + environment caveats | STATIC |
| `tests/perf/aggregate.py` | p50/p99/throughput aggregator (N ≥ 50 per hot call) | STATIC |
| `tests/perf/RESULTS.md` | Measured baseline vs budget table | **EXECUTED** (ATOMIC PASS, 4/5 budgeted calls measured: invoice_generate p99=12.6ms, webhook p99=12.3ms, hold p99=4.4ms, capture p99=2.6ms; booking create NOT-MEASURED at HTTP — Dapr-bound, covered by Go bench 0.86ms/op; debug-build/sandbox caveat stands) |
| `services/booking-service/internal/store/bench_test.go` | `BenchmarkCreateBookingTx` / `BenchmarkListBookings` vs embedded-postgres | STATIC (compile+run verified by V-Go) |
| `docs/runbooks/capacity-planning.md` §1a | Baseline structure + headroom formulas referencing `tests/perf/RESULTS.md` (no invented numbers) | STATIC |

## 4. Race / fuzz / property tests (W41-4, W41-6)

| Artifact | What it evidences | Status |
|---|---|---|
| `tests/race/RESULTS-booking.md` | `go test -race -p 1 ./...` on booking-service: pass/skip counts, race summary, wall time | **EXECUTED** (full `-race` run, 62m58s: 500 PASS / 0 FAIL / 1 SKIP, 0 data races) |
| `tests/fuzz/RESULTS.md` | Executed proptest + Go fuzz runs: commands, exec counts, durations, 0 crashes / counterexample handling | **EXECUTED** (Go fuzz: 373,912 execs, 0 failures + Rust `cargo test --locked` green in both crates incl. 10 property tests, mutation-tested; executed evidence now in `tests/fuzz/RESULTS.md`) |
| `services/payments-service/` proptest files + `Cargo.toml`/`Cargo.lock` | SimLedgerClient property tests: Σ debits == Σ credits, replay idempotence, no-overdraft, deterministic transfer ids | STATIC |
| `services/billing-engine/` proptest files + `Cargo.toml`/`Cargo.lock` | `verify_paystack_signature` properties: never panics, valid accepts, bit-flip/length-mismatch rejects | STATIC |
| `services/crm-sync-service/internal/httpapi/webhook_fuzz_test.go` (+ corpus) | `FuzzVerifySignature` seeds + 90 s fuzz execution | STATIC |

## 5. CI (W41-1)

| Artifact | What it evidences | Status |
|---|---|---|
| `.github/workflows/ci.yml` | Applied by maintainer directive in W41 (parked local-only in W39). Fixes: analytics-pipeline lane installs `-e '.[dev]'`; `workflow_dispatch` trigger added (e2e job was dead code); stale Cargo.lock comment corrected; new blocking `funds-e2e` lane | STATIC (yaml.safe_load_all parses; V-Docs re-lints) |
| `apps/admin-web/package-lock.json`, `apps/mobile/package-lock.json` | SYNC-OK vs package.json (recon-verified); pushed unchanged | STATIC |

## 6. Docs sweep (W41-7)

| Artifact | What it evidences | Status |
|---|---|---|
| `docs/slo-dashboards.md` | 1.3/4.2 rows now cite the W41 offline baseline; remaining "needs instrumentation" lines stay honest | STATIC |
| `docs/ROADMAP.md` G2/G3 | ci.yml applied + funds-e2e/race/restore/perf evidence locations | STATIC |
| `docs/industries.md` | customTool claims match pack YAMLs (ecommerce row fixed; travel/transportation already honest; consultancy's live `check_calendar_availability` targets the real booking service) | STATIC |

## 7. Wave ledger

| Artifact | What it evidences | Status |
|---|---|---|
| Engagement-workspace `plan.md` §W41 (ledger lives outside the repo tree — it is NOT a shipped in-tree artifact) | Push batches, per-file blob-SHA verification, audit ledger, any GitHub API rejection evidence for the workflow file | EXECUTED (append-only ledger) |
| This file | Index of the above | STATIC |
