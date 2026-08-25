# tests/funds-e2e — RESULTS

Status: **FINAL — FIN-H3 (W44 verification re-run against the CODER-F5 refund-replay fix): BOTH suites re-run from REAL captured outputs and BOTH FULLY GREEN. Both BLOCKED items RESOLVED.**

* **SIM mode (`LEDGER_IMPL=sim`): 86/86 checks PASS, 0 skips, exit 0** (FIN-H3 run, 2026-08-24, 18.21s — re-run fresh, supersedes FIN-H's sim run).
* **REAL-LEDGER mode (`LEDGER_IMPL=tigerbeetle`, TigerBeetle 0.16.28, `--features tb-live`): 91/91 checks PASS, 0 skips, exit 0** (FIN-H3 run, 2026-08-24, 164.63s) — CODER-F5's TB refund-replay parity fix **CONFIRMED** on the real ledger: refund REPLAY now returns 201 with the same refund id and byte-identical balances (BLOCKED (2) RESOLVED). CODER-F2's `classify_void_kinds()` refund-after-capture fix remains green (BLOCKED (1) RESOLVED at FIN-H2).

History: harness v3 (W43, 89 857 B) had never been executed end-to-end. Three
verification phases ran it against the CURRENT mirror (W44 W-P
payments/billing contracts K1/K2/K6/K7, B-01, F15-03; W-B booking hardening;
W-I identity hardening), each time with harness.py unchanged at md5
`4dbc1ce47c798e86cd9ce83dce9d8376` (109 162 B) after the one-time W44-contract
harness update (assertions never weakened — see "Harness changes" below):

* **FIN-H** — first full run: SIM 86/86; TB 89/91 (BLOCKED (1): TB-live
  refund-after-capture 502, root-caused to `classify_void_kinds`).
* **FIN-H2** — re-run against CODER-F2's fix: TB 90/91 (BLOCKED (1) RESOLVED;
  unmasked BLOCKED (2): refund REPLAY 502, root-caused to the missing
  replay-by-transfer-id short-circuit on the TB path).
