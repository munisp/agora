// Package apikey implements the booking-service half of SPEC-W45 K17:
// tenant API keys for the external read-only bookings surface
// (/v1/ext/bookings, fronted by the APISIX api-ext-booking route).
//
// Callers present X-Api-Key (minted at identity
// POST /v1/tenants/{slug}/api-keys). This middleware validates the key
// against identity's POST {IDENTITY_BASE_URL}/internal/api-keys/validate
// with X-Internal-Token == IDENTITY_INTERNAL_TOKEN (K2 internauth — the
// config is shared with the tenant resolver). The tenant binding is learned
// FROM the key (never from headers): the validated claims stamp the tenant
// into the request context and the store's withTenant sets the
// app.tenant_id GUC from it, so cross-tenant reads are impossible by
// construction.
//
// Fail posture (fail-CLOSED everywhere):
//   - IDENTITY_BASE_URL or IDENTITY_INTERNAL_TOKEN unset → 503 (never
//     silently open);
//   - missing/invalid key (identity 401) → 401;
//   - validator unreachable or answering a non-401 error → 503 (the key
//     validity is UNKNOWN — never guess);
//   - key valid but missing the required scope → 403;
//   - per-key rate limit exceeded → 429.
//
// Positive validations are cached for at most 60s (revocation latency is
// bounded and documented); negative answers are NEVER cached.
package apikey

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ScopeBookingsRead is the v1 scope required by the /v1/ext/bookings group
// (mirrors identity's allowedAPIScopes).
const ScopeBookingsRead = "bookings:read"

// DefaultCacheTTL bounds positive-validation caching (spec: ≤60s).
const DefaultCacheTTL = 60 * time.Second

// DefaultRateLimit / DefaultRateWindow are the per-key request budget
// (60/minute — the APISIX limit-count on the route is the coarser
// per-IP outer guard).
const (
	DefaultRateLimit  = 60
	DefaultRateWindow = time.Minute
)

// validatorTimeout bounds one identity validation call so a hung
// identity-service fails fast instead of stalling the ext read path.
const validatorTimeout = 5 * time.Second

// Claims is the validated key binding stamped into the request context.
type Claims struct {
	KeyID      string    `json:"key_id"`
	Prefix     string    `json:"prefix"`
	TenantID   uuid.UUID `json:"tenant_id"`
	TenantSlug string    `json:"tenant_slug"`
	Scopes     []string  `json:"scopes"`
}

