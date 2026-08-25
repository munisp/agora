package httpapi

import (
	"sync"
	"sync/atomic"
)

// ConsumerHealth tracks per-consumer liveness for /healthz (SPEC-W43 K-09):
// each supervised background consumer registers a flag at start; when its
// Run loop exits fatally (ctx not cancelled) main clears the flag and
// /healthz starts answering 503 so the orchestrator restarts the pod —
// a process whose Kafka consumer died silently must not keep reporting ok.
type ConsumerHealth struct {
	mu    sync.RWMutex
	flags map[string]*atomic.Bool
}

// NewConsumerHealth builds an empty registry.
func NewConsumerHealth() *ConsumerHealth {
	return &ConsumerHealth{flags: map[string]*atomic.Bool{}}
}

// Register adds a consumer (default healthy) and returns its flag. The
// supervising goroutine Stores false on fatal exit. Re-registering the same
// name returns the existing flag.
func (h *ConsumerHealth) Register(name string) *atomic.Bool {
	h.mu.RLock()
	f, ok := h.flags[name]
	h.mu.RUnlock()
	if ok {
		return f
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if f, ok = h.flags[name]; ok {
		return f
	}
	f = &atomic.Bool{}
	f.Store(true)
	h.flags[name] = f
	return f
}

// Failed returns the names of consumers whose liveness flag is cleared.
func (h *ConsumerHealth) Failed() []string {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []string
	for name, f := range h.flags {
		if !f.Load() {
			out = append(out, name)
		}
	}
	return out
}
