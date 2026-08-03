package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// Route wiring (SPEC-W16 Agent B): the router must build without chi
// pattern conflicts; /v1/devices + /v1/field/capture sit behind the tenant
// middleware (+ Permify require wrappers), and /internal/devices uses the
// X-Tenant-Slug internal pattern like /internal/contacts.
func TestDevicesFieldRoutesWiring(t *testing.T) {
	r := NewRouter(Deps{Logger: zap.NewNop()})

	// Tenant-scoped routes: tenant middleware applies (400 without header).
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/v1/devices"},
		{http.MethodGet, "/v1/devices"},
		{http.MethodDelete, "/v1/devices/some-token"},
		{http.MethodPost, "/v1/field/capture"},
		{http.MethodGet, "/internal/devices"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s %s without tenant header = %d, want 400", tc.method, tc.path, rec.Code)
		}
	}
}
