// Package workflows hosts the durable Temporal workflows of SPEC §6:
// BookingSagaWorkflow, ReminderWorkflow, NoShowFollowupWorkflow and
// TenantOnboardingWorkflow, plus the industry pack workflows of SPEC-CRM §C2
// (ClinicIntakeWorkflow, SalonDepositWorkflow, ConsultancyFollowupWorkflow,
// SupportEscalationWorkflow). Cross-service side effects run as activities
// via Dapr service invocation (see internal/activities).
package workflows

import "time"

// SagaInput is the input contract of BookingSagaWorkflow. It mirrors
// booking-service's bookingops.SagaInput (JSON-compatible; duplicated per
// service-boundary rules — no shared top-level package).
type SagaInput struct {
	BookingID    string    `json:"booking_id"`
	TenantID     string    `json:"tenant_id"`
	TenantSlug   string    `json:"tenant_slug"`
	OfferingID   string    `json:"offering_id"`
	TeamMemberID string    `json:"team_member_id"`
	ContactID    string    `json:"contact_id"`
	ContactPhone string    `json:"contact_phone"`
	ContactEmail string    `json:"contact_email"`
	ContactName  string    `json:"contact_name"`
	StartsAt     time.Time `json:"starts_at"`
	EndsAt       time.Time `json:"ends_at"`
	PriceCents   int64     `json:"price_cents"`
	Currency     string    `json:"currency"`
	Source       string    `json:"source"`

	// SPEC-CRM §C2/C3: industry pack id ("salon" when empty) and the deposit
	// policy resolved from the pack's bookingPolicy.
	Industry string `json:"industry,omitempty"`
	// DepositCents is ceil(price_cents * depositPercent/100). Only consulted
	// when DepositKnown is true; otherwise the saga holds the full price
	// (pre-CRM behavior).
	DepositCents int64 `json:"deposit_cents,omitempty"`
	// DepositKnown marks that the tenant's pack policy was resolved; a known
	// 0% deposit skips the hold entirely.
	DepositKnown bool `json:"deposit_known,omitempty"`
	// NoShowFeeCents and CancellationWindowHours ride along so the pack child
	// workflow does not need to reload the pack.
	NoShowFeeCents          int64 `json:"no_show_fee_cents,omitempty"`
	CancellationWindowHours int   `json:"cancellation_window_hours,omitempty"`
}

// IndustryOrDefault returns the effective industry id (default "salon").
func (in SagaInput) IndustryOrDefault() string {
	if in.Industry == "" {
		return "salon"
	}
	return in.Industry
}

// OpsAlertInput is the input of the EmitOpsAlert activity (SPEC-W34 GF16):
// fired when a saga compensation exhausts its retries — e.g. a VoidHold
// failure leaves an orphaned deposit hold that only manual ops can clear.
type OpsAlertInput struct {
	BookingID  string `json:"booking_id"`
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Activity   string `json:"activity"` // compensation activity that exhausted retries
	Error      string `json:"error"`    // final retry error text
}

// ReminderInput starts a ReminderWorkflow.
type ReminderInput struct {
	BookingID    string    `json:"booking_id"`
	TenantID     string    `json:"tenant_id"`
	TenantSlug   string    `json:"tenant_slug"`
	ContactPhone string    `json:"contact_phone"`
	ContactEmail string    `json:"contact_email"`
	ContactName  string    `json:"contact_name"`
	StartsAt     time.Time `json:"starts_at"`

	// DevOverrideDelays, when non-empty, replaces the T-24h/T-1h reminder
	// offsets (used by POST /dev/trigger-reminder for manual testing).
	DevOverrideDelays []time.Duration `json:"dev_override_delays,omitempty"`
}

// NoShowInput starts a NoShowFollowupWorkflow.
type NoShowInput struct {
	BookingID    string    `json:"booking_id"`
	TenantID     string    `json:"tenant_id"`
	TenantSlug   string    `json:"tenant_slug"`
	ContactPhone string    `json:"contact_phone"`
	ContactEmail string    `json:"contact_email"`
	EndsAt       time.Time `json:"ends_at"`
}

