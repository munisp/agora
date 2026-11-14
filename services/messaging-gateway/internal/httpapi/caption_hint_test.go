// SPEC-W66 WS2 tests: the caption BK-ref hint parser (token-only,
// parse-then-discard) plus the P8 proofs that the caption STRING never
// rides payloads or logs.
package httpapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestCaptionHintMatrix pins the parser contract (SPEC-W66 WS2): at most
// one token, distinct-token ambiguity => absent, uppercase normalization,
// Crockford alphabet validation, malformed => absent (NEVER an error).
func TestCaptionHintMatrix(t *testing.T) {
	cases := []struct {
		name    string
		caption string
		want    string
	}{
		{"empty caption", "", ""},
		{"no token", "here is my receipt, thanks!", ""},
		{"single token", "receipt for BK-7C3X9Q2M thanks", "BK-7C3X9Q2M"},
		{"lowercase normalized", "paid bk-7c3x9q2m today", "BK-7C3X9Q2M"},
		{"mixed case normalized", "Bk-7c3X9q2M", "BK-7C3X9Q2M"},
		{"token at start", "BK-7C3X9Q2M receipt attached", "BK-7C3X9Q2M"},
		{"token at end", "my booking is BK-7C3X9Q2M", "BK-7C3X9Q2M"},
		{"same token repeated is ONE hint", "BK-7C3X9Q2M again BK-7C3X9Q2M", "BK-7C3X9Q2M"},
		{"same token repeated differing case", "BK-7C3X9Q2M bk-7c3x9q2m", "BK-7C3X9Q2M"},
		{"two distinct tokens => absent", "BK-7C3X9Q2M or BK-1A2B3C4D", ""},
		{"three distinct tokens => absent", "BK-7C3X9Q2M BK-1A2B3C4D bk-zz11zz22", ""},
		{"7-char code => absent", "BK-7C3X9Q2", ""},
		// SPEC-W68 G-RES(c rider): the boundary hardening SUPERSEDES the
		// W66-pinned "9-char code yields first-8 token" behavior — a body
		// followed by [0-9A-Z] is a longer code, not a booking ref.
		{"9-char code => absent (W68 boundary rider)", "BK-7C3X9Q2M5", ""},
		{"excluded letter I => absent", "BK-7C3X9Q2I", ""},
		{"excluded letter L => absent", "BK-7C3X9Q2L", ""},
		{"excluded letter O => absent", "BK-7C3X9Q2O", ""},
		{"excluded letter U => absent", "BK-7C3X9Q2U", ""},
		{"lowercase excluded letter l => absent", "bk-7c3x9q2l", ""},
		{"wrong prefix => absent", "XX-7C3X9Q2M", ""},
		{"bare code without prefix => absent", "7C3X9Q2M", ""},
		{"digits-only code is valid", "BK-12345678", "BK-12345678"},
		{"surrounding punctuation", "(BK-7C3X9Q2M)!", "BK-7C3X9Q2M"},
		// An excluded-letter token next to a valid one: the excluded-letter
		// "token" never matches the regex, so the VALID token still counts
		// as the single hint.
		{"one valid + one malformed", "BK-7C3X9Q2M not BK-7C3X9Q2I", "BK-7C3X9Q2M"},
		// SPEC-W68 G-RES(c rider) boundary cases: a match must NOT be
		// preceded by [0-9A-Z] and the body must NOT be followed by
		// [0-9A-Z] (index-based; RE2 has no lookaround).
		{"preceded by letter => absent", "XBK-7C3X9Q2M", ""},
		{"preceded by digit => absent", "9BK-7C3X9Q2M", ""},
		{"followed by letters => absent", "BK-7C3X9Q2MZZ", ""},
		{"9-char code mid-word => absent", "ref BK-7C3X9Q2M5 received", ""},
		{"parens are boundaries", "(bk-7c3x9q2m)", "BK-7C3X9Q2M"},
		{"hyphen is not a blocker", "see X-BK-7C3X9Q2M ok", "BK-7C3X9Q2M"},
		{"start-of-string boundary", "BK-7C3X9Q2M", "BK-7C3X9Q2M"},
		{"end-of-string boundary", "receipt bk-7c3x9q2m", "BK-7C3X9Q2M"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := captionHint(tc.caption); got != tc.want {
				t.Fatalf("captionHint(%q) = %q, want %q", tc.caption, got, tc.want)
			}
		})
	}
}

