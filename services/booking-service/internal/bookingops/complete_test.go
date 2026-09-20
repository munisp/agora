package bookingops

// SPEC-W45 K11/K12 tests: booking completion (status validation,
// BookingCompleted event, deposit capture via the payments rail, loyalty
// hook, idempotent replay) and the refund rail (fail-closed posture).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// recordingAccruer is the BookingCompletedAccruer test double.
type recordingAccruer struct{ calls atomic.Int32 }

func (r *recordingAccruer) AccrueOnBookingCompleted(_, _ uuid.UUID) { r.calls.Add(1) }

// paymentsStub records the rail calls and answers like payments-service.
type paymentsStub struct {
	srv *httptest.Server

	captureCalls atomic.Int32
	refundCalls  atomic.Int32

	lastCapturePath   atomic.Value
	lastCaptureTenant atomic.Value
	lastUserID        atomic.Value
	lastRoles         atomic.Value
	lastToken         atomic.Value
	lastRefund        atomic.Value
}

func newPaymentsStub(t *testing.T) *paymentsStub {
	t.Helper()
	p := &paymentsStub{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.lastUserID.Store(r.Header.Get("X-User-Id"))
		p.lastRoles.Store(r.Header.Get("X-User-Roles"))
		p.lastToken.Store(r.Header.Get("X-Internal-Token"))
		switch {
		case r.Method == http.MethodPost && len(r.URL.Path) > len("/v1/deposits/") &&
			r.URL.Path[:len("/v1/deposits/")] == "/v1/deposits/" && r.URL.Path[len(r.URL.Path)-8:] == "/capture":
			p.captureCalls.Add(1)
			p.lastCapturePath.Store(r.URL.Path)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			p.lastCaptureTenant.Store(body["tenant_id"])
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"deposit_id": r.URL.Path[len("/v1/deposits/") : len(r.URL.Path)-8], "result": map[string]any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/refunds":
			p.refundCalls.Add(1)
			raw, _ := io.ReadAll(r.Body)
			p.lastRefund.Store(string(raw))
			w.WriteHeader(http.StatusCreated)
			// Full K12 RefundResponse shape: the honest rail outcome must
			// survive the trip to the caller (V2-advisory — never masked
			// as "posted").
			id := uuid.NewString()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": id, "refund_id": id, "amount": 5000, "amount_cents": 5000,
				"status": "queued_manual", "rail": "none",
				"rail_detail": "no provider transaction id on deposit; manual refund required",
			})
		default:
			http.Error(w, "no such payments route: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// completeFixture seeds a confirmed booking and returns the pieces.
func completeFixture(t *testing.T, status string) (*store.Store, context.Context, uuid.UUID, uuid.UUID) {
	t.Helper()
	st, ctx := newSweeperFixture(t)
	tenantID := uuid.New()
	offering := store.Offering{TenantID: tenantID, Name: "Cut", DurationMin: 30, PriceCents: 5000, Currency: "NGN"}
	if err := st.CreateOffering(ctx, &offering); err != nil {
		t.Fatal(err)
	}
	contact := store.Contact{TenantID: tenantID, Name: "Ada", Phone: "+234800"}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}
	b := addSweeperBooking(t, ctx, st, tenantID, offering, contact, status, 5)
	return st, ctx, tenantID, b.ID
}

func outboxTypes(t *testing.T, st *store.Store, topic string) []string {
	t.Helper()
	rows, err := st.FetchUnsentOutbox(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, r := range rows {
		if r.Topic != topic {
			continue
		}
		var ce map[string]any
		if err := json.Unmarshal(r.Payload, &ce); err != nil {
			t.Fatalf("outbox payload: %v", err)
		}
		types = append(types, ce["type"].(string))
	}
	return types
}

// K11: confirmed → completed, BookingCompleted event, deposit captured via
// the rail with the caller identity forwarded, loyalty hook fired.
func TestCompleteCapturesDepositEmitsEventFiresHook(t *testing.T) {
	st, ctx, tenantID, bookingID := completeFixture(t, store.StatusConfirmed)
	rail := newPaymentsStub(t)
	hook := &recordingAccruer{}
	svc := &Service{
		Store:       st,
		EventsTopic: "booking.events",
		Logger:      zap.NewNop(),
		Payments:    NewPaymentsClient(rail.srv.URL, "tok-123", zap.NewNop()),
		Loyalty:     hook,
	}
	depositID := uuid.New()
	res, err := svc.Complete(ctx, tenantID, "acme-ng", bookingID, &depositID,
		CallerIdentity{UserID: "user-9", Roles: []string{"admin", "finance"}})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if res.AlreadyCompleted || res.Booking.Status != store.StatusCompleted {
		t.Fatalf("result: %+v", res)
	}
	if res.Capture == nil || !res.Capture.Captured || res.Capture.DepositID != depositID.String() {
		t.Fatalf("capture result: %+v", res.Capture)
	}
	// The rail call: path, tenant body, caller identity + internal token.
	if got := rail.lastCapturePath.Load().(string); got != "/v1/deposits/"+depositID.String()+"/capture" {
		t.Fatalf("capture path = %q", got)
	}
	if got := rail.lastCaptureTenant.Load().(string); got != tenantID.String() {
		t.Fatalf("capture tenant = %q", got)
	}
	if got := rail.lastUserID.Load().(string); got != "user-9" {
		t.Fatalf("X-User-Id = %q (K6/K7 operator identity must propagate)", got)
	}
	if got := rail.lastRoles.Load().(string); got != "admin,finance" {
		t.Fatalf("X-User-Roles = %q", got)
	}
	if got := rail.lastToken.Load().(string); got != "tok-123" {
		t.Fatalf("X-Internal-Token = %q", got)
	}
	// BookingCompleted CloudEvent on the events topic (the fixture seed
	// bypasses ops.Create, so only the completion event lands on this
	// topic).
	types := outboxTypes(t, st, "booking.events")
	if len(types) != 1 || types[0] != "com.opendesk.booking.BookingCompleted" {
		t.Fatalf("outbox events: %v", types)
	}
	if hook.calls.Load() != 1 {
		t.Fatalf("loyalty hook calls = %d, want 1", hook.calls.Load())
	}

	// Idempotent replay: same row back, no second event, no second hook
	// fire; the capture IS re-attempted (payments dedupes by deposit id).
	res2, err := svc.Complete(ctx, tenantID, "acme-ng", bookingID, &depositID, CallerIdentity{})
	if err != nil {
		t.Fatalf("replay complete: %v", err)
	}
	if !res2.AlreadyCompleted || res2.Capture == nil {
		t.Fatalf("replay result: %+v", res2)
	}
	if rail.captureCalls.Load() != 2 {
		t.Fatalf("capture calls = %d, want 2 (replay re-attempts the idempotent rail call)", rail.captureCalls.Load())
	}
	if got := len(outboxTypes(t, st, "booking.events")); got != 1 {
		t.Fatalf("outbox after replay = %d events, want 1", got)
	}
	if hook.calls.Load() != 1 {
		t.Fatalf("loyalty hook after replay = %d, want 1 (no double accrual)", hook.calls.Load())
	}
}

// K11: only confirmed|checked_in may complete.
func TestCompleteRejectsIllegalTransition(t *testing.T) {
	st, ctx, tenantID, bookingID := completeFixture(t, store.StatusPending)
	svc := &Service{Store: st, EventsTopic: "booking.events", Logger: zap.NewNop()}
	if _, err := svc.Complete(ctx, tenantID, "acme-ng", bookingID, nil, CallerIdentity{}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err=%v, want ErrInvalidTransition", err)
	}
	// And the booking is untouched.
	b, _ := st.GetBooking(ctx, tenantID, bookingID)
	if b.Status != store.StatusPending {
		t.Fatalf("status mutated to %q", b.Status)
	}
}

// K11 fail-closed: a referenced deposit with NO payments rail errors BEFORE
// any mutation (httpapi maps ErrPaymentsNotConfigured → 503).
func TestCompleteFailsClosedWithoutPayments(t *testing.T) {
	st, ctx, tenantID, bookingID := completeFixture(t, store.StatusConfirmed)
	svc := &Service{Store: st, EventsTopic: "booking.events", Logger: zap.NewNop()} // Payments: nil
	depositID := uuid.New()
	if _, err := svc.Complete(ctx, tenantID, "acme-ng", bookingID, &depositID, CallerIdentity{}); !errors.Is(err, ErrPaymentsNotConfigured) {
		t.Fatalf("err=%v, want ErrPaymentsNotConfigured", err)
	}
	b, _ := st.GetBooking(ctx, tenantID, bookingID)
	if b.Status != store.StatusConfirmed {
		t.Fatalf("status mutated to %q despite closed rail", b.Status)
	}
	// Without a deposit reference, completion still works rail-less (no
	// capture to perform).
	res, err := svc.Complete(ctx, tenantID, "acme-ng", bookingID, nil, CallerIdentity{})
	if err != nil || res.Capture != nil {
		t.Fatalf("rail-less complete without deposit: %+v, %v", res, err)
	}
}

// K12: the refund rail — validation, idempotency key default, fail-closed.
func TestRefundViaPaymentsRail(t *testing.T) {
	st, ctx, tenantID, bookingID := completeFixture(t, store.StatusCompleted)
	rail := newPaymentsStub(t)
	svc := &Service{
		Store: st, EventsTopic: "booking.events", Logger: zap.NewNop(),
		Payments: NewPaymentsClient(rail.srv.URL, "", zap.NewNop()),
	}
	res, err := svc.Refund(ctx, tenantID, bookingID, RefundRequest{AmountCents: 5000, Reason: "customer request"}, CallerIdentity{})
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if res.RefundID == "" || res.AmountCents != 5000 {
		t.Fatalf("refund result: %+v", res)
	}
	// V2-advisory: the rail outcome is surfaced, never hardcoded "posted".
	if res.Status != "queued_manual" || res.Rail != "none" || res.RailDetail == "" {
		t.Fatalf("refund rail outcome masked: %+v", res)
	}
	body := rail.lastRefund.Load().(string)
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	if m["idempotency_key"] != "refund-"+bookingID.String() {
		t.Fatalf("default idempotency key = %v, want refund-{booking_id}: %s", m["idempotency_key"], body)
	}
	if m["tenant_id"] != tenantID.String() || m["reason"] != "customer request" {
		t.Fatalf("refund body: %s", body)
	}

	// Validation: non-positive amount.
	if _, err := svc.Refund(ctx, tenantID, bookingID, RefundRequest{AmountCents: 0}, CallerIdentity{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero amount: err=%v, want ErrInvalidInput", err)
	}
	// Fail-closed without a rail.
	svcNoRail := &Service{Store: st, EventsTopic: "booking.events", Logger: zap.NewNop()}
	if _, err := svcNoRail.Refund(ctx, tenantID, bookingID, RefundRequest{AmountCents: 100}, CallerIdentity{}); !errors.Is(err, ErrPaymentsNotConfigured) {
		t.Fatalf("no rail: err=%v, want ErrPaymentsNotConfigured", err)
	}
	// Unknown booking in the tenant → not found.
	if _, err := svc.Refund(ctx, tenantID, uuid.New(), RefundRequest{AmountCents: 100}, CallerIdentity{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown booking: err=%v, want ErrNotFound", err)
	}
}
