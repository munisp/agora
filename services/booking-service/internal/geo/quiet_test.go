package geo

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// ---------------------------------------------------------------------------
// Pure quiet-hours math (mirror of notification-worker pacer guard tests)
// ---------------------------------------------------------------------------

func lagos(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Africa/Lagos")
	require.NoError(t, err)
	return loc
}

func TestClassifyKind(t *testing.T) {
	for _, kind := range []string{"geo_campaign", "promo", "broadcast", "drip"} {
		require.Equal(t, ClassMarketing, ClassifyKind(kind), kind)
	}
	for _, kind := range []string{
		"confirmation", "reminder", "deposit_reminder", "intake_reminder",
		"proposal_reminder", "noshow_followup", "waitlist_claim", "follow_up",
		"staff_alert", "incident_alert", "otp",
	} {
		require.Equal(t, ClassTransactional, ClassifyKind(kind), kind)
	}
	// Unknown kinds default to transactional: the guards only ever apply to
	// kinds explicitly classified as marketing.
	require.Equal(t, ClassTransactional, ClassifyKind("something_new"))
	require.Equal(t, ClassTransactional, ClassifyKind(""))
}

func TestParseQuietWindow(t *testing.T) {
	w, err := ParseQuietWindow("20:00-08:00")
	require.NoError(t, err)
	require.Equal(t, QuietWindow{StartMin: 20 * 60, EndMin: 8 * 60}, w)

	w, err = ParseQuietWindow("22:30-06:15")
	require.NoError(t, err)
	require.Equal(t, QuietWindow{StartMin: 22*60 + 30, EndMin: 6*60 + 15}, w)

	for _, bad := range []string{"", "20:00", "20:00-", "24:00-08:00", "20:60-08:00", "20:00-20:00", "aa:bb-cc:dd", "20:00:00-08:00"} {
		_, err := ParseQuietWindow(bad)
		require.Error(t, err, bad)
	}
}

func TestQuietWindowContainsOvernight(t *testing.T) {
	loc := lagos(t)
	w := QuietWindow{StartMin: 20 * 60, EndMin: 8 * 60}
	at := func(h, m int) time.Time {
		return time.Date(2025, 3, 10, h, m, 0, 0, loc)
	}
	require.True(t, w.Contains(at(20, 0)), "window start inclusive")
	require.True(t, w.Contains(at(21, 0)))
	require.True(t, w.Contains(at(23, 59)))
	require.True(t, w.Contains(at(0, 0)))
	require.True(t, w.Contains(at(7, 59)))
	require.False(t, w.Contains(at(8, 0)), "window end exclusive")
	require.False(t, w.Contains(at(12, 0)))
	require.False(t, w.Contains(at(19, 59)))
}

