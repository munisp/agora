package workflows

import (
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Activity names registered by the worker (see internal/activities).
const (
	ActivityReserveSlot      = "ReserveSlot"
	ActivityHoldDeposit      = "HoldDeposit"
	ActivityConfirmBooking   = "ConfirmBooking"
	ActivitySendConfirmation = "SendConfirmation"
	ActivitySendReminder     = "SendReminder"
	ActivityReleaseSlot      = "ReleaseSlot"
	ActivityVoidHold         = "VoidHold"
	ActivityGetBookingStatus = "GetBookingStatus"
	// SPEC-W34 GF16: CRITICAL ops alert when a saga compensation exhausts
	// its retries (e.g. orphaned deposit hold — holds have no TB timeout).
	ActivityEmitOpsAlert = "EmitOpsAlert"
	ActivityMarkNoShow       = "MarkNoShow"
	ActivitySendNoShowFollow = "SendNoShowFollowup"

	ActivityEnsureKeycloakGroup = "EnsureKeycloakGroup"
	ActivityEnsurePermifyTenant = "EnsurePermifyTenant"
	ActivitySeedTenantData      = "SeedTenantData"
	ActivityEnsureSearchAlias   = "EnsureSearchAlias"

	// SPEC-CRM §C2 industry pack activities.
	ActivityApplyIndustryPack     = "ApplyIndustryPack"
	ActivityVerifyDepositHold     = "VerifyDepositHold"
	ActivitySendDepositReminder   = "SendDepositReminder"
	ActivityChargeNoShowFee       = "ChargeNoShowFee"
	ActivitySendIntakeReminder    = "SendIntakeReminder"
	ActivityCreateStaffAlertTask  = "CreateStaffAlertTask"
	ActivitySendFollowupEmail     = "SendFollowupEmail"
	ActivityCreateCRMFollowupTask = "CreateCRMFollowupTask"
	ActivitySendProposalReminder  = "SendProposalReminder"
	ActivityEscalateTicket        = "EscalateTicket"
)

// PackWorkflowForIndustry maps an industry id to its pack workflow name
// (SPEC-CRM §C2). Unknown industries fall back to SalonDepositWorkflow.
func PackWorkflowForIndustry(industry string) string {
	switch industry {
	case "clinic":
		return "ClinicIntakeWorkflow"
	case "consultancy":
		return "ConsultancyFollowupWorkflow"
	case "support-desk":
		return "SupportEscalationWorkflow"
	default:
		return "SalonDepositWorkflow"
	}
}

// sagaActivityOptions applies to booking/payments side effects: bounded
// retries with backoff, then the saga compensates.
func sagaActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		HeartbeatTimeout:    10 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3,
		},
	}
}

