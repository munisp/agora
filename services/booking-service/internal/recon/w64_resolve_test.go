package recon

// SPEC-W64: resolve path + event enrichment tests.
//   - run_id in the findingEventPayload data (unit, no DB)
//   - amount_comparable / amounts_equal detail booleans (unit, no DB)
//   - Store.ResolveFinding: UPDATE resolved_at/resolved_by WHERE tenant+id
//     AND resolved_at IS NULL RETURNING + the ReceiptReconResolved outbox
//     row in the SAME tx; replay/unknown/cross-tenant →
//     ErrFindingNotFoundOrResolved with NO outbox row
//   - Handlers.Resolve: 200 / 404 (unknown + already-resolved) / 400
// The store-backed tests ride the embedded-postgres rig from
// store_test.go (dedicated port 5580; -short skips).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// findingEventPayload carries run_id (SPEC-W64 item 1) — mapping.py already
// consumes it; before W64 the key was absent and the lake saw NULL.
func TestFindingEventPayloadRunID(t *testing.T) {
	svc := &Service{EventsTopic: "opendesk.booking.events"}
	runID := uuid.New()
	f := &Finding{
		ID: uuid.New(), TenantID: uuid.New(), RunID: runID,
		Kind: KindAmountMismatch, Confidence: ConfidenceExact,
		Detail: json.RawMessage(`{"currency_unknown":false}`),
	}
	payload, err := svc.findingEventPayload(context.Background(), f)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	var env struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.Type != events.ReceiptReconFindingEventType {
		t.Fatalf("type: %q", env.Type)
	}
	if env.Data["run_id"] != runID.String() {
		t.Fatalf("run_id: %v want %s", env.Data["run_id"], runID)
	}
}

// amountBooleans extracts the SPEC-W64 field-equality pair from a finding
// detail (ok=false when the key is absent — the booleans are pinned to the
// amount-verification branch only).
func amountBooleans(t *testing.T, f *Finding) (comparable, equal, ok bool) {
	t.Helper()
	if f == nil {
		t.Fatal("nil finding")
	}
	var detail map[string]any
	if err := json.Unmarshal(f.Detail, &detail); err != nil {
		t.Fatalf("detail json: %v", err)
	}
	c, cok := detail["amount_comparable"].(bool)
	e, eok := detail["amounts_equal"].(bool)
	return c, e, cok && eok
}

func w64Capture() ReceiptCapture {
	return ReceiptCapture{ClientID: "cap-w64", Kind: "checkin"}
}

// TestFindingAmountBooleans pins SPEC-W64 item 5: every finding produced by
// the amount-verification branch carries amount_comparable (post currency
// fold) and amounts_equal (= comparable && !divergent); the no-candidate
// branch carries NEITHER (no comparison ever ran).
func TestFindingAmountBooleans(t *testing.T) {
	svc := &Service{}
	tenantID, runID := uuid.New(), uuid.New()
	rail := int64(5000)
	candidate := func(currency string) *PaymentCandidate {
		return &PaymentCandidate{
			BookingID: uuid.New(), TxRef: "tx-w64", AmountMinor: &rail,
			Currency: currency, AttemptStatus: "paid",
		}
	}
	exact := func(c *PaymentCandidate) LadderOutcome {
		return LadderOutcome{Confidence: ConfidenceExact, Candidate: c, WinningRung: 1}
	}

	// Divergent pair: comparable, NOT equal → amount_mismatch.
	f := svc.findingForCapture(runID, tenantID, w64Capture(), exact(candidate("NGN")), rvWith(45.00, "NGN"))
	if f == nil || f.Kind != KindAmountMismatch {
		t.Fatalf("divergent pair must find amount_mismatch: %+v", f)
	}
	if c, e, ok := amountBooleans(t, f); !ok || !c || e {
		t.Fatalf("divergent: comparable=%v equal=%v ok=%v", c, e, ok)
	}

	// Equal pair: clean — NO finding at all (the documented W64 gap: clean
	// pairs stay unobservable; the booleans only ride on findings).
	if f := svc.findingForCapture(runID, tenantID, w64Capture(), exact(candidate("NGN")), rvWith(50.00, "NGN")); f != nil {
		t.Fatalf("equal pair must be clean (no finding): %+v", f)
	}

	// Unreadable receipt amount on an exact pair: not comparable, not equal
	// → currency_unknown (standalone, exact-only gate).
	var nullView receiptView
	f = svc.findingForCapture(runID, tenantID, w64Capture(), exact(candidate("NGN")), nullView)
	if f == nil || f.Kind != KindCurrencyUnknown {
		t.Fatalf("null-amount exact pair must find currency_unknown: %+v", f)
	}
	if c, e, ok := amountBooleans(t, f); !ok || c || e {
		t.Fatalf("not comparable: comparable=%v equal=%v ok=%v", c, e, ok)
	}

	// Currency mismatch (receipt USD vs rail NGN): the P7 fold forces
	// comparable=false BEFORE the booleans are stamped.
	f = svc.findingForCapture(runID, tenantID, w64Capture(), exact(candidate("NGN")), rvWith(50.00, "USD"))
	if f == nil || f.Kind != KindCurrencyUnknown {
		t.Fatalf("currency-mismatch exact pair must find currency_unknown: %+v", f)
	}
	if c, e, ok := amountBooleans(t, f); !ok || c || e {
		t.Fatalf("currency mismatch: comparable=%v equal=%v ok=%v", c, e, ok)
	}

	// No candidate at any rung: no comparison ran → the booleans are ABSENT.
	f = svc.findingForCapture(runID, tenantID, w64Capture(), LadderOutcome{Confidence: ConfidenceNone}, nullView)
	if f == nil || f.Kind != KindReceiptWithoutPayment {
		t.Fatalf("no candidate must find receipt_without_payment: %+v", f)
	}
	var detail map[string]any
	if err := json.Unmarshal(f.Detail, &detail); err != nil {
		t.Fatalf("detail json: %v", err)
	}
	if _, present := detail["amount_comparable"]; present {
		t.Fatalf("no-candidate finding must NOT carry amount booleans: %+v", detail)
	}
	if _, present := detail["amounts_equal"]; present {
		t.Fatalf("no-candidate finding must NOT carry amount booleans: %+v", detail)
	}
}

