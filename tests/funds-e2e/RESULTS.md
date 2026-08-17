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


---

## W42 — EXECUTED (fresh verifier V-W42, gates G2+G4, 2026-08-17)

Verification-only execution of the committed W42 v2 harness
(`tests/funds-e2e/harness.py`, unmodified) by a verifier that did not write
the code. Workspace: pristine copy of the repo at `/tmp/ws`; builds run with
`GOPROXY=https://goproxy.cn,direct`, `GOFLAGS=-mod=readonly -p=1`,
`XDG_RUNTIME_DIR=/tmp/xdg`; `go1.23.4 linux/amd64`; `cargo 1.97.1` /
`rustc 1.97.1` (rustup stable, ustc dist mirror); `pgserver==0.1.4`
(embedded PostgreSQL 16), `psycopg 3.3.4`; TigerBeetle server binary
`0.16.28+e97e337` (`/tmp/tigerbeetle`, zip sha verified by unzip -t);
`libclang` from the pip wheel
(`LIBCLANG_PATH=~/.local/lib/python3.12/site-packages/clang/native`) plus
`BINDGEN_EXTRA_CLANG_ARGS=-I/usr/lib/gcc/x86_64-linux-gnu/12/include
-I/usr/include/x86_64-linux-gnu` — REQUIRED: the libclang pip wheel ships no
builtin headers, so bindgen otherwise dies on `stddef.h`; Zig 0.13.0
tarball prefetched from ziglang.org and sha256-verified against the
official index (`d45312e6...1230ea`) because the per-connection throttle
made the in-build download impractical (served to the vendored
`zig/download` script via a PATH wget shim; no repo/harness file touched).

### 1. SIM mode — PASS (exit 0, 42/42), twice

Run A (full builds, default iters):
`python3 tests/funds-e2e/harness.py --workdir /tmp/funds-sim`
-> `[harness] OK: 42/42 checks passed, 23 timed calls, 462.81s` (wall
includes identity 45s + booking 70s + billing 211s + payments 134s builds),
timings -> `/tmp/funds-sim/timings/funds-e2e-timings.json`, summary ->
`/tmp/funds-sim/funds-e2e-summary.json`.

Run B (PERF_ITERS=50, same binaries via IDENTITY_BIN/BOOKING_BIN/
BILLING_BIN/PAYMENTS_BIN, fresh workdir/pgdata):
`FUNDS_E2E_PERF_ITERS=50 ... python3 tests/funds-e2e/harness.py --workdir
/tmp/funds-sim2` -> `[harness] OK: 42/42 checks passed, 366 timed calls,
7.7s`, 0 FAIL lines.

The NEW W42 booking-create assertions all PASS (run A observed values):
* `booking: POST /v1/bookings (authed, IDENTITY_BASE_URL resolution) -> 201
  pending` — status=201, body has id/status=pending (Coder G's direct-GET
  fallback against the REAL identity-service works; no Dapr anywhere).
* `booking: authed create committed booking row status=pending` —
  row_status=pending (honest degraded mode, no Temporal saga).
* `booking: authed create outbox row committed, sent_at IS NULL` —
  outbox_rows=2 unsent=2 (no Dapr dispatcher; asserted, not faked).
* `booking: authed create REPLAY same idempotency_key -> same booking,
  exactly 1 row` — status=201 replay_id identical, rows_for_key=1.
* Same four assertions for `POST /public/sites/{slug}/bookings` — all PASS
  (201 pending, row pending, outbox 2/2 unsent, replay rows_for_key=1).
All W41 assertions (identity, billing two-phase + webhook HMAC 401/200 +
SQL row-count idempotency, payments sim hold/capture/replay/balance, 4 RLS
probes) still PASS — 42/42 total, up from R1's 34/34 by the 8 new
booking-create checks.

### 2. REAL-LEDGER (TB) mode — FAIL (build-time defect, both crates)

Command:
`TB_BINARY=/tmp/tigerbeetle FUNDS_E2E_PERF_ITERS=50 python3
tests/funds-e2e/harness.py --workdir /tmp/funds-tb`