// BookingSagaWorkflow implements the booking saga of SPEC §6:
//
//	ReserveSlot (booking-svc) → HoldDeposit (payments-svc, when priced)
//	  → ConfirmBooking (booking-svc) → SendConfirmation (notification)
//
// with explicit compensation order (reverse): VoidHold → ReleaseSlot.
// A "cancel" signal compensates and terminates the saga; the "state" query
// reports progress.
func BookingSagaWorkflow(ctx workflow.Context, in SagaInput) error {
	logger := workflow.GetLogger(ctx)
	ao := sagaActivityOptions()
	ctx = workflow.WithActivityOptions(ctx, ao)

	state := "started"
	if err := workflow.SetQueryHandler(ctx, QueryState, func() (string, error) {
		return state, nil
	}); err != nil {
		return err
	}

	// cancel signal support
	cancelled := false
	signalCh := workflow.GetSignalChannel(ctx, SignalCancel)
	workflow.Go(ctx, func(gctx workflow.Context) {
		signalCh.Receive(gctx, nil)
		cancelled = true
	})

	// compensations run in reverse order on failure/cancel
	type compensation struct {
		name string
		fn   func(ctx workflow.Context) error
	}
	var compensations []compensation
	// compensate runs the registered compensations in reverse order and
	// returns the number that permanently failed (retries exhausted).
	compensate := func() (failed int) {
		// disconnected context: compensations must run even on cancellation
		dctx, _ := workflow.NewDisconnectedContext(ctx)
		for i := len(compensations) - 1; i >= 0; i-- {
			c := compensations[i]
			state = "compensating:" + c.name
			if err := c.fn(dctx); err != nil {
				failed++
				// SPEC-W34 GF16: a compensation that exhausts its retries
				// (e.g. VoidHold) leaves an orphaned deposit hold — holds
				// have no TB timeout. CRITICAL structured log + ops alert;
				// the saga must NOT report "compensated".
				logger.Error("CRITICAL: saga compensation exhausted retries — manual ops intervention required",
					"activity", c.name, "booking_id", in.BookingID,
					"tenant_id", in.TenantID, "error", err.Error())
				alertErr := workflow.ExecuteActivity(dctx, ActivityEmitOpsAlert, OpsAlertInput{
					BookingID:  in.BookingID,
					TenantID:   in.TenantID,
					TenantSlug: in.TenantSlug,
					Activity:   c.name,
					Error:      err.Error(),
				}).Get(dctx, nil)
				if alertErr != nil {
					logger.Error("CRITICAL: ops alert emission failed after compensation exhaustion",
						"activity", c.name, "booking_id", in.BookingID, "error", alertErr.Error())
				}
			}
		}
		if failed > 0 {
			// Distinct queryable state so ops can find sagas with orphaned
			// resources (SPEC-W34 GF16) — never "compensated" when a
			// compensation actually failed. Callers must not overwrite this
			// with a generic "failed:<step>" state.
			state = "compensation_failed"
		} else {
			state = "compensated"
		}
		return failed
	}

	// Step 1: ReserveSlot
	state = "reserving-slot"
	if err := workflow.ExecuteActivity(ctx, ActivityReserveSlot, in).Get(ctx, nil); err != nil {
		state = "failed:reserve-slot"
		return temporal.NewApplicationError("ReserveSlot failed", "SagaStepFailed", err)
	}
	compensations = append(compensations, compensation{ActivityReleaseSlot, func(c workflow.Context) error {
		return workflow.ExecuteActivity(c, ActivityReleaseSlot, in, "saga_compensation").Get(c, nil)
	}})
	if cancelled {
		compensate()
		return temporal.NewCanceledError("booking cancelled by signal")
	}

	// Step 2: HoldDeposit (only for priced offerings with a required deposit)
	var holdID string
	if depositRequired(in) {
		state = "holding-deposit"
		err := workflow.ExecuteActivity(ctx, ActivityHoldDeposit, in).Get(ctx, &holdID)
		if err != nil {
			if compensate() == 0 {
				state = "failed:hold-deposit"
			}
			return temporal.NewApplicationError("HoldDeposit failed", "SagaStepFailed", err)
		}
		compensations = append(compensations, compensation{ActivityVoidHold, func(c workflow.Context) error {
			return workflow.ExecuteActivity(c, ActivityVoidHold, in, holdID).Get(c, nil)
		}})
	}
	if cancelled {
		compensate()
		return temporal.NewCanceledError("booking cancelled by signal")
	}

	// Step 3: ConfirmBooking
	state = "confirming-booking"
	if err := workflow.ExecuteActivity(ctx, ActivityConfirmBooking, in).Get(ctx, nil); err != nil {
		if compensate() == 0 {
			state = "failed:confirm-booking"
		}
		return temporal.NewApplicationError("ConfirmBooking failed", "SagaStepFailed", err)
	}
	if cancelled {
		compensate()
		return temporal.NewCanceledError("booking cancelled by signal")
	}

	// Step 4: SendConfirmation (non-compensable; failure does not roll back a
	// confirmed booking — notification retries via its own retry policy).
	// Outbound send: route through NotifyPaced (CPS token + sender rotation)
	// like every other workflow-driven notification.
	state = "sending-confirmation"
	confirmReq := PacedSendRequest{Kind: PacedSendConfirmation, Confirmation: &PacedConfirmationSend{Input: in}}
	if err := workflow.ExecuteActivity(ctx, ActivityNotifyPaced, confirmReq).Get(ctx, nil); err != nil {
		logger.Error("SendConfirmation failed; booking remains confirmed", "error", err)
		state = "confirmed:notification-failed"
	} else {
		state = "confirmed"
	}

	// Start the reminder + no-show follow-up workflows as children.
	// ParentClosePolicy ABANDON: the saga returns right after starting them;
	// they must keep running (reminders/no-show fire hours to days later).
	childOpts := workflow.ChildWorkflowOptions{
		WorkflowID:        "reminder-" + in.BookingID,
		ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON,
	}
	reminderCtx := workflow.WithChildOptions(ctx, childOpts)
	workflow.ExecuteChildWorkflow(reminderCtx, "ReminderWorkflow", ReminderInput{
		BookingID:    in.BookingID,
		TenantID:     in.TenantID,
		TenantSlug:   in.TenantSlug,
		ContactPhone: in.ContactPhone,
		ContactEmail: in.ContactEmail,
		ContactName:  in.ContactName,
		StartsAt:     in.StartsAt,
	})

	noshowCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:        "noshow-" + in.BookingID,
		ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON,
	})
	workflow.ExecuteChildWorkflow(noshowCtx, "NoShowFollowupWorkflow", NoShowInput{
		BookingID:    in.BookingID,
		TenantID:     in.TenantID,
		TenantSlug:   in.TenantSlug,
		ContactPhone: in.ContactPhone,
		ContactEmail: in.ContactEmail,
		EndsAt:       in.EndsAt,
	})

	// SPEC-CRM §C2: start the industry pack workflow as a child. The pack
	// workflow name is resolved from the saga's industry (default salon).
	// Pack workflows are compensation-safe: they only notify / create tasks
	// and stop on a booking-event "cancelled" signal.
	packName := PackWorkflowForIndustry(in.IndustryOrDefault())
	packCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:        "pack-" + in.BookingID,
		ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON,
	})
	workflow.ExecuteChildWorkflow(packCtx, packName, packWorkflowInput(packName, in, holdID))
	logger.Info("industry pack workflow started", "workflow", packName, "industry", in.IndustryOrDefault())

	return nil
}

