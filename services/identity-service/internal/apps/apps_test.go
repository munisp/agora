package apps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/identity-service/internal/store"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeRepo struct {
	mu      sync.Mutex
	catalog []PlatformApp
	rows    map[string]TenantApp // key tenantID|appID
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		catalog: []PlatformApp{
			{AppID: "cac", Name: "Customer Acquisition", Version: "1.0.0", DefaultPlanTier: "standard"},
			{AppID: "helpdesk", Name: "Helpdesk", Version: "0.1.0", DefaultPlanTier: "pro"},
			{AppID: "receptionist", Name: "AI Receptionist", Version: "1.0.0", DefaultPlanTier: "free"},
		},
		rows: map[string]TenantApp{},
	}
}

func key(tenantID uuid.UUID, appID string) string { return tenantID.String() + "|" + appID }

func (f *fakeRepo) ListCatalog(context.Context) ([]PlatformApp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]PlatformApp, len(f.catalog))
	copy(out, f.catalog)
	return out, nil
}

func (f *fakeRepo) GetApp(_ context.Context, appID string) (PlatformApp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.catalog {
		if a.AppID == appID {
			return a, nil
		}
	}
	return PlatformApp{}, ErrUnknownApp
}

func (f *fakeRepo) ListTenantApps(_ context.Context, tenantID uuid.UUID) ([]TenantAppView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []TenantAppView{}
	for _, a := range f.catalog {
		v := TenantAppView{PlatformApp: a, Status: StatusNotProvisioned, Config: []byte(`{}`)}
		if row, ok := f.rows[key(tenantID, a.AppID)]; ok {
			v.Status = row.Status
			v.Config = row.Config
			provAt, updAt := row.ProvisionedAt, row.UpdatedAt
			v.ProvisionedAt = &provAt
			v.UpdatedAt = &updAt
			v.ProvisionedBy = row.ProvisionedBy
		}
		out = append(out, v)
	}
	return out, nil
}

func (f *fakeRepo) Provision(_ context.Context, tenantID uuid.UUID, appID, actor string) (TenantApp, AppStatus, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(tenantID, appID)
	if row, ok := f.rows[k]; ok {
		prev := row.Status
		row.Status = StatusEnabled
		row.UpdatedAt = time.Now().UTC()
		f.rows[k] = row
		return row, prev, false, nil
	}
	row := TenantApp{
		TenantID: tenantID, AppID: appID, Status: StatusEnabled,
		Config: []byte(`{}`), ProvisionedAt: time.Now().UTC(), ProvisionedBy: actor,
		UpdatedAt: time.Now().UTC(),
	}
	f.rows[k] = row
	return row, "", true, nil
}

func (f *fakeRepo) Patch(_ context.Context, tenantID uuid.UUID, appID string, status *AppStatus, config []byte) (TenantApp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(tenantID, appID)
	row, ok := f.rows[k]
	if !ok {
		return TenantApp{}, ErrNotFound
	}
	if status != nil {
		row.Status = *status
	}
	if config != nil {
		row.Config = config
	}
	row.UpdatedAt = time.Now().UTC()
	f.rows[k] = row
	return row, nil
}

func (f *fakeRepo) Disable(_ context.Context, tenantID uuid.UUID, appID string) (TenantApp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(tenantID, appID)
	row, ok := f.rows[k]
	if !ok {
		return TenantApp{}, ErrNotFound
	}
	row.Status = StatusDisabled
	row.UpdatedAt = time.Now().UTC()
	f.rows[k] = row
	return row, nil
}

func (f *fakeRepo) GetTenantApp(_ context.Context, tenantID uuid.UUID, appID string) (TenantApp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[key(tenantID, appID)]
	if !ok {
		return TenantApp{}, ErrNotFound
	}
	return row, nil
}

func (f *fakeRepo) Ping(context.Context) error { return nil }

type fakeTenants struct{ bySlug map[string]uuid.UUID }

func (f fakeTenants) GetTenantBySlug(_ context.Context, slug string) (store.Tenant, error) {
	id, ok := f.bySlug[slug]
	if !ok {
		return store.Tenant{}, store.ErrNotFound
	}
	return store.Tenant{ID: id, Slug: slug}, nil
}

type fakeAuthz struct {
	allow bool
	err   error
}

func (f fakeAuthz) Check(context.Context, string, string, string, string) (bool, error) {
	return f.allow, f.err
}

type publishedEvent struct {
	topic string
	data  any
}

type fakePublisher struct {
	mu     sync.Mutex
	events []publishedEvent
	err    error
}

