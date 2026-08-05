// Package consumer implements the graph-sync Kafka consumers (SPEC-W28 §4
// WS-A): one consumer per input topic in the shared "graph-sync" group,
// explicit commits, poison messages dead-lettered to opendesk.dlq after 3
// attempts — mirroring booking-service (W24) and crm-sync-service consumer
// patterns.
package consumer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opendesk/graph-sync/internal/events"
	"github.com/opendesk/graph-sync/internal/metrics"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// maxAttempts is how many times a message is processed before dead-lettering
// (W24 pattern: DLQ after 3 attempts).
const maxAttempts = 3

// Handler processes one parsed CloudEvent.
type Handler func(ctx context.Context, evt events.CloudEvent) error

// Consumer reads one topic and applies events via its Handler.
type Consumer struct {
	topic   string
	reader  *kafka.Reader
	dlq     *kafka.Writer
	handler Handler
	metrics *metrics.Registry
	log     *zap.Logger
}

// New builds a consumer for topic in the shared `graph-sync` group.
func New(brokers []string, topic, group, dlqTopic string, handler Handler, m *metrics.Registry, log *zap.Logger) *Consumer {
	return &Consumer{
		topic: topic,
		reader: kafka.NewReader(kafka.ReaderConfig{
			Brokers:        brokers,
			Topic:          topic,
			GroupID:        group,
			MinBytes:       1,
			MaxBytes:       10 << 20,
			CommitInterval: 0, // explicit commits only, after successful processing
			StartOffset:    kafka.FirstOffset,
		}),
		dlq: &kafka.Writer{
			Addr:         kafka.TCP(brokers...),
			Topic:        dlqTopic,
			Balancer:     &kafka.Hash{},
			RequiredAcks: kafka.RequireOne,
		},
		handler: handler,
		metrics: m,
		log:     log,
	}
}

// Run consumes until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	c.log.Info("consumer started", zap.String("topic", c.topic))
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("fetch message: %w", err)
		}
		if err := c.processWithRetry(ctx, msg); err != nil {
			c.log.Error("event dead-lettered",
				zap.String("topic", c.topic), zap.String("key", string(msg.Key)), zap.Error(err))
			if dlqErr := c.deadLetter(ctx, msg, err); dlqErr != nil {
				c.log.Error("failed to write DLQ", zap.Error(dlqErr))
				continue // do not commit; redelivery attempted
			}
			c.metrics.Inc("events_dlq." + c.topic)
		}
		if err := c.reader.CommitMessages(ctx, msg); err != nil {
			c.log.Error("commit failed", zap.Error(err))
		}
	}
}

// Close releases reader and writer.
func (c *Consumer) Close() error {
	return errors.Join(c.reader.Close(), c.dlq.Close())
}

func (c *Consumer) processWithRetry(ctx context.Context, msg kafka.Message) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := c.process(ctx, msg); err != nil {
			lastErr = err
			// Poison payloads and deterministic validation errors will not
			// heal with retries — dead-letter immediately.
			if errors.Is(err, errPermanent) {
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

func (c *Consumer) process(ctx context.Context, msg kafka.Message) error {
	evt, err := events.Parse(msg.Value)
	if err != nil {
		return permanent(err)
	}
	start := time.Now()
	err = c.handler(ctx, evt)
	c.metrics.Observe("event_handle."+evt.Type, time.Since(start))
	if err != nil {
		c.metrics.Inc("events_failed." + evt.Type)
		return err
	}
	c.metrics.Inc("events_processed." + evt.Type)
	return nil
}

// deadLetter forwards a poison message to opendesk.dlq with error metadata.
func (c *Consumer) deadLetter(ctx context.Context, msg kafka.Message, cause error) error {
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

// errPermanent marks errors that retries cannot heal.
var errPermanent = errors.New("permanent event error")

func permanent(err error) error { return fmt.Errorf("%w: %v", errPermanent, err) }

// Permanent marks a handler error as unretryable (exported for the syncer:
// malformed payloads, missing tenant_id, etc.).
func Permanent(err error) error { return permanent(err) }
