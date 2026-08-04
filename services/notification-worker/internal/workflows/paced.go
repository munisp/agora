package workflows

// Outbound CPS pacing (docs/VOICE-SCALING.md §4 telephony plane).
//
// Workflows are deterministic and must never sleep or rate-limit inline, so
// pacing lives activity-side: every outbound send goes through the single
// NotifyPaced activity, which acquires a CPS token and rotates the sender
// number (internal/pacer) BEFORE dispatching to the underlying send
// activity. Workflows only build a PacedSendRequest.

import (
	"time"

	"github.com/opendesk/notification-worker/internal/pacer"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	// ActivityNotifyPaced is the name of the pacing wrapper activity.
	ActivityNotifyPaced = "NotifyPaced"

	// PacedSendWaitlistClaim routes to SendWaitlistClaimNotification
	// (SPEC-W3 §3 innovation 7 waitlist backfill).
	PacedSendWaitlistClaim = "waitlist_claim"
	// PacedSendReminder routes to SendReminder (T-24h / T-1h reminders).
	PacedSendReminder = "reminder"
	// PacedSendDepositReminder routes to SendDepositReminder (salon pack:
	// missing-deposit nudge inside the cancellation window).
	PacedSendDepositReminder = "deposit_reminder"
	// PacedSendNoShow routes to SendNoShowFollowup (SPEC §6 no-show
	// follow-up message).
	PacedSendNoShow = "noshow_followup"
	// PacedSendConfirmation routes to SendConfirmation (booking saga step 4:
	// email + SMS confirmation after ConfirmBooking).
	PacedSendConfirmation = "confirmation"
	// PacedSendIntakeReminder routes to SendIntakeReminder (clinic pack:
	// T-72h intake form link).
	PacedSendIntakeReminder = "intake_reminder"
	// PacedSendFollowUp routes to SendFollowupEmail (consultancy pack:
	// post-session follow-up).
	PacedSendFollowUp = "follow_up"
	// PacedSendProposalReminder routes to SendProposalReminder (consultancy
	// pack: T+7d proposal-due reminder to staff).
	PacedSendProposalReminder = "proposal_reminder"
	// PacedSendStaffAlert routes to EscalateTicket (support-desk pack:
	// SLA-breach escalation email + CRM priority event).
	PacedSendStaffAlert = "staff_alert"
	// PacedSendGeoCampaign routes to SendGeoCampaignMessage (SPEC-W8 A2:
	// geo-targeted campaign sends, scheduled by booking-service's
	// GeoCampaignWorkflow on this task queue).
	PacedSendGeoCampaign = "geo_campaign"
	// PacedSendIncidentAlert routes to SendIncidentAlert (SPEC-W11 Part B
	// §5: critical/high-severity incident outreach, scheduled by
	// booking-service's IncidentAlertWorkflow). Always sent with
	// Priority=true — the pacer fast-lane bypasses the CPS token bucket
	// (still metered).
	PacedSendIncidentAlert = "incident_alert"
	// PacedSendPushNotification routes to SendPushNotification (SPEC-W16
	// contract §1): TRANSACTIONAL-class mobile/web push (booking lifecycle,
	// security) — never DND-suppressed, never quiet-hours deferred.
	PacedSendPushNotification = "push_notification"
	// PacedSendPushMarketing routes to SendPushNotification (SPEC-W16
	// contract §1): MARKETING-class push (promos/campaigns) — DND-suppressed
	// (when the payload carries a phone) and quiet-hours deferred on the
	// "push" channel, exactly like the sms marketing kinds.
	PacedSendPushMarketing = "push_marketing"

	// ActivitySendGeoCampaignMessage is the name of the geo campaign send
	// activity.
	ActivitySendGeoCampaignMessage = "SendGeoCampaignMessage"

	// ActivitySendPushNotification is the name of the push notification
	// fan-out activity (SPEC-W16 contract §1).
	ActivitySendPushNotification = "SendPushNotification"

	// ActivitySendIncidentAlert is the name of the incident alert send
	// activity (SPEC-W11 Part B §5).
	ActivitySendIncidentAlert = "SendIncidentAlert"
)

