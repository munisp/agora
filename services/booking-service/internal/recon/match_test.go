package recon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// SPEC-W56 Scope A item 8: matcher unit tests — ladder rungs, ambiguity →
// none, null amount → no amount rung, window edges. Pure functions only
// (no database); the SQL-backed sweep is covered by store_test.go.

func cand(bookingID uuid.UUID, txRef string, amountMinor int64) PaymentCandidate {
	return PaymentCandidate{BookingID: bookingID, TxRef: txRef, AmountMinor: &amountMinor}
}

func TestDecideLadderRungConfidence(t *testing.T) {
	b1, b2 := uuid.New(), uuid.New()
	c1 := cand(b1, "tx-1", 5000)
	c2 := cand(b2, "tx-2", 5000)

	// Rung 1 single candidate → exact.
	o := decideLadder([][]PaymentCandidate{{c1}, {c2}, nil})
	if o.Confidence != ConfidenceExact || o.Candidate == nil || o.Candidate.BookingID != b1 || o.WinningRung != 1 {
		t.Fatalf("rung1 exact: %+v", o)
	}
	// Rung 1 empty, rung 2 single → heuristic.
	o = decideLadder([][]PaymentCandidate{nil, {c2}, nil})
	if o.Confidence != ConfidenceHeuristic || o.Candidate == nil || o.WinningRung != 2 {
		t.Fatalf("rung2 heuristic: %+v", o)
	}
	// Rungs 1+2 empty, rung 3 single → heuristic.
	o = decideLadder([][]PaymentCandidate{nil, nil, {c1}})
	if o.Confidence != ConfidenceHeuristic || o.Candidate == nil || o.WinningRung != 3 {
		t.Fatalf("rung3 heuristic: %+v", o)
	}
	// No candidates anywhere → none, no winning rung.
	o = decideLadder([][]PaymentCandidate{nil, nil, nil})
	if o.Confidence != ConfidenceNone || o.Candidate != nil || o.WinningRung != 0 {
		t.Fatalf("empty ladder: %+v", o)
	}
}

func TestDecideLadderAmbiguityStopsAtWinningRung(t *testing.T) {
	b1, b2, b3 := uuid.New(), uuid.New(), uuid.New()
	c1, c2, c3 := cand(b1, "tx-1", 5000), cand(b2, "tx-2", 5000), cand(b3, "tx-3", 5000)

	// Two candidates at rung 1 → ambiguity wins IMMEDIATELY (confidence
	// none + candidates in detail); a single rung-2 candidate must NOT
	// rescue the match.
	o := decideLadder([][]PaymentCandidate{{c1, c2}, {c3}, nil})
	if o.Confidence != ConfidenceNone || o.Candidate != nil || o.WinningRung != 1 || len(o.Ambiguous) != 2 {
		t.Fatalf("ambiguous rung1 must stop the ladder: %+v", o)
	}
	// Same rule at rung 2.
	o = decideLadder([][]PaymentCandidate{nil, {c1, c2}, {c3}})
	if o.Confidence != ConfidenceNone || o.WinningRung != 2 || len(o.Ambiguous) != 2 {
		t.Fatalf("ambiguous rung2 must stop the ladder: %+v", o)
	}
}

func TestDecideLadderDedupesSameBooking(t *testing.T) {
	b := uuid.New()
	older := cand(b, "tx-old", 5000)
	older.PaidAt = time.Now().Add(-time.Hour)
	newer := cand(b, "tx-new", 5000) // fallback path: newer paid attempt, same booking
	newer.PaidAt = time.Now()
	// The same booking surfacing twice in one rung is ONE candidate.
	o := decideLadder([][]PaymentCandidate{{older, newer}, nil, nil})
	if o.Confidence != ConfidenceExact || o.Candidate == nil || o.Candidate.TxRef != "tx-new" {
		t.Fatalf("same-booking dedupe must keep the latest paid attempt: %+v", o)
	}
}

func TestCaptureContactIDCheckinOnly(t *testing.T) {
	contactID := uuid.New()
	payload, _ := json.Marshal(map[string]any{"contact_id": contactID.String()})

	// checkin kind → rung 1 anchor extracted.
	id, ok := captureContactID(ReceiptCapture{Kind: "checkin", Payload: payload})
	if !ok || id != contactID {
		t.Fatalf("checkin contact_id: %v %v", id, ok)
	}
	// SPEC item 3: the exact rung is the CHECKIN contact chain ONLY — a
	// lead_capture carrying a contact_id key never enters rung 1.
	if _, ok := captureContactID(ReceiptCapture{Kind: "lead_capture", Payload: payload}); ok {
		t.Fatal("non-checkin capture must not enter the exact rung")
	}
	// Missing/malformed contact_id → no anchor.
	if _, ok := captureContactID(ReceiptCapture{Kind: "checkin", Payload: json.RawMessage(`{}`)}); ok {
		t.Fatal("missing contact_id must yield no anchor")
	}
	if _, ok := captureContactID(ReceiptCapture{Kind: "checkin", Payload: json.RawMessage(`{"contact_id":"nope"}`)}); ok {
		t.Fatal("malformed contact_id must yield no anchor")
	}
}

