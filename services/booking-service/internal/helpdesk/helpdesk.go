// Package helpdesk implements SPEC-W19 Agent A: the HELPDESK enterprise app
// (SLA ticketing) — sla_policies, tickets and ticket_events with FORCE-RLS
// tenant isolation, first-response/resolve SLA tracking, event-timeline
// writes, least-open-tickets auto-assignment, CSAT capture, ticket_resolved
// usage metering and CloudEvents lifecycle emission on
// opendesk.helpdesk.events.v1.
//
// Anti-collision contract (SPEC-W19 §Anti-collision): the package is fully
// self-contained. It exposes NewStore/DialStore (devices idiom — the shared
// store.Store does not expose its pool) and RegisterRoutes; the INTEGRATOR
// wires it into httpapi/server.go with the tenant middleware, perms
// (manage_bookings writes / view_analytics reads) and appgate app_id
// "helpdesk". This package adds NO config code; the integrator maps these
// envs (safe defaults — the app is functional with zero config):
//
//	HELPDESK_EVENTS_TOPIC  CloudEvents topic for ticket lifecycle events
//	                       (default "opendesk.helpdesk.events.v1";
//	                       empty disables emission — graceful no-op)
//	HELPDESK_USAGE_TOPIC   usage-metering topic for ticket_resolved records
//	                       (default "opendesk.usage.events";
//	                       empty disables metering)
//	HELPDESK_DB_MAX_CONNS  dedicated pool size (default 4, devices idiom)
//
// Entitlement: every route under /v1/helpdesk is gated by the integrator via
// internal/appgate with app_id "helpdesk" (SPEC-W19 §Anti-collision).
package helpdesk

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Ticket priorities (SPEC-W19 Agent A enum) — shared by sla_policies and
// tickets (one policy per priority tier).
const (
	PriorityLow    = "low"
	PriorityNormal = "normal"
	PriorityHigh   = "high"
	PriorityUrgent = "urgent"
)

// Ticket statuses (SPEC-W19 Agent A enum).
const (
	StatusOpen     = "open"
	StatusPending  = "pending"
	StatusResolved = "resolved"
	StatusClosed   = "closed"
)

// Ticket event kinds (SPEC-W19 Agent A enum) — the timeline written by
// create/patch in the same transaction as the ticket mutation.
const (
	EventCreated       = "created"
	EventAssigned      = "assigned"
	EventStatusChanged = "status_changed"
	EventNote          = "note"
	EventFirstResponse = "first_response"
	EventResolved      = "resolved"
	EventReopened      = "reopened"
)

// ErrInvalidInput marks deterministic validation failures (400 at the API).
var ErrInvalidInput = errors.New("invalid helpdesk input")

// SLAPolicy mirrors sla_policies: the first-response / resolve
// targets (minutes from ticket creation) for one priority tier.
type SLAPolicy struct {
	ID                  uuid.UUID `json:"id"`
	TenantID            uuid.UUID `json:"tenant_id"`
	Name                string    `json:"name"`
	Priority            string    `json:"priority"` // low | normal | high | urgent
	FirstResponseMinute int       `json:"first_response_minutes"`
	ResolveMinutes      int       `json:"resolve_minutes"`
	Active              bool      `json:"active"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// Ticket mirrors tickets. due_* are recomputed from the effective
// SLA policy whenever priority or sla_policy_id changes (base: created_at);
// first_response_at/resolved_at stamp the first staff touch / resolution.
type Ticket struct {
	ID                 uuid.UUID  `json:"id"`
	TenantID           uuid.UUID  `json:"tenant_id"`
	ContactID          *uuid.UUID `json:"contact_id"`
	ConversationID     *uuid.UUID `json:"conversation_id"`
	Subject            string     `json:"subject"`
	Channel            string     `json:"channel"`
	Priority           string     `json:"priority"`
	Status             string     `json:"status"`
	AssigneeID         *uuid.UUID `json:"assignee_id"`
	SLAPolicyID        *uuid.UUID `json:"sla_policy_id"`
	DueFirstResponseAt *time.Time `json:"due_first_response_at"`
	DueResolveAt       *time.Time `json:"due_resolve_at"`
	FirstResponseAt    *time.Time `json:"first_response_at"`
	ResolvedAt         *time.Time `json:"resolved_at"`
	CSATRating         *int       `json:"csat_rating"`
	CSATComment        *string    `json:"csat_comment"`
	CSATAt             *time.Time `json:"csat_at"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// TicketEvent mirrors helpdesk_ticket_events: one timeline row per
// lifecycle mutation, written same-tx with the ticket change.
type TicketEvent struct {
	ID       uuid.UUID      `json:"id"`
	TenantID uuid.UUID      `json:"tenant_id"`
	TicketID uuid.UUID      `json:"ticket_id"`
	Kind     string         `json:"kind"`
	Actor    string         `json:"actor"`
	Payload  map[string]any `json:"payload"`
	Ts       time.Time      `json:"ts"`
}

// TeamMemberView is the read-only projection of booking.team_members used
// for the assignee picker and auto-assignment (id/name/email only — the
// table belongs to the core store; this package never writes it).
type TeamMemberView struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Email string    `json:"email"`
}

