package activities

import (
	"context"
	"fmt"

	"github.com/opendesk/notification-worker/internal/pacer"
	"github.com/opendesk/notification-worker/internal/workflows"
)

// Outbound CPS pacing (docs/VOICE-SCALING.md §4 telephony plane).

// NotifyPaced is the single entry point for outbound sends from workflows.
// It first acquires a token from the worker's CPS pacer (internal/pacer:
// fleet-wide Lua token bucket in redis, or a process-local limiter) — the
// pacing knob is simultaneously the carrier CPS ceiling and the
// spam-reputation discipline — and only then invokes the requested send
// activity. Workflows stay deterministic; all waiting happens here,
// activity-side.
//
// SPEC-W12 §3 DND guard: MARKETING kinds (geo_campaign, promo, broadcast,
// drip — see pacer.ClassifyKind) are checked against the DND registry (NCC
// 2442 global list + per-tenant opt-outs) BEFORE the CPS token is acquired,
// so a suppressed send consumes no pacing budget and never touches a channel
// binding. A suppressed send is NOT an error: the result carries status
// suppressed_dnd (+ reason) and the suppression is counted
// (notifications_suppressed_total{reason}) and logged. Transactional kinds
// skip the guard entirely.
//
// The Priority fast-lane (SPEC-W11 Part B §5) is unchanged: priority sends
// bypass the token bucket exactly as before — they are transactional
// (incident_alert), so the DND guard passes them instantly.
//
// The sender rotation itself happens inside notify(): every paced send
// picks the next OUTBOUND_FROM_NUMBERS entry and puts it in the binding
// payload metadata.
func (a *Activities) NotifyPaced(ctx context.Context, req workflows.PacedSendRequest) (workflows.PacedSendResult, error) {
	res := workflows.PacedSendResult{Status: workflows.PacedSendStatusSent}
	if a.Guards != nil {
		if dec := a.Guards.PreSend(ctx, guardInputFromRequest(req)); dec.Suppress {
			res.Status = workflows.PacedSendStatusSuppressedDND
			res.Reason = dec.Reason
			return res, nil
		}
	}
	if a.Pacer != nil {
		if req.Priority {
			// SPEC-W11 Part B §5 fast-lane: emergency-grade sends (kind
			// incident_alert) dispatch immediately, bypassing the CPS token
			// bucket — but stay metered (pacer priority counter + log).
			if err := a.Pacer.Priority(ctx); err != nil {
				return res, fmt.Errorf("pacer priority: %w", err)
			}
		} else if err := a.Pacer.Wait(ctx); err != nil {
			return res, fmt.Errorf("pacer wait: %w", err)
		}
	}
	return res, a.dispatchPacedSend(ctx, req)
}

// guardInputFromRequest extracts the guard view of a paced send. Only
// marketing kinds (and incident_alert, for completeness) carry recipient
// phone/channel today; the guard short-circuits on transactional kinds
// before reading any of these fields.
func guardInputFromRequest(req workflows.PacedSendRequest) pacer.GuardInput {
	in := pacer.GuardInput{Kind: req.Kind, Channel: workflows.PacedSendChannel(req)}
	switch req.Kind {
	case workflows.PacedSendGeoCampaign:
		if req.GeoCampaign != nil {
			in.TenantSlug = req.GeoCampaign.TenantSlug
			in.Phone = req.GeoCampaign.Phone
		}
	case workflows.PacedSendIncidentAlert:
		if req.IncidentAlert != nil {
			in.TenantSlug = req.IncidentAlert.TenantSlug
			in.Phone = req.IncidentAlert.Phone
		}
	case workflows.PacedSendPushNotification, workflows.PacedSendPushMarketing:
		// SPEC-W16 §1: push_notification is transactional (guard
		// short-circuits before reading these); push_marketing is checked
		// only when the payload carries a phone (registries are
		// phone-keyed) — token-only sends pass with the no-recipient warn.
		if req.Push != nil {
			in.TenantSlug = req.Push.TenantSlug
			in.Phone = req.Push.Phone
		}
	case workflows.PacedSendWhatsAppCampaign:
		// SPEC-W21: whatsapp_campaign is marketing-class; the payload is
		// phone-addressed so the phone-keyed registries always apply.
		if req.WhatsApp != nil {
			in.TenantSlug = req.WhatsApp.TenantSlug
			in.Phone = req.WhatsApp.Phone
		}
	case workflows.PacedSendCivicStatus:
		// SPEC-W32: civic_status is transactional-class (guard short-
		// circuits before reading these); carried for audit completeness.
		if req.Civic != nil {
			in.TenantSlug = req.Civic.TenantSlug
			in.Phone = req.Civic.Phone
		}
	}
	return in
}

