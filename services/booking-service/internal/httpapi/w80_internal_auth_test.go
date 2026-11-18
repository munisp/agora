package httpapi

// SPEC-W80 B7: internAuth on the legacy internal routes — the SAME
// middleware, env (BOOKING_INTERNAL_TOKEN → Deps.InternalToken) and
// status posture as the pre-existing guarded routes (civic sla-breach,
// recon receipts, media ingest): unset token → 503 fail-closed, missing or
// wrong X-Internal-Token → 401, correct token → falls through to the
// tenant middleware (400 without X-Tenant-Slug). Token matrix on two of
// the six guarded routes (plus a third for the fail-closed posture).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// w80GuardedRoutes is the B7 matrix sample: two routes of different
// shapes (GET collection + POST body).
var w80GuardedRoutes = []struct {
	method, path, body string
}{
	{http.MethodGet, "/internal/devices", ""},
	{http.MethodPost, "/v1/leads/resolve", `{"lead_ids":[]}`},
}

func TestB7InternalRoutesRequireToken(t *testing.T) {
	r := NewRouter(Deps{Logger: zap.NewNop(), InternalToken: "w80-secret"})
	for _, tc := range w80GuardedRoutes {
		// Missing token → 401 (same as the existing guarded routes).
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without token = %d, want 401", tc.method, tc.path, rec.Code)
		}
		// Wrong token → 401.
		req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("X-Internal-Token", "wrong")
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s with wrong token = %d, want 401", tc.method, tc.path, rec.Code)
		}
		// Correct token → the internAuth passes and the TENANT middleware
		// answers its usual 400 without X-Tenant-Slug (proves ordering:
		// token first, tenant second).
		req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("X-Internal-Token", "w80-secret")
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s %s with token but no tenant = %d, want 400 (tenant middleware)", tc.method, tc.path, rec.Code)
		}
	}
}

// Fail-closed: with BOOKING_INTERNAL_TOKEN unset the guarded routes answer
// 503 — never silently open (the K2 contract of the existing routes).
func TestB7InternalRoutesFailClosedWhenTokenUnset(t *testing.T) {
	r := NewRouter(Deps{Logger: zap.NewNop()})
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/internal/devices", ""},
		{http.MethodPost, "/v1/leads/resolve", `{"lead_ids":[]}`},
		{http.MethodPost, "/internal/contacts/upsert", `{"name":"Ada","phone":"+1"}`},
		{http.MethodGet, "/internal/contacts?phone=%2B1", ""},
		{http.MethodPost, "/internal/bookings/11111111-1111-1111-1111-111111111111/crm-note", `{"text":"x"}`},
		{http.MethodGet, "/internal/campaigns/11111111-1111-1111-1111-111111111111/spend-sum", ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s with unset token = %d, want 503 (fail-closed)", tc.method, tc.path, rec.Code)
		}
	}
}
