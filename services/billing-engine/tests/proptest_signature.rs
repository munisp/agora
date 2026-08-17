//! Property-based tests (W41-6) for `payments_qr::verify_paystack_signature`
//! — the HMAC-SHA512 webhook signature check on the funds path.
//!
//! The crate is binary-only (no lib target), so the module is included
//! directly into this integration-test crate via `#[path]`.
//!
//! Properties under test (SPEC-W41 W41-6):
//! 1. Arbitrary (body, secret, signature) inputs never panic.
//! 2. A correctly computed HMAC-SHA512 hex signature is always accepted.
//! 3. Any single-bit flip in a valid signature is always rejected.
//! 4. A wrong-length signature is always rejected.
//!
//! The "correctly computed" signature is produced with the in-crate public
//! `hmac_sha512`; absolute correctness of that primitive is anchored by the
//! RFC 4231 / Paystack known-vector unit tests inside `payments_qr.rs` (they
//! compile into — and run as part of — this test binary under `cfg(test)`),
//! while these properties pin the verify/compare logic around it. The hex
//! encoding used here is implemented locally, independent of the crate's
//! private `hex_lower`.

#[path = "../src/payments_qr.rs"]
mod payments_qr;

use payments_qr::{hmac_sha512, verify_paystack_signature};
use proptest::prelude::*;

/// Local hex encoder, independent of the crate-under-test's private one.
fn hex_lower(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

fn body_strategy() -> impl Strategy<Value = Vec<u8>> {
    prop::collection::vec(any::<u8>(), 0..=2048)
}

fn secret_strategy() -> impl Strategy<Value = String> {
    // Arbitrary unicode, sometimes repeated so the key exceeds the HMAC block
    // size (128 bytes) and exercises the key-hashing branch.
    (any::<String>(), 1u8..=8).prop_map(|(s, n)| s.repeat(n as usize))
}

proptest! {
    #![proptest_config(ProptestConfig::with_cases(256))]

    /// P1: arbitrary inputs never panic (proptest fails the case on panic).
    #[test]
    fn prop_never_panics(
        secret in secret_strategy(),
        body in body_strategy(),
        sig in any::<String>(),
    ) {
        let _ = verify_paystack_signature(&secret, &body, &sig);
    }

    /// P2: a correctly computed HMAC-SHA512 hex signature is always accepted.
    /// Surrounding whitespace is tolerated by design (`signature.trim()`).
    #[test]
    fn prop_valid_signature_accepted(
        secret in secret_strategy(),
        body in body_strategy(),
    ) {
        let sig = hex_lower(&hmac_sha512(secret.as_bytes(), &body));
        prop_assert!(verify_paystack_signature(&secret, &body, &sig));
        let padded = format!("  {sig}\n");
        prop_assert!(verify_paystack_signature(&secret, &body, &padded));
    }

    /// P3: any single-bit flip anywhere in the 512-bit digest is always
    /// rejected.
    #[test]
    fn prop_single_bit_flip_rejected(
        secret in secret_strategy(),
        body in body_strategy(),
        byte_idx in 0usize..64,
        bit in 0u8..8,
    ) {
        let mut mac = hmac_sha512(secret.as_bytes(), &body);
        mac[byte_idx] ^= 1 << bit;
        let sig = hex_lower(&mac);
        prop_assert!(!verify_paystack_signature(&secret, &body, &sig));
    }

    /// P4: any signature whose trimmed length differs from the 128-char hex
    /// digest is always rejected (truncation and non-whitespace extension).
    #[test]
    fn prop_wrong_length_rejected(
        secret in secret_strategy(),
        body in body_strategy(),
        truncate_to in 0usize..128,
        extend_by in 1usize..=8,
        extend_char in prop::sample::select(
            "0123456789abcdef".chars().collect::<Vec<_>>()
        ),
        extend in any::<bool>(),
    ) {
        let sig = hex_lower(&hmac_sha512(secret.as_bytes(), &body));
        let mutated = if extend {
            let mut s = sig.clone();
            for _ in 0..extend_by {
                s.push(extend_char);
            }
            s
        } else {
            sig[..truncate_to].to_string()
        };
        prop_assert!(mutated.trim().len() != 128);
        prop_assert!(!verify_paystack_signature(&secret, &body, &mutated));
    }
}
