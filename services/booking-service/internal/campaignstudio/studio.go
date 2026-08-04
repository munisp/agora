// Package campaignstudio implements SPEC-W19 Agent D: the Campaign Studio
// enterprise app (segments, journeys, enrollments) as a SELF-CONTAINED
// booking-service package per the W19 anti-collision architecture. The
// integrator wires Deps + RegisterRoutes + the Temporal worker registration
// (RegisterWorker) — this package touches no shared files.
//
// Model (PostgreSQL, RLS tenant_isolation on every table, mirroring the
// W16 devices store idiom):
//
//	studio_segments     {id, tenant_id, name, definition jsonb, approx_count, created_at, updated_at}
//	studio_journeys     {id, tenant_id, name, status, trigger_kind, segment_id, steps jsonb, created_at, updated_at}
//	studio_enrollments  {id, tenant_id, journey_id, contact_id, step_idx, state, enrolled_at, last_step_at, exited_reason}
//	studio_step_events  {id, tenant_id, journey_id, enrollment_id, step_idx, kind, payload jsonb, created_at}
//
// Execution is OPERATOR/CRON-TRIGGERED: POST /v1/studio/journeys/{id}/step
// advances due enrollments one step (wait → time check; send → paced send
// via the notification-worker NotifyPaced contract, marketing kinds only;
// branch → condition eval). Wiring a Temporal cron schedule that calls the
// step endpoint periodically is an honest OPS FOLLOW-UP (documented in
// docs/apps/campaign-studio.md).
//
// Config envs (for the integrator — this package adds NO config code;
// zero-config safe defaults keep the app functional):
//
//	STUDIO_DATABASE_URL   postgres DSN for the dedicated store pool
//	                      (DialStore; integrator may fall back to DATABASE_URL)
//	STUDIO_STEP_BATCH     max enrollments advanced per step call (default 200)
//	STUDIO_EVENTS_TOPIC   CloudEvents topic for journey lifecycle events
//	                      (default "opendesk.studio.events.v1"; empty disables)
//	USAGE_EVENTS_TOPIC    existing metering topic (opendesk.usage.events);
//	                      empty disables the journey_enrolled meter
//	QUIET_HOURS_DEFAULT / QUIET_HOURS_OVERRIDES
//	                      reused SPEC-W12 §8 quiet-hours config, captured at
//	                      step time and handed to the send workflow
//
// Entitlement: the integrator gates /v1/studio with appgate app_id
// "campaign-studio". Perms: manage_bookings for writes, view_analytics for
// reads (wired via Deps.RequireWrite / Deps.RequireRead).
package campaignstudio

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidInput marks deterministic validation failures (400 at the API).
var ErrInvalidInput = errors.New("invalid campaign studio input")

// ErrConflict marks state-machine / state-precondition failures (409).
var ErrConflict = errors.New("campaign studio state conflict")

// ---------------------------------------------------------------------------
// Segments
// ---------------------------------------------------------------------------

// Segment filter operators (definition jsonb {filters:[{field,op,value}]}).
const (
	OpEq       = "eq"
	OpNeq      = "neq"
	OpIn       = "in"
	OpGte      = "gte"
	OpLte      = "lte"
	OpContains = "contains"
)

// Segment filter fields (whitelist — the SQL builder maps these to column
// expressions; anything else is rejected so definitions can never inject
// SQL). Contact fields read booking.contacts (id, tenant_id, name, phone,
// email, notes + the SPEC-CRM source/external_id columns). Lead fields read
// booking.leads via the phone join (leads have no contact FK — documented
// in docs/apps/campaign-studio.md).
const (
	FieldName       = "name"
	FieldPhone      = "phone"
	FieldEmail      = "email"
	FieldSource     = "source"
	FieldExternalID = "external_id"

	FieldLeadStatus     = "lead_status"
	FieldLeadChannel    = "lead_channel"
	FieldLeadCampaignID = "lead_campaign_id"
	FieldLeadCreatedAt  = "lead_created_at"
)

// maxSegmentFilters bounds one segment definition.
const maxSegmentFilters = 20

// SegmentFilter is one predicate of a segment definition.
type SegmentFilter struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

// SegmentDefinition is the segments.definition jsonb shape.
type SegmentDefinition struct {
	Filters []SegmentFilter `json:"filters"`
}

// Value implements driver.Valuer (jsonb round-trip).
func (d SegmentDefinition) Value() (driver.Value, error) { return json.Marshal(d) }

// Scan implements sql.Scanner (jsonb round-trip).
func (d *SegmentDefinition) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*d = SegmentDefinition{}
		return nil
	case []byte:
		return json.Unmarshal(v, d)
	case string:
		return json.Unmarshal([]byte(v), d)
	default:
		return fmt.Errorf("scan segment definition from %T", src)
	}
}

