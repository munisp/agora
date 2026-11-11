package workflows

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

func waitlistTestInput() WaitlistBackfillInput {
	return WaitlistBackfillInput{
		BookingID:  "b-9",
		TenantID:   "t-1",
		TenantSlug: "acme",
		OfferingID: "o-1",
	}
}

func waitlistTestEntries(n int) []WaitlistEntry {
	out := make([]WaitlistEntry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, WaitlistEntry{
			ID:           "e-" + string(rune('a'+i)),
			OfferingID:   "o-1",
			ContactName:  "Caller",
			ContactPhone: "+1555000" + string(rune('0'+i)),
			WindowStart:  time.Now().Add(24 * time.Hour),
			WindowEnd:    time.Now().Add(48 * time.Hour),
			Status:       "waiting",
			ClaimToken:   "tok-" + string(rune('a'+i)),
		})
	}
	return out
}

// registerPacedStubs registers the two activities the workflow invokes:
// ListWaitlistEntries and the NotifyPaced pacing wrapper (VOICE-SCALING §4).
func registerPacedStubs(env *testsuite.TestWorkflowEnvironment, entries []WaitlistEntry) {
	env.RegisterActivityWithOptions(func(ctx context.Context, in WaitlistBackfillInput) ([]WaitlistEntry, error) {
		return entries, nil
	}, activity.RegisterOptions{Name: ActivityListWaitlistEntries})
	env.RegisterActivityWithOptions(func(ctx context.Context, req PacedSendRequest) error {
		return nil
	}, activity.RegisterOptions{Name: ActivityNotifyPaced})
}

// Happy path: five waiting entries → exactly the top 3 are notified, in FIFO
// order, and every send goes through the NotifyPaced pacing wrapper with the
// waitlist_claim payload.
func TestWaitlistBackfillWorkflowNotifiesTop3(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	registerPacedStubs(env, waitlistTestEntries(5))

	notified := make([]string, 0)
	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(PacedSendRequest)
			require.Equal(t, PacedSendWaitlistClaim, req.Kind, "every send is wrapped by NotifyPaced")
			require.NotNil(t, req.Waitlist)
			notified = append(notified, req.Waitlist.Entry.ID)
		}).Return(nil)

	env.ExecuteWorkflow(WaitlistBackfillWorkflow, waitlistTestInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{"e-a", "e-b", "e-c"}, notified, "exactly the top 3, FIFO order")
	env.AssertExpectations(t)
}

// Empty waitlist → no notifications, no error.
func TestWaitlistBackfillWorkflowEmptyWaitlist(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	registerPacedStubs(env, nil)

	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Panic("must not notify when the waitlist is empty")

	env.ExecuteWorkflow(WaitlistBackfillWorkflow, waitlistTestInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

// One failing notification must not block the remaining candidates.
func TestWaitlistBackfillWorkflowContinuesAfterNotifyFailure(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	entries := waitlistTestEntries(2)
	registerPacedStubs(env, entries)

	entryWithID := func(id string) interface{} {
		return mock.MatchedBy(func(req PacedSendRequest) bool {
			return req.Kind == PacedSendWaitlistClaim && req.Waitlist != nil && req.Waitlist.Entry.ID == id
		})
	}
	env.OnActivity(ActivityNotifyPaced, mock.Anything, entryWithID(entries[0].ID)).
		Return(errors.New("twilio down")).Once()
	env.OnActivity(ActivityNotifyPaced, mock.Anything, entryWithID(entries[1].ID)).
		Return(nil).Once()

	env.ExecuteWorkflow(WaitlistBackfillWorkflow, waitlistTestInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

// SPEC-W48 (A2/G): WaitlistNotifyWorkflow delivers exactly one paced claim
// notification for the staff-chosen entry, through the same
// PacedSendWaitlistClaim path as the backfill.
func TestWaitlistNotifyWorkflowSendsOnce(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(func(ctx context.Context, req PacedSendRequest) error {
		return nil
	}, activity.RegisterOptions{Name: ActivityNotifyPaced})
	entry := waitlistTestEntries(1)[0]
	entry.Status = "notified"
	send := PacedWaitlistSend{Input: waitlistTestInput(), Entry: entry}

	notified := 0
	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(PacedSendRequest)
			require.Equal(t, PacedSendWaitlistClaim, req.Kind)
			require.NotNil(t, req.Waitlist)
			require.Equal(t, entry.ID, req.Waitlist.Entry.ID)
			require.Equal(t, entry.ClaimToken, req.Waitlist.Entry.ClaimToken)
			require.Equal(t, entry.ContactPhone, req.Waitlist.Entry.ContactPhone)
			notified++
		}).Return(nil).Once()

	env.ExecuteWorkflow(WaitlistNotifyWorkflow, send)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 1, notified, "exactly one paced send for the chosen entry")
	env.AssertExpectations(t)
}

// A failed send is logged, not fatal (mirrors the backfill's per-entry
// error handling): the workflow completes successfully.
func TestWaitlistNotifyWorkflowSendFailureNotFatal(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(func(ctx context.Context, req PacedSendRequest) error {
		return nil
	}, activity.RegisterOptions{Name: ActivityNotifyPaced})
	send := PacedWaitlistSend{Input: waitlistTestInput(), Entry: waitlistTestEntries(1)[0]}

	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Return(errors.New("twilio down")).Once()

	env.ExecuteWorkflow(WaitlistNotifyWorkflow, send)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}
