package apps

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/identity-service/internal/events"
	"go.uber.org/zap"
)

// EventPublisher publishes a CloudEvent via Dapr pubsub (daprc.Client
// satisfies it; tests substitute a fake). Same interface shape as the
// consent handler's publisher.
type EventPublisher interface {
	PublishEvent(ctx context.Context, pubsub, topic string, data any) error
}

// Publisher emits the app-lifecycle CloudEvents of SPEC-W18 §1 on
// opendesk.apps.lifecycle.v1. Publishing is best-effort, mirroring the
// consent erasure idiom: the tenant_apps row is the durable record, a failed
// publish is logged (not fatal) and can be reconciled from the registry.
type Publisher struct {
	Events EventPublisher
	PubSub string
	Topic  string
	Logger *zap.Logger
}

// Lifecycle publishes one app lifecycle event with the contract payload
// {tenant_id, app_id, status, actor, ts}. A nil Publisher or nil Events
// (Dapr sidecar not wired, e.g. tests/dev) is a graceful no-op, logged at
// debug level; publish errors are logged at error level and swallowed.
func (p *Publisher) Lifecycle(ctx context.Context, eventType string, tenantID uuid.UUID, appID string, status AppStatus, actor string) {
	if p == nil || p.Events == nil {
		if p != nil && p.Logger != nil {
			p.Logger.Debug("dapr publisher unavailable; skipping app lifecycle event",
				zap.String("event_type", eventType), zap.String("app_id", appID))
		}
		return
	}
	evt := events.New("identity-service", eventType, appID, tenantID.String(), map[string]any{
		"tenant_id": tenantID.String(),
		"app_id":    appID,
		"status":    string(status),
		"actor":     actor,
		"ts":        time.Now().UTC(),
	})
	if err := p.Events.PublishEvent(ctx, p.PubSub, p.Topic, evt); err != nil {
		p.Logger.Error("failed to publish app lifecycle event",
			zap.String("event_type", eventType),
			zap.String("app_id", appID),
			zap.String("tenant_id", tenantID.String()),
			zap.Error(err))
	}
}
