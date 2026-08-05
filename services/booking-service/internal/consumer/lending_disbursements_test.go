package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"github.com/opendesk/booking-service/internal/lending"
	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// marshalIntent builds the CloudEvent the way lending.MarshalDisbursementIntent
// does (same data keys + ref_id anchor).
func marshalIntent(t *testing.T, tenantID, applicationID, loanID, contactID string, amountKobo int64) []byte {
	t.Helper()
	payload, err := json.Marshal(events.New("booking-service", lending.EventTypeDisbursementIntent, "tenant-slug", tenantID, map[string]any{
		"tenant_id":      tenantID,
		"intent":         "loan_disbursement_payout",
		"application_id": applicationID,
		"loan_id":        loanID,
		"contact_id":     contactID,
		"amount_kobo":    amountKobo,
		"currency":       "NGN",
		"ref_id":         applicationID,
	}))
	require.NoError(t, err)
	return payload
}

func newTestConsumer(rail DisbursementRail) *LendingDisbursementsConsumer {
	return &LendingDisbursementsConsumer{rail: rail, log: zap.NewNop()}
}

// recordingRail fails the test if the same transfer ID is created twice
// (simulating TigerBeetle's idempotent-by-ID create) and records calls.
type recordingRail struct {
	mu    sync.Mutex
	ids   []string
	failN int // fail the first failN calls (transient rail outage)
}

func (r *recordingRail) Name() string { return "recording" }

func (r *recordingRail) CreateTransfer(_ context.Context, in RailTransfer) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failN > 0 {
		r.failN--
		return "", errors.New("rail unavailable")
	}
	for _, id := range r.ids {
		if id == in.TransferID {
			return in.TransferID, nil // idempotent replay, like TB
		}
	}
	r.ids = append(r.ids, in.TransferID)
	return in.TransferID, nil
}

func (r *recordingRail) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ids)
}

func TestDisbursementIntentCreatesTransfer(t *testing.T) {
	tenantID, appID, loanID, contactID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	rail := &recordingRail{}
	c := newTestConsumer(rail)

	msg := kafka.Message{Value: marshalIntent(t, tenantID, appID, loanID, contactID, 250_000)}
	require.NoError(t, c.process(context.Background(), msg))
	require.Equal(t, 1, rail.count())
	require.Equal(t, DisbursementTransferID(appID), rail.ids[0])
}

// Redelivery idempotency: the same intent processed twice (Kafka
// redelivery after a commit failure) must create exactly ONE transfer.
func TestDisbursementIntentRedeliveryIsIdempotent(t *testing.T) {
	tenantID, appID, loanID, contactID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()

	t.Run("mock rail (TB unconfigured default)", func(t *testing.T) {
		rail := NewMockRail(zap.NewNop())
		c := newTestConsumer(rail)
		msg := kafka.Message{Value: marshalIntent(t, tenantID, appID, loanID, contactID, 250_000)}
		require.NoError(t, c.process(context.Background(), msg))
		require.NoError(t, c.process(context.Background(), msg)) // redelivery
		require.NoError(t, c.process(context.Background(), msg)) // redelivery
		require.Len(t, rail.Transfers(), 1)
		require.Equal(t, DisbursementTransferID(appID), rail.Transfers()[0])
	})

	t.Run("idempotent live rail", func(t *testing.T) {
		rail := &recordingRail{}
		c := newTestConsumer(rail)
		msg := kafka.Message{Value: marshalIntent(t, tenantID, appID, loanID, contactID, 250_000)}
		require.NoError(t, c.process(context.Background(), msg))
		require.NoError(t, c.process(context.Background(), msg)) // redelivery
		require.Equal(t, 1, rail.count())
	})
}

