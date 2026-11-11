package activities

import (
	"context"
	"fmt"

	"github.com/opendesk/notification-worker/internal/workflows"
	"go.uber.org/zap"
)

// USSD menu-action confirmations (SPEC-W47-C UIMOB-06). messaging-gateway
// terminates the Africa's Talking USSD session, dispatches the pack-menu
// action (book / status / handoff / info) and hands the promised
// confirmation SMS to THIS send path: httpapi POST /v1/notify/ussd-action
// starts PacedSendWorkflow (kind ussd_action, transactional-class) which
// schedules SendUSSDActionMessage through NotifyPaced — CPS pacing +
// sender rotation exactly like the other outbound kinds.

// SendUSSDActionMessage delivers one USSD menu-action confirmation SMS to
// the caller's phone, routed through the channel router
// (MESSAGING_CHANNELS / TENANT_CHANNEL_MAP — twilio native binding or the
// messaging-gateway HTTP bindings termii/africastalking) with sender
// rotation. SMS-only: the confirmation answers a USSD session, so the
// caller's phone channel is the contract.
func (a *Activities) SendUSSDActionMessage(ctx context.Context, in workflows.PacedUSSDActionSend) error {
	if in.Phone == "" {
		return fmt.Errorf("ussd action send: phone is required (action %s)", in.Action)
	}
	if in.Text == "" {
		return fmt.Errorf("ussd action send: text is required (action %s)", in.Action)
	}

	sender := a.TwilioFrom
	if a.Pacer != nil {
		if n := a.Pacer.NextSender(ctx); n != "" {
			sender = n
		}
	}

	provider := a.Channels.Provider(ChannelSMS, in.TenantSlug)
	if err := a.sendSMS(ctx, provider, in.Phone, in.Text, sender); err != nil {
		return fmt.Errorf("%s binding: %w", provider, err)
	}
	a.Log.Info("ussd action confirmation sent", zap.String("action", in.Action),
		zap.String("provider", provider), zap.String("phone", in.Phone),
		zap.String("sender_number", sender))
	return nil
}
