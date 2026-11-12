// Package signals bridges opendesk.booking.events (Kafka) to Temporal
// signals for the per-booking child workflows started by the booking saga
// (SPEC-CRM §C2):
//
//	BookingCancelled → signal pack-{bookingId} and reminder-{bookingId} with
//	                   "booking-event" {type: "cancelled"}
//	BookingNoShow    → signal pack-{bookingId} with "NoShow"
//
// Delivery is best-effort: the workflows re-check booking state via
// activities, so a missed signal is not fatal. Workflows that are not
// running (completed or never started) are logged and acknowledged — never
// retried and never dead-lettered.
package signals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/segmentio/kafka-go"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// Signaller abstracts the Temporal client (client.Client satisfies it).
type Signaller interface {
	SignalWorkflow(ctx context.Context, workflowID, runID, signalName string, arg interface{}) error
}

// WorkflowStarter abstracts Temporal workflow starts (client.Client
// satisfies it via ExecuteWorkflow) — used to kick off
// WaitlistBackfillWorkflow on BookingCancelled (SPEC-W3 §3 innovation 7).
type WorkflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflowType interface{}, args ...interface{}) (client.WorkflowRun, error)
}

// Bridge consumes booking events and forwards them as Temporal signals.
type Bridge struct {
	reader    *kafka.Reader
	temporal  Signaller
	starter   WorkflowStarter // nil disables waitlist backfill starts
	taskQueue string
	log       *zap.Logger
}

// Option customizes a Bridge.
type Option func(*Bridge)

// WithBackfillStarter enables WaitlistBackfillWorkflow starts on
// BookingCancelled events.
func WithBackfillStarter(starter WorkflowStarter, taskQueue string) Option {
	return func(b *Bridge) {
		b.starter = starter
		b.taskQueue = taskQueue
	}
}

// New builds the bridge. brokers is a direct broker list (e.g. kafka:9092);
// config mirrors booking-service's consumer (explicit commits, no DLQ —
// unknown/completed workflows are logged and acknowledged).
func New(brokers []string, topic, group string, temporal Signaller, log *zap.Logger, opts ...Option) *Bridge {
	b := &Bridge{
		reader: kafka.NewReader(kafka.ReaderConfig{
			Brokers:        brokers,
			Topic:          topic,
			GroupID:        group,
			MinBytes:       1,
			MaxBytes:       10 << 20,
			CommitInterval: 0, // explicit commits only, after processing
			StartOffset:    kafka.FirstOffset,
		}),
		temporal: temporal,
		log:      log,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Run consumes until ctx is cancelled.
func (b *Bridge) Run(ctx context.Context) error {
	b.log.Info("booking-events signal bridge started")
	for {
		msg, err := b.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("fetch message: %w", err)
		}
		if err := b.Process(ctx, msg.Value); err != nil {
			// Transient signalling failure (Temporal unreachable). Signals
			// are best-effort: log and ack instead of redelivering forever.
			b.log.Error("signal delivery failed; acknowledging anyway",
				zap.String("key", string(msg.Key)), zap.Error(err))
		}
		if err := b.reader.CommitMessages(ctx, msg); err != nil {
			b.log.Error("commit failed", zap.Error(err))
		}
	}
}

// Close releases the reader.
func (b *Bridge) Close() error { return b.reader.Close() }

// eventEnvelope is the CloudEvents envelope of booking events.
type eventEnvelope struct {
	Type     string         `json:"type"`
	Subject  string         `json:"subject"`  // tenant slug
	TenantID string         `json:"tenantid"` // CloudEvents extension
	Data     map[string]any `json:"data"`
}

func (env eventEnvelope) bookingID() string {
	if v, ok := env.Data["booking_id"].(string); ok {
		return v
	}
	return ""
}

// Process handles one raw event payload (exported for testing).
func (b *Bridge) Process(ctx context.Context, raw []byte) error {
	var env eventEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		b.log.Warn("malformed booking event; skipping", zap.Error(err))
		return nil
	}

	// SPEC-W48 (A2/G): entry-scoped waitlist events carry NO booking_id —
	// they must be handled before the booking-scoped gate below or they are
	// silently dropped (the exact gap W48-A flagged).
	switch env.Type {
	case eventTypeWaitlistEntryNotified:
		return b.startWaitlistNotify(ctx, env)
	case eventTypeBookingPaymentConfirmed, eventTypeBookingPaymentConfirmedBare:
		// SPEC-W50-B: payment receipt — handled BEFORE the booking-scoped
		// gate so a fail-closed ack (missing booking_id/tx_ref, no
		// reachable contact) is logged loudly, never silently dropped.
		return b.startPaymentReceipt(ctx, env)
	case eventTypeBookingPaymentAmountMismatch, eventTypeBookingPaymentAmountMismatchBare:
		// SPEC-W51-A: payment amount-mismatch OPS alert — handled BEFORE
		// the booking-scoped gate so a fail-closed ack (missing
		// booking_id/tx_ref) is logged loudly, never silently dropped.
		return b.startPaymentMismatchAlert(ctx, env)
	case eventTypeWaitlistEntryExpired:
		// Audit-only lifecycle event: paced sends are single-shot (no
		// pending future send for an entry exists), so an expiry has
		// nothing to stop. Explicitly acknowledged, never retried.
		return nil
	}

	id := env.bookingID()
	if id == "" {
		return nil // events without a booking reference need no signals
	}

	switch env.Type {
	case "com.opendesk.booking.BookingCancelled":
		sigErr := b.signalAll(ctx,
			[]string{"pack-" + id, "reminder-" + id},
			workflows.SignalBookingEvent, workflows.BookingEventSignal{Type: "cancelled"})
		// SPEC-W3 §3 innovation 7: every cancellation is also a backfill
		// opportunity for the offering's waitlist.
		startErr := b.startBackfill(ctx, env)
		return errors.Join(sigErr, startErr)
	case "com.opendesk.booking.BookingNoShow":
		return b.signalAll(ctx,
			[]string{"pack-" + id},
			workflows.SignalNoShow, nil)
	default:
		return nil // other event types need no workflow signals
	}
}