// ValidateSegmentDefinition enforces the whitelist/limits shared by the API
// and the evaluator.
func ValidateSegmentDefinition(d *SegmentDefinition) error {
	if d == nil || len(d.Filters) == 0 {
		return fmt.Errorf("%w: at least one filter is required", ErrInvalidInput)
	}
	if len(d.Filters) > maxSegmentFilters {
		return fmt.Errorf("%w: at most %d filters", ErrInvalidInput, maxSegmentFilters)
	}
	for i, f := range d.Filters {
		f.Field = strings.TrimSpace(f.Field)
		if !validFilterField(f.Field) {
			return fmt.Errorf("%w: filters[%d].field %q not supported", ErrInvalidInput, i, f.Field)
		}
		switch f.Op {
		case OpEq, OpNeq, OpGte, OpLte, OpContains:
			if f.Value == nil {
				return fmt.Errorf("%w: filters[%d].value is required", ErrInvalidInput, i)
			}
			if s, ok := f.Value.(string); ok && strings.TrimSpace(s) == "" {
				return fmt.Errorf("%w: filters[%d].value must not be empty", ErrInvalidInput, i)
			}
		case OpIn:
			vals, ok := toStringSlice(f.Value)
			if !ok || len(vals) == 0 {
				return fmt.Errorf("%w: filters[%d].value for op in must be a non-empty array", ErrInvalidInput, i)
			}
		default:
			return fmt.Errorf("%w: filters[%d].op %q (want eq|neq|in|gte|lte|contains)", ErrInvalidInput, i, f.Op)
		}
	}
	return nil
}

func validFilterField(field string) bool {
	switch field {
	case FieldName, FieldPhone, FieldEmail, FieldSource, FieldExternalID,
		FieldLeadStatus, FieldLeadChannel, FieldLeadCampaignID, FieldLeadCreatedAt:
		return true
	}
	return false
}

