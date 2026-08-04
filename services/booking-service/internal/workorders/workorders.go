// Package workorders implements SPEC-W19 Agent B: the FIELD SERVICE
// enterprise app (work orders & dispatch), app_id "field-service".
//
// A work order is a unit of field work dispatched to a team member:
//
//	created → assigned → en_route → on_site → completed
//	    any non-terminal state → cancelled
//
// Completion is gated: every checklist item must be done AND proof.notes
// must be non-empty (the field agent's completion note).
//
// Anti-collision contract (SPEC-W19): this package is SELF-CONTAINED — it
// exposes NewStore/DialStore (mirror internal/devices) and
// RegisterRoutes(r, d, mw...) (see handlers.go); the integrator wires Deps,
// route mounting, config envs and the appgate entitlement flag
// (app_id "field-service"). This package touches NO shared files.
//
// Config envs (documented for the integrator — no config code here; every
// one is optional and the app is functional with zero config):
//
//	WORKORDERS_NOTIFICATIONS_TOPIC — dispatch push target topic
//	    (default opendesk.notifications.outbox; empty disables notify)
//	WORKORDERS_USAGE_TOPIC       — metering topic
//	    (default opendesk.usage.events; empty disables metering)
//	WORKORDERS_FSM_EVENTS_TOPIC  — lifecycle CloudEvents topic
//	    (default opendesk.fsm.events.v1; empty disables events)
package workorders

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Work order statuses (SPEC-W19 Agent B state machine).
const (
	StatusCreated   = "created"
	StatusAssigned  = "assigned"
	StatusEnRoute   = "en_route"
	StatusOnSite    = "on_site"
	StatusCompleted = "completed"
	StatusCancelled = "cancelled"
)

// Statuses lists every status in machine order (board lanes, filters).
var Statuses = []string{
	StatusCreated, StatusAssigned, StatusEnRoute, StatusOnSite, StatusCompleted, StatusCancelled,
}

// openStatuses are the non-terminal states used by the auto-dispatch load
// counter (created orders carry no assignee, so they never appear in the
// per-member count anyway).
var openStatuses = []string{StatusAssigned, StatusEnRoute, StatusOnSite}

// ErrInvalidInput marks deterministic validation failures (400 at the API).
var ErrInvalidInput = errors.New("invalid work order input")

// ErrInvalidTransition marks state-machine violations (409 at the API).
var ErrInvalidTransition = errors.New("invalid work order status transition")

// ErrCompletionGate marks completion preconditions (checklist all-done +
// proof.notes) not being met (409 at the API).
var ErrCompletionGate = errors.New("work order completion requirements not met")

// transitions is the SPEC-W19 Agent B state machine:
// created→assigned→en_route→on_site→completed, any→cancelled (from any
// non-terminal state). Terminal states (completed, cancelled) have no
// outgoing edges.
var transitions = map[string][]string{
	StatusCreated:  {StatusAssigned, StatusCancelled},
	StatusAssigned: {StatusEnRoute, StatusCancelled},
	StatusEnRoute:  {StatusOnSite, StatusCancelled},
	StatusOnSite:   {StatusCompleted, StatusCancelled},
}

// CanTransition reports whether from→to is a legal edge of the state
// machine. from==to is always false EXCEPT assigned→assigned, which the
// dispatch endpoint uses for re-assignment (a no-op transition that keeps
// the order in the assigned lane with a new assignee — see Dispatch).
func CanTransition(from, to string) bool {
	if from == StatusAssigned && to == StatusAssigned {
		return true // re-dispatch / re-assignment
	}
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// ValidateTransition returns ErrInvalidTransition when from→to is illegal.
func ValidateTransition(from, to string) error {
	if !validStatus(from) || !validStatus(to) {
		return fmt.Errorf("%w: %q → %q (unknown status)", ErrInvalidTransition, from, to)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, from, to)
	}
	return nil
}

func validStatus(s string) bool {
	for _, st := range Statuses {
		if st == s {
			return true
		}
	}
	return false
}

// ValidateStatus enforces the status enum (filters / create input).
func ValidateStatus(s string) error {
	if !validStatus(s) {
		return fmt.Errorf("%w: status %q (want created|assigned|en_route|on_site|completed|cancelled)", ErrInvalidInput, s)
	}
	return nil
}

// ChecklistItem is one line of the job checklist (SPEC-W19: {label, done}).
type ChecklistItem struct {
	Label string `json:"label"`
	Done  bool   `json:"done"`
}

// maxChecklistItems bounds the checklist jsonb (a field job checklist is
// tens of items, never thousands).
const maxChecklistItems = 100

// maxLabelLen bounds one checklist label.
const maxLabelLen = 500

// ValidateChecklist enforces the checklist shape.
func ValidateChecklist(items []ChecklistItem) error {
	if len(items) > maxChecklistItems {
		return fmt.Errorf("%w: checklist exceeds %d items", ErrInvalidInput, maxChecklistItems)
	}
	for i, it := range items {
		if strings.TrimSpace(it.Label) == "" {
			return fmt.Errorf("%w: checklist item %d label is required", ErrInvalidInput, i)
		}
		if len(it.Label) > maxLabelLen {
			return fmt.Errorf("%w: checklist item %d label exceeds %d bytes", ErrInvalidInput, i, maxLabelLen)
		}
	}
	return nil
}

// Proof is the completion evidence jsonb (SPEC-W19: proof DEFAULT '{}';
// completing requires proof.notes). Photos holds optional URLs/keys of
// attached completion photos (the W16 field-capture flow can land them;
// they are stored verbatim, never fetched server-side).
type Proof struct {
	Notes  string   `json:"notes,omitempty"`
	Photos []string `json:"photos,omitempty"`
}