// PacedSendRequest is the payload of the NotifyPaced activity: which send
// to perform after the CPS token is granted, plus its arguments.
type PacedSendRequest struct {
	Kind string `json:"kind"` // PacedSend* constant
	// Priority engages the pacer fast-lane (SPEC-W11 Part B §5): the send
	// dispatches IMMEDIATELY, bypassing the CPS token bucket — but is still
	// metered/counted. Reserved for emergency-grade traffic (incident_alert).
	Priority      bool                       `json:"priority,omitempty"`
	Waitlist      *PacedWaitlistSend         `json:"waitlist,omitempty"`
	Reminder      *PacedReminderSend         `json:"reminder,omitempty"`
	Deposit       *PacedDepositReminderSend  `json:"deposit,omitempty"`
	NoShow        *PacedNoShowSend           `json:"noshow,omitempty"`
	Confirmation  *PacedConfirmationSend     `json:"confirmation,omitempty"`
	Intake        *PacedIntakeReminderSend   `json:"intake,omitempty"`
	FollowUp      *PacedFollowupSend         `json:"follow_up,omitempty"`
	Proposal      *PacedProposalReminderSend `json:"proposal,omitempty"`
	StaffAlert    *PacedStaffAlertSend       `json:"staff_alert,omitempty"`
	GeoCampaign   *PacedGeoCampaignSend      `json:"geo_campaign,omitempty"`
	IncidentAlert *PacedIncidentAlertSend    `json:"incident_alert,omitempty"`
	// Push carries the SendPushNotification arguments for the push kinds
	// (push_notification / push_marketing share one payload shape).
	Push *PacedPushNotificationSend `json:"push,omitempty"`
}

// PushTarget is one explicit device token in a push payload (SPEC-W16
// contract §1). An explicit Tokens list skips the booking-service device
// fetch entirely.
type PushTarget struct {
	Token    string `json:"token"`
	Platform string `json:"platform"` // android | ios | web (empty treated as android/fcm)
}

// PacedPushNotificationSend carries the SendPushNotification arguments
// (SPEC-W16 contract §1). The JSON contract is duplicated by any scheduling
// service (service boundary: duplicated, not shared) — keep the field tags
// in sync.
//
// Tokens are resolved in order:
//  1. Tokens (explicit list) — used as-is; no booking-service call;
//  2. ContactID — the activity fetches the contact's device tokens from
//     booking-service via Dapr invoke GET /internal/devices?contact_id=
//     (response: JSON array of {tenant_id, contact_id, token, platform,
//     app}); App then filters client-side.
type PacedPushNotificationSend struct {
	TenantSlug string       `json:"tenant_slug"`
	ContactID  string       `json:"contact_id,omitempty"`
	Tokens     []PushTarget `json:"tokens,omitempty"`
	// Phone is OPTIONAL: when present it lets the DND guard check
	// push_marketing sends against the NCC 2442 / tenant opt-out registries
	// (which are phone-keyed; device tokens are not). Without it a
	// push_marketing send passes the DND guard with the existing
	// no-recipient warn — quiet-hours deferral still applies.
	Phone string            `json:"phone,omitempty"`
	Title string            `json:"title"`
	Body  string            `json:"body"`
	Data  map[string]string `json:"data,omitempty"`
	// App optionally restricts fetched device tokens to one app
	// ("admin" | "field"); empty = all of the contact's devices.
	App string `json:"app,omitempty"`
}

// PushTokenResult is the per-token outcome of one SendPushNotification
// fan-out. Unregistered flags tokens the provider reported as gone
// (FCM UNREGISTERED / NotRegistered) — callers should prune them via
// booking-service DELETE /v1/devices/{token}.
type PushTokenResult struct {
	Token        string `json:"token"`
	Platform     string `json:"platform"`
	Provider     string `json:"provider"` // fcm | apns | "" when unroutable
	Success      bool   `json:"success"`
	StatusCode   int    `json:"status_code,omitempty"`
	Unregistered bool   `json:"unregistered,omitempty"`
	Error        string `json:"error,omitempty"`
}

// PushNotificationResult is the outcome of one SendPushNotification call:
// per-token results plus counters. Individual token failures are NOT
// activity errors (Temporal retries must not re-deliver to the tokens that
// already succeeded); the activity errors only on contract-level failures
// (missing payload fields, device-fetch failure).
type PushNotificationResult struct {
	Sent    int               `json:"sent"`
	Failed  int               `json:"failed"`
	Results []PushTokenResult `json:"results"`
}

// PacedIncidentAlertSend carries the SendIncidentAlert arguments
// (SPEC-W11 Part B §5). The JSON contract is duplicated by booking-service's
// internal/incidents package (service boundary: duplicated, not shared) —
// keep the field tags in sync.
type PacedIncidentAlertSend struct {
	TenantSlug string `json:"tenant_slug"`
	IncidentID string `json:"incident_id"`
	Channel    string `json:"channel"` // whatsapp | telegram | sms
	Phone      string `json:"phone"`
	Text       string `json:"text"` // template rendered by booking-service (ref + type + address)
}

