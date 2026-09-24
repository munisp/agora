// cache.go — SPEC-W46 (W46-A item 4, P-GO PERF-06 / P-DATA X-01): in-proc
// positive-decision TTL cache in front of the Permify permissions/check
// HTTP call. Previously EVERY guarded request (all /v1 write + most read
// routes) paid one uncached Permify POST.
//
// Fail-closed is preserved by construction:
//   - only ALLOWED decisions are cached; DENIED decisions are never cached
//     (a revoked-then-rechecked user always re-hits Permify);
//   - backend errors are never cached and propagate to the caller, whose
//     AUTHZ_OUTAGE_POLICY (fail_closed default) decides — unchanged.
//
// Staleness discipline: a cached allow lives at most TTL (30–60s band per
// SPEC-W46 invariant 2; PERMIFY_CACHE_TTL_SECONDS, default 45s). Keyed by
// ALL inputs that vary the result: (tenant, subject, permission, resource).
package permify

import (
	"context"
	"strings"
	"sync"
	"time"
)

// DefaultCacheTTL is the default positive-decision cache lifetime (inside
// the SPEC-mandated 30–60s band).
const DefaultCacheTTL = 45 * time.Second

// cacheMaxEntries bounds the cache map; on overflow the map is cleared
// (entries re-populate lazily — a cleared entry costs one Permify call,
// never a wrong decision).
const cacheMaxEntries = 10000

// CachedAuthorizer wraps an Authorizer with a positive-decision TTL cache.
type CachedAuthorizer struct {
	inner Authorizer
	ttl   time.Duration

	mu  sync.Mutex
	pos map[string]time.Time // key → expiry (allowed decisions only)
}

// NewCachedAuthorizer decorates inner with the positive-decision cache.
// ttl <= 0 falls back to DefaultCacheTTL.
func NewCachedAuthorizer(inner Authorizer, ttl time.Duration) *CachedAuthorizer {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &CachedAuthorizer{inner: inner, ttl: ttl, pos: map[string]time.Time{}}
}

func cacheKey(tenantID, subject, permission, resource string) string {
	var b strings.Builder
	b.Grow(len(tenantID) + len(subject) + len(permission) + len(resource) + 3)
	b.WriteString(tenantID)
	b.WriteByte(0)
	b.WriteString(subject)
	b.WriteByte(0)
	b.WriteString(permission)
	b.WriteByte(0)
	b.WriteString(resource)
	return b.String()
}

// Check implements Authorizer. Cached ALLOWED decisions skip the Permify
// round-trip until their TTL expires; everything else delegates to the
// inner authorizer (denials and errors stay uncached — fail closed).
func (c *CachedAuthorizer) Check(ctx context.Context, tenantID, subject, permission, resource string) (bool, error) {
	key := cacheKey(tenantID, subject, permission, resource)
	now := time.Now()
	c.mu.Lock()
	if exp, ok := c.pos[key]; ok {
		if now.Before(exp) {
			c.mu.Unlock()
			return true, nil
		}
		delete(c.pos, key) // expired
	}
	c.mu.Unlock()

	allowed, err := c.inner.Check(ctx, tenantID, subject, permission, resource)
	if err != nil || !allowed {
		return allowed, err
	}
	c.mu.Lock()
	if len(c.pos) >= cacheMaxEntries {
		// Drop expired entries first; if the map is still full, clear it —
		// bounded memory beats hit ratio, and a miss is always safe.
		for k, exp := range c.pos {
			if now.After(exp) {
				delete(c.pos, k)
			}
		}
		if len(c.pos) >= cacheMaxEntries {
			c.pos = map[string]time.Time{}
		}
	}
	c.pos[key] = now.Add(c.ttl)
	c.mu.Unlock()
	return true, nil
}
