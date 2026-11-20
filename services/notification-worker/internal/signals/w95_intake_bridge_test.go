package signals

// SPEC-W95 tests: the IntakeCompleted cloudevent (emitted by
// booking-service's public intake submit) signals the waiting
// ClinicIntakeWorkflow pack-{bookingId}. Best-effort ack semantics are
// unchanged — events without a booking_id signal nothing.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcessIntakeCompletedSignalsPack_W95(t *testing.T) {
	f := &fakeSignaller{}
	b := newBridge(f)
	evt := []byte(`{"type":"com.opendesk.booking.IntakeCompleted","subject":"acme","tenantid":"t-1","data":{"booking_id":"b-7"}}`)
	require.NoError(t, b.Process(context.Background(), evt))
	require.Equal(t, []recordedSignal{
		{workflowID: "pack-b-7", signal: "IntakeCompleted"},
	}, f.got)
}

func TestProcessIntakeCompletedWithoutBookingID_W95(t *testing.T) {
	f := &fakeSignaller{}
	b := newBridge(f)
	require.NoError(t, b.Process(context.Background(),
		[]byte(`{"type":"com.opendesk.booking.IntakeCompleted","data":{}}`)))
	require.Empty(t, f.got)
}
