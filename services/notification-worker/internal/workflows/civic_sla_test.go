package workflows

// SPEC-W32 WS-B: CivicSLAWorkflow timer/signal semantics and
// CivicStatusNotifyWorkflow quiet-hours hold + payload shape.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// breachStub records ReportCivicSLABreach activity inputs.
type breachStub struct {
	mu    sync.Mutex
	calls []CivicSLABreachReport
}

func (s *breachStub) run(_ context.Context, rep CivicSLABreachReport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, rep)
	return nil
}

func (s *breachStub) kinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	for i, c := range s.calls {
		out[i] = c.Kind
	}
	return out
}

// pacedStub records NotifyPaced requests and answers sent.
type pacedStub struct {
	mu    sync.Mutex
	calls []PacedSendRequest
}

func (s *pacedStub) run(_ context.Context, req PacedSendRequest) (PacedSendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	return PacedSendResult{Status: PacedSendStatusSent}, nil
}

func (s *pacedStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// civicSLAClockWorkflow wraps CivicSLAWorkflow and returns the workflow
// clock after completion, so tests can assert WHICH due time a timer fired
// at (the test env skips timer time instantly, but workflow.Now still
// advances by the full timer duration).
func civicSLAClockWorkflow(ctx workflow.Context, in CivicSLAInput) (time.Time, error) {
	if _, err := CivicSLAWorkflow(ctx, in); err != nil {
		return time.Time{}, err
	}
	return workflow.Now(ctx), nil
}

// civicBase is the workflow start instant the SLA due times are anchored
// to (the test env must be pinned to it — the default start time is the
// wall clock, which would leave the dues in the past).
var civicBase = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

func timePtr(t time.Time) *time.Time { return &t }

func newCivicEnv(t *testing.T, stub *breachStub) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.SetStartTime(civicBase)
	env.RegisterWorkflowWithOptions(CivicSLAWorkflow, workflow.RegisterOptions{Name: WorkflowTypeCivicSLA})
	env.RegisterWorkflowWithOptions(civicSLAClockWorkflow, workflow.RegisterOptions{Name: "civicSLAClockWorkflow"})
	env.RegisterActivityWithOptions(stub.run, activity.RegisterOptions{Name: ActivityReportCivicSLABreach})
	return env
}

func civicInput() CivicSLAInput {
	return CivicSLAInput{
		TenantID: "t-1", TenantSlug: "ikeja-lga", Ref: "GOV-IKEJA-03-2026-000042",
		MDAQueue:     "roads-dept",
		AckDueAt:     civicBase.Add(1 * time.Hour),
		ResolveDueAt: civicBase.Add(48 * time.Hour),
	}
}

// Workflow ID determinism (SPEC-W32 §3 WS-B: civic-sla-{tenant}-{ref}).
func TestCivicSLAWorkflowIDDeterminism(t *testing.T) {
	require.Equal(t, "civic-sla-ikeja-lga-GOV-1", CivicSLAWorkflowID("ikeja-lga", "GOV-1"))
	require.Equal(t, CivicSLAWorkflowID("ikeja-lga", "GOV-1"), CivicSLAWorkflowID("ikeja-lga", "GOV-1"),
		"same tenant+ref must always derive the same workflow ID")
	require.NotEqual(t, CivicSLAWorkflowID("ikeja-lga", "GOV-1"), CivicSLAWorkflowID("ikeja-lga", "GOV-2"))
	require.NotEqual(t, CivicSLAWorkflowID("ikeja-lga", "GOV-1"), CivicSLAWorkflowID("surulere-lga", "GOV-1"))
}

