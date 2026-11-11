// Privacy erase consumer (SPEC-W3 §2 innovation 13): consumes GDPR
// right-to-erasure tombstone CloudEvents from opendesk.privacy.events and
// anonymizes the matching booking contacts (name='erased', phone/email
// replaced by salted SHA-256). Connects to the broker directly via
// segmentio/kafka-go, like the command consumer; poison messages dead-letter
// to opendesk.dlq.
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// PrivacyEventType is the CloudEvent type emitted by GdprEraseWorkflow.
const PrivacyEventType = "PrivacyEraseRequested"

// privacyEnvelope is the CloudEvents envelope of privacy events.
type privacyEnvelope struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Phone    string `json:"phone"`
		Email    string `json:"email"`
		TenantID string `json:"tenant_id"`
	} `json:"data"`
}

// PrivacyConsumer anonymizes contacts on PrivacyEraseRequested events.
type PrivacyConsumer struct {
	reader *kafka.Reader
	dlq    *kafka.Writer
	store  *store.Store
	log    *zap.Logger
}

// NewPrivacy builds the privacy-events consumer.
func NewPrivacy(brokers []string, topic, group, dlqTopic string, st *store.Store, log *zap.Logger) *PrivacyConsumer {
	return &PrivacyConsumer{
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
		store: st,
		log:   log,
	}
}

// Run consumes until ctx is cancelled.
func (c *PrivacyConsumer) Run(ctx context.Context) error {
	c.log.Info("privacy erase consumer started")
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("fetch message: %w", err)
		}
		if err := c.processWithRetry(ctx, msg); err != nil {
			c.log.Error("privacy event dead-lettered",
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
func (c *PrivacyConsumer) Close() error {
	return errors.Join(c.reader.Close(), c.dlq.Close())
}

func (c *PrivacyConsumer) processWithRetry(ctx context.Context, msg kafka.Message) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := c.process(ctx, msg); err != nil {
			lastErr = err
			if errors.Is(err, errPermanentPrivacy) {
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

var errPermanentPrivacy = errors.New("permanent privacy event error")

func permanentPrivacy(err error) error { return fmt.Errorf("%w: %v", errPermanentPrivacy, err) }

// process handles one tombstone event. Unknown event types are acknowledged
// and skipped (the topic may gain more privacy event kinds later).
func (c *PrivacyConsumer) process(ctx context.Context, msg kafka.Message) error {
	var env privacyEnvelope
	if err := json.Unmarshal(msg.Value, &env); err != nil {
		return permanentPrivacy(fmt.Errorf("malformed privacy event: %v", err))
	}
	if env.Type != PrivacyEventType && env.Type != "com.opendesk.privacy."+PrivacyEventType {
		return nil
	}
	tenantID, err := uuid.Parse(env.Data.TenantID)
	if err != nil {
		return permanentPrivacy(fmt.Errorf("bad tenant_id %q", env.Data.TenantID))
	}
	if env.Data.Phone == "" && env.Data.Email == "" {
		return permanentPrivacy(errors.New("erase event carries neither phone nor email"))
	}
	// SPEC-W47 (W47-A item 6, A-DB DB-03): full per-subject erasure —
	// contacts PLUS every subject-keyed table (leads, promo_redemptions,
	// waitlist, referral_agents, referrals, civic_cases) in one tenant tx.
	sum, err := c.store.AnonymizeSubject(ctx, tenantID, env.Data.Phone, env.Data.Email)
	if err != nil {
		return fmt.Errorf("anonymize subject: %w", err)
	}
	c.log.Info("subject anonymized (GDPR erase)",
		zap.String("event_id", env.ID), zap.String("tenant_id", env.Data.TenantID),
		zap.Int64("contacts", sum.ContactsAnonymized), zap.Any("details", sum.Details))
	return nil
}

func (c *PrivacyConsumer) deadLetter(ctx context.Context, msg kafka.Message, cause error) error {
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