func TestQuietHoursOpenAt(t *testing.T) {
	loc := lagos(t)
	cfg := QuietHoursFromEnv("", "", nil) // defaults: 20:00-08:00 Africa/Lagos

	// 21:00 Lagos inside the overnight window → opens 08:00 the next day.
	open, inWindow, err := QuietHoursOpenAt(time.Date(2025, 3, 10, 21, 0, 0, 0, loc), "whatsapp", cfg)
	require.NoError(t, err)
	require.True(t, inWindow)
	require.Equal(t, time.Date(2025, 3, 11, 8, 0, 0, 0, loc), open)

	// 03:00 Lagos (after midnight) inside the window → opens 08:00 same day.
	open, inWindow, err = QuietHoursOpenAt(time.Date(2025, 3, 10, 3, 0, 0, 0, loc), "whatsapp", cfg)
	require.NoError(t, err)
	require.True(t, inWindow)
	require.Equal(t, time.Date(2025, 3, 10, 8, 0, 0, 0, loc), open)

	// 12:00 Lagos outside the window.
	_, inWindow, err = QuietHoursOpenAt(time.Date(2025, 3, 10, 12, 0, 0, 0, loc), "whatsapp", cfg)
	require.NoError(t, err)
	require.False(t, inWindow)

	// A non-Lagos instant is evaluated on the Lagos clock (21:30 UTC =
	// 22:30 Lagos → inside the window, opens 07:00 UTC = 08:00 Lagos).
	open, inWindow, err = QuietHoursOpenAt(time.Date(2025, 3, 10, 21, 30, 0, 0, time.UTC), "sms", cfg)
	require.NoError(t, err)
	require.True(t, inWindow)
	require.Equal(t, time.Date(2025, 3, 11, 7, 0, 0, 0, time.UTC), open.UTC())

	// Per-channel override wins over the default window.
	ovCfg := QuietHoursFromEnv("", "", map[string]string{"sms": "22:00-06:00"})
	_, inWindow, err = QuietHoursOpenAt(time.Date(2025, 3, 10, 21, 0, 0, 0, loc), "sms", ovCfg)
	require.NoError(t, err)
	require.False(t, inWindow, "sms override 22:00-06:00 not yet open at 21:00")
	open, inWindow, err = QuietHoursOpenAt(time.Date(2025, 3, 10, 23, 0, 0, 0, loc), "sms", ovCfg)
	require.NoError(t, err)
	require.True(t, inWindow)
	require.Equal(t, time.Date(2025, 3, 11, 6, 0, 0, 0, loc), open)

	// Malformed window / unknown timezone are errors (fail fast at run time).
	_, _, err = QuietHoursOpenAt(time.Now(), "sms", QuietHoursConfig{DefaultWindow: "bogus", Timezone: "Africa/Lagos"})
	require.Error(t, err)
	_, _, err = QuietHoursOpenAt(time.Now(), "sms", QuietHoursConfig{DefaultWindow: "20:00-08:00", Timezone: "Mars/Olympus"})
	require.Error(t, err)
}

func TestQuietHoursFromEnvDefaults(t *testing.T) {
	cfg := QuietHoursFromEnv("", "", nil)
	require.Equal(t, DefaultQuietHoursWindow, cfg.DefaultWindow)
	require.Equal(t, DefaultQuietHoursTimezone, cfg.Timezone)
	cfg = QuietHoursFromEnv("22:00-06:00", "UTC", map[string]string{"sms": "23:00-05:00"})
	require.Equal(t, "22:00-06:00", cfg.DefaultWindow)
	require.Equal(t, "UTC", cfg.Timezone)
	require.Equal(t, "23:00-05:00", cfg.Overrides["sms"])
}

func TestParseQuietHoursOverrides(t *testing.T) {
	ov, err := ParseQuietHoursOverrides("")
	require.NoError(t, err)
	require.Nil(t, ov)
	ov, err = ParseQuietHoursOverrides(`{"sms":"22:00-06:00"}`)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"sms": "22:00-06:00"}, ov)
	_, err = ParseQuietHoursOverrides("{not json")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// guardedPacedSend (temporal testsuite): deferral + suppressed_dnd contract
// ---------------------------------------------------------------------------

// guardedTestWorkflow wraps guardedPacedSend so tests can observe the
// workflow-clock instant AFTER the send (the mock clock advances across a
// workflow.Sleep when the timer fires) alongside the PacedSendResult.
type guardedTestResult struct {
	Result   PacedSendResult `json:"result"`
	AfterNow time.Time       `json:"after_now"`
}

func guardedTestWorkflow(ctx workflow.Context, req PacedSendRequest, quiet QuietHoursConfig) (guardedTestResult, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	res, err := guardedPacedSend(ctx, req, quiet)
	if err != nil {
		return guardedTestResult{}, err
	}
	return guardedTestResult{Result: res, AfterNow: workflow.Now(ctx)}, nil
}

