package workflows

import (
	"time"

	"go.temporal.io/sdk/workflow"
)

// Industry pack workflows (SPEC-CRM §C2). All four are compensation-safe:
// their side effects are notifications, CRM tasks and ledger charges that are
// idempotent by construction (deterministic ids downstream) and they stop
// cleanly on a booking-event "cancelled" signal. They are started as child
// workflows of BookingSagaWorkflow after ConfirmBooking.

const (
	clinicIntakeReminderLead = 72 * time.Hour // T-72h intake form reminder
	clinicIntakeDeadlineLead = 2 * time.Hour  // T-2h incomplete → staff alert

	consultancyProposalReminderDelay = 7 * 24 * time.Hour // T+7d proposal reminder

	defaultFirstResponseSLA = 4 * time.Hour // support-desk SLA
)

// waitForSignalOrTimer blocks until the timer fires, a signal arrives on
// sigCh (may be nil to only watch for cancellation), or a booking-event
// "cancelled" arrives. It reports which happened. A zero/negative delay
// skips the timer (signals are drained non-blocking).
func waitForSignalOrTimer(ctx workflow.Context, delay time.Duration, sigCh workflow.ReceiveChannel) (fired, signalled, cancelled bool) {
	evtCh := workflow.GetSignalChannel(ctx, SignalBookingEvent)
	var timer workflow.Future
	if delay > 0 {
		timer = workflow.NewTimer(ctx, delay)
	}
	for {
		sel := workflow.NewSelector(ctx)
		if timer != nil {
			sel.AddFuture(timer, func(f workflow.Future) { fired = true })
		} else {
			sel.AddDefault(func() {})
		}
		if sigCh != nil {
			sel.AddReceive(sigCh, func(c workflow.ReceiveChannel, more bool) {
				c.Receive(ctx, nil)
				signalled = true
			})
		}
		sel.AddReceive(evtCh, func(c workflow.ReceiveChannel, more bool) {
			var sig BookingEventSignal
			c.Receive(ctx, &sig)
			if sig.Type == "cancelled" {
				cancelled = true
			}
		})
		sel.Select(ctx)
		if fired || signalled || cancelled {
			return
		}
		// spurious booking-event (rescheduled/confirmed): keep waiting
	}
}

// ClinicIntakeWorkflow (clinic pack): emails the intake form link at T-72h;
// if the IntakeCompleted signal has not arrived by T-2h, raises a staff
// alert task in the CRM.
func ClinicIntakeWorkflow(ctx workflow.Context, in ClinicIntakeInput) error {
	logger := workflow.GetLogger(ctx)
	ctx = workflow.WithActivityOptions(ctx, sagaActivityOptions())

	state := "scheduled"
	if err := workflow.SetQueryHandler(ctx, QueryState, func() (string, error) {
		return state, nil
	}); err != nil {
		return err
	}

	intakeCh := workflow.GetSignalChannel(ctx, SignalIntakeCompleted)

	// Phase 1: wait until the T-72h intake reminder is due.
	reminderAt := in.StartsAt.Add(-clinicIntakeReminderLead)
	fired, signalled, cancelled := waitForSignalOrTimer(ctx, reminderAt.Sub(workflow.Now(ctx)), intakeCh)
	if cancelled {
		state = "stopped:cancelled"
		return nil
	}
	if signalled {
		state = "done:intake-completed-early"
		return nil
	}
	_ = fired

	state = "sending-intake-reminder"
	// Outbound send: route through NotifyPaced (CPS token + sender rotation).
	intakeReq := PacedSendRequest{Kind: PacedSendIntakeReminder, Intake: &PacedIntakeReminderSend{Input: in}}
	if err := workflow.ExecuteActivity(ctx, ActivityNotifyPaced, intakeReq).Get(ctx, nil); err != nil {
		// notification failures do not fail the intake watch (same policy as
		// SendConfirmation in the booking saga)
		logger.Error("SendIntakeReminder failed", "error", err)
		state = "intake-reminder-failed"
	}

	// Phase 2: wait for IntakeCompleted until the cutoff (SPEC-W95 C5-B:
	// pack-configured lead when >0, else the T-2h legacy default).
	cutoff := clinicIntakeDeadlineLead
	if in.IntakeCutoffHours > 0 {
		cutoff = time.Duration(in.IntakeCutoffHours) * time.Hour
	}
	deadline := in.StartsAt.Add(-cutoff)
	_, signalled, cancelled = waitForSignalOrTimer(ctx, deadline.Sub(workflow.Now(ctx)), intakeCh)
	if cancelled {
		state = "stopped:cancelled"
		return nil
	}
	if signalled {
		state = "done:intake-completed"
		return nil
	}

	// Intake still incomplete inside the T-2h window → alert the staff.
	state = "alerting-staff"
	if err := workflow.ExecuteActivity(ctx, ActivityCreateStaffAlertTask, in).Get(ctx, nil); err != nil {
		state = "failed:staff-alert"
		return err
	}
	state = "done:staff-alerted"
	return nil
}