func (f *fakePublisher) PublishEvent(_ context.Context, _, topic string, data any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, publishedEvent{topic: topic, data: data})
	return f.err
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	repo *fakeRepo
	pub  *fakePublisher
	tid  uuid.UUID
	http http.Handler
}

func newHarness() *harness {
	repo := newFakeRepo()
	pub := &fakePublisher{}
	tid := uuid.New()
	h := &Handler{
		Repo:      repo,
		Tenants:   fakeTenants{bySlug: map[string]uuid.UUID{"acme": tid}},
		Authz:     fakeAuthz{allow: true},
		Publisher: &Publisher{Events: pub, PubSub: "pubsub-kafka", Topic: DefaultLifecycleTopic, Logger: zap.NewNop()},
		Logger:    zap.NewNop(),
	}
	r := chi.NewRouter()
	h.RegisterRoutes(r)
	return &harness{repo: repo, pub: pub, tid: tid, http: r}
}

// do issues a request; admin requests carry X-User-Id (the gateway-forwarded
// subject idiom used by twin.go).
func (h *harness) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.http.ServeHTTP(rec, req)
	return rec
}

var adminHdr = map[string]string{"X-User-Id": "u-admin"}

func (h *harness) provision(t *testing.T, appID string) *httptest.ResponseRecorder {
	t.Helper()
	return h.do(http.MethodPost, "/v1/tenants/acme/apps/"+appID, "", adminHdr)
}

// ---------------------------------------------------------------------------
// Catalog tests
// ---------------------------------------------------------------------------

func TestLoadCatalogEmbedded(t *testing.T) {
	apps, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	// Agent C's 16-row catalog (contract §1: 8 shipped + 8 enterprise).
	if len(apps) != 16 {
		t.Fatalf("catalog rows = %d, want 16", len(apps))
	}
	byID := map[string]PlatformApp{}
	for _, a := range apps {
		byID[a.AppID] = a
		if a.Name == "" || a.Version == "" {
			t.Errorf("app %q missing name/version", a.AppID)
		}
		switch a.DefaultPlanTier {
		case "free", "standard", "pro":
		default:
			t.Errorf("app %q tier %q not a billing-engine tier", a.AppID, a.DefaultPlanTier)
		}
	}
	for _, id := range []string{"receptionist", "messaging", "cac", "payments", "kyc-compliance",
		"analytics", "incidents", "geo-campaigns", "helpdesk", "field-service", "loyalty-wallet",
		"campaign-studio", "crm-360", "surveys-voc", "lending", "workforce"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("contract app %q missing from catalog", id)
		}
	}
	// Version convention: shipped 1.0.0, catalog-only 0.1.0.
	if byID["receptionist"].Version != "1.0.0" || byID["helpdesk"].Version != "0.1.0" {
		t.Errorf("version convention broken: receptionist=%s helpdesk=%s",
			byID["receptionist"].Version, byID["helpdesk"].Version)
	}
}