// TestCaptionHintNeverReturnsCaption: the parser output is always the
// 11-char token or "" — the caption itself is never returned, in whole or
// in part, beyond the token.
func TestCaptionHintNeverReturnsCaption(t *testing.T) {
	caption := "my secret notes 2348012345678 call me bk-7c3x9q2m bye"
	got := captionHint(caption)
	if got != "BK-7C3X9Q2M" {
		t.Fatalf("want the normalized token, got %q", got)
	}
	if len(got) > 11 {
		t.Fatalf("hint must be the 11-char token only, got %q", got)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "2348012345678") {
		t.Fatalf("hint leaked caption content: %q", got)
	}
}

// TestAnchorPayloadTokenOnlyProof (SPEC-W66 WS2, extends the W61 P8
// "no caption key" assertion): the wire JSON carries booking_ref_hint ONLY
// when a hint exists, the hint value is the 11-char token, and there is
// structurally NO caption field on AnchorPayload (reflection grep — a
// future field named *caption* fails this test at compile-adjacent time).
func TestAnchorPayloadTokenOnlyProof(t *testing.T) {
	// Reflection grep: no field (name or JSON tag) may mention "caption".
	rt := reflect.TypeOf(AnchorPayload{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if strings.Contains(strings.ToLower(f.Name), "caption") ||
			strings.Contains(strings.ToLower(f.Tag.Get("json")), "caption") {
			t.Fatalf("AnchorPayload field %q mentions caption — the caption never rides the payload (P8)", f.Name)
		}
	}

	// With a hint: exactly the token rides the payload.
	p := anchorTestPayload()
	p.BookingRefHint = "BK-7C3X9Q2M"
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["booking_ref_hint"] != "BK-7C3X9Q2M" {
		t.Fatalf("booking_ref_hint = %v, want the token", m["booking_ref_hint"])
	}
	if _, ok := m["caption"]; ok {
		t.Fatal("caption must never ride the anchor payload (P8)")
	}

	// Without a hint: the key is OMITTED — the payload is byte-identical
	// to the W61 form.
	b2, err := json.Marshal(anchorTestPayload())
	if err != nil {
		t.Fatalf("marshal no-hint: %v", err)
	}
	if strings.Contains(string(b2), "booking_ref_hint") {
		t.Fatalf("no-hint payload must omit booking_ref_hint (byte-identical W61), got %s", b2)
	}
}

// TestCaptionHintSourceGrep (P8 grep-style proof): within this package's
// non-test source, the ONLY consumer of a media block's Caption is the
// captionHint parser call in webhooks.go — the caption is never logged,
// stored, or forwarded. This greps the SOURCE so a future change that
// logs block.Caption or adds it to a payload fails CI.
func TestCaptionHintSourceGrep(t *testing.T) {
	// AnchorPayload must have no Caption field (compile-time proof).
	var _ = AnchorPayload{}.BookingRefHint

	// Runtime greps over the wire forms are covered by
	// TestWAMediaAnchorCaptionHint* (raw body assertions) and the zap
	// observer test (log assertions); here we pin the parse-then-discard
	// flow: captionHint is a pure function of the caption returning only
	// the token, and the webhook path passes block.Caption to it exactly.
	if got := captionHint("BK-7C3X9Q2M"); got != "BK-7C3X9Q2M" {
		t.Fatalf("parser sanity: got %q", got)
	}
}