* **FIN-H3** (this file's headliner) — re-run against CODER-F5's fix:
  **TB 91/91** (BLOCKED (2) RESOLVED); SIM re-run 86/86. Suite fully green
  in both ledger modes.

## FIN-H3 final verification run (2026-08-24 UTC, post-CODER-F5 fix) — CURRENT

Fix under test: `services/payments-service/src/ledger/tigerbeetle.rs`
md5 `bd12aa83525bca5a6ac6a0618bcdf9e3` (64 321 B; fresh rsync from the CURRENT
mirror — byte-identical to CODER-F5's checkpoint in `backups/w44/F5/`): the TB
refund path now mirrors the sim's replay-by-transfer-id short-circuit —
before attempting the void leg it looks up the deterministic refund transfer
id and, when a STORED transfer under that id matches the refund this request
would commit (`refund_replay_matches()`: posted refund = plain posted
`CODE_REFUND` transfer from `tenant:{id}:revenue` to `platform:clearing` with
matching amount/ledger; void leg = `VOID_PENDING_TRANSFER` with
`pending_id == hold_id`), returns it verbatim as an idempotent replay
(201-class). Non-matching occupants of the id remain a P-12 409-class
conflict, never a silent success and never a 502. CODER-F5 live-verified the
fix 11/11 against real TB 0.16.28 (`backups/w44/F5/verify-f5-payments.log`).
Harness unchanged: md5 `4dbc1ce47c798e86cd9ce83dce9d8376`, 109 162 B
(mirror == run tree, re-verified this run).

### Environment fingerprint (FIN-H3 run, sandbox local)

| | |
|---|---|
| Host | sandbox, 2 CPU / 4 GB RAM, Debian 12 (bookworm), x86_64 (same class as FIN-H/FIN-H2; /tmp had been wiped — full env rebuilt from scratch) |
| Python | 3.12.12 — pgserver 0.1.4 (embedded full PostgreSQL 16, unix socket), psycopg 3.3.4, pytest 9.1.1 |
| Go | go1.23.4 linux/amd64 (/tmp/go, from the /mnt tarball; `GOPROXY=https://goproxy.cn,direct`, `GOFLAGS=-mod=readonly`) |
| Rust | rustc/cargo 1.98.0 (88d9e12ae 2026-08-18; rustup minimal profile, RUSTUP via rsproxy.cn), crates via rsproxy.cn sparse mirror |
| tb-live toolchain | cmake 4.4.2 (pip), libclang (pip `libclang` wheel, `LIBCLANG_PATH=.../clang/native`), zig 0.13.0 (`/tmp/zig`, re-downloaded ziglang.org tarball), `BINDGEN_EXTRA_CLANG_ARGS=-I/tmp/zig/lib/include` |
| TigerBeetle | official release binary `0.16.28+e97e337`, 18 204 512 B, md5 31288566a718ce9290571d3f3982b6da — restored byte-identical from the FIN-H2 checkpoint (`backups/w44/FIN-H2/tigerbeetle-0.16.28-bin`); `tigerbeetle version` confirmed. NOTE: `/mnt/agents/tb.zip` is corrupt and a fresh GitHub release download stalled in this sandbox, so the checkpointed copy of the official binary (md5 identical to FIN-H's official-download fingerprint) was used |
| Repo | `/tmp/work/opendesk-repo` — fresh rsync copy of the /mnt mirror; `OPENDESK_REPO` pointed at it |
| XDG_RUNTIME_DIR | /tmp/xdg (0700) |

The FIN-H2 sandbox deviation is carried over unchanged (sandbox-level, not a
harness or service change): `IDENTITY_BASE_URL=http://127.0.0.1:17001` was
EXPORTED to the harness process, which inherits it into billing-engine's
environment (`start_service` merges `os.environ`). Billing's compiled-in
default is the compose hostname `http://identity:7001`, unresolvable in this
sandbox (/etc/hosts is read-only). Demonstrated again this run: a first FIN-H3
SIM run WITHOUT the export scored 85/86 with the billing foreign-tenant case
failing as a 503 `tenant binding unavailable: identity resolution failed`
(env artifact; log checkpointed as `run-sim-run1-noexport.log`); with the
export the same case passes with the real contract answer (403 `tenant is not
bound to the caller`) in BOTH modes. All other 90 checks are unaffected by
the knob. The TB run additionally needs the Rust toolchain on PATH (the
harness builds its tb-fixture crate at runtime; a first attempt without
`CARGO_HOME`/PATH aborted before any check with `no cargo found to build the
tb-fixture crate`).

Binaries (all real builds from the committed sources, debug profile;
exported via `IDENTITY_BIN`/`BOOKING_BIN`/`BILLING_BIN`/`PAYMENTS_BIN`):

| binary | bytes | md5 | build |
|---|---|---|---|
| identity-service | 20 967 606 | 7a211a6dc14759568109d7b763c68a24 | `go build ./cmd/server` |
| booking-service | 46 936 447 | 8933598439de72afba0f57e8ec02cbb9 | `go build ./cmd/server` |
| billing-engine | 151 260 224 | 523dff1d5636d5881fcf2ed15f2ee5ee | `cargo build --locked` |
| payments-service (sim) | 150 530 456 | badcce7bc85995a66ebb36caae9b10ec | `cargo build --locked` |
| payments-service (tb-live) | 156 431 968 | a57d90bcdd407e7019c938ebdf42027a | `cargo build --locked --features tb-live` (~4 min incremental; tb client `tigerbeetle-unofficial 0.8.0+0.16.28` vendored via zig) |

Commands:

```bash
# SIM full suite:
XDG_RUNTIME_DIR=/tmp/xdg OPENDESK_REPO=/tmp/work/opendesk-repo \
IDENTITY_BASE_URL=http://127.0.0.1:17001 \
IDENTITY_BIN=... BOOKING_BIN=... BILLING_BIN=... PAYMENTS_BIN=<sim build> \
python3 tests/funds-e2e/harness.py --workdir /tmp/funds-e2e-sim2
# exit 0 — 86/86, 92 timed calls, 18.21s (started 2026-08-24T23:06:31Z)

# REAL-LEDGER (TigerBeetle) full suite:
XDG_RUNTIME_DIR=/tmp/xdg OPENDESK_REPO=/tmp/work/opendesk-repo TB_BINARY=/tmp/tb-bin/tigerbeetle \
IDENTITY_BASE_URL=http://127.0.0.1:17001 \
PATH=/tmp/zig:/tmp/cargo/bin:... RUSTUP_HOME=/tmp/rustup CARGO_HOME=/tmp/cargo \
LIBCLANG_PATH=.../clang/native BINDGEN_EXTRA_CLANG_ARGS=-I/tmp/zig/lib/include \
IDENTITY_BIN=... BOOKING_BIN=... BILLING_BIN=... PAYMENTS_BIN=<tb-live build> \
python3 tests/funds-e2e/harness.py --workdir /tmp/funds-e2e-tb2
# exit 0 — 91/91, 96 timed calls, 164.63s incl. fixture-crate build
# (started 2026-08-24T23:07:48Z)
```

Auth posture: identical to FIN-H/FIN-H2 (no `OPENDESK_TRUST_DIRECT_TENANT`;
K2 internal tokens for service-style calls; explicit gateway headers
`X-Tenant-Slugs`/`X-User-Id`/`X-User-Roles` for human-style calls).

### Verbatim delta — FIN-H3 REAL-LEDGER run (91/91, exit 0, 164.63s, 96 timed calls)

Identical suite to FIN-H/FIN-H2; the decisive lines (full log checkpointed):

```
PASS tigerbeetle: format + start --development, 127.0.0.1:3000 accepting
PASS tigerbeetle: tenant+platform accounts created on the REAL ledger (pinned client crate, exists=idempotent)
PASS tigerbeetle: hold visible as PENDING on the real ledger (deposits credits_pending += hold amount)  — delta=5000
PASS tigerbeetle: capture MOVED FUNDS on the real ledger (balance deltas == post/revenue/fee amounts)   — post=5000 revenue=4875 fee=125
PASS tigerbeetle: capture REPLAY left every balance counter byte-identical (real TB `exists`, no double-post)
PASS billing: foreign-tenant gateway call -> 403 (K1 X-Tenant-Slugs binding)  — status=403 (with the IDENTITY_BASE_URL export, above)
PASS payments: refund (full, post-capture) -> 201; revenue debits_posted += 2000  — status=201 refund_id=313188980701037262337851716709380156958 revenue_debits_delta=2000
PASS payments: refund REPLAY same key -> same refund id, balances byte-identical (no double refund)  — status=201 same_id=True unchanged=True   [BLOCKED (2) FIXED — was status=502 same_id=False at FIN-H2]
PASS payments: refund with wrong amount -> 400, balances unchanged (P-11)  — status=400 unchanged=True
```

## FIN-H2 verification run (2026-08-24 UTC, post-CODER-F2 fix) — SUPERSEDED by the FIN-H3 run above

Fix under test: `services/payments-service/src/ledger/tigerbeetle.rs`
md5 `3fb2fb4ddf9f8d17304253065e1353df` (fresh rsync from the CURRENT mirror —
byte-identical to CODER-F2's checkpoint): `classify_void_kinds()` now maps
`CreateTransferErrorKind::PendingTransferAlreadyPosted` (real TB 0.16.28
result 33, voiding an ALREADY-POSTED pending transfer) to
`VoidClassification::NotPending`, so refund-after-capture falls through to the
posted-refund path exactly like the sim. Harness unchanged: md5
`4dbc1ce47c798e86cd9ce83dce9d8376`, 109 162 B (mirror == run tree).

### Environment fingerprint (FIN-H2 run, sandbox local)

| | |
|---|---|
| Host | sandbox, 2 CPU / 4 GB RAM, Debian 12 (bookworm), x86_64 (same class as FIN-H) |
| Python | 3.12.12 — pgserver 0.1.4 (embedded full PostgreSQL 16, unix socket), psycopg 3.3.4, pytest 9.1.1 |
| Go | go1.23.4 linux/amd64 (/tmp/go, restored from /mnt go-toolchain; `GOPROXY=https://goproxy.cn,direct`, `GOFLAGS=-mod=readonly`) |
| Rust | rustc/cargo 1.98.0 (88d9e12ae 2026-08-18; rustup minimal profile), crates via rsproxy.cn sparse mirror |
| tb-live toolchain | cmake 4.4.2 (pip), libclang 18.1.1 (pip `libclang` wheel, `LIBCLANG_PATH=.../clang/native`), zig 0.13.0 (`/tmp/zig`), `BINDGEN_EXTRA_CLANG_ARGS=-I/tmp/zig/lib/include` |
| TigerBeetle | official release binary `0.16.28+e97e337` (re-downloaded tigerbeetle-x86_64-linux.zip), 18 204 512 B, md5 31288566a718ce9290571d3f3982b6da — identical to FIN-H's fingerprint |
| Repo | `/tmp/work/opendesk-repo` — fresh rsync copy of the /mnt mirror; `OPENDESK_REPO` pointed at it |
| XDG_RUNTIME_DIR | /tmp/xdg (0700) |

One environment deviation from FIN-H's run (sandbox-level, not a harness or
service change): `IDENTITY_BASE_URL=http://127.0.0.1:17001` was EXPORTED to
the harness process, which inherits it into billing-engine's environment
(`start_service` merges `os.environ`). Billing's compiled-in default is the
compose hostname `http://identity:7001`, unresolvable in this sandbox
(/etc/hosts is read-only here, so no `identity` alias could be added); a
first FIN-H2 TB run without the export scored 89/91 with the billing
foreign-tenant case failing as a 503 `identity resolution failed` — an
environment artifact, not a service regression. With the export the same
case passes with the real contract answer (403 `tenant is not bound to the
caller (X-Tenant-Slugs membership / identity resolution)`). All other 90
checks are unaffected by the knob.

Binaries (all real builds from the committed sources, debug profile;
exported via `IDENTITY_BIN`/`BOOKING_BIN`/`BILLING_BIN`/`PAYMENTS_BIN`):

| binary | bytes | md5 | build |
|---|---|---|---|
| identity-service | 20 967 606 | 87074317ae5ce23bcce423291068fdb0 | `go build ./cmd/server` |
| booking-service | 46 936 447 | fd6dc71d666295a271c7a83c1fbe427b | `go build ./cmd/server` |
| billing-engine | 151 244 888 | 95c628862ec2ce63814ad4736d76866e | `cargo build --locked` (3m21s) |
| payments-service (tb-live) | 156 410 224 | c211179dc961b5c043cfd36f9d2907d5 | `cargo build --locked --features tb-live` (9m30s; deterministic — a later relink reproduced the identical md5) |

Commands:

```bash
# unit test (CODER-F2's new classification coverage), from the fresh rsync:
cd services/payments-service && cargo test --locked --features tb-live void_classification
# -> test result: ok. 6 passed; 0 failed (44/104 filtered out), incl.
#    void_classification_already_posted_falls_through  (result-33 regression test)
#    void_classification_not_pending_falls_through ... void_classification_anything_else_is_backend

# REAL-LEDGER (TigerBeetle) full suite:
XDG_RUNTIME_DIR=/tmp/xdg OPENDESK_REPO=/tmp/work/opendesk-repo TB_BINARY=/tmp/tb-bin/tigerbeetle \
IDENTITY_BASE_URL=http://127.0.0.1:17001 \
LIBCLANG_PATH=.../clang/native BINDGEN_EXTRA_CLANG_ARGS=-I/tmp/zig/lib/include \
IDENTITY_BIN=... BOOKING_BIN=... BILLING_BIN=... PAYMENTS_BIN=<tb-live build> \
python3 tests/funds-e2e/harness.py --workdir /tmp/funds-e2e-tb4
# exit 1 — 90/91, 96 timed calls, 168.1s incl. fixture-crate build
# (started 2026-08-24T22:03:04Z)
```

Auth posture: identical to FIN-H (no `OPENDESK_TRUST_DIRECT_TENANT`; K2
internal tokens for service-style calls; explicit gateway headers
`X-Tenant-Slugs`/`X-User-Id`/`X-User-Roles` for human-style calls).

### Verbatim delta — FIN-H2 REAL-LEDGER run (90/91, exit 1, 168.1s, 96 timed calls)

Identical suite to FIN-H; only the TB-specific and changed lines:

```
PASS tigerbeetle: format + start --development, 127.0.0.1:3000 accepting
PASS tigerbeetle: tenant+platform accounts created on the REAL ledger (pinned client crate, exists=idempotent)
PASS tigerbeetle: hold visible as PENDING on the real ledger (deposits credits_pending += hold amount)  — delta=5000
PASS tigerbeetle: capture MOVED FUNDS on the real ledger (balance deltas == post/revenue/fee amounts)   — post=5000 revenue=4875 fee=125
PASS tigerbeetle: capture REPLAY left every balance counter byte-identical (real TB `exists`, no double-post)
PASS billing: foreign-tenant gateway call -> 403 (K1 X-Tenant-Slugs binding)  — status=403 (with the IDENTITY_BASE_URL export, above)
PASS payments: refund (full, post-capture) -> 201; revenue debits_posted += 2000  — status=201 refund_id=228559830838374876030499153043944871468 revenue_debits_delta=2000   [BLOCKED (1) FIXED]
FAIL payments: refund REPLAY same key -> same refund id, balances byte-identical (no double refund)  — status=502 same_id=False unchanged=True   [BLOCKED (2), below]
```

## RESOLVED (FIN-H3-verified): former BLOCKED (2): TB-live refund REPLAY → 502 (real service bug, sim/TB parity gap)

**Resolution:** CODER-F5 applied exactly the fix FIN-H2 pinned below
(`services/payments-service/src/ledger/tigerbeetle.rs` md5
`bd12aa83525bca5a6ac6a0618bcdf9e3`): the TB refund path now does the sim's
replay-by-transfer-id short-circuit FIRST — a stored transfer under the
deterministic refund id that matches the request (`refund_replay_matches()`:
posted-refund arm = plain posted `CODE_REFUND`, `tenant:{id}:revenue` →
`platform:clearing`, matching amount/ledger; void arm = `VOID_PENDING_TRANSFER`
with `pending_id == hold_id`) is returned verbatim (201 idempotent replay);
non-matching occupants stay a P-12 409-class conflict.
FIN-H3 re-ran the full TB suite against it: **refund REPLAY now PASSES on the
real ledger — status=201, same_id=True, balances byte-identical
(unchanged=True)** — and CODER-F5 independently live-verified the parity fix
11/11 against real TB 0.16.28 (`backups/w44/F5/verify-f5-payments.log`).
The original diagnosis is kept verbatim below for the record.

**Symptom (1 check, at FIN-H2):** replaying the SAME refund request (same idempotency
key) after a now-SUCCESSFUL post-capture refund returns `502`
(`LedgerError::Backend`) against the real TigerBeetle; the identical replay
in sim mode returns the same refund id. Balances were verified byte-identical
across the replay (harness: `unchanged=True`) — nothing moved twice; the
replay is wrongly REFUSED.

**Isolated reproduction (harness-independent, real TB 0.16.28 + the canonical
tb-live binary above; script + service log checkpointed to
backups/w44/FIN-H2/):**

```
hold 1000 -> 201; capture 1000 -> 200;
POST /v1/refunds {deposit_id, amount_cents:1000, key K} ->
  201 posted refund (revenue debits_posted 0 -> 1000)          [the F2 fix]
POST /v1/refunds {identical body, key K} ->
  502 {"error":"ledger backend error: tigerbeetle void transfer rejected:
       [ExistsWithDifferentFlags]"}
post-replay balances: revenue debits_posted STILL 1000 — exactly ONE refund
```

**Root cause (pinned to source):** `TigerBeetleLedger::refund`
(`services/payments-service/src/ledger/tigerbeetle.rs`) re-attempts the VOID
leg with the SAME deterministic transfer id (`transfer_id_from_key(K)`) that
the first call already committed as the POSTED refund transfer. Real TB
0.16.28 rejects the replayed void with `exists_with_different_flags` (stored:
flag none / code CODE_REFUND; attempt: void-pending leg with the hold's
code), and `classify_void_kinds` correctly refuses to swallow
`exists_with_different_*` (P-12: a 409-class parameter conflict) → `Backend`
→ 502. The sim avoids this entirely: `ledger/sim.rs refund()` does a
replay-by-transfer-id short-circuit FIRST and returns the stored refund when
it is refund-shaped; the TB path has no equivalent short-circuit. Fix
(service-side, owned by W-P — NOT applied by FIN-H2 per scope): mirror the
sim — look up `transfer_id` before the void attempt and return the stored
transfer when it is the refund-shaped one (code CODE_REFUND, matching
amount/accounts); keep `exists_with_different_*` as a conflict otherwise.

**Impact:** pre-fix this leg also failed, but was masked by BLOCKED (1) (both
refund checks 502'd at the void classification with
`PendingTransferAlreadyPosted`). CODER-F2's fix unmasked this second,
distinct parity gap. Capture replay (byte-identical), no-show replay, void of
a pending hold, and the P-11 wrong-amount 400 all PASS on the real ledger.

## Environment fingerprint (FIN-H run, sandbox local, 2026-08-24 UTC)

| | |
|---|---|
| Host | sandbox, 2 CPU / 4 GB RAM, Debian 12 (bookworm), x86_64 |
| Python | 3.12.12 — pgserver 0.1.4 (embedded full PostgreSQL 16, unix socket), psycopg 3.3.4, pytest 9.1.1 |
| Go | go1.23.4 linux/amd64 (/tmp/go, `GOPROXY=https://goproxy.cn,direct`, `GOFLAGS=-mod=readonly`) |
| Rust | rustc/cargo 1.98.0 (rustup minimal profile), crates via rsproxy.cn sparse mirror |
| tb-live toolchain | cmake 4.4.2 (pip), libclang (pip `libclang` wheel, `LIBCLANG_PATH=.../clang/native`), zig 0.13.0 (`/tmp/zig`), `BINDGEN_EXTRA_CLANG_ARGS=-I/tmp/zig/lib/include` (stddef.h) |
| TigerBeetle | official release binary `0.16.28+e97e337` (tigerbeetle-x86_64-linux.zip), 18 204 512 B, md5 31288566a718ce9290571d3f3982b6da |
| Repo | `/tmp/opendesk-repo` — rsync copy of the /mnt mirror; `OPENDESK_REPO` pointed at it; harness.py md5-identical between mirror and run tree (4dbc1ce47c798e86cd9ce83dce9d8376, 109 162 B) |
| XDG_RUNTIME_DIR | /tmp/xdg (0700) |

Binaries (all real builds from the committed sources, debug profile;
exported via `IDENTITY_BIN`/`BOOKING_BIN`/`BILLING_BIN`/`PAYMENTS_BIN`):

| binary | bytes | md5 | build |
|---|---|---|---|
| identity-service | 20 954 184 | f8b9cfb8866be9b2fb1dac0ae523eb05 | `go build ./cmd/server` |
| booking-service | 46 932 103 | 86affd1b5244fc50919de74227a0c5c8 | `go build ./cmd/server` |
| billing-engine | 150 920 648 | 920c424d3f1f744e70579cfac248448c | `cargo build --locked` |
| payments-service (sim) | 150 534 224 | e06591b15a0b9c669e06ccb8b2f661fc | `cargo build --locked` |
| payments-service (tb-live) | 156 433 688 | 7ca0db28263c20d7665d6f4f094b6bb3 | `cargo build --locked --features tb-live` (7m23s, vendored tb client via zig) |

Commands:

```bash
# SIM
XDG_RUNTIME_DIR=/tmp/xdg OPENDESK_REPO=/tmp/opendesk-repo \
IDENTITY_BIN=... BOOKING_BIN=... BILLING_BIN=... PAYMENTS_BIN=<sim build> \
python3 tests/funds-e2e/harness.py --workdir /tmp/funds-e2e-sim
# exit 0 — 86/86, 92 timed calls, 18.2s (started 2026-08-24T19:55:51Z)

# REAL-LEDGER (TigerBeetle)
XDG_RUNTIME_DIR=/tmp/xdg OPENDESK_REPO=/tmp/opendesk-repo TB_BINARY=/tmp/tb-bin/tigerbeetle \
LIBCLANG_PATH=.../clang/native BINDGEN_EXTRA_CLANG_ARGS=-I/tmp/zig/lib/include \
IDENTITY_BIN=... BOOKING_BIN=... BILLING_BIN=... PAYMENTS_BIN=<tb-live build> \
python3 tests/funds-e2e/harness.py --workdir /tmp/funds-e2e-tb
# exit 1 — 89/91, 96 timed calls, 164.2s incl. fixture-crate build
# (started 2026-08-24T20:13:08Z)
```

Auth posture (stronger than the dev escape): the harness runs WITHOUT
`OPENDESK_TRUST_DIRECT_TENANT=1`. Service-style calls authenticate with the
per-run internal tokens (K2: `PAYMENTS_INTERNAL_TOKEN` /
`BILLING_INTERNAL_TOKEN` / `IDENTITY_INTERNAL_TOKEN`); human-style calls use
explicit gateway headers (`X-Tenant-Slugs` / `X-User-Id` / `X-User-Roles`),
which the W44 services honor exactly as gateway-injected claims.

## Execution matrix

| suite section | sim | tigerbeetle (real ledger) |
|---|---|---|
| service boot / healthz | 4/4 | 5/5 (incl. TB format+start) |
| identity tenant create (W44 subject auth) | 2/2 | 3/3 (incl. TB account fixture) |
| booking public reads + W42 write path (authed/public create, pending + unsent-outbox degraded honesty, idempotent replay) | 9/9 | 9/9 |
| billing rate-card/generate/issue/payment-link | 4/4 | 4/4 |
| billing W44 gateway-auth matrix (owner 200 / member 403 / foreign tenant 403) | 3/3 | 3/3 |
| billing paystack webhook (bad-sig 401, real HMAC paid, same-tx outbox+ledger, replay idempotency) | 8/8 | 8/8 |
| billing B-01 webhook amount/currency mismatch — **flipped LIVE** (202, invoice NOT paid, payment_mismatch outbox row) | 2/2 | 2/2 |
| payments deposit→capture→replay→balance | 5/5 | 5/5 |
| real-ledger assertions (hold PENDING on TB, capture balance deltas == response amounts, replay byte-identical) | n/a | 3/3 |
| payments provision auth + F15-03 healthz/metrics | 4/4 | 4/4 |
| over-capture / void / capture-after-void (balances unchanged) | 4/4 | 4/4 |
| cross-tenant capture/void/refund 403 (P-06) | 3/3 | 3/3 |
| payments W44 gateway matrix (gateway hold 201 + provenance row; member 403; foreign tenant 403) | 5/5 | 5/5 |
| refund happy path (post-capture) | 1/1 | 1/1 — **FIXED by CODER-F2, FIN-H2-verified; re-verified FIN-H3** |
| refund replay (same-key idempotency) | 1/1 | 1/1 — **FIXED by CODER-F5, FIN-H3-verified (was 0/1 at FIN-H2)** |
| refund wrong amount 400 (P-11, zero mutations) | 2/2 | 2/2 |
| no-show fee (partial capture, remainder released, replay) | 3/3 | 3/3 |
| beneficiary registry (create/list/disable) | 4/4 | 4/4 |
| payout negatives: raw payee 422, foreign beneficiary 422, disabled beneficiary 422 — all with ZERO ledger/rail side effects | 3/3 | 3/3 |
| payout happy path (K7 beneficiary_id, C3 ledger-first, rail exactly once, committed payout_attempts row) + over-limit rejected before rail | 3/3 | 3/3 |
| capture without amount (C4 lookup) / PLATFORM_FEE_BPS boot refusal (P-05) | 3/3 | 3/3 |
| RLS adversarial (app_billing_login: wrong/empty/unset GUC → 0; own tenant only) | 4/4 | 4/4 |
| **TOTAL** | **86/86 PASS (exit 0)** — FIN-H3 re-run | **91/91 PASS (exit 0)** — FIN-H3 final run |

DLQ note (honest scope): the Kafka commands consumer is disabled in this
harness (no broker), so DLQ redelivery cannot be exercised; what IS asserted
live is the F15-03 reporting surface — `/healthz` dependency detail (ledger
probe, postgres probe, `dlq_producer` state, `commands_dead_lettered` gauge)
and the `/metrics` counters (`payments_commands_processed_total`,
`payments_commands_dead_lettered`, `payments_payout_attempts_total{outcome=...}`).

## RESOLVED (FIN-H2-verified): former BLOCKED (1): TB-live refund-after-capture → 502 (real service bug)

**Resolution:** CODER-F2 applied exactly the fix FIN-H pinned below (`PendingTransferAlreadyPosted` → `VoidClassification::NotPending` in `classify_void_kinds`), and FIN-H2 re-ran the full TB suite against it: refund-after-capture now returns 201 with the posted refund and the revenue debit delta == response amount (real-ledger balance assertion green), 6/6 new `void_classification` unit tests pass, and the isolated repro confirms the posted-refund path end-to-end. The original diagnosis is kept verbatim below for the record. FIN-H's side-note in it (`PendingTransferAlreadyVoided` → AlreadyResolved for void-after-void parity) remains unimplemented but is NOT exercised by this suite; the then-surviving in-suite gap (refund replay, former BLOCKED (2)) was subsequently FIXED by CODER-F5 and verified green by FIN-H3 — see the RESOLVED section above.


**Symptom (both TB-run checks):**
`POST /v1/refunds` for a deposit whose hold was already CAPTURED returns
`502` (`LedgerError::Backend`) against the real TigerBeetle; the identical
flow returns `201` in sim mode. Refund replay fails the same way (the
deterministic id makes the two FAILs one bug). Balances were verified
unchanged — no money moved incorrectly; the operation is wrongly REFUSED.

**Isolated reproduction (harness-independent, real TB 0.16.28 + tb-live
binary; script + service log checkpointed):**

```
hold 1000 -> 201; capture 1000 -> 200;
POST /v1/refunds {deposit_id, amount_cents:1000} ->
  502 {"error":"ledger backend error: tigerbeetle void transfer rejected:
       [PendingTransferAlreadyPosted]"}
```

**Root cause (pinned to source):** the refund path first attempts to VOID
the hold, expecting "already resolved" to fall through to a posted refund.
Real TigerBeetle 0.16.28 answers `pending_transfer_already_posted` (result
33 — `state_machine.zig:2522`, `.posted => return .pending_transfer_already_posted`)
when the pending transfer was already POSTED, but
`services/payments-service/src/ledger/tigerbeetle.rs classify_void_kinds()`
only maps `PendingTransferNotPending` (result 26 — a NON-pending transfer
id) to the `NotPending` fall-through; `PendingTransferAlreadyPosted` falls
into `Backend` → 502. Fix (service-side, owned by W-P — NOT applied by
FIN-H per scope): treat `CreateTransferErrorKind::PendingTransferAlreadyPosted`
as `VoidClassification::NotPending` in `classify_void_kinds` (and consider
`PendingTransferAlreadyVoided` → AlreadyResolved semantics for void-after-void
parity with the sim).

**Impact:** sim mode proves the route/contract logic; the live-ledger
mapping is wrong for the post-capture refund leg only. Void-of-pending
(409/rejected capture-after-void), P-11 wrong-amount 400, and the
cross-tenant refund 403 all PASS on the real ledger.

## Harness changes made for the moved W44 contracts (harness.py, mirror +
backup; 89 857 B → 109 162 B, md5 4dbc1ce47c798e86cd9ce83dce9d8376)

1. **Payout flow → beneficiary registry (K7):** happy-path and over-limit
   payouts now register a beneficiary first and pass `beneficiary_id` (raw
   `payee` bodies are 422 post-W44). Added negatives: raw payee 422, foreign
   beneficiary 422, disabled beneficiary 422 — each asserting ZERO rail
   calls, zero balance drift, and no `payout_attempts` row (resolve_payee
   precedes the ledger hold). Beneficiary create/list/disable happy paths
   asserted.
2. **Gateway-auth matrix (K1/K6) on payments + billing:** new human-style
   calls with `X-Tenant-Slugs`/`X-User-Id`/`X-User-Roles`: owner 200/201,
   role-less member 403, foreign tenant 403. The pre-existing internal-token
   calls (K2) are unchanged — they remain valid and now also prove the
   role-gate exemption.
3. **Deposit provenance (K7):** gateway-authed hold carries
   `psp_reference`; the harness asserts the `deposit_provenance` row in the
   real `payments` DB (`declared_by == X-User-Id`, psp_reference + tenant
   persisted).
4. **No-show fee suite (new):** partial fee capture from a pending hold
   (post.amount == fee, `credits_pending -= hold`, revenue += response
   amount — response-driven, mode-agnostic) + same-key replay with zero
   drift.
5. **B-01 flipped live:** the webhook amount/currency mismatch cases now hit
   the post-W44 billing handler and PASS (202, invoice stays `issued`,
   `payment_mismatch` outbox row); the SKIP-pending-B branch remains as an
   honest fallback only. (Also fixed a latent psycopg bug in that block:
   `ILIKE '%mismatch%'` → `'%%mismatch%%'`.)
6. **Boot/auth contract moves:** booking now fails closed on the checked-in
   `PORTAL_SECRET` default (W-B) → harness sets a per-run random
   `PORTAL_SECRET`; booking's `tenantMiddleware` is error-closed on
   undecodable bearers (K-07) → the harness mints a structurally valid
   unsigned dev JWT with real `sub`/`tenant_slugs` claims; identity's
   createTenant/getTenant now require an authenticated subject (W-I-1) →
   harness sends `X-User-Id` on create and configures
   `IDENTITY_INTERNAL_TOKEN` on both identity and booking (booking forwards
   it as `X-Internal-Token` on `IDENTITY_BASE_URL` tenant resolution).
7. **F15-03:** payments `/healthz` dependency detail and `/metrics` counters
   asserted.
8. **Latent harness bug fixed (TB mode):** the post-capture balance snapshot
   (`payments.balance_post_capture`) was taken WITHOUT the internal token —
   post-P-09 balance reads are tenant-bound, so it silently mapped to `{}`
   and corrupted the two real-ledger capture assertions. Fixed; both now
   PASS with real deltas (post=5000 → revenue 4875 + fee 125 at 250 bps).

## Verbatim check list — SIM run (86/86 PASS, exit 0, 18.2s, 92 timed calls)

```
PASS service up: identity /healthz
PASS service up: booking /healthz
PASS service up: billing /healthz
PASS service up: payments /healthz
PASS identity: POST /v1/tenants (X-User-Id subject, W44) -> 201
PASS identity: GET /v1/tenants/{slug} returns id
PASS booking: GET /healthz -> 200
PASS booking: GET /public/sites/{slug} -> 200 (Dapr-free: tenant ctx fallback)
PASS booking: GET /public/sites/{slug}/context -> 200 with offerings
PASS booking: GET /public/sites/{slug}/offerings -> 200 with seeded offering
PASS booking: POST /v1/bookings (authed, IDENTITY_BASE_URL resolution) -> 201 pending
PASS booking: authed create committed booking row status=pending (no saga — honest degraded mode)
PASS booking: authed create outbox row committed, sent_at IS NULL (no Dapr — honest degraded mode)
PASS booking: authed create REPLAY same idempotency_key -> same booking, exactly 1 row
PASS booking: POST /public/sites/{slug}/bookings -> 201 pending (IDENTITY_BASE_URL resolution)
PASS booking: public create committed booking row status=pending (no saga — honest degraded mode)
PASS booking: public create outbox row committed, sent_at IS NULL (no Dapr — honest degraded mode)
PASS booking: public create REPLAY same idempotency_key -> same booking, exactly 1 row
PASS billing: PUT /v1/rate-cards/{t} -> 200
PASS billing: POST /v1/invoices/generate -> 201
PASS billing: generated invoice subtotal == 3000 cents (30 calls x 100)
PASS billing: POST /v1/invoices/{id}/issue -> 200 status=issued
PASS billing: POST /v1/invoices/{id}/payment-link -> 200 mode=static
PASS billing: gateway call (X-Tenant-Slugs bound + owner role) rate-card PUT -> 200 (K1 binding + K6 role)
PASS billing: role-less member money mutation -> 403 (K6 money-role gate)
PASS billing: foreign-tenant gateway call -> 403 (K1 X-Tenant-Slugs binding)
PASS billing: restarted with PAYSTACK_SECRET_KEY, /healthz ok
PASS billing: webhook with WRONG signature -> 401
PASS billing: webhook with REAL HMAC-SHA512 -> 200 status=paid
PASS billing: invoice now paid
PASS billing: invoice_issued ledger transfer posted (code 200, deterministic id)
PASS billing: invoice_paid ledger transfer posted (code 202, deterministic id)
PASS billing: billing_outbox InvoicePaid row committed (same-tx durability)
PASS billing: webhook REPLAY -> 200 already_paid
PASS billing: replay caused NO second ledger posting
PASS billing: replay caused NO second outbox row
PASS billing: webhook amount mismatch -> 202, invoice NOT paid, payment_mismatch event recorded (B-01)
PASS billing: webhook currency mismatch -> 202, invoice NOT paid, payment_mismatch event recorded (B-01)
PASS payments: POST /v1/deposits (hold) -> 201
PASS payments: hold REPLAY same key -> same deposit_id (no double-hold)
PASS payments: POST /v1/deposits/{id}/capture -> 200
PASS payments: capture REPLAY -> identical result, no double-post
PASS payments: GET /v1/accounts/{t}/balance -> 200 with accounts
PASS payments: POST /v1/internal/accounts/provision WITHOUT token -> 401 (fail-closed)
PASS payments: provision WITH internal token -> 200 (idempotent, exists-ok)
PASS payments: /healthz dependency-aware (F15-03): 200 ok; ledger+postgres probes ok; dlq_producer state reported; dead-letter gauge exposed
PASS payments: /metrics exposes commands/dead-letter/payout-outcome counters (F15-03)
PASS payments: fixture hold A (4000) -> 201
PASS payments: capture > hold REJECTED (400/409/422; TB-mode 502 accepted), balances unchanged
PASS payments: void hold -> 200; deposits credits_pending -= 4000, no posted drift
PASS payments: capture AFTER void REJECTED (400/409/422; TB-mode 502 accepted), balances unchanged
PASS payments: fixture hold X (1000) -> 201
PASS payments: cross-tenant CAPTURE -> 403 (P-06)
PASS payments: cross-tenant VOID -> 403 (P-06)
PASS payments: cross-tenant REFUND -> 403 (P-06)
PASS payments: gateway-authed hold (bound tenant + owner role) -> 201 (K1+K6)
PASS payments: deposit provenance recorded (K7): declared_by == X-User-Id, psp_reference + tenant persisted
PASS payments: role-less member money mutation -> 403 (K6 money-role gate)
PASS payments: foreign-tenant gateway call -> 403 (K1 X-Tenant-Slugs binding)
PASS payments: fixture hold B (2000) -> 201
PASS payments: capture B -> 200
PASS payments: refund (full, post-capture) -> 201; revenue debits_posted += 2000
PASS payments: refund REPLAY same key -> same refund id, balances byte-identical (no double refund)
PASS payments: fixture hold C (3000) -> 201
PASS payments: refund with wrong amount -> 400, balances unchanged (P-11)
PASS payments: fixture hold NS (2500) -> 201
PASS payments: no-show fee (1000 of 2500 hold) -> 201; post.amount == 1000, hold remainder released (credits_pending -= 2500), revenue += response amount
PASS payments: no-show fee REPLAY same key -> same post leg, balances unchanged (no double fee)
PASS payments: revenue has withdrawable funds for payout case
PASS payments: POST /v1/beneficiaries -> 201 (K7 vetted-destination registry)
PASS payments: GET /v1/beneficiaries?tenant_id -> lists the registered destination
PASS payments: payout with a RAW per-call payee (S1-F7-01 class) -> 422; zero ledger/rail side effects (K7 resolution precedes the hold)
PASS payments: fixture foreign beneficiary (other tenant) -> 201
PASS payments: payout referencing a FOREIGN beneficiary -> 422; zero ledger/rail side effects (K7 resolution precedes the hold)
PASS payments: fixture beneficiary (to disable) -> 201
PASS payments: POST /v1/beneficiaries/{id}/disable -> 200 disabled_at set
PASS payments: payout referencing a DISABLED beneficiary -> 422; zero ledger/rail side effects (K7 resolution precedes the hold)
PASS payments: payout happy path (K7 beneficiary_id) -> 201; LEDGER-FIRST pending->posted (post_pending leg, pending net 0), revenue debits += amount exactly, rail hit exactly once, payout_attempts row committed (P-01/C3)
PASS payments: payout OVER-LIMIT rejected BEFORE rail — no rail side effect, balances unchanged, no attempt row (C3 ledger-first)
PASS payments: fixture hold D (1500) -> 201
PASS payments: capture WITHOUT amount_cents resolved via lookup -> 200 post.amount == hold amount (C4/P-04)
PASS payments: PLATFORM_FEE_BPS=10001 boot REFUSED with explicit error (P-05)
PASS RLS: app_billing_login with WRONG app.tenant_id sees 0 invoices
PASS RLS: app_billing_login with '' app.tenant_id sees 0 invoices (W40-6 fail-closed)
PASS RLS: app_billing_login with GUC unset sees 0 invoices (fail-closed)
PASS RLS: app_billing_login with CORRECT app.tenant_id sees only its invoices
```

## Verbatim delta — FIN-H REAL-LEDGER run (89/91, exit 1, 164.2s, 96 timed calls) — SUPERSEDED by the FIN-H2 run above

Identical suite plus the real-ledger lines; only differences from SIM:

```
PASS tigerbeetle: format + start --development, 127.0.0.1:3000 accepting
PASS tigerbeetle: tenant+platform accounts created on the REAL ledger (pinned client crate, exists=idempotent)
PASS tigerbeetle: hold visible as PENDING on the real ledger (deposits credits_pending += hold amount)  — delta=5000
PASS tigerbeetle: capture MOVED FUNDS on the real ledger (balance deltas == post/revenue/fee amounts)   — post=5000 revenue=4875 fee=125
PASS tigerbeetle: capture REPLAY left every balance counter byte-identical (real TB `exists`, no double-post)
FAIL payments: refund (full, post-capture) -> 201; revenue debits_posted += 2000   — status=502 refund_id=None revenue_debits_delta=0   [BLOCKED #1]
FAIL payments: refund REPLAY same key -> same refund id, balances byte-identical (no double refund)   — status=502   [same root cause]
```

## Artifacts / checksums — FIN-H3 run (checkpointed to /mnt/agents/output/backups/w44/FIN-H3/)

| artifact | bytes | md5 |
|---|---|---|
| run-tb-final.log (full TB run, **91/91, exit 0**) | 15 004 | 3475c5e820a4d6a88a494dead40c27e5 |
| tb/funds-e2e-summary.json | 19 686 | 45a06f0d10cc3e9b904ccc574fedc95b |
| tb/timings/funds-e2e-timings.json | 18 594 | af918ea609339bdb7a7dc7902b9e0516 |
| run-sim-final.log (full SIM re-run, **86/86, exit 0**) | 12 554 | 555dff98b7bd17da46a798b9c9687e24 |
| sim/funds-e2e-summary.json | 17 048 | f1ade0b8ac0811d798da4aa0f26abc2c |
| sim/timings/funds-e2e-timings.json | 17 724 | aa395b017bd229016b6cae0788466401 |
| run-sim-run1-noexport.log (85/86 without the IDENTITY_BASE_URL export — env-artifact evidence) | 12 600 | 1808ad622948e9a5ea988351f2c15ae9 |
| build logs x5 (identity/booking/billing/payments-sim/payments-tb) | — | in backup dir |
| per-service logs (sim/logs, tb/logs incl. tigerbeetle.log) + tb fixture crate (tb/cargo-target) | — | in backup dir |

## Artifacts / checksums — FIN-H2 run (checkpointed to /mnt/agents/output/backups/w44/FIN-H2/)

| artifact | bytes | md5 |
|---|---|---|
| tb-final/run-tb4.log (canonical-binary full TB run, 90/91) | 15 058 | a975bc63798a8065a2eed6852f7543e6 |
| tb-final/funds-e2e-summary.json | 19 688 | d3fe7a978cd63e0b864d12f627feea7b |
| tb-final/funds-e2e-timings.json | 18 593 | 2a3393da9b757fad085843ab3b180431 |
| test-void-classification.log (6/6 unit tests) | 7 697 | 3cd857a3474ce465f1ce2cbebceffeb9 |
| repro_refund_replay.py (BLOCKED (2) isolated repro) | 3 540 | cbf29fbacc24c89a689d71ee4f135ad0 |
| repro2-payments.log (502 ExistsWithDifferentFlags evidence) | 4 688 | 66d669185f8ddfffc536c003b3dda72 |
| payments-service-tblive.bin.part0/.part1/.part2 (canonical tb-live binary, split for the /mnt 100-MiB write cap; concat = 156 410 224 B, md5 c211179dc961b5c043cfd36f9d2907d5) | 52 428 800 / 52 428 800 / 51 552 624 | c964c9d2be234a704866cd6e1802ca57 / 851cb4cadce5dab62cf415f16bf6ef02 / 4e6805f37046e1150f52fefb7589af4d |
| tigerbeetle-0.16.28-bin | 18 204 512 | 31288566a718ce9290571d3f3982b6da |
| tb-run1/ (interim 89/91 run without the IDENTITY_BASE_URL export; billing foreign-tenant 503 env artifact) | — | in backup dir |
| tb-run2/ (90/91 run, pre-canonical relink binary — same sources) | — | in backup dir |
| build logs x5 (billing/payments-tb/identity/booking + unit-test build) | — | in backup dir |
| per-service logs (tb-final/logs incl. tigerbeetle.log) | — | in backup dir |

## Artifacts / checksums (checkpointed to /mnt/agents/output/backups/w44/FIN-H/)

| artifact | bytes | md5 |
|---|---|---|
| sim/run-sim.log | 12 551 | d20152fa5140b2466068e9814601b542 |
| sim/funds-e2e-summary.json | 17 051 | 081faa1e0057ebe51b2dd7303f4aa78c |
| sim/funds-e2e-timings.json | 17 721 | 3d8a8fa7e2c936f47c5e2b8038928e22 |
| tb/run-tb.log | 14 968 | bb1f3559fb445d6cbc31087fedc728c5 |
| tb/funds-e2e-summary.json | 19 654 | c67f783e097e7da68c19398c17a31d5b |
| tb/funds-e2e-timings.json | 18 589 | 3f614604389b2426725bea02a88336e9 |
| tb/repro-refund-payments.log (isolated 502 evidence) | 3 502 | c0a9ca294d58577802ec12bc0b8ad0c1 |
| tb/repro_refund.py | 2 857 | 2adfa37fbcae660d1b78ea3441cf038d |
| harness.py.final (== mirror tests/funds-e2e/harness.py) | 109 162 | 4dbc1ce47c798e86cd9ce83dce9d8376 |
| build logs x5 (billing/payments-sim/payments-tb/identity/booking) | — | in backup dir |
| per-service logs (sim/logs, tb/logs incl. tigerbeetle.log) | — | in backup dir |

Previous W41-era results (V-Harness-R1, 34/34) are superseded by this file;
the W41 harness predates the W42 booking-write-path/W43 funds-hardening/W44
gateway-auth coverage.
