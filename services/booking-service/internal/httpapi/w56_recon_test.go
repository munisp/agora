package httpapi

// SPEC-W56 Scope A item 8: endpoint tests for the receipt↔payment recon
// routes — GET /v1/recon/receipts behind manage_bookings (perm denial,
// nil-service 503) and POST /v1/internal/recon/receipts internauth
// fail-closed posture (503 when BOOKING_INTERNAL_TOKEN unset, 401 on a
// wrong token, constant-time compare). Middleware-level tests (no DB): the
// deny/503/401 paths never reach the store. The store-backed sweep itself
// is covered by internal/recon/store_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/daprc"
	"go.uber.org/zap"
)

// w56ReconRequest builds a request on the recon route with the tenant/user
// context the tenant middleware would have injected.
func w56ReconRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	ctx := context.WithValue(r.Context(), ctxTenant, bookingops.TenantInfo{ID: uuid.New(), Slug: "acme-w56"})
	ctx = context.WithValue(ctx, ctxUser, "user-1")
	return r.WithContext(ctx)
}

// GET /v1/recon/receipts requires manage_bookings: a Permify denial → 403
// and the handler never runs.
func TestReconListPermDenied(t *testing.T) {
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
	h.ServeHTTP(rec, w56ReconRequest(http.MethodGet, "/v1/recon/receipts"))
	if rec.Code != http.StatusForbidden || reached {
		t.Fatalf("denied perm must 403 before the handler (status %d, reached %v)", rec.Code, reached)
	}
}

// A Permify OUTAGE fails closed with 502 (AUTHZ_OUTAGE_POLICY default), the
// recon read path included.
func TestReconListPermOutageFailsClosed(t *testing.T) {
	s := &server{d: Deps{
		Authz:  failingAuthz{err: errors.New("permify down")},
		Logger: zap.NewNop(),
	}}
	reached := false
	h := s.require("manage_bookings")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, w56ReconRequest(http.MethodGet, "/v1/recon/receipts"))
	if rec.Code != http.StatusBadGateway || reached {
		t.Fatalf("outage must fail closed (status %d, reached %v)", rec.Code, reached)
	}
}

// GET /v1/recon/receipts with a nil recon service → 503 (never a panic).
func TestReconListServiceUnavailable(t *testing.T) {
	s := &server{d: Deps{Logger: zap.NewNop()}}
	rec := httptest.NewRecorder()
	s.reconListHandler(rec, w56ReconRequest(http.MethodGet, "/v1/recon/receipts"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil recon must 503, got %d", rec.Code)
	}
}

// POST /v1/internal/recon/receipts: BOOKING_INTERNAL_TOKEN unset → 503
// (fail closed, K2 idiom — never silently open).
func TestReconTriggerInternauthUnsetToken(t *testing.T) {
	s := &server{d: Deps{Logger: zap.NewNop()}}
	reached := false
	h := s.internAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/internal/recon/receipts", nil)
	req.Header.Set("X-Internal-Token", "anything")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || reached {
		t.Fatalf("unset token must 503 (status %d, reached %v)", rec.Code, reached)
	}
}

// Missing/wrong X-Internal-Token → 401.
func TestReconTriggerInternauthWrongToken(t *testing.T) {
	s := &server{d: Deps{Logger: zap.NewNop(), InternalToken: "sekrit"}}
	reached := false
	h := s.internAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	for _, tok := range []string{"", "wrong"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/internal/recon/receipts", nil)
		if tok != "" {
			req.Header.Set("X-Internal-Token", tok)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || reached {
			t.Fatalf("token %q must 401 (status %d, reached %v)", tok, rec.Code, reached)
		}
	}
}

// Correct token passes the middleware; a nil recon service then answers 503.
func TestReconTriggerServiceUnavailable(t *testing.T) {
	s := &server{d: Deps{Logger: zap.NewNop(), InternalToken: "sekrit"}}
	h := s.internAuth(http.HandlerFunc(s.reconTriggerHandler))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/internal/recon/receipts", nil)
	req.Header.Set("X-Internal-Token", "sekrit")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("valid token + nil recon must 503, got %d", rec.Code)
	}
}

// Route wiring proof: the router actually mounts both recon routes (W53
// lesson — central wiring, no self-registration). Full middleware chain
// with a stub identity resolver; nil recon service → both routes 503.
func TestReconRoutesWired(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme-w56", Name: "Acme W56", Timezone: "UTC", Currency: "NGN"}
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
		InternalToken: "sekrit",
		AuthzDisabled: true,
		Resolver:      resolver,
	})
	// GET /v1/recon/receipts — tenant resolves, require() disabled, nil
	// recon service answers 503 (an unwired route would 404).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/recon/receipts?status=open&limit=10", nil)
	req.Header.Set("X-Tenant-Slug", tenant.Slug)
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /v1/recon/receipts wired + nil service → 503, got %d", rec.Code)
	}
	// POST /v1/internal/recon/receipts — internauth passes, nil recon 503s.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/internal/recon/receipts", nil)
	req.Header.Set("X-Internal-Token", "sekrit")
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /v1/internal/recon/receipts wired + nil service → 503, got %d", rec.Code)
	}
}
