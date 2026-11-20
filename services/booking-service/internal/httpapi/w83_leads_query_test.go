package httpapi

// SPEC-W83: q filter shape for GET /v1/leads — the regex is the whole
// validation surface (a bad q is a deterministic 400 before the store is
// touched; the store applies its own 32-char defence-in-depth cap).

import "testing"

func TestLeadQueryReShape(t *testing.T) {
	accept := []string{
		"+234803",
		"0803",
		"+2347013333333",
		"234",
		"12345678901234567890123456789012", // exactly 32
	}
	for _, q := range accept {
		if !leadQueryRe.MatchString(q) {
			t.Fatalf("q %q must be accepted", q)
		}
	}
	reject := []string{
		"", "12", "+", "ab", " DROP", "+234 803", "+234-803",
		"+234;", "123456789012345678901234567890123", // 33
		"++234", "+234\n",
	}
	for _, q := range reject {
		if leadQueryRe.MatchString(q) {
			t.Fatalf("q %q must be rejected", q)
		}
	}
}
