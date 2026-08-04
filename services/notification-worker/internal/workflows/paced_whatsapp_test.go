package workflows

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/opendesk/notification-worker/internal/pacer"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

// SPEC-W21 Agent A: whatsapp_campaign kind — classification, payload
// contract, channel extraction, quiet-hours deferral.

// whatsappCampaignReq builds a canonical whatsapp_campaign paced request.
func whatsAppCampaignReq() PacedSendRequest {
	return PacedSendRequest{
		Kind: PacedSendWhatsAppCampaign,
		WhatsApp: &PacedWhatsAppCampaignSend{
			TenantSlug:   "acme",
			ContactID:    "ct-1",
			Phone:        "+2348012345678",
			TemplateName: "vote_reminder",
			Language:     "en_US",
			Params:       []string{"Ada", "Ward 3"},
			CampaignID:   "j-1",
		},
	}
}

// The kind is MARKETING-classified (DND + opt-out + quiet-hours apply).
func TestWhatsAppCampaignKindClassification(t *testing.T) {
	require.Equal(t, pacer.ClassMarketing, pacer.ClassifyKind(PacedSendWhatsAppCampaign))
	require.Equal(t, pacer.ClassMarketing, pacer.ClassifyKind("whatsapp_campaign"))
}

// The quiet-hours channel key is the fixed "whatsapp" (per-channel
// override target), independent of the payload.
func TestWhatsAppCampaignChannel(t *testing.T) {
	require.Equal(t, "whatsapp", PacedSendChannel(whatsAppCampaignReq()))
	require.Equal(t, "whatsapp", PacedSendChannel(PacedSendRequest{Kind: PacedSendWhatsAppCampaign}))
}

// Payload marshal contract: the PacedSendRequest JSON shape the
// campaign-studio producer emits (duplicated across the service boundary)
// must round-trip field-for-field.
func TestWhatsAppCampaignPayloadMarshalContract(t *testing.T) {
	raw, err := json.Marshal(whatsAppCampaignReq())
	require.NoError(t, err)

	// Exact wire shape (producer mirror in booking-service
	// internal/campaignstudio/workflow.go).
	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	require.Equal(t, "whatsapp_campaign", wire["kind"])
	payload, ok := wire["whatsapp_campaign"].(map[string]any)
	require.True(t, ok, "the whatsapp_campaign payload key must be present")
	require.Equal(t, map[string]any{
		"tenant_slug":   "acme",
		"contact_id":    "ct-1",
		"phone":         "+2348012345678",
		"template_name": "vote_reminder",
		"language":      "en_US",
		"params":        []any{"Ada", "Ward 3"},
		"campaign_id":   "j-1",
	}, payload)

	// Round-trip through the consumer's unmarshal path (CloudEvent data IS
	// the PacedSendRequest).
	var back PacedSendRequest
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, PacedSendWhatsAppCampaign, back.Kind)
	require.NotNil(t, back.WhatsApp)
	require.Equal(t, *whatsAppCampaignReq().WhatsApp, *back.WhatsApp)

	// campaign_id is nullable/omittable (SPEC: campaign_id null).
	minimal := PacedSendRequest{Kind: PacedSendWhatsAppCampaign, WhatsApp: &PacedWhatsAppCampaignSend{
		TenantSlug: "acme", Phone: "+234", TemplateName: "t",
	}}
	raw, err = json.Marshal(minimal)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &wire))
	payload, ok = wire["whatsapp_campaign"].(map[string]any)
	require.True(t, ok)
	_, hasCampaign := payload["campaign_id"]
	require.False(t, hasCampaign, "campaign_id omits when empty (null-allowed)")
	_, hasLang := payload["language"]
	require.False(t, hasLang, "language omits when empty (default en)")
	_, hasParams := payload["params"]
	require.False(t, hasParams, "params omits when empty")
}

// A whatsapp_campaign send inside the quiet-hours window is deferred to
// window open, exactly like the sms marketing kinds.
func TestGuardedPacedSendDefersWhatsAppCampaignDuringQuietHours(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	registerGuardedStubs(env)
	env.SetStartTime(lagosTime(t, 1, 21)) // 21:00 Lagos, inside 20:00-08:00

	sent := false
	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { sent = true }).
		Return(PacedSendResult{Status: PacedSendStatusSent}, nil).Once()

	env.ExecuteWorkflow(guardedTestWorkflow, whatsAppCampaignReq(),
		pacer.QuietHoursConfig{DefaultWindow: "20:00-08:00", Timezone: "Africa/Lagos"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.True(t, sent)

	end := mustResultTime(t, env)
	require.True(t, end.Equal(lagosTime(t, 2, 8)),
		"whatsapp marketing send must resume at 08:00 next day, got %s", end)
}

// A per-channel "whatsapp" override window applies to the kind (channel
// key contract of PacedSendChannel).
func TestGuardedPacedSendWhatsAppChannelOverride(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	registerGuardedStubs(env)
	start := lagosTime(t, 1, 12) // noon: outside default window, inside a 09:00-13:00 override
	env.SetStartTime(start)

	env.OnActivity(ActivityNotifyPaced, mock.Anything, mock.Anything).
		Return(PacedSendResult{Status: PacedSendStatusSent}, nil).Once()

	env.ExecuteWorkflow(guardedTestWorkflow, whatsAppCampaignReq(),
		pacer.QuietHoursConfig{
			DefaultWindow: "20:00-08:00",
			Overrides:     map[string]string{"whatsapp": "09:00-13:00"},
			Timezone:      "Africa/Lagos",
		})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	end := mustResultTime(t, env)
	require.True(t, end.Equal(lagosTime(t, 1, 13)),
		"the whatsapp override window must defer to 13:00, got %s", end)
}

func mustResultTime(t *testing.T, env *testsuite.TestWorkflowEnvironment) time.Time {
	t.Helper()
	var out time.Time
	require.NoError(t, env.GetWorkflowResult(&out))
	return out
}
