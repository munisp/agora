// Package eventmail consumes domain event topics and sends the matching
// transactional emails through the existing Dapr SMTP binding
// (bindings-smtp) — the same send path as notifyoutbox.BindingSender.
//
// SPEC-W45 consumers:
//   - K8/STK O16: opendesk.identity.events — MemberInvited → invite email,
//     TenantProvisioned → tenant welcome email.
//   - ORPH O8: opendesk.billing.events — InvoicePaid / InvoiceVoided →
//     notification to the tenant billing contact.
//
// Reliability contract (mirrors booking-service/consumer and opsalerts):
// explicit commits only after a successful send; poison payloads are
// dropped with a log; handler errors retry maxAttempts times with backoff
// and then dead-letter to opendesk.dlq with error metadata. Event-id
// idempotency: a bounded in-process dedupe suppresses repeat sends on
// redelivery (commit-failure replays); across restarts the Kafka offset is
// the anchor (at-least-once, same posture as the other consumers here).
package eventmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// maxAttempts is how many times an event is processed before dead-lettering
// (booking consumer idiom).
const maxAttempts = 3

// Sender delivers one email (notifyoutbox.BindingSender satisfies it with
// channel "email").
type Sender interface {
	Send(ctx context.Context, channel, destination, subject, text string) error
}

// Handler processes one parsed CloudEvent. Returning a Permanent error
// dead-letters immediately; any other error is retried.
type Handler func(ctx context.Context, evt CloudEvent) error

// CloudEvent is the canonical envelope subset (SPEC §4) the consumers need.
type CloudEvent struct {
	ID       string         `json:"id"`
	Source   string         `json:"source"`
	Type     string         `json:"type"`
	Subject  string         `json:"subject"`
	TenantID string         `json:"tenantid"`
	Data     map[string]any `json:"data"`
}

// DataString reads a string field from the event data ("" when absent or
// not a string).
func (e CloudEvent) DataString(keys ...string) string {
	for _, k := range keys {
		if v, ok := e.Data[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// TopicEnabled maps an env topic value: "off" → disabled (""), anything
// else (including the default) passes through.
func TopicEnabled(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), "off") {
		return ""
	}
	return strings.TrimSpace(v)
}

// Consumer reads one topic and applies events via its Handler.
type Consumer struct {
	topic   string
	reader  *kafka.Reader
	dlq     *kafka.Writer
	handler Handler
	log     *zap.Logger

	mu   sync.Mutex
	seen map[string]struct{}
	ord  []string
}

// dedupeCap bounds the in-process event-id dedupe set.
const dedupeCap = 10000

// New builds a consumer for topic (explicit commits, DLQ after maxAttempts).
func New(brokers []string, topic, group, dlqTopic string, handler Handler, log *zap.Logger) *Consumer {
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
		log:     log,
		seen:    map[string]struct{}{},
	}
}

// duplicate reports whether the event id was already processed (and records
// it when not). Events without an id cannot be deduped and are processed
// (at-least-once beats dropping — syncer posture).
func (c *Consumer) duplicate(id string) bool {
	if id == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[id]; ok {
		return true
	}
	if len(c.ord) >= dedupeCap {
		delete(c.seen, c.ord[0])
		c.ord = c.ord[1:]
	}
	c.seen[id] = struct{}{}
	c.ord = append(c.ord, id)
	return false
}

// Run consumes until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	c.log.Info("eventmail consumer started", zap.String("topic", c.topic))
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("fetch %s: %w", c.topic, err)
		}
		if err := c.processWithRetry(ctx, msg); err != nil {
			c.log.Error("event dead-lettered",
				zap.String("topic", c.topic), zap.String("key", string(msg.Key)), zap.Error(err))
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
func (c *Consumer) Close() error {
	return errors.Join(c.reader.Close(), c.dlq.Close())
}

func (c *Consumer) processWithRetry(ctx context.Context, msg kafka.Message) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := c.Process(ctx, msg.Value); err != nil {
			lastErr = err
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

// Process handles one raw CloudEvent payload (exported for testing):
// parse → dedupe → handler. Malformed payloads are dropped (nil error —
// poison must not stall the topic).
func (c *Consumer) Process(ctx context.Context, raw []byte) error {
	var evt CloudEvent
	if err := json.Unmarshal(raw, &evt); err != nil {
		c.log.Warn("malformed event; dropping", zap.String("topic", c.topic), zap.Error(err))
		return nil
	}
	if c.duplicate(evt.ID) {
		c.log.Info("duplicate event skipped (idempotency)",
			zap.String("topic", c.topic), zap.String("event_id", evt.ID), zap.String("type", evt.Type))
		return nil
	}
	return c.handler(ctx, evt)
}

// deadLetter forwards a failed message to the DLQ with error metadata.
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

// Permanent marks a handler error as unretryable (malformed payload, no
// resolvable recipient, ...).
func Permanent(err error) error { return fmt.Errorf("%w: %v", errPermanent, err) }