What works: `tigerbeetle format --cluster=0 --replica=0 --replica-count=1`
+ `start --addresses=127.0.0.1:3000 --development` ->
`PASS  tigerbeetle: format + start --development, 127.0.0.1:3000 accepting`
(real 0.16.28 server came up fine).

What fails — the harness-generated tb-fixture crate does NOT COMPILE
(`logs/build-tb-fixture.log`):
```
error[E0369]: binary operation `==` cannot be applied to type
`CreateAccountErrorKind`
  --> main.rs:41:63
   41 | ... if api.as_slice().iter().all(|e| e.kind() ==
      tb::error::CreateAccountErrorKind::Exists) => {}
note: `CreateAccountErrorKind` does not implement `PartialEq`
```
Root cause verified in the pinned dependency
(`tigerbeetle-unofficial-sys 0.8.0+0.16.28`, Cargo.lock checksum
1c71963b..., bindgen 0.71.1): the generated-safe error-kind enums are
emitted with `#[derive(Debug, Clone, Copy)]` ONLY — no PartialEq
(confirmed in the build OUT_DIR `generated.rs`).

Independent probe, UNMODIFIED service tree:
`cargo build --locked --features tb-live` in services/payments-service
-> `error: could not compile `payments-service` due to 2 previous errors`:
* src/ledger/tigerbeetle.rs:241:39 — `e.kind() ==
  CreateTransferErrorKind::Exists` — E0369 (no PartialEq)
* src/ledger/tigerbeetle.rs:305:39 — `e.kind() ==
  CreateAccountErrorKind::Exists` — E0369 (no PartialEq)

So the tb-live build of the shipped W42 tree CANNOT have been run against a
real TigerBeetle as claimed ("live-proven" in tigerbeetle.rs:32 is
falsified by this compile error): the service fails to compile with the
feature enabled, and the harness's own account-provisioning fixture fails
the same way before any ledger assertion executes. The TB-mode balance
assertions (hold pending, capture 5000 -> 4875 revenue + 125 fee, replay
byte-identical) are therefore UNVERIFIED — never reached.

### 3. Mutation check (SPEC-W42 G2) — FAIL (mandated signature unreachable)

Mutation applied to a scratch copy `/tmp/mut` of services/payments-service
(`git diff` of src/ledger/tigerbeetle.rs vs the pristine tree):
```
118:  build_void_hold_transfer: CODE_DEPOSIT_HOLD -> CODE_REFUND  (102)
181:  build_capture_batch leg 0 post: CODE_DEPOSIT_HOLD -> code   (101 for captures)
```
i.e. exactly the pre-W42 code-matching defect.

* Mutated tb-live build: `cargo build --locked --features tb-live` in
  /tmp/mut -> SAME 2xE0369 at tigerbeetle.rs:241/305 (exit 1). The
  `pending_transfer_has_different_code` runtime rejection can never occur
  because the binary cannot be built.
* Full harness TB-mode run against the mutated context (prebuilt
  sim-mode binaries for the other services, fresh workdir /tmp/funds-mut):
  exit=1, failing at `tb_fixture_bin` (the same fixture E0369) — TB server
  format+start PASSed first. The run fails, but NOT with the mandated
  `pending_transfer_has_different_code`; that signature is unreachable in
  this tree.

Conclusion: the mutation check as specified CANNOT demonstrate the intended
kill condition; W42's real-ledger leg fails at compile time upstream of it.
(The in-crate unit tests at tigerbeetle.rs:589+ that pin the code-matching
rule also cannot run: they compile the same non-compiling module under
`--features tb-live`.)

### 4. W42 verdicts

| leg | verdict | evidence |
|---|---|---|
| SIM mode (baseline, incl. new booking-create checks) | **PASS** | 42/42 exit 0, twice (iters=1 and 50); logs above |
| REAL-LEDGER (TB) mode | **FAIL** | tb-fixture E0369; service tb-live 2xE0369 at tigerbeetle.rs:241,305; ledger assertions never reached |
| Mutation check (must fail with `pending_transfer_has_different_code`) | **FAIL** | mutated tree fails at compile time instead; mandated runtime signature unobservable |
| PERF (G4) | **PASS with gap** | `tests/perf/aggregate.py` exit 0, ATOMIC PASS 7/7 measured — B1/B2 booking create now MEASURED-at-HTTP (n=51 each); see tests/perf/RESULTS.md. Gap: no TB-mode timings exist (build failure), so perf covers sim mode only |

