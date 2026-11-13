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
	"io"
	"net/http"
	"os"
	"strings"
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
	// ocr erases the ocr-service BYTES of media-linked documents after a
	// successful subject anonymize (SPEC-W61 G2). Nil when OCR_SERVICE_URL
	// is unset — the hook is then inert and the ocr TTL sweep remains the
	// byte backstop. This is the consumer-boundary seam: the production
	// value is the real HTTP client (newOCREraserFromEnv); ONLY tests
	// substitute a stub here (no production-path mocks).
	ocr ocrDocumentEraser
}

// ocrDocumentEraser is the per-document erase hop to ocr-service
// (POST /v1/ocr/documents/{id}/erase, SPEC-W61 G2).
type ocrDocumentEraser interface {
	EraseDocument(ctx context.Context, documentID string) error
}

// httpOCREraser is the production ocrDocumentEraser: bounded, NO retries
// (the erase is idempotent server-side — 204 on missing — so the consumer's
// own event redelivery is the retry).
type httpOCREraser struct {
	baseURL string
	token   string
	client  *http.Client
}

// newOCREraserFromEnv wires the erase hop from the EXISTING config envs
// (OCR_SERVICE_URL / OCR_INTERNAL_TOKEN, config.go — the receipt-OCR
// pair). Unset URL disables the hook; URL set but token unset fails CLOSED
// (hook disabled + loud log — the ocr side would 503 anyway).
func newOCREraserFromEnv(log *zap.Logger) ocrDocumentEraser {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("OCR_SERVICE_URL")), "/")
	if base == "" {
		return nil
	}
	token := os.Getenv("OCR_INTERNAL_TOKEN")
	if token == "" {
		log.Error("OCR_SERVICE_URL set but OCR_INTERNAL_TOKEN unset — media erase hook disabled (fail-closed)")
		return nil
	}
	return &httpOCREraser{baseURL: base, token: token, client: &http.Client{Timeout: 30 * time.Second}}
}

// EraseDocument POSTs /v1/ocr/documents/{id}/erase with the internal token.
// 2xx (incl. the idempotent 204) is success; anything else is an error the
// caller logs and moves on from.
func (h *httpOCREraser) EraseDocument(ctx context.Context, documentID string) error {
	if _, err := uuid.Parse(documentID); err != nil {
		return fmt.Errorf("invalid document id %q", documentID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.baseURL+"/v1/ocr/documents/"+documentID+"/erase", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Internal-Token", h.token)
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("ocr erase call: %w", err)
	}
	defer resp.Body.Close()               //nolint:errcheck
	_, _ = io.Copy(io.Discard, resp.Body) // drain for keep-alive reuse
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ocr erase %s: HTTP %d", documentID, resp.StatusCode)
	}
	return nil
}

// mediaPhoneDigits applies the SPEC-W61 G3 pinned canonical form
// (digits-only; a single leading "00" stripped once). Duplicated from the
// gateway's internal/phone package BY CONTRACT (no shared Go module,
// SPEC-W61 non-goal) — keep byte-identical semantics.
func mediaPhoneDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	if strings.HasPrefix(d, "00") {
		d = d[2:]
	}
	return d
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
		ocr:   newOCREraserFromEnv(log),
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
	//
	// SPEC-W61 G2 (media byte erasure): collect the ocr document_ids bound
	// to the subject via media_inbox BEFORE the anonymize commits — the
	// subject tombstone blanks media_inbox.sender_phone_digits in the same
	// tx, so the binding is unreadable AFTERWARDS. The actual ocr erase
	// calls happen only AFTER a successful anonymize (below): the row
	// tombstone is the authoritative erasure, the byte delete its mirror.
	var mediaDocIDs []string
	if c.ocr != nil && env.Data.Phone != "" {
		ids, err := c.store.MediaDocumentIDsForSubject(ctx, tenantID, mediaPhoneDigits(env.Data.Phone))
		if err != nil {
			// Never fail the erasure on a lookup problem — the TTL sweep
			// is the byte backstop (SPEC-W61 G2 posture).
			c.log.Warn("media document lookup failed; continuing erasure",
				zap.String("event_id", env.ID), zap.Error(err))
		} else {
			mediaDocIDs = ids
		}
	}
	sum, err := c.store.AnonymizeSubject(ctx, tenantID, env.Data.Phone, env.Data.Email)
	if err != nil {
		return fmt.Errorf("anonymize subject: %w", err)
	}
	c.log.Info("subject anonymized (GDPR erase)",
		zap.String("event_id", env.ID), zap.String("tenant_id", env.Data.TenantID),
		zap.Int64("contacts", sum.ContactsAnonymized), zap.Any("details", sum.Details))
	// Anonymize succeeded — now erase the media-linked ocr bytes. Erase
	// failures are logged and skipped (log + continue; NEVER fail the
	// erasure on ocr unavailability — the rows are already tombstoned and
	// the TTL sweep is the byte backstop).
	for _, id := range mediaDocIDs {
		if err := c.ocr.EraseDocument(ctx, id); err != nil {
			c.log.Warn("ocr media document erase failed (continuing)",
				zap.String("event_id", env.ID), zap.String("document_id", id), zap.Error(err))
			continue
		}
		c.log.Info("ocr media document erased",
			zap.String("event_id", env.ID), zap.String("document_id", id))
	}
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
