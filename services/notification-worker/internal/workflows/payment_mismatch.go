package workflows

import (
	"go.temporal.io/sdk/workflow"
)

// Payment amount-mismatch ops alert (SPEC-W51-A): booking-service emits
// com.opendesk.booking.BookingPaymentAmountMismatch on the booking events
// topic when a capture's amount differs from the offering price the booking
// expected; the signals bridge starts one PaymentMismatchAlertWorkflow per
// transaction (deterministic workflow ID payment-mismatch-<tx_ref>, so a
// Kafka redelivery can never double-alert).

// ActivitySendPaymentMismatchAlert is declared in paced.go next to the
// other paced-send activity names.

// PaymentMismatchAlertWorkflow raises exactly one OPS/STAFF alert through
// the paced send path (PacedSendOpsAlert → SendPaymentMismatchAlert). The
// kind is SYSTEM/TRANSACTIONAL (a funds-integrity alert, like the saga
// compensation exhaustion alert): never DND-suppressed, never quiet-hours
// deferred.
//
// GUARD: the alert goes to the ops channel ONLY — the payload carries no
// customer contact fields and the activity never touches a customer-facing
// binding, so a mismatch can never leak internal reconciliation detail to
// the customer.
//
// It deliberately does NOT look the booking up: the event payload already
// carries every field the alert needs (the sacred W51 cross-scope
// contract), so no extra cross-service call can fail the alert.
func PaymentMismatchAlertWorkflow(ctx workflow.Context, send PacedPaymentMismatchSend) error {
	logger := workflow.GetLogger(ctx)
	ctx = workflow.WithActivityOptions(ctx, sagaActivityOptions())
	req := PacedSendRequest{Kind: PacedSendOpsAlert, PaymentMismatch: &send}
	if err := workflow.ExecuteActivity(ctx, ActivityNotifyPaced, req).Get(ctx, nil); err != nil {
		// Mirrors the payment receipt: a failed alert is logged, never
		// fatal — the mismatch is recorded in booking-service regardless
		// of alert delivery. (The ops-alerts publish itself is retried
		// activity-side per sagaActivityOptions before we ever get here.)
		logger.Error("payment mismatch ops alert failed", "tx_ref", send.TxRef, "booking_id", send.BookingID, "error", err)
		return nil
	}
	logger.Info("payment mismatch ops alert sent", "tx_ref", send.TxRef, "booking_id", send.BookingID)
	return nil
}
