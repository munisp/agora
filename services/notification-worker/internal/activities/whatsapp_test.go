package activities

import (
	"context"
	"testing"

	"github.com/opendesk/notification-worker/internal/pacer"
	"github.com/opendesk/notification-worker/internal/provider"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// SPEC-W21 Agent A: whatsapp_campaign activity + paced plumbing.

// fakeWhatsAppSender captures SendTemplate calls (tests inject it via
// WhatsAppDeps instead of the env fallback).
type fakeWhatsAppSender struct {
	got    []provider.WhatsAppTemplateMessage
	status int
	body   []byte
	err    error
}

func (f *fakeWhatsAppSender) SendTemplate(_ context.Context, msg provider.WhatsAppTemplateMessage) (int, []byte, error) {
	f.got = append(f.got, msg)
	if f.err != nil {
		return f.status, f.body, f.err
	}
	if f.body == nil {
		f.status = 200
		f.body = []byte(`{"messages":[{"id":"wamid.fake-1"}]}`)
	}
	return f.status, f.body, nil
}

func whatsAppReq() workflows.PacedSendRequest {
	return workflows.PacedSendRequest{
		Kind: workflows.PacedSendWhatsAppCampaign,
		WhatsApp: &workflows.PacedWhatsAppCampaignSend{
			TenantSlug:   "acme",
			ContactID:    "ct-1",
			Phone:        "+2348012345678",
			TemplateName: "vote_reminder",
			Params:       []string{"Ada", "Ward 3"},
			CampaignID:   "j-1",
		},
	}
}

// The mock-send activity test: with WHATSAPP_MOCK=1 (zero-config default,
// env fallback — no wired provider) the send "delivers" locally and returns
// a deterministic fake wamid.
func TestSendWhatsAppCampaignMessageMockDefault(t *testing.T) {
	t.Setenv("WHATSAPP_MOCK", "1")
	a := pacedTestActivities(nil)

	res, err := a.SendWhatsAppCampaignMessage(context.Background(), *whatsAppReq().WhatsApp)
	require.NoError(t, err)
	require.Contains(t, res.MessageID, "wamid.mock-", "mock send returns a fake wamid")
}

// The wired provider receives exactly the contract fields; the language
// default (en) is applied activity-side.
func TestSendWhatsAppCampaignMessageContractFields(t *testing.T) {
	fake := &fakeWhatsAppSender{}
	a := pacedTestActivities(nil)
	a.WhatsApp = WhatsAppDeps{Provider: fake}

	in := *whatsAppReq().WhatsApp
	in.Language = "" // default
	res, err := a.SendWhatsAppCampaignMessage(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, "wamid.fake-1", res.MessageID)

	require.Len(t, fake.got, 1)
	got := fake.got[0]
	require.Equal(t, "+2348012345678", got.To)
	require.Equal(t, "vote_reminder", got.Template)
	require.Equal(t, "en", got.Language, "empty language defaults to en")
	require.Equal(t, []string{"Ada", "Ward 3"}, got.Params)
}

func TestSendWhatsAppCampaignMessageValidation(t *testing.T) {
	a := pacedTestActivities(nil)
	a.WhatsApp = WhatsAppDeps{Provider: &fakeWhatsAppSender{}}

	in := *whatsAppReq().WhatsApp
	in.Phone = ""
	_, err := a.SendWhatsAppCampaignMessage(context.Background(), in)
	require.ErrorContains(t, err, "phone is required")

	in = *whatsAppReq().WhatsApp
	in.TemplateName = ""
	_, err = a.SendWhatsAppCampaignMessage(context.Background(), in)
	require.ErrorContains(t, err, "template_name is required")

	in = *whatsAppReq().WhatsApp
	in.Params = make([]string, provider.MaxWhatsAppTemplateParams+1)
	_, err = a.SendWhatsAppCampaignMessage(context.Background(), in)
	require.ErrorContains(t, err, "at most 10")
}

// Provider failure is an activity error (Temporal retry path).
func TestSendWhatsAppCampaignMessageProviderError(t *testing.T) {
	a := pacedTestActivities(nil)
	a.WhatsApp = WhatsAppDeps{Provider: &fakeWhatsAppSender{
		err: &provider.Error{StatusCode: 400, Body: "template name does not exist"},
	}}
	_, err := a.SendWhatsAppCampaignMessage(context.Background(), *whatsAppReq().WhatsApp)
	require.ErrorContains(t, err, "whatsapp cloud api")
	require.ErrorContains(t, err, "template name does not exist")
}

// NotifyPaced dispatches whatsapp_campaign to the send activity AFTER the
// pacer grant (marketing class, not suppressed).
func TestNotifyPacedDispatchesWhatsAppCampaign(t *testing.T) {
	p := pacer.New(pacer.Config{CPS: 100, Burst: 10, Backend: "local"}, zap.NewNop())
	fake := &fakeWhatsAppSender{}
	a := pacedTestActivities(p)
	a.WhatsApp = WhatsAppDeps{Provider: fake}

	res, err := a.NotifyPaced(context.Background(), whatsAppReq())
	require.NoError(t, err)
	require.Equal(t, workflows.PacedSendStatusSent, res.Status)
	require.Len(t, fake.got, 1, "the send activity must run after the CPS grant")
	granted, _ := p.Stats()
	require.Equal(t, uint64(1), granted)
}

// The DND guard suppresses whatsapp_campaign BEFORE pacing/dispatch, with
// the tenant + phone from the whatsapp payload (marketing class).
func TestNotifyPacedSuppressesWhatsAppCampaignOnDND(t *testing.T) {
	p := pacer.New(pacer.Config{CPS: 100, Burst: 10, Backend: "local"}, zap.NewNop())
	dnd := &fakeDNDChecker{suppressed: true, reason: pacer.ReasonTenantOptOut}
	guards := pacer.NewGuards(pacer.GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())
	fake := &fakeWhatsAppSender{}
	a := pacedTestActivities(p)
	a.Guards = guards
	a.WhatsApp = WhatsAppDeps{Provider: fake}

	res, err := a.NotifyPaced(context.Background(), whatsAppReq())
	require.NoError(t, err, "suppression is a completion, not an error")
	require.Equal(t, workflows.PacedSendStatusSuppressedDND, res.Status)
	require.Equal(t, pacer.ReasonTenantOptOut, res.Reason)

	require.Equal(t, 1, dnd.calls)
	require.Equal(t, "acme", dnd.gotTenant)
	require.Equal(t, "+2348012345678", dnd.gotPhone)

	granted, _ := p.Stats()
	require.Equal(t, uint64(0), granted, "suppressed sends must not consume CPS tokens")
	require.Empty(t, fake.got, "suppressed sends must never reach the provider")
	require.Equal(t, map[string]uint64{pacer.ReasonTenantOptOut: 1}, guards.SuppressedStats())
}

// A missing whatsapp_campaign payload is a contract error, not a panic.
func TestNotifyPacedWhatsAppCampaignMissingPayload(t *testing.T) {
	a := pacedTestActivities(nil)
	err := notifyPacedErr(a, context.Background(), workflows.PacedSendRequest{Kind: workflows.PacedSendWhatsAppCampaign})
	require.ErrorContains(t, err, "missing whatsapp_campaign payload")
}
