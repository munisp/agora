package activities

import (
	"context"
	"fmt"

	"github.com/opendesk/notification-worker/internal/workflows"
	"go.uber.org/zap"
)

// Incident outreach (SPEC-W11 Part B §5). booking-service's
// IncidentAlertWorkflow schedules these through NotifyPaced (kind
// incident_alert, priority=true), so every alert dispatches on the pacer
// PRIORITY FAST-LANE (token-bucket bypass, still metered) with sender
// rotation exactly like the other outbound kinds.

// SendIncidentAlert delivers one incident alert on the requested channel:
//
//   - sms: routed through the channel router (MESSAGING_CHANNELS /
//     TENANT_CHANNEL_MAP — twilio native binding or the messaging-gateway
//     HTTP bindings termii/africastalking/whatsapp), with sender rotation;
//   - whatsapp / telegram: the messaging-gateway HTTP binding convention
//     ("bindings-"+channel, operation "post", {to, message} data).
func (a *Activities) SendIncidentAlert(ctx context.Context, in workflows.PacedIncidentAlertSend) error {
	if in.Phone == "" {
		return fmt.Errorf("incident alert send: phone is required (incident %s)", in.IncidentID)
	}
	if in.Text == "" {
		return fmt.Errorf("incident alert send: text is required (incident %s)", in.IncidentID)
	}

	sender := a.TwilioFrom
	if a.Pacer != nil {
		if n := a.Pacer.NextSender(ctx); n != "" {
			sender = n
		}
	}

	switch in.Channel {
	case ChannelSMS:
		provider := a.Channels.Provider(ChannelSMS, in.TenantSlug)
		if err := a.sendSMS(ctx, provider, in.Phone, in.Text, sender); err != nil {
			return fmt.Errorf("%s binding: %w", provider, err)
		}
		a.Log.Info("incident alert sent", zap.String("incident_id", in.IncidentID),
			zap.String("channel", in.Channel), zap.String("provider", provider),
			zap.String("phone", in.Phone), zap.String("sender_number", sender))
	case "whatsapp", "telegram":
		if err := a.Dapr.InvokeBinding(ctx, a.BindingName(in.Channel), "post", map[string]string{
			"to":      in.Phone,
			"message": in.Text,
		}, nil); err != nil {
			return fmt.Errorf("%s binding: %w", in.Channel, err)
		}
		a.Log.Info("incident alert sent", zap.String("incident_id", in.IncidentID),
			zap.String("channel", in.Channel), zap.String("phone", in.Phone))
	default:
		return fmt.Errorf("incident alert send: unknown channel %q (want whatsapp, telegram or sms)", in.Channel)
	}
	return nil
}
