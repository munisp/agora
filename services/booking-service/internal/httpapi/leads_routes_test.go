package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// Route wiring (SPEC-W13 Agent A): the router must build without chi
// pattern conflicts; the public promo redeem + Dapr-invoked paths bypass
// Permify, while the tenant-scoped leads/promo/campaigns API requires the
// tenant middleware.
func TestLeadRoutesWiring(t *testing.T) {
	r := NewRouter(Deps{Logger: zap.NewNop()})

	// Public promo redeem: no X-Tenant-Slug needed — 503 (Leads service
	// nil), not the tenant-middleware 400.
	req := httptest.NewRequest(http.MethodPost, "/v1/promo/redeem", strings.NewReader(`{"code":"X","phone":"+234"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("promo redeem without tenant header = %d, want 503 (public path)", rec.Code)
	}

	// Internal spend-sum: tenant middleware applies (400 without header) —
	// it is invoked with X-Tenant-Slug like /internal/contacts.
	req = httptest.NewRequest(http.MethodGet, "/internal/campaigns/11111111-1111-1111-1111-111111111111/spend-sum", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("spend-sum without tenant header = %d, want 400 (tenant middleware)", rec.Code)
	}

	// Tenant-scoped reads/mutations: tenant middleware applies.
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/leads"},
		{http.MethodPost, "/v1/leads"},
		{http.MethodGet, "/v1/promo"},
		{http.MethodPost, "/v1/promo"},
		{http.MethodGet, "/v1/campaigns"},
		{http.MethodPost, "/v1/campaigns"},
		{http.MethodPost, "/v1/campaigns/11111111-1111-1111-1111-111111111111/spend"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s %s without tenant header = %d, want 400", tc.method, tc.path, rec.Code)
		}
	}
}
