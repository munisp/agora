package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/opendesk/identity-service/internal/daprc"
	"github.com/opendesk/identity-service/internal/keycloak"
	"github.com/opendesk/identity-service/internal/store"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeStore struct {
	mu      sync.Mutex
	tenants map[string]store.Tenant
	members map[uuid.UUID][]store.Membership
	deleted []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{tenants: map[string]store.Tenant{}, members: map[uuid.UUID][]store.Membership{}}
}

func (f *fakeStore) addTenant(t store.Tenant) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	f.tenants[t.Slug] = t
}

func (f *fakeStore) Ping(context.Context) error { return nil }

func (f *fakeStore) GetTenantBySlug(_ context.Context, slug string) (store.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tenants[slug]
	if !ok {
		return store.Tenant{}, store.ErrNotFound
	}
	return t, nil
}

func (f *fakeStore) CreateTenant(_ context.Context, t *store.Tenant) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tenants[t.Slug]; ok {
		return store.ErrConflict
	}
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	f.tenants[t.Slug] = *t
	return nil
}

func (f *fakeStore) DeleteTenant(_ context.Context, slug string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tenants[slug]; !ok {
		return store.ErrNotFound
	}
	delete(f.tenants, slug)
	f.deleted = append(f.deleted, slug)
	return nil
}

func (f *fakeStore) MergeTerminology(_ context.Context, slug string, patch json.RawMessage) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tenants[slug]; !ok {
		return nil, store.ErrNotFound
	}
	return patch, nil
}

func (f *fakeStore) ListMembers(_ context.Context, tenantID uuid.UUID) ([]store.Membership, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.members[tenantID], nil
}

func (f *fakeStore) AddMember(_ context.Context, m store.Membership) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[m.TenantID] = append(f.members[m.TenantID], m)
	return nil
}

// fakePermify answers Check from a key set "permission|subject|resource".
type fakePermify struct {
	allowed map[string]bool
	err     error
}

func (f *fakePermify) Check(_ context.Context, tenantID, subject, permission, resource string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.allowed[permission+"|"+subject+"|"+resource], nil
}

func (f *fakePermify) CreateTenant(context.Context, string, string) error { return nil }

func (f *fakePermify) WriteRelationship(context.Context, string, string, string, string) error {
	return nil
}

type fakeKeycloak struct{ nextUser int }

func (f *fakeKeycloak) CreateTenantGroup(context.Context, string) (string, error) {
	return uuid.NewString(), nil
}

func (f *fakeKeycloak) CreateUser(_ context.Context, _ string, _ keycloak.CreateUserInput) (string, error) {
	f.nextUser++
	return fmt.Sprintf("user-%d", f.nextUser), nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	st      *fakeStore
	perm    *fakePermify
	http    http.Handler
	token   string
	tenID   uuid.UUID
	tenSlug string
}

func jwt(sub string, roles, slugs []string) string {
	payload, _ := json.Marshal(map[string]any{
		"sub":          sub,
		"tenant_slugs": slugs,
		"realm_access": map[string]any{"roles": roles},
	})
	return "Bearer x." + base64.RawURLEncoding.EncodeToString(payload) + ".y"
}

func newHarness(token string) *harness {
	st := newFakeStore()
	perm := &fakePermify{allowed: map[string]bool{}}
	tid := uuid.New()
	st.addTenant(store.Tenant{ID: tid, Slug: "acme", Name: "Acme"})
	h := &harness{st: st, perm: perm, token: token, tenID: tid, tenSlug: "acme"}
	h.http = NewRouter(h.deps())
	return h
}

// deps builds Deps with an unroutable Dapr client: publish/invoke fail fast
// and are logged (best-effort side effects), never failing the request.
func (h *harness) deps() Deps {
	return Deps{
		Store:         h.st,
		Keycloak:      &fakeKeycloak{},
		Permify:       h.perm,
		Dapr:          daprc.New("127.0.0.1", 1),
		Logger:        zap.NewNop(),
		InternalToken: h.token,
	}
}

func (h *harness) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.http.ServeHTTP(rec, req)
	return rec
}

// allowPermify grants permission to subject on the acme organization.
func (h *harness) allowPermify(permission, subject string) {
	h.perm.allowed[permission+"|user:"+subject+"|organization:"+h.tenID.String()] = true
}

// ---------------------------------------------------------------------------
// K2 internauth + W-I-2 internal delete
// ---------------------------------------------------------------------------

