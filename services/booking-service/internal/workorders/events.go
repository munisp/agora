package workorders

// CloudEvents lifecycle events, usage metering and the dispatch push
// envelope for SPEC-W19 Agent B. All three ride the transactional outbox
// (Store.EnqueueOutbox) and are best-effort post-commit — the same posture
// as internal/referrals/metering.go: eventing/metering/notification must
// never block a dispatch or completion; failures are logged for
// reconciliation.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// Lifecycle event types published to the FSM topic
// (opendesk.fsm.events.v1, SPEC-W19 contract §5).
const (
	// EventTypeWorkOrderAssigned fires when a work order is dispatched
	// (created→assigned) or re-dispatched (assigned→assigned).
	EventTypeWorkOrderAssigned = "com.opendesk.fsm.WorkOrderAssigned"
	// EventTypeWorkOrderCompleted fires at the →completed transition.
	EventTypeWorkOrderCompleted = "com.opendesk.fsm.WorkOrderCompleted"
)

// UsageMetricWorkOrderCompleted is the metered unit emitted once per
// completed work order (SPEC-W19 contract §4) on the usage topic
// (opendesk.usage.events). Value is ALWAYS 1; context lives in meta.
const UsageMetricWorkOrderCompleted = "workorder_completed"

// ---------------------------------------------------------------------------
// W16 push contract (duplicated shape — service boundary)
// ---------------------------------------------------------------------------

// The dispatch notification envelope mirrors the W16 push contract
// (notification-worker internal/workflows/paced.go): the CloudEvent data
// IS a PacedSendRequest of kind "push_notification" (TRANSACTIONAL — never
// DND-suppressed, never quiet-hours deferred) carrying a
// PacedPushNotificationSend payload. Field tags are kept in sync with
// paced.go by contract (duplicated, not shared — see the geo/incidents
// precedent).
//
// The envelope is published to the notifications outbox topic
// (opendesk.notifications.outbox):
//
//	{specversion, id, source, type: "com.opendesk.notifications.PacedSend",
//	 subject: <tenant slug>, tenantid, time,
//	 data: {kind: "push_notification",
//	        push: {tenant_slug, contact_id, title, body, data, app}}}
//
// ASSUMPTION (documented in docs/apps/field-service.md): PacedPushNotificationSend
// resolves device tokens by contact_id via booking-service
// GET /internal/devices?contact_id= — dispatch passes the assignee
// TEAM MEMBER id as contact_id, so staff devices must be registered with
// device_tokens.contact_id = team_members.id (app "field") for the push to
// fan out. The notification-worker's notifyoutbox consumer acknowledges
// unknown event types (forward-compatible); delivering PacedSend requires
// the consumer-side case, flagged for the integrator.
const (
	// EventTypePacedSend is the notifications-topic command type carrying a
	// PacedSendRequest payload.
	EventTypePacedSend = "com.opendesk.notifications.PacedSend"
	// pacedKindPushNotification mirrors workflows.PacedSendPushNotification
	// (TRANSACTIONAL class).
	pacedKindPushNotification = "push_notification"
	// pushAppField restricts the device-token fetch to the field app
	// (mirrors devices.AppField).
	pushAppField = "field"
)

// MarshalAssignedEvent builds the opendesk.fsm.events.v1 envelope for one
// (re-)assignment.
func MarshalAssignedEvent(tenantSlug string, w WorkOrder) ([]byte, error) {
	data := map[string]any{
		"tenant_id":     w.TenantID.String(),
		"work_order_id": w.ID.String(),
		"title":         w.Title,
		"ts":            time.Now().UTC(),
	}
	if w.AssigneeID != nil {
		data["assignee_id"] = w.AssigneeID.String()
	}
	if w.ScheduledStart != nil {
		data["scheduled_start"] = *w.ScheduledStart
	}
	return json.Marshal(events.New("booking-service", EventTypeWorkOrderAssigned, tenantSlug, w.TenantID.String(), data))
}

// MarshalCompletedEvent builds the opendesk.fsm.events.v1 envelope for one
// completion.
func MarshalCompletedEvent(tenantSlug string, w WorkOrder) ([]byte, error) {
	data := map[string]any{
		"tenant_id":     w.TenantID.String(),
		"work_order_id": w.ID.String(),
		"title":         w.Title,
		"completed_at":  time.Now().UTC(),
	}
	if w.CompletedAt != nil {
		data["completed_at"] = *w.CompletedAt
	}
	if w.AssigneeID != nil {
		data["assignee_id"] = w.AssigneeID.String()
	}
	return json.Marshal(events.New("booking-service", EventTypeWorkOrderCompleted, tenantSlug, w.TenantID.String(), data))
}

