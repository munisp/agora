package recon

// SPEC-W78 A4: clean-pair observability tests (real embedded Postgres per
// package convention — the rig rides store_test.go's harness on dedicated
// port 5580; -short skips).
//   (a) 1 clean pair + 1 mismatch pair → Summary.CleanPairs==1, findings==1
//   (b) a not-comparable HEURISTIC pair produces no finding AND does NOT
//       count as clean (the nil-return ambiguity is disambiguated)
//   (c) the booking_recon_clean_pairs_total counter delta over the run
//       equals Summary.CleanPairs (gather-and-parse off the DEFAULT
//       registry — also proves the promauto registration site)
//   (d) redirection: the additive JSON key "clean_pairs" must be present on
//       the marshalled Summary (absent on base recon.go → fail) and the
//       behavior assertions fail on base (no CleanPairs field at all).

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

// cleanPairsCounterValue reads booking_recon_clean_pairs_total straight off
// the DEFAULT gatherer (gather-and-parse per SPEC-W78 A2 — no testutil
// dependency, so go.mod stays byte-identical to base). Reaching the counter
// through prometheus.DefaultGatherer also PROVES the promauto registration
// landed on the default registry (the one the existing /metrics promhttp
// handler serves).
func cleanPairsCounterValue(t *testing.T) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("default gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "booking_recon_clean_pairs_total" {
			continue
		}
		if len(mf.GetMetric()) != 1 {
			t.Fatalf("counter must have exactly one metric (no labels): %+v", mf)
		}
		return mf.GetMetric()[0].GetCounter().GetValue()
	}
	t.Fatal("booking_recon_clean_pairs_total not registered on the default registry")
	return 0
}

