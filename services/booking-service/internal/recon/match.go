package recon

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// SPEC-W56 Scope A item 3 / RECON §3.3: the matching ladder. Pure functions
// live here (unit-tested without a database); the SQL candidate loaders
// live in store.go and recon.go drives the ladder per capture.
// ---------------------------------------------------------------------------

// receiptView is the parsed subset of receipt_fields the matcher needs.
// Every sub-field is tolerant-null (ocr_client.go ReceiptFields contract —
// the LLM may fail to read any field).
type receiptView struct {
	AmountMajor *float64   // receipt_fields.amount — MAJOR units as printed
	DateRaw     string     // receipt_fields.date verbatim ("" when absent)
	Date        *time.Time // parsed receipt date; nil when absent/unparseable
	// Currency (SPEC-W57 §3.5): receipt_fields.currency verbatim (""
	// when absent). The OCR service has always extracted it; only the
	// parse/display sites dropped it (R2 fold-in — NO DDL).
	Currency string
}

// receiptDateLayouts are the unambiguous date layouts accepted for the
// amount+date rung. Deliberately ISO-first: ambiguous DD/MM vs MM/DD
// layouts are NOT guessed (R5 honesty — an unparseable date skips the
// rung instead of risking a transposed-day false pair).
var receiptDateLayouts = []string{
	"2006-01-02",
	"2006/01/02",
	"2006-01-02 15:04",
	"2006-01-02 15:04:05",
	time.RFC3339,
	"2006-01-02T15:04:05",
}