// startBackfill starts one WaitlistBackfillWorkflow per cancelled booking.
// Redelivered events hit the WorkflowExecutionAlreadyStarted case and are
// acknowledged (the workflow ID is derived from the booking ID).
func (b *Bridge) startBackfill(ctx context.Context, env eventEnvelope) error {
	if b.starter == nil {
		return nil
	}
	offeringID, _ := env.Data["offering_id"].(string)
	if offeringID == "" {
		return nil // cannot query the waitlist without the offering
	}
	in := workflows.WaitlistBackfillInput{
		BookingID:  env.bookingID(),
		TenantID:   env.TenantID,
		TenantSlug: env.Subject,
		OfferingID: offeringID,
	}
	_, err := b.starter.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "waitlist-backfill-" + env.bookingID(),
		TaskQueue: b.taskQueue,
	}, "WaitlistBackfillWorkflow", in)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) || strings.Contains(err.Error(), "already started") {
			b.log.Info("WaitlistBackfillWorkflow already running; event acknowledged",
				zap.String("booking_id", env.bookingID()))
			return nil
		}
		return fmt.Errorf("start WaitlistBackfillWorkflow: %w", err)
	}
	b.log.Info("WaitlistBackfillWorkflow started",
		zap.String("booking_id", env.bookingID()), zap.String("offering_id", offeringID))
	return nil
}

// SPEC-W48 (A2/G): event types emitted by booking-service's staff waitlist
// routes (POST /v1/waitlist/{id}/notify|expire) and the auto-expiry sweep.
const (
	eventTypeWaitlistEntryNotified = "com.opendesk.booking.WaitlistEntryNotified"
	eventTypeWaitlistEntryExpired  = "com.opendesk.booking.WaitlistEntryExpired"

	// SPEC-W50-B: booking-service emits BookingPaymentConfirmed after a
	// capture transitions a booking to paid. Both the CloudEvents
	// type-prefix form and the bare form are accepted (mirroring
	// eventmail's identity/billing tolerance).
	eventTypeBookingPaymentConfirmed     = "com.opendesk.booking.BookingPaymentConfirmed"
	eventTypeBookingPaymentConfirmedBare = "BookingPaymentConfirmed"

	// SPEC-W51-A: booking-service emits BookingPaymentAmountMismatch when
	// a capture's amount differs from the offering price the booking
	// expected. Both the CloudEvents type-prefix form and the bare form
	// are accepted (mirroring the BookingPaymentConfirmed tolerance).
	eventTypeBookingPaymentAmountMismatch     = "com.opendesk.booking.BookingPaymentAmountMismatch"
	eventTypeBookingPaymentAmountMismatchBare = "BookingPaymentAmountMismatch"
)

