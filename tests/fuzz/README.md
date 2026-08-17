# tests/fuzz — property/fuzz harness evidence (W41-6)

Two lanes, both executed in-sandbox by the W41 verifiers (Coder A owns the
Rust proptest code, Coder B owns the Go fuzz test; THIS directory is only
the runner documentation + evidence skeleton):

## Lane 1 — Go native fuzz: crm-sync webhook signature

Target: `services/crm-sync-service/internal/httpapi/webhook_fuzz_test.go`
(`FuzzVerifySignature`; seeds valid/invalid hex, base64, `sha256=` prefix).

```bash
rm -rf /tmp/fuzz-ws && mkdir -p /tmp/fuzz-ws
cp -r /mnt/agents/output/opendesk/services/crm-sync-service /tmp/fuzz-ws/
cd /tmp/fuzz-ws/crm-sync-service
GOFLAGS=-mod=readonly GOCACHE=/tmp/fuzz-ws/gocache GOMODCACHE=/tmp/fuzz-ws/gomodcache \
  GOPROXY=https://goproxy.cn,direct \   # proxy.golang.org unreachable from this sandbox
  /tmp/go/bin/go test -fuzz=FuzzVerifySignature -fuzztime=90s ./internal/httpapi/ \
  2>&1 | tee /tmp/fuzz-ws/fuzz.log
```

Expected: ~90 s fuzzing + build time; `PASS` with `0` new interesting
inputs beyond the corpus, or a counterexample that MUST be fixed in-wave.
The generated corpus under `testdata/fuzz/` is test data and is committed
by Coder B.

## Lane 2 — Rust proptest (stable toolchain, dev-dependency — NO nightly)

```bash
export PATH="$HOME/.cargo/bin:$PATH"   # rustup stable (minimal profile)
cd services/payments-service && cargo test --locked 2>&1 | tee /tmp/fuzz-ws/proptest-payments.log
cd ../billing-engine && cargo test --locked 2>&1 | tee /tmp/fuzz-ws/proptest-billing.log
```

Properties under test (per SPEC-W41-6):

* payments-service `SimLedgerClient`: random hold/capture/refund/no-show
  sequences => Σ debits == Σ credits always; replaying the same transfer id
  leaves state unchanged; no-overdraft never violated;
  `transfer_id_from_key` deterministic + distinct keys => distinct ids.
* billing-engine `payments_qr::verify_paystack_signature`: arbitrary
  body/secret never panics; correctly-computed signature always accepts;
  any single bit-flip in the signature always rejects; length mismatch
  rejects.

Expected duration: first `cargo test` build dominates (10-30 min on 2 CPU
per crate cold cache; much less with a warm CARGO_TARGET_DIR).