func TestListCatalogEndpoint(t *testing.T) {
	h := newHarness()
	rec := h.do(http.MethodGet, "/v1/apps", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var out struct {
		Apps []PlatformApp `json:"apps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Apps) != 3 {
		t.Errorf("apps = %d, want 3", len(out.Apps))
	}
}

// ---------------------------------------------------------------------------
// Provision tests
// ---------------------------------------------------------------------------

func TestProvisionCreatesAndPublishes(t *testing.T) {
	h := newHarness()
	rec := h.provision(t, "cac")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var row TenantApp
	if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row.Status != StatusEnabled || row.TenantID != h.tid || row.ProvisionedBy != "u-admin" {
		t.Errorf("row: %+v", row)
	}
	if len(h.pub.events) != 1 {
		t.Fatalf("events = %d, want 1 (AppProvisioned)", len(h.pub.events))
	}
	evt := h.pub.events[0]
	if evt.topic != DefaultLifecycleTopic {
		t.Errorf("topic = %q", evt.topic)
	}
	payload, _ := json.Marshal(evt.data)
	var ce map[string]any
	_ = json.Unmarshal(payload, &ce)
	if ce["type"] != ProvisionedEventType {
		t.Errorf("event type = %v", ce["type"])
	}
	data, _ := ce["data"].(map[string]any)
	if data["tenant_id"] != h.tid.String() || data["app_id"] != "cac" ||
		data["status"] != "enabled" || data["actor"] != "u-admin" || data["ts"] == nil {
		t.Errorf("event payload: %v", data)
	}
}

func TestProvisionIsIdempotent(t *testing.T) {
	h := newHarness()
	first := h.provision(t, "cac")
	second := h.provision(t, "cac")
	if second.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", second.Code)
	}
	var a, b TenantApp
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(second.Body.Bytes(), &b)
	if !a.ProvisionedAt.Equal(b.ProvisionedAt) {
		t.Errorf("replay must keep original provisioned_at")
	}
	if b.ProvisionedBy != a.ProvisionedBy {
		t.Errorf("replay must keep original provisioned_by")
	}
	// enabled->enabled replay publishes no further lifecycle event.
	if len(h.pub.events) != 1 {
		t.Errorf("events = %d, want 1 (no event on enabled replay)", len(h.pub.events))
	}
}

func TestProvisionReEnablePublishesStatusChanged(t *testing.T) {
	h := newHarness()
	h.provision(t, "cac")
	h.do(http.MethodDelete, "/v1/tenants/acme/apps/cac", "", adminHdr)
	rec := h.provision(t, "cac") // re-provision re-enables
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(h.pub.events) != 3 {
		t.Fatalf("events = %d, want 3 (provisioned, disabled, re-enabled)", len(h.pub.events))
	}
	payload, _ := json.Marshal(h.pub.events[2].data)
	var ce map[string]any
	_ = json.Unmarshal(payload, &ce)
	if ce["type"] != StatusChangedEventType {
		t.Errorf("re-enable event type = %v, want AppStatusChanged", ce["type"])
	}
}

func TestProvisionValidation(t *testing.T) {
	h := newHarness()
	// Unknown app -> 404.
	if rec := h.provision(t, "nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown app: status = %d, want 404", rec.Code)
	}
	// Unknown tenant -> 404.
	if rec := h.do(http.MethodPost, "/v1/tenants/ghost/apps/cac", "", adminHdr); rec.Code != http.StatusNotFound {
		t.Errorf("unknown tenant: status = %d, want 404", rec.Code)
	}
}

func TestMutationsRequireOwnerOrAdmin(t *testing.T) {
	h := newHarness()
	// No authenticated subject -> 401.
	if rec := h.do(http.MethodPost, "/v1/tenants/acme/apps/cac", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no subject: status = %d, want 401", rec.Code)
	}
	// JWT bearer sub works too (twin.go idiom).
	jwt := "Bearer eyJhbGciOiJub25lIn0." +
		base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"u-jwt"}`)) + ".sig"
	if rec := h.do(http.MethodPost, "/v1/tenants/acme/apps/cac", "",
		map[string]string{"Authorization": jwt}); rec.Code != http.StatusCreated {
		t.Errorf("bearer sub: status = %d, want 201, body %s", rec.Code, rec.Body)
	}

	// Authorization denied (staff/viewer) -> 403.
	deny := newHarness()
	denyHandler := &Handler{
		Repo: deny.repo, Tenants: fakeTenants{bySlug: map[string]uuid.UUID{"acme": deny.tid}},
		Authz: fakeAuthz{allow: false}, Publisher: nil, Logger: zap.NewNop(),
	}
	r := chi.NewRouter()
	denyHandler.RegisterRoutes(r)
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/acme/apps/helpdesk", nil)
	req.Header.Set("X-User-Id", "u-staff")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("denied subject: status = %d, want 403", rec.Code)
	}

	// Nil Publisher (Dapr unwired) must not break mutations.
	if rec := deny.do(http.MethodPost, "/v1/tenants/acme/apps/helpdesk", "", adminHdr); rec.Code != http.StatusCreated {
		t.Errorf("nil publisher provision: status = %d, want 201", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Tenant app list (LEFT JOIN) tests
// ---------------------------------------------------------------------------

func TestListTenantAppsLeftJoin(t *testing.T) {
	h := newHarness()
	h.provision(t, "cac")
	rec := h.do(http.MethodGet, "/v1/tenants/acme/apps", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Apps []TenantAppView `json:"apps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// Every catalog app appears, provisioned or not.
	if len(out.Apps) != 3 {
		t.Fatalf("apps = %d, want 3 (full catalog)", len(out.Apps))
	}
	byID := map[string]TenantAppView{}
	for _, v := range out.Apps {
		byID[v.AppID] = v
	}
	if byID["cac"].Status != StatusEnabled {
		t.Errorf("cac status = %q", byID["cac"].Status)
	}
	for _, id := range []string{"helpdesk", "receptionist"} {
		v := byID[id]
		if v.Status != StatusNotProvisioned {
			t.Errorf("%s status = %q, want not_provisioned", id, v.Status)
		}
		if string(v.Config) != "{}" {
			t.Errorf("%s config = %s, want {}", id, v.Config)
		}
		if v.ProvisionedAt != nil {
			t.Errorf("%s provisioned_at must be null", id)
		}
	}
}

// ---------------------------------------------------------------------------
// PATCH tests
// ---------------------------------------------------------------------------

func TestPatchPartialSemantics(t *testing.T) {
	h := newHarness()
	h.provision(t, "cac")

	// Patch config only: status untouched.
	rec := h.do(http.MethodPatch, "/v1/tenants/acme/apps/cac", `{"config":{"greeting":"hi"}}`, adminHdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("config patch: status = %d, body %s", rec.Code, rec.Body)
	}
	var row TenantApp
	_ = json.Unmarshal(rec.Body.Bytes(), &row)
	if row.Status != StatusEnabled {
		t.Errorf("config-only patch changed status to %q", row.Status)
	}
	if string(row.Config) != `{"greeting":"hi"}` {
		t.Errorf("config = %s", row.Config)
	}

	// Patch status only: config preserved.
	rec = h.do(http.MethodPatch, "/v1/tenants/acme/apps/cac", `{"status":"suspended"}`, adminHdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status patch: status = %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &row)
	if row.Status != StatusSuspended {
		t.Errorf("status = %q, want suspended", row.Status)
	}
	if string(row.Config) != `{"greeting":"hi"}` {
		t.Errorf("status-only patch must preserve config, got %s", row.Config)
	}

	// Status change published exactly once (enabled->suspended).
	if len(h.pub.events) != 2 {
		t.Fatalf("events = %d, want 2 (provisioned + status_changed)", len(h.pub.events))
	}
	payload, _ := json.Marshal(h.pub.events[1].data)
	var ce map[string]any
	_ = json.Unmarshal(payload, &ce)
	if ce["type"] != StatusChangedEventType {
		t.Errorf("patch event type = %v", ce["type"])
	}
	data, _ := ce["data"].(map[string]any)
	if data["status"] != "suspended" {
		t.Errorf("event status = %v", data["status"])
	}

	// Repeating the same status is not a change -> no new event.
	h.do(http.MethodPatch, "/v1/tenants/acme/apps/cac", `{"status":"suspended"}`, adminHdr)
	if len(h.pub.events) != 2 {
		t.Errorf("same-status patch published a spurious event")
	}
}

func TestPatchValidation(t *testing.T) {
	h := newHarness()
	h.provision(t, "cac")
	cases := []struct{ name, body string }{
		{"empty patch", `{}`},
		{"bad status", `{"status":"bogus"}`},
		{"not_provisioned not storable", `{"status":"not_provisioned"}`},
		{"config not object", `{"config":[1,2]}`},
		{"bad json", `{`},
	}
	for _, tc := range cases {
		if rec := h.do(http.MethodPatch, "/v1/tenants/acme/apps/cac", tc.body, adminHdr); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, rec.Code)
		}
	}
	// Patch of a never-provisioned app -> 404.
	if rec := h.do(http.MethodPatch, "/v1/tenants/acme/apps/helpdesk", `{"status":"disabled"}`, adminHdr); rec.Code != http.StatusNotFound {
		t.Errorf("unprovisioned patch: status = %d, want 404", rec.Code)
	}
	// Unknown app -> 404.
	if rec := h.do(http.MethodPatch, "/v1/tenants/acme/apps/nope", `{"status":"disabled"}`, adminHdr); rec.Code != http.StatusNotFound {
		t.Errorf("unknown app patch: status = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// DELETE (soft) tests
// ---------------------------------------------------------------------------

func TestDeleteSoftDisablesAndRetainsRow(t *testing.T) {
	h := newHarness()
	h.provision(t, "cac")
	rec := h.do(http.MethodDelete, "/v1/tenants/acme/apps/cac", "", adminHdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d", rec.Code)
	}
	var row TenantApp
	_ = json.Unmarshal(rec.Body.Bytes(), &row)
	if row.Status != StatusDisabled {
		t.Errorf("status = %q, want disabled", row.Status)
	}

	// Row retained: still visible in the LEFT JOIN list as disabled.
	rec = h.do(http.MethodGet, "/v1/tenants/acme/apps", "", nil)
	var out struct {
		Apps []TenantAppView `json:"apps"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	found := false
	for _, v := range out.Apps {
		if v.AppID == "cac" {
			found = true
			if v.Status != StatusDisabled || v.ProvisionedAt == nil {
				t.Errorf("retained row: %+v", v)
			}
		}
	}
	if !found {
		t.Errorf("soft-deleted row missing from tenant app list")
	}

	// Repeat DELETE is idempotent (still disabled, 200).
	if rec := h.do(http.MethodDelete, "/v1/tenants/acme/apps/cac", "", adminHdr); rec.Code != http.StatusOK {
		t.Errorf("repeat delete: status = %d, want 200", rec.Code)
	}
	// DELETE of a never-provisioned app -> 404.
	if rec := h.do(http.MethodDelete, "/v1/tenants/acme/apps/helpdesk", "", adminHdr); rec.Code != http.StatusNotFound {
		t.Errorf("unprovisioned delete: status = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Entitlement check tests
// ---------------------------------------------------------------------------

func entitle(h *harness, appID string) (int, map[string]any) {
	rec := h.do(http.MethodGet, "/internal/entitlements/check?app_id="+appID, "",
		map[string]string{"X-Tenant-Slug": "acme"})
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestEntitlementReasons(t *testing.T) {
	h := newHarness()

	// Not provisioned -> allowed:false reason:not_provisioned (200: callers
	// need the reason payload).
	code, out := entitle(h, "cac")
	if code != http.StatusOK || out["allowed"] != false || out["reason"] != ReasonNotProvisioned {
		t.Errorf("not provisioned: code=%d body=%v", code, out)
	}
	if out["app_id"] != "cac" {
		t.Errorf("app_id echo: %v", out)
	}

	// Enabled.
	h.provision(t, "cac")
	code, out = entitle(h, "cac")
	if code != http.StatusOK || out["allowed"] != true || out["reason"] != ReasonEnabled {
		t.Errorf("enabled: code=%d body=%v", code, out)
	}

	// Disabled.
	h.do(http.MethodDelete, "/v1/tenants/acme/apps/cac", "", adminHdr)
	code, out = entitle(h, "cac")
	if code != http.StatusOK || out["allowed"] != false || out["reason"] != ReasonDisabled {
		t.Errorf("disabled: code=%d body=%v", code, out)
	}

	// Suspended.
	h.do(http.MethodPatch, "/v1/tenants/acme/apps/cac", `{"status":"suspended"}`, adminHdr)
	code, out = entitle(h, "cac")
	if code != http.StatusOK || out["allowed"] != false || out["reason"] != ReasonSuspended {
		t.Errorf("suspended: code=%d body=%v", code, out)
	}
}

func TestEntitlementUnknownAppIs404(t *testing.T) {
	h := newHarness()
	code, out := entitle(h, "nope")
	if code != http.StatusNotFound {
		t.Fatalf("unknown app: code = %d, want 404", code)
	}
	if _, ok := out["error"]; !ok {
		t.Errorf("404 shape must be {error}, got %v", out)
	}
}

func TestEntitlementValidation(t *testing.T) {
	h := newHarness()
	// Missing app_id -> 400.
	if rec := h.do(http.MethodGet, "/internal/entitlements/check", "",
		map[string]string{"X-Tenant-Slug": "acme"}); rec.Code != http.StatusBadRequest {
		t.Errorf("missing app_id: status = %d, want 400", rec.Code)
	}
	// Missing tenant header -> 400.
	if rec := h.do(http.MethodGet, "/internal/entitlements/check?app_id=cac", "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("missing tenant header: status = %d, want 400", rec.Code)
	}
	// Unknown tenant slug -> 404.
	if rec := h.do(http.MethodGet, "/internal/entitlements/check?app_id=cac", "",
		map[string]string{"X-Tenant-Slug": "ghost"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown tenant: status = %d, want 404", rec.Code)
	}
	// X-Tenant-ID uuid header accepted.
	h.provision(t, "cac")
	rec := h.do(http.MethodGet, "/internal/entitlements/check?app_id=cac", "",
		map[string]string{"X-Tenant-ID": h.tid.String()})
	if rec.Code != http.StatusOK {
		t.Errorf("uuid header: status = %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Publisher tests
// ---------------------------------------------------------------------------

func TestPublisherPublishFailureDoesNotFailMutation(t *testing.T) {
	h := newHarness()
	h.pub.err = errors.New("dapr down")
	rec := h.provision(t, "cac")
	if rec.Code != http.StatusCreated {
		t.Errorf("publish failure must not fail provisioning: status = %d", rec.Code)
	}
	code, out := entitle(h, "cac")
	if code != http.StatusOK || out["allowed"] != true {
		t.Errorf("registry must hold despite publish failure: %d %v", code, out)
	}
}
