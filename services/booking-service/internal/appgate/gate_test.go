package appgate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEntitlements is an httptest fake of identity-service's
// GET /internal/entitlements/check (SPEC-W18 contract §1). It records call
// count, path, query and the forwarded X-Tenant-Slug header.
type fakeEntitlements struct {
	calls    atomic.Int32
	lastPath atomic.Value // string
	lastSlug atomic.Value // string
	lastApp  atomic.Value // string

	mu      sync.Mutex
	handler http.HandlerFunc
}

func newFakeEntitlements(h http.HandlerFunc) (*fakeEntitlements, *httptest.Server) {
	f := &fakeEntitlements{handler: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.lastPath.Store(r.URL.Path)
		f.lastSlug.Store(r.Header.Get("X-Tenant-Slug"))
		f.lastApp.Store(r.URL.Query().Get("app_id"))
		f.mu.Lock()
		h := f.handler
		f.mu.Unlock()
		h(w, r)
	}))
	return f, srv
}

func (f *fakeEntitlements) setHandler(h http.HandlerFunc) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
}

// jsonEntitlement replies 200 with the contract entitlement body.
func jsonEntitlement(appID string, allowed bool, reason string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app_id":  appID,
			"allowed": allowed,
			"reason":  reason,
		})
	}
}

func newGate(enabled bool, baseURL string) *Gate {
	return New(Options{Enabled: enabled, BaseURL: baseURL})
}

// serve runs one gated request for tenant slug "acme" and returns the recorder.
func serve(g *Gate, appID, slug string) *httptest.ResponseRecorder {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := g.Middleware(appID)(next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/leads", nil)
	if slug != "" {
		req.Header.Set("X-Tenant-Slug", slug)
	}
	h.ServeHTTP(rec, req)
	return rec
}

// decodeBody decodes the {error, app_id, reason} denial body.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode denial body: %v (%q)", err, rec.Body.String())
	}
	return body
}

// Allowed/denied matrix: reason → HTTP status mapping (SPEC-W18 contract §4).
func TestGateDecisionMatrix(t *testing.T) {
	cases := []struct {
		name       string
		upstream   http.HandlerFunc
		wantStatus int
		wantReason string
	}{
		{"enabled allows", jsonEntitlement("cac", true, "enabled"), http.StatusNoContent, ""},
		{"disabled → 403", jsonEntitlement("cac", false, "disabled"), http.StatusForbidden, "disabled"},
		{"suspended → 403", jsonEntitlement("cac", false, "suspended"), http.StatusForbidden, "suspended"},
		{"not_provisioned → 402", jsonEntitlement("cac", false, "not_provisioned"), http.StatusPaymentRequired, "not_provisioned"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := newFakeEntitlements(tc.upstream)
			defer srv.Close()
			rec := serve(newGate(true, srv.URL), "cac", "acme")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusNoContent {
				return
			}
			body := decodeBody(t, rec)
			if body["reason"] != tc.wantReason || body["app_id"] != "cac" || body["error"] == "" {
				t.Fatalf("denial body = %v, want reason=%q app_id=cac with error", body, tc.wantReason)
			}
		})
	}
}

// The gate must call the Dapr-invoke path with the X-Tenant-Slug internal
// header and the app_id query param.
func TestGateInvokeShape(t *testing.T) {
	fake, srv := newFakeEntitlements(jsonEntitlement("cac", true, "enabled"))
	defer srv.Close()
	rec := serve(newGate(true, srv.URL), "cac", "acme")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := fake.lastPath.Load().(string); got != "/v1.0/invoke/identity/method/internal/entitlements/check" {
		t.Fatalf("upstream path = %q, want Dapr invoke of internal/entitlements/check", got)
	}
	if got := fake.lastSlug.Load().(string); got != "acme" {
		t.Fatalf("X-Tenant-Slug = %q, want acme", got)
	}
	if got := fake.lastApp.Load().(string); got != "cac" {
		t.Fatalf("app_id query = %q, want cac", got)
	}
}

// Unknown app → identity answers 404 {error}; callers treat as denied (403).
func TestGateUnknownApp404MapsTo403(t *testing.T) {
	_, srv := newFakeEntitlements(func(w http.ResponseWriter, r *http.Request) {

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown app"})
	})
	defer srv.Close()
	rec := serve(newGate(true, srv.URL), "no-such-app", "acme")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["reason"] != ReasonUnknownApp || body["app_id"] != "no-such-app" {
		t.Fatalf("denial body = %v, want reason=unknown_app app_id=no-such-app", body)
	}
}

// Entitlement outage (5xx) → fail closed 503 with Retry-After, and the
// failure is NOT cached (a retry hits upstream again and can succeed).
func TestGateUpstream5xxFailsClosed(t *testing.T) {
	fake, srv := newFakeEntitlements(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	defer srv.Close()
	g := newGate(true, srv.URL)

	rec := serve(g, "cac", "acme")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("503 must carry Retry-After")
	}
	body := decodeBody(t, rec)
	if body["reason"] != ReasonUnavailable {
		t.Fatalf("reason = %q, want %q", body["reason"], ReasonUnavailable)
	}

	// Outage results are never cached: upstream recovery is seen immediately.
	fake.setHandler(jsonEntitlement("cac", true, "enabled"))
	rec = serve(g, "cac", "acme")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("after recovery status = %d, want 204", rec.Code)
	}
	if fake.calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2 (outage must not be cached)", fake.calls.Load())
	}
}

