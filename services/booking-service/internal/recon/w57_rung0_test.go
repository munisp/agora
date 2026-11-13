package recon

// SPEC-W57 Scope A (A9): rung-0 (payload booking_id) + currency fold-in
// tests. Pure matcher tests need no database; the sweep tests reuse the
// embedded-Postgres rig and seed helpers from store_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// captureBookingID: rung-0 anchor is gated on kind=="payment_receipt" —
// the same posture as captureContactID's checkin gate.
func TestCaptureBookingIDPaymentReceiptOnly(t *testing.T) {
	bookingID := uuid.New()
	payload, _ := json.Marshal(map[string]any{"booking_id": bookingID.String()})

	id, ok := captureBookingID(ReceiptCapture{Kind: "payment_receipt", Payload: payload})
	if !ok || id != bookingID {
		t.Fatalf("payment_receipt booking_id: %v %v", id, ok)
	}
	// A checkin/lead carrying a booking_id key never enters rung 0.
	for _, kind := range []string{"checkin", "lead_capture", "civic_report"} {
		if _, ok := captureBookingID(ReceiptCapture{Kind: kind, Payload: payload}); ok {
			t.Fatalf("%s must not enter rung 0", kind)
		}
	}
	// Missing / null / malformed anchors yield no rung.
	for _, raw := range []string{`{}`, `{"booking_id":null}`, `{"booking_id":"nope"}`, `not-json`} {
		if _, ok := captureBookingID(ReceiptCapture{Kind: "payment_receipt", Payload: json.RawMessage(raw)}); ok {
			t.Fatalf("payload %s must yield no rung-0 anchor", raw)
		}
	}
}

// Currency fold-in: parseReceiptFields reads receipt_fields.currency
// tolerantly (absent/null/blank → "").
func TestParseReceiptFieldsCurrency(t *testing.T) {
	v := parseReceiptFields(json.RawMessage(`{"amount":50.00,"currency":"NGN"}`))
	if v.Currency != "NGN" {
		t.Fatalf("currency: %+v", v)
	}
	for _, raw := range []string{`{"amount":1}`, `{"currency":null}`, `{"currency":"  "}`, `null`, `not-json`} {
		if v := parseReceiptFields(json.RawMessage(raw)); v.Currency != "" {
			t.Fatalf("raw %q must yield empty currency: %+v", raw, v)
		}
	}
	// Currency parse never disturbs amount/date parsing.
	v = parseReceiptFields(json.RawMessage(`{"amount":12.5,"date":"2025-03-10","currency":"usd"}`))
	if v.AmountMajor == nil || *v.AmountMajor != 12.5 || v.Date == nil || v.Currency != "usd" {
		t.Fatalf("combined parse: %+v", v)
	}
}

// seedUnpaidBooking inserts a booking with NO payment attempt at all.
func seedUnpaidBooking(t *testing.T, rig reconRig, tenantID, offeringID uuid.UUID) uuid.UUID {
	t.Helper()
	bookingID := uuid.New()
	now := time.Now().UTC()
	if _, err := rig.raw.Exec(context.Background(),
		`INSERT INTO bookings (id, tenant_id, offering_id, starts_at, ends_at, status, source, payment_ref, payment_status)
		 VALUES ($1,$2,$3,$4,$5,'confirmed','api','','')`,
		bookingID, tenantID, offeringID, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("unpaid booking: %v", err)
	}
	return bookingID
}

func w57Offering(t *testing.T, rig reconRig, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	offering := uuid.New()
	if _, err := rig.raw.Exec(context.Background(),
		`INSERT INTO offerings (id, tenant_id, name, price_cents, currency) VALUES ($1,$2,'Consult',5000,'NGN')`,
		offering, tenantID); err != nil {
		t.Fatalf("offering: %v", err)
	}
	return offering
}

