// Package crm360 implements SPEC-W20 Agent A: the CRM-360 enterprise app
// (unified customer profile), app_id "crm-360".
//
// The package owns two small write-side tables — crm_notes (pinned agent
// notes on a contact) and crm_tags (normalized lowercase labels) — and
// serves a READ-ONLY 360 aggregation over the existing domain tables:
// contacts (base record), bookings, helpdesk tickets/ticket_events,
// work_orders, loyalty_wallets/loyalty_ledger. Every optional source
// degrades to an empty section when its table is absent (to_regclass
// guards) — the 360 view NEVER 500s on a missing optional source
// (SPEC-W20 Agent A contract).
//
// Conversations are NOT joined: the conversations table lives in the
// separate conversation database (03-conversation-schema.sql) and carries
// no contact_id key, so a per-contact conversation lookup is not
// resolvable from the booking DB in this wave. The section is reserved
// (always empty) and documented in docs/apps/crm-360.md as a follow-up
// (needs a contact-linkage key or a cross-service read).
//
// Consent status is resolved only when the integrator wires the optional
// Deps.ConsentResolver hook (consent records live in identity-service's
// consents table — a different database); otherwise the profile answers
// consent=null ("not resolvable", per the SPEC-W20 contract wording).
//
// Metering (SPEC-W20 contract §4): crm-360 emits NO usage metering. It is
// an internal-ops productivity surface (notes/tags/reads on data the
// tenant already owns) — there is no billable customer-facing action, so
// metering would be pure noise on opendesk.usage.events. This mirrors the
// contract's explicit "crm-360 and workforce: NO metering" decision.
//
// Anti-collision contract (SPEC-W20): this package is SELF-CONTAINED — it
// exposes NewStore/DialStore (mirrors internal/workorders) and
// RegisterRoutes(r, d, mw...) (see handlers.go); the integrator wires
// Deps, route mounting, config envs and the appgate entitlement flag
// (app_id "crm-360"). This package touches NO shared files.
//
// Config envs (documented for the integrator — no config code here; every
// one is optional and the app is functional with zero config):
//
//	CRM360_EVENTS_TOPIC — lifecycle CloudEvents topic for note/pin/tag
//	    changes (default opendesk.crm.events.v1; empty disables events)
package crm360

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidInput marks deterministic validation failures (400 at the API).
var ErrInvalidInput = errors.New("invalid crm input")

// ---------------------------------------------------------------------------
// Tags
// ---------------------------------------------------------------------------

// tagPattern enforces the SPEC-W20 tag shape: lowercase, 1..40 chars,
// [a-z0-9-_].
var tagPattern = regexp.MustCompile(`^[a-z0-9-_]{1,40}$`)

// maxTagsPerContact bounds the label set on one contact (chips UI sanity).
const maxTagsPerContact = 50

