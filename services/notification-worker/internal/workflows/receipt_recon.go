package workflows

import (
	"go.temporal.io/sdk/workflow"
)

// Receipt ↔ payment reconciliation ops alert (SPEC-W56-B): booking-service's
// reconciler persists a NEW finding (deduped booking-side via
// UNIQUE(tenant_id, kind, capture_client_id, booking_id) + ON CONFLICT DO
// NOTHING) and emits com.opendesk.booking.ReceiptReconFinding on the booking
// events topic in the same transaction; the signals bridge starts one
// ReceiptReconAlertWorkflow per finding (deterministic workflow ID
// receipt-recon-<finding_id>, so a Kafka redelivery can never double-alert).

// ActivitySendReceiptReconAlert is the name of the receipt-recon ops-alert
// activity (SPEC-W56-B). The workflow invokes it directly by name.
const ActivitySendReceiptReconAlert = "SendReceiptReconAlert"

// ReceiptReconAlertSend carries the SendReceiptReconAlert arguments
// (SPEC-W56-B): one OPS/STAFF-only alert for a receipt ↔ payment
// reconciliation finding. It mirrors the W56 cross-scope contract payload of
// the com.opendesk.booking.ReceiptReconFinding CloudEvent (finding_id,
// tenant_id, kind, confidence, plus the optional reconciliation anchors
// capture_client_id / booking_id / attempt_tx_ref / expected_amount_minor /
// captured_amount_minor / receipt_amount_major / detail) — the service
// boundary contract is duplicated, not shared; keep the field tags in sync
// with booking-service's outbox payload. TenantSlug is envelope-derived
// (CloudEvent subject). Optional fields decode tolerantly: absent = zero
// value, never an error.
//
// GUARD (SPEC-W56-B, mirroring SPEC-W51-A): this payload deliberately
// carries NO customer contact fields — recon findings are an internal
// post-hoc reconciliation signal routed to the ops channel, never to the
// customer.
type ReceiptReconAlertSend struct {
	FindingID  string `json:"finding_id"`
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug,omitempty"`
	// Kind is receipt_without_payment | amount_mismatch |
	// payment_without_receipt (| currency_unknown when it blocks an
	// otherwise-exact amount match).
	Kind       string `json:"kind"`
	Confidence string `json:"confidence"` // exact | heuristic | none
	// Optional reconciliation anchors — absent in the payload = "".
	CaptureClientID string `json:"capture_client_id,omitempty"`
	BookingID       string `json:"booking_id,omitempty"`
	AttemptTxRef    string `json:"attempt_tx_ref,omitempty"`
	// Optional amounts — absent in the payload = 0. Expected/captured are
	// INTEGER minor units; the receipt amount is float MAJOR units (receipts
	// carry no currency — currency_unknown rides in Detail per SPEC-W56
	// pinned decision 3).
	ExpectedAmountMinor int64   `json:"expected_amount_minor,omitempty"`
	CapturedAmountMinor int64   `json:"captured_amount_minor,omitempty"`
	ReceiptAmountMajor  float64 `json:"receipt_amount_major,omitempty"`
	// Detail is the finding's JSONB context (candidate list on ambiguous
	// matches, currency_unknown:true, amount-window notes); absent = nil.
	Detail map[string]any `json:"detail,omitempty"`
}

// ReceiptReconAlertWorkflow raises exactly one OPS/STAFF alert through
// SendReceiptReconAlert. The alert is SYSTEM/TRANSACTIONAL-class (a
// post-hoc reconciliation signal, softer than the W49 rail-vs-price
// mismatch): never DND-suppressed, never quiet-hours deferred.
//
// GUARD: the alert goes to the ops channel ONLY — the payload carries no
// customer contact fields and the activity never touches a customer-facing
// binding, so a finding can never leak internal reconciliation detail to
// the customer.
//
// The activity executes DIRECTLY (not via NotifyPaced), mirroring the
// saga's EmitOpsAlert ops-alert precedent (booking_saga.go): the paced
// dispatch table (activities/paced.go) is outside this wave's ownership,
// and ops alerts are SYSTEM-class — the pacer guard short-circuits on them
// anyway. It deliberately does NOT look anything up: the event payload
// already carries every field the alert needs (the W56 cross-scope
// contract), so no extra cross-service call can fail the alert.
func ReceiptReconAlertWorkflow(ctx workflow.Context, send ReceiptReconAlertSend) error {
	logger := workflow.GetLogger(ctx)
	ctx = workflow.WithActivityOptions(ctx, sagaActivityOptions())
	if err := workflow.ExecuteActivity(ctx, ActivitySendReceiptReconAlert, send).Get(ctx, nil); err != nil {
		// Mirrors the payment mismatch: a failed alert is logged, never
		// fatal — the finding is persisted in booking-service regardless of
		// alert delivery. (The ops-alerts publish itself is retried
		// activity-side per sagaActivityOptions before we ever get here.)
		logger.Error("receipt recon ops alert failed", "finding_id", send.FindingID, "kind", send.Kind, "error", err)
		return nil
	}
	logger.Info("receipt recon ops alert sent", "finding_id", send.FindingID, "kind", send.Kind)
	return nil
}
