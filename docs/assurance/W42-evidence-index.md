# W42 Evidence Index — Residual-Cap Closure Wave (SPEC-W42)

Single index of every W42 artifact. Status legend (carried over from the
W41 index): **EXECUTED** = produced by a real run; **PENDING-EXECUTION** =
skeleton committed in-wave, filled by the fresh verifiers (SPEC-W42 — the
people executing did not write the code); **STATIC** = config/docs/code
artifact; **REPAIR-IN-PROGRESS** = execution ran but a defect was found,
verifier re-run pending; **PARTIAL** = partially executed, some
measurements blocked by a known defect.

> Honesty rule (SPEC-W41, carried into W42): RESULTS files that start
> PENDING-EXECUTION must be filled from a real execution log before the
> re-score. Absent execution, the associated rubric points stay unearned —
> this index does not assert results, it locates artifacts.

All gates G1–G5 have now been EXECUTED by fresh verifiers (with repair
rounds R1/R2 for the TigerBeetle lane — R0/R1 failure history is preserved in
the RESULTS files). Statuses below are final. STATIC entries are the
code/docs/corpus artifacts committed in-wave.

## 1. Real TigerBeetle client correctness (Coder R)

| Artifact | What it evidences | Status |
|---|---|---|
| `services/payments-service/src/ledger/**` | Pending-transfer post/void legs carry the hold's own code (`CODE_DEPOSIT_HOLD`); comments document the TB rule and that sim does not enforce code matching | STATIC |
| `services/payments-service/Cargo.toml` | tb-live feature wiring for the real TigerBeetle client | STATIC |
| `infra/docker-compose.core.yml` (tigerbeetle service) | Server pin 0.16.4 → 0.16.28 with the client/server-must-match comment | STATIC |
| payments-service unit tests (no server needed) | Every post/void leg constructed by capture/no-show-fee/void paths uses `CODE_DEPOSIT_HOLD`; mutation-testable | STATIC |
| `cargo build` + `cargo test --locked --features tb-live` run | tb-live build/tests pass in-sandbox | **EXECUTED** (Gate G1, R1: default 81 passed; tb-live 39+24+42 passed incl. 8 code-matching tests; R0 compile defects E0369/E0277 found and fixed) |
| Mutation check: flip a post leg back to code 101 → real-TB funds-e2e run | Defect catches (real TB rejects `pending_transfer_has_different_code`) | **EXECUTED** (Gate G2: unit-level mutant kills 4 tests; runtime mutant fails first capture with verbatim `PendingTransferHasDifferentCode` + `LinkedEventFailed`; R2 `is_idempotent_replay` cannot mask it) |

## 2. Booking tenant-resolution fallback (Coder G)

| Artifact | What it evidences | Status |
|---|---|---|
| `services/booking-service/**` | `IDENTITY_BASE_URL` direct-GET tenant resolution (default EMPTY = unchanged Dapr behavior); booking row + outbox row commit atomically, status stays `pending`, outbox `sent_at` NULL | STATIC |
| booking-service Go unit tests | httptest identity stub; fallback hit/miss/timeout/404 mapping; Dapr path untouched when env empty | STATIC |
| `go build`/`vet`/`test` run (Go 1.23.4, `-p 1`) | Gate G1 green for booking-service | **EXECUTED** (25 packages ok, 9/9 TestResolver* incl. wire-format pinning; mutation on the fallback path caught; `-race` ok) |

## 3. J-14 asyncpg tenant-GUC closure (Coder C)

| Artifact | What it evidences | Status |
|---|---|---|
| `services/conversation-service/**` | Tenant-scoped reads inside explicit transactions with `SET LOCAL app.tenant_id`; fail-closed on unresolvable tenant; no session-level GUC survives pool release | STATIC |
| conversation-service regression test | Interleaved tenants A/B on a size 1–2 pool, zero cross-tenant reads; unset-tenant session sees zero rows on a FORCE-RLS table | STATIC |
| `pytest` suite run | Gate G3 green incl. adversarial pool-reuse probes | **EXECUTED** (142 passed; mutant without the None-guard fails the suite; cross-tenant reads/writes denied on a non-superuser role under FORCE RLS) |

