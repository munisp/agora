package campaignstudio

// SPEC-W19 shared contract §4: usage metering for Campaign Studio. Mirrors
// the referrals.metering.go idiom — one CloudEvents usage record on
// topic opendesk.usage.events:
//
//	{type: com.opendesk.usage.UsageRecord,
//	 data: {tenant_id, metric, value: 1, ts, meta: {...}}}
//
// Metric: journey_enrolled — emitted once per NEW enrollment (the
// non-idempotent path of Store.Enroll only, so a replayed enroll can
// never double-meter). Value is ALWAYS 1; context lives in meta.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// UsageMetricJourneyEnrolled is the metered unit emitted once per journey
// enrollment (SPEC-W19 contract §4: "studio: journey_enrolled").
const UsageMetricJourneyEnrolled = "journey_enrolled"

// MarshalJourneyEnrolledUsageRecord builds the usage-record payload for
// one new enrollment.
func MarshalJourneyEnrolledUsageRecord(tenantSlug string, tenantID, journeyID, contactID, enrollmentID uuid.UUID) ([]byte, error) {
	evt := events.New("booking-service", "com.opendesk.usage.UsageRecord", tenantSlug, tenantID.String(), map[string]any{
		"tenant_id": tenantID.String(),
		"metric":    UsageMetricJourneyEnrolled,
		"value":     1,
		"ts":        time.Now().UTC(),
		"meta": map[string]any{
			"journey_id":    journeyID.String(),
			"contact_id":    contactID.String(),
			"enrollment_id": enrollmentID.String(),
		},
	})
	return json.Marshal(evt)
}

// meterJourneyEnrolled writes one journey_enrolled usage record per new
// enrollment to the outbox (best-effort, post-commit — the same posture as
// the referrals meter: metering must never block an enrollment, failures
// are logged for reconciliation).
func (h *Handlers) meterJourneyEnrolled(ctx context.Context, tenantSlug string, tenantID uuid.UUID, created []Enrollment) {
	if h.UsageTopic == "" || len(created) == 0 {
		return
	}
	for _, e := range created {
		payload, err := MarshalJourneyEnrolledUsageRecord(tenantSlug, tenantID, e.JourneyID, e.ContactID, e.ID)
		if err != nil {
			h.log().Warn("journey_enrolled usage record marshal failed; skipping metering", zap.Error(err))
			continue
		}
		if err := h.Store.EnqueueOutbox(ctx, e.ID, h.UsageTopic, payload); err != nil {
			h.log().Warn("journey_enrolled usage record enqueue failed; skipping metering", zap.Error(err))
		}
	}
}