// Transport error (sidecar unreachable) → same fail-closed 503.
func TestGateTransportErrorFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close() // nothing listening now

	rec := serve(newGate(true, base), "cac", "acme")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("503 must carry Retry-After")
	}
}

// Cache hit behavior: a second request for the same (tenant, app) within the
// TTL makes NO upstream call; a different tenant is cached separately; after
// TTL expiry the decision is refreshed.
func TestGateCacheBehavior(t *testing.T) {
	fake, srv := newFakeEntitlements(jsonEntitlement("cac", true, "enabled"))
	defer srv.Close()
	g := New(Options{Enabled: true, BaseURL: srv.URL, CacheTTL: 60 * time.Millisecond})

	serve(g, "cac", "acme")
	serve(g, "cac", "acme")
	serve(g, "cac", "acme")
	if got := fake.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (cache hit must skip HTTP)", got)
	}

	serve(g, "cac", "globex") // different tenant → separate cache entry
	if got := fake.calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (per-tenant cache key)", got)
	}

	time.Sleep(80 * time.Millisecond) // let the TTL lapse
	serve(g, "cac", "acme")
	if got := fake.calls.Load(); got != 3 {
		t.Fatalf("upstream calls = %d, want 3 after TTL expiry", got)
	}
}

// Denials are cached too: a disabled decision is served from cache for the
// TTL, so status flips propagate within one TTL window.
func TestGateDenialCached(t *testing.T) {
	fake, srv := newFakeEntitlements(jsonEntitlement("cac", false, "disabled"))
	defer srv.Close()
	g := newGate(true, srv.URL)

	if rec := serve(g, "cac", "acme"); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if rec := serve(g, "cac", "acme"); rec.Code != http.StatusForbidden {
		t.Fatalf("cached status = %d, want 403", rec.Code)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (denial cached)", got)
	}
}

// Singleflight: concurrent cache misses for the same (tenant, app) fan out
// to exactly one upstream call.
func TestGateSingleflight(t *testing.T) {
	fake, srv := newFakeEntitlements(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(25 * time.Millisecond) // widen the overlap window
		jsonEntitlement("cac", true, "enabled")(w, r)
	})
	defer srv.Close()
	g := newGate(true, srv.URL)

	const n = 16
	var wg sync.WaitGroup
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = serve(g, "cac", "acme").Code
		}(i)
	}
	wg.Wait()
	for i, st := range statuses {
		if st != http.StatusNoContent {
			t.Fatalf("request %d status = %d, want 204", i, st)
		}
	}
	if got := fake.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (singleflight)", got)
	}
}

// APP_GATE_ENABLED=false (the DEFAULT): fully permissive pass-through — no
// upstream call even when identity would deny. Production behavior unchanged.
func TestGateDisabledPassThrough(t *testing.T) {
	fake, srv := newFakeEntitlements(jsonEntitlement("cac", false, "disabled"))
	defer srv.Close()
	g := newGate(false, srv.URL)
	if g.Enabled() {
		t.Fatal("gate must report disabled")
	}
	rec := serve(g, "cac", "acme")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (pass-through when disabled)", rec.Code)
	}
	if got := fake.calls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0 when APP_GATE_ENABLED=false", got)
	}
}

// A request without a resolvable tenant slug cannot be checked → 400.
func TestGateMissingTenantSlug(t *testing.T) {
	fake, srv := newFakeEntitlements(jsonEntitlement("cac", true, "enabled"))
	defer srv.Close()
	rec := serve(newGate(true, srv.URL), "cac", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := fake.calls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0 without a tenant slug", got)
	}
}

// SetTenantSlugFunc lets the host server prefer the middleware-resolved
// tenant over the raw header (JWT-claim path without X-Tenant-Slug).
func TestGateCustomSlugExtractor(t *testing.T) {
	fake, srv := newFakeEntitlements(jsonEntitlement("cac", true, "enabled"))
	defer srv.Close()
	g := newGate(true, srv.URL)
	g.SetTenantSlugFunc(func(r *http.Request) string { return "ctx-tenant" })

	rec := serve(g, "cac", "") // no header — extractor supplies the slug
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := fake.lastSlug.Load().(string); got != "ctx-tenant" {
		t.Fatalf("forwarded slug = %q, want ctx-tenant", got)
	}
}

// Defaults: TTL 60s, Retry-After 5s, identity app-id "identity".
func TestGateDefaults(t *testing.T) {
	g := New(Options{Enabled: true, BaseURL: "http://example"})
	if g.ttl != 60*time.Second {
		t.Fatalf("ttl = %v, want 60s", g.ttl)
	}
	if g.retryAfter != 5 {
		t.Fatalf("retryAfter = %d, want 5", g.retryAfter)
	}
	if g.appID != "identity" {
		t.Fatalf("appID = %q, want identity", g.appID)
	}
}

// A bare {"allowed": true} body (no reason) is tolerated.
func TestGateBareAllowedBody(t *testing.T) {
	_, srv := newFakeEntitlements(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"app_id":"cac","allowed":true}`)
	})
	defer srv.Close()
	if rec := serve(newGate(true, srv.URL), "cac", "acme"); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}