// Transient rail failure → processWithRetry retries and succeeds with
// still exactly one transfer.
func TestDisbursementIntentRetryAfterRailOutage(t *testing.T) {
	rail := &recordingRail{failN: 1}
	c := newTestConsumer(rail)
	msg := kafka.Message{Value: marshalIntent(t, uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), 10_000)}
	require.NoError(t, c.processWithRetry(context.Background(), msg))
	require.Equal(t, 1, rail.count())
}

// Other lending lifecycle events on the same topic are acked and skipped.
func TestNonIntentEventsAreSkipped(t *testing.T) {
	rail := &recordingRail{}
	c := newTestConsumer(rail)
	for _, typ := range []string{
		lending.EventTypeApplicationDecided,
		lending.EventTypeLoanDisbursed,
		lending.EventTypeLoanRepaid,
	} {
		payload, err := json.Marshal(events.New("booking-service", typ, "slug", uuid.NewString(), map[string]any{}))
		require.NoError(t, err)
		require.NoError(t, c.process(context.Background(), kafka.Message{Value: payload}))
	}
	require.Equal(t, 0, rail.count())
}

func TestMalformedAndInvalidIntentsArePermanent(t *testing.T) {
	rail := &recordingRail{}
	c := newTestConsumer(rail)

	// Malformed JSON.
	err := c.process(context.Background(), kafka.Message{Value: []byte("{not json")})
	require.ErrorIs(t, err, errPermanentIntent)

	// Bad tenant_id.
	bad := marshalIntent(t, "not-a-uuid", uuid.NewString(), uuid.NewString(), uuid.NewString(), 100)
	require.ErrorIs(t, c.process(context.Background(), kafka.Message{Value: bad}), errPermanentIntent)

	// Non-positive amount.
	zero := marshalIntent(t, uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), 0)
	require.ErrorIs(t, c.process(context.Background(), kafka.Message{Value: zero}), errPermanentIntent)

	// Permanent errors must not be retried by processWithRetry.
	require.Error(t, c.processWithRetry(context.Background(), kafka.Message{Value: zero}))
	require.Equal(t, 0, rail.count())
}

// Missing ref_id falls back to the CloudEvent ID as the idempotency anchor.
func TestTransferIDFallsBackToEventID(t *testing.T) {
	payload, err := json.Marshal(events.New("booking-service", lending.EventTypeDisbursementIntent, "slug", uuid.NewString(), map[string]any{
		"tenant_id":   uuid.NewString(),
		"loan_id":     uuid.NewString(),
		"amount_kobo": 5000,
		"currency":    "NGN",
	}))
	require.NoError(t, err)
	var env struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(payload, &env))

	rail := &recordingRail{}
	c := newTestConsumer(rail)
	require.NoError(t, c.process(context.Background(), kafka.Message{Value: payload}))
	require.Equal(t, DisbursementTransferID(env.ID), rail.ids[0])
}

// Mock posture is the default when TB is unconfigured; LENDING_TB_BRIDGE_URL
// selects the live HTTP bridge (payments ledger sim/mock convention).
func TestRailFromEnvPosture(t *testing.T) {
	t.Setenv(EnvLendingTBBridgeURL, "")
	require.Equal(t, "mock", RailFromEnv(zap.NewNop()).Name())
	t.Setenv(EnvLendingTBBridgeURL, "http://daprd:3500/v1.0/invoke/payments/method")
	require.Equal(t, "http", RailFromEnv(zap.NewNop()).Name())
}

// HTTPRail posts the transfer with the idempotency-key header.
func TestHTTPRailPostsTransfer(t *testing.T) {
	var gotKey, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Idempotency-Key")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rail := NewHTTPRail(srv.URL)
	tr := RailTransfer{TransferID: "ldisb_abc", AmountKobo: 100}
	ref, err := rail.CreateTransfer(context.Background(), tr)
	require.NoError(t, err)
	require.Equal(t, "ldisb_abc", ref)
	require.Equal(t, "ldisb_abc", gotKey)
	require.Equal(t, "/transfers", gotPath)
}
