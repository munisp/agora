package signals

// SPEC-W48 (A2/G) bridge tests: the staff-triggered WaitlistEntryNotified
// event (no booking_id) must start WaitlistNotifyWorkflow with the drop-in
// PacedWaitlistSend payload; WaitlistEntryExpired is acknowledged as a
// no-op; BookingCancelled must not double-send via the notify path.

import (
	"context"
	"testing"

	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"
	"go.uber.org/zap"
)

// waitlistNotifiedEvent is the exact CloudEvent shape booking-service
// emits (bookingops.waitlistNotifyData): input + entry + claim_path, no
// booking_id.
const waitlistNotifiedEvent = `{
  "type": "com.opendesk.booking.WaitlistEntryNotified",
  "subject": "acme",
  "tenantid": "t-1",
  "data": {
    "input": {"tenant_id": "t-1", "tenant_slug": "acme", "offering_id": "o-1"},
    "entry": {
      "id": "e-1", "offering_id": "o-1", "contact_name": "Ada",
      "contact_phone": "+234800", "window_start": "2030-01-01T10:00:00Z",
      "window_end": "2030-01-01T11:00:00Z", "status": "notified",
      "claim_token": "tok-e1"
    },
    "claim_path": "/p/acme/claim?entry=e-1&token=tok-e1"
  }
}`

// The happy path: exactly one WaitlistNotifyWorkflow start, deterministic
// dedupe ID, drop-in payload decoded into PacedWaitlistSend.
func TestWaitlistEntryNotifiedStartsNotifyWorkflow(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	require.NoError(t, b.Process(context.Background(), []byte(waitlistNotifiedEvent)))

	require.Equal(t, 1, st.calls)
	require.Equal(t, "waitlist-notify-e-1", st.options.ID, "dedupe ID derived from the entry")
	require.Equal(t, "q", st.options.TaskQueue)
	require.Equal(t, "WaitlistNotifyWorkflow", st.workflowType)
	require.Len(t, st.args, 1)
	send, ok := st.args[0].(workflows.PacedWaitlistSend)
	require.True(t, ok, "arg is a PacedWaitlistSend, got %T", st.args[0])
	require.Equal(t, "e-1", send.Entry.ID)
	require.Equal(t, "tok-e1", send.Entry.ClaimToken, "claim_token rides the drop-in payload")
	require.Equal(t, "+234800", send.Entry.ContactPhone, "customer channel rides the drop-in payload")
	require.Equal(t, "acme", send.Input.TenantSlug)
	require.Equal(t, "t-1", send.Input.TenantID)
	require.Equal(t, "o-1", send.Input.OfferingID)
	require.Empty(t, f.got, "no booking-scoped signals for an entry-scoped event")
}

// Envelope addressing fills payload gaps (input omitted).
func TestWaitlistEntryNotifiedEnvelopeDefaults(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	evt := []byte(`{"type":"com.opendesk.booking.WaitlistEntryNotified","subject":"acme","tenantid":"t-1",
	  "data":{"entry":{"id":"e-2","offering_id":"o-9","contact_name":"Ada","contact_phone":"+1","claim_token":"tok"}}}`)
	require.NoError(t, b.Process(context.Background(), evt))
	require.Equal(t, 1, st.calls)
	send := st.args[0].(workflows.PacedWaitlistSend)
	require.Equal(t, "acme", send.Input.TenantSlug)
	require.Equal(t, "t-1", send.Input.TenantID)
	require.Equal(t, "o-9", send.Input.OfferingID, "offering falls back to the entry's")
}

// Fail-closed: no claim token / no customer channel → acknowledged without
// any workflow start (never send a customer-facing message on bad data).
func TestWaitlistEntryNotifiedFailClosed(t *testing.T) {
	for name, data := range map[string]string{
		"missing claim_token": `{"input":{"tenant_slug":"acme"},"entry":{"id":"e-1","contact_phone":"+1"}}`,
		"missing phone":       `{"input":{"tenant_slug":"acme"},"entry":{"id":"e-1","claim_token":"tok"}}`,
		"missing entry id":    `{"input":{"tenant_slug":"acme"},"entry":{"contact_phone":"+1","claim_token":"tok"}}`,
		"empty data":          `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeSignaller{}
			st := &fakeStarter{}
			b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
			evt := []byte(`{"type":"com.opendesk.booking.WaitlistEntryNotified","subject":"acme","data":` + data + `}`)
			require.NoError(t, b.Process(context.Background(), evt))
			require.Equal(t, 0, st.calls, "no send on malformed/incomplete payload")
			require.Empty(t, f.got)
		})
	}
}

// Nil starter keeps the old posture: acknowledged, no panic.
func TestWaitlistEntryNotifiedNilStarter(t *testing.T) {
	f := &fakeSignaller{}
	b := newBridge(f)
	require.NoError(t, b.Process(context.Background(), []byte(waitlistNotifiedEvent)))
	require.Empty(t, f.got)
}

// Redelivery dedupe: WorkflowExecutionAlreadyStarted is acknowledged, not
// retried — a Kafka redelivery can never double-send.
func TestWaitlistEntryNotifiedAlreadyStartedIsAcked(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{err: &serviceerror.WorkflowExecutionAlreadyStarted{}}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	require.NoError(t, b.Process(context.Background(), []byte(waitlistNotifiedEvent)))
	require.Equal(t, 1, st.calls)
}

// WaitlistEntryExpired is audit-only: acknowledged, no signals, no starts
// (paced sends are single-shot — nothing pending to stop).
func TestWaitlistEntryExpiredIsAcknowledged(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	evt := []byte(`{"type":"com.opendesk.booking.WaitlistEntryExpired","subject":"acme","tenantid":"t-1",
	  "data":{"entry_id":"e-1","offering_id":"o-1","actor":"waitlist-sweeper","via":"sweep"}}`)
	require.NoError(t, b.Process(context.Background(), evt))
	require.Equal(t, 0, st.calls)
	require.Empty(t, f.got)
}

// No double-send: BookingCancelled starts the BACKFILL workflow only — it
// must never start the single-entry notify workflow.
func TestBookingCancelledDoesNotStartNotify(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	evt := []byte(`{"type":"com.opendesk.booking.BookingCancelled","subject":"acme","tenantid":"t-1","data":{"booking_id":"b-7","offering_id":"o-1"}}`)
	require.NoError(t, b.Process(context.Background(), evt))
	require.Equal(t, 1, st.calls, "exactly one workflow start")
	require.Equal(t, "WaitlistBackfillWorkflow", st.workflowType, "backfill only — never the single-entry notify")
}
