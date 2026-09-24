// USSD menu/config fetch cache (SPEC-W46 PERF-10): a USSD session start
// fetched the tenant pack menu from identity UNCACHED per session (+5-50ms
// on the most latency-sensitive telco path, up to the 5s client timeout on
// identity slowness). Menus are near-static config, so this wrapper caches
// the fetch with a 60s TTL and serves the last-known-good entry on
// upstream error (stale-on-error) — menu mode with slightly-old config
// beats degrading to pass-through text mode on a transient identity blip.
//
// Cache key: the tenant slug, which is the fetcher's ONLY varying input
// (the menu id is an attribute of the tenant pack, so tenant+menu reduces
// to the tenant slug here). A nil menu (pack defines none → pass-through
// text mode) is a valid resolution and is cached too.
//
// Failure semantics preserved: with NO entry at all the error propagates
// exactly as before (the session starts in pass-through text mode); the
// fetch stays best-effort enrichment, never an auth/authorization input.
// The 5s fetch timeout lives on the wrapped HTTPUSSDMenuFetcher and is
// unchanged.
package channel

import (
	"context"
	"sync"
	"time"
)

// USSDMenuCacheTTL is the menu/config cache TTL (SPEC-W46 invariant: hot
// path caches ≤ 300s; menu/config 60s+).
const USSDMenuCacheTTL = 60 * time.Second

// ussdMenuCacheMaxEntries bounds the cache (one entry per tenant slug —
// the tenant count is small, but a bound is mandated for every cache).
const ussdMenuCacheMaxEntries = 1024

type ussdMenuCacheEntry struct {
	menu      []USSDMenuItem
	fetchedAt time.Time
}

// CachedUSSDMenuFetcher wraps a USSDMenuFetcher with the PERF-10 TTL cache
// + stale-on-error fallback. Safe for concurrent use.
type CachedUSSDMenuFetcher struct {
	inner USSDMenuFetcher
	ttl   time.Duration

	mu  sync.Mutex
	m   map[string]ussdMenuCacheEntry
	now func() time.Time // injectable for tests
}

// NewCachedUSSDMenuFetcher wraps inner with the cache; ttl <= 0 selects
// USSDMenuCacheTTL (60s).
func NewCachedUSSDMenuFetcher(inner USSDMenuFetcher, ttl time.Duration) *CachedUSSDMenuFetcher {
	if ttl <= 0 {
		ttl = USSDMenuCacheTTL
	}
	return &CachedUSSDMenuFetcher{
		inner: inner,
		ttl:   ttl,
		m:     map[string]ussdMenuCacheEntry{},
		now:   time.Now,
	}
}

// SetClock injects the cache clock (tests).
func (c *CachedUSSDMenuFetcher) SetClock(now func() time.Time) { c.now = now }

// USSDMenu returns the cached menu when fresh; otherwise refetches. On a
// refetch error a stale entry (any age) is served when one exists; with no
// entry the error propagates (pre-existing behavior: pass-through mode).
func (c *CachedUSSDMenuFetcher) USSDMenu(ctx context.Context, tenantSlug string) ([]USSDMenuItem, error) {
	c.mu.Lock()
	if e, ok := c.m[tenantSlug]; ok && c.now().Sub(e.fetchedAt) < c.ttl {
		menu := e.menu
		c.mu.Unlock()
		return menu, nil
	}
	c.mu.Unlock()

	menu, err := c.inner.USSDMenu(ctx, tenantSlug)
	if err != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if e, ok := c.m[tenantSlug]; ok {
			return e.menu, nil // stale-on-error fallback
		}
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= ussdMenuCacheMaxEntries {
		// Bound: evict the oldest entry (tenant count is tiny; this only
		// guards a pathological slug churn).
		var oldestKey string
		var oldestAt time.Time
		first := true
		for k, e := range c.m {
			if first || e.fetchedAt.Before(oldestAt) {
				oldestKey, oldestAt, first = k, e.fetchedAt, false
			}
		}
		delete(c.m, oldestKey)
	}
	c.m[tenantSlug] = ussdMenuCacheEntry{menu: menu, fetchedAt: c.now()}
	return menu, nil
}