func TestCleanPairsCountedOnlyForVerifiedEqualPairs(t *testing.T) {
	rig := newReconRig(t)
	ctx := context.Background()
	tenant := uuid.New()
	offering := uuid.New()
	if _, err := rig.raw.Exec(ctx,
		`INSERT INTO offerings (id, tenant_id, name, price_cents, currency) VALUES ($1,$2,'Consult',5000,'NGN')`,
		offering, tenant); err != nil {
		t.Fatalf("offering: %v", err)
	}
	now := time.Now().UTC()
	capturedAt := now.Add(-time.Hour)
	today := now.Format("2006-01-02")

	// (a) CLEAN pair: exact contact-chain match, receipt 50.00 == rail 5000
	// minor → no finding, counts as clean.
	cClean, _ := seedPaidBooking(t, rig, tenant, offering, "+2348077000001", now, now, 5000, "w78-tx-clean")
	seedReceiptCapture(t, rig, tenant, "w78-cap-clean", "checkin",
		fmt.Sprintf(`{"contact_id":%q}`, cClean), "", fmt.Sprintf(`{"amount":50.00,"date":%q}`, today), capturedAt)

	// (a) MISMATCH pair: exact contact-chain match, receipt 45.00 vs rail
	// 5000 → amount_mismatch finding, NOT clean.
	cMis, bMis := seedPaidBooking(t, rig, tenant, offering, "+2348077000002", now, now, 5000, "w78-tx-mismatch")
	seedReceiptCapture(t, rig, tenant, "w78-cap-mismatch", "checkin",
		fmt.Sprintf(`{"contact_id":%q}`, cMis), "", fmt.Sprintf(`{"amount":45.00,"date":%q}`, today), capturedAt)

	// (b) NOT-COMPARABLE HEURISTIC pair: lead-phone chain match with an
	// UNREADABLE (absent) receipt amount → no standalone finding (heuristic
	// gate) and MUST NOT count as clean (amounts never verified).
	_, _ = seedPaidBooking(t, rig, tenant, offering, "+2348077000003", now, now, 6100, "w78-tx-heur")
	lead := uuid.New()
	if _, err := rig.raw.Exec(ctx,
		`INSERT INTO leads (id, tenant_id, phone_e164) VALUES ($1,$2,'+2348077000003')`, lead, tenant); err != nil {
		t.Fatalf("lead: %v", err)
	}
	seedReceiptCapture(t, rig, tenant, "w78-cap-heur-noamount", "lead_capture",
		`{"phone_e164":"+2348077000003"}`, fmt.Sprintf(`{"lead_id":%q}`, lead), `{"vendor":"Stall"}`, capturedAt)

	// (c) counter delta over the run == Summary.CleanPairs.
	before := cleanPairsCounterValue(t)
	sum, err := rig.svc.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	after := cleanPairsCounterValue(t)

	// (a) exactly one clean pair (the verified-equal exact match); exactly
	// one NEW finding (the amount_mismatch). The mismatch + heuristic
	// bookings are receipt-linked (matchedBookings), so no
	// payment_without_receipt fires.
	if sum.TenantsScanned != 1 || sum.TenantsFailed != 0 {
		t.Fatalf("summary tenants: %+v", sum)
	}
	if sum.FindingsNew != 1 || sum.FindingsDuped != 0 {
		t.Fatalf("want findings_new=1 deduped=0, got %+v", sum)
	}
	if sum.CleanPairs != 1 {
		t.Fatalf("(a)+(b): want CleanPairs=1 (mismatch and not-comparable heuristic excluded), got %d (%+v)", sum.CleanPairs, sum)
	}
	if delta := after - before; delta != float64(sum.CleanPairs) {
		t.Fatalf("(c): counter delta %v must equal Summary.CleanPairs %d", delta, sum.CleanPairs)
	}

	// The single finding is the amount_mismatch on the mismatch booking.
	rows, err := rig.st.ListFindings(ctx, tenant, "all", 100)
	if err != nil || len(rows) != 1 {
		t.Fatalf("want 1 finding, got %d (%v)", len(rows), err)
	}
	if rows[0].Kind != KindAmountMismatch || rows[0].BookingID == nil || *rows[0].BookingID != bMis {
		t.Fatalf("finding must be the mismatch pair's amount_mismatch: %+v", rows[0])
	}

	// (d) redirection, key presence: the marshalled Summary MUST carry the
	// additive clean_pairs key. On base recon.go the field does not exist —
	// this assertion (and the entire file) fails there.
	raw, err := json.Marshal(sum)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	v, present := keys["clean_pairs"]
	if !present {
		t.Fatalf("(d): Summary JSON must carry clean_pairs (additive key): %s", raw)
	}
	if v.(float64) != 1 {
		t.Fatalf("(d): clean_pairs=%v want 1: %s", v, raw)
	}

	// Re-sweep: the mismatch finding dedupes, the clean pair is STILL clean
	// (clean pairs are counted per sweep — the dedupe index only covers
	// findings), and the counter tracks the run delta again.
	before2 := cleanPairsCounterValue(t)
	sum2, err := rig.svc.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("re-sweep: %v", err)
	}
	after2 := cleanPairsCounterValue(t)
	if sum2.CleanPairs != 1 || sum2.FindingsNew != 0 || sum2.FindingsDuped != 1 {
		t.Fatalf("re-sweep summary: %+v (want clean=1 new=0 deduped=1)", sum2)
	}
	if delta := after2 - before2; delta != 1 {
		t.Fatalf("(c) re-sweep: counter delta %v want 1", delta)
	}
}

// TestCleanPairsKeyAbsentOnZeroCleanRun locks the omitempty contract (the
// W78 lead's ruling): a sweep that verifies ZERO clean pairs must NOT
// surface the clean_pairs key — the W66 manual-trigger byte-shape pin
// (TestTriggerEmptyBodyCompat, exactly the five pre-W66 keys) depends on
// it. Fixture: one orphan receipt (no payment candidate at any rung) → 1
// finding, 0 clean pairs.
func TestCleanPairsKeyAbsentOnZeroCleanRun(t *testing.T) {
	rig := newReconRig(t)
	ctx := context.Background()
	seedOrphanReceipt(t, rig, uuid.New(), "w78-cap-orphan")

	sum, err := rig.svc.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if sum.CleanPairs != 0 || sum.FindingsNew != 1 {
		t.Fatalf("want clean=0 new=1, got %+v", sum)
	}
	raw, err := json.Marshal(sum)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	if _, present := keys["clean_pairs"]; present {
		t.Fatalf("zero-clean run must NOT surface clean_pairs (W66 byte-shape pin): %s", raw)
	}
}

