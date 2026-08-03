package activities

import (
	"context"
	"testing"
	"time"

	"github.com/opendesk/notification-worker/internal/daprc"
	"github.com/opendesk/notification-worker/internal/pacer"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func pacedTestActivities(p *pacer.Pacer) *Activities {
	a := New(daprc.New("127.0.0.1", 1), "booking", "payments", "identity",
		"bindings-smtp", "bindings-twilio", "no-reply@test", "+10000000000", "", IndustryDeps{}, zap.NewNop())
	a.Pacer = p
	return a
}

// The waitlist claim request with no phone/email renders and skips both
// bindings, so it exercises NotifyPaced's pacing + dispatch without a
// Dapr sidecar.
func waitlistClaimReq() workflows.PacedSendRequest {
	return workflows.PacedSendRequest{
		Kind: workflows.PacedSendWaitlistClaim,
		Waitlist: &workflows.PacedWaitlistSend{
			Input: workflows.WaitlistBackfillInput{BookingID: "b-1", TenantSlug: "acme", OfferingID: "o-1"},
			Entry: workflows.WaitlistEntry{ID: "e-a", ContactName: "Caller", WindowStart: time.Now()},
		},
	}
}

// notifyPacedErr adapts NotifyPaced's (result, error) to the error-only
// shape the dispatch assertions want.
func notifyPacedErr(a *Activities, ctx context.Context, req workflows.PacedSendRequest) error {
	_, err := a.NotifyPaced(ctx, req)
	return err
}

// NotifyPaced acquires a CPS token before dispatching: with 1 CPS and a
// burst of 1 the second send must wait for a refill.
func TestNotifyPacedPacesBeforeSend(t *testing.T) {
	p := pacer.New(pacer.Config{CPS: 1.0, Burst: 1, Backend: "local"}, zap.NewNop())
	a := pacedTestActivities(p)
	ctx := context.Background()

	start := time.Now()
	res, err := a.NotifyPaced(ctx, waitlistClaimReq())
	require.NoError(t, err)
	require.Equal(t, workflows.PacedSendStatusSent, res.Status)
	res, err = a.NotifyPaced(ctx, waitlistClaimReq())
	require.NoError(t, err)
	require.Equal(t, workflows.PacedSendStatusSent, res.Status)
	require.GreaterOrEqual(t, time.Since(start), 800*time.Millisecond,
		"second send at 1 CPS / burst 1 must be paced ~1s")
}

// Dispatch validation: unknown kinds and missing payloads fail fast.
func TestNotifyPacedDispatchValidation(t *testing.T) {
	a := pacedTestActivities(nil) // nil pacer: pacing disabled
	ctx := context.Background()

	require.ErrorContains(t, notifyPacedErr(a, ctx, workflows.PacedSendRequest{Kind: "bogus"}), "unknown send kind")
	require.ErrorContains(t, notifyPacedErr(a, ctx, workflows.PacedSendRequest{Kind: workflows.PacedSendWaitlistClaim}), "missing waitlist payload")
	require.ErrorContains(t, notifyPacedErr(a, ctx, workflows.PacedSendRequest{Kind: workflows.PacedSendReminder}), "missing reminder payload")
	require.ErrorContains(t, notifyPacedErr(a, ctx, workflows.PacedSendRequest{Kind: workflows.PacedSendDepositReminder}), "missing deposit payload")
	require.ErrorContains(t, notifyPacedErr(a, ctx, workflows.PacedSendRequest{Kind: workflows.PacedSendNoShow}), "missing noshow payload")
	require.ErrorContains(t, notifyPacedErr(a, ctx, workflows.PacedSendRequest{Kind: workflows.PacedSendConfirmation}), "missing confirmation payload")
	require.ErrorContains(t, notifyPacedErr(a, ctx, workflows.PacedSendRequest{Kind: workflows.PacedSendIntakeReminder}), "missing intake payload")
	require.ErrorContains(t, notifyPacedErr(a, ctx, workflows.PacedSendRequest{Kind: workflows.PacedSendFollowUp}), "missing follow_up payload")
	require.ErrorContains(t, notifyPacedErr(a, ctx, workflows.PacedSendRequest{Kind: workflows.PacedSendProposalReminder}), "missing proposal payload")
	require.ErrorContains(t, notifyPacedErr(a, ctx, workflows.PacedSendRequest{Kind: workflows.PacedSendStaffAlert}), "missing staff_alert payload")

	// Valid reminder dispatch (no recipients → no binding calls).
	require.NoError(t, notifyPacedErr(a, ctx, workflows.PacedSendRequest{
		Kind:     workflows.PacedSendReminder,
		Reminder: &workflows.PacedReminderSend{Input: workflows.ReminderInput{BookingID: "b-1"}, Kind: "24h0m0s"},
	}))
}

// New-kind dispatch: a confirmation request with no recipients renders the
// template and skips both bindings, exercising the SendConfirmation dispatch
// without a Dapr sidecar.
func TestNotifyPacedDispatchesConfirmation(t *testing.T) {
	a := pacedTestActivities(nil)
	require.NoError(t, notifyPacedErr(a, context.Background(), workflows.PacedSendRequest{
		Kind:         workflows.PacedSendConfirmation,
		Confirmation: &workflows.PacedConfirmationSend{Input: workflows.SagaInput{BookingID: "b-1", TenantSlug: "acme"}},
	}))
}
