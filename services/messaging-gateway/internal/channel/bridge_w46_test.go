package channel

// SPEC-W46 PERF-18 regression tests: heap-ordered dedupe expiry (no O(n)
// sweep), TTL semantics preserved, and the bounded map evicts oldest-first.

import (
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
)

func newDedupeBridge(now *time.Time) *Bridge {
	b := NewBridge(map[string]Site{}, "http://conv", "http://voice", nil, nil, zap.NewNop())
	b.SetClock(func() time.Time { return *now })
	return b
}

func TestBridgeDedupeTTLExpiry(t *testing.T) {
	now := time.Now()
	b := newDedupeBridge(&now)
	msg := InboundMessage{Channel: "whatsapp", MessageID: "m1"}
	if b.alreadyDone(msg) {
		t.Fatal("unknown message must not dedupe")
	}
	b.markDone(msg)
	if !b.alreadyDone(msg) {
		t.Fatal("completed message must dedupe inside the window")
	}
	now = now.Add(bridgeDedupeTTL + time.Second) // past the window
	if b.alreadyDone(msg) {
		t.Fatal("expired completion must not dedupe")
	}
	if len(b.done) != 0 || len(b.doneExp) != 0 {
		t.Fatalf("expired entries must be swept: map=%d heap=%d", len(b.done), len(b.doneExp))
	}
}

func TestBridgeDedupeHeapExpiryOrder(t *testing.T) {
	now := time.Now()
	b := newDedupeBridge(&now)
	// Interleave completions; advance the clock so only the oldest expire.
	for i := 0; i < 10; i++ {
		b.markDone(InboundMessage{Channel: "whatsapp", MessageID: fmt.Sprintf("m%d", i)})
		now = now.Add(time.Hour)
	}
	// now = start + TTL + 5h; m_i completed at start+i*h is expired iff i < 5.
	now = now.Add(bridgeDedupeTTL - 5*time.Hour)
	b.sweepForTest()
	if len(b.done) != 5 {
		t.Fatalf("expected 5 live completions, got %d", len(b.done))
	}
	if len(b.doneExp) != 5 {
		t.Fatalf("heap must shrink with the map, got %d", len(b.doneExp))
	}
	if b.alreadyDone(InboundMessage{Channel: "whatsapp", MessageID: "m4"}) {
		t.Fatal("m4 must have expired")
	}
	if !b.alreadyDone(InboundMessage{Channel: "whatsapp", MessageID: "m5"}) {
		t.Fatal("m5 must still dedupe")
	}
	if !b.alreadyDone(InboundMessage{Channel: "whatsapp", MessageID: "m9"}) {
		t.Fatal("m9 must still dedupe")
	}
}

// sweepForTest forces an expiry sweep at the current injected time.
func (b *Bridge) sweepForTest() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweepDoneLocked(b.now())
}

func TestBridgeDedupeBoundedMap(t *testing.T) {
	now := time.Now()
	b := newDedupeBridge(&now)
	total := bridgeDedupeMaxEntries + 10
	for i := 0; i < total; i++ {
		b.markDone(InboundMessage{Channel: "telegram", MessageID: fmt.Sprintf("id-%d", i)})
		now = now.Add(time.Microsecond) // strictly ordered completion times
	}
	if len(b.done) != bridgeDedupeMaxEntries {
		t.Fatalf("map must be bounded at %d, got %d", bridgeDedupeMaxEntries, len(b.done))
	}
	if len(b.doneExp) != bridgeDedupeMaxEntries {
		t.Fatalf("heap must track the map, got %d", len(b.doneExp))
	}
	// Oldest-first eviction: the first 10 ids were evicted, the newest live.
	if b.alreadyDone(InboundMessage{Channel: "telegram", MessageID: "id-0"}) {
		t.Fatal("oldest completion must be evicted at capacity")
	}
	if !b.alreadyDone(InboundMessage{Channel: "telegram", MessageID: fmt.Sprintf("id-%d", total-1)}) {
		t.Fatal("newest completion must survive capacity eviction")
	}
}