func findingsByCapture(t *testing.T, rig reconRig, tenantID uuid.UUID) map[string]Finding {
	t.Helper()
	rows, err := rig.st.ListFindings(context.Background(), tenantID, "all", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := map[string]Finding{}
	for _, f := range rows {
		if f.CaptureClientID != nil {
			out[*f.CaptureClientID] = f
		}
	}
	return out
}

func detailOf(t *testing.T, f Finding) map[string]any {
	t.Helper()
	var detail map[string]any
	if err := json.Unmarshal(f.Detail, &detail); err != nil {
		t.Fatalf("detail json: %v", err)
	}
	return detail
}

// Rung 0 end-to-end: paid booking linked by payload booking_id with
// agreeing amounts → NO finding and NO payment_without_receipt; an unpaid
// linked booking → receipt_without_payment at confidence EXACT with
// reason=linked_booking_unpaid and winning_rung=0 (the cash-collection
// detector); a third capture whose booking_id points nowhere falls through
// to the heuristic ladder (payment_receipt has no contact/lead anchor).
func TestSweepRung0LinkedBookings(t *testing.T) {
	rig := newReconRig(t)
	ctx := context.Background()
	tenant := uuid.New()
	offering := w57Offering(t, rig, tenant)
	now := time.Now().UTC()
	capturedAt := now.Add(-time.Hour)
	today := now.Format("2006-01-02")

	// cap-linked-paid: rung-0 hit, paid attempt, amounts agree → clean.
	_, paidBooking := seedPaidBooking(t, rig, tenant, offering, "+2348010000001", now, now, 5000, "tx-p1")
	seedReceiptCapture(t, rig, tenant, "cap-linked-paid", "payment_receipt",
		fmt.Sprintf(`{"booking_id":%q}`, paidBooking), "",
		fmt.Sprintf(`{"amount":50.00,"date":%q,"currency":"NGN"}`, today), capturedAt)

	// cap-linked-unpaid: rung-0 hit, NO paid attempt → the P4 signal.
	unpaidBooking := seedUnpaidBooking(t, rig, tenant, offering)
	seedReceiptCapture(t, rig, tenant, "cap-linked-unpaid", "payment_receipt",
		fmt.Sprintf(`{"booking_id":%q}`, unpaidBooking), "",
		fmt.Sprintf(`{"amount":30.00,"date":%q,"currency":"NGN"}`, today), capturedAt)

	// cap-linked-gone: booking_id does not exist → rung 0 misses, no other
	// anchor → receipt_without_payment at confidence none.
	seedReceiptCapture(t, rig, tenant, "cap-linked-gone", "payment_receipt",
		fmt.Sprintf(`{"booking_id":%q}`, uuid.New()), "",
		`{"vendor":"Stall"}`, capturedAt)

	sum, err := rig.svc.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if sum.TenantsScanned != 1 || sum.TenantsFailed != 0 {
		t.Fatalf("summary: %+v", sum)
	}

	byCapture := findingsByCapture(t, rig, tenant)

	// Clean rung-0 pair: no capture finding at all.
	if f, found := byCapture["cap-linked-paid"]; found {
		t.Fatalf("clean rung-0 pair must produce no finding: %+v", f)
	}

	// Linked-but-unpaid: receipt_without_payment, EXACT, booking linked.
	f, found := byCapture["cap-linked-unpaid"]
	if !found {
		t.Fatal("linked_booking_unpaid finding missing")
	}
	if f.Kind != KindReceiptWithoutPayment || f.Confidence != ConfidenceExact {
		t.Fatalf("linked-unpaid kind/confidence: %+v", f)
	}
	if f.BookingID == nil || *f.BookingID != unpaidBooking {
		t.Fatalf("linked-unpaid booking_id: %+v", f)
	}
	d := detailOf(t, f)
	if d["reason"] != "linked_booking_unpaid" || d["winning_rung"].(float64) != 0 {
		t.Fatalf("linked-unpaid detail: %+v", d)
	}
	// P7: the receipt carried NGN → currency_unknown is FALSE and the row
	// + detail carry the receipt currency.
	if cu, _ := d["currency_unknown"].(bool); cu {
		t.Fatalf("currency_unknown must be conditional (receipt had NGN): %+v", d)
	}
	if d["receipt_currency"] != "NGN" || f.ReceiptCurrency != "NGN" {
		t.Fatalf("receipt currency fold-in: detail=%+v row=%q", d, f.ReceiptCurrency)
	}

	// Gone booking: falls through the ladder → no candidate anywhere.
	f, found = byCapture["cap-linked-gone"]
	if !found || f.Kind != KindReceiptWithoutPayment || f.Confidence != ConfidenceNone {
		t.Fatalf("gone booking must fall through to confidence none: %+v", f)
	}
	if d := detailOf(t, f); d["reason"] != "no_payment_candidate" {
		t.Fatalf("gone booking detail: %+v", d)
	}

	// No payment_without_receipt: the paid booking was receipt-linked via
	// rung 0 and the unpaid booking has no paid attempt. (These findings
	// carry no capture_client_id — read the full listing.)
	all, err := rig.st.ListFindings(ctx, tenant, "all", 100)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	for _, f := range all {
		if f.Kind == KindPaymentWithoutReceipt {
			t.Fatalf("no payment_without_receipt expected: %+v", f)
		}
	}

	// Re-sweep idempotence (dedupe key covers rung-0 findings).
	sum2, err := rig.svc.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("re-sweep: %v", err)
	}
	if sum2.FindingsNew != 0 || sum2.FindingsDuped != 2 {
		t.Fatalf("re-sweep: %+v (want 0 new / 2 deduped)", sum2)
	}
}