// NormalizeTag trims and lowercases raw input. It does NOT validate — call
// ValidateTag on the normalized value.
func NormalizeTag(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// ValidateTag enforces the tag grammar on an already-normalized value.
func ValidateTag(tag string) error {
	if !tagPattern.MatchString(tag) {
		return fmt.Errorf("%w: tag %q (want lowercase, 1-40 chars, [a-z0-9-_])", ErrInvalidInput, tag)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Notes
// ---------------------------------------------------------------------------

const (
	maxNoteBodyBytes = 8000
	maxAuthorBytes   = 200
)

// Note mirrors crm_notes (SPEC-W20 Agent A): one agent note on a contact,
// optionally pinned to the top of the profile.
type Note struct {
	ID        uuid.UUID `json:"id"`
	TenantID  uuid.UUID `json:"tenant_id"`
	ContactID uuid.UUID `json:"contact_id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	Pinned    bool      `json:"pinned"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate enforces the note field bounds (body 1..8000 bytes after trim).
func (n *Note) Validate() error {
	if n.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if n.ContactID == uuid.Nil {
		return fmt.Errorf("%w: contact_id is required", ErrInvalidInput)
	}
	n.Body = strings.TrimSpace(n.Body)
	if n.Body == "" {
		return fmt.Errorf("%w: body is required", ErrInvalidInput)
	}
	if len(n.Body) > maxNoteBodyBytes {
		return fmt.Errorf("%w: body exceeds %d bytes", ErrInvalidInput, maxNoteBodyBytes)
	}
	n.Author = strings.TrimSpace(n.Author)
	if len(n.Author) > maxAuthorBytes {
		return fmt.Errorf("%w: author exceeds %d bytes", ErrInvalidInput, maxAuthorBytes)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 360 aggregation shapes
// ---------------------------------------------------------------------------

// Contact is the base contact record (booking.contacts + the W12 reverse-
// sync columns source/external_id).
type Contact struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenant_id"`
	Name       string    `json:"name"`
	Phone      string    `json:"phone"`
	Email      string    `json:"email"`
	Notes      string    `json:"notes"` // legacy free-text column, distinct from crm_notes
	Source     string    `json:"source"`
	ExternalID string    `json:"external_id"`
}

// ContactSearchResult is one /contacts/search row: the contact plus its
// current tags (for chip rendering without a second round-trip).
type ContactSearchResult struct {
	Contact
	Tags []string `json:"tags"`
}

// TicketSummary is one helpdesk ticket row in the 360 tickets section.
type TicketSummary struct {
	ID        uuid.UUID `json:"id"`
	Subject   string    `json:"subject"`
	Status    string    `json:"status"`
	Priority  string    `json:"priority"`
	CreatedAt time.Time `json:"created_at"`
}

// BookingSummary is one booking row in the 360 bookings section.
type BookingSummary struct {
	ID        uuid.UUID `json:"id"`
	Status    string    `json:"status"`
	Source    string    `json:"source"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	CreatedAt time.Time `json:"created_at"`
}

// WorkOrderSummary is one ACTIVE work order in the 360 work-orders section.
type WorkOrderSummary struct {
	ID             uuid.UUID  `json:"id"`
	Title          string     `json:"title"`
	Status         string     `json:"status"`
	ScheduledStart *time.Time `json:"scheduled_start"`
}

// WalletSummary is the loyalty wallet section (absent → nil on the
// profile, per SPEC "loyalty wallet {balance, tier} if any").
type WalletSummary struct {
	Balance int64  `json:"balance"`
	Tier    string `json:"tier"`
}

// Profile360 is the GET /v1/crm/contacts/{id}/360 payload. Every optional
// section is an empty array (never null) when its source is absent or
// empty; Wallet and Consent are null when not resolvable.
type Profile360 struct {
	Contact         Contact            `json:"contact"`
	Tags            []string           `json:"tags"`
	OpenTicketCount int                `json:"open_ticket_count"`
	Tickets         []TicketSummary    `json:"tickets"`     // latest 5, any status
	Bookings        []BookingSummary   `json:"bookings"`    // latest 5 by starts_at
	WorkOrders      []WorkOrderSummary `json:"work_orders"` // active only
	Wallet          *WalletSummary     `json:"wallet"`      // null when no wallet row
	Consent         *string            `json:"consent"`     // null unless Deps.ConsentResolver resolves it
}

// TimelineItem is one merged-feed row: {ts, kind, summary, ref_id}
// (SPEC-W20 Agent A). kind is one of booking|ticket_event|work_order|
// loyalty|note.
type TimelineItem struct {
	TS      time.Time `json:"ts"`
	Kind    string    `json:"kind"`
	Summary string    `json:"summary"`
	RefID   string    `json:"ref_id"`
}

// Timeline kinds (merged feed).
const (
	KindBooking     = "booking"
	KindTicketEvent = "ticket_event"
	KindWorkOrder   = "work_order"
	KindLoyalty     = "loyalty"
	KindNote        = "note"
)

const (
	// defaultTimelineLimit / maxTimelineLimit bound the merged feed.
	defaultTimelineLimit = 50
	maxTimelineLimit     = 200
	// defaultSearchLimit / maxSearchLimit bound /contacts/search.
	defaultSearchLimit = 20
	maxSearchLimit     = 100
)
