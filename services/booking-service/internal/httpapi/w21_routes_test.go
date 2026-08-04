package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/daprc"
	"github.com/opendesk/booking-service/internal/socialpub"
	"go.uber.org/zap"
)

// Route wiring (SPEC-W21 integrator): the social-publisher route group
// (/v1/social, appgate app_id "social-publisher") must build without chi
// pattern conflicts and sit behind tenant resolution → appgate → the
// method-aware perms chain (GET/HEAD → view_analytics, writes →
// manage_bookings). socialpub follows the W19 helpdesk posture (it reads
// the tenant via the accessor NewRouter attaches), so the proofs are:
//
//  1. no tenant slug → 400 from httpapi's tenantMiddleware (the group is
//     mounted — an unregistered group would hit the same 400 only via
//     /v1's own chain, so proofs 2–3 disambiguate);
//  2. slug resolved (fake daprd) + NO JWT sub → 401 from require()
//     ("authenticated subject required") — the perms chain runs AFTER
//     tenant resolution, i.e. it is wired;
//  3. slug + JWT sub + denying Authz → 403 (GET → view_analytics, POST →
//     manage_bookings) — the method-aware requireReadWrite chain is the
//     gate in front of every social route;
//  4. nil Social Deps → the group is absent, indistinguishable from any
//     other unknown /v1 path (partial-deployment posture, mirrors the
//     W19/W20 tests).

// w21FakeDaprd fakes the identity-service tenant-resolution invocation
// endpoint behind Dapr (mirrors bookingops/resolver_test.go's fakeDaprd).
type w21FakeDaprd struct {
	srv *httptest.Server
}

func newW21FakeDaprd(t *testing.T) *w21FakeDaprd {
	t.Helper()
	f := &w21FakeDaprd{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(bookingops.TenantInfo{
			ID:       uuid.MustParse("22222222-2222-2222-2222-222222222222"),
			Name:     "Acme",
			Timezone: "Africa/Lagos",
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *w21FakeDaprd) resolver(t *testing.T) *bookingops.TenantResolver {
	t.Helper()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(f.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return bookingops.NewTenantResolver(daprc.New(host, port), "identity", time.Minute, zap.NewNop())
}

// w21FakeAuthz is a deterministic permify.Authorizer double.
type w21FakeAuthz struct{ allow bool }

func (f w21FakeAuthz) Check(_ context.Context, _, _, _, _ string) (bool, error) {
	return f.allow, nil
}

// w21JWT builds an unsigned JWT carrying just the sub claim (signature
// verification is the APISIX gateway's job — parseBearerClaims only reads
// the payload).
func w21JWT(sub string) string {
	enc := base64.RawURLEncoding
	payload, _ := json.Marshal(map[string]any{"sub": sub})
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		enc.EncodeToString(payload) + "." + enc.EncodeToString([]byte("sig"))
}

func newW21Router(t *testing.T, authz w21FakeAuthz) http.Handler {
	t.Helper()
	return NewRouter(Deps{
		Logger:   zap.NewNop(),
		Resolver: newW21FakeDaprd(t).resolver(t),
		Authz:    authz,
		Social:   &socialpub.Deps{}, // mount proof only — no store needed: the chain stops requests before any handler runs
	})
}

var w21SocialRoutes = []struct {
	method, path string
}{
	{http.MethodGet, "/v1/social/accounts"},
	{http.MethodPost, "/v1/social/accounts"},
	{http.MethodGet, "/v1/social/creatives"},
	{http.MethodPost, "/v1/social/creatives"},
	{http.MethodGet, "/v1/social/posts"},
	{http.MethodPost, "/v1/social/posts"},
	{http.MethodPost, "/v1/social/posts/33333333-3333-3333-3333-333333333333/publish"},
	{http.MethodGet, "/v1/social/ads"},
	{http.MethodPost, "/v1/social/ads"},
	{http.MethodPost, "/v1/social/ads/44444444-4444-4444-4444-444444444444/launch"},
	{http.MethodGet, "/v1/social/ads/44444444-4444-4444-4444-444444444444/stats"},
}

func TestW21SocialRoutesWiring(t *testing.T) {
	r := newW21Router(t, w21FakeAuthz{allow: false})

	// (1) No tenant slug → 400 from httpapi's tenantMiddleware.
	for _, tc := range w21SocialRoutes {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s %s without tenant header = %d, want 400", tc.method, tc.path, rec.Code)
		}
	}

	// (2) Slug resolved but NO JWT sub → 401 from require() — the perms
	// chain is wired after tenant resolution (an unmounted group could
	// never get past the 400 above into require()).
	for _, tc := range w21SocialRoutes {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.Header.Set("X-Tenant-Slug", "acme")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s with tenant, no JWT = %d, want 401 (perms chain reached)", tc.method, tc.path, rec.Code)
		}
	}

	// (3) Slug + JWT sub + denying Authz → 403 for BOTH method classes
	// (GET → view_analytics, POST → manage_bookings via requireReadWrite).
	for _, tc := range w21SocialRoutes {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.Header.Set("X-Tenant-Slug", "acme")
		req.Header.Set("Authorization", "Bearer "+w21JWT("user-1"))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s with denying authz = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}

	// (3b) Sanity: an allowing Authz lets the request through the chain —
	// it then reaches the (store-less) handler, proving the route table
	// itself is mounted (500 from the nil store, recovered by middleware —
	// never 404/405).
	allowing := newW21Router(t, w21FakeAuthz{allow: true})
	req := httptest.NewRequest(http.MethodGet, "/v1/social/accounts", nil)
	req.Header.Set("X-Tenant-Slug", "acme")
	req.Header.Set("Authorization", "Bearer "+w21JWT("user-1"))
	rec := httptest.NewRecorder()
	allowing.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed ||
		rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("GET /v1/social/accounts with allowing authz = %d, want a handler response (mounted)", rec.Code)
	}
}

// Partial deployments: nil Social Deps leave the group unregistered — the
// responses must be indistinguishable from any other unknown /v1 path
// (same posture as the W19/W20 tests), and the router must never panic.
func TestW21SocialRoutesAbsentWhenDepsNil(t *testing.T) {
	r := NewRouter(Deps{Logger: zap.NewNop()})
	baseline := httptest.NewRecorder()
	r.ServeHTTP(baseline, httptest.NewRequest(http.MethodGet, "/v1/no-such-group/resource", nil))

	for _, path := range []string{
		"/v1/social/accounts",
		"/v1/social/creatives",
		"/v1/social/posts",
		"/v1/social/ads",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != baseline.Code || rec.Body.String() != baseline.Body.String() {
			t.Fatalf("GET %s with nil Social Deps = %d %q, want baseline %d %q (unregistered)",
				path, rec.Code, rec.Body.String(), baseline.Code, baseline.Body.String())
		}
	}
}
