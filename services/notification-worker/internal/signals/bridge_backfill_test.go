package signals

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// fakeStarter records ExecuteWorkflow calls. err, when set, is returned
// (e.g. WorkflowExecutionAlreadyStarted for the redelivery-dedupe tests).
type fakeStarter struct {
	workflowType interface{}
	args         []interface{}
	options      client.StartWorkflowOptions
	calls        int
	err          error
}

func (f *fakeStarter) ExecuteWorkflow(_ context.Context, options client.StartWorkflowOptions, workflowType interface{}, args ...interface{}) (client.WorkflowRun, error) {
	f.calls++
	f.options = options
	f.workflowType = workflowType
	f.args = args
	return nil, f.err
}

func TestBookingCancelledStartsWaitlistBackfill(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	evt := []byte(`{"type":"com.opendesk.booking.BookingCancelled","subject":"acme","tenantid":"t-1","data":{"booking_id":"b-7","offering_id":"o-1"}}`)
	require.NoError(t, b.Process(context.Background(), evt))
	require.Equal(t, 1, st.calls)
	require.Equal(t, "waitlist-backfill-b-7", st.options.ID)
	require.Equal(t, "q", st.options.TaskQueue)
	require.Equal(t, "WaitlistBackfillWorkflow", st.workflowType)
}

func TestBookingCancelledWithoutOfferingSkipsBackfill(t *testing.T) {
	f := &fakeSignaller{}
	st := &fakeStarter{}
	b := New([]string{"localhost:9092"}, "topic", "group", f, zap.NewNop(), WithBackfillStarter(st, "q"))
	evt := []byte(`{"type":"com.opendesk.booking.BookingCancelled","subject":"acme","data":{"booking_id":"b-8"}}`)
	require.NoError(t, b.Process(context.Background(), evt))
	require.Equal(t, 0, st.calls)
	// signals to pack/reminder still delivered
	require.Len(t, f.got, 2)
}

func TestNilStarterKeepsOldBehavior(t *testing.T) {
	f := &fakeSignaller{}
	b := newBridge(f) // no starter configured
	evt := []byte(`{"type":"com.opendesk.booking.BookingCancelled","subject":"acme","data":{"booking_id":"b-9","offering_id":"o-1"}}`)
	require.NoError(t, b.Process(context.Background(), evt))
	require.Len(t, f.got, 2)
}
