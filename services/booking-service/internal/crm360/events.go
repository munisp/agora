package crm360

// CloudEvents lifecycle events for SPEC-W20 Agent A: note create/edit,
// pin toggles and tag add/remove all emit onto the CRM events topic
// (opendesk.crm.events.v1, SPEC-W20 contract §5) via the transactional
// outbox (Store.EnqueueOutbox), best-effort post-commit — the same
// posture as the W19 packages: eventing must never block a mutation;
// failures are logged for reconciliation.
//
// Metering: deliberately ABSENT (internal-ops app, SPEC-W20 contract §4 —
// see the package doc).

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// DefaultEventsTopic is the CRM lifecycle topic (SPEC-W20 contract §5).
// The integrator passes it via Deps.CRMEventsTopic; empty disables
// emission (graceful no-op).
const DefaultEventsTopic = "opendesk.crm.events.v1"

// Lifecycle event types published to the CRM events topic.
const (
	// EventTypeNoteCreated fires when a note is added to a contact.
	EventTypeNoteCreated = "com.opendesk.crm.NoteCreated"
	// EventTypeNoteUpdated fires when a note body is edited or its pin
	// is toggled (payload carries the resulting pinned state).
	EventTypeNoteUpdated = "com.opendesk.crm.NoteUpdated"
	// EventTypeTagAdded fires when a tag is attached to a contact.
	EventTypeTagAdded = "com.opendesk.crm.TagAdded"
	// EventTypeTagRemoved fires when a tag is detached from a contact.
	EventTypeTagRemoved = "com.opendesk.crm.TagRemoved"
)

// MarshalNoteEvent builds the CRM events envelope for one note change
// (created or updated). The note body is NOT included — events stay
// small and avoid leaking free-text into the bus; consumers fetch via
// the API with ref note_id.
func MarshalNoteEvent(tenantSlug, eventType string, n Note) ([]byte, error) {
	data := map[string]any{
		"tenant_id":  n.TenantID.String(),
		"note_id":    n.ID.String(),
		"contact_id": n.ContactID.String(),
		"author":     n.Author,
		"pinned":     n.Pinned,
		"ts":         time.Now().UTC(),
	}
	return json.Marshal(events.New("booking-service", eventType, tenantSlug, n.TenantID.String(), data))
}

// MarshalTagEvent builds the CRM events envelope for one tag change
// (added or removed).
func MarshalTagEvent(tenantSlug, eventType string, tenantID, contactID uuid.UUID, tag string) ([]byte, error) {
	data := map[string]any{
		"tenant_id":  tenantID.String(),
		"contact_id": contactID.String(),
		"tag":        tag,
		"ts":         time.Now().UTC(),
	}
	return json.Marshal(events.New("booking-service", eventType, tenantSlug, tenantID.String(), data))
}

// publishNoteEvent emits one note lifecycle event when the topic is
// configured (empty = graceful no-op, SPEC-W20 contract §5).
func (h *Handlers) publishNoteEvent(ctx context.Context, tenantSlug, eventType string, n Note) {
	if h.EventsTopic == "" {
		return
	}
	payload, err := MarshalNoteEvent(tenantSlug, eventType, n)
	if err != nil {
		h.log().Warn("crm note event marshal failed; skipping emission", zap.Error(err))
		return
	}
	if err := h.Store.EnqueueOutbox(ctx, n.ID, h.EventsTopic, payload); err != nil {
		h.log().Warn("crm note event enqueue failed; skipping emission",
			zap.String("note_id", n.ID.String()), zap.Error(err))
	}
}

// publishTagEvent emits one tag lifecycle event when the topic is
// configured.
func (h *Handlers) publishTagEvent(ctx context.Context, tenantSlug, eventType string, tenantID, contactID uuid.UUID, tag string) {
	if h.EventsTopic == "" {
		return
	}
	payload, err := MarshalTagEvent(tenantSlug, eventType, tenantID, contactID, tag)
	if err != nil {
		h.log().Warn("crm tag event marshal failed; skipping emission", zap.Error(err))
		return
	}
	if err := h.Store.EnqueueOutbox(ctx, contactID, h.EventsTopic, payload); err != nil {
		h.log().Warn("crm tag event enqueue failed; skipping emission",
			zap.String("contact_id", contactID.String()), zap.String("tag", tag), zap.Error(err))
	}
}
