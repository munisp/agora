package signals

// SPEC-W51-A bridge tests: com.opendesk.booking.BookingPaymentAmountMismatch
// must start PaymentMismatchAlertWorkflow with the sacred-contract payload
// (incl. the W51 delta expected_amount_minor) and the deterministic dedupe
// ID payment-mismatch-<tx_ref>; malformed events fail closed (warn+ack, no
// start); redeliveries dedupe on WorkflowExecutionAlreadyStarted. Unlike the
// payment receipt there is NO reachable-contact requirement — the alert is
// OPS/STAFF-only and the payload carries no customer contact fields.

import (
	"context"
	"testing"

	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"
	"go.uber.org/zap"
)

// amountMismatchEvent is the exact CloudEvent shape booking-service emits
// (sacred W51 cross-scope contract): booking_id, tenant_id, tx_ref,
// amount_minor (int64, captured), expected_amount_minor (int64, W51 delta —
// the offering price), currency, booking_ref (human public ref), paid_at.
const amountMismatchEvent = `{
  "type": "com.opendesk.booking.BookingPaymentAmountMismatch",
  "subject": "acme",
  "tenantid": "t-1",
  "data": {
    "booking_id": "b-42",
    "tenant_id": "t-1",
    "tx_ref": "tx_mm789",
    "amount_minor": 450000,
    "expected_amount_minor": 500000,
    "currency": "NGN",
    "booking_ref": "BK-7XQ4V2PZ",
    "paid_at": "2030-01-01T10:00:00Z"
  }
}`

// Happy path: exactly one PaymentMismatchAlertWorkflow start, deterministic
// dedupe ID derived from tx_ref, payload decoded field-by-field (incl.
// expected_amount_minor as int64).
func TestBookingPaymentAmountMismatchStartsAlertWorkflow(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	require.NoError(t, b.Process(context.Background(), []byte(amountMismatchEvent)))

	require.Equal(t, 1, st.calls)
	require.Equal(t, "payment-mismatch-tx_mm789", st.options.ID, "dedupe ID derived from tx_ref")
	require.Equal(t, "q", st.options.TaskQueue)
	require.Equal(t, "PaymentMismatchAlertWorkflow", st.workflowType)
	require.Len(t, st.args, 1)
	send, ok := st.args[0].(workflows.PacedPaymentMismatchSend)
	require.True(t, ok, "arg is a PacedPaymentMismatchSend, got %T", st.args[0])
	require.Equal(t, "b-42", send.BookingID)
	require.Equal(t, "t-1", send.TenantID)
	require.Equal(t, "acme", send.TenantSlug, "tenant slug rides the envelope subject")
	require.Equal(t, "tx_mm789", send.TxRef)
	require.Equal(t, int64(450000), send.AmountMinor, "amount_minor (captured) decodes as int64 minor units")
	require.Equal(t, int64(500000), send.ExpectedAmountMinor, "W51 delta: expected_amount_minor decodes as int64 minor units")
	require.Equal(t, "NGN", send.Currency)
	require.Equal(t, "BK-7XQ4V2PZ", send.BookingRef, "human public ref rides through")
	require.Equal(t, "2030-01-01T10:00:00Z", send.PaidAt)
	require.Empty(t, f.got, "no booking-scoped signals for a mismatch event")
}

// The bare (unprefixed) event type is accepted too, mirroring the
// BookingPaymentConfirmed tolerance.
func TestBookingPaymentAmountMismatchBareTypeAccepted(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	evt := []byte(`{"type":"BookingPaymentAmountMismatch","subject":"acme","tenantid":"t-1",
	  "data":{"booking_id":"b-1","tenant_id":"t-1","tx_ref":"tx_bare_mm","amount_minor":99,"expected_amount_minor":100,"currency":"USD"}}`)
	require.NoError(t, b.Process(context.Background(), evt))
	require.Equal(t, 1, st.calls)
	require.Equal(t, "payment-mismatch-tx_bare_mm", st.options.ID)
	send := st.args[0].(workflows.PacedPaymentMismatchSend)
	require.Equal(t, int64(99), send.AmountMinor)
	require.Equal(t, int64(100), send.ExpectedAmountMinor)
}

