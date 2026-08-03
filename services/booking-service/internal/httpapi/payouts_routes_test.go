package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// Route wiring (SPEC-W14 Agent B): GET /v1/payouts must build without chi
// pattern conflicts and sit behind the tenant middleware (400 without
// X-Tenant-Slug — same assert as the referrals route test). The nil-Deps
// 503 path needs a resolver and follows the same posture as Agent A's
// handlers (covered by service-level tests).
func TestPayoutRoutesWiring(t *testing.T) {
	r := NewRouter(Deps{Logger: zap.NewNop()})

	req := httptest.NewRequest(http.MethodGet, "/v1/payouts", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET /v1/payouts without tenant header = %d, want 400 (route must exist behind tenant middleware)", rec.Code)
	}
}