// startWaitlistNotify starts one WaitlistNotifyWorkflow per
// WaitlistEntryNotified event: the staff-chosen entry is delivered via the
// same paced waitlist-claim send path the backfill uses
// (PacedSendWaitlistClaim → SendWaitlistClaimNotification). The payload is
// the drop-in PacedWaitlistSend shape booking-service emits
// (data.input + data.entry with claim_token/contact_phone).
//
// Fail-closed: an event without a resolvable claim (entry id + claim_token)
// or a reachable customer channel (contact_phone) is acknowledged WITHOUT
// sending — a malformed event must never produce a customer-facing message.
// Redelivered events dedupe on the deterministic workflow ID
// (waitlist-notify-<entry_id>, WorkflowExecutionAlreadyStarted → ack), so a
// Kafka redelivery can never double-send.
func (b *Bridge) startWaitlistNotify(ctx context.Context, env eventEnvelope) error {
	if b.starter == nil {
		return nil
	}
	var data struct {
		Input workflows.WaitlistBackfillInput `json:"input"`
		Entry workflows.WaitlistEntry         `json:"entry"`
	}
	raw, err := json.Marshal(env.Data)
	if err != nil {
		return fmt.Errorf("marshal WaitlistEntryNotified data: %w", err)
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		b.log.Warn("malformed WaitlistEntryNotified payload; acknowledged without send", zap.Error(err))
		return nil
	}
	if data.Entry.ID == "" || data.Entry.ClaimToken == "" || data.Entry.ContactPhone == "" {
		b.log.Warn("WaitlistEntryNotified missing entry id/claim_token/contact_phone; acknowledged without send",
			zap.String("entry_id", data.Entry.ID))
		return nil
	}
	// Envelope defaults when the payload omits addressing fields.
	if data.Input.TenantID == "" {
		data.Input.TenantID = env.TenantID
	}
	if data.Input.TenantSlug == "" {
		data.Input.TenantSlug = env.Subject
	}
	if data.Input.OfferingID == "" {
		data.Input.OfferingID = data.Entry.OfferingID
	}
	send := workflows.PacedWaitlistSend{Input: data.Input, Entry: data.Entry}
	_, err = b.starter.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "waitlist-notify-" + data.Entry.ID,
		TaskQueue: b.taskQueue,
	}, "WaitlistNotifyWorkflow", send)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) || strings.Contains(err.Error(), "already started") {
			b.log.Info("WaitlistNotifyWorkflow already running; event acknowledged (no double-send)",
				zap.String("entry_id", data.Entry.ID))
			return nil
		}
		return fmt.Errorf("start WaitlistNotifyWorkflow: %w", err)
	}
	b.log.Info("WaitlistNotifyWorkflow started",
		zap.String("entry_id", data.Entry.ID), zap.String("offering_id", data.Input.OfferingID))
	return nil
}

// startPaymentReceipt starts one PaymentReceiptWorkflow per
// BookingPaymentConfirmed event (SPEC-W50-B). The payload is the sacred W50
// cross-scope contract: booking_id, tenant_id, tx_ref, amount_minor
// (int64), currency, customer_name, contact_phone, contact_email,
// booking_ref, paid_at.
//
// Fail-closed: an event without booking_id/tx_ref, or with BOTH
// contact_phone and contact_email empty, is acknowledged WITHOUT starting a
// workflow — no send is possible, and a malformed event must never produce
// a customer-facing message. Redelivered events dedupe on the deterministic
// workflow ID (payment-receipt-<tx_ref>, WorkflowExecutionAlreadyStarted →
// ack), so a Kafka redelivery can never double-send a receipt.
func (b *Bridge) startPaymentReceipt(ctx context.Context, env eventEnvelope) error {
	if b.starter == nil {
		return nil
	}
	var send workflows.PacedPaymentReceiptSend
	raw, err := json.Marshal(env.Data)
	if err != nil {
		return fmt.Errorf("marshal BookingPaymentConfirmed data: %w", err)
	}
	if err := json.Unmarshal(raw, &send); err != nil {
		b.log.Warn("malformed BookingPaymentConfirmed payload; acknowledged without send", zap.Error(err))
		return nil
	}
	if send.BookingID == "" || send.TxRef == "" {
		b.log.Warn("BookingPaymentConfirmed missing booking_id/tx_ref; acknowledged without send",
			zap.String("booking_id", send.BookingID), zap.String("tx_ref", send.TxRef))
		return nil
	}
	if send.ContactPhone == "" && send.ContactEmail == "" {
		b.log.Warn("BookingPaymentConfirmed has no reachable contact (phone and email empty); acknowledged without send",
			zap.String("booking_id", send.BookingID), zap.String("tx_ref", send.TxRef))
		return nil
	}
	// Envelope defaults when the payload omits addressing fields.
	if send.TenantID == "" {
		send.TenantID = env.TenantID
	}
	send.TenantSlug = env.Subject
	_, err = b.starter.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "payment-receipt-" + send.TxRef,
		TaskQueue: b.taskQueue,
	}, "PaymentReceiptWorkflow", send)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) || strings.Contains(err.Error(), "already started") {
			b.log.Info("PaymentReceiptWorkflow already running; event acknowledged (no double-send)",
				zap.String("tx_ref", send.TxRef))
			return nil
		}
		return fmt.Errorf("start PaymentReceiptWorkflow: %w", err)
	}
	b.log.Info("PaymentReceiptWorkflow started",
		zap.String("booking_id", send.BookingID), zap.String("tx_ref", send.TxRef))
	return nil
}

