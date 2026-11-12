// Payment-capture reconciliation consumer (SPEC-W49 B1): consumes
// FlutterwavePaymentCaptured CloudEvents from opendesk.payments.events
// (published by payments-service after the verified webhook capture) and
// flips the matching booking's payment_status to 'paid' — matched on
// bookings.payment_ref == event tx_ref within the event's tenant. Connects
// to the broker directly via segmentio/kafka-go (same client lib, explicit
// offset commits and DLQ discipline as the command/privacy consumers; NO
// Dapr/Temporal involvement).
//
// Poison posture (SPEC-W49 B1): an unknown tx_ref, a malformed envelope or
// missing fields are ACKED with a structured warning — never retried, never
// DLQ-looped (payments emits at-least-once and the capture itself is
// already durable rail-side; there is nothing a redelivery could heal).
// Only store/DB failures consume the retry budget and then dead-letter.
//
// Success emits BookingPaymentConfirmed via the transactional outbox (same
// tx as the state flip — notification-worker consumes it later); an
// amount/currency mismatch against the offering price flips
// payment_status='paid_amount_mismatch' and emits a
// BookingPaymentAmountMismatch alert instead, still acked.
//
// SPEC-W50 A1/A2: when the bookings.payment_ref fast path misses, the
// capture is retried through the booking_payment_attempts audit table
// (superseded-link fallback — same single-tx transition, tenant-scoped),
// and the outbox payload carries the enriched cross-scope contract fields
// (amount_minor int64, customer/contact/booking_ref, paid_at).
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"github.com/opendesk/booking-service/internal/store"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// PaymentsCapturedEventType is the CloudEvent type of the payments-service
// capture event (cross-scope contract, SPEC-W49 — the prefixed
// com.opendesk.payments. form is accepted too, mirroring the privacy
// consumer's tolerance).
const PaymentsCapturedEventType = "FlutterwavePaymentCaptured"

// BookingPaymentConfirmedEventType / BookingPaymentAmountMismatchEventType
// are the outbox CloudEvent types emitted on reconciliation.
const (
	BookingPaymentConfirmedEventType      = "com.opendesk.booking.BookingPaymentConfirmed"
	BookingPaymentAmountMismatchEventType = "com.opendesk.booking.BookingPaymentAmountMismatch"
)

// paymentsEventEnvelope is the CloudEvents envelope of payments events. The
// contract payload fields (tx_ref, amount in MAJOR units, currency,
// tenant_id, captured_at) live in data; tenant_id falls back to the
// envelope's tenantid extension for producers that only set the extension.
type paymentsEventEnvelope struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	TenantID string `json:"tenantid"`
	Data     struct {
		TxRef      string  `json:"tx_ref"`
		Amount     float64 `json:"amount"`
		Currency   string  `json:"currency"`
		TenantID   string  `json:"tenant_id"`
		CapturedAt string  `json:"captured_at"`
	} `json:"data"`
}

// PaymentsEventsConsumer reconciles payment captures onto bookings.
type PaymentsEventsConsumer struct {
	reader      *kafka.Reader
	dlq         *kafka.Writer
	store       *store.Store
	eventsTopic string // outbox topic for BookingPaymentConfirmed / alerts
	log         *zap.Logger
}

// NewPaymentsEvents builds the payments-events consumer.
func NewPaymentsEvents(brokers []string, topic, group, dlqTopic string, st *store.Store, eventsTopic string, log *zap.Logger) *PaymentsEventsConsumer {
	return &PaymentsEventsConsumer{
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
		store:       st,
		eventsTopic: eventsTopic,
		log:         log,
	}
}

// Run consumes until ctx is cancelled.
func (c *PaymentsEventsConsumer) Run(ctx context.Context) error {
	c.log.Info("payments events consumer started")
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("fetch message: %w", err)
		}
		if err := c.processWithRetry(ctx, msg.Value); err != nil {
			c.log.Error("payments event dead-lettered",
				zap.String("key", string(msg.Key)), zap.Error(err))
			if dlqErr := c.deadLetter(ctx, msg, err); dlqErr != nil {
				// SPEC-W43 K-03 (same posture as the command consumer):
				// kafka-go commits are CUMULATIVE — continuing past a failed
				// DLQ write cannot guarantee redelivery. Stop the loop; the
				// supervised restart re-fetches from the last commit.
				return fmt.Errorf("dlq write failed; stopping consumer to keep offset at the failed message: %w", dlqErr)
			}
		}
		if err := c.reader.CommitMessages(ctx, msg); err != nil {
			c.log.Error("commit failed", zap.Error(err))
		}
	}
}

