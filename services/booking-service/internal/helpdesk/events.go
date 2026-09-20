package helpdesk

// SPEC-W19 contract §5: CloudEvents on the meaningful ticket lifecycle —
// ticket created / resolved on topic opendesk.helpdesk.events.v1 via the
// shared transactional outbox (same idiom as leads.Service.emit). Emission
// is a graceful no-op when the topic is empty (events disabled).

import (
	"context"
	"encoding/json"

	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// EventTypeTicket is the CloudEvents type on topic
// opendesk.helpdesk.events.v1.
const EventTypeTicket = "com.opendesk.helpdesk.TicketEvent"

// Ticket lifecycle event names carried in data.event_name.
const (
	EventNameTicketCreated  = "ticket_created"
	EventNameTicketResolved = "ticket_resolved"
	// EventNameSLAPolicyDetached is the SPEC-W45 K24 audit event emitted
	// when an admin detaches the SLA policy from a ticket (data carries the
	// actor + detached_policy_id).
	EventNameSLAPolicyDetached = "sla_policy_detached"
)

// ticketEventData builds the data payload for one lifecycle event.
func ticketEventData(t Ticket, eventName string) map[string]any {
	data := map[string]any{
		"event_name": eventName,
		"tenant_id":  t.TenantID.String(),
		"ticket_id":  t.ID.String(),
		"subject":    t.Subject,
		"channel":    t.Channel,
		"priority":   t.Priority,
		"status":     t.Status,
		"created_at": t.CreatedAt,
	}
	if t.ContactID != nil {
		data["contact_id"] = t.ContactID.String()
	}
	if t.ConversationID != nil {
		data["conversation_id"] = t.ConversationID.String()
	}
	if t.AssigneeID != nil {
		data["assignee_id"] = t.AssigneeID.String()
	}
	if t.ResolvedAt != nil {
		data["resolved_at"] = *t.ResolvedAt
	}
	return data
}

// emit publishes one TicketEvent CloudEvent to the helpdesk events topic via
// the outbox. Best-effort (same posture as the leads funnel emission): the
// ticket row is durable; an enqueue failure is logged loudly for
// reconciliation. extra is merged into the data payload (nil for none) —
// e.g. the K24 CSAT token on ticket_resolved, the actor + detached policy
// id on the sla_policy_detached audit event.
func (h *Handlers) emit(ctx context.Context, t Ticket, eventName, tenantSlug string, extra map[string]any) {
	if h.EventsTopic == "" {
		return
	}
	data := ticketEventData(t, eventName)
	for k, v := range extra {
		data[k] = v
	}
	payload, err := json.Marshal(events.New("booking-service", EventTypeTicket, tenantSlug,
		t.TenantID.String(), data))
	if err != nil {
		h.log().Warn("helpdesk event marshal failed; skipping emission", zap.Error(err))
		return
	}
	if err := h.Store.EnqueueOutbox(ctx, t.ID, h.EventsTopic, payload); err != nil {
		h.log().Error("helpdesk event enqueue failed; ticket durable but event row lost — reconcile",
			zap.String("ticket_id", t.ID.String()), zap.String("event_name", eventName), zap.Error(err))
	}
}