// jsonAmount formats a major amount for the test receipt JSON.
func jsonAmount(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// rvWith builds the parsed receipt view for an amount+currency pair.
func rvWith(amount float64, currency string) receiptView {
	return parseReceiptFields(json.RawMessage(
		`{"amount":` + jsonAmount(amount) + `,"currency":"` + currency + `"}`))
}

// insertW64Finding seeds one finding (via the production insert path so the
// finding-outbox row lands too) and returns it.
func insertW64Finding(t *testing.T, rig reconRig, tenantID, runID uuid.UUID) *Finding {
	t.Helper()
	ctx := context.Background()
	f := &Finding{
		TenantID: tenantID, RunID: runID, Kind: KindReceiptWithoutPayment,
		Confidence: ConfidenceNone,
		Detail:     json.RawMessage(`{"currency_unknown":true}`),
	}
	payload, err := rig.svc.findingEventPayload(ctx, f)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	inserted, err := rig.st.InsertFindingWithOutbox(ctx, f, rig.svc.EventsTopic, payload)
	if err != nil || !inserted {
		t.Fatalf("seed insert: inserted=%v err=%v", inserted, err)
	}
	return f
}

// TestResolveFindingOutbox covers the store+service resolve path: the row
// is stamped resolved_at/resolved_by, the ReceiptReconResolved outbox event
// carries exactly the pinned payload, the list status filter flips, and a
// replayed / unknown / cross-tenant resolve is
// ErrFindingNotFoundOrResolved with NO further outbox row.
func TestResolveFindingOutbox(t *testing.T) {
	rig := newReconRig(t)
	ctx := context.Background()
	tenantID, runID := uuid.New(), uuid.New()
	f := insertW64Finding(t, rig, tenantID, runID)

	resolved, err := rig.svc.ResolveFinding(ctx, tenantID, f.ID, "user-77")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ID != f.ID || resolved.RunID != runID {
		t.Fatalf("resolved row identity: %+v", resolved)
	}
	if resolved.ResolvedAt == nil {
		t.Fatalf("resolved_at must be stamped: %+v", resolved)
	}
	if resolved.ResolvedBy == nil || *resolved.ResolvedBy != "user-77" {
		t.Fatalf("resolved_by: %+v", resolved.ResolvedBy)
	}

	// The DB row carries the same stamps (read back through RLS-scoped list).
	resolvedList, err := rig.st.ListFindings(ctx, tenantID, "resolved", 100)
	if err != nil || len(resolvedList) != 1 {
		t.Fatalf("resolved list: %d %v", len(resolvedList), err)
	}
	open, err := rig.st.ListFindings(ctx, tenantID, "open", 100)
	if err != nil || len(open) != 0 {
		t.Fatalf("open list must be empty after resolve: %d %v", len(open), err)
	}

	// Outbox: row 1 = the finding event; row 2 = ReceiptReconResolved with
	// the pinned payload {finding_id, tenant_id, run_id, kind, resolved_at,
	// resolved_by}.
	var count int
	if err := rig.raw.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&count); err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	if count != 2 {
		t.Fatalf("want 2 outbox rows (finding + resolved), got %d", count)
	}
	// Outbox ids are random UUIDs (no insertion order) — select the resolved
	// event by its type, never by ORDER BY id.
	var rawPayload []byte
	if err := rig.raw.QueryRow(ctx,
		`SELECT payload FROM outbox WHERE topic='opendesk.booking.events'
		   AND payload->>'type'=$1`, events.ReceiptReconResolvedEventType).Scan(&rawPayload); err != nil {
		t.Fatalf("read resolved outbox: %v", err)
	}
	var env struct {
		Type     string         `json:"type"`
		TenantID string         `json:"tenantid"`
		Data     map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rawPayload, &env); err != nil {
		t.Fatalf("resolved envelope: %v", err)
	}
	if env.Type != events.ReceiptReconResolvedEventType || env.TenantID != tenantID.String() {
		t.Fatalf("resolved envelope type/tenant: %+v", env)
	}
	if env.Data["finding_id"] != f.ID.String() ||
		env.Data["tenant_id"] != tenantID.String() ||
		env.Data["run_id"] != runID.String() ||
		env.Data["kind"] != KindReceiptWithoutPayment ||
		env.Data["resolved_by"] != "user-77" {
		t.Fatalf("resolved payload fields: %+v", env.Data)
	}
	if ra, _ := env.Data["resolved_at"].(string); ra == "" {
		t.Fatalf("resolved_at must be present: %+v", env.Data)
	}

	// Idempotent replay: the second resolve resolves NOTHING (RETURNING no
	// row) and emits NO second event.
	if _, err := rig.svc.ResolveFinding(ctx, tenantID, f.ID, "user-77"); !errors.Is(err, ErrFindingNotFoundOrResolved) {
		t.Fatalf("replay must be ErrFindingNotFoundOrResolved, got %v", err)
	}
	// Unknown id: same sentinel.
	if _, err := rig.svc.ResolveFinding(ctx, tenantID, uuid.New(), "user-77"); !errors.Is(err, ErrFindingNotFoundOrResolved) {
		t.Fatalf("unknown id must be ErrFindingNotFoundOrResolved, got %v", err)
	}
	// Cross-tenant: another tenant cannot resolve (or even observe) the
	// finding — same sentinel, row untouched.
	if _, err := rig.svc.ResolveFinding(ctx, uuid.New(), f.ID, "intruder"); !errors.Is(err, ErrFindingNotFoundOrResolved) {
		t.Fatalf("cross-tenant resolve must be ErrFindingNotFoundOrResolved, got %v", err)
	}
	if err := rig.raw.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&count); err != nil {
		t.Fatalf("outbox recount: %v", err)
	}
	if count != 2 {
		t.Fatalf("replay/unknown/cross-tenant must emit nothing: outbox=%d", count)
	}
}

