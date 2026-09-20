package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// kcMock records the admin calls the client makes and answers the token
// endpoint. Paths are matched on suffix for brevity.
type kcMock struct {
	mu     sync.Mutex
	calls  []string // "METHOD path"
	bodies map[string]string
	groups map[string]string // path -> id
	roles  map[string]string // role name -> id
	realm  map[string]any
}

func newKcMock() *kcMock {
	return &kcMock{
		bodies: map[string]string{},
		groups: map[string]string{"/tenants/acme": "grp-1"},
		roles:  map[string]string{"admin": "role-admin", "staff": "role-staff"},
		realm:  map[string]any{"realm": "opendesk", "smtpServer": map[string]any{"host": "old", "ssl": "true"}},
	}
}

func (m *kcMock) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300}`))
			return
		}
		key := r.Method + " " + r.URL.Path
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		m.mu.Lock()
		m.calls = append(m.calls, key)
		m.bodies[key] = string(body)
		m.mu.Unlock()

		switch {
		case strings.Contains(r.URL.Path, "/group-by-path/"):
			path := r.URL.Path[strings.Index(r.URL.Path, "/group-by-path/")+len("/group-by-path/"):]
			m.mu.Lock()
			id, ok := m.groups[path]
			m.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
		case strings.HasSuffix(r.URL.Path, "/execute-actions-email"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/logout"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/role-mappings/realm"):
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(r.URL.Path, "/roles/") && r.Method == http.MethodGet:
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/roles/")+len("/roles/"):]
			m.mu.Lock()
			id, ok := m.roles[name]
			m.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "name": name})
		case strings.Contains(r.URL.Path, "/groups/") && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(r.URL.Path, "/users/") && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/admin/realms/opendesk"):
			w.Header().Set("Content-Type", "application/json")
			m.mu.Lock()
			_ = json.NewEncoder(w).Encode(m.realm)
			m.mu.Unlock()
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/admin/realms/opendesk"):
			var rep map[string]any
			_ = json.Unmarshal(body, &rep)
			m.mu.Lock()
			m.realm = rep
			m.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (m *kcMock) called(substr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func newTestClient(t *testing.T) (*Client, *kcMock, func()) {
	t.Helper()
	m := newKcMock()
	srv := httptest.NewServer(m.handler())
	return New(srv.URL, "opendesk", "svc", "secret"), m, srv.Close
}

func TestDisableUserAndLogout(t *testing.T) {
	c, m, done := newTestClient(t)
	defer done()
	ctx := context.Background()
	if err := c.DisableUser(ctx, "u-1"); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	if body := m.bodies["PUT /admin/realms/opendesk/users/u-1"]; !strings.Contains(body, `"enabled":false`) {
		t.Errorf("disable body = %q", body)
	}
	if err := c.LogoutUserSessions(ctx, "u-1"); err != nil {
		t.Fatalf("LogoutUserSessions: %v", err)
	}
	if !m.called("POST /admin/realms/opendesk/users/u-1/logout") {
		t.Errorf("logout call missing: %v", m.calls)
	}
}

func TestRealmRoleMappings(t *testing.T) {
	c, m, done := newTestClient(t)
	defer done()
	ctx := context.Background()
	if err := c.AssignRealmRole(ctx, "u-1", "admin"); err != nil {
		t.Fatalf("AssignRealmRole: %v", err)
	}
	if body := m.bodies["POST /admin/realms/opendesk/users/u-1/role-mappings/realm"]; !strings.Contains(body, "role-admin") {
		t.Errorf("assign body = %q", body)
	}
	if err := c.RemoveRealmRole(ctx, "u-1", "staff"); err != nil {
		t.Fatalf("RemoveRealmRole: %v", err)
	}
	if !m.called("DELETE /admin/realms/opendesk/users/u-1/role-mappings/realm") {
		t.Errorf("remove call missing: %v", m.calls)
	}
	// Unknown role: typed ErrRoleNotFound (fail-soft for callers).
	err := c.AssignRealmRole(ctx, "u-1", "wizard")
	if !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("unknown role: err = %v, want ErrRoleNotFound", err)
	}
}

func TestSendExecuteActionsEmail(t *testing.T) {
	c, m, done := newTestClient(t)
	defer done()
	if err := c.SendExecuteActionsEmail(context.Background(), "u-9",
		[]string{"UPDATE_PASSWORD", "VERIFY_EMAIL"}); err != nil {
		t.Fatalf("SendExecuteActionsEmail: %v", err)
	}
	body := m.bodies["PUT /admin/realms/opendesk/users/u-9/execute-actions-email"]
	if !strings.Contains(body, "UPDATE_PASSWORD") || !strings.Contains(body, "VERIFY_EMAIL") {
		t.Errorf("actions body = %q", body)
	}
}

func TestDeleteTenantGroupIdempotent(t *testing.T) {
	c, m, done := newTestClient(t)
	defer done()
	ctx := context.Background()
	if err := c.DeleteTenantGroup(ctx, "acme"); err != nil {
		t.Fatalf("DeleteTenantGroup: %v", err)
	}
	if !m.called("DELETE /admin/realms/opendesk/groups/grp-1") {
		t.Errorf("group delete missing: %v", m.calls)
	}
	// Absent group: no-op, no error (idempotent cascade).
	if err := c.DeleteTenantGroup(ctx, "ghost"); err != nil {
		t.Errorf("absent group must be a no-op: %v", err)
	}
}

func TestApplyRealmSMTPMergesExistingRealm(t *testing.T) {
	c, m, done := newTestClient(t)
	defer done()
	err := c.ApplyRealmSMTP(context.Background(), RealmSMTPConfig{
		Host: "smtp", Port: "587", From: "noreply@x.dev", User: "u", Password: "p",
	})
	if err != nil {
		t.Fatalf("ApplyRealmSMTP: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	smtp, _ := m.realm["smtpServer"].(map[string]any)
	if smtp["host"] != "smtp" || smtp["port"] != "587" || smtp["from"] != "noreply@x.dev" ||
		smtp["user"] != "u" || smtp["password"] != "p" || smtp["auth"] != "true" {
		t.Errorf("smtpServer = %v", smtp)
	}
	// Pre-existing realm + smtp attributes survive the merge.
	if m.realm["realm"] != "opendesk" || smtp["ssl"] != "true" {
		t.Errorf("realm rep lost attributes: %v / %v", m.realm["realm"], smtp)
	}
}

func TestCreateUserConflictIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300}`))
			return
		}
		w.WriteHeader(http.StatusConflict) // user exists
	}))
	defer srv.Close()
	c := New(srv.URL, "opendesk", "svc", "secret")
	_, err := c.CreateUser(context.Background(), "acme", CreateUserInput{Email: "x@y.z"})
	if !errors.Is(err, ErrUserExists) {
		t.Fatalf("err = %v, want ErrUserExists", err)
	}
}
