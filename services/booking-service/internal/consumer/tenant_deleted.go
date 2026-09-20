// TenantDeleted cascade consumer (SPEC-W45 K9): consumes
// com.opendesk.identity.TenantDeleted CloudEvents from
// opendesk.identity.events and applies booking-service's share of the
// tenant teardown via the store's DeleteTenantData helper (anonymize
// contacts + cancel open bookings for the tenant — provided by the
// booking core team). Same reliability posture as the privacy consumer:
// explicit commits, 3 attempts, poison messages dead-lettered to
// opendesk.dlq. DeleteTenantData is idempotent, so redelivery converges.
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// TenantDeletedEventType is the CloudEvent type emitted by identity-service
// on tenant deletion (SPEC-W45 K9 contract).
const TenantDeletedEventType = "TenantDeleted"

// TenantDataDeleter is the store slice the cascade needs. Satisfied by
// *store.Store.DeleteTenantData (booking core contract: anonymize the
// tenant's contacts and cancel its open bookings; MUST be idempotent).
type TenantDataDeleter interface {
	DeleteTenantData(ctx context.Context, slug string) error
}

// tenantDeletedEnvelope is the CloudEvents envelope of identity lifecycle
// events (payload per K9: tenant_slug, tenant_id, deleted_at, actor).
type tenantDeletedEnvelope struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Subject string `json:"subject"` // tenant slug
	Data    struct {
		TenantSlug string `json:"tenant_slug"`
		TenantID   string `json:"tenant_id"`
		DeletedAt  string `json:"deleted_at"`
		Actor      string `json:"actor"`
	} `json:"data"`
}

// TenantDeletedConsumer applies the booking-side tenant teardown.
type TenantDeletedConsumer struct {
	reader  *kafka.Reader
	dlq     *kafka.Writer
	deleter TenantDataDeleter
	log     *zap.Logger
}

// NewTenantDeleted builds the identity-events consumer. brokers is a direct
// broker list (e.g. kafka:9092), topic the identity events topic
// (opendesk.identity.events), group a dedicated consumer group.
func NewTenantDeleted(brokers []string, topic, group, dlqTopic string, deleter TenantDataDeleter, log *zap.Logger) *TenantDeletedConsumer {
	return &TenantDeletedConsumer{
		reader: kafka.NewReader(kafka.ReaderConfig{
			Brokers:        brokers,
			Topic:          topic,
			GroupID:        group,
			MinBytes:       1,
			MaxBytes:       1 << 20,
			CommitInterval: 0, // explicit commits only, after successful processing
			StartOffset:    kafka.FirstOffset,
		}),
		dlq: &kafka.Writer{
			Addr:         kafka.TCP(brokers...),
			Topic:        dlqTopic,
			Balancer:     &kafka.Hash{},
			RequiredAcks: kafka.RequireOne,
		},
		deleter: deleter,
		log:     log,
	}
}

// Run consumes until ctx is cancelled.
func (c *TenantDeletedConsumer) Run(ctx context.Context) error {
	c.log.Info("tenant-deleted consumer started")
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("fetch message: %w", err)
		}
		if err := c.processWithRetry(ctx, msg); err != nil {
			c.log.Error("identity event dead-lettered",
				zap.String("key", string(msg.Key)), zap.Error(err))
			if dlqErr := c.deadLetter(ctx, msg, err); dlqErr != nil {
				c.log.Error("failed to write DLQ", zap.Error(dlqErr))
				continue // do not commit; redelivery attempted
			}
		}
		if err := c.reader.CommitMessages(ctx, msg); err != nil {
			c.log.Error("commit failed", zap.Error(err))
		}
	}
}

// Close releases reader and writer.
func (c *TenantDeletedConsumer) Close() error {
	return errors.Join(c.reader.Close(), c.dlq.Close())
}

func (c *TenantDeletedConsumer) processWithRetry(ctx context.Context, msg kafka.Message) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := c.Process(ctx, msg.Value); err != nil {
			lastErr = err
			if errors.Is(err, errPermanentTenantDeleted) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
			continue
		}
		return nil
	}
	return lastErr
}

var errPermanentTenantDeleted = errors.New("permanent identity event error")

func permanentTenantDeleted(err error) error {
	return fmt.Errorf("%w: %v", errPermanentTenantDeleted, err)
}

// Process handles one raw identity event (exported for testing). Other
// event types on the topic (MemberInvited, TenantProvisioned, ...) are
// acknowledged and skipped — this consumer cares only about TenantDeleted.
func (c *TenantDeletedConsumer) Process(ctx context.Context, raw []byte) error {
	var env tenantDeletedEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return permanentTenantDeleted(fmt.Errorf("malformed identity event: %v", err))
	}
	if env.Type != TenantDeletedEventType && env.Type != "com.opendesk.identity."+TenantDeletedEventType {
		return nil
	}
	slug := env.Data.TenantSlug
	if slug == "" {
		slug = env.Subject
	}
	if slug == "" {
		return permanentTenantDeleted(errors.New("TenantDeleted carries no tenant_slug"))
	}
	if err := c.deleter.DeleteTenantData(ctx, slug); err != nil {
		return fmt.Errorf("delete tenant data %s: %w", slug, err)
	}
	c.log.Info("tenant data deleted (K9 TenantDeleted cascade)",
		zap.String("event_id", env.ID), zap.String("tenant_slug", slug),
		zap.String("tenant_id", env.Data.TenantID), zap.String("actor", env.Data.Actor))
	return nil
}

func (c *TenantDeletedConsumer) deadLetter(ctx context.Context, msg kafka.Message, cause error) error {
	headers := append([]kafka.Header{}, msg.Headers...)
	headers = append(headers,
		kafka.Header{Key: "dlq-error", Value: []byte(cause.Error())},
		kafka.Header{Key: "dlq-origin-topic", Value: []byte(msg.Topic)},
		kafka.Header{Key: "dlq-time", Value: []byte(time.Now().UTC().Format(time.RFC3339))},
	)
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.dlq.WriteMessages(writeCtx, kafka.Message{
		Key:     msg.Key,
		Value:   msg.Value,
		Headers: headers,
		Time:    time.Now(),
	})
}
