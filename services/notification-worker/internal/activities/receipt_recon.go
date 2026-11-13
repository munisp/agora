package activities

// Receipt ↔ payment reconciliation ops alert (SPEC-W56-B): booking-service's
// reconciler emits com.opendesk.booking.ReceiptReconFinding for each NEW
// persisted finding (receipt_without_payment / amount_mismatch /
// payment_without_receipt). This is a post-hoc reconciliation signal for
// OPS/STAFF ONLY — never the customer contact.
//
// Routing: the existing ops-alert channel binding (SPEC-W34 GF16 /
// SPEC-W44 K3 — activities/ops_alerts.go), cloned from the W49/W51
// PaymentMismatch path: a structured log plus a
// com.opendesk.ops.ReceiptReconFinding CloudEvent published to the ops
// alerts topic (OPS_ALERTS_TOPIC, default opendesk.ops.alerts), which the
// opsalerts consumer persists for the role-gated GET /v1/ops-alerts
// read-back. Nil producer / "off" topic degrades to log-only, exactly like
// EmitOpsAlert. The activity NEVER invokes a customer-facing Dapr binding
// (no sms/email/whatsapp) — the payload struct carries no customer contact
// fields at all, so a leak is structurally impossible.
//
// Severity (SPEC-W56 pinned decision 5): receipt_without_payment and
// amount_mismatch are "high"; payment_without_receipt is "info" — softer
// than the W49 critical mismatch (post-hoc recon, not rail-vs-price).
// Unknown/future kinds default to "info" (softest honest default).
//
// DND classification (internal/pacer guards.go): this alert is
// SYSTEM/TRANSACTIONAL-class like the other ops alerts — marketing DND
// suppression and quiet-hours deferral never apply (and the phone-keyed
// registries have no recipient to check).

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.uber.org/zap"
)

// EventTypeReceiptReconFinding is emitted once per receipt ↔ payment
// reconciliation finding on the ops-alerts topic.
const EventTypeReceiptReconFinding = "com.opendesk.ops.ReceiptReconFinding"

// Finding kinds emitted by booking-service's reconciler (SPEC-W56 /
// RECON §3.1).
const (
	ReconKindReceiptWithoutPayment = "receipt_without_payment"
	ReconKindAmountMismatch        = "amount_mismatch"
	ReconKindPaymentWithoutReceipt = "payment_without_receipt"
)

// receiptReconSeverity maps a finding kind to its ops-alert severity per
// SPEC-W56 pinned decision 5. Anything unrecognized (including
// currency_unknown) degrades to "info" — the softest honest default for a
// post-hoc signal.
func receiptReconSeverity(kind string) string {
	switch kind {
	case ReconKindReceiptWithoutPayment, ReconKindAmountMismatch:
		return "high"
	default:
		return "info"
	}
}

