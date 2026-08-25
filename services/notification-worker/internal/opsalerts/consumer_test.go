package opsalerts

// SPEC-W44 K3/F15-04 consumer tests: idempotent persistence on the
// CloudEvent id, no-commit-on-error semantics (Process returns the store
// error so Run leaves the offset behind), poison-message tolerance.

import (
	"context"
	"errors"
	"testing"

	"github.com/opendesk/notification-worker/internal/store"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type fakeAlertStore struct {
	inserted []store.OpsAlert
	fail     bool
	seen     map[string]bool
}

func (f *fakeAlertStore) InsertOpsAlert(_ context.Context, a *store.OpsAlert) (bool, error) {
	if f.fail {
		return false, errors.New("db down")
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[a.EventID] {
		return false, nil // redelivery dedupes
	}
	f.seen[a.EventID] = true
	f.inserted = append(f.inserted, *a)
	return true, nil
}

func TestProcessPersistsAndDedupes(t *testing.T) {
	st := &fakeAlertStore{}
	c := New([]string{"127.0.0.1:9092"}, Topic, "g", st, zap.NewNop())
	raw := []byte(`{"id":"evt-9","source":"notification-worker","type":"opendesk.payments.webhook.exhausted","tenantid":"acme-uuid","data":{"severity":"critical"}}`)

	require.NoError(t, c.Process(context.Background(), raw))
	require.Len(t, st.inserted, 1)
	a := st.inserted[0]
	require.Equal(t, "evt-9", a.EventID)
	require.Equal(t, "acme-uuid", a.TenantID)
	require.Equal(t, "critical", a.Severity)
	require.Equal(t, "opendesk.payments.webhook.exhausted", a.Type)
	require.JSONEq(t, string(raw), string(a.Payload))

	// Redelivery of the same CloudEvent id is a no-op (inserted=false) and
	// still commits.
	require.NoError(t, c.Process(context.Background(), raw))
	require.Len(t, st.inserted, 1)
}

func TestProcessNoCommitOnError(t *testing.T) {
	st := &fakeAlertStore{fail: true}
	c := New([]string{"127.0.0.1:9092"}, Topic, "g", st, zap.NewNop())
	err := c.Process(context.Background(),
		[]byte(`{"id":"evt-1","type":"t","data":{}}`))
	require.Error(t, err, "store failure must propagate so the offset is NOT committed")
}

func TestProcessDropsPoisonMessages(t *testing.T) {
	st := &fakeAlertStore{}
	c := New([]string{"127.0.0.1:9092"}, Topic, "g", st, zap.NewNop())
	// Malformed JSON → dropped (nil error so the offset still commits).
	require.NoError(t, c.Process(context.Background(), []byte(`not json`)))
	// Valid JSON without a CloudEvent id → dropped (no idempotency anchor).
	require.NoError(t, c.Process(context.Background(), []byte(`{"type":"t","data":{}}`)))
	require.Empty(t, st.inserted)
}

func TestTopicEnabled(t *testing.T) {
	require.Equal(t, Topic, TopicEnabled(""))
	require.Equal(t, Topic, TopicEnabled(Topic))
	require.Empty(t, TopicEnabled("off"))
	require.Empty(t, TopicEnabled("OFF"))
	require.Equal(t, "custom.topic", TopicEnabled("custom.topic"))
}
