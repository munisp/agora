// Package consumer implements the Kafka command-channel consumer for
// opendesk.booking.commands (BookAppointment / RescheduleAppointment /
// CancelAppointment coming from the voice-agent path, SPEC §4). It connects
// to the broker directly via segmentio/kafka-go (NOT through Dapr), processes
// commands idempotently and dead-letters poison messages to opendesk.dlq.
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// maxAttempts is how many times a command is processed before dead-lettering.
const maxAttempts = 3

// commandEnvelope is the CloudEvents envelope of booking commands.
type commandEnvelope struct {
	SpecVersion string         `json:"specversion"`
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	Subject     string         `json:"subject"` // tenant slug
	TenantID    string         `json:"tenantid"`
	Data        map[string]any `json:"data"`
}

// Consumer reads booking commands and applies them via bookingops.
type Consumer struct {
	reader   *kafka.Reader
	dlq      *kafka.Writer
	ops      *bookingops.Service
	resolver *bookingops.TenantResolver
	log      *zap.Logger
}

// New builds the consumer. brokers is a direct broker list (e.g. kafka:9092).
func New(brokers []string, topic, group, dlqTopic string, ops *bookingops.Service, resolver *bookingops.TenantResolver, log *zap.Logger) *Consumer {
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
		ops:      ops,
		resolver: resolver,
		log:      log,
	}
}

