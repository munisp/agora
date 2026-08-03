package activities

import (
	"context"
	"testing"

	"github.com/opendesk/notification-worker/internal/pacer"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// SPEC-W12 Agent B: the activity-side DND guard in NotifyPaced.

type fakeDNDChecker struct {
	suppressed bool
	reason     string
	gotTenant  string
	gotPhone   string
	calls      int
}

func (f *fakeDNDChecker) IsSuppressed(_ context.Context, tenantSlug, phone string) (bool, string, error) {
	f.calls++
	f.gotTenant, f.gotPhone = tenantSlug, phone
	return f.suppressed, f.reason, nil
}

// A marketing send (geo_campaign) to a number on the NCC 2442 global list
// is suppressed BEFORE pacing/dispatch: no CPS token consumed, no binding
// invoked, status suppressed_dnd + reason, counter incremented.
func TestNotifyPacedSuppressesMarketingOnDND(t *testing.T) {
	dapr := newFakeDapr(t)
	p := pacer.New(pacer.Config{CPS: 100, Burst: 10, Backend: "local"}, zap.NewNop())
	dnd := &fakeDNDChecker{suppressed: true, reason: pacer.ReasonGlobalDND}
	guards := pacer.NewGuards(pacer.GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())
	a := pacedTestActivities(p)
	a.Dapr = dapr.client(t)
	a.Guards = guards

	res, err := a.NotifyPaced(context.Background(), workflows.PacedSendRequest{
		Kind: workflows.PacedSendGeoCampaign,
		GeoCampaign: &workflows.PacedGeoCampaignSend{
			TenantSlug: "acme", CampaignID: "c-1", Channel: ChannelSMS,
			Phone: "+2348012345678", Text: "flash sale",
		},
	})
	require.NoError(t, err, "suppression is a completion, not an error")
	require.Equal(t, workflows.PacedSendStatusSuppressedDND, res.Status)
	require.Equal(t, pacer.ReasonGlobalDND, res.Reason)

	require.Equal(t, 1, dnd.calls)
	require.Equal(t, "acme", dnd.gotTenant)
	require.Equal(t, "+2348012345678", dnd.gotPhone)

	granted, priority := p.Stats()
	require.Equal(t, uint64(0), granted, "suppressed sends must not consume CPS tokens")
	require.Equal(t, uint64(0), priority)

	dapr.mu.Lock()
	bindingCalls := len(dapr.bindings)
	dapr.mu.Unlock()
	require.Equal(t, 0, bindingCalls, "suppressed sends must not touch any channel binding")

	require.Equal(t, map[string]uint64{pacer.ReasonGlobalDND: 1}, guards.SuppressedStats(),
		"notifications_suppressed_total{reason=global_dnd} must increment")
}

// A tenant opt-out suppresses with reason tenant_optout.
func TestNotifyPacedSuppressesMarketingOnTenantOptOut(t *testing.T) {
	dnd := &fakeDNDChecker{suppressed: true, reason: pacer.ReasonTenantOptOut}
	guards := pacer.NewGuards(pacer.GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())
	a := pacedTestActivities(nil)
	a.Guards = guards

	res, err := a.NotifyPaced(context.Background(), workflows.PacedSendRequest{
		Kind: workflows.PacedSendGeoCampaign,
		GeoCampaign: &workflows.PacedGeoCampaignSend{
			TenantSlug: "acme", CampaignID: "c-1", Channel: "telegram",
			Phone: "+2348012345678", Text: "flash sale",
		},
	})
	require.NoError(t, err)
	require.Equal(t, workflows.PacedSendStatusSuppressedDND, res.Status)
	require.Equal(t, pacer.ReasonTenantOptOut, res.Reason)
	require.Equal(t, map[string]uint64{pacer.ReasonTenantOptOut: 1}, guards.SuppressedStats())
}

// Transactional kinds bypass the guard even when the registry would
// suppress the number: incident_alert (Priority lane) still sends.
func TestNotifyPacedIncidentAlertIgnoresDND(t *testing.T) {
	dapr := newFakeDapr(t)
	p := pacer.New(pacer.Config{CPS: 0.1, Burst: 1, Backend: "local"}, zap.NewNop())
	dnd := &fakeDNDChecker{suppressed: true, reason: pacer.ReasonGlobalDND}
	guards := pacer.NewGuards(pacer.GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())
	a := pacedTestActivities(p)
	a.Dapr = dapr.client(t)
	a.Guards = guards

	res, err := a.NotifyPaced(context.Background(), workflows.PacedSendRequest{
		Kind:     workflows.PacedSendIncidentAlert,
		Priority: true,
		IncidentAlert: &workflows.PacedIncidentAlertSend{
			TenantSlug: "acme", IncidentID: "i-1", Channel: ChannelSMS,
			Phone: "+2348012345678", Text: "EMERGENCY ALERT",
		},
	})
	require.NoError(t, err)
	require.Equal(t, workflows.PacedSendStatusSent, res.Status)
	require.Equal(t, 0, dnd.calls, "transactional kinds must not consult the DND registry")

	dapr.mu.Lock()
	_, sent := dapr.invokeCalls["/v1.0/bindings/bindings-twilio"]
	dapr.mu.Unlock()
	require.True(t, sent, "incident_alert must dispatch despite the DND hit")
	require.Empty(t, guards.SuppressedStats())
}

// With enforcement disabled, marketing sends dispatch normally.
func TestNotifyPacedDNDEnforcementDisabled(t *testing.T) {
	dapr := newFakeDapr(t)
	dnd := &fakeDNDChecker{suppressed: true, reason: pacer.ReasonGlobalDND}
	guards := pacer.NewGuards(pacer.GuardConfig{DNDEnforcement: false, DND: dnd}, zap.NewNop())
	a := pacedTestActivities(nil)
	a.Dapr = dapr.client(t)
	a.Guards = guards

	res, err := a.NotifyPaced(context.Background(), workflows.PacedSendRequest{
		Kind: workflows.PacedSendGeoCampaign,
		GeoCampaign: &workflows.PacedGeoCampaignSend{
			TenantSlug: "acme", CampaignID: "c-1", Channel: "telegram",
			Phone: "+2348012345678", Text: "flash sale",
		},
	})
	require.NoError(t, err)
	require.Equal(t, workflows.PacedSendStatusSent, res.Status)
	require.Equal(t, 0, dnd.calls)
	dapr.mu.Lock()
	_, sent := dapr.invokeCalls["/v1.0/bindings/bindings-telegram"]
	dapr.mu.Unlock()
	require.True(t, sent, "DND_ENFORCEMENT=false must pass marketing sends")
}

// Nil guards (tests / no DATABASE_URL) preserve the pre-W12 behavior.
func TestNotifyPacedNilGuardsUnchanged(t *testing.T) {
	a := pacedTestActivities(nil)
	res, err := a.NotifyPaced(context.Background(), waitlistClaimReq())
	require.NoError(t, err)
	require.Equal(t, workflows.PacedSendStatusSent, res.Status)
}
