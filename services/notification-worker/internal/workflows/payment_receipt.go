package workflows

import (
	"go.temporal.io/sdk/workflow"
)

// Payment receipt (SPEC-W50-B): booking-service emits
// com.opendesk.booking.BookingPaymentConfirmed on the booking events topic
// after a capture transitions a booking to paid; the signals bridge starts
// one PaymentReceiptWorkflow per transaction (deterministic workflow ID
// payment-receipt-<tx_ref>, so a Kafka redelivery can never double-send).

// ActivitySendPaymentReceipt is declared in paced.go next to the other
// paced-send activity names.

// PaymentReceiptWorkflow delivers exactly one payment receipt through the
// paced send path (PacedSendPaymentReceipt → SendPaymentReceipt). The kind
// is TRANSACTIONAL (a receipt is a transaction record, like the booking
// confirmation): never DND-suppressed, never quiet-hours deferred.
//
// It deliberately does NOT look the booking up: the event payload already
// carries every field the receipt needs (the sacred W50 cross-scope
// contract), so no extra cross-service call can fail the send.
func PaymentReceiptWorkflow(ctx workflow.Context, send PacedPaymentReceiptSend) error {
	logger := workflow.GetLogger(ctx)
	ctx = workflow.WithActivityOptions(ctx, sagaActivityOptions())
	req := PacedSendRequest{Kind: PacedSendPaymentReceipt, PaymentReceipt: &send}
	if err := workflow.ExecuteActivity(ctx, ActivityNotifyPaced, req).Get(ctx, nil); err != nil {
		// Mirrors the waitlist notify: a failed send is logged, never
		// fatal — delivery is best-effort and the payment is recorded
		// regardless of notification delivery.
		logger.Error("payment receipt send failed", "tx_ref", send.TxRef, "booking_id", send.BookingID, "error", err)
		return nil
	}
	logger.Info("payment receipt sent", "tx_ref", send.TxRef, "booking_id", send.BookingID)
	return nil
}
