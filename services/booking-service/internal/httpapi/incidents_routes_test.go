package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// Route wiring (SPEC-W11 Part B §3/§6): the router must build without chi
// pattern conflicts; the ingest + delivery-update routes bypass the tenant
// middleware (Dapr-invoked), while the admin incidents API requires it.
func TestIncidentRoutesWiring(t *testing.T) {
	r := NewRouter(Deps{Logger: zap.NewNop()})

	// Ingest: no X-Tenant-Slug needed — 503 (Incidents service nil), not
	// the tenant-middleware 400.
	req := httptest.NewRequest(http.MethodPost, "/v1/incidents/ingest", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ingest without tenant header = %d, want 503 (tenant middleware must not run)", rec.Code)
	}

	// Delivery update: also middleware-free.
	req = httptest.NewRequest(http.MethodPost, "/internal/incidents/deliveries/11111111-1111-1111-1111-111111111111", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("delivery update = %d, want 503", rec.Code)
	}

	// Admin list: tenant middleware applies (400 without X-Tenant-Slug).
	req = httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("admin list without tenant header = %d, want 400 (tenant middleware)", rec.Code)
	}
}