// depositRequired reports whether the saga must hold a deposit: full-price
// hold for priced offerings when no pack policy was resolved (legacy
// behavior), pack deposit amount otherwise (0% means no hold).
func depositRequired(in SagaInput) bool {
	if in.PriceCents <= 0 {
		return false
	}
	if in.DepositKnown {
		return in.DepositCents > 0
	}
	return true
}

// packWorkflowInput builds the typed input of the pack child workflow.
func packWorkflowInput(packName string, in SagaInput, holdID string) any {
	switch packName {
	case "ClinicIntakeWorkflow":
		return ClinicIntakeInput{
			BookingID: in.BookingID, TenantID: in.TenantID, TenantSlug: in.TenantSlug,
			ContactName: in.ContactName, ContactEmail: in.ContactEmail, ContactPhone: in.ContactPhone,
			StartsAt: in.StartsAt,
		}
	case "ConsultancyFollowupWorkflow":
		return ConsultancyFollowupInput{
			BookingID: in.BookingID, TenantID: in.TenantID, TenantSlug: in.TenantSlug,
			ContactName: in.ContactName, ContactEmail: in.ContactEmail, ContactPhone: in.ContactPhone,
			EndsAt: in.EndsAt,
		}
	case "SupportEscalationWorkflow":
		return SupportEscalationInput{
			BookingID: in.BookingID, TenantID: in.TenantID, TenantSlug: in.TenantSlug,
			ContactName: in.ContactName, ContactEmail: in.ContactEmail,
			CreatedAt: in.StartsAt, FirstResponseSLAHours: 4,
		}
	default: // SalonDepositWorkflow
		return SalonDepositInput{
			BookingID: in.BookingID, TenantID: in.TenantID, TenantSlug: in.TenantSlug,
			ContactName: in.ContactName, ContactEmail: in.ContactEmail, ContactPhone: in.ContactPhone,
			StartsAt: in.StartsAt, EndsAt: in.EndsAt,
			HoldID: holdID, DepositCents: in.DepositCents,
			NoShowFeeCents: in.NoShowFeeCents, Currency: in.Currency,
			CancellationWindowHours: in.CancellationWindowHours,
		}
	}
}
