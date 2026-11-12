package activities

// Payment amount-mismatch ops alert (SPEC-W51-A): booking-service emits
// com.opendesk.booking.BookingPaymentAmountMismatch when a captured amount
// differs from the offering price the booking expected (sacred W51 contract
// delta: expected_amount_minor). This is a funds-integrity signal for
// OPS/STAFF ONLY — never the customer contact.
//
// Routing: the existing ops-alert channel binding (SPEC-W34 GF16 /
// SPEC-W44 K3 — activities/ops_alerts.go): a CRITICAL structured log plus a
// com.opendesk.ops.PaymentAmountMismatch CloudEvent published to the ops
// alerts topic (OPS_ALERTS_TOPIC, default opendesk.ops.alerts), which the
// opsalerts consumer persists for the role-gated GET /v1/ops-alerts
// read-back. Nil producer / "off" topic degrades to log-only, exactly like
// EmitOpsAlert. The activity NEVER invokes a customer-facing Dapr binding
// (no sms/email/whatsapp) — the payload struct carries no customer contact
// fields at all, so a leak is structurally impossible.
//
// DND classification (internal/pacer guards.go): ops_alert is
// SYSTEM/TRANSACTIONAL — marketing DND suppression and quiet-hours deferral
// never apply (and the phone-keyed registries have no recipient to check).

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.uber.org/zap"
)

// EventTypePaymentAmountMismatch is emitted once per payment amount
// mismatch on the ops-alerts topic.
const EventTypePaymentAmountMismatch = "com.opendesk.ops.PaymentAmountMismatch"

// SendPaymentMismatchAlert raises one ops/staff alert for a payment amount
// mismatch. A missing booking_id/tx_ref is an activity error — the signals
// bridge already fail-closes such events, so seeing one here means a
// malformed schedule (never a silent no-op on a funds-integrity signal). A
// publish failure is returned as an activity error so Temporal's retry
// schedule applies (mirrors EmitOpsAlert: this alert may be the ONLY signal
// of a capture/price divergence, so it must not be dropped silently).
func (a *Activities) SendPaymentMismatchAlert(ctx context.Context, in workflows.PacedPaymentMismatchSend) error {
	if in.BookingID == "" || in.TxRef == "" {
		return fmt.Errorf("payment mismatch alert %s: missing booking_id/tx_ref (booking_id=%q)", in.TxRef, in.BookingID)
	}
	msg := paymentMismatchMessage(in)

	a.Log.Error("CRITICAL: payment amount mismatch — captured amount differs from the booking's expected offering price; manual ops reconciliation required",
		zap.String("booking_id", in.BookingID),
		zap.String("booking_ref", in.BookingRef),
		zap.String("tenant_id", in.TenantID),
		zap.String("tenant_slug", in.TenantSlug),
		zap.String("tx_ref", in.TxRef),
		zap.String("currency", in.Currency),
		zap.Int64("expected_amount_minor", in.ExpectedAmountMinor),
		zap.Int64("captured_amount_minor", in.AmountMinor),
		zap.String("paid_at", in.PaidAt),
		zap.String("message", msg))

	if a.Ops.Producer == nil || a.Ops.Topic == "" || a.Ops.Topic == "off" {
		return nil // log-only degradation (no brokers / disabled), mirrors EmitOpsAlert
	}
	now := time.Now().UTC()
	evt := map[string]any{
		"specversion": "1.0",
		"id":          uuid.NewString(),
		"source":      "notification-worker",
		"type":        EventTypePaymentAmountMismatch,
		"subject":     in.BookingID,
		"time":        now.Format(time.RFC3339),
		"tenantid":    in.TenantID,
		"data": map[string]any{
			"tenant_id":             in.TenantID,
			"tenant_slug":           in.TenantSlug,
			"booking_id":            in.BookingID,
			"booking_ref":           in.BookingRef,
			"tx_ref":                in.TxRef,
			"currency":              in.Currency,
			"expected_amount_minor": in.ExpectedAmountMinor,
			"captured_amount_minor": in.AmountMinor,
			"paid_at":               in.PaidAt,
			"message":               msg,
			"severity":              "critical",
			"ts":                    now.Format(time.RFC3339),
		},
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal payment mismatch ops alert: %w", err)
	}
	// Keyed by booking id so one booking's alerts stay ordered (mirrors
	// EmitOpsAlert).
	if err := a.Ops.Producer.Produce(ctx, a.Ops.Topic, []byte(in.BookingID), payload); err != nil {
		return fmt.Errorf("publish payment mismatch ops alert to %s: %w", a.Ops.Topic, err)
	}
	return nil
}

// paymentMismatchMessage renders the ops alert body:
//
//	"PAYMENT AMOUNT MISMATCH — tenant acme booking BK-1042 (b-42): expected
//	NGN 5,000.00, captured NGN 4,500.00. Ref: tx_abc123. Paid at
//	2030-01-01T10:00:00Z."
//
// booking_ref (the human public ref, BK-XXXXXXXX per the W51 contract) is
// preferred; booking_id is the fallback when the producer could not resolve
// one. Both amounts render via the W50-B FormatMinorAmount helper.
func paymentMismatchMessage(in workflows.PacedPaymentMismatchSend) string {
	ref := in.BookingRef
	if ref == "" {
		ref = in.BookingID
	}
	msg := fmt.Sprintf("PAYMENT AMOUNT MISMATCH — tenant %s booking %s (%s): expected %s, captured %s. Ref: %s.",
		in.TenantSlug, ref, in.BookingID,
		FormatMinorAmount(in.Currency, in.ExpectedAmountMinor),
		FormatMinorAmount(in.Currency, in.AmountMinor),
		in.TxRef)
	if in.PaidAt != "" {
		msg += " Paid at " + in.PaidAt + "."
	}
	return msg
}