// SendReceiptReconAlert raises one ops/staff alert for a receipt ↔ payment
// reconciliation finding. A missing finding_id/kind is an activity error —
// the signals bridge already fail-closes such events, so seeing one here
// means a malformed schedule (never a silent no-op on a reconciliation
// signal). A publish failure is returned as an activity error so Temporal's
// retry schedule applies (mirrors SendPaymentMismatchAlert: this alert may
// be the ONLY signal of a receipt/payment divergence, so it must not be
// dropped silently).
func (a *Activities) SendReceiptReconAlert(ctx context.Context, in workflows.ReceiptReconAlertSend) error {
	if in.FindingID == "" || in.Kind == "" {
		return fmt.Errorf("receipt recon alert %s: missing finding_id/kind (finding_id=%q kind=%q)", in.FindingID, in.FindingID, in.Kind)
	}
	severity := receiptReconSeverity(in.Kind)
	msg := receiptReconMessage(in)

	fields := []zap.Field{
		zap.String("finding_id", in.FindingID),
		zap.String("tenant_id", in.TenantID),
		zap.String("tenant_slug", in.TenantSlug),
		zap.String("kind", in.Kind),
		zap.String("confidence", in.Confidence),
		zap.String("severity", severity),
		zap.String("capture_client_id", in.CaptureClientID),
		zap.String("booking_id", in.BookingID),
		zap.String("attempt_tx_ref", in.AttemptTxRef),
		zap.String("message", msg),
	}
	if severity == "high" {
		a.Log.Warn("receipt reconciliation finding — manual ops review required", fields...)
	} else {
		a.Log.Info("receipt reconciliation finding", fields...)
	}

	if a.Ops.Producer == nil || a.Ops.Topic == "" || a.Ops.Topic == "off" {
		return nil // log-only degradation (no brokers / disabled), mirrors EmitOpsAlert
	}
	now := time.Now().UTC()
	// Ops-only payload: reconciliation anchors + amounts + context, NEVER
	// customer contact fields (mirrors the payment-mismatch discipline).
	// Optional anchors/amounts are included only when present in the source
	// event, so absence stays honest (no fabricated zero amounts).
	data := map[string]any{
		"tenant_id":   in.TenantID,
		"tenant_slug": in.TenantSlug,
		"finding_id":  in.FindingID,
		"kind":        in.Kind,
		"confidence":  in.Confidence,
		"message":     msg,
		"severity":    severity,
		"ts":          now.Format(time.RFC3339),
	}
	if in.CaptureClientID != "" {
		data["capture_client_id"] = in.CaptureClientID
	}
	if in.BookingID != "" {
		data["booking_id"] = in.BookingID
	}
	if in.AttemptTxRef != "" {
		data["attempt_tx_ref"] = in.AttemptTxRef
	}
	if in.ExpectedAmountMinor != 0 {
		data["expected_amount_minor"] = in.ExpectedAmountMinor
	}
	if in.CapturedAmountMinor != 0 {
		data["captured_amount_minor"] = in.CapturedAmountMinor
	}
	if in.ReceiptAmountMajor != 0 {
		data["receipt_amount_major"] = in.ReceiptAmountMajor
	}
	if in.Detail != nil {
		data["detail"] = in.Detail
	}
	evt := map[string]any{
		"specversion": "1.0",
		"id":          uuid.NewString(),
		"source":      "notification-worker",
		"type":        EventTypeReceiptReconFinding,
		"subject":     in.FindingID,
		"time":        now.Format(time.RFC3339),
		"tenantid":    in.TenantID,
		"data":        data,
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal receipt recon ops alert: %w", err)
	}
	// Keyed by finding id so one finding's alerts stay ordered (mirrors
	// EmitOpsAlert's per-anchor keying; findings may carry no booking id).
	if err := a.Ops.Producer.Produce(ctx, a.Ops.Topic, []byte(in.FindingID), payload); err != nil {
		return fmt.Errorf("publish receipt recon ops alert to %s: %w", a.Ops.Topic, err)
	}
	return nil
}

// receiptReconMessage renders the ops alert body. Amounts render raw
// (minor units for payment amounts, major for the receipt) with the
// currency honesty note — receipts carry no currency, so comparisons
// assume the offering currency (SPEC-W56 pinned decision 3).
func receiptReconMessage(in workflows.ReceiptReconAlertSend) string {
	anchor := in.CaptureClientID
	if anchor == "" {
		anchor = in.BookingID
	}
	if anchor == "" {
		anchor = in.AttemptTxRef
	}
	msg := fmt.Sprintf("RECEIPT RECONCILIATION — tenant %s: %s (finding %s, confidence %s)",
		in.TenantSlug, in.Kind, in.FindingID, in.Confidence)
	if anchor != "" {
		msg += " — " + anchor
	}
	switch in.Kind {
	case ReconKindAmountMismatch:
		msg += fmt.Sprintf(": expected %d minor, captured %d minor, receipt %g major (currency unknown — assumed offering currency).",
			in.ExpectedAmountMinor, in.CapturedAmountMinor, in.ReceiptAmountMajor)
	case ReconKindReceiptWithoutPayment:
		msg += ": field-captured receipt has no matching payment candidate."
	case ReconKindPaymentWithoutReceipt:
		msg += ": captured payment has no matching field receipt in the recon window."
	default:
		msg += "."
	}
	return msg
}