Environment fixes the verifier had to add (no repo files changed): pip
`cmake`/`libclang` wheels, `BINDGEN_EXTRA_CLANG_ARGS` for bindgen builtin
headers, ustc mirrors for crates/rustup (per-connection throttle), zig
0.13.0 prefetch + wget shim (sha256-verified).


---

## W42 R1 (repair round) — fresh-verifier execution evidence (2026-08-17, verifier W42-gate-G2+G4-R1)

Environment (sandbox, built from scratch this round): Go 1.23.4 (/tmp/go), Rust 1.97.1 stable
(manual toolchain from static.rust-lang.org dist 2026-07-16, sha256-verified components
cargo/rustc/rust-std/rustfmt), cmake 4.4.2 + libclang 18.1.1 (pip wheels, sha256-verified),
TigerBeetle 0.16.28+e97e337 (official release zip, `/tmp/tigerbeetle version` =>
"TigerBeetle version 0.16.28+e97e337"), pgserver 0.1.4 + psycopg + pytest.
Zig 0.13.0 prefetched from ziglang.org (sha256 d45312e61ebcc48032b77bc4cf7fd6915c11fa16e4aad116b66c9468211230ea,
verified) and served to the tigerbeetle-unofficial-sys build script via a PATH `wget` shim
(offline/throttled network). crates.io via ustc sparse mirror; `cargo fetch --locked` clean.

### 1. TB (real-ledger) mode run

Command:
`TB_BINARY=/tmp/tigerbeetle FUNDS_E2E_PERF_ITERS=50 python3 tests/funds-e2e/harness.py --workdir /tmp/funds-tb`
(repo=/tmp/ws, a copy of /mnt/agents/output/opendesk)

Two executed runs, identical outcome (deterministic):
- run A (cold builds): `HARNESS_EXIT=1 wall=943s` — `[harness] FAILED: 46/47 checks passed, 370 timed calls, 934.76s`
  (build times: tb-fixture 2m06s, booking 69s, billing-engine 292s, payments --features tb-live incl. zig tb_client C build)
- run B (cached builds, fresh pgdata + fresh TB datafile): `HARNESS_EXIT=1 wall=33s` —
  `[harness] FAILED: 46/47 checks passed, 370 timed calls, 23.16s`

PASS evidence (both runs), verbatim lines:
```
PASS  tigerbeetle: format + start --development, 127.0.0.1:3000 accepting
PASS  tigerbeetle: tenant+platform accounts created on the REAL ledger (pinned client crate, exists=idempotent)
PASS  payments: POST /v1/deposits (hold) -> 201
PASS  payments: hold REPLAY same key -> same deposit_id (no double-hold)
PASS  payments: POST /v1/deposits/{id}/capture -> 200
PASS  tigerbeetle: hold visible as PENDING on the real ledger (deposits credits_pending += hold amount)  — delta=5000
PASS  tigerbeetle: capture MOVED FUNDS on the real ledger (balance deltas == post/revenue/fee amounts)  — post=5000 revenue=4875 fee=125
PASS  tigerbeetle: capture REPLAY left every balance counter byte-identical (real TB `exists`, no double-post)
PASS  booking: POST /v1/bookings (authed, IDENTITY_BASE_URL resolution) -> 201 pending
PASS  booking: POST /public/sites/{slug}/bookings -> 201 pending (IDENTITY_BASE_URL resolution)
PASS  booking: authed/public create REPLAY same idempotency_key -> same booking, exactly 1 row
```
(booking-create checks PASS via the IDENTITY_BASE_URL fallback; RLS adversarial 4/4 PASS; billing suite PASS.)

The R0 tb-live compile defects are confirmed FIXED: `cargo build --locked --features tb-live`
succeeds for payments-service (matches!/derive(Debug) repairs compile clean) and the generated
tb-fixture crate builds and provisions accounts against the real server.

