package bookingops

// V2-advisory regression tests: Refund must decode and surface payments'
// honest rail outcome (status/rail/rail_detail, SPEC-W45 K12
// RefundResponse) instead of hardcoding Status "posted" — a queued_manual
// refund reported as posted masks money that has NOT moved.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// refundStub answers POST /v1/refunds with the given body (201).
func refundStub(t *testing.T, body map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/refunds" {
			http.Error(w, "no such route: "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Full K12 shape: the queued_manual honest fallback is surfaced verbatim.
func TestRefundDecodesRailOutcome(t *testing.T) {
	srv := refundStub(t, map[string]any{
		"id":           "rf-1",
		"refund_id":    "rf-1",
		"amount":       5000,
		"amount_cents": 5000,
		"status":       "queued_manual",
		"rail":         "none",
		"rail_detail":  "no provider transaction id on deposit; manual refund required",
	})
	c := NewPaymentsClient(srv.URL, "", zap.NewNop())
	res, err := c.Refund(context.Background(), uuid.NewString(), nil, 5000, "cancel", "refund-x", CallerIdentity{})
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if res.RefundID != "rf-1" || res.AmountCents != 5000 {
		t.Fatalf("refund id/amount: %+v", res)
	}
	if res.Status != "queued_manual" {
		t.Fatalf("status = %q, want queued_manual (payments' truth, never masked)", res.Status)
	}
	if res.Rail != "none" {
		t.Fatalf("rail = %q, want none", res.Rail)
	}
	if res.RailDetail != "no provider transaction id on deposit; manual refund required" {
		t.Fatalf("rail_detail = %q", res.RailDetail)
	}
}

// The provider-executed outcome surfaces too (flutterwave/refunded), and a
// LEGACY minimal body ({id, amount} only) decodes with an EMPTY status —
// booking must never invent "posted" again.
func TestRefundDecodeVariants(t *testing.T) {
	srv := refundStub(t, map[string]any{
		"id": "rf-2", "amount": 7000,
		"status": "refunded", "rail": "flutterwave",
	})
	c := NewPaymentsClient(srv.URL, "", zap.NewNop())
	res, err := c.Refund(context.Background(), uuid.NewString(), nil, 7000, "", "refund-y", CallerIdentity{})
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if res.Status != "refunded" || res.Rail != "flutterwave" || res.RailDetail != "" {
		t.Fatalf("refunded outcome: %+v", res)
	}

	legacy := refundStub(t, map[string]any{"id": "rf-3", "amount": 100})
	res, err = NewPaymentsClient(legacy.URL, "", zap.NewNop()).
		Refund(context.Background(), uuid.NewString(), nil, 100, "", "refund-z", CallerIdentity{})
	if err != nil {
		t.Fatalf("legacy refund: %v", err)
	}
	if res.RefundID != "rf-3" || res.AmountCents != 100 {
		t.Fatalf("legacy id/amount: %+v", res)
	}
	if res.Status != "" {
		t.Fatalf("legacy status = %q — booking must NOT fabricate a status (was hardcoded posted)", res.Status)
	}
}

// ---------------------------------------------------------------------------
// SPEC-W48 A3: Flutterwave initialize (public pay leg) client tests.
// ---------------------------------------------------------------------------

// initializeStub answers POST /v1/payments/flutterwave/initialize with the
// payments contract (201 {tx_ref, link, deposit_id}) and captures the
// request for assertions.
func initializeStub(t *testing.T, status int, body map[string]any) (*httptest.Server, *http.Request, map[string]any) {
	t.Helper()
	var gotBody map[string]any
	var gotReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/payments/flutterwave/initialize" {
			http.Error(w, "no such route: "+r.URL.Path, http.StatusNotFound)
			return
		}
		gotReq = r
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, gotReq, gotBody
}

// The happy path sends the payments contract body with the K2 internal
// token and decodes link/tx_ref.
func TestInitializeFlutterwave(t *testing.T) {
	srv, _, _ := initializeStub(t, http.StatusCreated, map[string]any{
		"tx_ref": "flw-tx-1", "link": "https://pay.test/checkout", "deposit_id": "dep-1",
	})
	c := NewPaymentsClient(srv.URL, "tok", zap.NewNop())
	tenantID := uuid.NewString()
	bookingID := uuid.NewString()
	res, err := c.InitializeFlutterwave(context.Background(), tenantID, FlutterwaveInitializeRequest{
		AmountCents:    7500,
		Currency:       "NGN",
		Email:          "ada@example.com",
		BookingID:      bookingID,
		IdempotencyKey: "agora-" + bookingID + "-1700000000",
		RedirectURL:    "https://site.test/done",
	}, CallerIdentity{}) // public leg: no gateway identity — the token authenticates
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if res.Link != "https://pay.test/checkout" || res.TxRef != "flw-tx-1" || res.DepositID != "dep-1" {
		t.Fatalf("result: %+v", res)
	}

	// Re-run against a capturing stub to assert the wire contract.
	var got map[string]any
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Token") != "tok" {
			t.Errorf("X-Internal-Token = %q, want tok (K2 service auth)", r.Header.Get("X-Internal-Token"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"tx_ref":"flw-tx-2","link":"https://pay.test/c2","deposit_id":"dep-2"}`))
	}))
	t.Cleanup(srv2.Close)
	c2 := NewPaymentsClient(srv2.URL, "tok", zap.NewNop())
	key := "agora-" + bookingID + "-1700000001"
	if _, err := c2.InitializeFlutterwave(context.Background(), tenantID, FlutterwaveInitializeRequest{
		AmountCents: 7500, Currency: "NGN", Email: "ada@example.com",
		BookingID: bookingID, IdempotencyKey: key,
	}, CallerIdentity{}); err != nil {
		t.Fatalf("initialize 2: %v", err)
	}
	if got["tenant_id"] != tenantID || got["booking_id"] != bookingID ||
		got["email"] != "ada@example.com" || got["currency"] != "NGN" ||
		got["idempotency_key"] != key {
		t.Fatalf("wire body: %+v", got)
	}
	if got["amount_cents"] != float64(7500) {
		t.Fatalf("amount_cents = %v, want 7500", got["amount_cents"])
	}
}

// A payments failure surfaces as a PaymentsError (mapped to 502 upstream);
// nothing is masked as success.
func TestInitializeFlutterwaveRailError(t *testing.T) {
	srv, _, _ := initializeStub(t, http.StatusBadGateway, map[string]any{"error": "flutterwave initialize failed"})
	c := NewPaymentsClient(srv.URL, "tok", zap.NewNop())
	_, err := c.InitializeFlutterwave(context.Background(), uuid.NewString(), FlutterwaveInitializeRequest{
		AmountCents: 100, Currency: "NGN", Email: "a@b.c", IdempotencyKey: "k",
	}, CallerIdentity{})
	var perr *PaymentsError
	if err == nil || !errors.As(err, &perr) || perr.Status != http.StatusBadGateway {
		t.Fatalf("err = %v, want *PaymentsError(502)", err)
	}
}