func TestInternalDeleteTenantAuthMatrix(t *testing.T) {
	h := newHarness("secret-token")

	// 401: missing token.
	rec := h.do(http.MethodDelete, "/internal/tenants/acme", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rec.Code)
	}
	// 401: wrong token.
	rec = h.do(http.MethodDelete, "/internal/tenants/acme", "",
		map[string]string{"X-Internal-Token": "wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", rec.Code)
	}
	// 404: valid token, unknown slug.
	rec = h.do(http.MethodDelete, "/internal/tenants/nope", "",
		map[string]string{"X-Internal-Token": "secret-token"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown slug: status = %d, want 404", rec.Code)
	}
	// 200: valid token deletes WITHOUT any Permify grant (platform actor).
	rec = h.do(http.MethodDelete, "/internal/tenants/acme", "",
		map[string]string{"X-Internal-Token": "secret-token"})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d, body %s", rec.Code, rec.Body)
	}
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["deleted"] != "acme" {
		t.Errorf("body = %v", out)
	}
	if len(h.st.deleted) != 1 || h.st.deleted[0] != "acme" {
		t.Errorf("store deletions = %v", h.st.deleted)
	}
}

func TestInternalEndpointsFailClosedWhenTokenUnset(t *testing.T) {
	h := newHarness("")
	for _, path := range []string{
		"/internal/tenants/acme/ensure-group",
		"/internal/tenants/acme/twin",
		"/internal/consents/check?subject=x&purpose=y",
	} {
		method := http.MethodPost
		if strings.HasPrefix(path, "/internal/consents") {
			method = http.MethodGet
		}
		if rec := h.do(method, path, "{}", nil); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503 (fail-closed)", path, rec.Code)
		}
	}
	// Non-internal paths are NOT gated by internauth.
	rec := h.do(http.MethodGet, "/healthz", "", nil)
	if rec.Code == http.StatusServiceUnavailable {
		t.Errorf("healthz must not be internauth-gated")
	}
}

