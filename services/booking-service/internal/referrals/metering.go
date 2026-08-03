package referrals

// SPEC-W14 Agent D (ADDITIVE): usage metering for the referral & commission
// engine. This mirrors the existing metering patterns —
// geo.MarshalGeoUsageRecord (geo_campaign_message) and incidents meter
// (incident_alert_message) — building the CloudEvents payload for one usage
// record on topic opendesk.usage.events:
//
//	{type: com.opendesk.usage.UsageRecord,
//	 data: {tenant_id, metric, value: 1, ts, meta: {...}}}
//
// Metric ownership (Wave-14 coordination):
//
//   - referral_verified — THIS FILE (Agent D), called from Service.Verify
//     (Agent A's service.go — the two integration lines there are flagged
//     "SPEC-W14 Agent D (additive)").
//   - commission_payout — Agent B's payout flow (payouts.go) emits it at
//     the payout-PAID transition, same-tx with the status flip
//     (UsageMetricCommissionPayout lives there). This file deliberately
//     does NOT redeclare or re-emit that metric — one row per paid payout.
//   - commission_recon_alert — Agent B's recon workflow (contract §5
//     "metered notification"), also in payouts.go.
//
// Value is ALWAYS 1 per event (one verification); context lives in meta so
// billing can price tiers without re-parsing ledgers.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// UsageMetricReferralVerified is the metered unit emitted once per
// referral verification (a referral that passed the rules engine and
// flipped to verified/converted, SPEC-W14 contract §1).
const UsageMetricReferralVerified = "referral_verified"

// MarshalReferralVerifiedUsageRecord builds the usage-record payload for
// one verified referral. campaignID may be nil (organic referral).
func MarshalReferralVerifiedUsageRecord(tenantSlug string, tenantID, referralID uuid.UUID, referrerType, referrerID string, campaignID *uuid.UUID) ([]byte, error) {
	meta := map[string]any{
		"referral_id":   referralID.String(),
		"referrer_type": referrerType, // contact | agent | staff
		"referrer_id":   referrerID,
	}
	if campaignID != nil && *campaignID != uuid.Nil {
		meta["campaign_id"] = campaignID.String()
	}
	evt := events.New("booking-service", "com.opendesk.usage.UsageRecord", tenantSlug, tenantID.String(), map[string]any{
		"tenant_id": tenantID.String(),
		"metric":    UsageMetricReferralVerified,
		"value":     1,
		"ts":        time.Now().UTC(),
		"meta":      meta,
	})
	return json.Marshal(evt)
}

// meterReferralVerified writes one referral_verified usage record to the
// outbox after a successful Verify (best-effort, post-commit — the same
// posture as the incidents meter and the §6 hooks: metering must never
// block a verification, failures are logged for reconciliation). Called
// from Service.Verify only on the NON-idempotent path, so a replayed
// verify can never double-meter.
func (s *Service) meterReferralVerified(ctx context.Context, ref Referral, tenantSlug string) {
	if s.UsageTopic == "" {
		return
	}
	payload, err := MarshalReferralVerifiedUsageRecord(tenantSlug, ref.TenantID, ref.ID, ref.ReferrerType, ref.ReferrerID, ref.CampaignID)
	if err != nil {
		s.log().Warn("referral usage record marshal failed; skipping metering", zap.Error(err))
		return
	}
	if err := s.Store.EnqueueOutbox(ctx, ref.ID, s.UsageTopic, payload); err != nil {
		s.log().Warn("referral usage record enqueue failed; skipping metering", zap.Error(err))
	}
}
