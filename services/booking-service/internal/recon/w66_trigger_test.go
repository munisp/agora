package recon

// SPEC-W66 WS1-B: manual-trigger since/lookback_days/tenant_id override tests.
//   - empty body (and {}) = pre-W66 configured-window behavior, byte-shape pinned
//   - since reaches receipts older than the configured trailing window
//   - lookback_days alone; since WINS when both are present
//   - tenant_id scopes the sweep (pinned-4 gate still applies)
//   - a resolved finding is NEVER reopened/duplicated by any re-sweep
//   - re-run idempotency: zero duplicate findings, zero extra outbox rows
//   - 400 validations (bad since/lookback_days/tenant_id, unknown keys,
//     non-object body) sweep NOTHING
//   - timeout: triggerTimeout exceeded => 202 + partial counts (never a
//     silent 504); a follow-up call resumes cleanly
// The store-backed tests ride the embedded-postgres rig from store_test.go
// (dedicated port 5580; -short skips).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// triggerPOST invokes Handlers.Trigger with an optional raw body (empty
// string = NO body at all — the pre-W66 call shape).
func triggerPOST(h *Handlers, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodPost, "/v1/internal/recon/receipts", nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, "/v1/internal/recon/receipts", strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	h.Trigger(rec, req)
	return rec
}

// triggerSummary decodes a trigger response body into the generic map (for
// key-shape pins) plus the typed Summary (for counts).
func triggerSummary(t *testing.T, rec *httptest.ResponseRecorder) (map[string]any, Summary) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("response json: %v (%s)", err, rec.Body.String())
	}
	var sum Summary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatalf("summary json: %v (%s)", err, rec.Body.String())
	}
	return raw, sum
}

// seedReceiptCaptureAt seeds a receipt-bearing capture with BOTH timestamps
// pinned to ts (seedReceiptCapture always stamps server_received_at=now(),
// which can never fall outside the trailing window).
func seedReceiptCaptureAt(t *testing.T, rig reconRig, tenantID uuid.UUID, clientID, payload, receiptFields string, ts time.Time) {
	t.Helper()
	ctx := context.Background()
	var fields any
	if receiptFields != "" {
		fields = json.RawMessage(receiptFields)
	}
	if _, err := rig.raw.Exec(ctx,
		`INSERT INTO field_captures (tenant_id, client_id, kind, payload, receipt_document_id, receipt_fields, captured_at, server_received_at)
		 VALUES ($1,$2,'checkin',$3,$4,$5,$6,$6)`,
		tenantID, clientID, json.RawMessage(payload), uuid.New(), fields, ts); err != nil {
		t.Fatalf("capture %s: %v", clientID, err)
	}
}

// seedOrphanReceipt seeds a receipt capture with a contact_id that has no
// bookings => exactly one receipt_without_payment finding per sweep.
func seedOrphanReceipt(t *testing.T, rig reconRig, tenantID uuid.UUID, clientID string) {
	t.Helper()
	seedReceiptCapture(t, rig, tenantID, clientID, "checkin",
		`{"contact_id":"`+uuid.NewString()+`"}`, "", `{"amount":10.00}`, time.Now().UTC())
}