// toStringSlice coerces a json-decoded array value to []string.
func toStringSlice(v any) ([]string, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// Segment mirrors booking.studio_segments.
type Segment struct {
	ID          uuid.UUID         `json:"id"`
	TenantID    uuid.UUID         `json:"tenant_id"`
	Name        string            `json:"name"`
	Definition  SegmentDefinition `json:"definition"`
	ApproxCount int64             `json:"approx_count"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// ---------------------------------------------------------------------------
// Journeys
// ---------------------------------------------------------------------------

// Journey statuses (status machine draft→active→paused↔active→archived).
const (
	StatusDraft    = "draft"
	StatusActive   = "active"
	StatusPaused   = "paused"
	StatusArchived = "archived"
)

// Journey trigger kinds.
const (
	TriggerSegment = "segment"
	TriggerManual  = "manual"
	TriggerEvent   = "event"
)

// Journey step types.
const (
	StepWait   = "wait"
	StepSend   = "send"
	StepBranch = "branch"
)

// Send step kinds. sms and push_marketing dispatch through the
// notification-worker paced contract (marketing class — DND-suppressed
// activity-side, quiet-hours deferred workflow-side). ussd is accepted in
// journey definitions (the channel exists for inbound) but there is NO
// outbound USSD binding: ussd send steps are advanced and counted as
// skipped (documented limitation).
const (
	KindSMS           = "sms"
	KindPushMarketing = "push_marketing"
	KindUSSD          = "ussd"
)

// maxJourneySteps bounds one journey definition.
const maxJourneySteps = 50

// maxTemplateLen bounds one send template.
const maxTemplateLen = 4096

// JourneyStep is one entry of journeys.steps jsonb.
type JourneyStep struct {
	Type      string             `json:"type"`                 // wait | send | branch
	Kind      string             `json:"kind,omitempty"`       // sms | push_marketing | ussd (send only)
	Template  string             `json:"template,omitempty"`   // send only; {name} token supported
	WaitHours int                `json:"wait_hours,omitempty"` // wait only; >= 0
	Condition *SegmentDefinition `json:"condition,omitempty"`  // branch only
	AbVariant string             `json:"ab_variant,omitempty"` // optional A|B tag
}

// Steps is the journeys.steps jsonb shape.
type Steps []JourneyStep

// Value implements driver.Valuer (jsonb round-trip).
func (s Steps) Value() (driver.Value, error) {
	if s == nil {
		s = Steps{}
	}
	return json.Marshal(s)
}

// Scan implements sql.Scanner (jsonb round-trip).
func (s *Steps) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*s = Steps{}
		return nil
	case []byte:
		return json.Unmarshal(v, s)
	case string:
		return json.Unmarshal([]byte(v), s)
	default:
		return fmt.Errorf("scan journey steps from %T", src)
	}
}

// ValidateSteps enforces the SPEC-W19 step contract: known types/kinds,
// wait_hours >= 0, per-type field discipline.
func ValidateSteps(steps Steps) error {
	if len(steps) == 0 {
		return fmt.Errorf("%w: at least one step is required", ErrInvalidInput)
	}
	if len(steps) > maxJourneySteps {
		return fmt.Errorf("%w: at most %d steps", ErrInvalidInput, maxJourneySteps)
	}
	for i, st := range steps {
		if st.AbVariant != "" && st.AbVariant != "A" && st.AbVariant != "B" {
			return fmt.Errorf("%w: steps[%d].ab_variant %q (want A|B)", ErrInvalidInput, i, st.AbVariant)
		}
		if st.WaitHours < 0 {
			return fmt.Errorf("%w: steps[%d].wait_hours must be >= 0", ErrInvalidInput, i)
		}
		switch st.Type {
		case StepWait:
			if st.Kind != "" || st.Template != "" || st.Condition != nil {
				return fmt.Errorf("%w: steps[%d] wait takes only wait_hours", ErrInvalidInput, i)
			}
		case StepSend:
			switch st.Kind {
			case KindSMS, KindPushMarketing, KindUSSD:
			default:
				return fmt.Errorf("%w: steps[%d].kind %q (want sms|push_marketing|ussd)", ErrInvalidInput, i, st.Kind)
			}
			st.Template = strings.TrimSpace(st.Template)
			if st.Template == "" {
				return fmt.Errorf("%w: steps[%d].template is required for send", ErrInvalidInput, i)
			}
			if len(st.Template) > maxTemplateLen {
				return fmt.Errorf("%w: steps[%d].template exceeds %d bytes", ErrInvalidInput, i, maxTemplateLen)
			}
			if st.WaitHours != 0 || st.Condition != nil {
				return fmt.Errorf("%w: steps[%d] send takes only kind/template", ErrInvalidInput, i)
			}
		case StepBranch:
			if st.Kind != "" || st.Template != "" || st.WaitHours != 0 {
				return fmt.Errorf("%w: steps[%d] branch takes only condition", ErrInvalidInput, i)
			}
			if err := ValidateSegmentDefinition(st.Condition); err != nil {
				return fmt.Errorf("%w: steps[%d].condition: %v", ErrInvalidInput, i, err)
			}
		default:
			return fmt.Errorf("%w: steps[%d].type %q (want wait|send|branch)", ErrInvalidInput, i, st.Type)
		}
	}
	return nil
}

// CanTransition reports whether the journey status machine allows from→to
// (draft→active→paused↔active→archived; archived is terminal; draft may be
// archived directly). A same-state request is not a transition — handlers
// treat it as a no-op before consulting this.
func CanTransition(from, to string) bool {
	switch from {
	case StatusDraft:
		return to == StatusActive || to == StatusArchived
	case StatusActive:
		return to == StatusPaused || to == StatusArchived
	case StatusPaused:
		return to == StatusActive || to == StatusArchived
	default: // archived
		return false
	}
}

func validTriggerKind(k string) bool {
	switch k {
	case TriggerSegment, TriggerManual, TriggerEvent:
		return true
	}
	return false
}

// Journey mirrors booking.studio_journeys.
type Journey struct {
	ID          uuid.UUID  `json:"id"`
	TenantID    uuid.UUID  `json:"tenant_id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	TriggerKind string     `json:"trigger_kind"`
	SegmentID   *uuid.UUID `json:"segment_id"`
	Steps       Steps      `json:"steps"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// ---------------------------------------------------------------------------
// Enrollments
// ---------------------------------------------------------------------------

// Enrollment states.
const (
	EnrollActive    = "active"
	EnrollCompleted = "completed"
	EnrollExited    = "exited"
)

// Enrollment mirrors booking.studio_enrollments. step_idx indexes
// journeys.steps; when step_idx reaches len(steps) the enrollment is
// completed. last_step_at drives the wait-step time check.
type Enrollment struct {
	ID           uuid.UUID `json:"id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	JourneyID    uuid.UUID `json:"journey_id"`
	ContactID    uuid.UUID `json:"contact_id"`
	StepIdx      int       `json:"step_idx"`
	State        string    `json:"state"`
	EnrolledAt   time.Time `json:"enrolled_at"`
	LastStepAt   time.Time `json:"last_step_at"`
	ExitedReason *string   `json:"exited_reason"`
}

// Step event kinds written to studio_step_events (audit + per-step stats).
const (
	EventWaitPassed     = "wait_passed"
	EventBranchTrue     = "branch_true"
	EventBranchFalse    = "branch_false"
	EventSendQueued     = "send_queued"
	EventSendSent       = "send_sent"
	EventSendSuppressed = "send_suppressed" // DND guard outcome
	EventSendFailed     = "send_failed"
	EventSendSkipped    = "send_skipped" // ussd (no outbound binding) / missing channel address
	EventCompleted      = "completed"
	EventExited         = "exited"
)
