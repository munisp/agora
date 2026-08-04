package socialpub

// SPEC-W21 Agent B: usage metering — one social_ad_launched usage record
// per successful ad launch, mirroring internal/helpdesk/metering.go (which
// mirrors geo/incidents/referrals):
//
//	{type: com.opendesk.usage.UsageRecord,
//	 data: {tenant_id, metric, value: 1, ts, meta: {...}}}
//
// Value is ALWAYS 1 per launch; context lives in meta so billing can price
// tiers without re-parsing ads. Emitted best-effort post-commit — metering
// must never block a launch; failures are logged for reconciliation.
// meterLaunched is called only on the path that actually transitioned the
// ad INTO review with a provider_ad_id (never on a rejected/failed launch).

import (
	"context"
	"encoding/json"
	"time"

	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// UsageMetricAdLaunched is the metered unit emitted once per ad launch
// (SPEC-W21 Agent B — social-publisher: social_ad_launched).
const UsageMetricAdLaunched = "social_ad_launched"

// MarshalAdLaunchedUsageRecord builds the usage-record payload for one
// launched ad (NO creative body — same privacy posture as the events).
func MarshalAdLaunchedUsageRecord(tenantSlug string, a Ad, provider string) ([]byte, error) {
	meta := map[string]any{
		"ad_id":     a.ID.String(),
		"provider":  provider,
		"objective": a.Objective,
		"political": a.Political,
	}
	if a.ProviderAdID != nil {
		meta["provider_ad_id"] = *a.ProviderAdID
	}
	evt := events.New("booking-service", "com.opendesk.usage.UsageRecord", tenantSlug, a.TenantID.String(), map[string]any{
		"tenant_id": a.TenantID.String(),
		"metric":    UsageMetricAdLaunched,
		"value":     1,
		"ts":        time.Now().UTC(),
		"meta":      meta,
	})
	return json.Marshal(evt)
}

// meterLaunched writes one social_ad_launched usage record to the outbox
// after a successful launch (best-effort, post-commit — the same posture
// as the helpdesk meter).
func (h *Handlers) meterLaunched(ctx context.Context, a Ad, provider, tenantSlug string) {
	if h.UsageTopic == "" {
		return
	}
	payload, err := MarshalAdLaunchedUsageRecord(tenantSlug, a, provider)
	if err != nil {
		h.log().Warn("social usage record marshal failed; skipping metering", zap.Error(err))
		return
	}
	if err := h.Store.EnqueueOutbox(ctx, a.ID, h.UsageTopic, payload); err != nil {
		h.log().Warn("social usage record enqueue failed; skipping metering", zap.Error(err))
	}
}