// Close releases reader and writer.
func (c *PaymentsEventsConsumer) Close() error {
	return errors.Join(c.reader.Close(), c.dlq.Close())
}

func (c *PaymentsEventsConsumer) processWithRetry(ctx context.Context, raw []byte) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := c.Process(ctx, raw); err != nil {
			lastErr = err
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

// Process handles one raw payments-event payload. Poison (malformed
// envelope, non-capture type, missing/invalid fields, unknown tx_ref) is
// acknowledged with a structured log — nil error, no retry (SPEC-W49 B1).
// Only the store call can produce a retryable error.
func (c *PaymentsEventsConsumer) Process(ctx context.Context, raw []byte) error {
	var env paymentsEventEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		c.log.Warn("malformed payments event; acknowledging (poison)",
			zap.Error(err), zap.Int("bytes", len(raw)))
		return nil
	}
	if env.Type != PaymentsCapturedEventType && env.Type != "com.opendesk.payments."+PaymentsCapturedEventType {
		return nil // other payments event kinds: not ours, ack
	}
	txRef := env.Data.TxRef
	tenantStr := env.Data.TenantID
	if tenantStr == "" {
		tenantStr = env.TenantID
	}
	tenantID, terr := uuid.Parse(tenantStr)
	if txRef == "" || terr != nil {
		c.log.Warn("capture event missing usable tx_ref/tenant_id; acknowledging (poison)",
			zap.String("event_id", env.ID), zap.String("tx_ref", txRef), zap.String("tenant_id", tenantStr))
		return nil
	}
	// V-INT-50: notification-worker routes receipt providers via
	// ChannelRouter.Provider(channel, subject) — the CloudEvent SUBJECT must
	// carry the tenant slug or tenant SMS overrides (termii/africastalking)
	// never engage. bookingops sets it from the API layer's site resolution
	// (MarshalBookingEvent); the consumer reverse-resolves it from the same
	// site registry (GetSiteByTenant). Fail-closed for the RECEIPT, not the
	// lookup: a failed/missing slug must never drop a payment confirmation
	// — emit with an empty subject and log a structured warning.
	tenantSlug := ""
	if site, serr := c.store.GetSiteByTenant(ctx, tenantID); serr != nil {
		c.log.Warn("tenant slug lookup failed; emitting payment event with empty subject",
			zap.String("event_id", env.ID), zap.String("tenant_id", tenantStr),
			zap.String("tx_ref", txRef), zap.Error(serr))
	} else {
		tenantSlug = site.TenantSlug
	}
	outcome, bookingID, err := c.store.MarkBookingPaymentPaidTx(ctx, tenantID, txRef,
		env.Data.Amount, env.Data.Currency, c.outboxBuilder(tenantStr, tenantSlug, txRef, env.Data.Amount, env.Data.Currency))
	if errors.Is(err, store.ErrNotFound) {
		// SPEC-W50 A1 superseded-link fallback: the capture may reference
		// an OLDER checkout link (superseded when the customer
		// re-initialized payment) that no longer sits on
		// bookings.payment_ref but IS recorded in booking_payment_attempts.
		// Same tenant scope, same single-tx transition semantics.
		outcome, bookingID, err = c.store.MarkBookingPaymentPaidByAttemptTx(ctx, tenantID, txRef,
			env.Data.Amount, env.Data.Currency, c.outboxBuilder(tenantStr, tenantSlug, txRef, env.Data.Amount, env.Data.Currency))
	}
	if errors.Is(err, store.ErrNotFound) {
		// Unknown tx_ref: the capture is not ours (or the initialize leg
		// never persisted). ACK — redelivery cannot heal this.
		c.log.Warn("payment capture for unknown tx_ref; acknowledging",
			zap.String("event_id", env.ID), zap.String("tenant_id", tenantStr), zap.String("tx_ref", txRef))
		return nil
	}
	if err != nil {
		return fmt.Errorf("mark booking payment paid (tx_ref %s): %w", txRef, err)
	}
	switch outcome {
	case store.PaymentMarkPaid:
		c.log.Info("booking payment confirmed",
			zap.String("event_id", env.ID), zap.String("tenant_id", tenantStr),
			zap.String("tx_ref", txRef), zap.String("booking_id", bookingID.String()))
	case store.PaymentMarkAmountMismatch:
		c.log.Warn("payment capture amount/currency mismatch vs offering price; flagged paid_amount_mismatch",
			zap.String("event_id", env.ID), zap.String("tenant_id", tenantStr),
			zap.String("tx_ref", txRef), zap.String("booking_id", bookingID.String()),
			zap.Float64("captured_amount", env.Data.Amount), zap.String("captured_currency", env.Data.Currency))
	default: // already_paid / already_amount_mismatch — idempotent replay
		c.log.Info("payment capture replay; no-op",
			zap.String("event_id", env.ID), zap.String("tenant_id", tenantStr),
			zap.String("tx_ref", txRef), zap.String("outcome", string(outcome)))
	}
	return nil
}

