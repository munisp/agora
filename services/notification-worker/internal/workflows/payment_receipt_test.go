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

// SPEC-W50-B: PaymentReceiptWorkflow delivers exactly one paced receipt
// (PacedSendPaymentReceipt → NotifyPaced) per BookingPaymentConfirmed event.

func paymentReceiptTestSend() PacedPaymentReceiptSend {
	return PacedPaymentReceiptSend{
		BookingID:    "b-42",
		TenantID:     "t-1",
		TenantSlug:   "acme",
		TxRef:        "tx_abc123",
		AmountMinor:  500000,
		Currency:     "NGN",
		CustomerName: "Ada",
		ContactPhone: "+2348012345678",
		ContactEmail: "ada@example.com",
		BookingRef:   "BK-1042",
	}
}

// Happy path: exactly one NotifyPaced with the payment_receipt kind and
// the full sacred-contract payload riding through.
func TestPaymentReceiptWorkflowSendsOnce(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(func(ctx context.Context, req PacedSendRequest) error {
		return nil
	}, activity.RegisterOptions{Name: ActivityNotifyPaced})
	send := paymentReceiptTestSend()

	notified := 0
	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(PacedSendRequest)
			require.Equal(t, PacedSendPaymentReceipt, req.Kind)
			require.NotNil(t, req.PaymentReceipt)
			require.Equal(t, send, *req.PaymentReceipt, "the payload rides through verbatim")
			notified++
		}).Return(nil).Once()

	env.ExecuteWorkflow(PaymentReceiptWorkflow, send)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 1, notified, "exactly one paced receipt send per event")
	env.AssertExpectations(t)
}

// A failed send is logged, not fatal (mirrors the waitlist notify): the
// payment is recorded regardless of notification delivery, so the workflow
// completes successfully.
func TestPaymentReceiptWorkflowSendFailureNotFatal(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(func(ctx context.Context, req PacedSendRequest) error {
		return nil
	}, activity.RegisterOptions{Name: ActivityNotifyPaced})

	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Return(errors.New("twilio down")).Once()

	env.ExecuteWorkflow(PaymentReceiptWorkflow, paymentReceiptTestSend())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}