// parseReceiptFields extracts amount/date from a receipt_fields JSONB blob.
// Null/malformed input yields the zero view — never an error.
func parseReceiptFields(raw json.RawMessage) receiptView {
	var v receiptView
	if len(raw) == 0 || string(raw) == "null" {
		return v
	}
	var fields struct {
		Amount   *float64 `json:"amount"`
		Date     *string  `json:"date"`
		Currency *string  `json:"currency"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return v
	}
	v.AmountMajor = fields.Amount
	if fields.Currency != nil {
		v.Currency = strings.TrimSpace(*fields.Currency)
	}
	if fields.Date != nil {
		v.DateRaw = strings.TrimSpace(*fields.Date)
		if v.DateRaw != "" {
			for _, layout := range receiptDateLayouts {
				if t, err := time.Parse(layout, v.DateRaw); err == nil {
					t = t.UTC()
					v.Date = &t
					break
				}
			}
		}
	}
	return v
}

// receiptAmountMinor converts the receipt's MAJOR-unit float to integer
// minor units. SPEC sacred constraint: this conversion (math.Round ×100)
// happens at the comparison site ONLY; floats are never stored in money
// columns. ok=false on a null amount — a null receipt amount skips the
// amount rung entirely (never a guess, R5).
func receiptAmountMinor(amountMajor *float64) (minor int64, ok bool) {
	if amountMajor == nil {
		return 0, false
	}
	return int64(math.Round(*amountMajor * 100)), true
}

// windowBounds returns the inclusive ±days window around center used by the
// SQL BETWEEN predicates (boundary timestamps match — inclusive edges).
func windowBounds(center time.Time, days int) (from, to time.Time) {
	d := time.Duration(days) * 24 * time.Hour
	return center.Add(-d), center.Add(d)
}

// captureContactID extracts rung 1's anchor: payload->>'contact_id'.
// SPEC item 3: the exact rung is the CHECKIN contact chain ONLY — a
// non-checkin capture never enters rung 1 even if its payload happens to
// carry a contact_id key.
func captureContactID(c ReceiptCapture) (uuid.UUID, bool) {
	if c.Kind != "checkin" {
		return uuid.Nil, false
	}
	var p struct {
		ContactID *uuid.UUID `json:"contact_id"`
	}
	if err := json.Unmarshal(c.Payload, &p); err != nil || p.ContactID == nil || *p.ContactID == uuid.Nil {
		return uuid.Nil, false
	}
	return *p.ContactID, true
}

// captureBookingID extracts rung 0's anchor (SPEC-W57 P4):
// payload->>'booking_id'. Same gating posture as captureContactID — ONLY a
// payment_receipt capture enters rung 0, even if another kind's payload
// happens to carry a booking_id key.
func captureBookingID(c ReceiptCapture) (uuid.UUID, bool) {
	if c.Kind != "payment_receipt" {
		return uuid.Nil, false
	}
	var p struct {
		BookingID *uuid.UUID `json:"booking_id"`
	}
	if err := json.Unmarshal(c.Payload, &p); err != nil || p.BookingID == nil || *p.BookingID == uuid.Nil {
		return uuid.Nil, false
	}
	return *p.BookingID, true
}

// captureLeadID extracts rung 2's anchor: result->>'lead_id' (the capture
// outcome of a lead_capture item).
func captureLeadID(c ReceiptCapture) (uuid.UUID, bool) {
	var r struct {
		LeadID *uuid.UUID `json:"lead_id"`
	}
	if err := json.Unmarshal(c.Result, &r); err != nil || r.LeadID == nil || *r.LeadID == uuid.Nil {
		return uuid.Nil, false
	}
	return *r.LeadID, true
}

// dedupeCandidates collapses rung candidates by booking (one booking may
// surface twice via two contacts or two paid attempts — the fallback path
// can advance payment_ref). The latest-paid row per booking wins so the
// amount comparison sees the freshest capture.
func dedupeCandidates(in []PaymentCandidate) []PaymentCandidate {
	byBooking := map[uuid.UUID]PaymentCandidate{}
	var order []uuid.UUID
	for _, c := range in {
		prev, seen := byBooking[c.BookingID]
		if !seen {
			order = append(order, c.BookingID)
			byBooking[c.BookingID] = c
			continue
		}
		if c.PaidAt.After(prev.PaidAt) {
			byBooking[c.BookingID] = c
		}
	}
	out := make([]PaymentCandidate, 0, len(order))
	for _, id := range order {
		out = append(out, byBooking[id])
	}
	return out
}

// LadderOutcome is the §3.3 ladder verdict for one capture.
type LadderOutcome struct {
	// Confidence: exact (rung 1), heuristic (rungs 2/3) or none (no
	// candidate anywhere, or ambiguity at the winning rung).
	Confidence string
	// Candidate is the single winning candidate (nil when Confidence none).
	Candidate *PaymentCandidate
	// WinningRung: 1/2/3 for the classic ladder rungs; SPEC-W57 P4 adds
	// rung 0 (payload booking_id direct link, payment_receipt only) — a
	// rung-0 hit reports WinningRung 0 WITH Candidate set (ConfidenceExact).
	// WinningRung 0 with Candidate nil still means no rung produced
	// candidates. Rung 0 short-circuits in matchCapture (it can never be
	// ambiguous — one booking id dedupes to one candidate), so
	// decideLadder's 1-based semantics are unchanged.
	WinningRung int
	// Ambiguous carries the winning rung's candidate list when >1
	// (confidence 'none' + candidates in detail, per pinned decision 1).
	Ambiguous []PaymentCandidate
}

// decideLadder applies the ladder rule to the per-rung candidate lists
// (index 0 = rung 1 … index 2 = rung 3): the HIGHEST rung with at least
// one candidate decides — exactly one candidate wins with that rung's
// confidence; more than one is ambiguous (confidence 'none'); an empty
// rung falls through to the next.
func decideLadder(rungs [][]PaymentCandidate) LadderOutcome {
	for i, cands := range rungs {
		cands = dedupeCandidates(cands)
		switch len(cands) {
		case 0:
			continue
		case 1:
			conf := ConfidenceHeuristic
			if i == 0 {
				conf = ConfidenceExact
			}
			c := cands[0]
			return LadderOutcome{Confidence: conf, Candidate: &c, WinningRung: i + 1}
		default:
			return LadderOutcome{Confidence: ConfidenceNone, WinningRung: i + 1, Ambiguous: cands}
		}
	}
	return LadderOutcome{Confidence: ConfidenceNone}
}

// amountDivergence compares a matched pair: the receipt amount (major)
// against the rail-captured amount (minor). comparable=false when either
// side is unknown (null receipt amount or a pre-W56 attempt row with NULL
// amount_minor) — the caller then emits no amount verdict (and, for an
// exact-rung pair, the currency_unknown finding).
func amountDivergence(receiptMajor *float64, capturedMinor *int64) (divergent bool, comparable bool) {
	receiptMinor, ok := receiptAmountMinor(receiptMajor)
	if !ok || capturedMinor == nil {
		return false, false
	}
	return receiptMinor != *capturedMinor, true
}
