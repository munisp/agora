package signals

// SPEC-W50-B bridge tests: com.opendesk.booking.BookingPaymentConfirmed
// must start PaymentReceiptWorkflow with the sacred-contract payload and
// the deterministic dedupe ID payment-receipt-<tx_ref>; malformed or
// unreachable-contact events fail closed (ack, no start); redeliveries
// dedupe on WorkflowExecutionAlreadyStarted.

import (
	"context"
	"sync"
	"testing"

	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// paymentConfirmedEvent is the exact CloudEvent shape booking-service emits
// (sacred W50 cross-scope contract): booking_id, tenant_id, tx_ref,
// amount_minor (int64), currency, customer_name, contact_phone,
// contact_email, booking_ref, paid_at.
const paymentConfirmedEvent = `{
  "type": "com.opendesk.booking.BookingPaymentConfirmed",
  "subject": "acme",
  "tenantid": "t-1",
  "data": {
    "booking_id": "b-42",
    "tenant_id": "t-1",
    "tx_ref": "tx_abc123",
    "amount_minor": 500000,
    "currency": "NGN",
    "customer_name": "Ada",
    "contact_phone": "+2348012345678",
    "contact_email": "ada@example.com",
    "booking_ref": "BK-1042",
    "paid_at": "2030-01-01T10:00:00Z"
  }
}`

// Happy path: exactly one PaymentReceiptWorkflow start, deterministic
// dedupe ID derived from tx_ref, payload decoded field-by-field.
func TestBookingPaymentConfirmedStartsReceiptWorkflow(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	require.NoError(t, b.Process(context.Background(), []byte(paymentConfirmedEvent)))

	require.Equal(t, 1, st.calls)
	require.Equal(t, "payment-receipt-tx_abc123", st.options.ID, "dedupe ID derived from tx_ref")
	require.Equal(t, "q", st.options.TaskQueue)
	require.Equal(t, "PaymentReceiptWorkflow", st.workflowType)
	require.Len(t, st.args, 1)
	send, ok := st.args[0].(workflows.PacedPaymentReceiptSend)
	require.True(t, ok, "arg is a PacedPaymentReceiptSend, got %T", st.args[0])
	require.Equal(t, "b-42", send.BookingID)
	require.Equal(t, "t-1", send.TenantID)
	require.Equal(t, "acme", send.TenantSlug, "tenant slug rides the envelope subject")
	require.Equal(t, "tx_abc123", send.TxRef)
	require.Equal(t, int64(500000), send.AmountMinor, "amount_minor decodes as int64 minor units")
	require.Equal(t, "NGN", send.Currency)
	require.Equal(t, "Ada", send.CustomerName)
	require.Equal(t, "+2348012345678", send.ContactPhone)
	require.Equal(t, "ada@example.com", send.ContactEmail)
	require.Equal(t, "BK-1042", send.BookingRef)
	require.Empty(t, f.got, "no booking-scoped signals for a payment event")
}

// The bare (unprefixed) event type is accepted too, mirroring eventmail's
// identity/billing tolerance.
func TestBookingPaymentConfirmedBareTypeAccepted(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	evt := []byte(`{"type":"BookingPaymentConfirmed","subject":"acme","tenantid":"t-1",
	  "data":{"booking_id":"b-1","tenant_id":"t-1","tx_ref":"tx_bare","amount_minor":99,"currency":"USD","contact_email":"a@b.c"}}`)
	require.NoError(t, b.Process(context.Background(), evt))
	require.Equal(t, 1, st.calls)
	require.Equal(t, "payment-receipt-tx_bare", st.options.ID)
	send := st.args[0].(workflows.PacedPaymentReceiptSend)
	require.Equal(t, int64(99), send.AmountMinor)
	require.Equal(t, "a@b.c", send.ContactEmail, "email-only contact is reachable")
}

// Envelope defaults: tenant_id falls back to the CloudEvents tenantid
// extension; the slug always comes from the subject.
func TestBookingPaymentConfirmedEnvelopeDefaults(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	evt := []byte(`{"type":"com.opendesk.booking.BookingPaymentConfirmed","subject":"acme","tenantid":"t-9",
	  "data":{"booking_id":"b-2","tx_ref":"tx_env","amount_minor":100,"currency":"USD","contact_phone":"+1"}}`)
	require.NoError(t, b.Process(context.Background(), evt))
	require.Equal(t, 1, st.calls)
	send := st.args[0].(workflows.PacedPaymentReceiptSend)
	require.Equal(t, "t-9", send.TenantID)
	require.Equal(t, "acme", send.TenantSlug)
}

// Fail-closed: missing booking_id/tx_ref, or BOTH contacts empty, or a
// malformed payload → acknowledged without any workflow start (a receipt
// must never be sent on bad data, and no send is possible without a
// contact).
func TestBookingPaymentConfirmedFailClosed(t *testing.T) {
	for name, data := range map[string]string{
		"missing booking_id": `{"tx_ref":"tx-1","contact_phone":"+1"}`,
		"missing tx_ref":     `{"booking_id":"b-1","contact_phone":"+1"}`,
		"both contacts empty": `{"booking_id":"b-1","tx_ref":"tx-1"}`,
		"both contacts explicitly empty": `{"booking_id":"b-1","tx_ref":"tx-1","contact_phone":"","contact_email":""}`,
		"empty data": `{}`,
		"malformed amount": `{"booking_id":"b-1","tx_ref":"tx-1","contact_phone":"+1","amount_minor":"lots"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeSignaller{}
			st := &fakeStarter{}
			b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
			evt := []byte(`{"type":"com.opendesk.booking.BookingPaymentConfirmed","subject":"acme","data":` + data + `}`)
			require.NoError(t, b.Process(context.Background(), evt))
			require.Equal(t, 0, st.calls, "no workflow start on malformed/incomplete payload")
			require.Empty(t, f.got)
		})
	}
}

// Nil starter keeps the old posture: acknowledged, no panic.
func TestBookingPaymentConfirmedNilStarter(t *testing.T) {
	f := &fakeSignaller{}
	b := newBridge(f)
	require.NoError(t, b.Process(context.Background(), []byte(paymentConfirmedEvent)))
	require.Empty(t, f.got)
}

// Redelivery dedupe: WorkflowExecutionAlreadyStarted is acknowledged, not
// retried — a Kafka redelivery can never double-send a receipt.
func TestBookingPaymentConfirmedAlreadyStartedIsAcked(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{err: &serviceerror.WorkflowExecutionAlreadyStarted{}}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	require.NoError(t, b.Process(context.Background(), []byte(paymentConfirmedEvent)))
	require.Equal(t, 1, st.calls)
}

// dedupeStarter fails the second start of the same workflow ID with
// WorkflowExecutionAlreadyStarted, emulating Temporal's dedupe.
type dedupeStarter struct {
	mu    sync.Mutex
	seen  map[string]int
	calls int
}

func (d *dedupeStarter) ExecuteWorkflow(_ context.Context, options client.StartWorkflowOptions, _ interface{}, _ ...interface{}) (client.WorkflowRun, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.seen == nil {
		d.seen = map[string]int{}
	}
	d.seen[options.ID]++
	if d.seen[options.ID] > 1 {
		return nil, &serviceerror.WorkflowExecutionAlreadyStarted{}
	}
	return nil, nil
}

// A replayed identical event (Kafka redelivery) resolves to the same
// deterministic workflow ID and is acknowledged on AlreadyStarted: exactly
// one effective workflow for any number of deliveries.
func TestBookingPaymentConfirmedReplayDeduplicates(t *testing.T) {
	f := &fakeSignaller{}
	st := &dedupeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	require.NoError(t, b.Process(context.Background(), []byte(paymentConfirmedEvent)))
	require.NoError(t, b.Process(context.Background(), []byte(paymentConfirmedEvent)))
	require.NoError(t, b.Process(context.Background(), []byte(paymentConfirmedEvent)))
	require.Equal(t, 3, st.calls, "every delivery attempts a start")
	require.Len(t, st.seen, 1, "all deliveries resolve to ONE workflow ID")
	require.Equal(t, 3, st.seen["payment-receipt-tx_abc123"])
}
