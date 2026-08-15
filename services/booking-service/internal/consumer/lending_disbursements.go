// Lending disbursement-intent consumer (SPEC-W24 Agent B, WS-B2): consumes
// com.opendesk.lending.DisbursementIntent CloudEvents from the lending
// events topic (LENDING_EVENTS_TOPIC, default opendesk.lending.events.v1)
// and creates the corresponding TigerBeetle transfer through the
// disbursement rail seam below — the integration point the lending package
// explicitly defers to (internal/lending/events.go: "the rail's consumer
// subscribes to this intent, performs the actual payout and owns
// settlement/reconciliation"). Connects to the broker directly via
// segmentio/kafka-go like the command/privacy consumers; poison messages
// dead-letter to opendesk.dlq.
//
// Rail posture (W39 mock-posture contract): there is no Go TigerBeetle
// client in this repo — payments-service owns the TB ledger. This
// consumer fails closed by default: LENDING_TB_BRIDGE_URL selects the
// live HTTP bridge (the payments-service Dapr invoke gateway — the
// deployment convention documented for PAYOUT_PROVIDER_BASE_URL in
// internal/referrals/payouts.go); the deterministic MockRail (no money
// movement) is only available under the explicit ALLOW_MOCK_RAILS=1 dev
// opt-in; with neither configured, RailFromEnv returns
// ErrRailNotConfigured and intents dead-letter instead of being silently
// simulated.
//
// Exactly-once under redelivery: the transfer ID is derived
// deterministically from the intent's ref_id (the rail-side idempotency
// anchor = application id, set by lending.MarshalDisbursementIntent),
// falling back to the CloudEvent ID — the same convention as
// referrals.PayoutReference. TigerBeetle creates are idempotent by
// transfer ID and the MockRail dedupes on it, so a redelivered intent
// never creates a second transfer.
package consumer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/lending"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// DefaultLendingDisbursementsGroup is the consumer group when
// LENDING_DISBURSEMENTS_GROUP is unset (main.go wiring).
const DefaultLendingDisbursementsGroup = "booking-lending-disbursements"

// EnvLendingTBBridgeURL selects the live rail: non-empty → HTTPRail
// posting {base}/transfers (point it at the payments-service Dapr invoke
// gateway, e.g. http://daprd-booking:3500/v1.0/invoke/payments/method).
// Empty → the MockRail is only available under the explicit
// ALLOW_MOCK_RAILS=1 dev opt-in (lending.MockRailsAllowed); otherwise
// RailFromEnv fails closed with ErrRailNotConfigured (W39 SIM-001).
const EnvLendingTBBridgeURL = "LENDING_TB_BRIDGE_URL"

// ErrRailNotConfigured is the explicit fail-closed error when neither the
// live rail (LENDING_TB_BRIDGE_URL) nor the mock opt-in
// (ALLOW_MOCK_RAILS=1) is configured. A disbursement intent must NEVER be
// silently simulated in the default posture.
var ErrRailNotConfigured = errors.New("lending disbursement rail not configured: set LENDING_TB_BRIDGE_URL for the live TigerBeetle bridge, or ALLOW_MOCK_RAILS=1 to opt into the dev simulation")

// disbursementIntentData mirrors the data payload of
// lending.MarshalDisbursementIntent (duplicated per service-boundary
// rules — the consumer must not depend on the producer's map shape).
type disbursementIntentData struct {
	TenantID      string `json:"tenant_id"`
	Intent        string `json:"intent"`
	ApplicationID string `json:"application_id"`
	LoanID        string `json:"loan_id"`
	ContactID     string `json:"contact_id"`
	AmountKobo    int64  `json:"amount_kobo"`
	Currency      string `json:"currency"`
	RefID         string `json:"ref_id"` // rail-side idempotency anchor (= application id)
}

// intentEnvelope is the CloudEvents envelope of lending events.
type intentEnvelope struct {
	ID   string                 `json:"id"`
	Type string                 `json:"type"`
	Data disbursementIntentData `json:"data"`
}

// RailTransfer is one TigerBeetle-shaped disbursement transfer: the house
// flow account (501) is debited and the borrower's loan-principal account
// (500) is credited — the same balanced-pair direction as lending's
// mirrored Postgres ledger ("disbursement = debit 501 / credit 500").
type RailTransfer struct {
	TransferID        string `json:"transfer_id"` // deterministic idempotency key
	TenantID          string `json:"tenant_id"`
	LoanID            string `json:"loan_id"`
	ApplicationID     string `json:"application_id"`
	ContactID         string `json:"contact_id"`
	AmountKobo        int64  `json:"amount_kobo"`
	Currency          string `json:"currency"`
	DebitAccountCode  int    `json:"debit_account_code"`  // lending.AccountRepaymentReceived (501, house)
	CreditAccountCode int    `json:"credit_account_code"` // lending.AccountPrincipalDisbursed (500, borrower)
}