// Ack timer fires unsatisfied → breach callback with kind=ack and the
// exact callback payload (ref, tenant, mda_queue).
func TestCivicSLAAckTimerFires(t *testing.T) {
	stub := &breachStub{}
	env := newCivicEnv(t, stub)
	in := civicInput()
	in.ResolveDueAt = time.Time{} // resolve SLA unknown → only the ack timer
	env.ExecuteWorkflow(WorkflowTypeCivicSLA, in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var state CivicSLAState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.True(t, state.AckBreached)
	require.False(t, state.Acked)
	require.Equal(t, []string{"ack"}, stub.kinds())
	require.Len(t, stub.calls, 1)
	rep := stub.calls[0]
	require.Equal(t, "GOV-IKEJA-03-2026-000042", rep.Ref)
	require.Equal(t, "ikeja-lga", rep.TenantSlug)
	require.Equal(t, "t-1", rep.TenantID)
	require.Equal(t, "roads-dept", rep.MDAQueue)
}

// Resolve timer fires unsatisfied → breach callback with kind=resolve.
func TestCivicSLAResolveTimerFires(t *testing.T) {
	stub := &breachStub{}
	env := newCivicEnv(t, stub)
	in := civicInput()
	in.AckDueAt = time.Time{}
	env.ExecuteWorkflow(WorkflowTypeCivicSLA, in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var state CivicSLAState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.True(t, state.ResolveBreached)
	require.Equal(t, []string{"resolve"}, stub.kinds())
}

// Both timers fire in due order when neither is satisfied.
func TestCivicSLABothTimersBreachInOrder(t *testing.T) {
	stub := &breachStub{}
	env := newCivicEnv(t, stub)
	env.ExecuteWorkflow(WorkflowTypeCivicSLA, civicInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{"ack", "resolve"}, stub.kinds())
}

// An ack-satisfying status (assigned) cancels the ack timer; a later
// resolved cancels the resolve timer — no breach callbacks at all.
func TestCivicSLATimerCancelsOnAck(t *testing.T) {
	stub := &breachStub{}
	env := newCivicEnv(t, stub)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalCivicStatus, CivicStatusSignal{Status: "assigned"})
	}, 30*time.Minute)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalCivicStatus, CivicStatusSignal{Status: "resolved"})
	}, 90*time.Minute)
	env.ExecuteWorkflow(WorkflowTypeCivicSLA, civicInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var state CivicSLAState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.True(t, state.Acked, "assigned must satisfy the ack SLA")
	require.True(t, state.Resolved, "resolved must satisfy the resolve SLA")
	require.False(t, state.AckBreached)
	require.False(t, state.ResolveBreached)
	require.Empty(t, stub.kinds(), "no breach callbacks when both SLAs are satisfied in time")
}

// resolved satisfies BOTH timers at once (even before the ack due time).
func TestCivicSLAResolvedSatisfiesBothTimers(t *testing.T) {
	stub := &breachStub{}
	env := newCivicEnv(t, stub)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalCivicStatus, CivicStatusSignal{Status: "resolved"})
	}, 10*time.Minute)
	env.ExecuteWorkflow(WorkflowTypeCivicSLA, civicInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var state CivicSLAState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.True(t, state.Acked)
	require.True(t, state.Resolved)
	require.Empty(t, stub.kinds())
}

// closed completes the run immediately (timers cancelled).
func TestCivicSLAClosedCompletesImmediately(t *testing.T) {
	stub := &breachStub{}
	env := newCivicEnv(t, stub)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalCivicStatus, CivicStatusSignal{Status: "closed"})
	}, 5*time.Minute)
	env.ExecuteWorkflow(WorkflowTypeCivicSLA, civicInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var state CivicSLAState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.True(t, state.Closed)
	require.Empty(t, stub.kinds())
}

// Merged: timers cancelled, canonical ref recorded, no breach callbacks —
// the canonical case's own SLA workflow owns the SLA from then on.
func TestCivicSLAMergedCancelsTimers(t *testing.T) {
	stub := &breachStub{}
	env := newCivicEnv(t, stub)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalCivicMerged, CivicMergedSignal{CanonicalRef: "GOV-IKEJA-03-2026-000007"})
	}, 15*time.Minute)
	env.ExecuteWorkflow(WorkflowTypeCivicSLA, civicInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var state CivicSLAState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.True(t, state.Merged)
	require.Equal(t, "GOV-IKEJA-03-2026-000007", state.CanonicalRef)
	require.Empty(t, stub.kinds(), "merged case must never breach — the canonical case owns the SLA")
}

// A breach activity failure surfaces as a workflow error (Temporal retries
// the run).
func TestCivicSLABreachActivityErrorPropagates(t *testing.T) {
	env := newCivicEnv(t, &breachStub{})
	env.OnActivity(ActivityReportCivicSLABreach, mock.Anything, mock.Anything).
		Return(errors.New("booking-service down"))
	in := civicInput()
	in.ResolveDueAt = time.Time{}
	env.ExecuteWorkflow(WorkflowTypeCivicSLA, in)
	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
}

// SPEC-W32 W3 (a): a triage signal carrying a RECOMPUTED (later)
// resolve_due_at re-arms the resolve timer — the breach fires at the NEW
// due time, not the intake due time.
func TestCivicSLARecomputedResolveDueRearmsTimer(t *testing.T) {
	stub := &breachStub{}
	env := newCivicEnv(t, stub)
	in := civicInput()
	in.AckDueAt = time.Time{}                      // only the resolve SLA in play
	in.ResolveDueAt = civicBase.Add(2 * time.Hour) // intake SLA
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalCivicStatus, CivicStatusSignal{
			Status:       "triaged",
			ResolveDueAt: timePtr(civicBase.Add(5 * time.Hour)), // recomputed at triage
		})
	}, 30*time.Minute)
	env.ExecuteWorkflow("civicSLAClockWorkflow", in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var after time.Time
	require.NoError(t, env.GetWorkflowResult(&after))
	require.True(t, civicBase.Add(5*time.Hour).Equal(after),
		"resolve timer must fire at the RECOMPUTED due time, not the intake time (got %s)", after)
	require.Equal(t, []string{"resolve"}, stub.kinds())
}

