package httpapi

// SPEC-W64: endpoint tests for POST /v1/recon/receipts/{id}/resolve —
// manage_bookings guard (mirroring the recon LIST route's posture exactly:
// Permify only, NO owner/admin realm-role gate — that is KYC/media-inbox
// only), nil-service 503, and route-wiring proof. Middleware-level (no DB);
// the store-backed 200/404/idempotency behavior is covered by
// internal/recon/w64_resolve_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/daprc"
	"go.uber.org/zap"
)

// w64ResolveRequest builds a resolve request with the tenant/user context
// the tenant middleware would have injected.
func w64ResolveRequest(path string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, nil)
	ctx := context.WithValue(r.Context(), ctxTenant, bookingops.TenantInfo{ID: uuid.New(), Slug: "acme-w64"})
	ctx = context.WithValue(ctx, ctxUser, "user-1")
	return r.WithContext(ctx)
}

// POST /v1/recon/receipts/{id}/resolve requires manage_bookings — a Permify
// denial → 403 and the handler never runs (same guard as the list route; a
// member without manage_bookings is rejected here).
func TestReconResolvePermDenied(t *testing.T) {
	s := &server{d: Deps{
		Authz:             failingAuthz{err: nil},
		AuthzOutagePolicy: AuthzFailClosed,
		Logger:            zap.NewNop(),
	}}
	reached := false
	h := s.require("manage_bookings")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, w64ResolveRequest("/v1/recon/receipts/"+uuid.NewString()+"/resolve"))
	if rec.Code != http.StatusForbidden || reached {
		t.Fatalf("denied perm must 403 before the handler (status %d, reached %v)", rec.Code, reached)
	}
}

// An anonymous caller (no JWT sub / X-User-Id resolved) is 401'd by
// require() before the Permify check — resolved_by can never be empty on
// the guarded path.
func TestReconResolveAnonymousRejected(t *testing.T) {
	s := &server{d: Deps{Logger: zap.NewNop(), Authz: failingAuthz{err: nil}}}
	reached := false
	h := s.require("manage_bookings")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/recon/receipts/"+uuid.NewString()+"/resolve", nil)
	ctx := context.WithValue(r.Context(), ctxTenant, bookingops.TenantInfo{ID: uuid.New(), Slug: "acme-w64"})
	h.ServeHTTP(rec, r.WithContext(ctx)) // no ctxUser
	if rec.Code != http.StatusUnauthorized || reached {
		t.Fatalf("anonymous must 401 (status %d, reached %v)", rec.Code, reached)
	}
}

// A nil recon service answers 503 (never a panic).
func TestReconResolveServiceUnavailable(t *testing.T) {
	s := &server{d: Deps{Logger: zap.NewNop()}}
	rec := httptest.NewRecorder()
	s.reconResolveHandler(rec, w64ResolveRequest("/v1/recon/receipts/"+uuid.NewString()+"/resolve"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil recon must 503, got %d", rec.Code)
	}
}

// Route wiring proof: the router actually mounts the resolve route (W53
// lesson — central wiring, no self-registration). Full middleware chain
// with a stub identity resolver; nil recon service → 503 (an unwired route
// would 404/405).
func TestReconResolveRouteWired(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme-w64", Name: "Acme W64", Timezone: "UTC", Currency: "NGN"}
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tenants/"+tenant.Slug {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tenant)
	}))
	t.Cleanup(identity.Close)
	resolver := bookingops.NewTenantResolver(daprc.New("localhost", 1), "identity", time.Minute, zap.NewNop(),
		bookingops.WithIdentityBaseURL(identity.URL))

	s := NewRouter(Deps{
		Logger:        zap.NewNop(),
		AuthzDisabled: true,
		Resolver:      resolver,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/recon/receipts/"+uuid.NewString()+"/resolve", nil)
	req.Header.Set("X-Tenant-Slug", tenant.Slug)
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /v1/recon/receipts/{id}/resolve wired + nil service → 503, got %d", rec.Code)
	}
}