// PacedGeoCampaignSend carries the SendGeoCampaignMessage arguments
// (SPEC-W8 A2). The JSON contract is duplicated by booking-service's
// internal/geo package (service boundary: duplicated, not shared) — keep
// the field tags in sync.
type PacedGeoCampaignSend struct {
	TenantSlug string `json:"tenant_slug"`
	CampaignID string `json:"campaign_id"`
	Channel    string `json:"channel"` // whatsapp | telegram | sms
	Phone      string `json:"phone"`
	Name       string `json:"name"`
	Text       string `json:"text"` // {name} already substituted by the workflow
}

// PacedWaitlistSend carries the SendWaitlistClaimNotification arguments.
type PacedWaitlistSend struct {
	Input WaitlistBackfillInput `json:"input"`
	Entry WaitlistEntry         `json:"entry"`
}

// PacedReminderSend carries the SendReminder arguments.
type PacedReminderSend struct {
	Input ReminderInput `json:"input"`
	Kind  string        `json:"kind"` // e.g. "24h0m0s", "1h0m0s"
}

// PacedDepositReminderSend carries the SendDepositReminder arguments.
type PacedDepositReminderSend struct {
	Input SalonDepositInput `json:"input"`
}

// PacedNoShowSend carries the SendNoShowFollowup arguments.
type PacedNoShowSend struct {
	Input NoShowInput `json:"input"`
}

// PacedConfirmationSend carries the SendConfirmation arguments.
type PacedConfirmationSend struct {
	Input SagaInput `json:"input"`
}

// PacedIntakeReminderSend carries the SendIntakeReminder arguments.
type PacedIntakeReminderSend struct {
	Input ClinicIntakeInput `json:"input"`
}

// PacedFollowupSend carries the SendFollowupEmail arguments.
type PacedFollowupSend struct {
	Input ConsultancyFollowupInput `json:"input"`
}

// PacedProposalReminderSend carries the SendProposalReminder arguments.
type PacedProposalReminderSend struct {
	Input ConsultancyFollowupInput `json:"input"`
}

// PacedStaffAlertSend carries the EscalateTicket arguments.
type PacedStaffAlertSend struct {
	Input SupportEscalationInput `json:"input"`
}

// ---------------------------------------------------------------------------
// SPEC-W12 Agent B: DND/quiet-hours compliance guards
// ---------------------------------------------------------------------------

// Paced send completion statuses.
const (
	// PacedSendStatusSent means NotifyPaced dispatched the send to its
	// channel binding.
	PacedSendStatusSent = "sent"
	// PacedSendStatusSuppressedDND means the DND guard (SPEC-W12 §3) stopped
	// a marketing send: the recipient is on the NCC 2442 global list or the
	// tenant's opt-out list. The send consumed no CPS token and the
	// suppression was counted (notifications_suppressed_total{reason}) and
	// logged. Workflows that record send outcomes should complete the send
	// with this status.
	PacedSendStatusSuppressedDND = "suppressed_dnd"
)

// PacedSendResult is the outcome of one NotifyPaced call (SPEC-W12). It is
// additive: callers that only care about errors keep using Get(ctx, nil).
type PacedSendResult struct {
	Status string `json:"status"` // sent | suppressed_dnd
	// Reason is the suppression reason (tenant_optout | global_dnd) when
	// Status is suppressed_dnd.
	Reason string `json:"reason,omitempty"`
}

// PacedSendChannel extracts the delivery channel of a paced send (used for
// per-channel quiet-hours overrides); "" when the kind carries no channel.
func PacedSendChannel(req PacedSendRequest) string {
	switch req.Kind {
	case PacedSendGeoCampaign:
		if req.GeoCampaign != nil {
			return req.GeoCampaign.Channel
		}
	case PacedSendIncidentAlert:
		if req.IncidentAlert != nil {
			return req.IncidentAlert.Channel
		}
	case PacedSendPushNotification, PacedSendPushMarketing:
		// Fixed channel key: QUIET_HOURS_OVERRIDES may carry a "push"
		// window (SPEC-W16 §1 — push_marketing is quiet-hours deferred
		// like the sms marketing kinds).
		return "push"
	}
	return ""
}

