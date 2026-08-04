// Package workforce implements SPEC-W20 Agent D: the WORKFORCE enterprise
// app (shifts, time tracking, leave), app_id "workforce".
//
// Three RLS-scoped tables back the app:
//
//	shifts        — an agent's scheduled work window (overlap-guarded)
//	time_entries  — clock-in/clock-out (one open entry per agent)
//	leave_requests— annual|sick|unpaid leave with approve/decline decision
//
// Agent ids reference the core team_members table (the same table helpdesk
// auto-assignment resolves against — mirrored here: shift creation and
// clock-in validate the agent is an ACTIVE team member of the tenant).
//
// Anti-collision contract (SPEC-W20): this package is SELF-CONTAINED — it
// exposes NewStore/DialStore (mirrors internal/devices + the W19 packages)
// and RegisterRoutes(r, d, mw...) (see handlers.go); the integrator wires
// Deps, route mounting, config envs and the appgate entitlement flag
// (app_id "workforce"). This package touches NO shared files.
//
// Metering: NONE — workforce is an internal-ops app (SPEC-W20 contract §4:
// no billable tenant-facing action; shifts/clock events/leave decisions are
// operational records, not usage). This decision is also documented in
// docs/apps/workforce.md.
//
// Config envs (documented for the integrator — no config code here; every
// one is optional and the app is functional with zero config):
//
//	WORKFORCE_EVENTS_TOPIC — lifecycle CloudEvents topic
//	    (default opendesk.workforce.events.v1; empty disables events)
package workforce

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Shift statuses (SPEC-W20 Agent D). scheduled|confirmed are the live
// planning states; completed|no_show|cancelled are terminal.
const (
	ShiftScheduled = "scheduled"
	ShiftConfirmed = "confirmed"
	ShiftCompleted = "completed"
	ShiftNoShow    = "no_show"
	ShiftCancelled = "cancelled"
)

// ShiftStatuses lists every shift status (filters, validation).
var ShiftStatuses = []string{
	ShiftScheduled, ShiftConfirmed, ShiftCompleted, ShiftNoShow, ShiftCancelled,
}

// Clock-in methods (SPEC-W20 Agent D: web|field_pwa).
const (
	MethodWeb      = "web"
	MethodFieldPWA = "field_pwa"
)

// Leave kinds (SPEC-W20 Agent D).
const (
	LeaveAnnual = "annual"
	LeaveSick   = "sick"
	LeaveUnpaid = "unpaid"
)

// Leave request statuses.
const (
	LeavePending  = "pending"
	LeaveApproved = "approved"
	LeaveDeclined = "declined"
)

// ErrInvalidInput marks deterministic validation failures (400 at the API).
var ErrInvalidInput = errors.New("invalid workforce input")

// ErrShiftOverlap marks a shift that would overlap another non-cancelled
// shift of the same agent (409 at the API, with the conflicting shift id).
var ErrShiftOverlap = errors.New("shift overlaps an existing shift")

// ErrOpenEntry marks a clock-in attempt while the agent already has an
// open time entry (409 at the API, with the open entry id).
var ErrOpenEntry = errors.New("agent already has an open time entry")

// ErrNoOpenEntry marks a clock-out attempt with no open entry (404 at the
// API).
var ErrNoOpenEntry = errors.New("no open time entry for agent")

// ErrInvalidTransition marks state-machine violations (shift status and
// leave decisions — 409 at the API).
var ErrInvalidTransition = errors.New("invalid status transition")

// OverlapError carries the conflicting shift id for the 409 response
// (SPEC-W20: overlap → 409 with conflicting shift id).
type OverlapError struct {
	ConflictShiftID uuid.UUID
}

func (e OverlapError) Error() string {
	return fmt.Sprintf("%s: conflicts with shift %s", ErrShiftOverlap, e.ConflictShiftID)
}

// Unwrap lets errors.Is(err, ErrShiftOverlap) match.
func (e OverlapError) Unwrap() error { return ErrShiftOverlap }

// OpenEntryError carries the existing open entry id for the 409 response.
type OpenEntryError struct {
	EntryID uuid.UUID
}

func (e OpenEntryError) Error() string {
	return fmt.Sprintf("%s: open entry %s", ErrOpenEntry, e.EntryID)
}

