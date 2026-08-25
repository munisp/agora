package httpapi

// SPEC-W44 K3/F15-04 tests: GET /v1/ops-alerts role gate + binding + limit.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opendesk/notification-worker/internal/store"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type fakeOpsAlertStore struct {
	alerts   []store.OpsAlert
	gotLimit int
	gotTen   string
}

func (f *fakeOpsAlertStore) ListOpsAlerts(_ context.Context, tenantID string, limit int) ([]store.OpsAlert, error) {
	f.gotTen, f.gotLimit = tenantID, limit
	return f.alerts, nil
}

func TestOpsAlertsHandler(t *testing.T) {
	st := &fakeOpsAlertStore{alerts: []store.OpsAlert{{EventID: "evt-1", Severity: "critical"}}}
	srv := httptest.NewServer(NewRouter(&Server{Log: zap.NewNop(), OpsAlerts: st}))
	defer srv.Close()

	get := func(headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/ops-alerts?limit=900", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		srv.Config.Handler.ServeHTTP(rec, req)
		return rec
	}

	// Role gate (DND_ADMIN_ROLES): no roles → 403.
	require.Equal(t, http.StatusForbidden, get(nil).Code)

	// platform-admin → 200, limit clamped to 500.
	rec := get(map[string]string{"X-User-Roles": "platform-admin"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 500, st.gotLimit)
	var body struct {
		Alerts []store.OpsAlert `json:"alerts"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	require.Len(t, body.Alerts, 1)
	require.Equal(t, "evt-1", body.Alerts[0].EventID)
}

func TestOpsAlertsTenantBinding(t *testing.T) {
	st := &fakeOpsAlertStore{}
	s := &Server{Log: zap.NewNop(), OpsAlerts: st}
	h := NewRouter(s)

	// Unbound tenant filter → 403.
	req := httptest.NewRequest(http.MethodGet, "/v1/ops-alerts?tenant=acme", nil)
	req.Header.Set("X-User-Roles", "platform-admin")
	req.Header.Set("X-Tenant-Slugs", "other")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

	// Bound tenant filter → 200 and forwarded to the store.
	req = httptest.NewRequest(http.MethodGet, "/v1/ops-alerts?tenant=acme", nil)
	req.Header.Set("X-User-Roles", "platform-admin")
	req.Header.Set("X-Tenant-Slugs", "acme")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "acme", st.gotTen)

	// Bad limit → 400.
	req = httptest.NewRequest(http.MethodGet, "/v1/ops-alerts?limit=abc", nil)
	req.Header.Set("X-User-Roles", "platform-admin")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestOpsAlertsUnavailable(t *testing.T) {
	h := NewRouter(&Server{Log: zap.NewNop()}) // OpsAlerts nil
	req := httptest.NewRequest(http.MethodGet, "/v1/ops-alerts", nil)
	req.Header.Set("X-User-Roles", "platform-admin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