// SPEC-W32 W3 (b) backward compat: a signal WITHOUT due times leaves the
// intake-armed timer untouched — the breach fires at the original due.
func TestCivicSLASignalWithoutDueTimesKeepsIntakeTimer(t *testing.T) {
	stub := &breachStub{}
	env := newCivicEnv(t, stub)
	in := civicInput()
	in.AckDueAt = time.Time{}
	in.ResolveDueAt = civicBase.Add(2 * time.Hour)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalCivicStatus, CivicStatusSignal{Status: "triaged"})
	}, 30*time.Minute)
	env.ExecuteWorkflow("civicSLAClockWorkflow", in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var after time.Time
	require.NoError(t, env.GetWorkflowResult(&after))
	require.True(t, civicBase.Add(2*time.Hour).Equal(after),
		"signal without due times must not move the timer (got %s)", after)
	require.Equal(t, []string{"resolve"}, stub.kinds())
}

// SPEC-W32 W3 (c): a recomputed resolve_due_at already in the PAST (on an
// unresolved case) fires the breach promptly — never a negative sleep.
func TestCivicSLAPastRecomputedDueBreachesPromptly(t *testing.T) {
	stub := &breachStub{}
	env := newCivicEnv(t, stub)
	in := civicInput()
	in.AckDueAt = time.Time{}
	in.ResolveDueAt = civicBase.Add(2 * time.Hour)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalCivicStatus, CivicStatusSignal{
			Status:       "triaged",
			ResolveDueAt: timePtr(civicBase.Add(15 * time.Minute)), // already past at signal time (30m)
		})
	}, 30*time.Minute)
	env.ExecuteWorkflow("civicSLAClockWorkflow", in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var after time.Time
	require.NoError(t, env.GetWorkflowResult(&after))
	require.True(t, civicBase.Add(30*time.Minute).Equal(after),
		"past recomputed due must breach promptly at signal time (got %s)", after)
	require.Equal(t, []string{"resolve"}, stub.kinds())
}

// SPEC-W32 W3: a recomputed due never resurrects a satisfied timer —
// resolved before the signal keeps the workflow breach-free.
func TestCivicSLARecomputedDueAfterResolvedDoesNotRearm(t *testing.T) {
	stub := &breachStub{}
	env := newCivicEnv(t, stub)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalCivicStatus, CivicStatusSignal{
			Status:       "resolved",
			ResolveDueAt: timePtr(civicBase.Add(96 * time.Hour)),
		})
	}, 10*time.Minute)
	env.ExecuteWorkflow(WorkflowTypeCivicSLA, civicInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var state CivicSLAState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.True(t, state.Resolved)
	require.Empty(t, stub.kinds(), "a satisfied SLA must never be re-armed")
}

// A recomputed ack_due_at re-arms a still-pending ack timer.
func TestCivicSLARecomputedAckDueRearmsTimer(t *testing.T) {
	stub := &breachStub{}
	env := newCivicEnv(t, stub)
	in := civicInput()
	in.AckDueAt = civicBase.Add(1 * time.Hour)
	in.ResolveDueAt = time.Time{}
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalCivicStatus, CivicStatusSignal{
			Status:   "new", // not ack-satisfying: the ack SLA is still open
			AckDueAt: timePtr(civicBase.Add(3 * time.Hour)),
		})
	}, 30*time.Minute)
	env.ExecuteWorkflow("civicSLAClockWorkflow", in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var after time.Time
	require.NoError(t, env.GetWorkflowResult(&after))
	require.True(t, civicBase.Add(3*time.Hour).Equal(after),
		"ack timer must fire at the recomputed due time (got %s)", after)
	require.Equal(t, []string{"ack"}, stub.kinds())
}

// ---------------------------------------------------------------------------
// CivicStatusNotifyWorkflow
// ---------------------------------------------------------------------------

// civicNotifyClockWorkflow wraps the notify workflow and returns the
// workflow clock after the send, so tests can assert the quiet-hours hold
// actually slept (the test env skips timer time instantly, but
// workflow.Now still advances by the full sleep duration).
func civicNotifyClockWorkflow(ctx workflow.Context, in CivicStatusNotifyInput) (time.Time, error) {
	if _, err := CivicStatusNotifyWorkflow(ctx, in); err != nil {
		return time.Time{}, err
	}
	return workflow.Now(ctx), nil
}

func newNotifyEnv(t *testing.T, stub *pacedStub) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(CivicStatusNotifyWorkflow, workflow.RegisterOptions{Name: WorkflowTypeCivicStatusNotify})
	env.RegisterWorkflowWithOptions(civicNotifyClockWorkflow, workflow.RegisterOptions{Name: "civicNotifyClockWorkflow"})
	env.RegisterActivityWithOptions(stub.run, activity.RegisterOptions{Name: ActivityNotifyPaced})
	return env
}

