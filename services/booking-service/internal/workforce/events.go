package workforce

// CloudEvents lifecycle events for SPEC-W20 Agent D (contract §5):
// shift assigned + leave decided ride the transactional outbox
// (Store.EnqueueOutbox), best-effort post-commit — eventing must never
// block a rostering or decision mutation; failures are logged for
// reconciliation. Topic: opendesk.workforce.events.v1 (empty disables
// emission — graceful no-op).
//
// METERING: deliberately NONE (contract §4 — workforce is an internal-ops
// app; nothing here is a billable tenant-facing action).

import (
	"context"
	"encoding/json"
	"time"

	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// Lifecycle event types published to the workforce topic
// (opendesk.workforce.events.v1, SPEC-W20 contract §5).
const (
	// EventTypeShiftAssigned fires when a shift is created for an agent or
	// re-assigned to a different agent via PATCH.
	EventTypeShiftAssigned = "com.opendesk.workforce.ShiftAssigned"
	// EventTypeLeaveDecided fires when a leave request is approved or
	// declined.
	EventTypeLeaveDecided = "com.opendesk.workforce.LeaveDecided"
)

// MarshalShiftAssignedEvent builds the opendesk.workforce.events.v1
// envelope for one shift assignment.
func MarshalShiftAssignedEvent(tenantSlug string, s Shift) ([]byte, error) {
	data := map[string]any{
		"tenant_id": s.TenantID.String(),
		"shift_id":  s.ID.String(),
		"agent_id":  s.AgentID.String(),
		"starts_at": s.StartsAt,
		"ends_at":   s.EndsAt,
		"status":    s.Status,
		"ts":        time.Now().UTC(),
	}
	if s.Role != "" {
		data["role"] = s.Role
	}
	return json.Marshal(events.New("booking-service", EventTypeShiftAssigned, tenantSlug, s.TenantID.String(), data))
}

// MarshalLeaveDecidedEvent builds the opendesk.workforce.events.v1
// envelope for one leave decision (approved|declined).
func MarshalLeaveDecidedEvent(tenantSlug string, l LeaveRequest) ([]byte, error) {
	data := map[string]any{
		"tenant_id":  l.TenantID.String(),
		"leave_id":   l.ID.String(),
		"agent_id":   l.AgentID.String(),
		"kind":       l.Kind,
		"starts_on":  l.StartsOn.Format("2006-01-02"),
		"ends_on":    l.EndsOn.Format("2006-01-02"),
		"decision":   l.Status, // approved | declined
		"decided_by": l.DecidedBy,
		"ts":         time.Now().UTC(),
	}
	if l.DecidedAt != nil {
		data["decided_at"] = *l.DecidedAt
	}
	return json.Marshal(events.New("booking-service", EventTypeLeaveDecided, tenantSlug, l.TenantID.String(), data))
}

// publishShiftAssigned emits the shift-assigned lifecycle event when the
// events topic is configured (empty = graceful no-op, contract §5).
func (h *Handlers) publishShiftAssigned(ctx context.Context, tenantSlug string, s Shift) {
	if h.EventsTopic == "" {
		return
	}
	payload, err := MarshalShiftAssignedEvent(tenantSlug, s)
	if err != nil {
		h.log().Warn("shift assigned event marshal failed; skipping emission", zap.Error(err))
		return
	}
	if err := h.Store.EnqueueOutbox(ctx, s.ID, h.EventsTopic, payload); err != nil {
		h.log().Warn("shift assigned event enqueue failed; skipping emission",
			zap.String("shift_id", s.ID.String()), zap.Error(err))
	}
}

// publishLeaveDecided emits the leave-decided lifecycle event when the
// events topic is configured.
func (h *Handlers) publishLeaveDecided(ctx context.Context, tenantSlug string, l LeaveRequest) {
	if h.EventsTopic == "" {
		return
	}
	payload, err := MarshalLeaveDecidedEvent(tenantSlug, l)
	if err != nil {
		h.log().Warn("leave decided event marshal failed; skipping emission", zap.Error(err))
		return
	}
	if err := h.Store.EnqueueOutbox(ctx, l.ID, h.EventsTopic, payload); err != nil {
		h.log().Warn("leave decided event enqueue failed; skipping emission",
			zap.String("leave_id", l.ID.String()), zap.Error(err))
	}
}
