package bookingops

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/daprc"
	"go.uber.org/zap"
)

// fakeDaprd fakes the identity-service invocation endpoint behind Dapr.
type fakeDaprd struct {
	srv    *httptest.Server
	calls  atomic.Int32
	broken atomic.Bool
	last   atomic.Value // "METHOD path" of the most recent request
	tenant TenantInfo
}

func newFakeDaprd(t *testing.T) *fakeDaprd {
	t.Helper()
	f := &fakeDaprd{tenant: TenantInfo{
		ID:       uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Name:     "Acme",
		Timezone: "Europe/Berlin",
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.last.Store(r.Method + " " + r.URL.Path)
		if f.broken.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		f.calls.Add(1)
		_ = json.NewEncoder(w).Encode(f.tenant)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDaprd) client(t *testing.T) *daprc.Client {
	t.Helper()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(f.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return daprc.New(host, port)
}

func TestResolverCachesWithinTTL(t *testing.T) {
	f := newFakeDaprd(t)
	r := NewTenantResolver(f.client(t), "identity", time.Minute, zap.NewNop())
	ctx := context.Background()

	t1, err := r.BySlug(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if t1.ID != f.tenant.ID || t1.Slug != "acme" || t1.Timezone != "Europe/Berlin" {
		t.Fatalf("unexpected tenant: %+v", t1)
	}
	if _, err := r.BySlug(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("identity-service called %d times, want 1 (TTL cache)", f.calls.Load())
	}
}

func TestResolverRefreshesAfterTTL(t *testing.T) {
	f := newFakeDaprd(t)
	r := NewTenantResolver(f.client(t), "identity", 30*time.Millisecond, zap.NewNop())
	ctx := context.Background()

	if _, err := r.BySlug(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := r.BySlug(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 2 {
		t.Fatalf("identity-service called %d times, want 2 (refresh after TTL)", f.calls.Load())
	}
}

// identity-service outage after the TTL expired: the stale cached entry is
// served instead of failing (Wave 5 #5).
func TestResolverServesStaleOnIdentityOutage(t *testing.T) {
	f := newFakeDaprd(t)
	r := NewTenantResolver(f.client(t), "identity", 30*time.Millisecond, zap.NewNop())
	ctx := context.Background()

	if _, err := r.BySlug(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the entry expire
	f.broken.Store(true)

	got, err := r.BySlug(ctx, "acme")
	if err != nil {
		t.Fatalf("stale entry should be served without error, got %v", err)
	}
	if got.ID != f.tenant.ID || got.Timezone != "Europe/Berlin" {
		t.Fatalf("stale tenant mismatch: %+v", got)
	}
}

// With no prior successful resolution, an outage is a hard error.
func TestResolverErrorsWhenNeverResolved(t *testing.T) {
	f := newFakeDaprd(t)
	f.broken.Store(true)
	r := NewTenantResolver(f.client(t), "identity", time.Minute, zap.NewNop())
	if _, err := r.BySlug(context.Background(), "acme"); err == nil {
		t.Fatal("expected error when identity-service is down and cache is cold")
	}
}

// fakeIdentity is a direct-HTTP identity-service stub (test double at the
// test boundary only) for the IDENTITY_BASE_URL fallback path.
type fakeIdentity struct {
	srv    *httptest.Server
	calls  atomic.Int32
	last   atomic.Value // "METHOD path" of the most recent request
	tenant TenantInfo
	status int           // response status override (0 = 200)
	delay  time.Duration // artificial latency for timeout tests
}

func newFakeIdentity(t *testing.T) *fakeIdentity {
	t.Helper()
	f := &fakeIdentity{tenant: TenantInfo{
		ID:       uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Name:     "Direct Co",
		Timezone: "Africa/Lagos",
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.last.Store(r.Method + " " + r.URL.Path)
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		status := f.status
		if status == 0 {
			status = http.StatusOK
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.tenant)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// SPEC-W42 Coder G — IDENTITY_BASE_URL hit: BySlug issues a direct HTTP GET
// {base}/v1/tenants/{slug} (no Dapr client involved) and the TTL cache
// behaves identically to the Dapr path.
func TestResolverDirectHTTPHit(t *testing.T) {
	f := newFakeIdentity(t)
	r := NewTenantResolver(nil, "identity", time.Minute, zap.NewNop(), WithIdentityBaseURL(f.srv.URL))
	ctx := context.Background()

	got, err := r.BySlug(ctx, "direct-co")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != f.tenant.ID || got.Slug != "direct-co" || got.Timezone != "Africa/Lagos" {
		t.Fatalf("unexpected tenant: %+v", got)
	}
	if got := f.last.Load().(string); got != "GET /v1/tenants/direct-co" {
		t.Fatalf("unexpected identity request: %q", got)
	}
	if _, err := r.BySlug(ctx, "direct-co"); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("identity called %d times, want 1 (TTL cache)", f.calls.Load())
	}
}

// SPEC-W42 Coder G — direct-HTTP miss: identity 404 maps to the same
// "resolve tenant %q" error semantics as the Dapr failure path (the
// middleware turns it into tenant-not-found), and nothing is cached.
func TestResolverDirectHTTPMiss404(t *testing.T) {
	f := newFakeIdentity(t)
	f.status = http.StatusNotFound
	r := NewTenantResolver(nil, "identity", time.Minute, zap.NewNop(), WithIdentityBaseURL(f.srv.URL))
	_, err := r.BySlug(context.Background(), "ghost")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), `resolve tenant "ghost"`) {
		t.Fatalf("error should carry resolve-tenant semantics, got %v", err)
	}
	if _, cached := r.cache["ghost"]; cached {
		t.Fatal("404 must not populate the tenant cache")
	}
}

// SPEC-W42 Coder G — direct-HTTP timeout: a hung identity-service surfaces
// the same error semantics as a Dapr timeout, bounded by the client timeout
// (3s default; shrunk here for a fast test).
func TestResolverDirectHTTPTimeout(t *testing.T) {
	f := newFakeIdentity(t)
	f.delay = 300 * time.Millisecond
	r := NewTenantResolver(nil, "identity", time.Minute, zap.NewNop(), WithIdentityBaseURL(f.srv.URL))
	r.httpClient.Timeout = 50 * time.Millisecond
	start := time.Now()
	if _, err := r.BySlug(context.Background(), "slow"); err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("resolver did not fail fast: %v", elapsed)
	}
}

// SPEC-W42 Coder G — cache behavior identical on the direct path: an
// expired entry is served stale when identity then errors (Wave 5 #5 parity).
func TestResolverDirectHTTPServesStaleOnOutage(t *testing.T) {
	f := newFakeIdentity(t)
	r := NewTenantResolver(nil, "identity", 30*time.Millisecond, zap.NewNop(), WithIdentityBaseURL(f.srv.URL))
	ctx := context.Background()
	if _, err := r.BySlug(ctx, "direct-co"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the entry expire
	f.status = http.StatusBadGateway

	got, err := r.BySlug(ctx, "direct-co")
	if err != nil {
		t.Fatalf("stale entry should be served without error, got %v", err)
	}
	if got.ID != f.tenant.ID || got.Timezone != "Africa/Lagos" {
		t.Fatalf("stale tenant mismatch: %+v", got)
	}
}

// SPEC-W42 Coder G — env empty (the default): the Dapr code path is
// untouched; WithIdentityBaseURL("") is a no-op and resolution still goes
// through daprd's service-invocation API (POST .../invoke/{appID}/method/...).
func TestResolverDaprPathUntouchedWhenBaseURLEmpty(t *testing.T) {
	f := newFakeDaprd(t)
	r := NewTenantResolver(f.client(t), "identity", time.Minute, zap.NewNop(), WithIdentityBaseURL(""))
	got, err := r.BySlug(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != f.tenant.ID || got.Slug != "acme" {
		t.Fatalf("unexpected tenant: %+v", got)
	}
	want := "POST /v1.0/invoke/identity/method/v1/tenants/acme"
	if got := f.last.Load().(string); got != want {
		t.Fatalf("Dapr path altered: got %q, want %q", got, want)
	}
}