// Rung-0 precedence: the payload booking_id wins over a rung-3 amount+date
// candidate pointing at a DIFFERENT booking (rung 0 has no time window and
// outranks the heuristic ladder).
func TestSweepRung0PrecedenceOverHeuristic(t *testing.T) {
	rig := newReconRig(t)
	ctx := context.Background()
	tenant := uuid.New()
	offering := w57Offering(t, rig, tenant)
	now := time.Now().UTC()
	capturedAt := now.Add(-time.Hour)
	today := now.Format("2006-01-02")

	// A paid booking whose amount+date WOULD match the receipt (rung 3)…
	_, otherPaid := seedPaidBooking(t, rig, tenant, offering, "+2348010000002", now, now, 7500, "tx-p2")
	// …but the agent linked an UNPAID booking — rung 0 must win and report
	// linked_booking_unpaid against the LINKED booking, never the rung-3 one.
	unpaid := seedUnpaidBooking(t, rig, tenant, offering)
	seedReceiptCapture(t, rig, tenant, "cap-precedence", "payment_receipt",
		fmt.Sprintf(`{"booking_id":%q}`, unpaid), "",
		fmt.Sprintf(`{"amount":75.00,"date":%q}`, today), capturedAt)

	if _, err := rig.svc.SweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	f, found := findingsByCapture(t, rig, tenant)["cap-precedence"]
	if !found {
		t.Fatal("precedence finding missing")
	}
	d := detailOf(t, f)
	if f.Kind != KindReceiptWithoutPayment || f.Confidence != ConfidenceExact ||
		d["reason"] != "linked_booking_unpaid" || d["winning_rung"].(float64) != 0 {
		t.Fatalf("rung 0 must outrank rung 3: kind=%s conf=%s detail=%+v", f.Kind, f.Confidence, d)
	}
	if f.BookingID == nil || *f.BookingID != unpaid || *f.BookingID == otherPaid {
		t.Fatalf("the finding must point at the LINKED booking: %+v", f.BookingID)
	}
	// …and the rung-3 candidate booking keeps its own honest orphan signal
	// (payment_without_receipt findings carry no capture_client_id, so
	// they are read from the full listing, not findingsByCapture).
	all, err := rig.st.ListFindings(ctx, tenant, "all", 100)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	var orphanForOther bool
	for _, of := range all {
		if of.Kind == KindPaymentWithoutReceipt && of.BookingID != nil && *of.BookingID == otherPaid {
			orphanForOther = true
		}
	}
	if !orphanForOther {
		t.Fatal("the unmatched paid booking must still flag payment_without_receipt")
	}
}

// Currency fold-in (P7): both currencies present and DIFFERENT → not
// comparable, NEVER converted — a currency_mismatch detail on a
// currency_unknown finding (exact pair blocked); both present and EQUAL
// with agreeing amounts → clean pair, no finding, no currency_unknown.
func TestSweepCurrencyMismatchAndMatch(t *testing.T) {
	rig := newReconRig(t)
	ctx := context.Background()
	tenant := uuid.New()
	offering := w57Offering(t, rig, tenant)
	now := time.Now().UTC()
	capturedAt := now.Add(-time.Hour)
	today := now.Format("2006-01-02")

	// cap-fx-mismatch: receipt says USD 50.00, the rail captured 5000 NGN —
	// numerically equal but different currencies: NOT comparable.
	_, fxBooking := seedPaidBooking(t, rig, tenant, offering, "+2348010000003", now, now, 5000, "tx-fx")
	seedReceiptCapture(t, rig, tenant, "cap-fx-mismatch", "payment_receipt",
		fmt.Sprintf(`{"booking_id":%q}`, fxBooking), "",
		fmt.Sprintf(`{"amount":50.00,"date":%q,"currency":"USD"}`, today), capturedAt)

	// cap-fx-match: NGN receipt vs NGN rail, amounts agree → clean.
	_, okBooking := seedPaidBooking(t, rig, tenant, offering, "+2348010000004", now, now, 5000, "tx-ok")
	seedReceiptCapture(t, rig, tenant, "cap-fx-match", "payment_receipt",
		fmt.Sprintf(`{"booking_id":%q}`, okBooking), "",
		fmt.Sprintf(`{"amount":50.00,"date":%q,"currency":"NGN"}`, today), capturedAt)

	if _, err := rig.svc.SweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	byCapture := findingsByCapture(t, rig, tenant)

	f, found := byCapture["cap-fx-mismatch"]
	if !found {
		t.Fatal("currency_mismatch finding missing")
	}
	if f.Kind != KindCurrencyUnknown || f.Confidence != ConfidenceExact {
		t.Fatalf("mismatched exact pair must surface as currency_unknown/exact: %+v", f)
	}
	d := detailOf(t, f)
	if cm, _ := d["currency_mismatch"].(bool); !cm {
		t.Fatalf("currency_mismatch detail missing: %+v", d)
	}
	if d["receipt_currency"] != "USD" || d["rail_currency"] != "NGN" || d["reason"] != "currency_mismatch" {
		t.Fatalf("mismatch detail must carry both currencies, never a conversion: %+v", d)
	}
	if cu, _ := d["currency_unknown"].(bool); cu {
		t.Fatalf("receipt currency present → currency_unknown false: %+v", d)
	}
	if f.ReceiptCurrency != "USD" {
		t.Fatalf("finding row must carry receipt_currency: %q", f.ReceiptCurrency)
	}
	// Amounts were NEVER converted/compared across currencies.
	if f.ExpectedAmountMinor != nil || f.CapturedAmountMinor != nil {
		t.Fatalf("cross-currency pair must not record amount columns: %+v", f)
	}

	// Same-currency agreeing pair: nothing emitted.
	if f, found := byCapture["cap-fx-match"]; found {
		t.Fatalf("same-currency agreeing pair must be clean: %+v", f)
	}
}
