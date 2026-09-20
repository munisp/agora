// Per-key sliding-window rate limiter (SPEC-W45 K14: USSD callback per-phone
// abuse control).
//
// R8-style residual note: this limiter is IN-MEMORY and therefore
// per-replica — with N gateway replicas the effective cap is N×limit and
// windows reset on restart. That is accepted for the USSD callback path:
// the blast radius of excess callbacks is bounded by the conversation-turn
// cost and the aggregator itself retries politely. If exact multi-replica
// limits are ever required, move the windows to a shared store (redis /
// dapr state) — the PhoneRateLimiter interface (Allow) is the seam.
package httpapi

import (
	"sync"
	"time"
)

// phoneRateLimiterMaxKeys bounds the map against unbounded growth: the key
// is the UNVERIFIED phoneNumber form field, so an attacker could otherwise
// mint unlimited keys. Past the cap, stale keys are swept and — when still
// full — new keys are simply rate-limited (fail-closed).
const phoneRateLimiterMaxKeys = 10000

// PhoneRateLimiter is a per-key sliding-window counter.
type PhoneRateLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time // injectable for tests

	mu   sync.Mutex
	hits map[string][]time.Time
}

// NewPhoneRateLimiter allows `limit` events per key per `window`.
func NewPhoneRateLimiter(limit int, window time.Duration) *PhoneRateLimiter {
	if limit <= 0 {
		limit = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	return &PhoneRateLimiter{
		limit:  limit,
		window: window,
		now:    time.Now,
		hits:   map[string][]time.Time{},
	}
}

// SetClock injects the clock (tests).
func (l *PhoneRateLimiter) SetClock(now func() time.Time) { l.now = now }

// Allow records one event for key and reports whether it is within the
// window limit.
func (l *PhoneRateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.window)

	if len(l.hits) >= phoneRateLimiterMaxKeys {
		l.sweep(cutoff)
	}
	kept := l.hits[key][:0]
	for _, at := range l.hits[key] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if len(kept) >= l.limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

// sweep drops keys whose newest hit is outside the window.
func (l *PhoneRateLimiter) sweep(cutoff time.Time) {
	for k, hits := range l.hits {
		if len(hits) == 0 || !hits[len(hits)-1].After(cutoff) {
			delete(l.hits, k)
		}
	}
}