// GuardedPacedSend executes a paced send with the SPEC-W12 §3 compliance
// guards applied workflow-side:
//
//   - MARKETING kinds (geo_campaign, promo, broadcast, drip — the explicit
//     classification table lives in internal/pacer/guards.go) arriving inside
//     the tenant's quiet-hours window are DEFERRED: the workflow durably
//     Sleeps until the window opens (default 20:00-08:00 Africa/Lagos,
//     per-channel overrides via quiet.Overrides), then sends.
//   - TRANSACTIONAL kinds (booking confirmations, reminders, incident_alert,
//     otp, ...) pass immediately — no sleep, ever.
//   - The Priority fast-lane (SPEC-W11 Part B §5, incident_alert) is NOT
//     altered: priority sends skip this deferral exactly as they skip the
//     CPS token bucket.
//
// DND suppression itself is activity-side (NotifyPaced checks the registry
// before acquiring a CPS token); the result carries suppressed_dnd for the
// scheduling workflow to record.
//
// quiet is passed in by the SCHEDULING workflow (built from its input or the
// QUIET_HOURS_* env at schedule time) so replay stays deterministic when the
// env changes between runs of the same workflow. The caller must have
// configured ActivityOptions on ctx (StartToCloseTimeout etc.), exactly as
// the existing paced-send workflows do before ExecuteActivity.
func GuardedPacedSend(ctx workflow.Context, req PacedSendRequest, quiet pacer.QuietHoursConfig) (PacedSendResult, error) {
	var res PacedSendResult
	if pacer.ClassifyKind(req.Kind) == pacer.ClassMarketing && !req.Priority {
		open, inWindow, err := pacer.QuietHoursOpenAt(workflow.Now(ctx), PacedSendChannel(req), quiet)
		if err != nil {
			return res, err
		}
		if inWindow {
			delay := open.Sub(workflow.Now(ctx))
			if delay > 0 {
				workflow.GetLogger(ctx).Info("quiet hours: deferring marketing send until window opens",
					"kind", req.Kind, "channel", PacedSendChannel(req),
					"window_open", open.String(), "delay", delay.String())
				if err := workflow.Sleep(ctx, delay); err != nil {
					return res, err
				}
			}
		}
	}
	if err := workflow.ExecuteActivity(ctx, ActivityNotifyPaced, req).Get(ctx, &res); err != nil {
		return res, err
	}
	if res.Status == "" {
		// Older workers returned no result payload; the send happened.
		res.Status = PacedSendStatusSent
	}
	return res, nil
}

// WorkflowTypePacedSend is the registered name of the fire-and-forget
// paced send workflow (SPEC-W19 integrator): the notifyoutbox consumer
// starts one per com.opendesk.notifications.PacedSend CloudEvent on
// opendesk.notifications.outbox (producer today: booking-service
// field-service dispatch push, kind push_notification — TRANSACTIONAL,
// so no quiet-hours deferral and no DND suppression).
const WorkflowTypePacedSend = "PacedSendWorkflow"

// PacedSendWorkflow executes ONE PacedSendRequest through the guarded
// paced path (GuardedPacedSend → the NotifyPaced activity). It exists so
// fire-and-forget producers that cannot start workflow-scoped sends of
// their own (Kafka commands on the notifications outbox) still get the
// CPS pacer + sender rotation + SPEC-W12 compliance guards instead of a
// raw binding call. The workflow ID is derived from the CloudEvent id by
// the caller, so a redelivered command can never send twice
// (WorkflowExecutionAlreadyStarted is tolerated consumer-side).
func PacedSendWorkflow(ctx workflow.Context, req PacedSendRequest) (PacedSendResult, error) {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		HeartbeatTimeout:    30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)
	// Contract defaults (20:00-08:00 Africa/Lagos, SPEC-W12 §8); the
	// outbox command carries no per-tenant override, exactly like the
	// other env-configured senders.
	return GuardedPacedSend(ctx, req, QuietHoursFromEnv("", "", nil))
}

// QuietHoursFromEnv is a convenience for workflows that accept the
// quiet-hours configuration as plain strings in their input: it builds the
// config handed to GuardedPacedSend. tz defaults to Africa/Lagos, window to
// 20:00-08:00 (SPEC-W12 §8).
func QuietHoursFromEnv(defaultWindow, tz string, overrides map[string]string) pacer.QuietHoursConfig {
	if defaultWindow == "" {
		defaultWindow = pacer.DefaultQuietHoursWindow
	}
	if tz == "" {
		tz = pacer.DefaultQuietHoursTimezone
	}
	return pacer.QuietHoursConfig{DefaultWindow: defaultWindow, Overrides: overrides, Timezone: tz}
}
