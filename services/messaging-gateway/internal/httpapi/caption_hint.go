// SPEC-W66 WS2: caption BK-ref token hint (W61 G5 follow-up).
//
// A WhatsApp media caption MAY quote the booking's public ref
// ("here's my receipt for BK-7C3X9Q2M"). The hint lets booking-service
// NARROW an ambiguous phone match or CORROBORATE a unique one — but it
// NEVER anchors without phone corroboration, NEVER creates anything, and a
// contradicting hint parks + flags the row (binding SPEC-W66 rulings).
//
// P8 (binding): the caption STRING is parse-then-discard. Only the
// 11-char normalized token may cross the wire (AnchorPayload.
// booking_ref_hint); the caption itself is never logged, stored, or
// forwarded. The parser therefore returns the token and nothing else, and
// callers must never log the caption input.
package httpapi

import (
	"regexp"
	"strings"
)

// captionBookingRefRe finds BK-XXXXXXXX tokens: "BK-" plus exactly 8
// Crockford-base32 characters (the SPEC-W51 public_ref alphabet, which
// excludes the ambiguous I/L/O/U). Case-insensitive because customers type
// refs in any case; matches are uppercase-normalized afterwards. The class
// itself already excludes i/l/o/u (Unicode simple folding keeps those
// letters out even under (?i)); the post-normalization alphabet validation
// in captionHint is the belt-and-suspenders check for exotic fold matches.
var captionBookingRefRe = regexp.MustCompile(`(?i)BK-[0-9A-HJKMNPQRSTVWXYZ]{8}`)

// crockfordAlphabet is the SPEC-W51 public_ref alphabet (Crockford base32,
// uppercase, no I/L/O/U) — booking-service public_ref.go's pinned set.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// captionHint extracts AT MOST ONE booking-ref hint from a media caption.
//
// Rules (SPEC-W66 WS2, binding):
//   - no token                     => "" (absent)
//   - one token (or the SAME token repeated) => the uppercase-normalized ref
//   - >=2 DISTINCT tokens          => "" (ambiguity never narrows)
//   - malformed (bad charset/length after normalization) => "" (absent)
//
// A malformed or ambiguous hint is NEVER an error — the hint is
// best-effort, so this function has no error return and the caller never
// answers 400 because of a caption. The returned string is the token ONLY;
// the caption is neither returned nor retained.
func captionHint(caption string) string {
	if caption == "" {
		return ""
	}
	matches := captionBookingRefRe.FindAllString(caption, -1)
	if len(matches) == 0 {
		return ""
	}
	hint := strings.ToUpper(matches[0])
	for _, m := range matches[1:] {
		if strings.ToUpper(m) != hint {
			return "" // >=2 DISTINCT tokens: ambiguity never narrows
		}
	}
	if !isValidBookingRefHint(hint) {
		return ""
	}
	return hint
}

// isValidBookingRefHint validates the canonical hint form: "BK-" + 8
// Crockford-alphabet chars (ASCII only). Anything else is malformed and is
// treated as absent by the caller (never surfaced as an error).
func isValidBookingRefHint(h string) bool {
	if len(h) != 11 || !strings.HasPrefix(h, "BK-") {
		return false
	}
	for i := 3; i < 11; i++ {
		c := h[i]
		if c >= 0x80 || !strings.ContainsRune(crockfordAlphabet, rune(c)) {
			return false
		}
	}
	return true
}
