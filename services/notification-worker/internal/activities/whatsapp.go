package activities

// WhatsApp campaign template sends (SPEC-W21 Agent A: paced kind
// whatsapp_campaign). booking-service's campaign-studio whatsapp steps
// schedule these through NotifyPaced, so every send is CPS-paced and
// SPEC-W12 §3 compliant (marketing class: DND-suppressed activity-side,
// quiet-hours deferred workflow-side) exactly like the sms marketing kinds.
//
// Transport idiom: DIRECT Meta Cloud API call from the worker via
// internal/provider.WhatsApp. Mock posture (SIM-011): WHATSAPP_MOCK
// defaults OFF — the deterministic mock (no network, fake wamid) is an
// explicit dev/test opt-in; with the mock off and no Cloud API credentials
// configured, sends fail closed with an explicit error. The
// messaging-gateway is NOT involved: its /v1/whatsapp/send endpoint
// predates this contract and carries neither the template language nor
// positional params, and the established worker-side provider seam
// (internal/provider, FCM) is the documented pattern for
// credential-bearing outbound sends.

import (
	"context"
	"fmt"

	"github.com/opendesk/notification-worker/internal/provider"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.uber.org/zap"
)

// WhatsAppSender is the provider seam the activity sends through
// (provider.WhatsApp satisfies it; tests inject fakes).
type WhatsAppSender interface {
	SendTemplate(ctx context.Context, msg provider.WhatsAppTemplateMessage) (int, []byte, error)
}

// WhatsAppDeps bundles the WhatsApp campaign dependencies; set by main
// after New. A nil Provider falls back to provider.NewWhatsAppFromEnv at
// send time (WHATSAPP_MOCK defaults OFF — SIM-011), so the seam works
// WITHOUT main/config edits (integrator may wire an explicit provider
// later, mirroring PushDeps).
type WhatsAppDeps struct {
	Provider WhatsAppSender
}

// whatsappSender resolves the send provider: the wired dependency first,
// else the env-derived provider (WHATSAPP_MOCK default OFF, SIM-011).
func (a *Activities) whatsappSender() WhatsAppSender {
	if a.WhatsApp.Provider != nil {
		return a.WhatsApp.Provider
	}
	return provider.NewWhatsAppFromEnv(a.Log)
}

// SendWhatsAppCampaignMessage delivers one WhatsApp business-initiated
// template message (paced kind whatsapp_campaign). Contract-level failures
// (missing phone/template, >10 params, provider failure) are activity
// errors so Temporal retries per the paced retry policy; a Meta 4xx is a
// caller/template fault surfaced via the provider error body.
func (a *Activities) SendWhatsAppCampaignMessage(ctx context.Context, w workflows.PacedWhatsAppCampaignSend) (workflows.WhatsAppCampaignResult, error) {
	var res workflows.WhatsAppCampaignResult
	if w.Phone == "" {
		return res, fmt.Errorf("whatsapp campaign send: phone is required (tenant %s)", w.TenantSlug)
	}
	if w.TemplateName == "" {
		return res, fmt.Errorf("whatsapp campaign send: template_name is required (tenant %s)", w.TenantSlug)
	}
	if len(w.Params) > provider.MaxWhatsAppTemplateParams {
		return res, fmt.Errorf("whatsapp campaign send: at most %d params (tenant %s)", provider.MaxWhatsAppTemplateParams, w.TenantSlug)
	}
	language := w.Language
	if language == "" {
		language = "en" // contract default (SPEC-W21)
	}

	status, body, err := a.whatsappSender().SendTemplate(ctx, provider.WhatsAppTemplateMessage{
		To:       w.Phone,
		Template: w.TemplateName,
		Language: language,
		Params:   w.Params,
	})
	if err != nil {
		return res, fmt.Errorf("whatsapp cloud api: %w", err)
	}
	res.MessageID = provider.WhatsAppMessageID(body)
	a.Log.Info("whatsapp campaign message sent",
		zap.String("tenant", w.TenantSlug), zap.String("campaign_id", w.CampaignID),
		zap.String("contact_id", w.ContactID), zap.String("phone", w.Phone),
		zap.String("template", w.TemplateName), zap.String("language", language),
		zap.Int("params", len(w.Params)), zap.Int("provider_status", status),
		zap.String("message_id", res.MessageID))
	return res, nil
}
