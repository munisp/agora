// SPEC-W61 G3 conformance tests for the pinned digits-only phone
// normalization contract. The SAME fixture set MUST exist in the
// booking-service store tests (SQL both-sides matching) — the two suites
// are the cross-service lock for the canonical form.
package phone

import "testing"

// conformanceFixtures are the SPEC-W61 §Phone normalization pinned cases.
// Any change here is a contract change, not an implementation detail.
var conformanceFixtures = []struct {
	name string
	in   string
	want string
}{
	{"meta digits-no-plus form passes through", "2348012345678", "2348012345678"},
	{"plus and separators stripped", "+234 801 234 5678", "2348012345678"},
	{"single 00 international prefix stripped once", "002348012345678", "2348012345678"},
	{"national format NOT folded", "08012345678", "08012345678"},
	{"us format with punctuation", "+1 (415) 555-0132", "14155550132"},
}

func TestDigitsConformanceFixtures(t *testing.T) {
	for _, f := range conformanceFixtures {
		if got := Digits(f.in); got != f.want {
			t.Errorf("%s: Digits(%q) = %q, want %q", f.name, f.in, got, f.want)
		}
	}
}

func TestDigitsEdgeCases(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},                               // empty stays empty
		{"+", ""},                              // no digits at all
		{"00", ""},                             // bare prefix strips to empty
		{"0002348012345678", "02348012345678"}, // ONE prefix stripped, not recursively
		{"  +234-801-234-5678 ", "2348012345678"},
		{"+234 (0) 801 234 5678", "23408012345678"}, // trunk digit kept — no country inference
		{"tel:+234-801-234-5678", "2348012345678"},  // uri-style junk dropped
	}
	for _, c := range cases {
		if got := Digits(c.in); got != c.want {
			t.Errorf("Digits(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