func TestCaptureLeadID(t *testing.T) {
	leadID := uuid.New()
	result, _ := json.Marshal(map[string]any{"lead_id": leadID.String()})
	id, ok := captureLeadID(ReceiptCapture{Result: result})
	if !ok || id != leadID {
		t.Fatalf("lead_id: %v %v", id, ok)
	}
	if _, ok := captureLeadID(ReceiptCapture{Result: json.RawMessage(`{"status":"applied"}`)}); ok {
		t.Fatal("result without lead_id must yield no anchor")
	}
	if _, ok := captureLeadID(ReceiptCapture{Result: json.RawMessage(`null`)}); ok {
		t.Fatal("null result must yield no anchor")
	}
}

func TestParseReceiptFieldsTolerantNull(t *testing.T) {
	// Full nulls (the OCR tolerant-null contract).
	v := parseReceiptFields(json.RawMessage(`{"vendor":null,"amount":null,"date":null,"items":[]}`))
	if v.AmountMajor != nil || v.Date != nil || v.DateRaw != "" {
		t.Fatalf("null fields: %+v", v)
	}
	// Null blob / empty / malformed → zero view, never an error.
	for _, raw := range []string{`null`, ``, `not-json`} {
		if v := parseReceiptFields(json.RawMessage(raw)); v.AmountMajor != nil || v.Date != nil {
			t.Fatalf("raw %q: %+v", raw, v)
		}
	}
	// Amount + ISO date parsed.
	v = parseReceiptFields(json.RawMessage(`{"amount":125.50,"date":"2025-03-10"}`))
	if v.AmountMajor == nil || *v.AmountMajor != 125.50 {
		t.Fatalf("amount: %+v", v)
	}
	if v.Date == nil || v.Date.Format("2006-01-02") != "2025-03-10" {
		t.Fatalf("date: %+v", v)
	}
	// Unparseable date → DateRaw kept (honest echo) but Date nil → the
	// amount+date rung is skipped (no day-transposition guessing).
	v = parseReceiptFields(json.RawMessage(`{"amount":10,"date":"10/03/2025"}`))
	if v.Date != nil || v.DateRaw != "10/03/2025" {
		t.Fatalf("ambiguous date layout must not parse: %+v", v)
	}
}

func TestReceiptAmountMinorConversion(t *testing.T) {
	// Null amount → ok=false (the rung-3 skip signal; R5 never-guess).
	if _, ok := receiptAmountMinor(nil); ok {
		t.Fatal("null amount must not convert")
	}
	// Major → minor, rounding at the comparison site ONLY.
	a := 12.34
	minor, ok := receiptAmountMinor(&a)
	if !ok || minor != 1234 {
		t.Fatalf("12.34 → %d %v", minor, ok)
	}
	b := 0.005
	if minor, _ = receiptAmountMinor(&b); minor != 1 {
		t.Fatalf("0.005 rounds to 1 minor unit, got %d", minor)
	}
}

func TestAmountDivergence(t *testing.T) {
	rail := int64(5000)
	match := 50.00
	mismatch := 50.01

	divergent, comparable := amountDivergence(&match, &rail)
	if !comparable || divergent {
		t.Fatalf("equal amounts: divergent=%v comparable=%v", divergent, comparable)
	}
	divergent, comparable = amountDivergence(&mismatch, &rail)
	if !comparable || !divergent {
		t.Fatalf("diverging amounts: divergent=%v comparable=%v", divergent, comparable)
	}
	// Null receipt amount → not comparable (never a mismatch guess).
	if divergent, comparable = amountDivergence(nil, &rail); comparable || divergent {
		t.Fatalf("null receipt amount: divergent=%v comparable=%v", divergent, comparable)
	}
	// Pre-W56 attempt row (NULL amount_minor) → not comparable.
	if divergent, comparable = amountDivergence(&match, nil); comparable || divergent {
		t.Fatalf("null rail amount: divergent=%v comparable=%v", divergent, comparable)
	}
}

func TestWindowBoundsInclusiveEdges(t *testing.T) {
	center := time.Date(2025, 3, 10, 12, 0, 0, 0, time.UTC)
	from, to := windowBounds(center, 7)
	if !from.Equal(center.AddDate(0, 0, -7)) || !to.Equal(center.AddDate(0, 0, 7)) {
		t.Fatalf("±7d bounds: %v … %v", from, to)
	}
	// Edge semantics: the SQL BETWEEN is inclusive, so a timestamp exactly
	// at ±days is inside the window; one second outside is not.
	edge := center.AddDate(0, 0, 7)
	if edge.Before(from) || edge.After(to) {
		t.Fatal("exact +7d edge must be inside the window")
	}
	if outside := to.Add(time.Second); !outside.After(to) {
		t.Fatal("one second past the edge must be outside")
	}
	from3, to3 := windowBounds(center, 3)
	if !from3.Equal(center.AddDate(0, 0, -3)) || !to3.Equal(center.AddDate(0, 0, 3)) {
		t.Fatalf("±3d bounds: %v … %v", from3, to3)
	}
}

func TestAnchorTimeFallback(t *testing.T) {
	now := time.Now().UTC()
	captured := now.Add(-2 * time.Hour)
	// captured_at present → the match anchor (SPEC rung 1 windows on
	// captured_at).
	c := ReceiptCapture{CapturedAt: &captured, ServerReceivedAt: now}
	if !c.anchorTime().Equal(captured) {
		t.Fatalf("anchor must be captured_at: %v", c.anchorTime())
	}
	// captured_at NULL (legacy row) → trusted server_received_at fallback.
	c = ReceiptCapture{ServerReceivedAt: now}
	if !c.anchorTime().Equal(now) {
		t.Fatalf("anchor fallback must be server_received_at: %v", c.anchorTime())
	}
}
