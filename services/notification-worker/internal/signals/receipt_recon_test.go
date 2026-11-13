package signals

// SPEC-W56-B bridge tests: com.opendesk.booking.ReceiptReconFinding must
// start ReceiptReconAlertWorkflow with the W56 cross-scope contract payload
// and the deterministic dedupe ID receipt-recon-<finding_id>; malformed
// events fail closed (warn+ack, no start); redeliveries dedupe on
// WorkflowExecutionAlreadyStarted. Optional payload fields decode
// tolerantly (absent = zero value). There is NO reachable-contact
// requirement — the alert is OPS/STAFF-only and the payload carries no
// customer contact fields.

import (
	"context"
	"testing"

	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"
	"go.uber.org/zap"
)

// receiptReconEvent is the exact CloudEvent shape booking-service's
// reconciler emits (W56 cross-scope contract): finding_id, tenant_id, kind,
// confidence, plus the optional reconciliation anchors capture_client_id,
// booking_id, attempt_tx_ref, expected_amount_minor, captured_amount_minor,
// receipt_amount_major, detail.
const receiptReconEvent = `{
  "type": "com.opendesk.booking.ReceiptReconFinding",
  "subject": "acme",
  "tenantid": "t-1",
  "data": {
    "finding_id": "f-123",
    "tenant_id": "t-1",
    "kind": "amount_mismatch",
    "confidence": "exact",
    "capture_client_id": "cap-9",
    "booking_id": "b-42",
    "attempt_tx_ref": "tx_abc",
    "expected_amount_minor": 500000,
    "captured_amount_minor": 450000,
    "receipt_amount_major": 4500.5,
    "detail": {"currency_unknown": true, "amount_window_days": 3}
  }
}`

// Happy path: exactly one ReceiptReconAlertWorkflow start, deterministic
// dedupe ID derived from finding_id, payload decoded field-by-field (int64
// minor units, float64 receipt major units, detail passthrough).
func TestReceiptReconFindingStartsAlertWorkflow(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	require.NoError(t, b.Process(context.Background(), []byte(receiptReconEvent)))

	require.Equal(t, 1, st.calls)
	require.Equal(t, "receipt-recon-f-123", st.options.ID, "dedupe ID derived from finding_id")
	require.Equal(t, "q", st.options.TaskQueue)
	require.Equal(t, "ReceiptReconAlertWorkflow", st.workflowType)
	require.Len(t, st.args, 1)
	send, ok := st.args[0].(workflows.ReceiptReconAlertSend)
	require.True(t, ok, "arg is a ReceiptReconAlertSend, got %T", st.args[0])
	require.Equal(t, "f-123", send.FindingID)
	require.Equal(t, "t-1", send.TenantID)
	require.Equal(t, "acme", send.TenantSlug, "tenant slug rides the envelope subject")
	require.Equal(t, "amount_mismatch", send.Kind)
	require.Equal(t, "exact", send.Confidence)
	require.Equal(t, "cap-9", send.CaptureClientID)
	require.Equal(t, "b-42", send.BookingID)
	require.Equal(t, "tx_abc", send.AttemptTxRef)
	require.Equal(t, int64(500000), send.ExpectedAmountMinor, "expected_amount_minor decodes as int64 minor units")
	require.Equal(t, int64(450000), send.CapturedAmountMinor, "captured_amount_minor decodes as int64 minor units")
	require.InDelta(t, 4500.5, send.ReceiptAmountMajor, 1e-9, "receipt_amount_major decodes as float major units")
	require.Equal(t, map[string]any{"currency_unknown": true, "amount_window_days": float64(3)}, send.Detail,
		"detail rides through verbatim")
	require.Empty(t, f.got, "no booking-scoped signals for a recon finding event")
}

// The bare (unprefixed) event type is accepted too, mirroring the
// BookingPaymentAmountMismatch tolerance.
func TestReceiptReconFindingBareTypeAccepted(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	evt := []byte(`{"type":"ReceiptReconFinding","subject":"acme","tenantid":"t-1",
	  "data":{"finding_id":"f-bare","tenant_id":"t-1","kind":"receipt_without_payment","confidence":"none"}}`)
	require.NoError(t, b.Process(context.Background(), evt))
	require.Equal(t, 1, st.calls)
	require.Equal(t, "receipt-recon-f-bare", st.options.ID)
	send := st.args[0].(workflows.ReceiptReconAlertSend)
	require.Equal(t, "receipt_without_payment", send.Kind)
}

