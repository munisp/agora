// Package appgate implements the OpenDesk app entitlement gate (SPEC-W18
// contract §4): per-route-group HTTP middleware that asks identity-service
// whether the tenant is entitled to the app owning those routes.
//
// Contract (identity-service, SPEC-W18 §1):
//
//	GET /internal/entitlements/check?app_id=<id>   (X-Tenant-Slug header)
//	200 → {"app_id": "...", "allowed": bool,
//	       "reason": "enabled|disabled|suspended|not_provisioned"}
//	404 → {"error": ...}   (unknown app_id — callers treat as denied)
//
// The gate Dapr-invokes identity through the sidecar
// (GET {base}/v1.0/invoke/{identity-app-id}/method/internal/entitlements/check),
// forwarding X-Tenant-Slug per the platform's internal-call pattern, and
// caches decisions per (tenant, app) for CacheTTL (default 60s) with
// singleflight so a cache miss fans out to exactly one upstream call.
//
// FAILURE POLICY (mirrors the AUTHZ_OUTAGE_POLICY=fail_closed idiom):
//   - entitlement endpoint 5xx / timeout / transport error → fail CLOSED:
//     503 with Retry-After (outage results are never cached);
//   - reason=not_provisioned → 402 Payment Required;
//   - reason=disabled|suspended, unknown app (404), or any other denial → 403;
//   - denial bodies are {"error", "app_id", "reason"}.
//
// OPT-IN ONLY: with Enabled=false (the APP_GATE_ENABLED=false DEFAULT) the
// middleware is a pure pass-through — no upstream call, no behavior change.
// Production behavior is unchanged unless explicitly opted in.
package appgate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

// Decision reasons from identity's entitlement check (SPEC-W18 §1) plus the
// reasons the gate synthesizes itself.
const (
	ReasonEnabled        = "enabled"
	ReasonDisabled       = "disabled"
	ReasonSuspended      = "suspended"
	ReasonNotProvisioned = "not_provisioned"
	// ReasonUnknownApp is synthesized when identity answers 404 (unknown
	// app_id). Per contract, callers treat it as denied.
	ReasonUnknownApp = "unknown_app"
	// ReasonUnavailable is synthesized when the entitlement endpoint itself
	// errors (5xx/timeout) and the gate fails closed with 503.
	ReasonUnavailable = "entitlement_unavailable"
)

// Defaults (overridable via Options).
const (
	DefaultIdentityAppID     = "identity"
	DefaultCacheTTL          = 60 * time.Second
	DefaultRetryAfterSeconds = 5
	defaultHTTPTimeout       = 5 * time.Second
)

