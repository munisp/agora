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

// SPEC-W51-A: PaymentMismatchAlertWorkflow raises exactly one paced ops
// alert (PacedSendOpsAlert → NotifyPaced) per BookingPaymentAmountMismatch
// event — OPS/STAFF routing only, never the customer.

func paymentMismatchTestSend() PacedPaymentMismatchSend {
	return PacedPaymentMismatchSend{
		BookingID:           "b-42",
		TenantID:            "t-1",
		TenantSlug:          "acme",
		TxRef:               "tx_mm789",
		AmountMinor:         450000,
		ExpectedAmountMinor: 500000,
		Currency:            "NGN",
		BookingRef:          "BK-7XQ4V2PZ",
		PaidAt:              "2030-01-01T10:00:00Z",
	}
}

// Happy path: exactly one NotifyPaced with the ops_alert kind and the full
// sacred-contract payload (incl. expected_amount_minor) riding through.
func TestPaymentMismatchAlertWorkflowSendsOnce(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(func(ctx context.Context, req PacedSendRequest) error {
		return nil
	}, activity.RegisterOptions{Name: ActivityNotifyPaced})
	send := paymentMismatchTestSend()

	notified := 0
	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(PacedSendRequest)
			require.Equal(t, PacedSendOpsAlert, req.Kind)
			require.NotNil(t, req.PaymentMismatch)
			require.Equal(t, send, *req.PaymentMismatch, "the payload rides through verbatim")
			require.Nil(t, req.PaymentReceipt, "an ops alert is never a customer receipt send")
			notified++
		}).Return(nil).Once()

	env.ExecuteWorkflow(PaymentMismatchAlertWorkflow, send)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 1, notified, "exactly one paced ops alert per event")
	env.AssertExpectations(t)
}

// A failed alert is logged, not fatal (mirrors the payment receipt): the
// mismatch is recorded in booking-service regardless of alert delivery, so
// the workflow completes successfully.
func TestPaymentMismatchAlertWorkflowSendFailureNotFatal(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(func(ctx context.Context, req PacedSendRequest) error {
		return nil
	}, activity.RegisterOptions{Name: ActivityNotifyPaced})

	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Return(errors.New("kafka down")).Once()

	env.ExecuteWorkflow(PaymentMismatchAlertWorkflow, paymentMismatchTestSend())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}
