package referrals

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Commission payout workflow (SPEC-W14 §4). Runs on booking-service's own
// Temporal worker on the shared opendesk-main task queue (same pattern as
// GeoCampaignWorkflow / IncidentAlertWorkflow). NOT the Wave-5 webhook
// retry pattern: payouts are Temporal activities with their own retry
// (3 attempts, exponential backoff) and then a terminal "failed" status
// with the reason.
const (
	// WorkflowTypePayout is the registered name of the payout workflow.
	WorkflowTypePayout = "CommissionPayoutWorkflow"

	// Activity names (registered on booking-service's worker).
	ActivityPayoutTransfer   = "CommissionPayoutTransfer"
	ActivityPayoutMarkPaid   = "CommissionPayoutMarkPaid"
	ActivityPayoutMarkFailed = "CommissionPayoutMarkFailed"
	// ActivityPayoutFeed (SPEC-W44 W-B/S1-F7-08): the nightly recon first
	// feeds matured commission_payable balances into the payout queue, then
	// fans a CommissionPayoutWorkflow child out per queued payout.
	ActivityPayoutFeed = "CommissionPayoutFeedMatured"
)

// PayoutInput starts one CommissionPayoutWorkflow (workflow ID
// "commission-payout-{payoutID}" — see temporalclient.StartCommissionPayout).
type PayoutInput struct {
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	PayoutID   string `json:"payout_id"`
}

// PayoutStarter is the workflow-start seam (implemented by
// temporalclient.Client; used by Agent A's service to launch payouts).
type PayoutStarter interface {
	StartCommissionPayout(ctx context.Context, in PayoutInput) (string, error)
}

// CommissionPayoutWorkflow executes one payout:
//
//	queued → processing → provider transfer (3 attempts, backoff)
//	      → paid (+ balanced posting 300→302 + metering)
//	      → failed (reason) after the retries are exhausted
//
// The terminal failed transition runs on a disconnected context so it also
// fires when the workflow is cancelled.
func CommissionPayoutWorkflow(ctx workflow.Context, in PayoutInput) error {
	logger := workflow.GetLogger(ctx)
	ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3, // contract §4: 3 attempts then "failed"
		},
	})

	var res TransferResult
	err := workflow.ExecuteActivity(ao, ActivityPayoutTransfer,
		TransferActivityInput{TenantID: in.TenantID, TenantSlug: in.TenantSlug, PayoutID: in.PayoutID}).Get(ctx, &res)
	if err != nil {
		dctx, cancel := workflow.NewDisconnectedContext(ctx)
		defer cancel()
		dao := workflow.WithActivityOptions(dctx, workflow.ActivityOptions{
			StartToCloseTimeout: time.Minute,
			RetryPolicy: &temporal.RetryPolicy{
				InitialInterval:    time.Second,
				BackoffCoefficient: 2,
				MaximumInterval:    10 * time.Second,
				MaximumAttempts:    3,
			},
		})
		if ferr := workflow.ExecuteActivity(dao, ActivityPayoutMarkFailed,
			FailActivityInput{TenantID: in.TenantID, PayoutID: in.PayoutID, Reason: err.Error()}).Get(dctx, nil); ferr != nil {
			logger.Error("commission payout mark-failed activity failed", "payout_id", in.PayoutID, "error", ferr)
		}
		return fmt.Errorf("commission payout transfer: %w", err)
	}

	if err := workflow.ExecuteActivity(ao, ActivityPayoutMarkPaid,
		FinalizeActivityInput{TenantID: in.TenantID, TenantSlug: in.TenantSlug, PayoutID: in.PayoutID, ProviderRef: res.ProviderRef}).Get(ctx, nil); err != nil {
		// The provider already moved the money — surfacing the error lets
		// the nightly recon (CommissionReconWorkflow) catch the divergence
		// via the ledger_processing_provider_successful mismatch.
		return fmt.Errorf("commission payout finalize: %w", err)
	}
	logger.Info("commission payout paid", "payout_id", in.PayoutID, "provider_ref", res.ProviderRef)
	return nil
}
