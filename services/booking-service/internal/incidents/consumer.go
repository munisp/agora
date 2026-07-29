package incidents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Consumer ingests Incident Data Packets from topic opendesk.incidents
// (consumer group booking-incidents, SPEC-W11 Part B §2). It connects to
// the broker directly via segmentio/kafka-go (NOT through Dapr), following
// the booking command consumer pattern: explicit commits after successful
// processing, poison messages dead-lettered to opendesk.dlq.
type Consumer struct {
	reader *kafka.Reader
	dlq    *kafka.Writer
	svc    *Service
	log    *zap.Logger
}

// maxAttempts mirrors the booking command consumer's retry budget.
const maxAttempts = 3

// envelope is the CloudEvents envelope wrapping an IDP on
// opendesk.incidents (type com.opendesk.incidents.IDPCreated; the IDP
// itself is the data payload).
type envelope struct {
	SpecVersion string          `json:"specversion"`
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	Subject     string          `json:"subject"` // tenant slug
	TenantID    string          `json:"tenantid"`
	Data        json.RawMessage `json:"data"`
}

// NewConsumer builds the incidents consumer. brokers is a direct broker
// list (e.g. kafka:9092).
func NewConsumer(brokers []string, topic, group, dlqTopic string, svc *Service, log *zap.Logger) *Consumer {
	return &Consumer{
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
		svc: svc,
		log: log,
	}
}

// Run consumes until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	c.log.Info("incidents consumer started")
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("fetch message: %w", err)
		}
		if err := c.processWithRetry(ctx, msg); err != nil {
			c.log.Error("incident dead-lettered",
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
func (c *Consumer) Close() error {
	rerr := c.reader.Close()
	werr := c.dlq.Close()
	return errors.Join(rerr, werr)
}

func (c *Consumer) processWithRetry(ctx context.Context, msg kafka.Message) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := c.process(ctx, msg); err != nil {
			lastErr = err
			// deterministic validation errors won't heal with retries
			if errors.Is(err, ErrInvalidInput) {
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

// process decodes one IDP envelope and persists it (idempotent on
// incident_id — replays and duplicate emissions are no-ops).
func (c *Consumer) process(ctx context.Context, msg kafka.Message) error {
	var env envelope
	if err := json.Unmarshal(msg.Value, &env); err != nil {
		return fmt.Errorf("%w: malformed incident envelope: %v", ErrInvalidInput, err)
	}
	if env.Type != "" && env.Type != EventTypeIDPCreated {
		return fmt.Errorf("%w: unexpected event type %q", ErrInvalidInput, env.Type)
	}
	var idp IDP
	if err := json.Unmarshal(env.Data, &idp); err != nil {
		return fmt.Errorf("%w: malformed idp payload: %v", ErrInvalidInput, err)
	}
	_, _, err := c.svc.Ingest(ctx, idp, env.Subject)
	return err
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