### 2. REMAINING DEFECT (live-proven, deterministic): capture replay returns 502 in TB mode

```
FAIL  payments: capture REPLAY -> identical result, no double-post  — status=502 identical=False
```
All 50 replay iterations returned HTTP 502 (timings JSON: capture_replay statuses all 502, ~1.8 ms each).
Manual reproduction against the committed ledger (verbatim):
```
$ curl -X POST .../v1/deposits/83053016-88e6-564c-9e40-2725430a110e/capture -d '{"tenant_id":"...","amount_cents":5000}'
{"error":"ledger backend error: tigerbeetle transfer rejected: Failed to create transfers: 3 api errors occurred at transfers' creation"}
HTTP=502
```
Root cause, proven with a purpose-built probe crate linking the SAME pinned
tigerbeetle-unofficial 0.8.0+0.16.28 client against a fresh real TB 0.16.28 (verbatim probe output):
```
first fixed-code capture committed
REPLAY index=0 kind=Exists
REPLAY index=1 kind=LinkedEventFailed
REPLAY index=2 kind=LinkedEventFailed
```
TigerBeetle treats `exists` as a chain-breaking result inside a LINKED batch: on replay of the
committed capture batch, leg 0 (post leg, code=CODE_DEPOSIT_HOLD — the W42 fix works, the first
capture commits) resolves to `Exists`, and the two linked split legs fail with `LinkedEventFailed`.
The service's submit() (GF11) only accepts the all-`Exists` case, so replay surfaces as 502.
The ledger itself stayed correct — balances after 50 replays are byte-identical (the separate
byte-identical assertion PASSED; no double-post). The defect is the replay API contract
(200 + identical result), not fund safety. SIM mode (42/42, R0) is unaffected because the sim
ledger reuses stored transfers.

### 3. Runtime mutation check (the R0 gap — executed)

Mutant: copy of services/payments-service at /tmp/mut with the W42 fix REVERTED in
src/ledger/tigerbeetle.rs — capture post leg code CODE_DEPOSIT_HOLD -> `code` (CODE_CAPTURE=101)
in build_capture_batch leg 0, and void leg CODE_DEPOSIT_HOLD -> CODE_REFUND=102 in
build_void_hold_transfer. Built: `cargo build --locked --features tb-live` => MUT_BUILD_EXIT=0.

Run (verbatim tail):
```
TB_BINARY=/tmp/tigerbeetle PAYMENTS_BIN=/tmp/mut-target/debug/payments-service FUNDS_E2E_PERF_ITERS=1 python3 tests/funds-e2e/harness.py --workdir /tmp/funds-tb
PASS  payments: POST /v1/deposits (hold) -> 201  — deposit_id=480f61dacd1b593b9ede5ac12bc6c014
PASS  payments: hold REPLAY same key -> same deposit_id (no double-hold)
FAIL  payments: POST /v1/deposits/{id}/capture -> 200  — status=502 body={'error': "ledger backend error: tigerbeetle transfer rejected: Failed to create transfers: 3 api errors occurred at transfers' creation"}
[harness] FAILED: 42/44 checks passed, 26 timed calls, 18.46s
MUT_HARNESS_EXIT=1
```
The mutant FAILS at runtime on the FIRST capture, as required. Verbatim server-side error kinds
for the mutant-style batch (same probe crate, fresh TB):
```
MUTANT index=0 kind=PendingTransferHasDifferentCode
MUTANT index=1 kind=LinkedEventFailed
MUTANT index=2 kind=LinkedEventFailed
```
i.e. exactly `pending_transfer_has_different_code` on the mis-coded post leg — the W42 unit-test
comment's prediction confirmed against the real server.

### R1 verdict (this gate)

- TB real-ledger mode: **FAIL** — 46/47; the money path, hold-pending visibility, capture fee
  split (5000 -> 4875 revenue + 125 fee) and balance idempotency all proven on the real ledger,
  but the capture-replay API contract returns 502 (Exists/LinkedEventFailed linked-batch semantic).
  Remaining work: submit() must treat `Exists` + trailing `LinkedEventFailed`-after-`Exists` on a
  verbatim replayed batch as idempotent success (or TB-side retry semantics revisited).
