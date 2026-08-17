package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// FuzzVerifySignature is the property harness for the Twenty webhook HMAC
// check (SPEC-W41 W41-6). Invariants over arbitrary secret/body/header:
//  1. it never panics;
//  2. a correctly computed HMAC-SHA256 digest — hex, base64, or
//     "sha256="-prefixed — is ALWAYS accepted (non-empty secret);
//  3. a mutated digest (any single bit flip) is ALWAYS rejected;
//  4. an empty secret NEVER authenticates.
func FuzzVerifySignature(f *testing.F) {
	goodBody := []byte(`{"event":"person.created","id":"1"}`)
	goodHex := signHex("s3cret", goodBody)
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(goodBody)
	goodB64 := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	// Seed corpus: valid signatures in every accepted encoding plus the
	// invalid shapes the unit tests already cover.
	f.Add("s3cret", goodBody, goodHex)
	f.Add("s3cret", goodBody, "sha256="+goodHex)
	f.Add("s3cret", goodBody, goodB64)
	f.Add("s3cret", goodBody, "sha256="+goodB64)
	f.Add("other", goodBody, goodHex)                 // wrong secret
	f.Add("s3cret", []byte(`{"event":"x"}`), goodHex) // tampered body
	f.Add("s3cret", goodBody, "not-a-signature")
	f.Add("s3cret", goodBody, "")
	f.Add("", goodBody, goodHex) // empty secret
	f.Add("s3cret", []byte{}, goodHex)

	f.Fuzz(func(t *testing.T, secret string, body []byte, header string) {
		// (1) Arbitrary input must never panic; result is a plain bool.
		_ = VerifySignature(secret, body, header)

		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := mac.Sum(nil)
		wantHex := hex.EncodeToString(want)
		wantB64 := base64.StdEncoding.EncodeToString(want)

		if secret == "" {
			// (4) Empty secret never authenticates, whatever the header.
			if VerifySignature(secret, body, wantHex) {
				t.Fatal("empty secret: correctly computed signature accepted")
			}
			return
		}

		// (2) Correctly computed signature, any accepted encoding.
		if !VerifySignature(secret, body, wantHex) {
			t.Fatal("valid hex signature rejected")
		}
		if !VerifySignature(secret, body, "sha256="+wantHex) {
			t.Fatal("valid sha256=-prefixed hex signature rejected")
		}
		if !VerifySignature(secret, body, wantB64) {
			t.Fatal("valid base64 signature rejected")
		}
		if !VerifySignature(secret, body, "sha256="+wantB64) {
			t.Fatal("valid sha256=-prefixed base64 signature rejected")
		}

		// (3) Any single bit flip in the digest must be rejected, in both
		// encodings (the mutation is on the decoded bytes, so re-encoding
		// always yields a well-formed header of the right length).
		mutated := make([]byte, len(want))
		copy(mutated, want)
		mutated[0] ^= 0x01
		if VerifySignature(secret, body, hex.EncodeToString(mutated)) {
			t.Fatal("mutated hex signature accepted")
		}
		if VerifySignature(secret, body, base64.StdEncoding.EncodeToString(mutated)) {
			t.Fatal("mutated base64 signature accepted")
		}
	})
}
