# tests/fuzz — RESULTS

Status: **EXECUTED** — Go fuzz lane (V-Go) plus both Rust proptest lanes
(independent verifier V-Rust on pristine /tmp copies, re-verified by
V-Rust-R1). Effective generated property cases: 6x128 + 4x256 = 1,792,
0 failures. Commands: see [README.md](README.md).

## Go fuzz — FuzzVerifySignature (crm-sync-service) — EXECUTED 2026-08-17 (V-Go)

* Date/host: 2026-08-17 04:32:38 -> 04:34:09 CST, sandbox (2 CPU / 4 GB)
* Go version: `go1.23.4 linux/amd64`
* Command: `go test ./internal/httpapi/ -run xxx -fuzz=FuzzVerifySignature -fuzztime=90s`
  (pristine /tmp copy of services/crm-sync-service)
* Exec count: 373,912 execs (peak ~8,202/sec; two ~15s sandbox CPU stalls
  visible as 0/sec windows in the progress log — environmental, not hangs;
  run completed normally with PASS)
* Duration: 91.033s (`ok github.com/opendesk/crm-sync-service/internal/httpapi 91.033s`)
* New corpus files: 78 new-interesting inputs written to the GOCACHE fuzz
  corpus ($HOME/.cache/go-build/fuzz/.../FuzzVerifySignature, 78 files);
  total corpus 88 = 10 seed inputs + 78 generated
* Crashes/counterexamples: **0** — PASS, exit code 0. All four documented
  invariants (no panic; accept valid hex/base64/sha256=-prefixed digest;
  reject single-bit-flipped digest; empty secret never authenticates) held
  for every execution.

## Rust proptest — payments-service — EXECUTED (V-Rust)

* Environment: rustup stable 1.97.1, cmake 4.4.2 (pip, for rdkafka-sys),
  tb-live OFF; pristine /tmp copy of services/payments-service
* `cargo test --locked` exit code: 0
* Unit tests: `31 passed; 0 failed`
* Integration tests (`tests/proptest_ledger.rs`): `16 passed; 0 failed` =
  10 re-compiled sim unit tests + 6 property tests
  (prop_double_entry_conserved, prop_replay_is_idempotent,
  prop_no_overdraft_and_clean_rollback, prop_transfer_id_deterministic,
  prop_transfer_id_distinct_keys, prop_transfer_id_random_fallback)
* Proptest config: 128 cases/property (6 x 128 = 768 generated cases)
* Property tests run/passed: 6/6
* Counterexamples: **0**
* Mutation evidence (V-Rust): mutants applied to the /tmp copy, both
  killed — `apply_pending` skip-credit => prop_double_entry_conserved
  FAILED case 1; check_replay disabled => prop_replay_is_idempotent
  FAILED case 1. Mutants restored, re-ran green.
* Regression-guard mutation (V-Rust-R1): reverting ONE route to axum 0.8
  `{param}` syntax kills route_smoke (2 failed); full pre-fix simulation:
  3 assertions fail.

## Rust proptest — billing-engine — EXECUTED (V-Rust)

* Environment: rustup stable 1.97.1, cmake 4.4.2 (pip, for rdkafka-sys),
  tb-live OFF; pristine /tmp copy of services/billing-engine
* `cargo test --locked` exit code: 0
* Unit tests: `30 passed; 0 failed`
* Integration tests (`tests/proptest_signature.rs`): `12 passed; 0 failed`
  = 8 re-compiled payments_qr unit tests + 4 property tests
  (prop_never_panics, prop_valid_signature_accepted,
  prop_single_bit_flip_rejected, prop_wrong_length_rejected)
* `verify_paystack_signature` properties (no-panic / accept-valid /
  reject-bitflip / reject-length-mismatch): all PASS at 256
  cases/property (4 x 256 = 1,024 generated cases)
* Counterexamples: **0**
* Mutation evidence (V-Rust): `verify_paystack_signature` reduced to a
  length-only compare => prop_single_bit_flip_rejected FAILED. Mutant
  restored, re-ran green.
* Regression-guard mutation (V-Rust-R1): reverting ONE route to axum 0.8
  `{param}` syntax kills route_smoke (2 failed); full pre-fix simulation:
  2 assertions fail.
