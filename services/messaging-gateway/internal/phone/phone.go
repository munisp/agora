// Package phone pins the ONE canonical phone normalization form for the
// W61 media-anchor contract (SPEC-W61 §Phone normalization, G3):
// DIGITS-ONLY.
//
// The repo carries at least five divergent normalizers (notification-worker
// keeps '+', graph-sync hashes digits-only, voice strips separators, …) and
// Meta delivers the webhook `from` as E.164 WITHOUT the '+', while
// booking-service contacts.phone is stored UNNORMALIZED (exactly as the
// writer passed it). Without one pinned canonical form the anchor match
// silently never fires. There is intentionally no shared Go module (seven
// standalone modules, no go.work — SPEC-W61 NON-GOALS): the contract is
// pinned here plus SQL-side both-sides normalization in booking-service,
// and conformance fixtures in both test suites keep them in lockstep.
package phone

// Digits reduces a phone string to its canonical DIGITS-ONLY form: every
// character outside [0-9] is dropped (spaces, '+', parentheses, dashes),
// and if the result starts with the international-access prefix "00" that
// ONE prefix is stripped ("00234…" -> "234…"). No country inference, no
// national folding ("08012345678" stays "08012345678" — folding national
// formats would require country knowledge the gateway does not have and is
// explicitly out of scope).
func Digits(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= '0' && c <= '9' {
			b = append(b, c)
		}
	}
	d := string(b)
	if len(d) >= 2 && d[0] == '0' && d[1] == '0' {
		d = d[2:] // strip the "00" international prefix exactly ONCE
	}
	return d
}