// DisbursementRail abstracts the TigerBeetle bridge so the consumer stays
// mockable (mirrors referrals.PayoutProvider).
type DisbursementRail interface {
	Name() string // mock | http
	// CreateTransfer creates the disbursement transfer. Implementations
	// MUST be idempotent on RailTransfer.TransferID — a duplicate create
	// is a success replay, never a second transfer.
	CreateTransfer(ctx context.Context, in RailTransfer) (railRef string, err error)
}

// DisbursementTransferID derives the deterministic transfer ID /
// idempotency key for one intent (sha256 of the anchor, prefixed — the
// referrals.PayoutReference convention). anchor is the intent's ref_id, or
// the CloudEvent ID when ref_id is empty.
func DisbursementTransferID(anchor string) string {
	sum := sha256.Sum256([]byte("lending-disbursement|" + anchor))
	return "ldisb_" + hex.EncodeToString(sum[:])[:24]
}

// ---------------------------------------------------------------------------
// MockRail — DEV-ONLY simulation (ALLOW_MOCK_RAILS=1 opt-in since W39;
// previously the silent default): deterministic, no network, no money
// movement. The intent is logged and acknowledged so lending is not
// blocked while TB is unconfigured — but ONLY when the operator
// explicitly opted into the simulation. In-process dedupe on the
// transfer ID gives exactly-once under redelivery; cross-process
// exactly-once is the live rail's job (TB transfer IDs are idempotent).
// ---------------------------------------------------------------------------

// MockRail is the disabled/mock DisbursementRail.
type MockRail struct {
	mu   sync.Mutex
	seen map[string]bool
	log  *zap.Logger
}

// NewMockRail builds the mock rail.
func NewMockRail(log *zap.Logger) *MockRail {
	return &MockRail{seen: map[string]bool{}, log: log}
}

// Name implements DisbursementRail.
func (*MockRail) Name() string { return "mock" }

// CreateTransfer implements DisbursementRail: the first delivery records
// the transfer; redeliveries of the same transfer ID are no-op replays.
func (m *MockRail) CreateTransfer(_ context.Context, in RailTransfer) (string, error) {
	m.mu.Lock()
	dup := m.seen[in.TransferID]
	if !dup {
		m.seen[in.TransferID] = true
	}
	m.mu.Unlock()
	if dup {
		m.log.Info("lending disbursement transfer replayed (mock rail, idempotent no-op)",
			zap.String("transfer_id", in.TransferID), zap.String("loan_id", in.LoanID))
		return in.TransferID, nil
	}
	m.log.Info("lending disbursement transfer recorded (mock rail — TB unconfigured, no money moved)",
		zap.String("transfer_id", in.TransferID), zap.String("loan_id", in.LoanID),
		zap.Int64("amount_kobo", in.AmountKobo), zap.String("currency", in.Currency),
		zap.Int("debit_account_code", in.DebitAccountCode), zap.Int("credit_account_code", in.CreditAccountCode))
	return in.TransferID, nil
}

// Transfers returns the deduped transfer IDs recorded so far (test hook).
func (m *MockRail) Transfers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.seen))
	for id := range m.seen {
		out = append(out, id)
	}
	return out
}

// ---------------------------------------------------------------------------
// HTTPRail — the live bridge: POST {BaseURL}/transfers with the RailTransfer
// body, pointed at the payments-service Dapr invoke gateway (the
// PAYOUT_PROVIDER_BASE_URL deployment convention of
// internal/referrals/payouts.go). Payments-service owns the TigerBeetle
// ledger; transfer IDs are idempotent there.
// ---------------------------------------------------------------------------

// HTTPRail posts transfers to the payments-service bridge endpoint.
type HTTPRail struct {
	BaseURL string
	hc      *http.Client
}

// NewHTTPRail builds the live rail against baseURL (trailing slash trimmed).
func NewHTTPRail(baseURL string) *HTTPRail {
	return &HTTPRail{BaseURL: strings.TrimRight(baseURL, "/"), hc: &http.Client{Timeout: 20 * time.Second}}
}

// Name implements DisbursementRail.
func (HTTPRail) Name() string { return "http" }

// CreateTransfer implements DisbursementRail.
func (r *HTTPRail) CreateTransfer(ctx context.Context, in RailTransfer) (string, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.BaseURL+"/transfers", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", in.TransferID)
	resp, err := r.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("rail transfer %s: %w", in.TransferID, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("rail transfer %s: bridge status %d", in.TransferID, resp.StatusCode)
	}
	return in.TransferID, nil
}

// failClosedRail is the defensive rail when none was wired: every
// transfer fails explicitly with ErrRailNotConfigured — never a silent
// simulation, never a success claim.
type failClosedRail struct{}

// Name implements DisbursementRail.
func (failClosedRail) Name() string { return "unconfigured" }

// CreateTransfer implements DisbursementRail (fail-closed).
func (failClosedRail) CreateTransfer(context.Context, RailTransfer) (string, error) {
	return "", ErrRailNotConfigured
}

