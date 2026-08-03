package referrals

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// reconTestEnv stubs the two recon activities under production names.
type reconTestEnv struct {
	env         *testsuite.TestWorkflowEnvironment
	candidates  []Payout
	checkErrs   map[string]error // payout_id → activity error
	checkCalls  []string
	fetchCalled bool
}

func newReconTestEnv(t *testing.T, mismatches map[string]string) *reconTestEnv {
	t.Helper()
	r := &reconTestEnv{checkErrs: map[string]error{}}
	r.env = (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	r.env.RegisterActivityWithOptions(func(ctx context.Context, in ReconFetchInput) ([]Payout, error) {
		r.fetchCalled = true
		if in.Limit > 0 && len(r.candidates) > in.Limit {
			return r.candidates[:in.Limit], nil
		}
		return r.candidates, nil
	}, activity.RegisterOptions{Name: ActivityReconFetch})

	r.env.RegisterActivityWithOptions(func(ctx context.Context, in ReconCheckInput) (*ReconMismatch, error) {
		r.checkCalls = append(r.checkCalls, in.PayoutID)
		if err := r.checkErrs[in.PayoutID]; err != nil {
			return nil, err
		}
		if kind := mismatches[in.PayoutID]; kind != "" {
			return &ReconMismatch{TenantID: in.TenantID, PayoutID: in.PayoutID, Mismatch: kind}, nil
		}
		return nil, nil
	}, activity.RegisterOptions{Name: ActivityReconCheck})
	return r
}

func reconCandidate() Payout {
	return Payout{
		ID:          uuid.New(),
		TenantID:    uuid.New(),
		Status:      PayoutStatusPaid,
		Provider:    ProviderPaystack,
		ProviderRef: "cpay_x",
	}
}

// Contract §5: candidates are scanned one activity per payout; clean
// payouts produce nothing, mismatches are surfaced (the activity itself
// wrote the alert + meter rows — covered by the stub contract).
func TestCommissionReconWorkflowScansCandidates(t *testing.T) {
	c1, c2, c3 := reconCandidate(), reconCandidate(), reconCandidate()
	r := newReconTestEnv(t, map[string]string{c2.ID.String(): MismatchPaidNotSuccessful})
	r.candidates = []Payout{c1, c2, c3}

	r.env.ExecuteWorkflow(CommissionReconWorkflow, ReconInput{})
	require.True(t, r.env.IsWorkflowCompleted())
	require.NoError(t, r.env.GetWorkflowError())
	require.True(t, r.fetchCalled)
	require.Len(t, r.checkCalls, 3, "every candidate gets its own check activity")
	require.Contains(t, r.checkCalls, c1.ID.String())
	require.Contains(t, r.checkCalls, c2.ID.String())
	require.Contains(t, r.checkCalls, c3.ID.String())
}

// A failing check activity must not abort the nightly run.
func TestCommissionReconWorkflowToleratesCheckFailure(t *testing.T) {
	c1, c2 := reconCandidate(), reconCandidate()
	r := newReconTestEnv(t, map[string]string{c2.ID.String(): MismatchProcessingSucceeded})
	r.candidates = []Payout{c1, c2}
	r.checkErrs[c1.ID.String()] = errors.New("provider timeout")

	r.env.ExecuteWorkflow(CommissionReconWorkflow, ReconInput{})
	require.True(t, r.env.IsWorkflowCompleted())
	require.NoError(t, r.env.GetWorkflowError(), "one failed check must not fail the run")
	require.Len(t, r.checkCalls, 4, "c1 retried 3× (activity retry policy) + c2 once")
}

// Empty candidate set: fetch-only run.
func TestCommissionReconWorkflowNoCandidates(t *testing.T) {
	r := newReconTestEnv(t, nil)
	r.env.ExecuteWorkflow(CommissionReconWorkflow, ReconInput{Limit: 50})
	require.True(t, r.env.IsWorkflowCompleted())
	require.NoError(t, r.env.GetWorkflowError())
	require.True(t, r.fetchCalled)
	require.Empty(t, r.checkCalls)
}