// HasScope reports whether the claims carry scope.
func (c Claims) HasScope(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

type ctxKey struct{}

// ClaimsFrom returns the validated key claims from the request context
// (present only after Validator.Middleware accepted the request).
func ClaimsFrom(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(Claims)
	return c, ok
}

// Validator validates tenant API keys against identity-service, caches
// positive answers, and rate-limits per key.
type Validator struct {
	baseURL       string // IDENTITY_BASE_URL ("" = unconfigured → 503)
	internalToken string // IDENTITY_INTERNAL_TOKEN ("" = unconfigured → 503)
	hc            *http.Client
	log           *zap.Logger

	cacheTTL time.Duration
	mu       sync.Mutex
	cache    map[string]cacheEntry // sha256(key) → positive claims

	rateLimit  int
	rateWindow time.Duration
	buckets    sync.Map // sha256(key) → *bucket
}

type cacheEntry struct {
	claims  Claims
	expires time.Time
}

// bucket is a fixed-window per-key rate counter.
type bucket struct {
	mu        sync.Mutex
	windowEnd time.Time
	count     int
}

// Option customizes New (tests use these to shrink cache/rate budgets).
type Option func(*Validator)

// WithCacheTTL overrides the positive-validation cache TTL (≤60s).
func WithCacheTTL(ttl time.Duration) Option {
	return func(v *Validator) {
		if ttl > 0 && ttl <= DefaultCacheTTL {
			v.cacheTTL = ttl
		}
	}
}

// WithRateLimit overrides the per-key budget (n requests per window).
func WithRateLimit(n int, window time.Duration) Option {
	return func(v *Validator) {
		if n > 0 {
			v.rateLimit = n
		}
		if window > 0 {
			v.rateWindow = window
		}
	}
}

// New builds the validator. baseURL is IDENTITY_BASE_URL, internalToken is
// IDENTITY_INTERNAL_TOKEN (both shared with the tenant resolver config).
// Either empty = UNCONFIGURED: the middleware fails closed with 503.
func New(baseURL, internalToken string, log *zap.Logger, opts ...Option) *Validator {
	if log == nil {
		log = zap.NewNop()
	}
	v := &Validator{
		baseURL:       strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		internalToken: internalToken,
		hc:            &http.Client{Timeout: validatorTimeout},
		log:           log,
		cacheTTL:      DefaultCacheTTL,
		cache:         map[string]cacheEntry{},
		rateLimit:     DefaultRateLimit,
		rateWindow:    DefaultRateWindow,
	}
	for _, o := range opts {
		o(v)
	}
	return v
}

// keyHash is the cache/rate-limit key for a presented key — the raw key is
// never stored in process memory beyond the request lifetime.
func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Configured reports whether the validator has everything it needs to reach
// identity (unconfigured → the middleware answers 503, fail-closed).
func (v *Validator) Configured() bool {
	return v.baseURL != "" && v.internalToken != ""
}

// validateResponse mirrors identity's POST /internal/api-keys/validate 200
// body (duplicated per service-boundary rules).
type validateResponse struct {
	TenantSlug string   `json:"tenant_slug"`
	TenantID   string   `json:"tenant_id"`
	Scopes     []string `json:"scopes"`
	KeyID      string   `json:"key_id"`
	Prefix     string   `json:"prefix"`
}

// errInvalidKey marks identity's 401 (the key is unknown/revoked).
var errInvalidKey = fmt.Errorf("invalid api key")

// validate calls identity (or serves the positive cache). ErrInvalidKey →
// 401; any other error → 503 at the middleware.
func (v *Validator) validate(ctx context.Context, key string) (Claims, error) {
	h := keyHash(key)
	now := time.Now()
	v.mu.Lock()
	if e, ok := v.cache[h]; ok && now.Before(e.expires) {
		v.mu.Unlock()
		return e.claims, nil
	}
	v.mu.Unlock()

	body, err := json.Marshal(map[string]string{"key": key})
	if err != nil {
		return Claims{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		v.baseURL+"/internal/api-keys/validate", bytes.NewReader(body))
	if err != nil {
		return Claims{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", v.internalToken)
	resp, err := v.hc.Do(req)
	if err != nil {
		return Claims{}, fmt.Errorf("identity api-key validation: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Claims{}, fmt.Errorf("identity api-key validation: unreadable response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return Claims{}, errInvalidKey
	}
	if resp.StatusCode != http.StatusOK {
		return Claims{}, fmt.Errorf("identity api-key validation: status %d: %s", resp.StatusCode, string(raw))
	}
	var vr validateResponse
	if err := json.Unmarshal(raw, &vr); err != nil {
		return Claims{}, fmt.Errorf("identity api-key validation: undecodable response: %w", err)
	}
	tenantID, err := uuid.Parse(vr.TenantID)
	if err != nil {
		return Claims{}, fmt.Errorf("identity api-key validation: bad tenant_id %q", vr.TenantID)
	}
	claims := Claims{
		KeyID:      vr.KeyID,
		Prefix:     vr.Prefix,
		TenantID:   tenantID,
		TenantSlug: vr.TenantSlug,
		Scopes:     vr.Scopes,
	}
	// Positive cache only — a 401/revocation must take effect on the next
	// uncached call, never be extended by a cached negative.
	v.mu.Lock()
	v.cache[h] = cacheEntry{claims: claims, expires: now.Add(v.cacheTTL)}
	v.mu.Unlock()
	return claims, nil
}

// allow consumes one unit of the key's fixed-window budget.
func (v *Validator) allow(key string, now time.Time) bool {
	h := keyHash(key)
	bi, _ := v.buckets.LoadOrStore(h, &bucket{})
	b := bi.(*bucket)
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.After(b.windowEnd) {
		b.windowEnd = now.Add(v.rateWindow)
		b.count = 0
	}
	if b.count >= v.rateLimit {
		return false
	}
	b.count++
	return true
}

// Middleware returns the X-Api-Key auth middleware for the ext routes.
// requiredScope is enforced AFTER validation (403 on a valid key without
// the scope). The validated claims are stamped into the request context for
// ClaimsFrom; the tenant binding comes exclusively from the key.
func (v *Validator) Middleware(requiredScope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !v.Configured() {
				v.log.Error("ext api-key route hit but IDENTITY_BASE_URL/IDENTITY_INTERNAL_TOKEN is unset (fail closed)")
				writeError(w, http.StatusServiceUnavailable, "api key validation not configured")
				return
			}
			key := strings.TrimSpace(r.Header.Get("X-Api-Key"))
			if key == "" {
				writeError(w, http.StatusUnauthorized, "missing X-Api-Key header")
				return
			}
			// Per-key budget BEFORE validation: a hammering caller must not
			// turn into a hammering of identity (cached positives still
			// count — the budget is per key, not per validation).
			if !v.allow(key, time.Now()) {
				writeError(w, http.StatusTooManyRequests, "api key rate limit exceeded")
				return
			}
			claims, err := v.validate(r.Context(), key)
			if err != nil {
				if err == errInvalidKey { //nolint:errorlint // sentinel from a controlled path
					writeError(w, http.StatusUnauthorized, "invalid api key")
					return
				}
				v.log.Error("api key validation unavailable (fail closed)",
					zap.String("key_prefix", keyPrefix(key)), zap.Error(err))
				writeError(w, http.StatusServiceUnavailable, "api key validation unavailable")
				return
			}
			if requiredScope != "" && !claims.HasScope(requiredScope) {
				writeError(w, http.StatusForbidden, "missing scope "+requiredScope)
				return
			}
			ctx := context.WithValue(r.Context(), ctxKey{}, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// keyPrefix returns the log-safe key prefix (the public segment before the
// secret separator, truncated defensively).
func keyPrefix(key string) string {
	if i := strings.IndexByte(key, '.'); i > 0 {
		return key[:i]
	}
	if len(key) > 12 {
		return key[:12]
	}
	return key
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
