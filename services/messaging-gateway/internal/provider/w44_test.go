package provider

// SPEC-W44 N-06 tests: failover 5-min dedupe window (injectable clock) +
// jittered backoff bounds.

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"go.uber.org/zap"
)

// countingProvider is a minimal SMSProvider for the dedupe tests.
type countingProvider struct {
	calls  int
	status int
}

func (p *countingProvider) Configured() bool { return true }
func (p *countingProvider) SendSMS(context.Context, string, string, string) (int, []byte, error) {
	p.calls++
	return 200, []byte(`{"ok":true}`), nil
}

func TestFailoverDedupeWindow(t *testing.T) {
	p := &countingProvider{}
	now := time.Now()
	clock := &now
	f := NewFailover(map[string]SMSProvider{"africastalking": p}, "africastalking", zap.NewNop())
	f.SetClock(func() time.Time { return *clock })
	ctx := context.Background()

	name, status, _, err := f.SendSMS(ctx, "+1", "hi", "od")
	if err != nil || name != "africastalking" || status != 200 {
		t.Fatalf("first send: %v %q %d", err, name, status)
	}
	if p.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", p.calls)
	}

	// Exact repeat inside the window → recorded result, NO re-send.
	name, status, _, err = f.SendSMS(ctx, "+1", "hi", "od")
	if err != nil || name != "africastalking" {
		t.Fatalf("deduped send: %v %q", err, name)
	}
	if p.calls != 1 {
		t.Fatalf("redelivery must not re-send within 5min, calls = %d", p.calls)
	}

	// A different message is NOT deduped.
	if _, _, _, err = f.SendSMS(ctx, "+1", "different", "od"); err != nil {
		t.Fatal(err)
	}
	if p.calls != 2 {
		t.Fatalf("different message must send, calls = %d", p.calls)
	}

	// Past the window the same message sends again.
	*clock = now.Add(DedupeWindow + time.Second)
	if _, _, _, err = f.SendSMS(ctx, "+1", "hi", "od"); err != nil {
		t.Fatal(err)
	}
	if p.calls != 3 {
		t.Fatalf("after the window the message re-sends, calls = %d", p.calls)
	}
}

func TestBackoffDelayJitterBounds(t *testing.T) {
	rnd := rand.New(rand.NewSource(42))
	wantBase := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, time.Second, time.Second}
	for attempt, base := range wantBase {
		a := attempt + 1
		for i := 0; i < 200; i++ {
			d := backoffDelay(a, rnd)
			lo, hi := base*3/4, base*5/4
			if d < lo || d >= hi {
				t.Fatalf("attempt %d: %v outside [%v,%v)", a, d, lo, hi)
			}
		}
	}
	// Jitter actually varies.
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[backoffDelay(1, rnd)] = true
	}
	if len(seen) < 2 {
		t.Fatal("backoff must jitter (not a constant)")
	}
}
