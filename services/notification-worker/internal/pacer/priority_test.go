package pacer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Priority fast-lane (SPEC-W11 Part B §5): with the token bucket EXHAUSTED,
// a non-priority send blocks (context deadline) while a priority send
// dispatches immediately — and is still metered (priority counter).
func TestPriorityBypassesExhaustedBucket(t *testing.T) {
	// CPS ~0 (one token per 10s), burst 1: the first Wait drains the bucket.
	p := New(Config{CPS: 0.1, Burst: 1, Backend: "local", RedisAddr: "redis:6379"}, zap.NewNop())
	ctx := context.Background()
	require.NoError(t, p.Wait(ctx)) // drains the only token

	// Non-priority: blocked by the bucket (would wait ~10s) → wait exceeds
	// the 150ms budget (x/time/rate reports its own deadline error).
	blocked, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	err := p.Wait(blocked)
	require.Error(t, err, "non-priority send must NOT get a token from the exhausted bucket")
	require.Contains(t, err.Error(), "deadline")

	// Priority: immediate, despite the exhausted bucket.
	start := time.Now()
	require.NoError(t, p.Priority(ctx))
	require.Less(t, time.Since(start), 50*time.Millisecond, "priority send must not wait for a token")

	// Metered: 1 bucket grant + 1 priority bypass.
	granted, priority := p.Stats()
	require.Equal(t, uint64(1), granted)
	require.Equal(t, uint64(1), priority)
}

// Priority respects an already-cancelled context.
func TestPriorityCancelledContext(t *testing.T) {
	p := New(testConfig(), zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, p.Priority(ctx), context.Canceled)
	_, priority := p.Stats()
	require.Equal(t, uint64(0), priority, "cancelled priority sends are not metered")
}
