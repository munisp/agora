package httpapi

// S1-F7-03 tests: DND mutations are gated on X-User-Roles ∩
// DND_ADMIN_ROLES; the tenant-scoped delete binds its slug to
// X-Tenant-Slugs (K1/C1).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func dndAuthServer(st DNDStore, mutate func(*Server)) *httptest.Server {
	s := &Server{Log: zap.NewNop(), DND: st}
	if mutate != nil {
		mutate(s)
	}
	return httptest.NewServer(NewRouter(s))
}

func dndReq(t *testing.T, srv *httptest.Server, method, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestDNDImportRoleGate(t *testing.T) {
	st := newFakeDNDStore()
	srv := dndAuthServer(st, nil)
	defer srv.Close()

	// No roles header → 403 (fail-closed, K1).
	resp := dndReq(t, srv, http.MethodPost, "/v1/dnd/import", `{"phones":["+234801"]}`, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	// Wrong role → 403.
	resp = dndReq(t, srv, http.MethodPost, "/v1/dnd/import", `{"phones":["+234801"]}`,
		map[string]string{"X-User-Roles": "staff"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	// platform-admin (default DND_ADMIN_ROLES) → 200.
	resp = dndReq(t, srv, http.MethodPost, "/v1/dnd/import", `{"phones":["+234801"]}`,
		map[string]string{"X-User-Roles": "staff,platform-admin"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

func TestDNDImportCustomRolesEnv(t *testing.T) {
	st := newFakeDNDStore()
	srv := dndAuthServer(st, func(s *Server) { s.DNDAdminRoles = []string{"dnd-ops"} })
	defer srv.Close()

	resp := dndReq(t, srv, http.MethodPost, "/v1/dnd/import", `{"phones":["+234801"]}`,
		map[string]string{"X-User-Roles": "platform-admin"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "env override replaces the default role set")
	resp.Body.Close()

	resp = dndReq(t, srv, http.MethodPost, "/v1/dnd/import", `{"phones":["+234801"]}`,
		map[string]string{"X-User-Roles": "dnd-ops"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

func TestDNDDeleteGates(t *testing.T) {
	st := newFakeDNDStore()
	st.tenant["acme"] = map[string]bool{"+234801": true}
	srv := dndAuthServer(st, nil)
	defer srv.Close()

	// Global delete without an admin role → 403.
	resp := dndReq(t, srv, http.MethodDelete, "/v1/dnd/+234801", "", nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	// Global delete with the admin role → 200 (removes global + all tenant
	// rows — a full re-consent).
	resp = dndReq(t, srv, http.MethodDelete, "/v1/dnd/+234801", "",
		map[string]string{"X-User-Roles": "platform-admin"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Re-add the tenant row for the scoped-delete cases below.
	st.tenant["acme"]["+234801"] = true

	// Tenant-scoped delete: slug not in the verified claim → 403.
	resp = dndReq(t, srv, http.MethodDelete, "/v1/dnd/+234801?tenant=acme", "",
		map[string]string{"X-Tenant-Slugs": "other"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
	require.True(t, st.tenant["acme"]["+234801"], "rejected delete must not mutate")

	// Tenant-scoped delete: bound slug → 200 without any admin role.
	resp = dndReq(t, srv, http.MethodDelete, "/v1/dnd/+234801?tenant=acme", "",
		map[string]string{"X-Tenant-Slugs": "acme"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	require.False(t, st.tenant["acme"]["+234801"])
}

func TestDNDRoleGateDevEscape(t *testing.T) {
	st := newFakeDNDStore()
	srv := dndAuthServer(st, func(s *Server) { s.TrustDirectTenancy = true })
	defer srv.Close()
	// OPENDESK_TRUST_DIRECT_TENANT=1: no gateway headers at all → allowed.
	resp := dndReq(t, srv, http.MethodPost, "/v1/dnd/import", `{"phones":["+1"]}`, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}