// outboxBuilder returns the store.PaymentOutboxBuilder that marshals the
// reconciliation CloudEvent (BookingPaymentConfirmed on paid,
// BookingPaymentAmountMismatch on mismatch) for the atomic outbox insert.
// The payload follows the SPEC-W50/W51 sacred cross-scope contract:
// booking_id, tenant_id, tx_ref, amount_minor (int64 — the event arrives in
// MAJOR units; converted ONCE here at the boundary), currency,
// customer_name, contact_phone, contact_email ("" when unknown, never nil),
// booking_ref (SPEC-W51: the booking's public_ref BK-XXXXXXXX, supplied by
// the store from the locked booking row) and paid_at (RFC3339, time of the
// transition). The mismatch alert additionally carries
// expected_amount_minor (int64 — the offering price at the transition,
// SPEC-W51 contract delta). tenantSlug becomes the CloudEvent SUBJECT
// (V-INT-50 — the ChannelRouter provider-routing key), exactly as
// bookingops sets it on the other booking events.
func (c *PaymentsEventsConsumer) outboxBuilder(tenantID, tenantSlug, txRef string, amountMajor float64, currency string) store.PaymentOutboxBuilder {
	amountMinor := int64(math.Round(amountMajor * 100))
	return func(outcome store.PaymentMarkOutcome, info store.PaymentTransitionInfo) (string, []byte, error) {
		var evtType string
		switch outcome {
		case store.PaymentMarkPaid:
			evtType = BookingPaymentConfirmedEventType
		case store.PaymentMarkAmountMismatch:
			evtType = BookingPaymentAmountMismatchEventType
		default:
			return "", nil, nil
		}
		data := map[string]any{
			"booking_id":    info.BookingID.String(),
			"tenant_id":     tenantID,
			"tx_ref":        txRef,
			"amount_minor":  amountMinor,
			"currency":      currency,
			"customer_name": info.CustomerName,
			"contact_phone": info.ContactPhone,
			"contact_email": info.ContactEmail,
			"booking_ref":   info.BookingRef,
			"paid_at":       info.PaidAt.UTC().Format(time.RFC3339),
		}
		// SPEC-W51 sacred contract delta: ONLY the mismatch alert carries the
		// expected amount (the offering price in minor units captured at the
		// transition) — BookingPaymentConfirmed stays byte-contractual except
		// booking_ref now being the public_ref.
		if outcome == store.PaymentMarkAmountMismatch {
			data["expected_amount_minor"] = info.ExpectedAmountMinor
		}
		payload, err := json.Marshal(events.New("booking-service", evtType, tenantSlug, tenantID, data))
		if err != nil {
			return "", nil, err
		}
		return c.eventsTopic, payload, nil
	}
}

func (c *PaymentsEventsConsumer) deadLetter(ctx context.Context, msg kafka.Message, cause error) error {
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