// Run consumes until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	c.log.Info("booking command consumer started")
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("fetch message: %w", err)
		}
		if err := c.processWithRetry(ctx, msg); err != nil {
			c.log.Error("command dead-lettered",
				zap.String("key", string(msg.Key)), zap.Error(err))
			if dlqErr := c.deadLetter(ctx, msg, err); dlqErr != nil {
				// SPEC-W43 K-03: kafka-go commits are CUMULATIVE — a later
				// successful CommitMessages would silently advance the offset
				// PAST this failed message even if we never commit it
				// ourselves, so "continue" cannot guarantee redelivery. Stop
				// the loop and surface the failure instead: the consumer
				// restarts (supervised in main) and re-fetches from the last
				// committed offset.
				return fmt.Errorf("dlq write failed; stopping consumer to keep offset at the failed message: %w", dlqErr)
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
			if errors.Is(err, bookingops.ErrInvalidInput) || errors.Is(err, bookingops.ErrPhoneRequired) {
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

// process dispatches one command message.
func (c *Consumer) process(ctx context.Context, msg kafka.Message) error {
	var env commandEnvelope
	if err := json.Unmarshal(msg.Value, &env); err != nil {
		return fmt.Errorf("%w: malformed command envelope: %v", bookingops.ErrInvalidInput, err)
	}
	if env.TenantID == "" {
		return fmt.Errorf("%w: missing tenantid extension", bookingops.ErrInvalidInput)
	}
	tenantID, err := uuid.Parse(env.TenantID)
	if err != nil {
		return fmt.Errorf("%w: bad tenantid %q", bookingops.ErrInvalidInput, env.TenantID)
	}
	// SPEC-W43 K-05: tenant context must resolve — the previous silent
	// fallback left the command with a zero TenantInfo (UTC timezone, no
	// pack policy), mis-booking the tenant's calendar. A resolver failure
	// is retryable (not ErrInvalidInput), so it goes through the retry
	// budget and then the DLQ.
	info, err := c.resolver.BySlug(ctx, env.Subject)
	if err != nil {
		return fmt.Errorf("resolve tenant slug %q: %w", env.Subject, err)
	}
	// Cross-check: the subject (slug) must resolve to the SAME tenant the
	// envelope's tenantid extension claims — a mismatch means a confused
	// producer and is deterministically undeliverable (straight to DLQ).
	if info.ID != tenantID {
		return fmt.Errorf("%w: subject %q resolves to tenant %s, not envelope tenantid %s",
			bookingops.ErrInvalidInput, env.Subject, info.ID, tenantID)
	}
	timezone := info.Timezone

	switch env.Type {
	case "com.opendesk.booking.command.BookAppointment", "BookAppointment":
		return c.bookAppointment(ctx, env, tenantID, info)
	case "com.opendesk.booking.command.RescheduleAppointment", "RescheduleAppointment":
		return c.rescheduleAppointment(ctx, env, tenantID, timezone)
	case "com.opendesk.booking.command.CancelAppointment", "CancelAppointment":
		return c.cancelAppointment(ctx, env, tenantID)
	default:
		return fmt.Errorf("%w: unknown command type %q", bookingops.ErrInvalidInput, env.Type)
	}
}

func strField(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

func uuidField(data map[string]any, key string) (uuid.UUID, error) {
	v := strField(data, key)
	if v == "" {
		return uuid.Nil, fmt.Errorf("%w: missing %s", bookingops.ErrInvalidInput, key)
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: bad %s %q", bookingops.ErrInvalidInput, key, v)
	}
	return id, nil
}

func timeField(data map[string]any, key string) (time.Time, error) {
	v := strField(data, key)
	if v == "" {
		return time.Time{}, fmt.Errorf("%w: missing %s", bookingops.ErrInvalidInput, key)
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: bad %s %q", bookingops.ErrInvalidInput, key, v)
	}
	return t, nil
}

func (c *Consumer) bookAppointment(ctx context.Context, env commandEnvelope, tenantID uuid.UUID, tenant bookingops.TenantInfo) error {
	offeringID, err := uuidField(env.Data, "offering_id")
	if err != nil {
		return err
	}
	teamMemberID, err := uuidField(env.Data, "team_member_id")
	if err != nil {
		return err
	}
	startsAt, err := timeField(env.Data, "starts_at")
	if err != nil {
		return err
	}
	// Server-side guard of the phone-confirmation policy (SPEC §11).
	phone := strField(env.Data, "phone")
	if phone == "" {
		return bookingops.ErrPhoneRequired
	}
	// Idempotency: the CloudEvent ID is the natural dedup key for commands.
	key := strField(env.Data, "idempotency_key")
	if key == "" {
		key = env.ID
	}
	in := bookingops.CreateInput{
		TenantID:     tenantID,
		TenantSlug:   env.Subject,
		Timezone:     tenant.Timezone,
		OfferingID:   offeringID,
		TeamMemberID: teamMemberID,
		Contact: &bookingops.ContactInput{
			Name:  strField(env.Data, "contact_name"),
			Phone: phone,
			Email: strField(env.Data, "email"),
		},
		StartsAt:       startsAt,
		Source:         "voice",
		IdempotencyKey: key,
	}
	// SPEC-CRM §C3: voice bookings get the tenant's industry + pack booking
	// policy exactly like the REST and public paths (correct pack workflow
	// and deposit amount in the saga).
	in.Industry = tenant.Industry
	if tenant.Pack != nil {
		policy := tenant.Pack.BookingPolicy
		in.BookingPolicy = &policy
	}
	_, err = c.ops.Create(ctx, in)
	return err
}

func (c *Consumer) rescheduleAppointment(ctx context.Context, env commandEnvelope, tenantID uuid.UUID, timezone string) error {
	bookingID, err := uuidField(env.Data, "booking_id")
	if err != nil {
		return err
	}
	startsAt, err := timeField(env.Data, "starts_at")
	if err != nil {
		return err
	}
	_, err = c.ops.Reschedule(ctx, tenantID, env.Subject, timezone, bookingID, startsAt)
	return err
}

func (c *Consumer) cancelAppointment(ctx context.Context, env commandEnvelope, tenantID uuid.UUID) error {
	bookingID, err := uuidField(env.Data, "booking_id")
	if err != nil {
		return err
	}
	reason := strField(env.Data, "reason")
	if reason == "" {
		reason = "voice_command"
	}
	_, err = c.ops.Cancel(ctx, tenantID, env.Subject, bookingID, reason)
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