// Entitlement mirrors identity-service's entitlement-check response.
type Entitlement struct {
	AppID   string `json:"app_id"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

// Options configures a Gate. See the package doc for the failure policy.
type Options struct {
	// Enabled gates requests when true. false (APP_GATE_ENABLED default) →
	// fully permissive pass-through: production behavior is unchanged.
	Enabled bool
	// IdentityAppID is the Dapr app-id of identity-service (default "identity").
	IdentityAppID string
	// BaseURL is the Dapr sidecar base, e.g. "http://daprd-booking:3500".
	// Required when Enabled. Tests point it at an httptest server.
	BaseURL string
	// CacheTTL is the per-(tenant, app) decision cache TTL (default 60s).
	CacheTTL time.Duration
	// RetryAfterSeconds is the Retry-After value on fail-closed 503s
	// (default 5).
	RetryAfterSeconds int
	// TenantSlug extracts the tenant slug for the upstream X-Tenant-Slug
	// header. Default reads the incoming X-Tenant-Slug header; servers with
	// a tenant middleware install an extractor that prefers the resolved
	// tenant context (see SetTenantSlugFunc).
	TenantSlug func(*http.Request) string
	// HTTPClient overrides the upstream client (default: 5s timeout).
	HTTPClient *http.Client
	// Logger (default: no-op).
	Logger *zap.Logger
}

// Gate is a concurrency-safe entitlement gate. Construct once at boot with
// New and share it across route groups.
type Gate struct {
	enabled    bool
	appID      string
	base       string
	ttl        time.Duration
	retryAfter int
	log        *zap.Logger
	hc         *http.Client

	mu     sync.RWMutex // guards slugOf and cache
	slugOf func(*http.Request) string
	cache  map[string]cacheEntry
	sf     singleflight.Group
}

type cacheEntry struct {
	ent       Entitlement
	fetchedAt time.Time
}

// New builds a Gate. When opts.Enabled is false the returned gate is a pure
// pass-through and no other option matters.
func New(opts Options) *Gate {
	if opts.IdentityAppID == "" {
		opts.IdentityAppID = DefaultIdentityAppID
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = DefaultCacheTTL
	}
	if opts.RetryAfterSeconds <= 0 {
		opts.RetryAfterSeconds = DefaultRetryAfterSeconds
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if opts.Logger == nil {
		opts.Logger = zap.NewNop()
	}
	g := &Gate{
		enabled:    opts.Enabled,
		appID:      opts.IdentityAppID,
		base:       opts.BaseURL,
		ttl:        opts.CacheTTL,
		retryAfter: opts.RetryAfterSeconds,
		log:        opts.Logger,
		hc:         opts.HTTPClient,
		slugOf:     opts.TenantSlug,
		cache:      map[string]cacheEntry{},
	}
	if g.slugOf == nil {
		g.slugOf = func(r *http.Request) string { return r.Header.Get("X-Tenant-Slug") }
	}
	return g
}

// SetTenantSlugFunc installs the slug extractor (e.g. one preferring the
// tenant resolved by an upstream middleware). Call during router
// construction, before serving.
func (g *Gate) SetTenantSlugFunc(fn func(*http.Request) string) {
	g.mu.Lock()
	g.slugOf = fn
	g.mu.Unlock()
}

// Enabled reports whether the gate enforces (false → pass-through).
func (g *Gate) Enabled() bool { return g.enabled }

// Middleware returns chi-compatible middleware gating a route group behind
// appID (the catalog app_id owning those routes). With the gate disabled
// this is a pure pass-through: production behavior is unchanged unless
// APP_GATE_ENABLED=true is set explicitly.
func (g *Gate) Middleware(appID string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !g.enabled {
				// APP_GATE_ENABLED=false (DEFAULT): fully permissive
				// pass-through — no upstream call, behavior unchanged.
				next.ServeHTTP(w, r)
				return
			}
			g.mu.RLock()
			slug := g.slugOf(r)
			g.mu.RUnlock()
			if slug == "" {
				writeGateError(w, http.StatusBadRequest, appID, "",
					"tenant slug (X-Tenant-Slug header or resolved tenant) required for entitlement check")
				return
			}
			ent, err := g.entitled(r.Context(), slug, appID)
			if err != nil {
				// Fail CLOSED on entitlement outages (5xx/timeout/transport),
				// mirroring AUTHZ_OUTAGE_POLICY=fail_closed. Outage results
				// are never cached — the next request retries upstream.
				g.log.Error("app entitlement check failed; failing closed",
					zap.String("app_id", appID), zap.String("tenant_slug", slug), zap.Error(err))
				w.Header().Set("Retry-After", strconv.Itoa(g.retryAfter))
				writeGateError(w, http.StatusServiceUnavailable, appID, ReasonUnavailable,
					"app entitlement service unavailable")
				return
			}
			if ent.Allowed {
				next.ServeHTTP(w, r)
				return
			}
			if ent.Reason == ReasonNotProvisioned {
				writeGateError(w, http.StatusPaymentRequired, appID, ent.Reason,
					"app "+appID+" is not provisioned for this tenant")
				return
			}
			// disabled | suspended | unknown_app | any other denial → 403.
			writeGateError(w, http.StatusForbidden, appID, ent.Reason,
				"app "+appID+" is not enabled for this tenant")
		})
	}
}

// entitled resolves the cached or freshly-fetched entitlement decision for
// (slug, appID). Allow/deny decisions (including unknown_app) are cached for
// the TTL; upstream errors are returned, never cached.
func (g *Gate) entitled(ctx context.Context, slug, appID string) (Entitlement, error) {
	key := slug + "|" + appID
	if ent, ok := g.cached(key); ok {
		return ent, nil
	}
	v, err, _ := g.sf.Do(key, func() (any, error) {
		// Double-check inside the flight: a concurrent caller may have
		// populated the cache while this one queued.
		if ent, ok := g.cached(key); ok {
			return ent, nil
		}
		ent, err := g.fetch(ctx, slug, appID)
		if err != nil {
			return Entitlement{}, err
		}
		g.mu.Lock()
		g.cache[key] = cacheEntry{ent: ent, fetchedAt: time.Now()}
		g.mu.Unlock()
		return ent, nil
	})
	if err != nil {
		return Entitlement{}, err
	}
	return v.(Entitlement), nil
}

func (g *Gate) cached(key string) (Entitlement, bool) {
	g.mu.RLock()
	entry, ok := g.cache[key]
	g.mu.RUnlock()
	if !ok || time.Since(entry.fetchedAt) >= g.ttl {
		return Entitlement{}, false
	}
	return entry.ent, true
}

// fetch performs the Dapr service invocation:
// GET {base}/v1.0/invoke/{identity}/method/internal/entitlements/check?app_id=
// with the X-Tenant-Slug internal-call header. 404 (unknown app) maps to a
// denied decision; 5xx/timeout/other statuses map to an error (fail closed).
func (g *Gate) fetch(ctx context.Context, slug, appID string) (Entitlement, error) {
	u := fmt.Sprintf("%s/v1.0/invoke/%s/method/internal/entitlements/check?app_id=%s",
		g.base, g.appID, url.QueryEscape(appID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Entitlement{}, err
	}
	req.Header.Set("X-Tenant-Slug", slug)
	resp, err := g.hc.Do(req)
	if err != nil {
		return Entitlement{}, fmt.Errorf("entitlement check %s: %w", appID, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusOK:
		var ent Entitlement
		if err := json.Unmarshal(body, &ent); err != nil {
			return Entitlement{}, fmt.Errorf("entitlement check %s: decode: %w", appID, err)
		}
		if ent.AppID == "" {
			ent.AppID = appID
		}
		if ent.Reason == "" {
			// Tolerate a bare {"allowed": ...} body.
			if ent.Allowed {
				ent.Reason = ReasonEnabled
			} else {
				ent.Reason = ReasonDisabled
			}
		}
		return ent, nil
	case resp.StatusCode == http.StatusNotFound:
		// Unknown app_id ({error} shape) — contract: callers treat as denied.
		return Entitlement{AppID: appID, Allowed: false, Reason: ReasonUnknownApp}, nil
	default:
		return Entitlement{}, fmt.Errorf("entitlement check %s: status %d: %s",
			appID, resp.StatusCode, string(body))
	}
}

// writeGateError emits the contract denial body {"error", "app_id", "reason"}.
func writeGateError(w http.ResponseWriter, status int, appID, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":  msg,
		"app_id": appID,
		"reason": reason,
	})
}