// Stats backs GET /v1/helpdesk/stats: open tickets by priority, current
// SLA-breach count and 30-day averages (SPEC-W19 Agent A).
type Stats struct {
	OpenByPriority         map[string]int `json:"open_by_priority"`
	OpenCount              int            `json:"open_count"`
	BreachedCount          int            `json:"breached_count"`
	Resolved30d            int            `json:"resolved_30d"`
	AvgFirstResponseMin30d *float64       `json:"avg_first_response_minutes_30d"`
	AvgResolveMinutes30d   *float64       `json:"avg_resolve_minutes_30d"`
	AvgCSAT30d             *float64       `json:"avg_csat_30d"`
}

// BreachTicket is a Ticket plus the breach flags computed by GET
// /v1/helpdesk/breaches (now > due_*, status not in resolved|closed).
type BreachTicket struct {
	Ticket
	BreachedFirstResponse bool `json:"breached_first_response"`
	BreachedResolve       bool `json:"breached_resolve"`
}

// ValidatePriority / ValidateStatus enforce the contract enums.
func ValidatePriority(p string) error {
	switch p {
	case PriorityLow, PriorityNormal, PriorityHigh, PriorityUrgent:
		return nil
	}
	return fmt.Errorf("%w: priority %q (want low|normal|high|urgent)", ErrInvalidInput, p)
}

func ValidateStatus(s string) error {
	switch s {
	case StatusOpen, StatusPending, StatusResolved, StatusClosed:
		return nil
	}
	return fmt.Errorf("%w: status %q (want open|pending|resolved|closed)", ErrInvalidInput, s)
}

// Bounds for the free-text columns.
const (
	maxSubjectLen = 500
	maxChannelLen = 100
	maxNoteLen    = 8000
	maxNameLen    = 200
	maxCSATLen    = 4000
)

// ValidatePolicy checks an SLA policy for persistence.
func ValidatePolicy(p *SLAPolicy) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if len(p.Name) > maxNameLen {
		return fmt.Errorf("%w: name exceeds %d bytes", ErrInvalidInput, maxNameLen)
	}
	if err := ValidatePriority(p.Priority); err != nil {
		return err
	}
	if p.FirstResponseMinute <= 0 {
		return fmt.Errorf("%w: first_response_minutes must be > 0", ErrInvalidInput)
	}
	if p.ResolveMinutes <= 0 {
		return fmt.Errorf("%w: resolve_minutes must be > 0", ErrInvalidInput)
	}
	return nil
}

// ValidateTicket checks the minimal field set for a new ticket.
func ValidateTicket(t *Ticket) error {
	if t.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	t.Subject = strings.TrimSpace(t.Subject)
	if t.Subject == "" {
		return fmt.Errorf("%w: subject is required", ErrInvalidInput)
	}
	if len(t.Subject) > maxSubjectLen {
		return fmt.Errorf("%w: subject exceeds %d bytes", ErrInvalidInput, maxSubjectLen)
	}
	t.Channel = strings.ToLower(strings.TrimSpace(t.Channel))
	if t.Channel == "" {
		t.Channel = "web"
	}
	if len(t.Channel) > maxChannelLen {
		return fmt.Errorf("%w: channel exceeds %d bytes", ErrInvalidInput, maxChannelLen)
	}
	if t.Priority == "" {
		t.Priority = PriorityNormal
	}
	if err := ValidatePriority(t.Priority); err != nil {
		return err
	}
	if t.Status == "" {
		t.Status = StatusOpen
	}
	return ValidateStatus(t.Status)
}

// computeDues derives the SLA due timestamps from a policy, based on the
// ticket creation time (the SLA clock starts at creation — recomputation on
// priority/policy change keeps the original clock, so a tightened policy can
// honestly show an already-breached ticket).
func computeDues(createdAt time.Time, p *SLAPolicy) (dueFirst, dueResolve *time.Time) {
	if p == nil {
		return nil, nil
	}
	fr := createdAt.Add(time.Duration(p.FirstResponseMinute) * time.Minute)
	rs := createdAt.Add(time.Duration(p.ResolveMinutes) * time.Minute)
	return &fr, &rs
}
