# tests/race — RESULTS: booking-service `go test -race -p 1 ./...`

Status: **EXECUTED** (filled by independent verifier V-Go, wave W41, from a
real run in a pristine /tmp copy; no hand-written counts).
Command: see [README.md](README.md). Never hand-write pass counts.

## Run 1 — 2026-08-17 (V-Go)

* Date/host: 2026-08-17 04:35:21 -> 05:38:19 CST, sandbox (2 CPU / 4 GB,
  Intel Xeon Platinum, loopback embedded Postgres 16.4)
* Go version: `go1.23.4 linux/amd64`
* Command: `go test -race -p 1 -v ./...` in a pristine /tmp copy of
  services/booking-service (`-v` added to enumerate skips; `-p 1` as per
  README to avoid embedded-postgres port collisions)
* Exit code: 0
* Wall time: 62m58s (04:35:21 -> 05:38:19 CST)

### Per-package results — 2026-08-17 (V-Go)

| package | pass | fail | skip | notes |
|---|---|---|---|---|
| cmd/server | - | - | - | [no test files] |
| internal/appgate | 17 | 0 | 0 | ok 1.137s |
| internal/availability | 16 | 0 | 0 | ok 1.014s |
| internal/bookingops | 14 | 0 | 0 | ok 1.124s |
| internal/cache | 11 | 0 | 0 | ok 1.227s |
| internal/campaignstudio | 26 | 0 | 0 | ok 270.624s (embedded PG) |
| internal/civic | 40 | 0 | 0 | ok 1.033s |
| internal/config | - | - | - | [no test files] |
| internal/consumer | 12 | 0 | 0 | ok 1.536s |
| internal/crm360 | 14 | 0 | 0 | ok 227.782s (embedded PG) |
| internal/daprc | - | - | - | [no test files] |
| internal/devices | 7 | 0 | 0 | ok 136.573s (embedded PG) |
| internal/events | - | - | - | [no test files] |
| internal/fieldcapture | 10 | 0 | 0 | ok 114.846s (embedded PG) |
| internal/geo | 44 | 0 | 0 | ok 1.217s |
| internal/helpdesk | 14 | 0 | 0 | ok 316.482s (embedded PG) |
| internal/httpapi | 45 | 0 | 0 | ok 142.675s (embedded PG) |
| internal/incidents | 8 | 0 | 0 | ok 47.511s (embedded PG) |
| internal/leads | 7 | 0 | 0 | ok 68.950s (embedded PG) |
| internal/lending | 35 | 0 | 1 | ok 319.106s; 1 skip, see taxonomy |
| internal/loyalty | 13 | 0 | 0 | ok 204.450s (embedded PG) |
| internal/optimize | 8 | 0 | 0 | ok 1.013s |
| internal/outbox | - | - | - | [no test files] |
| internal/permify | - | - | - | [no test files] |
| internal/referrals | 45 | 0 | 0 | ok 227.596s (embedded PG) |
| internal/socialpub | 35 | 0 | 0 | ok 281.674s (embedded PG) |
| internal/socialpub/provider | 5 | 0 | 0 | ok 1.012s |
| internal/store | 26 | 0 | 0 | ok 546.890s (embedded PG; incl. W41-5 bench fixtures compiled, not run) |
| internal/surveys | 13 | 0 | 0 | ok 115.125s (embedded PG) |
| internal/temporalclient | - | - | - | [no test files] |
| internal/workforce | 22 | 0 | 0 | ok 363.768s (embedded PG) |
| internal/workorders | 13 | 0 | 0 | ok 205.076s (embedded PG) |
| **TOTAL** | **500** | **0** | **1** | 26 packages ok, 6 packages [no test files] |

### Race detector output summary — 2026-08-17 (V-Go)

* `WARNING: DATA RACE` count: **0** (grep over the full -v log; no race
  trace was emitted by any package)
* Skips enumerated by taxonomy (SHORT / EXTERNAL_BLOCKED / ENV /
  PREEXISTING):
  * ENV x1: `--- SKIP: TestKYCServiceGate (22.64s)` (internal/lending,
    handlers_test.go:334) — `embedded postgres unavailable: process already
    listening on port 5565`. A stale embedded-postgres listener from the
    preceding package run held port 5565 despite `-p 1`; the test's own
    Skipf fired. Infrastructure/fixture limitation, not a product-code
    failure; no product assertion was bypassed (the Skipf path is the
    package's documented constrained-environment behavior).
  * SHORT / EXTERNAL_BLOCKED / PREEXISTING: 0 each (run was not `-short`).
* 501 tests started (`=== RUN`), 500 `--- PASS`, 1 `--- SKIP`, 0 `--- FAIL`.
