// Erasure outbox relay (SPEC-W43 I-04 / SPEC-W44 W-I-1+K4).
//
// The consent erasure path used to publish ErasureRequested best-effort
// (F15-06: tombstone durable, event losable; and nothing reached
// opendesk.privacy.events, so booking/conversation never anonymized).
// Now Erase writes a consent_events_outbox row in the tombstone transaction
// and this Relay publishes from it until acknowledged:
//
//   - com.opendesk.consent.ErasureRequested on the consent erasure topic
//     (unchanged contract), AND
//   - PrivacyEraseRequested on opendesk.privacy.events with the EXACT
//     CloudEvent shape the booking consumer
//     (booking-service/internal/consumer/privacy.go) and the conversation
//     consumer (conversation-service/app/privacy.py) expect:
//     {specversion, id, source, type, subject, time, tenantid,
//     data: {phone, email, tenant_id}} — identical to notification-worker's
//     GdprPublishEraseTombstone.
package consent

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/identity-service/internal/events"
	"go.uber.org/zap"
)

// Relay publishes durable erasure outbox rows to Kafka (via Dapr pubsub).
type Relay struct {
	Repo Repository
	// Events publishes CloudEvents (daprc.Client satisfies it; tests
	// substitute a fake).
	Events EventPublisher
	PubSub string
	// ConsentTopic is the consent erasure topic (CONSENT_ERASURE_TOPIC,
	// default opendesk.consent.erasure.v1).
	ConsentTopic string
	// PrivacyTopic is the K4 privacy topic (PRIVACY_EVENTS_TOPIC, default
	// opendesk.privacy.events).
	PrivacyTopic string
	Logger       *zap.Logger
}

// Run sweeps the outbox every interval until ctx is cancelled. Publish
// failures are logged and retried on the next tick — never fatal.
func (r *Relay) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := r.Sweep(ctx); err != nil {
				r.Logger.Warn("consent outbox sweep failed", zap.Error(err))
			}
		}
	}
}

// Sweep publishes every unsent outbox row (bounded batch). A row is marked
// sent only after all of its events are published; partial failure leaves
// the row unsent for the next sweep (consumers dedupe/idempotently
// re-anonymize — the events carry no delivery-once semantics).
func (r *Relay) Sweep(ctx context.Context) (int, error) {
	rows, err := r.Repo.FetchUnsentOutbox(ctx, 100)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, row := range rows {
		if err := r.publishRow(ctx, row); err != nil {
			r.Logger.Error("erasure outbox publish failed — will retry",
				zap.Int64("outbox_id", row.ID),
				zap.String("subject", row.DataSubjectID), zap.Error(err))
			continue
		}
		if err := r.Repo.MarkOutboxSent(ctx, row.ID); err != nil {
			r.Logger.Error("erasure outbox mark-sent failed", zap.Int64("outbox_id", row.ID), zap.Error(err))
			continue
		}
		sent++
	}
	return sent, nil
}

// publishRow publishes the consent ErasureRequested event and — when the
// subject resolves to a phone or email — the K4 PrivacyEraseRequested event.
func (r *Relay) publishRow(ctx context.Context, row OutboxEvent) error {
	tenantID := row.TenantID.String()
	consentEvt := events.New("identity-service", ErasureEventType, row.DataSubjectID, tenantID, map[string]any{
		"tenant_id":       tenantID,
		"data_subject_id": row.DataSubjectID,
		"purpose":         row.Purpose,
		"erased_records":  row.ErasedRecords,
		"erasure_ts":      row.CreatedAt,
		"synthetic":       row.Synthetic, // SPEC-W17 §8.8: downstream anonymizers may fast-path too
	})
	if err := r.Events.PublishEvent(ctx, r.PubSub, r.ConsentTopic, consentEvt); err != nil {
		return err
	}

	phone, email := contactOf(row.DataSubjectID)
	if phone == "" && email == "" {
		// Subjects that are neither phone nor email (e.g. contact uuids)
		// carry no locator the privacy consumers can match on; publishing
		// would DLQ downstream (booking treats empty phone+email as a
		// permanent poison error). Logged, and the consent event above
		// remains the audit-grade record.
		r.Logger.Info("privacy erase event skipped: subject is neither phone nor email",
			zap.String("subject", row.DataSubjectID), zap.String("tenant_id", tenantID))
		return nil
	}
	// Exact shape parity with notification-worker's GdprPublishEraseTombstone
	// (booking privacyEnvelope: id/type/data.phone/data.email/data.tenant_id).
	privacyEvt := map[string]any{
		"specversion": "1.0",
		"id":          uuid.NewString(),
		"source":      "identity-service",
		"type":        PrivacyEraseEventType,
		"subject":     tenantID,
		"time":        time.Now().UTC().Format(time.RFC3339),
		"tenantid":    tenantID,
		"data": map[string]any{
			"phone":     phone,
			"email":     email,
			"tenant_id": tenantID,
		},
	}
	return r.Events.PublishEvent(ctx, r.PubSub, r.PrivacyTopic, privacyEvt)
}

// contactOf maps a data-subject id to the phone/email locator pair the
// privacy consumers match on: E.164 numbers ("+...") become phone, addresses
// containing "@" become email, anything else (contact uuids) neither.
func contactOf(subject string) (phone, email string) {
	if strings.Contains(subject, "@") {
		return "", subject
	}
	if strings.HasPrefix(subject, "+") {
		return subject, ""
	}
	return "", ""
}
