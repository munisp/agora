package bookingops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/daprc"
	"go.uber.org/zap"
)

// TenantInfo is the tenant context resolved from identity-service.
type TenantInfo struct {
	ID       uuid.UUID `json:"id"`
	Slug     string    `json:"slug"`
	Name     string    `json:"name"`
	Timezone string    `json:"timezone"`
	Currency string    `json:"currency"`
	Locale   string    `json:"locale"`
	Plan     string    `json:"plan"`
	// SPEC-CRM §C3: industry pack id + resolved pack summary (absent for
	// tenants created before packs existed or when no pack is loaded).
	Industry string       `json:"industry"`
	Pack     *PackSummary `json:"pack"`
}

// PackSummary mirrors identity-service's pack projection (only the fields
// booking-service consumes are typed; terminology/dashboardLabels/agentPersona
// are passed through for other consumers).
type PackSummary struct {
	ID               string            `json:"id"`
	DisplayName      string            `json:"displayName"`
	Terminology      map[string]string `json:"terminology"`
	BookingPolicy    BookingPolicy     `json:"bookingPolicy"`
	DashboardLabels  map[string]string `json:"dashboardLabels"`
	AgentPersona     string            `json:"agentPersona"`
	TemporalWorkflow string            `json:"temporalWorkflow"`
}

// BookingPolicy mirrors the pack bookingPolicy block.
type BookingPolicy struct {
	DepositPercent          int   `json:"depositPercent"`
	NoShowFeeCents          int64 `json:"noShowFeeCents"`
	PhoneConfirmation       bool  `json:"phoneConfirmation"`
	IntakeRequired          bool  `json:"intakeRequired"`
	CancellationWindowHours int   `json:"cancellationWindowHours"`
}

// Location returns the tenant's IANA timezone (UTC fallback).
func (t TenantInfo) Location() *time.Location {
	loc, err := time.LoadLocation(t.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// DefaultTenantCacheTTL is used when TENANT_CACHE_TTL_SECONDS is unset.
const DefaultTenantCacheTTL = 5 * time.Minute

// identityDirectTimeout bounds the direct HTTP fallback call
// (IDENTITY_BASE_URL mode) so a hung identity-service fails fast instead of
// stalling the booking write path.
const identityDirectTimeout = 3 * time.Second

// TenantResolver resolves tenant slugs to IDs/context via Dapr service
// invocation to identity-service, with an in-memory TTL cache. When
// IDENTITY_BASE_URL is configured (WithIdentityBaseURL) resolution instead
// issues a direct HTTP GET {base}/v1/tenants/{slug} — identity's route is
// GET while Dapr InvokeService POSTs, hence the fallback (used by tests and
// no-Dapr dev). Cache behavior is identical on both paths.
//
// Resilience (Wave 5 #5): a cached entry is served until its TTL expires;
// when identity-service then times out or errors on refresh, the EXPIRED
// entry is served stale (logged) rather than failing every tenant-scoped
// request. A tenant that was never resolved successfully still errors.
type TenantResolver struct {
	dapr  *daprc.Client
	appID string
	ttl   time.Duration
	log   *zap.Logger

	// baseURL/httpClient implement the IDENTITY_BASE_URL direct-HTTP
	// fallback. baseURL empty (the default) = unchanged Dapr behavior.
	baseURL    string
	httpClient *http.Client

	mu    sync.Mutex
	cache map[string]tenantCacheEntry
}

// TenantResolverOption customizes NewTenantResolver.
type TenantResolverOption func(*TenantResolver)

// WithIdentityBaseURL switches tenant resolution to a direct HTTP GET
// {base}/v1/tenants/{slug} (bounded by identityDirectTimeout) instead of
// Dapr service invocation. An empty base is a no-op: the Dapr code path
// stays untouched.
func WithIdentityBaseURL(base string) TenantResolverOption {
	return func(r *TenantResolver) {
		base = strings.TrimSuffix(strings.TrimSpace(base), "/")
		if base == "" {
			return
		}
		r.baseURL = base
		r.httpClient = &http.Client{Timeout: identityDirectTimeout}
	}
}

type tenantCacheEntry struct {
	info      TenantInfo
	fetchedAt time.Time
}

// NewTenantResolver builds the resolver. ttl <= 0 falls back to
// DefaultTenantCacheTTL.
func NewTenantResolver(d *daprc.Client, identityAppID string, ttl time.Duration, log *zap.Logger, opts ...TenantResolverOption) *TenantResolver {
	if ttl <= 0 {
		ttl = DefaultTenantCacheTTL
	}
	if log == nil {
		log = zap.NewNop()
	}
	r := &TenantResolver{dapr: d, appID: identityAppID, ttl: ttl, log: log, cache: map[string]tenantCacheEntry{}}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// BySlug resolves (and caches) a tenant by slug.
func (r *TenantResolver) BySlug(ctx context.Context, slug string) (TenantInfo, error) {
	r.mu.Lock()
	entry, cached := r.cache[slug]
	fresh := cached && time.Since(entry.fetchedAt) < r.ttl
	r.mu.Unlock()
	if fresh {
		return entry.info, nil
	}

	var t TenantInfo
	if err := r.fetch(ctx, slug, &t); err != nil {
		if cached {
			// identity-service timeout/outage: serve the expired entry
			// stale instead of failing every request for this tenant.
			r.log.Warn("identity-service unreachable; serving stale tenant context",
				zap.String("slug", slug), zap.Duration("age", time.Since(entry.fetchedAt)), zap.Error(err))
			return entry.info, nil
		}
		return TenantInfo{}, fmt.Errorf("resolve tenant %q: %w", slug, err)
	}
	if t.ID == uuid.Nil {
		return TenantInfo{}, fmt.Errorf("resolve tenant %q: empty id", slug)
	}
	t.Slug = slug
	r.mu.Lock()
	r.cache[slug] = tenantCacheEntry{info: t, fetchedAt: time.Now()}
	r.mu.Unlock()
	return t, nil
}

// fetch performs the identity-service lookup: Dapr service invocation by
// default, or a direct HTTP GET when IDENTITY_BASE_URL is set. Non-200
// responses (including a 404 unknown slug) and timeouts map to the same
// error semantics as the Dapr failure path, so callers (tenantMiddleware →
// 404 "tenant not found", public booking POST → 500) behave identically on
// both paths.
func (r *TenantResolver) fetch(ctx context.Context, slug string, out *TenantInfo) error {
	if r.baseURL == "" {
		return r.dapr.InvokeService(ctx, r.appID, "v1/tenants/"+slug, nil, out)
	}
	url := r.baseURL + "/v1/tenants/" + slug
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("identity direct get %s: %w", url, err)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("identity direct get %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("identity direct get %s: status %d: %s", url, resp.StatusCode, string(b))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
		return fmt.Errorf("decode identity response: %w", err)
	}
	return nil
}
