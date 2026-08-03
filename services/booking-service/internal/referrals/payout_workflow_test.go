package referrals

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// payoutTestEnv mirrors the geo workflow test harness: stub activities
// registered under their production names.
type payoutTestEnv struct {
	env          *testsuite.TestWorkflowEnvironment
	transferErr  error // non-nil → every transfer attempt fails
	paid         *FinalizeActivityInput
	failed       *FailActivityInput
	transferRuns int
}

func newPayoutTestEnv(t *testing.T) *payoutTestEnv {
	t.Helper()
	p := &payoutTestEnv{}
	p.env = (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	p.env.RegisterActivityWithOptions(func(ctx context.Context, in TransferActivityInput) (TransferResult, error) {
		p.transferRuns++
		if p.transferErr != nil {
			return TransferResult{}, p.transferErr
		}
		return TransferResult{ProviderRef: "cpay_test_ref", Status: "success"}, nil
	}, activity.RegisterOptions{Name: ActivityPayoutTransfer})

	p.env.RegisterActivityWithOptions(func(ctx context.Context, in FinalizeActivityInput) error {
		p.paid = &in
		return nil
	}, activity.RegisterOptions{Name: ActivityPayoutMarkPaid})

	p.env.RegisterActivityWithOptions(func(ctx context.Context, in FailActivityInput) error {
		p.failed = &in
		return nil
	}, activity.RegisterOptions{Name: ActivityPayoutMarkFailed})
	return p
}

func (p *payoutTestEnv) input() PayoutInput {
	return PayoutInput{TenantID: uuid.NewString(), TenantSlug: "acme", PayoutID: uuid.NewString()}
}

// Happy path: transfer → finalize paid with the provider ref; never failed.
func TestCommissionPayoutWorkflowSuccess(t *testing.T) {
	p := newPayoutTestEnv(t)
	in := p.input()
	p.env.ExecuteWorkflow(CommissionPayoutWorkflow, in)
	require.True(t, p.env.IsWorkflowCompleted())
	require.NoError(t, p.env.GetWorkflowError())
	require.Equal(t, 1, p.transferRuns)
	require.NotNil(t, p.paid, "finalize-paid activity must run")
	require.Equal(t, in.PayoutID, p.paid.PayoutID)
	require.Equal(t, "cpay_test_ref", p.paid.ProviderRef)
	require.Nil(t, p.failed, "mark-failed must not run on success")
}

// Contract §4: transfer activity retried 3× (backoff) then the payout is
// marked failed with the reason; the workflow surfaces the error.
func TestCommissionPayoutWorkflowFailsAfterRetries(t *testing.T) {
	p := newPayoutTestEnv(t)
	p.transferErr = errors.New("provider 502: bad gateway")
	in := p.input()
	p.env.ExecuteWorkflow(CommissionPayoutWorkflow, in)
	require.True(t, p.env.IsWorkflowCompleted())
	require.ErrorContains(t, p.env.GetWorkflowError(), "commission payout transfer")
	require.Equal(t, 3, p.transferRuns, "3 attempts per contract §4 retry policy")
	require.NotNil(t, p.failed, "mark-failed activity must run after retries")
	require.Equal(t, in.PayoutID, p.failed.PayoutID)
	require.Contains(t, p.failed.Reason, "bad gateway")
	require.Nil(t, p.paid, "finalize-paid must not run on failure")
}

// A finalize failure surfaces the error (recon catches the divergence).
func TestCommissionPayoutWorkflowFinalizeError(t *testing.T) {
	p := newPayoutTestEnv(t)
	p.env.OnActivity(ActivityPayoutMarkPaid, mock.Anything, mock.Anything).
		Return(errors.New("ledger posting: connection reset"))
	in := p.input()
	p.env.ExecuteWorkflow(CommissionPayoutWorkflow, in)
	require.True(t, p.env.IsWorkflowCompleted())
	require.ErrorContains(t, p.env.GetWorkflowError(), "commission payout finalize")
	require.Nil(t, p.failed, "a paid-at-provider payout must NOT be marked failed")
}
