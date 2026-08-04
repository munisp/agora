package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/crm360"
	"github.com/opendesk/booking-service/internal/lending"
	"github.com/opendesk/booking-service/internal/surveys"
	"github.com/opendesk/booking-service/internal/workforce"
	"go.uber.org/zap"
)

// w20FakeResolver satisfies the W20 packages' TenantResolver; each
// package's own tenant middleware maps the resolution failure to 404
// (tenant not found) before httpapi's tenant middleware runs — proving
// the group is mounted (mirrors w19FakeResolver for workorders).
type w20FakeResolver struct{}

func (w20FakeResolver) BySlug(_ context.Context, _ string) (bookingops.TenantInfo, error) {
	return bookingops.TenantInfo{}, errors.New("no such tenant")
}

// Route wiring (SPEC-W20 integrator): the four batch-2 enterprise app
// route groups must build without chi pattern conflicts and sit behind
// tenant resolution + the appgate/perms chain (pass-through here:
// Deps.AppGate nil, Authz never reached — tenant resolution fails first).
// CRITICAL EXCEPTION: POST /v1/surveys/respond is PUBLIC — it must NOT
// sit behind the tenant/appgate/auth chain.
func TestW20AppRoutesWiring(t *testing.T) {
	r := NewRouter(Deps{
		Logger:    zap.NewNop(),
		CRM360:    &crm360.Deps{Resolver: w20FakeResolver{}},
		Surveys:   &surveys.Deps{Resolver: w20FakeResolver{}},
		Lending:   &lending.Deps{Resolver: w20FakeResolver{}},
		Workforce: &workforce.Deps{Resolver: w20FakeResolver{}},
	})

	// The W20 packages resolve the tenant themselves FIRST (their package
	// middleware wraps the integrator chain — the workorders posture):
	// without a slug the package middleware answers 400 (an unregistered
	// group would fall through to httpapi's /v1 400 — indistinguishable,
	// so the slug-bearing 404 below is the real mount proof).
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/crm/contacts/search"},
		{http.MethodGet, "/v1/surveys/surveys"},
		{http.MethodPost, "/v1/surveys/surveys"},
		{http.MethodGet, "/v1/lending/products"},
		{http.MethodGet, "/v1/lending/portfolio"},
		{http.MethodGet, "/v1/workforce/shifts"},
		{http.MethodPost, "/v1/workforce/time/clock-in"},
		{http.MethodGet, "/v1/workforce/team-members"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s %s without tenant header = %d, want 400", tc.method, tc.path, rec.Code)
		}
	}

	// With a slug and a failing resolver the package tenant middleware
	// answers 404 — proving every group (and its key routes) is mounted.
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/crm/contacts/search"},
		{http.MethodGet, "/v1/crm/contacts/00000000-0000-0000-0000-000000000000/360"},
		{http.MethodGet, "/v1/surveys/surveys"},
		{http.MethodGet, "/v1/surveys/voc/themes"},
		{http.MethodGet, "/v1/lending/products"},
		{http.MethodGet, "/v1/lending/loans"},
		{http.MethodGet, "/v1/workforce/shifts"},
		{http.MethodGet, "/v1/workforce/shifts/week"},
		{http.MethodGet, "/v1/workforce/coverage"},
		{http.MethodGet, "/v1/workforce/utilization"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-Tenant-Slug", "acme")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s with failing resolver = %d, want 404 (group mounted)", tc.method, tc.path, rec.Code)
		}
	}

	// PUBLIC exception (SPEC-W20 integrator contract §2): POST
	// /v1/surveys/respond must NOT be gated — no X-Tenant-Slug, no JWT, no
	// appgate. An empty/missing token short-circuits to the handler's 404
	// ("not found") WITHOUT any tenant resolution; 400 (tenant header
	// required) / 401 / 403 would prove the chain leaked onto it.
	for _, body := range []string{`{}`, `{"token":""}`} {
		req := httptest.NewRequest(http.MethodPost, "/v1/surveys/respond", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("POST /v1/surveys/respond %s without auth = %d, want 404 (public path reached the handler; body %s)",
				body, rec.Code, rec.Body.String())
		}
	}
}

// Partial deployments: nil app Deps leave the groups unregistered — the
// responses must be indistinguishable from any other unknown /v1 path
// (same posture as the W19 test), and the router must never panic. The
// public respond route must ALSO be absent (it is registered by the
// package only when Surveys Deps are wired).
func TestW20AppRoutesAbsentWhenDepsNil(t *testing.T) {
	r := NewRouter(Deps{Logger: zap.NewNop()})
	baseline := httptest.NewRecorder()
	r.ServeHTTP(baseline, httptest.NewRequest(http.MethodGet, "/v1/no-such-group/resource", nil))

	for _, path := range []string{
		"/v1/crm/contacts/search",
		"/v1/surveys/surveys",
		"/v1/lending/products",
		"/v1/workforce/shifts",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != baseline.Code || rec.Body.String() != baseline.Body.String() {
			t.Fatalf("GET %s with nil app Deps = %d %q, want baseline %d %q (unregistered)",
				path, rec.Code, rec.Body.String(), baseline.Code, baseline.Body.String())
		}
	}

	postBaseline := httptest.NewRecorder()
	r.ServeHTTP(postBaseline, httptest.NewRequest(http.MethodPost, "/v1/no-such-group/resource", strings.NewReader(`{}`)))
	req := httptest.NewRequest(http.MethodPost, "/v1/surveys/respond", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != postBaseline.Code || rec.Body.String() != postBaseline.Body.String() {
		t.Fatalf("POST /v1/surveys/respond with nil Deps = %d %q, want baseline %d %q (unregistered)",
			rec.Code, rec.Body.String(), postBaseline.Code, postBaseline.Body.String())
	}
}