func newGuardedTestEnv(result *PacedSendResult) *testsuite.TestWorkflowEnvironment {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(func(ctx context.Context, req PacedSendRequest) (PacedSendResult, error) {
		if result != nil {
			return *result, nil
		}
		return PacedSendResult{}, nil // legacy worker: no payload
	}, activity.RegisterOptions{Name: ActivityNotifyPaced})
	return env
}

// 21:00 Africa/Lagos inside the default 20:00-08:00 window: a marketing
// (geo_campaign) send is deferred until the window opens at 08:00 the next
// day, then dispatched through NotifyPaced.
func TestGuardedPacedSendDefersMarketingUntilWindowOpens(t *testing.T) {
	loc := lagos(t)
	env := newGuardedTestEnv(&PacedSendResult{Status: PacedSendStatusSent})
	// 21:00 Lagos = 20:00 UTC.
	env.SetStartTime(time.Date(2025, 3, 10, 21, 0, 0, 0, loc))

	req := PacedSendRequest{Kind: PacedSendGeoCampaign, Geo: &PacedGeoCampaignSend{
		TenantSlug: "acme", CampaignID: "c1", Channel: "whatsapp",
		Phone: "+23480000001", Name: "Ada", Text: "Hi Ada!",
	}}
	env.ExecuteWorkflow(guardedTestWorkflow, req, QuietHoursFromEnv("", "", nil))
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var out guardedTestResult
	require.NoError(t, env.GetWorkflowResult(&out))
	require.Equal(t, PacedSendStatusSent, out.Result.Status)
	require.Equal(t, time.Date(2025, 3, 11, 8, 0, 0, 0, loc), out.AfterNow.In(loc),
		"21:00 send deferred to 08:00 next day (11h durable sleep)")
}

// Outside the window a marketing send dispatches immediately (no sleep).
func TestGuardedPacedSendMarketingOutsideWindowSendsImmediately(t *testing.T) {
	loc := lagos(t)
	env := newGuardedTestEnv(&PacedSendResult{Status: PacedSendStatusSent})
	start := time.Date(2025, 3, 10, 12, 0, 0, 0, loc)
	env.SetStartTime(start)

	req := PacedSendRequest{Kind: PacedSendGeoCampaign, Geo: &PacedGeoCampaignSend{Channel: "sms"}}
	env.ExecuteWorkflow(guardedTestWorkflow, req, QuietHoursFromEnv("", "", nil))
	require.NoError(t, env.GetWorkflowError())

	var out guardedTestResult
	require.NoError(t, env.GetWorkflowResult(&out))
	require.True(t, start.Equal(out.AfterNow), "no deferral outside the quiet-hours window (got %s)", out.AfterNow)
}

// Transactional kinds never sleep, even inside the window.
func TestGuardedPacedSendTransactionalNeverSleeps(t *testing.T) {
	loc := lagos(t)
	env := newGuardedTestEnv(&PacedSendResult{Status: PacedSendStatusSent})
	start := time.Date(2025, 3, 10, 23, 30, 0, 0, loc) // deep inside the window
	env.SetStartTime(start)

	// geo's PacedSendRequest only carries the geo payload, but the
	// classification table is kind-driven: a transactional kind must pass
	// immediately regardless of the hour.
	req := PacedSendRequest{Kind: "confirmation"}
	env.ExecuteWorkflow(guardedTestWorkflow, req, QuietHoursFromEnv("", "", nil))
	require.NoError(t, env.GetWorkflowError())

	var out guardedTestResult
	require.NoError(t, env.GetWorkflowResult(&out))
	require.True(t, start.Equal(out.AfterNow), "transactional kinds are never deferred (got %s)", out.AfterNow)
}