func countRows(t *testing.T, rig reconRig, query string, args ...any) int {
	t.Helper()
	var n int
	if err := rig.raw.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestTriggerEmptyBodyCompat pins byte-identical pre-W66 behavior: no body
// and `{}` both run the configured trailing window and answer 200 with the
// bare Summary shape (exactly the five pre-W66 keys — no "partial").
func TestTriggerEmptyBodyCompat(t *testing.T) {
	rig := newReconRig(t)
	tenant := uuid.New()
	seedOrphanReceipt(t, rig, tenant, "cap-b")
	h := &Handlers{Svc: rig.svc, Log: zap.NewNop()}

	rec := triggerPOST(h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty body: got %d (%s)", rec.Code, rec.Body.String())
	}
	raw, sum := triggerSummary(t, rec)
	wantKeys := map[string]bool{"run_id": true, "tenants_scanned": true, "tenants_failed": true, "findings_new": true, "findings_deduped": true}
	for k := range raw {
		if !wantKeys[k] {
			t.Fatalf("empty-body response gained key %q — shape must stay byte-identical: %s", k, rec.Body.String())
		}
	}
	if len(raw) != len(wantKeys) {
		t.Fatalf("empty-body response keys: %v", raw)
	}
	if sum.TenantsScanned != 1 || sum.TenantsFailed != 0 || sum.FindingsNew != 1 || sum.FindingsDuped != 0 {
		t.Fatalf("empty-body summary: %+v", sum)
	}

	// `{}` is the same configured-window call; the finding dedupes.
	rec = triggerPOST(h, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty object: got %d (%s)", rec.Code, rec.Body.String())
	}
	raw, sum = triggerSummary(t, rec)
	if _, hasPartial := raw["partial"]; hasPartial {
		t.Fatalf("completed sweep must NOT carry partial: %s", rec.Body.String())
	}
	if sum.FindingsNew != 0 || sum.FindingsDuped != 1 {
		t.Fatalf("re-trigger must dedupe: %+v", sum)
	}
}

// TestTriggerSinceOverrideReachesOlderReceipts proves the W66 gap is closed:
// a capture 60d old is invisible to the configured 30d window and appears
// once since reaches back far enough; a same-window re-sweep dedupes.
func TestTriggerSinceOverrideReachesOlderReceipts(t *testing.T) {
	rig := newReconRig(t)
	ctx := context.Background()
	tenant := uuid.New()
	old := time.Now().UTC().AddDate(0, 0, -60)
	seedReceiptCaptureAt(t, rig, tenant, "cap-old",
		`{"contact_id":"`+uuid.NewString()+`"}`, `{"amount":25.00}`, old)
	h := &Handlers{Svc: rig.svc, Log: zap.NewNop()}

	// Default window (30d): the 60d-old receipt is invisible — the pre-W66 gap.
	rec := triggerPOST(h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("default sweep: %d (%s)", rec.Code, rec.Body.String())
	}
	_, sum := triggerSummary(t, rec)
	if sum.TenantsScanned != 0 || sum.FindingsNew != 0 {
		t.Fatalf("60d-old capture must be outside the default window: %+v", sum)
	}

	// since override reaches it.
	since := time.Now().UTC().AddDate(0, 0, -90).Format(time.RFC3339)
	rec = triggerPOST(h, `{"since":"`+since+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("since sweep: %d (%s)", rec.Code, rec.Body.String())
	}
	_, sum = triggerSummary(t, rec)
	if sum.TenantsScanned != 1 || sum.FindingsNew != 1 {
		t.Fatalf("since override must reach the old receipt: %+v", sum)
	}
	rows, err := rig.st.ListFindings(ctx, tenant, "open", 100)
	if err != nil || len(rows) != 1 || rows[0].Kind != KindReceiptWithoutPayment {
		t.Fatalf("findings: %d %v", len(rows), err)
	}

	// Same-window re-sweep: zero duplicates, zero extra outbox rows.
	rec = triggerPOST(h, `{"since":"`+since+`"}`)
	_, sum = triggerSummary(t, rec)
	if sum.FindingsNew != 0 || sum.FindingsDuped != 1 {
		t.Fatalf("backfill re-sweep must dedupe: %+v", sum)
	}
	if n := countRows(t, rig, `SELECT count(*) FROM receipt_recon_findings`); n != 1 {
		t.Fatalf("re-sweep duplicated findings: %d", n)
	}
	if n := countRows(t, rig, `SELECT count(*) FROM outbox`); n != 1 {
		t.Fatalf("re-sweep emitted duplicate events: %d", n)
	}
}

// TestTriggerLookbackDaysOverride: lookback_days alone widens the window.
func TestTriggerLookbackDaysOverride(t *testing.T) {
	rig := newReconRig(t)
	tenant := uuid.New()
	old := time.Now().UTC().AddDate(0, 0, -60)
	seedReceiptCaptureAt(t, rig, tenant, "cap-lb",
		`{"contact_id":"`+uuid.NewString()+`"}`, `{"amount":25.00}`, old)
	h := &Handlers{Svc: rig.svc, Log: zap.NewNop()}

	// 30d lookback: still invisible (rig default LookbackDays is also 30).
	rec := triggerPOST(h, `{"lookback_days":30}`)
	_, sum := triggerSummary(t, rec)
	if rec.Code != http.StatusOK || sum.TenantsScanned != 0 || sum.FindingsNew != 0 {
		t.Fatalf("30d lookback must miss the 60d-old capture: %d %+v", rec.Code, sum)
	}
	// 90d lookback: found.
	rec = triggerPOST(h, `{"lookback_days":90}`)
	_, sum = triggerSummary(t, rec)
	if rec.Code != http.StatusOK || sum.TenantsScanned != 1 || sum.FindingsNew != 1 {
		t.Fatalf("90d lookback must reach the capture: %d %+v (%s)", rec.Code, sum, rec.Body.String())
	}
}

// TestTriggerSinceWinsOverLookback pins the rule: when BOTH since and
// lookback_days are present, SINCE wins.
func TestTriggerSinceWinsOverLookback(t *testing.T) {
	rig := newReconRig(t)
	tenant := uuid.New()
	tenDaysAgo := time.Now().UTC().AddDate(0, 0, -10)
	seedReceiptCaptureAt(t, rig, tenant, "cap-win",
		`{"contact_id":"`+uuid.NewString()+`"}`, `{"amount":25.00}`, tenDaysAgo)
	h := &Handlers{Svc: rig.svc, Log: zap.NewNop()}

	// lookback_days:5 alone misses the 10d-old capture (proves the knob moves
	// the window at all).
	rec := triggerPOST(h, `{"lookback_days":5}`)
	_, sum := triggerSummary(t, rec)
	if rec.Code != http.StatusOK || sum.TenantsScanned != 0 || sum.FindingsNew != 0 {
		t.Fatalf("5d lookback must miss the 10d-old capture: %d %+v", rec.Code, sum)
	}
	// since (20d) + lookback_days:5 => since WINS => the capture is found.
	since := time.Now().UTC().AddDate(0, 0, -20).Format(time.RFC3339)
	rec = triggerPOST(h, `{"since":"`+since+`","lookback_days":5}`)
	_, sum = triggerSummary(t, rec)
	if rec.Code != http.StatusOK || sum.FindingsNew != 1 {
		t.Fatalf("since must win over lookback_days: %d %+v (%s)", rec.Code, sum, rec.Body.String())
	}
}

// TestTriggerTenantFilter: tenant_id scopes the sweep to one tenant; the
// pinned-4 gate still applies to an explicitly-named tenant with no receipt
// captures in the window.
func TestTriggerTenantFilter(t *testing.T) {
	rig := newReconRig(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	seedOrphanReceipt(t, rig, tenantA, "cap-a")
	seedOrphanReceipt(t, rig, tenantB, "cap-b")
	h := &Handlers{Svc: rig.svc, Log: zap.NewNop()}

	rec := triggerPOST(h, `{"tenant_id":"`+tenantA.String()+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant sweep: %d (%s)", rec.Code, rec.Body.String())
	}
	_, sum := triggerSummary(t, rec)
	if sum.TenantsScanned != 1 || sum.FindingsNew != 1 {
		t.Fatalf("tenant_id must scope the sweep: %+v", sum)
	}
	rowsA, err := rig.st.ListFindings(ctx, tenantA, "all", 100)
	if err != nil || len(rowsA) != 1 {
		t.Fatalf("tenantA findings: %d %v", len(rowsA), err)
	}
	rowsB, err := rig.st.ListFindings(ctx, tenantB, "all", 100)
	if err != nil || len(rowsB) != 0 {
		t.Fatalf("tenantB must be untouched: %d %v", len(rowsB), err)
	}

	// Unknown tenant_id: nothing swept, no error (the gate finds no captures).
	rec = triggerPOST(h, `{"tenant_id":"`+uuid.NewString()+`"}`)
	_, sum = triggerSummary(t, rec)
	if rec.Code != http.StatusOK || sum.TenantsScanned != 0 || sum.FindingsNew != 0 {
		t.Fatalf("unknown tenant must sweep nothing: %d %+v", rec.Code, sum)
	}

	// Full re-sweep: both tenants scanned, A dedupes, B is new.
	rec = triggerPOST(h, "")
	_, sum = triggerSummary(t, rec)
	if rec.Code != http.StatusOK || sum.TenantsScanned != 2 || sum.FindingsNew != 1 || sum.FindingsDuped != 1 {
		t.Fatalf("full re-sweep: %d %+v", rec.Code, sum)
	}
}

// TestTriggerResolvedFindingNeverReopened: a resolved finding can never be
// reopened or duplicated by any re-sweep/backfill — the dedupe key ignores
// resolved_at and ResolveFinding guards on resolved_at IS NULL.
func TestTriggerResolvedFindingNeverReopened(t *testing.T) {
	rig := newReconRig(t)
	ctx := context.Background()
	tenant := uuid.New()
	seedOrphanReceipt(t, rig, tenant, "cap-res")
	h := &Handlers{Svc: rig.svc, Log: zap.NewNop()}

	rec := triggerPOST(h, "")
	_, sum := triggerSummary(t, rec)
	if rec.Code != http.StatusOK || sum.FindingsNew != 1 {
		t.Fatalf("initial sweep: %d %+v", rec.Code, sum)
	}
	open, err := rig.st.ListFindings(ctx, tenant, "open", 100)
	if err != nil || len(open) != 1 {
		t.Fatalf("open: %d %v", len(open), err)
	}
	if _, err := rig.svc.ResolveFinding(ctx, tenant, open[0].ID, "ops-1"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Deep backfill re-sweep after resolution: the finding dedupes and stays
	// resolved — no re-insert, no reopen, no new outbox row.
	rec = triggerPOST(h, `{"lookback_days":90}`)
	_, sum = triggerSummary(t, rec)
	if rec.Code != http.StatusOK || sum.FindingsNew != 0 || sum.FindingsDuped != 1 {
		t.Fatalf("post-resolve re-sweep must dedupe: %d %+v", rec.Code, sum)
	}
	open, err = rig.st.ListFindings(ctx, tenant, "open", 100)
	if err != nil || len(open) != 0 {
		t.Fatalf("resolved finding must NOT reopen: %d %v", len(open), err)
	}
	resolved, err := rig.st.ListFindings(ctx, tenant, "resolved", 100)
	if err != nil || len(resolved) != 1 {
		t.Fatalf("resolved: %d %v", len(resolved), err)
	}
	if n := countRows(t, rig, `SELECT count(*) FROM receipt_recon_findings`); n != 1 {
		t.Fatalf("exactly one finding row must exist: %d", n)
	}
	// Outbox: 1 finding event + 1 resolved event; the re-sweep added nothing.
	if n := countRows(t, rig, `SELECT count(*) FROM outbox`); n != 2 {
		t.Fatalf("outbox must hold finding+resolved only: %d", n)
	}
}

// TestTriggerValidation400: every malformed input answers 400 with a clear
// error and sweeps NOTHING (no findings, no outbox rows).
func TestTriggerValidation400(t *testing.T) {
	rig := newReconRig(t)
	tenant := uuid.New()
	seedOrphanReceipt(t, rig, tenant, "cap-400")
	h := &Handlers{Svc: rig.svc, Log: zap.NewNop()}

	bad := []struct{ name, body string }{
		{"since not RFC3339", `{"since":"yesterday"}`},
		{"since date only", `{"since":"2024-01-01"}`},
		{"since impossible date", `{"since":"2024-02-30T10:00:00Z"}`},
		{"lookback zero", `{"lookback_days":0}`},
		{"lookback negative", `{"lookback_days":-3}`},
		{"lookback fractional", `{"lookback_days":1.5}`},
		{"lookback string", `{"lookback_days":"90"}`},
		{"tenant not uuid", `{"tenant_id":"nope"}`},
		{"tenant numeric", `{"tenant_id":123}`},
		{"unknown key", `{"bogus":1}`},
		{"non-object body", `[1,2,3]`},
		{"malformed json", `{"since":`},
	}
	for _, tc := range bad {
		rec := triggerPOST(h, tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d (%s)", tc.name, rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: error body json: %v", tc.name, err)
		}
		if msg, _ := body["error"].(string); msg == "" {
			t.Fatalf("%s: 400 must carry a clear error message: %s", tc.name, rec.Body.String())
		}
	}
	if n := countRows(t, rig, `SELECT count(*) FROM receipt_recon_findings`); n != 0 {
		t.Fatalf("bad input must sweep nothing: %d findings", n)
	}
	if n := countRows(t, rig, `SELECT count(*) FROM outbox`); n != 0 {
		t.Fatalf("bad input must emit nothing: %d outbox rows", n)
	}
}

// TestTriggerTimeoutPartial pins the deep-window timeout behavior: when the
// sweep exceeds triggerTimeout the trigger answers 202 Accepted with the
// partial summary (partial:true) — never a silent 504. A follow-up call
// resumes cleanly (dedupe is the checkpoint). The negative Timeout override
// yields an already-expired sweep context (deterministic).
func TestTriggerTimeoutPartial(t *testing.T) {
	rig := newReconRig(t)
	tenant := uuid.New()
	seedOrphanReceipt(t, rig, tenant, "cap-to")

	expiring := &Handlers{Svc: rig.svc, Log: zap.NewNop(), Timeout: -time.Second}
	rec := triggerPOST(expiring, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("timeout must answer 202, got %d (%s)", rec.Code, rec.Body.String())
	}
	raw, sum := triggerSummary(t, rec)
	if p, _ := raw["partial"].(bool); !p {
		t.Fatalf("202 body must carry partial:true: %s", rec.Body.String())
	}
	if sum.TenantsScanned != 0 || sum.FindingsNew != 0 {
		t.Fatalf("instantly-expired sweep must report zero progress: %+v", sum)
	}

	// The resume story: a normal call after the timeout completes the work.
	h := &Handlers{Svc: rig.svc, Log: zap.NewNop()}
	rec = triggerPOST(h, "")
	_, sum = triggerSummary(t, rec)
	if rec.Code != http.StatusOK || sum.FindingsNew != 1 {
		t.Fatalf("follow-up sweep must complete: %d %+v", rec.Code, sum)
	}
}

// TestSweepOnceSinceExpiredContext pins the service half of the timeout
// contract: an expired ctx returns the partial Summary (zero progress when
// expiry precedes enumeration) together with context.DeadlineExceeded.
func TestSweepOnceSinceExpiredContext(t *testing.T) {
	rig := newReconRig(t)
	seedOrphanReceipt(t, rig, uuid.New(), "cap-ctx")
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	sum, err := rig.svc.SweepOnceSince(ctx, time.Time{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired ctx must surface DeadlineExceeded, got %v", err)
	}
	if sum.TenantsScanned != 0 || sum.FindingsNew != 0 || sum.FindingsDuped != 0 {
		t.Fatalf("partial summary must be zero: %+v", sum)
	}
	if sum.RunID == uuid.Nil {
		t.Fatal("partial summary must still carry the run id")
	}
}

// TestTriggerServiceUnavailableNilSvc: the nil-service guard predates W66 and
// must survive the body-parsing addition (bad bodies on a nil service still
// answer 503, not 400).
func TestTriggerServiceUnavailableNilSvc(t *testing.T) {
	h := &Handlers{}
	for _, body := range []string{"", `{"since":"yesterday"}`} {
		rec := triggerPOST(h, body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("nil service (body %q): want 503, got %d", body, rec.Code)
		}
	}
}
