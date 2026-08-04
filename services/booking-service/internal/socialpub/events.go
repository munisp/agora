package socialpub

// SPEC-W21 Agent B: CloudEvents on the meaningful social-publisher
// lifecycle — post published / ad launched / ad rejected on topic
// opendesk.social.events.v1 via the shared transactional outbox (same
// idiom as helpdesk's emit). Emission is a graceful no-op when the topic
// is empty (events disabled).
//
// PRIVACY CONTRACT: payloads carry ids + metadata ONLY — never the
// creative body, media URL or disclaimer text (the event bus is
// cross-tenant infrastructure; creative content stays in the database).

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// CloudEvents types on topic opendesk.social.events.v1.
const (
	EventTypePostPublished = "com.opendesk.social.PostPublished"
	EventTypeAdLaunched    = "com.opendesk.social.AdLaunched"
	EventTypeAdRejected    = "com.opendesk.social.AdRejected"
)

// postPublishedData builds the PostPublished payload (NO creative body).
func postPublishedData(p Post, provider string) map[string]any {
	data := map[string]any{
		"tenant_id":   p.TenantID.String(),
		"post_id":     p.ID.String(),
		"account_id":  p.AccountID.String(),
		"creative_id": p.CreativeID.String(),
		"provider":    provider,
	}
	if p.ProviderPostID != nil {
		data["provider_post_id"] = *p.ProviderPostID
	}
	if p.PublishedAt != nil {
		data["published_at"] = *p.PublishedAt
	}
	return data
}

// adLaunchedData builds the AdLaunched payload (NO creative body).
func adLaunchedData(a Ad, provider string) map[string]any {
	data := map[string]any{
		"tenant_id":          a.TenantID.String(),
		"ad_id":              a.ID.String(),
		"account_id":         a.AccountID.String(),
		"creative_id":        a.CreativeID.String(),
		"provider":           provider,
		"objective":          a.Objective,
		"budget_kobo":        a.BudgetKobo,
		"daily_budget_kobo":  a.DailyBudgetKobo,
		"political":          a.Political,
		"disclaimer_present": EffectiveDisclaimer(&a, nil) != "",
	}
	if a.ProviderAdID != nil {
		data["provider_ad_id"] = *a.ProviderAdID
	}
	return data
}

// adRejectedData builds the AdRejected payload (NO creative body).
func adRejectedData(a Ad, provider, reason string) map[string]any {
	return map[string]any{
		"tenant_id":   a.TenantID.String(),
		"ad_id":       a.ID.String(),
		"account_id":  a.AccountID.String(),
		"creative_id": a.CreativeID.String(),
		"provider":    provider,
		"objective":   a.Objective,
		"political":   a.Political,
		"reason":      truncate(reason, maxErrorLen),
	}
}

// emit publishes one social lifecycle CloudEvent to the events topic via
// the outbox. Best-effort (same posture as helpdesk): the row is durable;
// an enqueue failure is logged loudly for reconciliation.
func (h *Handlers) emit(ctx context.Context, aggregateID uuid.UUID, eventType, tenantSlug, tenantID string, data map[string]any) {
	if h.EventsTopic == "" {
		return
	}
	payload, err := json.Marshal(events.New("booking-service", eventType, tenantSlug, tenantID, data))
	if err != nil {
		h.log().Warn("social event marshal failed; skipping emission", zap.Error(err))
		return
	}
	if err := h.Store.EnqueueOutbox(ctx, aggregateID, h.EventsTopic, payload); err != nil {
		h.log().Error("social event enqueue failed; row durable but event row lost — reconcile",
			zap.String("event_type", eventType), zap.Error(err))
	}
}
