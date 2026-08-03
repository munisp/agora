package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// Route wiring (SPEC-W14 Agent A): the router must build without chi
// pattern conflicts and every referrals/commissions route must sit behind
// the tenant middleware (400 without X-Tenant-Slug — same assert as the
// leads route test). With Deps.Referrals nil the handlers answer 503 AFTER
// tenant resolution; that path needs a resolver and is covered by the
// service tests instead.
func TestReferralRoutesWiring(t *testing.T) {
	r := NewRouter(Deps{Logger: zap.NewNop()})

	id := "11111111-1111-1111-1111-111111111111"
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/referrals"},
		{http.MethodPost, "/v1/referrals"},
		{http.MethodGet, "/v1/referrals/" + id},
		{http.MethodPost, "/v1/referrals/" + id + "/verify"},
		{http.MethodPost, "/v1/referrals/" + id + "/reject"},
		{http.MethodGet, "/v1/commissions/rules"},
		{http.MethodPost, "/v1/commissions/rules"},
		{http.MethodPut, "/v1/commissions/rules/" + id},
		{http.MethodDelete, "/v1/commissions/rules/" + id},
		{http.MethodGet, "/v1/commissions/ledger"},
		{http.MethodGet, "/v1/commissions/balance/agent-1"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s %s without tenant header = %d, want 400 (route must exist behind tenant middleware)", tc.method, tc.path, rec.Code)
		}
	}
}
