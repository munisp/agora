package activities

import (
	"context"
	"fmt"

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
// The sender rotation itself happens inside notify(): every paced send
// picks the next OUTBOUND_FROM_NUMBERS entry and puts it in the binding
// payload metadata.
func (a *Activities) NotifyPaced(ctx context.Context, req workflows.PacedSendRequest) error {
	if a.Pacer != nil {
		if req.Priority {
			// SPEC-W11 Part B §5 fast-lane: emergency-grade sends (kind
			// incident_alert) dispatch immediately, bypassing the CPS token
			// bucket — but stay metered (pacer priority counter + log).
			if err := a.Pacer.Priority(ctx); err != nil {
				return fmt.Errorf("pacer priority: %w", err)
			}
		} else if err := a.Pacer.Wait(ctx); err != nil {
			return fmt.Errorf("pacer wait: %w", err)
		}
	}
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
	default:
		return fmt.Errorf("NotifyPaced: unknown send kind %q", req.Kind)
	}
}