// Tolerant decode: a payload carrying ONLY the required fields still
// starts the alert — every optional anchor/amount/detail decodes to its
// zero value, never an error (a receipt_without_payment finding
// legitimately carries no booking/payment anchors).
func TestReceiptReconFindingOptionalFieldsTolerated(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	evt := []byte(`{"type":"com.opendesk.booking.ReceiptReconFinding","subject":"acme","tenantid":"t-1",
	  "data":{"finding_id":"f-min","kind":"receipt_without_payment","confidence":"none"}}`)
	require.NoError(t, b.Process(context.Background(), evt))
	require.Equal(t, 1, st.calls, "no booking_id/contact needed for an ops alert")
	send := st.args[0].(workflows.ReceiptReconAlertSend)
	require.Empty(t, send.CaptureClientID)
	require.Empty(t, send.BookingID)
	require.Empty(t, send.AttemptTxRef)
	require.Zero(t, send.ExpectedAmountMinor)
	require.Zero(t, send.CapturedAmountMinor)
	require.Zero(t, send.ReceiptAmountMajor)
	require.Nil(t, send.Detail)
}

// Envelope defaults: tenant_id falls back to the CloudEvents tenantid
// extension; the slug always comes from the subject.
func TestReceiptReconFindingEnvelopeDefaults(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	evt := []byte(`{"type":"com.opendesk.booking.ReceiptReconFinding","subject":"acme","tenantid":"t-9",
	  "data":{"finding_id":"f-env","kind":"payment_without_receipt","confidence":"heuristic","booking_id":"b-2"}}`)
	require.NoError(t, b.Process(context.Background(), evt))
	require.Equal(t, 1, st.calls)
	send := st.args[0].(workflows.ReceiptReconAlertSend)
	require.Equal(t, "t-9", send.TenantID)
	require.Equal(t, "acme", send.TenantSlug)
}

// Fail-closed: missing finding_id/kind or a malformed payload → warn+ack
// without any workflow start (an alert without reconciliation anchors must
// never fire).
func TestReceiptReconFindingFailClosed(t *testing.T) {
	for name, data := range map[string]string{
		"missing finding_id":    `{"kind":"amount_mismatch","confidence":"exact"}`,
		"missing kind":          `{"finding_id":"f-1","confidence":"exact"}`,
		"empty data":            `{}`,
		"malformed expected":    `{"finding_id":"f-1","kind":"amount_mismatch","expected_amount_minor":"lots"}`,
		"malformed captured":    `{"finding_id":"f-1","kind":"amount_mismatch","captured_amount_minor":"lots"}`,
		"malformed receipt amt": `{"finding_id":"f-1","kind":"amount_mismatch","receipt_amount_major":"lots"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeSignaller{}
			st := &fakeStarter{}
			b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
			evt := []byte(`{"type":"com.opendesk.booking.ReceiptReconFinding","subject":"acme","data":` + data + `}`)
			require.NoError(t, b.Process(context.Background(), evt))
			require.Equal(t, 0, st.calls, "no workflow start on malformed/incomplete payload")
			require.Empty(t, f.got)
		})
	}
}

// Nil starter keeps the old posture: acknowledged, no panic.
func TestReceiptReconFindingNilStarter(t *testing.T) {
	f := &fakeSignaller{}
	b := newBridge(f)
	require.NoError(t, b.Process(context.Background(), []byte(receiptReconEvent)))
	require.Empty(t, f.got)
}

// Redelivery dedupe: WorkflowExecutionAlreadyStarted is acknowledged, not
// retried — a Kafka redelivery can never double-alert.
func TestReceiptReconFindingAlreadyStartedIsAcked(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{err: &serviceerror.WorkflowExecutionAlreadyStarted{}}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	require.NoError(t, b.Process(context.Background(), []byte(receiptReconEvent)))
	require.Equal(t, 1, st.calls)
}

// A replayed identical event (Kafka redelivery) resolves to the same
// deterministic workflow ID and is acknowledged on AlreadyStarted: exactly
// one effective workflow for any number of deliveries.
func TestReceiptReconFindingReplayDeduplicates(t *testing.T) {
	f := &fakeSignaller{}
	st := &dedupeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	require.NoError(t, b.Process(context.Background(), []byte(receiptReconEvent)))
	require.NoError(t, b.Process(context.Background(), []byte(receiptReconEvent)))
	require.NoError(t, b.Process(context.Background(), []byte(receiptReconEvent)))
	require.Equal(t, 3, st.calls, "every delivery attempts a start")
	require.Len(t, st.seen, 1, "all deliveries resolve to ONE workflow ID")
	require.Equal(t, 3, st.seen["receipt-recon-f-123"])
}
