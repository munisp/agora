package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/opendesk/crm-sync-service/internal/events"
	"github.com/opendesk/crm-sync-service/internal/metrics"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// maxAttempts is how many times a message is processed before dead-lettering
// (SPEC-CRM §B: DLQ after 3 attempts).
const maxAttempts = 3

// Handler processes one parsed CloudEvent.
type Handler func(ctx context.Context, evt events.CloudEvent) error

// messageReader is the kafka.Reader surface the Consumer needs (interface
// for testability; *kafka.Reader satisfies it).
type messageReader interface {
	FetchMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// dlqWriter is the kafka.Writer surface the Consumer needs (interface for
// testability; *kafka.Writer satisfies it).
type dlqWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// Consumer reads one topic and applies events via its Handler.
type Consumer struct {
	topic   string
	reader  messageReader
	dlq     dlqWriter
	handler Handler
	metrics *metrics.Registry
	log     *zap.Logger
}

// New builds a consumer for topic in the shared `crm-sync` group.
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
			// SPEC-W80 A5: a graceful shutdown cancels ctx mid-processing
			// (processWithRetry returns ctx.Err()). Dead-lettering a message
			// that was never really POISON — it simply lost the shutdown
			// race — pollutes the DLQ with healthy events; instead stop
			// consuming and let the uncommitted offset redeliver on the
			// next boot.
			if errors.Is(err, context.Canceled) {
				c.log.Info("shutdown during processing; message left uncommitted (no DLQ)",
					zap.String("topic", c.topic), zap.String("key", string(msg.Key)))
				return nil
			}
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

// dlqRecord wraps the dead-lettered message with forensic context.
type dlqRecord struct {
	SourceTopic string          `json:"source_topic"`
	EventID     string          `json:"event_id,omitempty"`
	EventType   string          `json:"event_type,omitempty"`
	Error       string          `json:"error"`
	FailedAt    time.Time       `json:"failed_at"`
	Payload     json.RawMessage `json:"payload"`
}

func (c *Consumer) deadLetter(ctx context.Context, msg kafka.Message, cause error) error {
	rec := dlqRecord{
		SourceTopic: c.topic,
		Error:       cause.Error(),
		FailedAt:    time.Now().UTC(),
		Payload:     msg.Value,
	}
	if evt, err := events.Parse(msg.Value); err == nil {
		rec.EventID = evt.ID
		rec.EventType = evt.Type
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal dlq record: %w", err)
	}
	wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.dlq.WriteMessages(wctx, kafka.Message{Key: msg.Key, Value: b})
}