// RailFromEnv builds the DisbursementRail from the environment:
// LENDING_TB_BRIDGE_URL non-empty → HTTPRail (live); else
// ALLOW_MOCK_RAILS=1 → MockRail (dev simulation); else FAIL CLOSED with
// ErrRailNotConfigured (W39 SIM-001 — the mock posture is opt-in only).
func RailFromEnv(log *zap.Logger) (DisbursementRail, error) {
	if base := os.Getenv(EnvLendingTBBridgeURL); base != "" {
		return NewHTTPRail(base), nil
	}
	if lending.MockRailsAllowed() {
		return NewMockRail(log), nil
	}
	return nil, ErrRailNotConfigured
}

// ---------------------------------------------------------------------------
// Consumer
// ---------------------------------------------------------------------------

// LendingDisbursementsConsumer turns disbursement intents into rail
// transfers.
type LendingDisbursementsConsumer struct {
	reader *kafka.Reader
	dlq    *kafka.Writer
	rail   DisbursementRail
	log    *zap.Logger
}

// NewLendingDisbursements builds the lending disbursement-intent consumer.
// A nil rail selects the FAIL-CLOSED posture (defensive: every intent
// errors with ErrRailNotConfigured and dead-letters; wiring uses
// RailFromEnv).
func NewLendingDisbursements(brokers []string, topic, group, dlqTopic string, rail DisbursementRail, log *zap.Logger) *LendingDisbursementsConsumer {
	if rail == nil {
		rail = failClosedRail{}
	}
	return &LendingDisbursementsConsumer{
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
		rail: rail,
		log:  log,
	}
}

// Run consumes until ctx is cancelled.
func (c *LendingDisbursementsConsumer) Run(ctx context.Context) error {
	c.log.Info("lending disbursement consumer started", zap.String("rail", c.rail.Name()))
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("fetch message: %w", err)
		}
		if err := c.processWithRetry(ctx, msg); err != nil {
			c.log.Error("lending disbursement intent dead-lettered",
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
func (c *LendingDisbursementsConsumer) Close() error {
	return errors.Join(c.reader.Close(), c.dlq.Close())
}

var errPermanentIntent = errors.New("permanent disbursement intent error")

func permanentIntent(err error) error { return fmt.Errorf("%w: %v", errPermanentIntent, err) }

func (c *LendingDisbursementsConsumer) processWithRetry(ctx context.Context, msg kafka.Message) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := c.process(ctx, msg); err != nil {
			lastErr = err
			if errors.Is(err, errPermanentIntent) {
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

// process handles one lending event. Non-intent event types (the topic
// also carries ApplicationDecided / LoanDisbursed / LoanRepaid) are
// acknowledged and skipped — the same posture as the privacy consumer's
// unknown-type handling.
func (c *LendingDisbursementsConsumer) process(ctx context.Context, msg kafka.Message) error {
	var env intentEnvelope
	if err := json.Unmarshal(msg.Value, &env); err != nil {
		return permanentIntent(fmt.Errorf("malformed lending event: %v", err))
	}
	if env.Type != lending.EventTypeDisbursementIntent {
		return nil
	}
	d := env.Data
	if _, err := uuid.Parse(d.TenantID); err != nil {
		return permanentIntent(fmt.Errorf("bad tenant_id %q", d.TenantID))
	}
	if _, err := uuid.Parse(d.LoanID); err != nil {
		return permanentIntent(fmt.Errorf("bad loan_id %q", d.LoanID))
	}
	if d.AmountKobo <= 0 {
		return permanentIntent(fmt.Errorf("non-positive amount_kobo %d", d.AmountKobo))
	}
	// Idempotency anchor: ref_id (= application id) is the rail-side anchor
	// set by the producer; fall back to the CloudEvent ID (the command
	// consumer's natural-dedup convention).
	anchor := d.RefID
	if anchor == "" {
		anchor = env.ID
	}
	if anchor == "" {
		return permanentIntent(errors.New("intent carries neither ref_id nor event id"))
	}
	transfer := RailTransfer{
		TransferID:        DisbursementTransferID(anchor),
		TenantID:          d.TenantID,
		LoanID:            d.LoanID,
		ApplicationID:     d.ApplicationID,
		ContactID:         d.ContactID,
		AmountKobo:        d.AmountKobo,
		Currency:          d.Currency,
		DebitAccountCode:  lending.AccountRepaymentReceived,  // 501 house side
		CreditAccountCode: lending.AccountPrincipalDisbursed, // 500 borrower principal
	}
	ref, err := c.rail.CreateTransfer(ctx, transfer)
	if err != nil {
		return fmt.Errorf("create disbursement transfer: %w", err)
	}
	c.log.Info("lending disbursement transfer created",
		zap.String("event_id", env.ID), zap.String("transfer_id", transfer.TransferID),
		zap.String("rail", c.rail.Name()), zap.String("rail_ref", ref),
		zap.String("loan_id", d.LoanID), zap.Int64("amount_kobo", d.AmountKobo))
	return nil
}

func (c *LendingDisbursementsConsumer) deadLetter(ctx context.Context, msg kafka.Message, cause error) error {
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
