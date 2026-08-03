package workflows

import (
	"context"
	"testing"
	"time"

	"github.com/opendesk/notification-worker/internal/pacer"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// SPEC-W12 Agent B: workflow-side quiet-hours deferral + suppressed_dnd
// result propagation through GuardedPacedSend.

// guardedTestWorkflow runs one guarded send and returns the workflow clock
// AFTER it, so tests can assert the quiet-hours deferral actually slept
// (the test env skips timer time instantly, but workflow.Now still advances
// by the full sleep duration). The caller configures ActivityOptions on ctx
// exactly like the production workflows do.
func guardedTestWorkflow(ctx workflow.Context, req PacedSendRequest, quiet pacer.QuietHoursConfig) (time.Time, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	if _, err := GuardedPacedSend(ctx, req, quiet); err != nil {
		return time.Time{}, err
	}
	return workflow.Now(ctx), nil
}

// guardedResultTestWorkflow returns the send result (status propagation).
func guardedResultTestWorkflow(ctx workflow.Context, req PacedSendRequest, quiet pacer.QuietHoursConfig) (PacedSendResult, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	return GuardedPacedSend(ctx, req, quiet)
}

func registerGuardedStubs(env *testsuite.TestWorkflowEnvironment) {
	env.RegisterActivityWithOptions(func(ctx context.Context, req PacedSendRequest) (PacedSendResult, error) {
		return PacedSendResult{Status: PacedSendStatusSent}, nil
	}, activity.RegisterOptions{Name: ActivityNotifyPaced})
	env.RegisterWorkflowWithOptions(guardedTestWorkflow, workflow.RegisterOptions{Name: "guardedTestWorkflow"})
	env.RegisterWorkflowWithOptions(guardedResultTestWorkflow, workflow.RegisterOptions{Name: "guardedResultTestWorkflow"})
}

func lagosTime(t *testing.T, day, hour int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Africa/Lagos")
	require.NoError(t, err)
	return time.Date(2026, 1, day, hour, 0, 0, 0, loc)
}

func geoCampaignReq() PacedSendRequest {
	return PacedSendRequest{
		Kind: PacedSendGeoCampaign,
		GeoCampaign: &PacedGeoCampaignSend{
			TenantSlug: "acme", CampaignID: "c-1", Channel: "sms",
			Phone: "+2348012345678", Name: "Ada", Text: "flash sale",
		},
	}
}

// A marketing send at 21:00 Lagos (inside 20:00-08:00) must be deferred to
// 08:00 the next morning BEFORE NotifyPaced runs.
func TestGuardedPacedSendDefersMarketingDuringQuietHours(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	registerGuardedStubs(env)
	env.SetStartTime(lagosTime(t, 1, 21)) // 21:00 Lagos, inside the default window

	sent := false
	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { sent = true }).
		Return(PacedSendResult{Status: PacedSendStatusSent}, nil).Once()

	env.ExecuteWorkflow(guardedTestWorkflow, geoCampaignReq(),
		pacer.QuietHoursConfig{DefaultWindow: "20:00-08:00", Timezone: "Africa/Lagos"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.True(t, sent, "the send must still happen after the deferral")

	var end time.Time
	require.NoError(t, env.GetWorkflowResult(&end))
	require.True(t, end.Equal(lagosTime(t, 2, 8)),
		"marketing send must resume at 08:00 next day, got %s", end)
}

// A marketing send at noon (outside the window) goes straight through.
func TestGuardedPacedSendMarketingOutsideWindowPasses(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	registerGuardedStubs(env)
	start := lagosTime(t, 1, 12)
	env.SetStartTime(start)

	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Return(PacedSendResult{Status: PacedSendStatusSent}, nil).Once()

	env.ExecuteWorkflow(guardedTestWorkflow, geoCampaignReq(),
		pacer.QuietHoursConfig{DefaultWindow: "20:00-08:00", Timezone: "Africa/Lagos"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var end time.Time
	require.NoError(t, env.GetWorkflowResult(&end))
	require.True(t, end.Equal(start), "no deferral outside quiet hours, got %s", end)
}

// Transactional kinds (reminders, incident_alert, ...) are NEVER deferred,
// even at 21:00 inside the window.
func TestGuardedPacedSendTransactionalNeverDeferred(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	registerGuardedStubs(env)
	start := lagosTime(t, 1, 21)
	env.SetStartTime(start)

	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Return(PacedSendResult{Status: PacedSendStatusSent}, nil).Once()

	req := PacedSendRequest{
		Kind:     PacedSendReminder,
		Reminder: &PacedReminderSend{Input: ReminderInput{BookingID: "b-1"}, Kind: "24h0m0s"},
	}
	env.ExecuteWorkflow(guardedTestWorkflow, req,
		pacer.QuietHoursConfig{DefaultWindow: "20:00-08:00", Timezone: "Africa/Lagos"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var end time.Time
	require.NoError(t, env.GetWorkflowResult(&end))
	require.True(t, end.Equal(start), "transactional kinds must not sleep, got %s", end)
}

// The Priority fast-lane is exempt from quiet-hours deferral (unchanged
// SPEC-W11 semantics): a priority send dispatches immediately even inside
// the window.
func TestGuardedPacedSendPriorityNotDeferred(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	registerGuardedStubs(env)
	start := lagosTime(t, 1, 23)
	env.SetStartTime(start)

	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Return(PacedSendResult{Status: PacedSendStatusSent}, nil).Once()

	req := PacedSendRequest{
		Kind:     PacedSendIncidentAlert,
		Priority: true,
		IncidentAlert: &PacedIncidentAlertSend{
			TenantSlug: "acme", IncidentID: "i-1", Channel: "sms",
			Phone: "+2348012345678", Text: "EMERGENCY ALERT",
		},
	}
	env.ExecuteWorkflow(guardedTestWorkflow, req,
		pacer.QuietHoursConfig{DefaultWindow: "20:00-08:00", Timezone: "Africa/Lagos"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var end time.Time
	require.NoError(t, env.GetWorkflowResult(&end))
	require.True(t, end.Equal(start), "priority sends must not sleep, got %s", end)
}

// A DND-suppressed marketing send completes with status suppressed_dnd (the
// activity did the suppression; the workflow just propagates it).
func TestGuardedPacedSendPropagatesSuppressedDND(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	registerGuardedStubs(env)
	env.SetStartTime(lagosTime(t, 1, 12))

	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Return(PacedSendResult{Status: PacedSendStatusSuppressedDND, Reason: pacer.ReasonGlobalDND}, nil).Once()

	env.ExecuteWorkflow(guardedResultTestWorkflow, geoCampaignReq(),
		pacer.QuietHoursConfig{DefaultWindow: "20:00-08:00", Timezone: "Africa/Lagos"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var res PacedSendResult
	require.NoError(t, env.GetWorkflowResult(&res))
	require.Equal(t, PacedSendStatusSuppressedDND, res.Status)
	require.Equal(t, pacer.ReasonGlobalDND, res.Reason)
}

// PacedSendChannel extraction (per-channel quiet-hours overrides key off it).
func TestPacedSendChannel(t *testing.T) {
	require.Equal(t, "sms", PacedSendChannel(geoCampaignReq()))
	require.Equal(t, "whatsapp", PacedSendChannel(PacedSendRequest{
		Kind:          PacedSendIncidentAlert,
		IncidentAlert: &PacedIncidentAlertSend{Channel: "whatsapp"},
	}))
	require.Equal(t, "", PacedSendChannel(PacedSendRequest{Kind: PacedSendGeoCampaign}))
	require.Equal(t, "", PacedSendChannel(PacedSendRequest{Kind: PacedSendReminder}))
}
