package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/store"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/segmentio/kafka-go"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// SubscriptionStore is the slice of persistence the dispatcher needs
// (*store.Store satisfies it; tests use an in-memory fake).
type SubscriptionStore interface {
	ActiveSubscriptions(ctx context.Context, tenantID uuid.UUID) ([]store.WebhookSubscription, error)
	CreateDelivery(ctx context.Context, d *store.WebhookDelivery) error
}

// WorkflowStarter abstracts Temporal workflow starts (client.Client
// satisfies it via ExecuteWorkflow).
type WorkflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflowType interface{}, args ...interface{}) (client.WorkflowRun, error)
}

// Dispatcher consumes booking + conversation events and fans them out to
// matching webhook subscriptions.
type Dispatcher struct {
	readers   []*kafka.Reader
	store     SubscriptionStore
	starter   WorkflowStarter
	taskQueue string
	log       *zap.Logger
}

// New builds the dispatcher: one reader per topic, all in the
// notification-webhooks consumer group (explicit commits). N-03: a failed
// fan-out is NOT committed — the offset is left behind so the event is
// redelivered (restart/rebalance); delivery rows dedupe on
// UNIQUE(sub_id, event_id) and workflow ids are deterministic
// (whd-{sub_id}-{event_id}), so redelivery converges instead of
// duplicating.
func New(brokers []string, topics []string, group string, st SubscriptionStore, starter WorkflowStarter, taskQueue string, log *zap.Logger) *Dispatcher {
	d := &Dispatcher{store: st, starter: starter, taskQueue: taskQueue, log: log}
	for _, topic := range topics {
		d.readers = append(d.readers, kafka.NewReader(kafka.ReaderConfig{
			Brokers:        brokers,
			Topic:          topic,
			GroupID:        group,
			MinBytes:       1,
			MaxBytes:       10 << 20,
			CommitInterval: 0,
			StartOffset:    kafka.FirstOffset,
		}))
	}
	return d
}

// Run consumes all topics until ctx is cancelled; the first fatal reader
// error is returned.
func (d *Dispatcher) Run(ctx context.Context) error {
	errCh := make(chan error, len(d.readers))
	for _, r := range d.readers {
		go func(r *kafka.Reader) {
			d.log.Info("webhook dispatcher consuming", zap.String("topic", r.Config().Topic))
			for {
				msg, err := r.FetchMessage(ctx)
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return
					}
					errCh <- fmt.Errorf("fetch %s: %w", r.Config().Topic, err)
					return
				}
				if err := d.Process(ctx, msg.Value); err != nil {
					// N-03: never commit a failed fan-out — the offset stays
					// behind and the event is redelivered on the next
					// restart/rebalance. Delivery rows + workflow ids are
					// deterministic, so redelivery is safe.
					d.log.Error("webhook fan-out failed; offset NOT committed (redelivery pending)",
						zap.String("key", string(msg.Key)), zap.Error(err))
					continue
				}
				if err := r.CommitMessages(ctx, msg); err != nil {
					d.log.Error("commit failed", zap.Error(err))
				}
			}
		}(r)
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

// Close releases all readers.
func (d *Dispatcher) Close() error {
	var errs []error
	for _, r := range d.readers {
		errs = append(errs, r.Close())
	}
	return errors.Join(errs...)
}

// eventEnvelope is the CloudEvents envelope shared by all OpenDesk topics.
type eventEnvelope struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Subject  string          `json:"subject"`  // tenant slug
	TenantID string          `json:"tenantid"` // CloudEvents extension
	Data     json.RawMessage `json:"data"`
}

// DeliveryWorkflowID is the deterministic Temporal workflow id of one
// (subscription, event) delivery (N-03): whd-{sub_id}-{event_id}.
func DeliveryWorkflowID(subID, eventID string) string {
	return "whd-" + subID + "-" + eventID
}

// Process handles one raw event payload (exported for testing): match
// subscriptions of the event's tenant, insert a pending delivery row per
// match and start its WebhookDeliveryWorkflow.
func (d *Dispatcher) Process(ctx context.Context, raw []byte) error {
	var env eventEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		d.log.Warn("malformed event; skipping", zap.Error(err))
		return nil
	}
	if env.Type == "" || env.TenantID == "" {
		return nil // no tenant → no subscription can match
	}
	tenantID, err := uuid.Parse(env.TenantID)
	if err != nil {
		return nil
	}
	subs, err := d.store.ActiveSubscriptions(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("list subscriptions: %w", err)
	}
	var errs []error
	for _, sub := range subs {
		if !EventMatches(sub.Events, env.Type) {
			continue
		}
		delivery := store.WebhookDelivery{
			SubID:     sub.ID,
			TenantID:  tenantID,
			EventID:   env.ID,
			EventType: env.Type,
		}
		if err := d.store.CreateDelivery(ctx, &delivery); err != nil {
			errs = append(errs, fmt.Errorf("create delivery for sub %s: %w", sub.ID, err))
			continue
		}
		_, err := d.starter.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			// N-03: deterministic workflow id — a redelivered event maps to
			// the SAME workflow (Temporal dedupes the start), so redelivery
			// after a partial fan-out converges.
			ID:        DeliveryWorkflowID(sub.ID.String(), env.ID),
			TaskQueue: d.taskQueue,
		}, "WebhookDeliveryWorkflow", workflows.WebhookDeliveryInput{
			DeliveryID: delivery.ID.String(),
			URL:        sub.URL,
			Secret:     sub.Secret,
			EventType:  env.Type,
			Body:       raw,
		})
		if err != nil {
			if strings.Contains(err.Error(), "already started") {
				continue // redelivered event: the workflow is already running
			}
			errs = append(errs, fmt.Errorf("start delivery workflow %s: %w", delivery.ID, err))
			continue
		}
		d.log.Info("webhook delivery started",
			zap.String("delivery_id", delivery.ID.String()),
			zap.String("sub_id", sub.ID.String()),
			zap.String("event_type", env.Type))
	}
	return errors.Join(errs...)
}
