package workflows

// Outbound CPS pacing (docs/VOICE-SCALING.md §4 telephony plane).
//
// Workflows are deterministic and must never sleep or rate-limit inline, so
// pacing lives activity-side: every outbound send goes through the single
// NotifyPaced activity, which acquires a CPS token and rotates the sender
// number (internal/pacer) BEFORE dispatching to the underlying send
// activity. Workflows only build a PacedSendRequest.
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

	// ActivitySendGeoCampaignMessage is the name of the geo campaign send
	// activity.
	ActivitySendGeoCampaignMessage = "SendGeoCampaignMessage"

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