// SalonDepositWorkflow (salon pack): verifies the deposit hold with
// payments-service, sends a deposit reminder when the booking is inside the
// cancellation window without a hold, and charges the pack no-show fee when
// a NoShow signal arrives.
func SalonDepositWorkflow(ctx workflow.Context, in SalonDepositInput) error {
	logger := workflow.GetLogger(ctx)
	ctx = workflow.WithActivityOptions(ctx, sagaActivityOptions())

	state := "verifying-deposit"
	if err := workflow.SetQueryHandler(ctx, QueryState, func() (string, error) {
		return state, nil
	}); err != nil {
		return err
	}

	noShowCh := workflow.GetSignalChannel(ctx, SignalNoShow)

	// Step 1: verify the deposit hold via the payments balance. When the
	// verification call itself fails we keep the saga-reported hold state
	// (the workflow must not nag a customer because of a payments outage).
	held := in.HoldID != ""
	var verified bool
	if err := workflow.ExecuteActivity(ctx, ActivityVerifyDepositHold, in).Get(ctx, &verified); err != nil {
		logger.Warn("VerifyDepositHold failed; trusting saga hold state", "error", err)
	} else {
		held = verified
	}

	// Step 2: inside the cancellation window without a deposit → remind.
	window := time.Duration(in.CancellationWindowHours) * time.Hour
	if !held && in.CancellationWindowHours > 0 {
		windowStart := in.StartsAt.Add(-window)
		if d := windowStart.Sub(workflow.Now(ctx)); d > 0 {
			// wait for the window to open; a NoShow/cancel may arrive first
			state = "waiting-cancellation-window"
			_, signalled, cancelled := waitForSignalOrTimer(ctx, d, noShowCh)
			if cancelled {
				state = "stopped:cancelled"
				return nil
			}
			if signalled {
				return chargeNoShowFee(ctx, in, &state)
			}
		}
		state = "sending-deposit-reminder"
		// Outbound send: route through NotifyPaced (CPS token + sender
		// rotation) like every other workflow-driven notification.
		req := PacedSendRequest{Kind: PacedSendDepositReminder, Deposit: &PacedDepositReminderSend{Input: in}}
		if err := workflow.ExecuteActivity(ctx, ActivityNotifyPaced, req).Get(ctx, nil); err != nil {
			logger.Error("SendDepositReminder failed", "error", err)
			state = "deposit-reminder-failed"
		}
	}

	// Step 3: wait until the appointment end for a NoShow signal.
	state = "awaiting-outcome"
	_, signalled, cancelled := waitForSignalOrTimer(ctx, in.EndsAt.Sub(workflow.Now(ctx)), noShowCh)
	if cancelled {
		state = "stopped:cancelled"
		return nil
	}
	if !signalled {
		state = "done:completed"
		return nil
	}
	return chargeNoShowFee(ctx, in, &state)
}

// chargeNoShowFee runs the no-show fee activity when a hold exists to
// capture from; without a hold (or a zero fee) it only records the outcome.
func chargeNoShowFee(ctx workflow.Context, in SalonDepositInput, state *string) error {
	logger := workflow.GetLogger(ctx)
	if in.HoldID == "" || in.NoShowFeeCents <= 0 {
		*state = "done:no-show-no-fee"
		logger.Info("no-show without capturable hold; fee skipped", "booking_id", in.BookingID)
		return nil
	}
	*state = "charging-no-show-fee"
	if err := workflow.ExecuteActivity(ctx, ActivityChargeNoShowFee, in).Get(ctx, nil); err != nil {
		*state = "failed:no-show-fee"
		return err
	}
	*state = "done:no-show-fee-charged"
	return nil
}