func notifyInput() CivicStatusNotifyInput {
	return CivicStatusNotifyInput{
		TenantID: "t-1", TenantSlug: "ikeja-lga", Ref: "GOV-IKEJA-03-2026-000042",
		Status: "assigned", Phone: "+2348012345678",
	}
}

// Inside quiet hours (21:00 Lagos) the send HELD until the window opens
// (08:00 next day) — transactional civic updates bypass DND but respect
// quiet hours (SPEC-W32 §3 WS-B).
func TestCivicStatusNotifyQuietHoursHold(t *testing.T) {
	stub := &pacedStub{}
	env := newNotifyEnv(t, stub)
	lagos, err := time.LoadLocation("Africa/Lagos")
	require.NoError(t, err)
	start := time.Date(2026, 8, 5, 21, 30, 0, 0, lagos) // inside 20:00-08:00
	env.SetStartTime(start)
	env.ExecuteWorkflow("civicNotifyClockWorkflow", notifyInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var after time.Time
	require.NoError(t, env.GetWorkflowResult(&after))
	open := time.Date(2026, 8, 6, 8, 0, 0, 0, lagos)
	require.True(t, open.Equal(after), "send must be held until the quiet window opens (got %s)", after)
	require.Equal(t, 1, stub.count())
	req := stub.calls[0]
	require.Equal(t, PacedSendCivicStatus, req.Kind)
	require.NotNil(t, req.Civic)
	require.Equal(t, "Case GOV-IKEJA-03-2026-000042: now assigned", req.Civic.Text)
	require.Equal(t, "+2348012345678", req.Civic.Phone)
	require.Equal(t, "sms", req.Civic.Channel)
	require.Equal(t, "ikeja-lga", req.Civic.TenantSlug)
}

// Outside quiet hours (10:00 Lagos) the send dispatches immediately.
func TestCivicStatusNotifyOutsideQuietHoursSendsImmediately(t *testing.T) {
	stub := &pacedStub{}
	env := newNotifyEnv(t, stub)
	lagos, err := time.LoadLocation("Africa/Lagos")
	require.NoError(t, err)
	start := time.Date(2026, 8, 5, 10, 0, 0, 0, lagos)
	env.SetStartTime(start)
	env.ExecuteWorkflow("civicNotifyClockWorkflow", notifyInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var after time.Time
	require.NoError(t, env.GetWorkflowResult(&after))
	require.True(t, start.Equal(after), "no hold outside the quiet window (got %s)", after)
	require.Equal(t, 1, stub.count())
}

// Early-morning edge of the overnight window (03:00 Lagos) also holds.
func TestCivicStatusNotifyEarlyMorningHolds(t *testing.T) {
	stub := &pacedStub{}
	env := newNotifyEnv(t, stub)
	lagos, err := time.LoadLocation("Africa/Lagos")
	require.NoError(t, err)
	start := time.Date(2026, 8, 5, 3, 0, 0, 0, lagos)
	env.SetStartTime(start)
	env.ExecuteWorkflow("civicNotifyClockWorkflow", notifyInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var after time.Time
	require.NoError(t, env.GetWorkflowResult(&after))
	require.True(t, time.Date(2026, 8, 5, 8, 0, 0, 0, lagos).Equal(after), "got %s", after)
}

// Merged case: the notification names the CANONICAL ref (SPEC-W32 §4.3).
func TestCivicStatusNotifyUsesCanonicalRef(t *testing.T) {
	stub := &pacedStub{}
	env := newNotifyEnv(t, stub)
	lagos, err := time.LoadLocation("Africa/Lagos")
	require.NoError(t, err)
	env.SetStartTime(time.Date(2026, 8, 5, 10, 0, 0, 0, lagos))
	in := notifyInput()
	in.CanonicalRef = "GOV-IKEJA-03-2026-000007"
	env.ExecuteWorkflow(WorkflowTypeCivicStatusNotify, in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 1, stub.count())
	require.Equal(t, "GOV-IKEJA-03-2026-000007", stub.calls[0].Civic.Ref)
	require.Equal(t, "Case GOV-IKEJA-03-2026-000007: now assigned", stub.calls[0].Civic.Text)
}

// No reporter phone → workflow rejects (consumer only starts it when
// wants_updates, so this is a contract guard).
func TestCivicStatusNotifyRequiresPhone(t *testing.T) {
	stub := &pacedStub{}
	env := newNotifyEnv(t, stub)
	in := notifyInput()
	in.Phone = ""
	env.ExecuteWorkflow(WorkflowTypeCivicStatusNotify, in)
	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	require.Equal(t, 0, stub.count())
}
