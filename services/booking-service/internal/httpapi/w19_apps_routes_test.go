package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/campaignstudio"
	"github.com/opendesk/booking-service/internal/helpdesk"
	"github.com/opendesk/booking-service/internal/loyalty"
	"github.com/opendesk/booking-service/internal/workorders"
	"go.uber.org/zap"
)

// w19FakeResolver satisfies workorders.TenantResolver; the package tenant
// middleware maps the resolution failure to 404 (tenant not found) before
// httpapi's own tenant middleware runs — proving the group is mounted.
type w19FakeResolver struct{}

func (w19FakeResolver) BySlug(_ context.Context, _ string) (bookingops.TenantInfo, error) {
	return bookingops.TenantInfo{}, errors.New("no such tenant")
}

// Route wiring (SPEC-W19 integrator): the four enterprise app route groups
// must build without chi pattern conflicts and sit behind tenant
// resolution (no request reaches a handler without a tenant context). The
// appgate middleware is a pass-through here (Deps.AppGate nil).
func TestW19AppRoutesWiring(t *testing.T) {
	r := NewRouter(Deps{
		Logger:     zap.NewNop(),
		Helpdesk:   &helpdesk.Deps{},
		Workorders: &workorders.Deps{Resolver: w19FakeResolver{}},
		Loyalty:    &loyalty.Deps{},
		Studio:     &campaignstudio.Deps{Store: &campaignstudio.Store{}},
	})

	// httpapi's tenantMiddleware rejects these before any handler (400 —
	// same posture as the W16 devices/field routes).
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/helpdesk/tickets"},
		{http.MethodPost, "/v1/helpdesk/tickets"},
		{http.MethodGet, "/v1/helpdesk/sla-policies"},
		{http.MethodGet, "/v1/helpdesk/stats"},
		{http.MethodGet, "/v1/loyalty/programs"},
		{http.MethodPost, "/v1/loyalty/accrue"},
		{http.MethodGet, "/v1/loyalty/leaderboard"},
		{http.MethodGet, "/v1/studio/segments"},
		{http.MethodGet, "/v1/studio/journeys"},
		{http.MethodPost, "/v1/studio/journeys"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s %s without tenant header = %d, want 400", tc.method, tc.path, rec.Code)
		}
	}

	// workorders resolves the tenant itself FIRST (its package middleware
	// wraps the integrator chain): with a resolver wired, a slug-bearing
	// request reaches tenant resolution and fails 404 — an unregistered
	// group would fall through to httpapi's 400 above.
	for _, path := range []string{
		"/v1/field-service/work-orders",
		"/v1/field-service/board",
		"/v1/field-service/today",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Tenant-Slug", "acme")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s with failing resolver = %d, want 404 (group mounted)", path, rec.Code)
		}
	}
}

// Partial deployments: nil app Deps leave the groups unregistered — the
// responses must be indistinguishable from any other unknown /v1 path
// (the pre-existing posture: httpapi's /v1 tenant middleware answers 400
// before chi's NotFound), and the router must never panic.
func TestW19AppRoutesAbsentWhenDepsNil(t *testing.T) {
	r := NewRouter(Deps{Logger: zap.NewNop()})
	baseline := httptest.NewRecorder()
	r.ServeHTTP(baseline, httptest.NewRequest(http.MethodGet, "/v1/no-such-group/resource", nil))

	for _, path := range []string{
		"/v1/helpdesk/tickets",
		"/v1/field-service/work-orders",
		"/v1/loyalty/programs",
		"/v1/studio/segments",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != baseline.Code || rec.Body.String() != baseline.Body.String() {
			t.Fatalf("GET %s with nil app Deps = %d %q, want baseline %d %q (unregistered)",
				path, rec.Code, rec.Body.String(), baseline.Code, baseline.Body.String())
		}
	}
}