// startPaymentMismatchAlert starts one PaymentMismatchAlertWorkflow per
// BookingPaymentAmountMismatch event (SPEC-W51-A). The payload is the
// sacred W51 cross-scope contract: booking_id, tenant_id, tx_ref,
// amount_minor (int64, captured), expected_amount_minor (int64, the
// offering price the booking expected — the W51 contract delta), currency,
// booking_ref (human public ref, BK-XXXXXXXX), paid_at.
//
// GUARD: the alert is OPS/STAFF-only — unlike the payment receipt there is
// NO reachable-contact requirement (the payload carries no customer contact
// fields and the send path never touches a customer-facing binding).
//
// Fail-closed: an event without booking_id/tx_ref is acknowledged WITHOUT
// starting a workflow — a malformed event must never produce an alert with
// no reconciliation anchors. Redelivered events dedupe on the deterministic
// workflow ID (payment-mismatch-<tx_ref>, WorkflowExecutionAlreadyStarted →
// ack), so a Kafka redelivery can never double-alert.
func (b *Bridge) startPaymentMismatchAlert(ctx context.Context, env eventEnvelope) error {
	if b.starter == nil {
		return nil
	}
	var send workflows.PacedPaymentMismatchSend
	raw, err := json.Marshal(env.Data)
	if err != nil {
		return fmt.Errorf("marshal BookingPaymentAmountMismatch data: %w", err)
	}
	if err := json.Unmarshal(raw, &send); err != nil {
		b.log.Warn("malformed BookingPaymentAmountMismatch payload; acknowledged without alert", zap.Error(err))
		return nil
	}
	if send.BookingID == "" || send.TxRef == "" {
		b.log.Warn("BookingPaymentAmountMismatch missing booking_id/tx_ref; acknowledged without alert",
			zap.String("booking_id", send.BookingID), zap.String("tx_ref", send.TxRef))
		return nil
	}
	// Envelope defaults when the payload omits addressing fields.
	if send.TenantID == "" {
		send.TenantID = env.TenantID
	}
	send.TenantSlug = env.Subject
	_, err = b.starter.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "payment-mismatch-" + send.TxRef,
		TaskQueue: b.taskQueue,
	}, "PaymentMismatchAlertWorkflow", send)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) || strings.Contains(err.Error(), "already started") {
			b.log.Info("PaymentMismatchAlertWorkflow already running; event acknowledged (no double-alert)",
				zap.String("tx_ref", send.TxRef))
			return nil
		}
		return fmt.Errorf("start PaymentMismatchAlertWorkflow: %w", err)
	}
	b.log.Info("PaymentMismatchAlertWorkflow started",
		zap.String("booking_id", send.BookingID), zap.String("tx_ref", send.TxRef),
		zap.Int64("expected_amount_minor", send.ExpectedAmountMinor),
		zap.Int64("captured_amount_minor", send.AmountMinor))
	return nil
}

// signalAll delivers one signal to each target workflow. A workflow that is
// not running (NotFound) is logged and skipped — it is not an error and the
// message must not be retried or dead-lettered.
func (b *Bridge) signalAll(ctx context.Context, workflowIDs []string, signal string, payload any) error {
	var errs []error
	for _, id := range workflowIDs {
		err := b.temporal.SignalWorkflow(ctx, id, "", signal, payload)
		if err == nil {
			b.log.Info("workflow signalled",
				zap.String("workflow_id", id), zap.String("signal", signal))
			continue
		}
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) || strings.Contains(err.Error(), "workflow not found") {
			b.log.Info("workflow not running; signal skipped",
				zap.String("workflow_id", id), zap.String("signal", signal))
			continue
		}
		errs = append(errs, fmt.Errorf("signal %s to %s: %w", signal, id, err))
	}
	return errors.Join(errs...)
}