// OnboardingInput starts a TenantOnboardingWorkflow.
type OnboardingInput struct {
	TenantID string `json:"tenant_id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Plan     string `json:"plan"`
	// Industry selects the pack applied by the ApplyIndustryPack activity
	// (SPEC-CRM §C2). Empty means "salon".
	Industry string `json:"industry,omitempty"`
}

// IndustryOrDefault returns the effective industry id (default "salon").
func (in OnboardingInput) IndustryOrDefault() string {
	if in.Industry == "" {
		return "salon"
	}
	return in.Industry
}

// BookingEventSignal is delivered to ReminderWorkflow / BookingSagaWorkflow
// when the booking changes underneath them.
type BookingEventSignal struct {
	Type        string    `json:"type"` // cancelled | rescheduled | confirmed
	NewStartsAt time.Time `json:"new_starts_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Industry pack workflow inputs (SPEC-CRM §C2)
// ---------------------------------------------------------------------------

// ClinicIntakeInput starts a ClinicIntakeWorkflow.
type ClinicIntakeInput struct {
	BookingID    string    `json:"booking_id"`
	TenantID     string    `json:"tenant_id"`
	TenantSlug   string    `json:"tenant_slug"`
	ContactName  string    `json:"contact_name"`
	ContactEmail string    `json:"contact_email"`
	ContactPhone string    `json:"contact_phone,omitempty"`
	StartsAt     time.Time `json:"starts_at"`
}

// SalonDepositInput starts a SalonDepositWorkflow.
type SalonDepositInput struct {
	BookingID    string    `json:"booking_id"`
	TenantID     string    `json:"tenant_id"`
	TenantSlug   string    `json:"tenant_slug"`
	ContactName  string    `json:"contact_name"`
	ContactEmail string    `json:"contact_email"`
	ContactPhone string    `json:"contact_phone,omitempty"`
	StartsAt     time.Time `json:"starts_at"`
	EndsAt       time.Time `json:"ends_at"`
	// HoldID is the payments hold identifier produced by the saga's
	// HoldDeposit step (empty when no deposit was held).
	HoldID       string `json:"hold_id,omitempty"`
	DepositCents int64  `json:"deposit_cents,omitempty"`
	// NoShowFeeCents is charged on a NoShow signal when a hold exists.
	NoShowFeeCents int64  `json:"no_show_fee_cents,omitempty"`
	Currency       string `json:"currency"`
	// CancellationWindowHours comes from the pack bookingPolicy.
	CancellationWindowHours int `json:"cancellation_window_hours,omitempty"`
}

// ConsultancyFollowupInput starts a ConsultancyFollowupWorkflow.
type ConsultancyFollowupInput struct {
	BookingID    string    `json:"booking_id"`
	TenantID     string    `json:"tenant_id"`
	TenantSlug   string    `json:"tenant_slug"`
	ContactName  string    `json:"contact_name"`
	ContactEmail string    `json:"contact_email"`
	ContactPhone string    `json:"contact_phone,omitempty"`
	OfferingName string    `json:"offering_name,omitempty"`
	EndsAt       time.Time `json:"ends_at"`
}

// SupportEscalationInput starts a SupportEscalationWorkflow.
type SupportEscalationInput struct {
	BookingID    string    `json:"booking_id"` // doubles as the ticket reference
	TenantID     string    `json:"tenant_id"`
	TenantSlug   string    `json:"tenant_slug"`
	ContactName  string    `json:"contact_name"`
	ContactEmail string    `json:"contact_email"`
	CreatedAt    time.Time `json:"created_at"`
	// FirstResponseSLAHours defaults to 4 (support-desk pack SLA).
	FirstResponseSLAHours int `json:"first_response_sla_hours,omitempty"`
}

// Signal and query names.
const (
	SignalBookingEvent    = "booking-event"
	SignalCancel          = "cancel"
	SignalIntakeCompleted = "IntakeCompleted"
	SignalNoShow          = "NoShow"
	SignalResponded       = "Responded"
	QueryState            = "state"
)