// maxProofNotesLen bounds the completion note.
const maxProofNotesLen = 4000

// Validate enforces proof bounds (completeness is gated separately by
// checkCompletionGate — proof is allowed to be empty mid-flight).
func (p *Proof) Validate() error {
	if len(p.Notes) > maxProofNotesLen {
		return fmt.Errorf("%w: proof.notes exceeds %d bytes", ErrInvalidInput, maxProofNotesLen)
	}
	for i, ph := range p.Photos {
		if strings.TrimSpace(ph) == "" {
			return fmt.Errorf("%w: proof.photos[%d] is empty", ErrInvalidInput, i)
		}
	}
	return nil
}

// maxTitleLen / maxDescriptionLen bound the free-text columns.
const (
	maxTitleLen       = 300
	maxDescriptionLen = 8000
)

// WorkOrder mirrors booking.work_orders (SPEC-W19 Agent B).
type WorkOrder struct {
	ID          uuid.UUID  `json:"id"`
	TenantID    uuid.UUID  `json:"tenant_id"`
	ContactID   *uuid.UUID `json:"contact_id"` // customer being served (nullable)
	BookingID   *uuid.UUID `json:"booking_id"` // originating booking (nullable)
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Status      string     `json:"status"`
	// AssigneeID references team_members.id (the dispatch target).
	AssigneeID     *uuid.UUID      `json:"assignee_id"`
	ScheduledStart *time.Time      `json:"scheduled_start"`
	ScheduledEnd   *time.Time      `json:"scheduled_end"`
	GPSLat         *float64        `json:"gps_lat"`
	GPSLng         *float64        `json:"gps_lng"`
	GPSAccuracy    *float64        `json:"gps_accuracy"` // meters
	Checklist      []ChecklistItem `json:"checklist"`
	Proof          Proof           `json:"proof"`
	// FieldCaptureID links the W16 offline-queue anchor row
	// (field_captures.client_id — a client-generated UUID string) whose
	// payload/check-in evidence belongs to this order. Nullable.
	FieldCaptureID *string    `json:"field_capture_id"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CompletedAt    *time.Time `json:"completed_at"`
}

// Validate checks the minimal field set required for persistence.
func (w *WorkOrder) Validate() error {
	if w.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	w.Title = strings.TrimSpace(w.Title)
	if w.Title == "" {
		return fmt.Errorf("%w: title is required", ErrInvalidInput)
	}
	if len(w.Title) > maxTitleLen {
		return fmt.Errorf("%w: title exceeds %d bytes", ErrInvalidInput, maxTitleLen)
	}
	if len(w.Description) > maxDescriptionLen {
		return fmt.Errorf("%w: description exceeds %d bytes", ErrInvalidInput, maxDescriptionLen)
	}
	if err := ValidateStatus(w.Status); err != nil {
		return err
	}
	if w.ScheduledStart != nil && w.ScheduledEnd != nil && w.ScheduledEnd.Before(*w.ScheduledStart) {
		return fmt.Errorf("%w: scheduled_end is before scheduled_start", ErrInvalidInput)
	}
	if err := w.ValidateGPS(); err != nil {
		return err
	}
	if err := ValidateChecklist(w.Checklist); err != nil {
		return err
	}
	if err := w.Proof.Validate(); err != nil {
		return err
	}
	if w.FieldCaptureID != nil {
		v := strings.TrimSpace(*w.FieldCaptureID)
		if v == "" || len(v) > 128 {
			return fmt.Errorf("%w: field_capture_id must be 1-128 bytes", ErrInvalidInput)
		}
		w.FieldCaptureID = &v
	}
	return nil
}

// ValidateGPS enforces sane coordinate ranges (same bounds as the W16
// fieldcapture GPS): lat and lng must be set together; accuracy is
// optional and non-negative.
func (w *WorkOrder) ValidateGPS() error {
	if (w.GPSLat == nil) != (w.GPSLng == nil) {
		return fmt.Errorf("%w: gps_lat and gps_lng must be set together", ErrInvalidInput)
	}
	if w.GPSLat != nil && (*w.GPSLat < -90 || *w.GPSLat > 90) {
		return fmt.Errorf("%w: gps_lat out of range", ErrInvalidInput)
	}
	if w.GPSLng != nil && (*w.GPSLng < -180 || *w.GPSLng > 180) {
		return fmt.Errorf("%w: gps_lng out of range", ErrInvalidInput)
	}
	if w.GPSAccuracy != nil && *w.GPSAccuracy < 0 {
		return fmt.Errorf("%w: gps_accuracy must be >= 0", ErrInvalidInput)
	}
	return nil
}

// checkCompletionGate enforces the SPEC-W19 completion preconditions:
// every checklist item done (an empty checklist passes vacuously — there
// is nothing left undone) AND a non-empty proof.notes.
func (w *WorkOrder) checkCompletionGate() error {
	for _, it := range w.Checklist {
		if !it.Done {
			return fmt.Errorf("%w: checklist item %q is not done", ErrCompletionGate, it.Label)
		}
	}
	if strings.TrimSpace(w.Proof.Notes) == "" {
		return fmt.Errorf("%w: proof.notes is required to complete", ErrCompletionGate)
	}
	return nil
}

// IsTerminal reports whether the order is in a terminal state.
func (w *WorkOrder) IsTerminal() bool {
	return w.Status == StatusCompleted || w.Status == StatusCancelled
}

// BoardItem is one dispatch-board row: the work order plus the resolved
// assignee display name (joined from team_members; "" when unassigned).
type BoardItem struct {
	WorkOrder
	AssigneeName string `json:"assignee_name"`
}
