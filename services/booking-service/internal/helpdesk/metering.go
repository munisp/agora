package helpdesk

// SPEC-W19 contract §4: usage metering for the helpdesk app — one
// ticket_resolved usage record per ticket resolution, mirroring
// internal/referrals/metering.go (which mirrors geo/incidents):
//
//	{type: com.opendesk.usage.UsageRecord,
//	 data: {tenant_id, metric, value: 1, ts, meta: {...}}}
//
// Value is ALWAYS 1 per resolution; context lives in meta so billing can
// price tiers without re-parsing tickets. Emitted best-effort post-commit —
// metering must never block a resolution; failures are logged for
// reconciliation. meterResolved is called only on the non-idempotent path
// (the patch that actually transitioned the ticket INTO resolved), so a
// replayed patch can never double-meter.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// UsageMetricTicketResolved is the metered unit emitted once per ticket
// resolution (SPEC-W19 contract §4 — helpdesk: ticket_resolved).
const UsageMetricTicketResolved = "ticket_resolved"

// MarshalTicketResolvedUsageRecord builds the usage-record payload for one
// resolved ticket.
func MarshalTicketResolvedUsageRecord(tenantSlug string, t Ticket) ([]byte, error) {
	meta := map[string]any{
		"ticket_id": t.ID.String(),
		"priority":  t.Priority,
		"channel":   t.Channel,
	}
	if t.AssigneeID != nil {
		meta["assignee_id"] = t.AssigneeID.String()
	}
	evt := events.New("booking-service", "com.opendesk.usage.UsageRecord", tenantSlug, t.TenantID.String(), map[string]any{
		"tenant_id": t.TenantID.String(),
		"metric":    UsageMetricTicketResolved,
		"value":     1,
		"ts":        time.Now().UTC(),
		"meta":      meta,
	})
	return json.Marshal(evt)
}

// meterResolved writes one ticket_resolved usage record to the outbox after
// a successful resolution (best-effort, post-commit — the same posture as
// the referrals meter).
func (h *Handlers) meterResolved(ctx context.Context, t Ticket, tenantSlug string) {
	if h.UsageTopic == "" {
		return
	}
	payload, err := MarshalTicketResolvedUsageRecord(tenantSlug, t)
	if err != nil {
		h.log().Warn("helpdesk usage record marshal failed; skipping metering", zap.Error(err))
		return
	}
	if err := h.Store.EnqueueOutbox(ctx, t.ID, h.UsageTopic, payload); err != nil {
		h.log().Warn("helpdesk usage record enqueue failed; skipping metering", zap.Error(err))
	}
}