// ConsultancyFollowupWorkflow (consultancy pack): after the session ends,
// sends the follow-up email and creates the CRM follow-up task, then fires
// the T-7d proposal reminder.
func ConsultancyFollowupWorkflow(ctx workflow.Context, in ConsultancyFollowupInput) error {
	logger := workflow.GetLogger(ctx)
	ctx = workflow.WithActivityOptions(ctx, sagaActivityOptions())

	state := "awaiting-session-end"
	if err := workflow.SetQueryHandler(ctx, QueryState, func() (string, error) {
		return state, nil
	}); err != nil {
		return err
	}

	// Wait for the session to end (booking cancellation stops the workflow).
	_, _, cancelled := waitForSignalOrTimer(ctx, in.EndsAt.Sub(workflow.Now(ctx)), nil)
	if cancelled {
		state = "stopped:cancelled"
		return nil
	}

	state = "sending-followup"
	// Outbound send: route through NotifyPaced (CPS token + sender rotation).
	followupReq := PacedSendRequest{Kind: PacedSendFollowUp, FollowUp: &PacedFollowupSend{Input: in}}
	if err := workflow.ExecuteActivity(ctx, ActivityNotifyPaced, followupReq).Get(ctx, nil); err != nil {
		logger.Error("SendFollowupEmail failed", "error", err)
		state = "followup-email-failed"
	}

	state = "creating-crm-followup-task"
	if err := workflow.ExecuteActivity(ctx, ActivityCreateCRMFollowupTask, in).Get(ctx, nil); err != nil {
		state = "failed:crm-followup-task"
		return err
	}

	// T-7d proposal reminder.
	state = "awaiting-proposal-reminder"
	_, _, cancelled = waitForSignalOrTimer(ctx, in.EndsAt.Add(consultancyProposalReminderDelay).Sub(workflow.Now(ctx)), nil)
	if cancelled {
		state = "stopped:cancelled"
		return nil
	}

	state = "sending-proposal-reminder"
	// Outbound send: route through NotifyPaced (CPS token + sender rotation).
	proposalReq := PacedSendRequest{Kind: PacedSendProposalReminder, Proposal: &PacedProposalReminderSend{Input: in}}
	if err := workflow.ExecuteActivity(ctx, ActivityNotifyPaced, proposalReq).Get(ctx, nil); err != nil {
		logger.Error("SendProposalReminder failed", "error", err)
		state = "proposal-reminder-failed"
		return nil
	}
	state = "done"
	return nil
}

// SupportEscalationWorkflow (support-desk pack): enforces the 4h
// first-response SLA. A Responded signal closes the ticket watch; on timeout
// the ticket is escalated (owner email + priority flag event on
// opendesk.crm.events).
func SupportEscalationWorkflow(ctx workflow.Context, in SupportEscalationInput) error {
	logger := workflow.GetLogger(ctx)
	ctx = workflow.WithActivityOptions(ctx, sagaActivityOptions())

	state := "awaiting-first-response"
	if err := workflow.SetQueryHandler(ctx, QueryState, func() (string, error) {
		return state, nil
	}); err != nil {
		return err
	}

	sla := time.Duration(in.FirstResponseSLAHours) * time.Hour
	if sla <= 0 {
		sla = defaultFirstResponseSLA
	}
	respondedCh := workflow.GetSignalChannel(ctx, SignalResponded)

	deadline := in.CreatedAt.Add(sla)
	_, responded, cancelled := waitForSignalOrTimer(ctx, deadline.Sub(workflow.Now(ctx)), respondedCh)
	if cancelled {
		state = "stopped:cancelled"
		return nil
	}
	if responded {
		state = "done:responded-within-sla"
		return nil
	}

	state = "escalating"
	// Outbound send: route through NotifyPaced (CPS token + sender rotation);
	// the escalation email + CRM priority event both happen inside the
	// EscalateTicket dispatch.
	escalateReq := PacedSendRequest{Kind: PacedSendStaffAlert, StaffAlert: &PacedStaffAlertSend{Input: in}}
	if err := workflow.ExecuteActivity(ctx, ActivityNotifyPaced, escalateReq).Get(ctx, nil); err != nil {
		state = "failed:escalate"
		return err
	}
	logger.Info("ticket escalated after SLA breach", "ticket", in.BookingID, "sla", sla.String())
	state = "done:escalated"
	return nil
}