// TestClassifyCaptureCleanFlagDisambiguates unit-pins the SPEC-W78 A1
// disambiguation WITHOUT a database: findingForCapture's nil return was
// ambiguous between "clean pair" and "no finding for another reason";
// classifyCapture's clean flag is true ONLY for a comparable, equal,
// matched pair (the default branch) and false for a not-comparable
// heuristic pair — even though both yield a nil finding.
func TestClassifyCaptureCleanFlagDisambiguates(t *testing.T) {
	svc := &Service{}
	tenantID, runID := uuid.New(), uuid.New()
	rail := int64(5000)
	cand := &PaymentCandidate{
		BookingID: uuid.New(), TxRef: "w78-tx", AmountMinor: &rail,
		Currency: "NGN", AttemptStatus: "paid",
	}

	// Comparable + equal, exact match → nil finding, clean=true.
	f, clean := svc.classifyCapture(runID, tenantID, w64Capture(),
		LadderOutcome{Confidence: ConfidenceExact, Candidate: cand, WinningRung: 1}, rvWith(50.00, "NGN"))
	if f != nil || !clean {
		t.Fatalf("equal exact pair: f=%v clean=%v, want nil/true", f, clean)
	}

	// Comparable + equal, HEURISTIC match → still clean (heuristic pairs
	// count when amounts verify equal; only not-comparable ones do not).
	f, clean = svc.classifyCapture(runID, tenantID, w64Capture(),
		LadderOutcome{Confidence: ConfidenceHeuristic, Candidate: cand, WinningRung: 2}, rvWith(50.00, "NGN"))
	if f != nil || !clean {
		t.Fatalf("equal heuristic pair: f=%v clean=%v, want nil/true", f, clean)
	}

	// Not-comparable heuristic (null receipt amount) → nil finding,
	// clean=false. THE disambiguation case.
	var nullView receiptView
	f, clean = svc.classifyCapture(runID, tenantID, w64Capture(),
		LadderOutcome{Confidence: ConfidenceHeuristic, Candidate: cand, WinningRung: 2}, nullView)
	if f != nil || clean {
		t.Fatalf("not-comparable heuristic pair: f=%v clean=%v, want nil/false", f, clean)
	}

	// Divergent pair → finding, clean=false.
	f, clean = svc.classifyCapture(runID, tenantID, w64Capture(),
		LadderOutcome{Confidence: ConfidenceExact, Candidate: cand, WinningRung: 1}, rvWith(45.00, "NGN"))
	if f == nil || f.Kind != KindAmountMismatch || clean {
		t.Fatalf("divergent pair: f=%+v clean=%v, want amount_mismatch/false", f, clean)
	}

	// Not-comparable EXACT pair → currency_unknown finding, clean=false.
	f, clean = svc.classifyCapture(runID, tenantID, w64Capture(),
		LadderOutcome{Confidence: ConfidenceExact, Candidate: cand, WinningRung: 1}, nullView)
	if f == nil || f.Kind != KindCurrencyUnknown || clean {
		t.Fatalf("not-comparable exact pair: f=%+v clean=%v, want currency_unknown/false", f, clean)
	}

	// No candidate → finding, clean=false.
	f, clean = svc.classifyCapture(runID, tenantID, w64Capture(),
		LadderOutcome{Confidence: ConfidenceNone}, nullView)
	if f == nil || f.Kind != KindReceiptWithoutPayment || clean {
		t.Fatalf("no candidate: f=%+v clean=%v, want receipt_without_payment/false", f, clean)
	}
}