// OPS-only guarantee: the event carries NO customer contact fields and the
// alert still starts — there is no reachable-contact gate (unlike the
// customer-facing payment receipt).
func TestBookingPaymentAmountMismatchNoContactRequired(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	evt := []byte(`{"type":"com.opendesk.booking.BookingPaymentAmountMismatch","subject":"acme","tenantid":"t-1",
	  "data":{"booking_id":"b-1","tx_ref":"tx_nocontact","amount_minor":1,"expected_amount_minor":2,"currency":"USD"}}`)
	require.NoError(t, b.Process(context.Background(), evt))
	require.Equal(t, 1, st.calls, "no customer contact needed for an ops alert")
}

// Envelope defaults: tenant_id falls back to the CloudEvents tenantid
// extension; the slug always comes from the subject.
func TestBookingPaymentAmountMismatchEnvelopeDefaults(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	evt := []byte(`{"type":"com.opendesk.booking.BookingPaymentAmountMismatch","subject":"acme","tenantid":"t-9",
	  "data":{"booking_id":"b-2","tx_ref":"tx_env_mm","amount_minor":100,"expected_amount_minor":200,"currency":"USD"}}`)
	require.NoError(t, b.Process(context.Background(), evt))
	require.Equal(t, 1, st.calls)
	send := st.args[0].(workflows.PacedPaymentMismatchSend)
	require.Equal(t, "t-9", send.TenantID)
	require.Equal(t, "acme", send.TenantSlug)
}

// Fail-closed: missing booking_id/tx_ref or a malformed payload → warn+ack
// without any workflow start (an alert without reconciliation anchors must
// never fire).
func TestBookingPaymentAmountMismatchFailClosed(t *testing.T) {
	for name, data := range map[string]string{
		"missing booking_id":        `{"tx_ref":"tx-1","amount_minor":1,"expected_amount_minor":2}`,
		"missing tx_ref":            `{"booking_id":"b-1","amount_minor":1,"expected_amount_minor":2}`,
		"empty data":                `{}`,
		"malformed amount":          `{"booking_id":"b-1","tx_ref":"tx-1","amount_minor":"lots"}`,
		"malformed expected amount": `{"booking_id":"b-1","tx_ref":"tx-1","expected_amount_minor":"lots"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeSignaller{}
			st := &fakeStarter{}
			b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
			evt := []byte(`{"type":"com.opendesk.booking.BookingPaymentAmountMismatch","subject":"acme","data":` + data + `}`)
			require.NoError(t, b.Process(context.Background(), evt))
			require.Equal(t, 0, st.calls, "no workflow start on malformed/incomplete payload")
			require.Empty(t, f.got)
		})
	}
}

// Nil starter keeps the old posture: acknowledged, no panic.
func TestBookingPaymentAmountMismatchNilStarter(t *testing.T) {
	f := &fakeSignaller{}
	b := newBridge(f)
	require.NoError(t, b.Process(context.Background(), []byte(amountMismatchEvent)))
	require.Empty(t, f.got)
}

// Redelivery dedupe: WorkflowExecutionAlreadyStarted is acknowledged, not
// retried — a Kafka redelivery can never double-alert.
func TestBookingPaymentAmountMismatchAlreadyStartedIsAcked(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{err: &serviceerror.WorkflowExecutionAlreadyStarted{}}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	require.NoError(t, b.Process(context.Background(), []byte(amountMismatchEvent)))
	require.Equal(t, 1, st.calls)
}

// A replayed identical event (Kafka redelivery) resolves to the same
// deterministic workflow ID and is acknowledged on AlreadyStarted: exactly
// one effective workflow for any number of deliveries.
func TestBookingPaymentAmountMismatchReplayDeduplicates(t *testing.T) {
	f := &fakeSignaller{}
	st := &dedupeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	require.NoError(t, b.Process(context.Background(), []byte(amountMismatchEvent)))
	require.NoError(t, b.Process(context.Background(), []byte(amountMismatchEvent)))
	require.NoError(t, b.Process(context.Background(), []byte(amountMismatchEvent)))
	require.Equal(t, 3, st.calls, "every delivery attempts a start")
	require.Len(t, st.seen, 1, "all deliveries resolve to ONE workflow ID")
	require.Equal(t, 3, st.seen["payment-mismatch-tx_mm789"])
}
