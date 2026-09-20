package bookingops

// V2-advisory regression tests: Refund must decode and surface payments'
// honest rail outcome (status/rail/rail_detail, SPEC-W45 K12
// RefundResponse) instead of hardcoding Status "posted" — a queued_manual
// refund reported as posted masks money that has NOT moved.

import (
	"context"
	"encoding/json"
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