// Unwrap lets errors.Is(err, ErrOpenEntry) match.
func (e OpenEntryError) Unwrap() error { return ErrOpenEntry }

// shiftTransitions is the shift status machine: scheduled and confirmed
// can move to any terminal state (or scheduled→confirmed); terminal states
// have no outgoing edges. from==to is always legal (a no-op PATCH).
var shiftTransitions = map[string][]string{
	ShiftScheduled: {ShiftConfirmed, ShiftCompleted, ShiftNoShow, ShiftCancelled},
	ShiftConfirmed: {ShiftCompleted, ShiftNoShow, ShiftCancelled},
}

func validShiftStatus(s string) bool {
	for _, st := range ShiftStatuses {
		if st == s {
			return true
		}
	}
	return false
}

// ValidateShiftStatus enforces the shift status enum.
func ValidateShiftStatus(s string) error {
	if !validShiftStatus(s) {
		return fmt.Errorf("%w: status %q (want %s)", ErrInvalidInput, s, strings.Join(ShiftStatuses, "|"))
	}
	return nil
}

// ValidateShiftTransition returns ErrInvalidTransition when from→to is not
// a legal edge (from==to is a no-op and always legal).
func ValidateShiftTransition(from, to string) error {
	if !validShiftStatus(from) || !validShiftStatus(to) {
		return fmt.Errorf("%w: %q → %q (unknown status)", ErrInvalidTransition, from, to)
	}
	if from == to {
		return nil
	}
	for _, next := range shiftTransitions[from] {
		if next == to {
			return nil
		}
	}
	return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, from, to)
}

// maxRoleLen / maxReasonLen bound the free-text columns.
const (
	maxRoleLen   = 120
	maxReasonLen = 2000
)