- Runtime mutation check: **PASS** (mutant detected at runtime; verbatim PendingTransferHasDifferentCode).
- R0 compile repairs (matches!/derive(Debug), created_at stripping in TB-mode comparison): confirmed compiled
  and exercised; the created_at normalization was not reached because replay 502s before comparison.


---

## W42 R2 — TB-mode reverification after is_idempotent_replay repair (verifier gate G2, fresh env)

R1 live-proven defect (replay of a committed LINKED capture batch returned Exists+LinkedEventFailed
-> 502) is FIXED by `is_idempotent_replay()` in services/payments-service/src/ledger/tigerbeetle.rs:246
(accepts iff every kind in {Exists, LinkedEventFailed} AND >=1 Exists anchor; empty set keeps prior
vacuous acceptance).

### 1. cargo test --locked --features tb-live (in /tmp/ws copy, CARGO_TARGET_DIR=/tmp/ttarget)

    test ledger::tigerbeetle::tests::replay_all_exists_is_accepted ... ok
    test ledger::tigerbeetle::tests::replay_linked_exists_then_linked_event_failed_is_accepted ... ok
    test ledger::tigerbeetle::tests::linked_event_failed_without_exists_anchor_is_rejected ... ok
    test ledger::tigerbeetle::tests::mutant_different_code_plus_linked_event_failed_is_rejected ... ok
    test ledger::tigerbeetle::tests::any_genuine_error_kind_defeats_replay_acceptance ... ok
    test ledger::tigerbeetle::tests::empty_error_set_keeps_prior_vacuous_acceptance ... ok
    test result: ok. 48 passed; 0 failed; 0 ignored (final suite; 45- and 30-test suites also ok)
    TEST_EXIT=0

All 6 new is_idempotent_replay tests PASS; all prior tests PASS.

### 2. TB mode (REAL TigerBeetle 0.16.28, single-replica --development)

    TB_BINARY=/tmp/tigerbeetle FUNDS_E2E_PERF_ITERS=50 python3 tests/funds-e2e/harness.py --workdir /tmp/funds-tb-r2
    (prebuilt bins via PAYMENTS_BIN/BILLING_BIN/IDENTITY_BIN/BOOKING_BIN; payments built --features tb-live)

    [harness] OK: 47/47 checks passed, 370 timed calls, 145.92s   (wall 156s)  HARNESS_EXIT=0

Key checks (verbatim):
    PASS  payments: POST /v1/deposits/{id}/capture -> 200  — status=200
    PASS  payments: capture REPLAY -> identical result, no double-post  — status=200 identical=True
    PASS  tigerbeetle: capture MOVED FUNDS on the real ledger — post=5000 revenue=4875 fee=125
    PASS  tigerbeetle: capture REPLAY left every balance counter byte-identical (real TB `exists`, no double-post)

The R1 502-on-replay defect no longer reproduces: replay returns 200 with an identical (modulo
created_at) result and every ledger balance counter byte-identical.

### 3. Runtime mutation check (MUST still fail) — /tmp/mut copy, post leg code 100->101, void leg 100->102

    PAYMENTS_BIN=<mutant tb-live build> python3 tests/funds-e2e/harness.py --workdir /tmp/funds-tb-mut

    FAIL  payments: POST /v1/deposits/{id}/capture -> 200  — status=502 body={'error': "ledger backend error:
          tigerbeetle transfer rejected: Failed to create transfers: 3 api errors occurred at transfers' creation"}
    FAIL  payments: capture REPLAY -> identical result, no double-post  — status=502 identical=True
    [harness] FAILED: 42/44 checks passed, 369 timed calls, 144.44s   HARNESS_EXIT=1

Mutant detected at the FIRST capture (502; the 3 api errors = pending_transfer_has_different_code on the
post leg + linked_event_failed x2 on the linked split legs — exactly the kind vector the R2 unit test
`mutant_different_code_plus_linked_event_failed_is_rejected` rejects). Mutation check: **PASS** (still fails).
