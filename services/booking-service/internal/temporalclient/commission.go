package temporalclient

// SPEC-W14 Agent B (ADDITIVE): commission payout + nightly recon starters
// and the recon schedule bootstrap. Follows the existing starter patterns
// in client.go (deterministic workflow IDs, reject-duplicate,
// already-started tolerance).

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/referrals"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// StartCommissionPayout starts CommissionPayoutWorkflow (hosted by
// booking-service's own worker) with workflow ID
// "commission-payout-{payoutID}" so duplicate starts are idempotent.
// Implements referrals.PayoutStarter.
func (c *Client) StartCommissionPayout(ctx context.Context, in referrals.PayoutInput) (string, error) {
	id := "commission-payout-" + in.PayoutID
	opts := client.StartWorkflowOptions{
		ID:                    id,
		TaskQueue:             c.taskQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}
	_, err := c.tc.ExecuteWorkflow(ctx, opts, referrals.WorkflowTypePayout, in)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			return id, nil
		}
		return "", fmt.Errorf("execute %s: %w", referrals.WorkflowTypePayout, err)
	}
	return id, nil
}

// StartCommissionRecon starts one CommissionReconWorkflow run manually
// (ops re-drive; the nightly cadence is the Temporal Schedule bootstrapped
// by EnsureCommissionReconSchedule). Each manual run gets a unique ID.
func (c *Client) StartCommissionRecon(ctx context.Context, in referrals.ReconInput) (string, error) {
	opts := client.StartWorkflowOptions{
		ID:        "commission-recon-manual-" + uuid.NewString(),
		TaskQueue: c.taskQueue,
	}
	run, err := c.tc.ExecuteWorkflow(ctx, opts, referrals.WorkflowTypeRecon, in)
	if err != nil {
		return "", fmt.Errorf("execute %s: %w", referrals.WorkflowTypeRecon, err)
	}
	return run.GetID(), nil
}

// DefaultReconCron is the contract §7 default (02:30 Africa/Lagos).
const DefaultReconCron = "30 2 * * *"

// ReconScheduleTimeZone is the contract §5 timezone.
const ReconScheduleTimeZone = "Africa/Lagos"

// EnsureCommissionReconSchedule idempotently creates the
// commission-recon-nightly Temporal Schedule (contract §5): cron
// RECON_CRON (default "30 2 * * *") in Africa/Lagos, firing
// CommissionReconWorkflow on the shared task queue. Called once at boot
// from main.go's additive block. An existing schedule is left untouched
// (Describe-first; operators may have paused/edited it).
func (c *Client) EnsureCommissionReconSchedule(ctx context.Context, cronExpr string) error {
	if cronExpr == "" {
		cronExpr = DefaultReconCron
	}
	sc := c.tc.ScheduleClient()
	handle := sc.GetHandle(ctx, referrals.ReconScheduleID)
	if _, err := handle.Describe(ctx); err == nil {
		return nil // already bootstrapped (or operator-managed) — leave as-is
	} else {
		var notFound *serviceerror.NotFound
		if !errors.As(err, &notFound) {
			return fmt.Errorf("describe schedule %s: %w", referrals.ReconScheduleID, err)
		}
	}
	_, err := sc.Create(ctx, client.ScheduleOptions{
		ID: referrals.ReconScheduleID,
		Spec: client.ScheduleSpec{
			CronExpressions: []string{cronExpr},
			TimeZoneName:    ReconScheduleTimeZone,
		},
		Action: &client.ScheduleWorkflowAction{
			ID:        "commission-recon-nightly-run",
			Workflow:  referrals.WorkflowTypeRecon,
			Args:      []interface{}{referrals.ReconInput{}},
			TaskQueue: c.taskQueue,
		},
	})
	if err != nil {
		var alreadyExists *serviceerror.AlreadyExists
		if errors.As(err, &alreadyExists) {
			return nil // raced with another replica's bootstrap
		}
		return fmt.Errorf("create schedule %s: %w", referrals.ReconScheduleID, err)
	}
	return nil
}