## 4. Funds-E2E v2 / perf throughput / restore-drill postgis mode (Coder H)

| Artifact | What it evidences | Status |
|---|---|---|
| `tests/funds-e2e/**` (harness v2) | Optional real-ledger mode (`TB_BINARY`, `LEDGER_IMPL=tigerbeetle`); booking-create coverage (public + authed, status=pending, outbox sent_at NULL, idempotent replay) | STATIC |
| `tests/funds-e2e/RESULTS.md` | Executed runs BOTH modes (sim + tigerbeetle): full assertion suite, real balance deltas, capture-replay `exists`/no double-post | **EXECUTED** (Gate G2: sim 42/42 ×2; TB 47/47 after R2 — real hold/capture deltas 5000→4875+125, replay byte-identical balances; R0/R1 failures recorded) |
| `tests/perf/aggregate.py` | Also computes sustained throughput (rps = N / wall) per call and compares p50/p99/rps against budgets B1–B7 with PASS/FAIL per budget; booking create measured at HTTP | STATIC |
| `tests/perf/RESULTS.md` | Executed perf aggregation incl. throughput vs budgets | **EXECUTED** (Gate G4: ATOMIC PASS 7/7 incl. sustained rps; B1/B2 booking create measured at HTTP; B6/B7 real-ledger numbers from the TB run) |
| `tests/restore-drill/drill.py` + READMEs | Optional system-PG mode (`DRILL_PG=system`) incl. postgis init script; honest SKIP with precise reason if postgis unavailable | STATIC |
| `tests/restore-drill/RESULTS.md` | Executed drill: pgserver mode; system-PG mode iff postgis installs, else documented SKIP with apt evidence | **EXECUTED** (Gate G5: pgserver 29/29 ×2; system mode with REAL postgis 3.6.4 via conda-forge (apt blocked, no root) → 30/30 ×2 incl. live `pg_extension` assertion; adversarial SKIP-cannot-masquerade checks) |

## 5. Docs/corpus compliance closure (Coder P)

| Artifact | What it evidences | Status |
|---|---|---|
| `docs/performance-budgets.md` | Methodology matches the aggregate.py contract: p50/p99 AND sustained rps per timed call vs budgets B1–B7, PASS/FAIL per budget; booking create measured at HTTP; environment caveats kept, no invented numbers | STATIC |
| `services/crm-sync-service/internal/httpapi/testdata/fuzz/FuzzVerifySignature/` | Go fuzz seed corpus committed in-tree (10 files, one per `f.Add(...)` seed in `webhook_fuzz_test.go`) — closes the SPEC-W41 corpus claim | STATIC |
| `tests/fuzz/README.md` | Corpus note updated: seeds in-tree; run-generated corpus still lands in `GOCACHE` | STATIC |
| `docs/runbooks/operations.md` §4 | TB server pin 0.16.28 + client/server-must-match rule; pending-transfer post/void legs carry the hold's transfer code (`pending_transfer_has_different_code`) | STATIC |
| This file | Index of the above | STATIC |

## 6. Gates & push stage

| Artifact | What it evidences | Status |
|---|---|---|
| Gates G1–G5 verifier evidence | Fresh-verifier execution records (build/test, funds-e2e both modes, J-14 regression, perf aggregation, restore-drill) | **EXECUTED** (all five gates green after repair rounds R1/R2; failure history preserved in RESULTS files) |
| Gate G6 push stage | Text-only push_files batches (≤15 files / ≤40KB), pinned-ref blob-SHA verification, full-tree audit at final HEAD, independent re-score vs the W39 rubric | **PENDING-EXECUTION** |
