// Package fieldcapture implements SPEC-W16 Agent B (contract §4): the
// server half of the field PWA / mobile offline queue. Clients batch
// queued items to POST /v1/field/capture; every item carries a
// client-generated idempotency key (client_id, a UUID — logical dedupe key
// "field_capture:{client_id}") so flushing the queue after connectivity
// loss, and any retry of the same flush, applies each item exactly once.
//
// Kinds:
//   - lead_capture → creates a lead via the W13 leads service (channel
//     "field", honoring its 24h first-touch dedupe on top of the
//     client_id anchor);
//   - checkin → appends a geo check-in row. The W8 location store
//     (store/geolocations.go contact_locations) is a last-known-position
//     UPSERT keyed (tenant_id, contact_id) — it exposes NO history — so
//     check-ins persist in the new field_checkins table per the spec's
//     fallback clause.
//
// Persistence mirrors the W13 leads idiom (idempotent bootstrap DDL, RLS
// enabled + forced, tenant_isolation policy) packaged like the W14
// PayoutStore (dedicated small pool).
package fieldcapture

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Capture kinds (contract §4 enum).
const (
	KindLeadCapture = "lead_capture"
	KindCheckin     = "checkin"
)

// Item result statuses.
const (
	StatusApplied = "applied" // first application (or leads 24h dedupe hit — still exactly-once)
	StatusDeduped = "deduped" // client_id replay: original outcome returned, nothing re-applied
	StatusError   = "error"   // deterministic validation failure (recorded; replays dedupe to it)
)

// ErrInvalidInput marks deterministic validation failures.
var ErrInvalidInput = errors.New("invalid field capture input")

// maxClientIDLen bounds client_id (a UUID string is 36 chars; headroom for
// prefixed variants).
const maxClientIDLen = 128

// GPS is the optional fix attached to a capture item (contract §4:
// gps:{lat,lng,accuracy} null).
type GPS struct {
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
	Accuracy float64 `json:"accuracy"` // meters
}

// Validate enforces sane coordinate ranges.
func (g *GPS) Validate() error {
	if g.Lat < -90 || g.Lat > 90 {
		return fmt.Errorf("%w: gps.lat out of range", ErrInvalidInput)
	}
	if g.Lng < -180 || g.Lng > 180 {
		return fmt.Errorf("%w: gps.lng out of range", ErrInvalidInput)
	}
	if g.Accuracy < 0 {
		return fmt.Errorf("%w: gps.accuracy must be >= 0", ErrInvalidInput)
	}
	return nil
}

// CaptureItem is one offline-queue entry (contract §4). Payload is
// kind-specific and kept as raw JSON: it is persisted verbatim on the
// field_captures anchor row (audit trail / no data loss for fields the
// server does not structure yet).
type CaptureItem struct {
	ClientID   string          `json:"client_id"`
	Kind       string          `json:"kind"`
	Payload    json.RawMessage `json:"payload"`
	CapturedAt *time.Time      `json:"captured_at"`
	GPS        *GPS            `json:"gps"`
}

// Validate checks the kind-agnostic fields. Kind-specific payload
// validation happens in the service (it owns the payload schemas).
func (it *CaptureItem) Validate() error {
	it.ClientID = strings.TrimSpace(it.ClientID)
	if it.ClientID == "" {
		return fmt.Errorf("%w: client_id is required", ErrInvalidInput)
	}
	if len(it.ClientID) > maxClientIDLen {
		return fmt.Errorf("%w: client_id exceeds %d bytes", ErrInvalidInput, maxClientIDLen)
	}
	switch it.Kind {
	case KindLeadCapture, KindCheckin:
	default:
		return fmt.Errorf("%w: kind %q (want lead_capture|checkin)", ErrInvalidInput, it.Kind)
	}
	if len(it.Payload) == 0 {
		it.Payload = json.RawMessage(`{}`)
	}
	if !json.Valid(it.Payload) {
		return fmt.Errorf("%w: payload is not valid JSON", ErrInvalidInput)
	}
	if it.GPS != nil {
		if err := it.GPS.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// LeadCapturePayload is the kind=lead_capture payload schema. PhoneE164 is
// the only required field (mirrors POST /v1/leads); name/notes are
// preserved verbatim on the anchor row's payload but have no leads column
// today (documented in docs/field-capture.md).
type LeadCapturePayload struct {
	PhoneE164  string         `json:"phone_e164"`
	Name       string         `json:"name,omitempty"`
	Notes      string         `json:"notes,omitempty"`
	UTM        map[string]any `json:"utm,omitempty"`
	CampaignID *uuid.UUID     `json:"campaign_id,omitempty"`
	LgaID      *int           `json:"lga_id,omitempty"`
	ConsentID  *uuid.UUID     `json:"consent_id,omitempty"`
}

// CheckinPayload is the kind=checkin payload schema: an optional contact
// being visited plus a free-text note. The GPS fix rides on the item, not
// the payload.
type CheckinPayload struct {
	ContactID *uuid.UUID `json:"contact_id,omitempty"`
	Note      string     `json:"note,omitempty"`
}

// ItemResult is the per-item outcome of a batch. On a deduped replay the
// ORIGINAL result (lead_id / checkin_id / error) is returned unchanged so
// the client can reconcile its outbox without side effects.
type ItemResult struct {
	ClientID  string     `json:"client_id"`
	Kind      string     `json:"kind"`
	Status    string     `json:"status"` // applied | deduped | error
	LeadID    *uuid.UUID `json:"lead_id,omitempty"`
	CheckinID *uuid.UUID `json:"checkin_id,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// Checkin mirrors booking.field_checkins: one geo check-in event (the W8
// contact_locations store has no history, so the history lives here).
type Checkin struct {
	ID         uuid.UUID       `json:"checkin_id"`
	TenantID   uuid.UUID       `json:"tenant_id"`
	ContactID  *uuid.UUID      `json:"contact_id"`
	Lat        *float64        `json:"lat"`
	Lng        *float64        `json:"lng"`
	AccuracyM  *float64        `json:"accuracy_m"`
	Note       string          `json:"note"`
	Payload    json.RawMessage `json:"payload"`
	CapturedAt *time.Time      `json:"captured_at"`
	CreatedAt  time.Time       `json:"created_at"`
}