// Shift mirrors booking shifts (SPEC-W20 Agent D).
type Shift struct {
	ID        uuid.UUID `json:"id"`
	TenantID  uuid.UUID `json:"tenant_id"`
	AgentID   uuid.UUID `json:"agent_id"` // team_members.id
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	Role      string    `json:"role"` // optional label (front desk, field, …)
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate checks the field set required for persistence (mirrors the
// CHECK ends_at > starts_at constraint at the application layer so the API
// answers 400, not a 500 on the database error).
func (s *Shift) Validate() error {
	if s.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if s.AgentID == uuid.Nil {
		return fmt.Errorf("%w: agent_id is required", ErrInvalidInput)
	}
	if s.StartsAt.IsZero() || s.EndsAt.IsZero() {
		return fmt.Errorf("%w: starts_at and ends_at are required", ErrInvalidInput)
	}
	if !s.EndsAt.After(s.StartsAt) {
		return fmt.Errorf("%w: ends_at must be after starts_at", ErrInvalidInput)
	}
	s.Role = strings.TrimSpace(s.Role)
	if len(s.Role) > maxRoleLen {
		return fmt.Errorf("%w: role exceeds %d bytes", ErrInvalidInput, maxRoleLen)
	}
	return ValidateShiftStatus(s.Status)
}

// IsTerminal reports whether the shift is in a terminal state.
func (s *Shift) IsTerminal() bool {
	return s.Status == ShiftCompleted || s.Status == ShiftNoShow || s.Status == ShiftCancelled
}

// TimeEntry mirrors booking time_entries (SPEC-W20 Agent D). GPS is the
// optional fix attached at clock-in (same bounds as the W16 fieldcapture /
// W19 workorders GPS).
type TimeEntry struct {
	ID         uuid.UUID  `json:"id"`
	TenantID   uuid.UUID  `json:"tenant_id"`
	AgentID    uuid.UUID  `json:"agent_id"`
	ClockInAt  time.Time  `json:"clock_in_at"`
	ClockOutAt *time.Time `json:"clock_out_at"` // null = open entry
	Method     string     `json:"method"`
	GPSLat     *float64   `json:"gps_lat"`
	GPSLng     *float64   `json:"gps_lng"`
}

// ValidateGPS enforces the coordinate ranges (lat/lng must be set
// together — mirrors workorders.ValidateGPS).
func (e *TimeEntry) ValidateGPS() error {
	if (e.GPSLat == nil) != (e.GPSLng == nil) {
		return fmt.Errorf("%w: gps_lat and gps_lng must be set together", ErrInvalidInput)
	}
	if e.GPSLat != nil && (*e.GPSLat < -90 || *e.GPSLat > 90) {
		return fmt.Errorf("%w: gps_lat out of range", ErrInvalidInput)
	}
	if e.GPSLng != nil && (*e.GPSLng < -180 || *e.GPSLng > 180) {
		return fmt.Errorf("%w: gps_lng out of range", ErrInvalidInput)
	}
	return nil
}

// ValidateMethod enforces the clock-in method enum.
func ValidateMethod(m string) error {
	if m != MethodWeb && m != MethodFieldPWA {
		return fmt.Errorf("%w: method %q (want web|field_pwa)", ErrInvalidInput, m)
	}
	return nil
}

// LeaveRequest mirrors booking leave_requests (SPEC-W20 Agent D).
type LeaveRequest struct {
	ID        uuid.UUID  `json:"id"`
	TenantID  uuid.UUID  `json:"tenant_id"`
	AgentID   uuid.UUID  `json:"agent_id"` // team_members.id
	Kind      string     `json:"kind"`
	StartsOn  time.Time  `json:"starts_on"` // date (UTC midnight)
	EndsOn    time.Time  `json:"ends_on"`   // date (UTC midnight)
	Status    string     `json:"status"`
	Reason    string     `json:"reason"`
	DecidedBy string     `json:"decided_by"` // JWT sub of the approver/decliner
	DecidedAt *time.Time `json:"decided_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// ValidateKind enforces the leave kind enum.
func ValidateKind(k string) error {
	if k != LeaveAnnual && k != LeaveSick && k != LeaveUnpaid {
		return fmt.Errorf("%w: kind %q (want annual|sick|unpaid)", ErrInvalidInput, k)
	}
	return nil
}

// Validate checks the field set required for persistence (mirrors the
// CHECK ends_on >= starts_on constraint).
func (l *LeaveRequest) Validate() error {
	if l.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if l.AgentID == uuid.Nil {
		return fmt.Errorf("%w: agent_id is required", ErrInvalidInput)
	}
	if err := ValidateKind(l.Kind); err != nil {
		return err
	}
	if l.StartsOn.IsZero() || l.EndsOn.IsZero() {
		return fmt.Errorf("%w: starts_on and ends_on are required", ErrInvalidInput)
	}
	if l.EndsOn.Before(l.StartsOn) {
		return fmt.Errorf("%w: ends_on must be on or after starts_on", ErrInvalidInput)
	}
	l.Reason = strings.TrimSpace(l.Reason)
	if len(l.Reason) > maxReasonLen {
		return fmt.Errorf("%w: reason exceeds %d bytes", ErrInvalidInput, maxReasonLen)
	}
	return nil
}

// ShiftView is a shift plus the resolved agent display name (joined from
// team_members; "" when unresolvable) — the week-grid row shape.
type ShiftView struct {
	Shift
	AgentName string `json:"agent_name"`
}

// UtilizationRow is one agent's row in GET /v1/workforce/utilization:
// scheduled vs clocked hours over the range. Entries still clocked in are
// counted to now and flagged via OpenEntries.
type UtilizationRow struct {
	AgentID        uuid.UUID `json:"agent_id"`
	AgentName      string    `json:"agent_name"`
	ScheduledHours float64   `json:"scheduled_hours"`
	ClockedHours   float64   `json:"clocked_hours"`
	// UtilizationPct is clocked/scheduled*100; null when the agent has no
	// scheduled hours in the range (division by zero is honest, not 0%).
	UtilizationPct *float64 `json:"utilization_pct"`
	// OpenEntries counts time entries still clocked in (counted to now).
	OpenEntries int `json:"open_entries"`
}

// CoverageDay is one day in GET /v1/workforce/coverage: how many distinct
// agents have a non-cancelled shift overlapping the day vs how many
// bookings start that day.
type CoverageDay struct {
	Date            string `json:"date"` // YYYY-MM-DD
	AgentsScheduled int    `json:"agents_scheduled"`
	Bookings        int    `json:"bookings"`
}

// TeamMemberView is the read-only projection of the core team_members
// table for the agent pickers (mirrors helpdesk.TeamMemberView).
type TeamMemberView struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Email string    `json:"email"`
}