// TestResolveFindingEmptyTopicNoOutbox pins the house "empty topic disables
// emission" idiom on the resolve path: the row still resolves, no outbox
// event is written.
func TestResolveFindingEmptyTopicNoOutbox(t *testing.T) {
	rig := newReconRig(t)
	rig.svc.EventsTopic = "" // empty topic disables emission (house idiom)
	ctx := context.Background()
	tenantID := uuid.New()
	// The seed insert rides the same suppressed topic, so NO finding event
	// is written either — the outbox must stay empty end-to-end.
	f := insertW64Finding(t, rig, tenantID, uuid.New())
	resolved, err := rig.svc.ResolveFinding(ctx, tenantID, f.ID, "user-88")
	if err != nil {
		t.Fatalf("resolve (empty topic): %v", err)
	}
	if resolved.ResolvedAt == nil || resolved.ResolvedBy == nil || *resolved.ResolvedBy != "user-88" {
		t.Fatalf("resolved stamps: %+v", resolved)
	}
	var count int
	if err := rig.raw.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&count); err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	if count != 0 {
		t.Fatalf("empty topic must suppress ALL emission, got %d outbox rows", count)
	}
}

// w64ResolveRequest invokes Handlers.Resolve with the chi route context the
// router would have installed ({id} path param).
func w64ResolveRequest(id string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/recon/receipts/"+id+"/resolve", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// TestResolveHandlerHTTP covers the HTTP surface: 200 with the resolved
// finding (resolved_by = the server-passed actor), 404 on replay and on an
// unknown id, 400 on a malformed id.
func TestResolveHandlerHTTP(t *testing.T) {
	rig := newReconRig(t)
	tenantID := uuid.New()
	f := insertW64Finding(t, rig, tenantID, uuid.New())
	h := &Handlers{Svc: rig.svc, Log: zap.NewNop()}
	tenant := bookingops.TenantInfo{ID: tenantID, Slug: "acme-w64"}

	// 200: resolve stamps resolved_by from the actor argument (server-side).
	rec := httptest.NewRecorder()
	h.Resolve(rec, w64ResolveRequest(f.ID.String()), tenant, "user-9")
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve 200 expected, got %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Finding Finding `json:"finding"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response: %v", err)
	}
	if body.Finding.ID != f.ID || body.Finding.ResolvedAt == nil ||
		body.Finding.ResolvedBy == nil || *body.Finding.ResolvedBy != "user-9" {
		t.Fatalf("resolved finding payload: %+v", body.Finding)
	}

	// 404: replay (already resolved) and unknown id are indistinguishable.
	for _, id := range []string{f.ID.String(), uuid.NewString()} {
		rec = httptest.NewRecorder()
		h.Resolve(rec, w64ResolveRequest(id), tenant, "user-9")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("id %s: replay/unknown must 404, got %d", id, rec.Code)
		}
	}

	// 400: malformed id.
	rec = httptest.NewRecorder()
	h.Resolve(rec, w64ResolveRequest("not-a-uuid"), tenant, "user-9")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed id must 400, got %d", rec.Code)
	}
}