// dispatchPacedSend routes the granted send to its underlying send
// activity. Unchanged by SPEC-W12 except for extraction from NotifyPaced.
func (a *Activities) dispatchPacedSend(ctx context.Context, req workflows.PacedSendRequest) error {
	switch req.Kind {
	case workflows.PacedSendWaitlistClaim:
		if req.Waitlist == nil {
			return fmt.Errorf("NotifyPaced %s: missing waitlist payload", req.Kind)
		}
		return a.SendWaitlistClaimNotification(ctx, req.Waitlist.Input, req.Waitlist.Entry)
	case workflows.PacedSendReminder:
		if req.Reminder == nil {
			return fmt.Errorf("NotifyPaced %s: missing reminder payload", req.Kind)
		}
		return a.SendReminder(ctx, req.Reminder.Input, req.Reminder.Kind)
	case workflows.PacedSendDepositReminder:
		if req.Deposit == nil {
			return fmt.Errorf("NotifyPaced %s: missing deposit payload", req.Kind)
		}
		return a.SendDepositReminder(ctx, req.Deposit.Input)
	case workflows.PacedSendNoShow:
		if req.NoShow == nil {
			return fmt.Errorf("NotifyPaced %s: missing noshow payload", req.Kind)
		}
		return a.SendNoShowFollowup(ctx, req.NoShow.Input)
	case workflows.PacedSendConfirmation:
		if req.Confirmation == nil {
			return fmt.Errorf("NotifyPaced %s: missing confirmation payload", req.Kind)
		}
		return a.SendConfirmation(ctx, req.Confirmation.Input)
	case workflows.PacedSendIntakeReminder:
		if req.Intake == nil {
			return fmt.Errorf("NotifyPaced %s: missing intake payload", req.Kind)
		}
		return a.SendIntakeReminder(ctx, req.Intake.Input)
	case workflows.PacedSendFollowUp:
		if req.FollowUp == nil {
			return fmt.Errorf("NotifyPaced %s: missing follow_up payload", req.Kind)
		}
		return a.SendFollowupEmail(ctx, req.FollowUp.Input)
	case workflows.PacedSendProposalReminder:
		if req.Proposal == nil {
			return fmt.Errorf("NotifyPaced %s: missing proposal payload", req.Kind)
		}
		return a.SendProposalReminder(ctx, req.Proposal.Input)
	case workflows.PacedSendStaffAlert:
		if req.StaffAlert == nil {
			return fmt.Errorf("NotifyPaced %s: missing staff_alert payload", req.Kind)
		}
		return a.EscalateTicket(ctx, req.StaffAlert.Input)
	case workflows.PacedSendGeoCampaign:
		if req.GeoCampaign == nil {
			return fmt.Errorf("NotifyPaced %s: missing geo_campaign payload", req.Kind)
		}
		return a.SendGeoCampaignMessage(ctx, *req.GeoCampaign)
	case workflows.PacedSendIncidentAlert:
		if req.IncidentAlert == nil {
			return fmt.Errorf("NotifyPaced %s: missing incident_alert payload", req.Kind)
		}
		return a.SendIncidentAlert(ctx, *req.IncidentAlert)
	case workflows.PacedSendPushNotification, workflows.PacedSendPushMarketing:
		if req.Push == nil {
			return fmt.Errorf("NotifyPaced %s: missing push payload", req.Kind)
		}
		// Per-token results stay activity-local here (logged inside
		// SendPushNotification); workflows needing them call the
		// SendPushNotification activity directly.
		_, err := a.SendPushNotification(ctx, *req.Push)
		return err
	case workflows.PacedSendWhatsAppCampaign:
		if req.WhatsApp == nil {
			return fmt.Errorf("NotifyPaced %s: missing whatsapp_campaign payload", req.Kind)
		}
		_, err := a.SendWhatsAppCampaignMessage(ctx, *req.WhatsApp)
		return err
	case workflows.PacedSendCivicStatus:
		if req.Civic == nil {
			return fmt.Errorf("NotifyPaced %s: missing civic payload", req.Kind)
		}
		return a.SendCivicStatusUpdate(ctx, *req.Civic)
	default:
		return fmt.Errorf("NotifyPaced: unknown send kind %q", req.Kind)
	}
}
