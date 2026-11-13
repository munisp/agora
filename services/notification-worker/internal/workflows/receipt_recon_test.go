package workflows

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// SPEC-W56-B: ReceiptReconAlertWorkflow raises exactly one ops alert
// (SendReceiptReconAlert) per ReceiptReconFinding event — OPS/STAFF routing
// only, never the customer.

func receiptReconTestSend() ReceiptReconAlertSend {
	return ReceiptReconAlertSend{
		FindingID:           "f-123",
		TenantID:            "t-1",
		TenantSlug:          "acme",
		Kind:                "amount_mismatch",
		Confidence:          "exact",
		CaptureClientID:     "cap-9",
		BookingID:           "b-42",
		AttemptTxRef:        "tx_abc",
		ExpectedAmountMinor: 500000,
		CapturedAmountMinor: 450000,
		ReceiptAmountMajor:  4500.5,
		Detail:              map[string]any{"currency_unknown": true},
	}
}

// Happy path: exactly one SendReceiptReconAlert with the full W56
// cross-scope contract payload riding through verbatim.
func TestReceiptReconAlertWorkflowSendsOnce(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(func(ctx context.Context, send ReceiptReconAlertSend) error {
		return nil
	}, activity.RegisterOptions{Name: ActivitySendReceiptReconAlert})
	send := receiptReconTestSend()

	sent := 0
	env.OnActivity(ActivitySendReceiptReconAlert, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			got := args.Get(1).(ReceiptReconAlertSend)
			require.Equal(t, send, got, "the payload rides through verbatim")
			sent++
		}).Return(nil).Once()

	env.ExecuteWorkflow(ReceiptReconAlertWorkflow, send)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 1, sent, "exactly one ops alert per finding")
	env.AssertExpectations(t)
}

// A failed alert is logged, not fatal (mirrors the payment mismatch): the
// finding is persisted in booking-service regardless of alert delivery, so
// the workflow completes successfully.
func TestReceiptReconAlertWorkflowSendFailureNotFatal(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(func(ctx context.Context, send ReceiptReconAlertSend) error {
		return nil
	}, activity.RegisterOptions{Name: ActivitySendReceiptReconAlert})

	env.OnActivity(ActivitySendReceiptReconAlert, mock.Anything, mock.Anything).
		Return(errors.New("kafka down")).Once()

	env.ExecuteWorkflow(ReceiptReconAlertWorkflow, receiptReconTestSend())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}