// suppressed_dnd is a completion status, not an error: the guard result is
// returned to the scheduling workflow for recording.
func TestGuardedPacedSendReturnsSuppressedDND(t *testing.T) {
	loc := lagos(t)
	env := newGuardedTestEnv(&PacedSendResult{Status: PacedSendStatusSuppressedDND, Reason: "global_dnd"})
	env.SetStartTime(time.Date(2025, 3, 10, 12, 0, 0, 0, loc))

	req := PacedSendRequest{Kind: PacedSendGeoCampaign, Geo: &PacedGeoCampaignSend{Channel: "sms"}}
	env.ExecuteWorkflow(guardedTestWorkflow, req, QuietHoursFromEnv("", "", nil))
	require.NoError(t, env.GetWorkflowError())

	var out guardedTestResult
	require.NoError(t, env.GetWorkflowResult(&out))
	require.Equal(t, PacedSendStatusSuppressedDND, out.Result.Status)
	require.Equal(t, "global_dnd", out.Result.Reason)
}

// Legacy workers return no result payload: an empty status means the send
// happened (backward compatible with Get(ctx, nil) callers).
func TestGuardedPacedSendEmptyStatusMeansSent(t *testing.T) {
	loc := lagos(t)
	env := newGuardedTestEnv(nil)
	env.SetStartTime(time.Date(2025, 3, 10, 12, 0, 0, 0, loc))

	req := PacedSendRequest{Kind: PacedSendGeoCampaign, Geo: &PacedGeoCampaignSend{Channel: "sms"}}
	env.ExecuteWorkflow(guardedTestWorkflow, req, QuietHoursFromEnv("", "", nil))
	require.NoError(t, env.GetWorkflowError())

	var out guardedTestResult
	require.NoError(t, env.GetWorkflowResult(&out))
	require.Equal(t, PacedSendStatusSent, out.Result.Status)
}

// A malformed quiet-hours window fails the send path loudly (fail fast,
// mirroring notification-worker's boot-time validation).
func TestGuardedPacedSendMalformedWindowFails(t *testing.T) {
	loc := lagos(t)
	env := newGuardedTestEnv(&PacedSendResult{Status: PacedSendStatusSent})
	env.SetStartTime(time.Date(2025, 3, 10, 12, 0, 0, 0, loc))

	req := PacedSendRequest{Kind: PacedSendGeoCampaign, Geo: &PacedGeoCampaignSend{Channel: "sms"}}
	env.ExecuteWorkflow(guardedTestWorkflow, req, QuietHoursConfig{DefaultWindow: "bogus", Timezone: "Africa/Lagos"})
	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
}

// ---------------------------------------------------------------------------
// GeoCampaignWorkflow: suppressed_dnd outcomes are recorded, not sent
// ---------------------------------------------------------------------------

// A recipient whose paced send completes suppressed_dnd is recorded as a
// suppression (skipped from the send ledger + metering), while the other
// recipients send and record normally.
func TestGeoCampaignWorkflowRecordsSuppressedDND(t *testing.T) {
	g := newGeoTestEnv(t, 3)

	g.env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.MatchedBy(func(req PacedSendRequest) bool {
		return req.Geo != nil && req.Geo.Phone == g.recipients[1].Phone
	})).Return(PacedSendResult{Status: PacedSendStatusSuppressedDND, Reason: "tenant_optout"}, nil).Once()
	g.env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.MatchedBy(func(req PacedSendRequest) bool {
		return req.Geo != nil && req.Geo.Phone != g.recipients[1].Phone
	})).Return(PacedSendResult{Status: PacedSendStatusSent}, nil)

	// Noon Lagos: outside the quiet-hours window, so no deferral interferes.
	loc := lagos(t)
	g.env.SetStartTime(time.Date(2025, 3, 10, 12, 0, 0, 0, loc))

	g.env.ExecuteWorkflow(GeoCampaignWorkflow, g.input(50))
	require.True(t, g.env.IsWorkflowCompleted())
	require.NoError(t, g.env.GetWorkflowError())
	require.Len(t, g.recorded, 1)
	require.Len(t, g.recorded[0], 2, "suppressed recipient is not recorded as sent")
	for _, r := range g.recorded[0] {
		require.NotEqual(t, g.recipients[1].ContactID, r.ContactID)
	}
	require.True(t, g.completed)
}