// MarshalCompletedUsageRecord builds the usage-record payload for one
// completed work order (mirrors
// referrals.MarshalReferralVerifiedUsageRecord).
func MarshalCompletedUsageRecord(tenantSlug string, w WorkOrder) ([]byte, error) {
	meta := map[string]any{
		"work_order_id":   w.ID.String(),
		"checklist_items": len(w.Checklist),
	}
	if w.AssigneeID != nil {
		meta["assignee_id"] = w.AssigneeID.String()
	}
	evt := events.New("booking-service", "com.opendesk.usage.UsageRecord", tenantSlug, w.TenantID.String(), map[string]any{
		"tenant_id": w.TenantID.String(),
		"metric":    UsageMetricWorkOrderCompleted,
		"value":     1,
		"ts":        time.Now().UTC(),
		"meta":      meta,
	})
	return json.Marshal(evt)
}

// MarshalDispatchPush builds the notifications-topic envelope for one
// dispatch push notification (W16 PacedSendRequest shape — see the file
// comment). assigneeID is passed as the push contact_id (documented
// ASSUMPTION above); assigneeName only renders the human text.
func MarshalDispatchPush(tenantSlug string, tenantID uuid.UUID, w WorkOrder, assigneeID uuid.UUID, assigneeName string) ([]byte, error) {
	body := "You have a new work order: " + w.Title
	if w.ScheduledStart != nil {
		body += " (scheduled " + w.ScheduledStart.UTC().Format("2006-01-02 15:04") + " UTC)"
	}
	push := map[string]any{
		"tenant_slug": tenantSlug,
		"contact_id":  assigneeID.String(),
		"title":       "Work order dispatched",
		"body":        body,
		"app":         pushAppField,
		"data": map[string]string{
			"kind":           "dispatch",
			"work_order_id":  w.ID.String(),
			"assignee_name":  assigneeName,
			"work_order_ref": w.Title,
		},
	}
	evt := events.New("booking-service", EventTypePacedSend, tenantSlug, tenantID.String(), map[string]any{
		"kind": pacedKindPushNotification,
		"push": push,
	})
	return json.Marshal(evt)
}

// ---------------------------------------------------------------------------
// best-effort publishers (post-commit; never block the mutation)
// ---------------------------------------------------------------------------

// publishAssigned emits the assigned lifecycle event when the FSM topic is
// configured (empty = graceful no-op, SPEC-W19 contract §5).
func (h *Handlers) publishAssigned(ctx context.Context, tenantSlug string, w WorkOrder) {
	if h.FSMEventsTopic == "" {
		return
	}
	payload, err := MarshalAssignedEvent(tenantSlug, w)
	if err != nil {
		h.log().Warn("workorder assigned event marshal failed; skipping emission", zap.Error(err))
		return
	}
	if err := h.Store.EnqueueOutbox(ctx, w.ID, h.FSMEventsTopic, payload); err != nil {
		h.log().Warn("workorder assigned event enqueue failed; skipping emission",
			zap.String("work_order_id", w.ID.String()), zap.Error(err))
	}
}

// publishCompleted emits the completed lifecycle event + the metered
// workorder_completed usage record (each gated on its own topic).
func (h *Handlers) publishCompleted(ctx context.Context, tenantSlug string, w WorkOrder) {
	if h.FSMEventsTopic != "" {
		payload, err := MarshalCompletedEvent(tenantSlug, w)
		if err != nil {
			h.log().Warn("workorder completed event marshal failed; skipping emission", zap.Error(err))
		} else if err := h.Store.EnqueueOutbox(ctx, w.ID, h.FSMEventsTopic, payload); err != nil {
			h.log().Warn("workorder completed event enqueue failed; skipping emission",
				zap.String("work_order_id", w.ID.String()), zap.Error(err))
		}
	}
	if h.UsageTopic == "" {
		return
	}
	payload, err := MarshalCompletedUsageRecord(tenantSlug, w)
	if err != nil {
		h.log().Warn("workorder usage record marshal failed; skipping metering", zap.Error(err))
		return
	}
	if err := h.Store.EnqueueOutbox(ctx, w.ID, h.UsageTopic, payload); err != nil {
		h.log().Warn("workorder usage record enqueue failed; skipping metering",
			zap.String("work_order_id", w.ID.String()), zap.Error(err))
	}
}

// notifyDispatch enqueues the paced push_notification envelope for one
// dispatch. Returns false (gracefully) when the notifications topic is
// disabled (empty) or the enqueue fails — dispatch itself never fails
// because a notification could not be sent.
func (h *Handlers) notifyDispatch(ctx context.Context, tenantSlug string, tenantID uuid.UUID, w WorkOrder, assigneeID uuid.UUID, assigneeName string) bool {
	if h.NotificationsTopic == "" {
		return false
	}
	payload, err := MarshalDispatchPush(tenantSlug, tenantID, w, assigneeID, assigneeName)
	if err != nil {
		h.log().Warn("dispatch push marshal failed; skipping notification", zap.Error(err))
		return false
	}
	if err := h.Store.EnqueueOutbox(ctx, w.ID, h.NotificationsTopic, payload); err != nil {
		h.log().Warn("dispatch push enqueue failed; skipping notification",
			zap.String("work_order_id", w.ID.String()), zap.Error(err))
		return false
	}
	return true
}
