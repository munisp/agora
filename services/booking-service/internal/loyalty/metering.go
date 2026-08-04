package loyalty

// SPEC-W19 contract §4: usage metering for the loyalty app. Mirrors the
// W14 referrals metering idiom (internal/referrals/metering.go): one
// CloudEvents usage record on the shared usage topic
// (USAGE_EVENTS_TOPIC, default opendesk.usage.events):
//
//	{type: com.opendesk.usage.UsageRecord,
//	 data: {tenant_id, metric, value: 1, ts, meta: {...}}}
//
// Metric ownership: points_redeemed — THIS FILE, emitted once per
// non-idempotent redemption (a replayed redeem can never double-meter).
// Value is ALWAYS 1 per event; the redeemed points total lives in meta so
// billing can price tiers without re-parsing ledgers.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// UsageMetricPointsRedeemed is the metered unit emitted once per loyalty
// redemption (SPEC-W19 contract §4: loyalty bills on points_redeemed).
const UsageMetricPointsRedeemed = "points_redeemed"

// MarshalPointsRedeemedUsageRecord builds the usage-record payload for one
// redemption.
func MarshalPointsRedeemedUsageRecord(tenantSlug string, tenantID, contactID uuid.UUID, points int64, refID string) ([]byte, error) {
	evt := events.New("booking-service", "com.opendesk.usage.UsageRecord", tenantSlug, tenantID.String(), map[string]any{
		"tenant_id": tenantID.String(),
		"metric":    UsageMetricPointsRedeemed,
		"value":     1,
		"ts":        time.Now().UTC(),
		"meta": map[string]any{
			"contact_id": contactID.String(),
			"points":     points,
			"ref_id":     refID,
		},
	})
	return json.Marshal(evt)
}

// meterPointsRedeemed writes one points_redeemed usage record to the
// outbox after a successful Redeem (best-effort, post-commit — metering
// must never block a redemption, failures are logged for reconciliation).
// Called from Service.Redeem only on the NON-idempotent path.
func (s *Service) meterPointsRedeemed(ctx context.Context, tenantID, contactID uuid.UUID, res RedeemResult) {
	if s.UsageTopic == "" {
		return
	}
	payload, err := MarshalPointsRedeemedUsageRecord("", tenantID, contactID, res.Redeemed, res.RedeemRef)
	if err != nil {
		s.log().Warn("loyalty usage record marshal failed; skipping metering", zap.Error(err))
		return
	}
	if err := s.Store.EnqueueOutbox(ctx, contactID, s.UsageTopic, payload); err != nil {
		s.log().Warn("loyalty usage record enqueue failed; skipping metering", zap.Error(err))
	}
}
