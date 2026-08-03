package pacer

// SPEC-W16 contract §1: push kind classification.
//
//	push_notification → TRANSACTIONAL (never DND-suppressed, no quiet hours)
//	push_marketing    → MARKETING (DND-suppressed + quiet-hours deferred)

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestClassifyPushKinds(t *testing.T) {
	require.Equal(t, ClassTransactional, ClassifyKind("push_notification"))
	require.Equal(t, ClassMarketing, ClassifyKind("push_marketing"))
	// The incident/priority lane and existing kinds are untouched.
	require.Equal(t, ClassTransactional, ClassifyKind("incident_alert"))
	require.Equal(t, ClassMarketing, ClassifyKind("geo_campaign"))
}

type pushFakeDND struct {
	calls      int
	suppressed bool
	reason     string
}

func (f *pushFakeDND) IsSuppressed(_ context.Context, _, _ string) (bool, string, error) {
	f.calls++
	return f.suppressed, f.reason, nil
}

// push_notification never reaches the registry; push_marketing is checked
// (and suppressed on a hit) exactly like the sms marketing kinds.
func TestPreSendPushClassification(t *testing.T) {
	dnd := &pushFakeDND{suppressed: true, reason: ReasonGlobalDND}
	g := NewGuards(GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())

	dec := g.PreSend(context.Background(), GuardInput{Kind: "push_notification", TenantSlug: "acme", Phone: "+234801"})
	require.False(t, dec.Suppress)
	require.Equal(t, ClassTransactional, dec.Class)
	require.Equal(t, 0, dnd.calls, "transactional push must bypass the DND registry")

	dec = g.PreSend(context.Background(), GuardInput{Kind: "push_marketing", TenantSlug: "acme", Phone: "+234801", Channel: "push"})
	require.True(t, dec.Suppress)
	require.Equal(t, ClassMarketing, dec.Class)
	require.Equal(t, ReasonGlobalDND, dec.Reason)
	require.Equal(t, 1, dnd.calls)
}

// push_marketing participates in quiet hours via the fixed "push" channel
// key (an override for "push" must resolve like any other channel).
func TestPushQuietHoursChannel(t *testing.T) {
	cfg := QuietHoursConfig{
		DefaultWindow: "20:00-08:00",
		Overrides:     map[string]string{"push": "12:00-14:00"},
		Timezone:      "Africa/Lagos",
	}
	// 12:30 Lagos: inside the push override window (but outside the default).
	lagos, err := time.LoadLocation("Africa/Lagos")
	require.NoError(t, err)
	now := time.Date(2026, 3, 2, 12, 30, 0, 0, lagos)

	_, inWindow, err := QuietHoursOpenAt(now, "push", cfg)
	require.NoError(t, err)
	require.True(t, inWindow, "push override window must apply to the push channel")

	_, inWindow, err = QuietHoursOpenAt(now, "sms", cfg)
	require.NoError(t, err)
	require.False(t, inWindow, "default window (20:00-08:00) is not active at 12:30")
}