func TestInternalTokenGatesTwinAndEnsureRoutes(t *testing.T) {
	h := newHarness("secret-token")
	// 401 without token on twin provisioning (S1-F7-06: was unguarded).
	rec := h.do(http.MethodPost, "/internal/tenants/acme/twin", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("twin without token: status = %d, want 401", rec.Code)
	}
	rec = h.do(http.MethodPost, "/internal/tenants/acme/twin", "",
		map[string]string{"X-Internal-Token": "secret-token"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("twin with token: status = %d, body %s", rec.Code, rec.Body)
	}
	var twin store.Tenant
	_ = json.Unmarshal(rec.Body.Bytes(), &twin)
	if !twin.IsTwin {
		t.Errorf("created twin must carry is_twin=true")
	}
	if !strings.Contains(twin.Slug, "-twin-") || twin.Plan != "twin" {
		t.Errorf("twin slug/plan: %+v", twin)
	}
}

// ---------------------------------------------------------------------------
// S1-F7-06: is_twin deletion guard (exact flag, not slug substring)
// ---------------------------------------------------------------------------

func TestDeleteTenantTwinGuardUsesFlagNotSlug(t *testing.T) {
	h := newHarness("secret-token")

	// Regression: a NON-twin tenant whose slug contains "-twin-" must NOT be
	// freely deletable (old strings.Contains bypass).
	h.st.addTenant(store.Tenant{Slug: "acme-twin-prod", Name: "Real"})
	rec := h.do(http.MethodDelete, "/v1/tenants/acme-twin-prod", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("slug-marker non-twin delete without auth: status = %d, want 401", rec.Code)
	}
	if len(h.st.deleted) != 0 {
		t.Fatalf("non-twin tenant was deleted: %v", h.st.deleted)
	}

	// A real twin (is_twin=true) deletes freely.
	h.st.addTenant(store.Tenant{Slug: "acme-twin-abc123", Name: "Twin", Plan: "twin", IsTwin: true})
	rec = h.do(http.MethodDelete, "/v1/tenants/acme-twin-abc123", "", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("twin delete: status = %d, body %s", rec.Code, rec.Body)
	}
}

func TestDeleteTenantNonTwinAuthz(t *testing.T) {
	h := newHarness("secret-token")

	// 401: no subject.
	if rec := h.do(http.MethodDelete, "/v1/tenants/acme", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: status = %d, want 401", rec.Code)
	}
	// 401: malformed presented bearer token (error-closed).
	if rec := h.do(http.MethodDelete, "/v1/tenants/acme", "",
		map[string]string{"Authorization": "Bearer not-a-jwt"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("malformed token: status = %d, want 401", rec.Code)
	}
	// 403: authenticated but no manage_catalog.
	if rec := h.do(http.MethodDelete, "/v1/tenants/acme", "",
		map[string]string{"Authorization": jwt("u-member", nil, []string{"acme"})}); rec.Code != http.StatusForbidden {
		t.Errorf("member: status = %d, want 403", rec.Code)
	}
	// 200: manage_catalog grant.
	h.allowPermify("manage_catalog", "u-owner")
	rec := h.do(http.MethodDelete, "/v1/tenants/acme", "",
		map[string]string{"Authorization": jwt("u-owner", nil, nil)})
	if rec.Code != http.StatusOK {
		t.Errorf("owner: status = %d, body %s", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------------------
// S1-F7-02: createTenant / inviteMember / listMembers authz (KC-1)
// ---------------------------------------------------------------------------

func TestCreateTenantRequiresAuth(t *testing.T) {
	h := newHarness("secret-token")
	body := `{"slug":"newco","name":"New Co"}`
	if rec := h.do(http.MethodPost, "/v1/tenants", body, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous create: status = %d, want 401", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/v1/tenants", body,
		map[string]string{"Authorization": "Bearer garbage"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("malformed token: status = %d, want 401", rec.Code)
	}
}

func TestCreateTenantPlanForcedFree(t *testing.T) {
	h := newHarness("secret-token")
	// Non-admin asking for enterprise gets free (tenant-takeover chain:
	// self-assigned paid plans are gone).
	rec := h.do(http.MethodPost, "/v1/tenants",
		`{"slug":"newco","name":"New Co","plan":"enterprise","owner_user_id":"attacker"}`,
		map[string]string{"X-User-Id": "u-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var out store.Tenant
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Plan != "free" {
		t.Errorf("plan = %q, want free", out.Plan)
	}

	// Platform admin (realm role) may set a paid plan.
	rec = h.do(http.MethodPost, "/v1/tenants",
		`{"slug":"bigco","name":"Big Co","plan":"pro"}`,
		map[string]string{"Authorization": jwt("admin-1", []string{"platform-admin"}, nil)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin create: status = %d, body %s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Plan != "pro" {
		t.Errorf("admin plan = %q, want pro", out.Plan)
	}

	// Bogus plan even for admins: 400 (DB CHECK parity).
	rec = h.do(http.MethodPost, "/v1/tenants",
		`{"slug":"badplan","name":"Bad","plan":"gold"}`,
		map[string]string{"Authorization": jwt("admin-1", []string{"platform-admin"}, nil)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid plan: status = %d, want 400", rec.Code)
	}
}

func TestCreateTenantPlatformAdminAllowlist(t *testing.T) {
	h := newHarness("secret-token")
	rec := h.do(http.MethodPost, "/v1/tenants",
		`{"slug":"envco","name":"Env Co","plan":"pro"}`,
		map[string]string{"X-User-Id": "u-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	var out store.Tenant
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Plan != "free" {
		t.Errorf("non-allowlisted subject must get free, got %q", out.Plan)
	}
	// Allowlisted subject (OPENDESK_PLATFORM_ADMINS) gets the paid plan.
	d := h.deps()
	d.PlatformAdmins = []string{"boss"}
	h.http = NewRouter(d)
	rec = h.do(http.MethodPost, "/v1/tenants",
		`{"slug":"envco2","name":"Env Co 2","plan":"enterprise"}`,
		map[string]string{"X-User-Id": "boss"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("allowlist create: status = %d, body %s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Plan != "enterprise" {
		t.Errorf("allowlisted plan = %q, want enterprise", out.Plan)
	}
}

func TestInviteMemberAuthzMatrix(t *testing.T) {
	h := newHarness("secret-token")
	body := `{"email":"x@example.com","role":"staff"}`

	// 401: anonymous.
	if rec := h.do(http.MethodPost, "/v1/tenants/acme/members", body, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: status = %d, want 401", rec.Code)
	}
	// 403: plain member (no manage_catalog).
	if rec := h.do(http.MethodPost, "/v1/tenants/acme/members", body,
		map[string]string{"Authorization": jwt("u-member", nil, []string{"acme"})}); rec.Code != http.StatusForbidden {
		t.Errorf("member: status = %d, want 403", rec.Code)
	}
	// 201: manage_catalog grant (owner/admin).
	h.allowPermify("manage_catalog", "u-admin")
	rec := h.do(http.MethodPost, "/v1/tenants/acme/members", body,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin invite: status = %d, body %s", rec.Code, rec.Body)
	}

	// Owner-role invite by an admin (no owner relation): 403 — an admin
	// cannot mint new owners (KC-1).
	rec = h.do(http.MethodPost, "/v1/tenants/acme/members",
		`{"email":"y@example.com","role":"owner"}`,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)})
	if rec.Code != http.StatusForbidden {
		t.Errorf("admin owner-invite: status = %d, want 403", rec.Code)
	}

	// Owner-role invite by an owner: 201.
	h.allowPermify("manage_catalog", "u-owner")
	h.allowPermify("owner", "u-owner")
	rec = h.do(http.MethodPost, "/v1/tenants/acme/members",
		`{"email":"y@example.com","role":"owner"}`,
		map[string]string{"Authorization": jwt("u-owner", nil, nil)})
	if rec.Code != http.StatusCreated {
		t.Errorf("owner owner-invite: status = %d, body %s", rec.Code, rec.Body)
	}
}

func TestListMembersTenantScoped(t *testing.T) {
	h := newHarness("secret-token")

	// 401: anonymous (was realm-wide open — S1-F7-09).
	if rec := h.do(http.MethodGet, "/v1/tenants/acme/members", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: status = %d, want 401", rec.Code)
	}
	// 403: authenticated but bound to a DIFFERENT tenant (cross-tenant).
	if rec := h.do(http.MethodGet, "/v1/tenants/acme/members", "",
		map[string]string{"Authorization": jwt("u-other", nil, []string{"other-co"})}); rec.Code != http.StatusForbidden {
		t.Errorf("cross-tenant: status = %d, want 403", rec.Code)
	}
	// 200: slug in tenant_slugs (K1 binding fast path).
	rec := h.do(http.MethodGet, "/v1/tenants/acme/members", "",
		map[string]string{"Authorization": jwt("u-member", nil, []string{"acme"})})
	if rec.Code != http.StatusOK {
		t.Errorf("bound member: status = %d, body %s", rec.Code, rec.Body)
	}
	// 200: X-Tenant-Slugs gateway header binding (K1).
	rec = h.do(http.MethodGet, "/v1/tenants/acme/members", "",
		map[string]string{"X-User-Id": "u-member", "X-Tenant-Slugs": "acme"})
	if rec.Code != http.StatusOK {
		t.Errorf("header-bound member: status = %d, body %s", rec.Code, rec.Body)
	}
	// 200: Permify view_dashboard grant fallback.
	h.allowPermify("view_dashboard", "u-viewer")
	rec = h.do(http.MethodGet, "/v1/tenants/acme/members", "",
		map[string]string{"X-User-Id": "u-viewer"})
	if rec.Code != http.StatusOK {
		t.Errorf("permify member: status = %d, body %s", rec.Code, rec.Body)
	}
}

func TestGetTenantScoped(t *testing.T) {
	h := newHarness("secret-token")

	// 401: anonymous without internal token (was realm-wide open — S1-F7-09).
	if rec := h.do(http.MethodGet, "/v1/tenants/acme", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: status = %d, want 401", rec.Code)
	}
	// 200: service-to-service with the internal token (booking resolver,
	// conversation/analytics session injection path).
	rec := h.do(http.MethodGet, "/v1/tenants/acme", "",
		map[string]string{"X-Internal-Token": "secret-token"})
	if rec.Code != http.StatusOK {
		t.Errorf("internal caller: status = %d, body %s", rec.Code, rec.Body)
	}
	// 403: subject bound to another tenant.
	if rec := h.do(http.MethodGet, "/v1/tenants/acme", "",
		map[string]string{"Authorization": jwt("u-other", nil, []string{"other-co"})}); rec.Code != http.StatusForbidden {
		t.Errorf("cross-tenant: status = %d, want 403", rec.Code)
	}
	// 200: bound subject.
	rec = h.do(http.MethodGet, "/v1/tenants/acme", "",
		map[string]string{"Authorization": jwt("u-member", nil, []string{"acme"})})
	if rec.Code != http.StatusOK {
		t.Errorf("bound member: status = %d, body %s", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------------------
// W-I-5: /metrics
// ---------------------------------------------------------------------------

func TestMetricsEndpoint(t *testing.T) {
	h := newHarness("secret-token")
	rec := h.do(http.MethodGet, "/metrics", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "go_goroutines") {
		t.Errorf("metrics body missing go_goroutines")
	}
}

// ---------------------------------------------------------------------------
// misc auth helpers
// ---------------------------------------------------------------------------

func TestResolveCallerMalformedIsErrorClosed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer bad")
	if _, err := resolveCaller(req); !errors.Is(err, errMalformedToken) {
		t.Errorf("err = %v, want errMalformedToken", err)
	}
	// Header fallback union: X-User-Roles/X-Tenant-Slugs are honored (K1).
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-User-Id", "u-1")
	req.Header.Set("X-User-Roles", "platform-admin, owner")
	req.Header.Set("X-Tenant-Slugs", "acme, other")
	c, err := resolveCaller(req)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "u-1" || !c.HasRole("platform-admin") || !c.HasSlug("other") {
		t.Errorf("caller = %+v", c)
	}
}
