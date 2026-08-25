package referrals

import (
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Nightly commission reconciliation (SPEC-W14 §5). A Temporal Schedule
// (commission-recon-nightly, "30 2 * * *" Africa/Lagos — bootstrap in
// temporalclient.EnsureCommissionReconSchedule via main.go's additive
// block) fires CommissionReconWorkflow, which compares ledger payout
// statuses against the provider's transfer statuses (mockable provider
// client). Every mismatch produces:
//
//   - an outbox alert row (kind commission_recon_alert →
//     com.opendesk.notifications.CommissionReconAlert on the notifications
//     outbox topic), and
//   - a metered usage row (commission_recon_alert on opendesk.usage.events).
const (
	// WorkflowTypeRecon is the registered name of the recon workflow.
	WorkflowTypeRecon = "CommissionReconWorkflow"

	// ReconScheduleID is the Temporal Schedule ID (contract §5).
	ReconScheduleID = "commission-recon-nightly"

	// Activity names (registered on booking-service's worker).
	ActivityReconFetch = "CommissionReconFetchCandidates"
	ActivityReconCheck = "CommissionReconCheckTransfer"

	// DefaultReconLimit bounds one nightly scan.
	DefaultReconLimit = 200
)

// ReconInput starts one CommissionReconWorkflow. The schedule fires it
// with the zero value; Limit overrides the candidate bound (ops/manual
// runs).
type ReconInput struct {
	Limit int `json:"limit,omitempty"`
}

// ReconResult summarizes one nightly run (workflow return value, visible
// in the Temporal UI / schedule history).
type ReconResult struct {
	Checked    int      `json:"checked"`
	Mismatched int      `json:"mismatched"`
	PayoutIDs  []string `json:"payout_ids,omitempty"`
	// Fed counts payouts queued by the feed step (SPEC-W44 W-B/S1-F7-08).
	Fed int `json:"fed"`
}

// CommissionReconWorkflow fans the candidate scan out into one check
// activity per payout (small, individually retried). A failing check does
// not abort the run — recon must be maximally self-healing; the failure is
// logged and the payout is retried by the next nightly run.
func CommissionReconWorkflow(ctx workflow.Context, in ReconInput) error {
	logger := workflow.GetLogger(ctx)
	ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    15 * time.Second,
			MaximumAttempts:    3,
		},
	})

	limit := in.Limit
	if limit <= 0 {
		limit = DefaultReconLimit
	}

	// Feed FIRST (SPEC-W44 W-B/S1-F7-08): queue payouts for matured
	// commission_payable balances and fan a CommissionPayoutWorkflow child
	// out per queued payout (deterministic child ID
	// "commission-payout-{payoutID}", REJECT_DUPLICATE → a schedule replay /
	// double-feed is dropped, never double-paid). A feed failure is
	// NON-FATAL to the recon run — the checks below still execute.
	result := ReconResult{}
	var fed []FedPayout
	if err := workflow.ExecuteActivity(ao, ActivityPayoutFeed, FeedMaturedInput{Limit: limit}).Get(ctx, &fed); err != nil {
		logger.Error("payout feed failed; continuing with recon checks", "error", err)
	} else {
		result.Fed = len(fed)
		for _, p := range fed {
			cwo := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
				WorkflowID:            "commission-payout-" + p.PayoutID,
				WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
			})
			child := workflow.ExecuteChildWorkflow(cwo, WorkflowTypePayout, PayoutInput{
				TenantID:   p.TenantID,
				TenantSlug: p.TenantSlug,
				PayoutID:   p.PayoutID,
			})
			// Wait only for the START (not completion): recon does not block
			// on money movement. An AlreadyStarted error is the dedupe fence.
			if err := child.GetChildWorkflowExecution().Get(ctx, nil); err != nil {
				logger.Info("payout child not started (already running or rejected)",
					"payout_id", p.PayoutID, "error", err)
			}
		}
	}

	var candidates []Payout
	if err := workflow.ExecuteActivity(ao, ActivityReconFetch, ReconFetchInput{Limit: limit}).Get(ctx, &candidates); err != nil {
		return err
	}
	for _, c := range candidates {
		var m *ReconMismatch
		err := workflow.ExecuteActivity(ao, ActivityReconCheck,
			ReconCheckInput{TenantID: c.TenantID.String(), PayoutID: c.ID.String()}).Get(ctx, &m)
		if err != nil {
			logger.Error("recon check failed; skipping payout",
				"payout_id", c.ID.String(), "error", err)
			continue
		}
		result.Checked++
		if m != nil {
			result.Mismatched++
			result.PayoutIDs = append(result.PayoutIDs, m.PayoutID)
		}
	}
	logger.Info("commission recon completed",
		"checked", result.Checked, "mismatched", result.Mismatched)
	return nil
}
