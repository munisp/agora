package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/civic"
	"github.com/opendesk/booking-service/internal/incidents"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// In-memory civic store fake (httpapi flavor of the civic package fake)
// ---------------------------------------------------------------------------

type httpFakeCivicStore struct {
	mu         sync.Mutex
	categories map[uuid.UUID]store.CivicCategory
	rules      []store.CivicRoutingRule
	cases      map[uuid.UUID]*store.CivicCase
	refSeq     int64
}

func newHTTPFakeCivicStore(tenantID uuid.UUID) *httpFakeCivicStore {
	f := &httpFakeCivicStore{categories: map[uuid.UUID]store.CivicCategory{}, cases: map[uuid.UUID]*store.CivicCase{}}
	catID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	f.categories[catID] = store.CivicCategory{
		ID: catID, TenantID: tenantID,
		Name: "Roads", Slug: "roads", MDAQueue: "mda-works", AckSLAHours: 2, ResolveSLAHours: 49, Active: true,
	}
	return f
}

func (f *httpFakeCivicStore) roadsCategory(tenantID uuid.UUID) store.CivicCategory {
	for _, c := range f.categories {
		if c.TenantID == tenantID && c.Slug == "roads" {
			return c
		}
	}
	return store.CivicCategory{}
}

func (f *httpFakeCivicStore) ListCivicCategories(_ context.Context, tenantID uuid.UUID, activeOnly bool) ([]store.CivicCategory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.CivicCategory{}
	for _, c := range f.categories {
		if c.TenantID == tenantID && (!activeOnly || c.Active) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *httpFakeCivicStore) GetCivicCategoryBySlug(_ context.Context, tenantID uuid.UUID, slug string) (store.CivicCategory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.categories {
		if c.TenantID == tenantID && c.Slug == slug {
			return c, nil
		}
	}
	return store.CivicCategory{}, store.ErrNotFound
}

func (f *httpFakeCivicStore) GetCivicCategory(_ context.Context, tenantID, id uuid.UUID) (store.CivicCategory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.categories[id]
	if !ok || c.TenantID != tenantID {
		return store.CivicCategory{}, store.ErrNotFound
	}
	return c, nil
}

func (f *httpFakeCivicStore) CreateCivicCategory(_ context.Context, c *store.CivicCategory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	f.categories[c.ID] = *c
	return nil
}

func (f *httpFakeCivicStore) UpdateCivicCategory(_ context.Context, c *store.CivicCategory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.categories[c.ID]; !ok {
		return store.ErrNotFound
	}
	f.categories[c.ID] = *c
	return nil
}

func (f *httpFakeCivicStore) ListCivicRoutingRules(_ context.Context, tenantID uuid.UUID) ([]store.CivicRoutingRule, error) {
	return []store.CivicRoutingRule{}, nil
}

func (f *httpFakeCivicStore) CreateCivicRoutingRule(_ context.Context, r *store.CivicRoutingRule) error {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	f.rules = append(f.rules, *r)
	return nil
}

func (f *httpFakeCivicStore) UpdateCivicRoutingRule(_ context.Context, r *store.CivicRoutingRule) error {
	for i := range f.rules {
		if f.rules[i].ID == r.ID {
			f.rules[i] = *r
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *httpFakeCivicStore) DeleteCivicRoutingRule(_ context.Context, tenantID, id uuid.UUID) error {
	for i := range f.rules {
		if f.rules[i].ID == id {
			f.rules = append(f.rules[:i], f.rules[i+1:]...)
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *httpFakeCivicStore) NextCivicRefSeq(_ context.Context, tenantID uuid.UUID, year int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refSeq++
	return f.refSeq, nil
}

func (f *httpFakeCivicStore) InsertCivicCase(_ context.Context, c *store.CivicCase) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	c.CreatedAt, c.UpdatedAt = now, now
	cp := *c
	f.cases[c.ID] = &cp
	return nil
}

func (f *httpFakeCivicStore) GetCivicCase(_ context.Context, tenantID, id uuid.UUID) (store.CivicCase, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cases[id]
	if !ok || c.TenantID != tenantID {
		return store.CivicCase{}, store.ErrNotFound
	}
	return *c, nil
}

func (f *httpFakeCivicStore) GetCivicCaseByRef(_ context.Context, tenantID uuid.UUID, ref string) (store.CivicCase, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cases {
		if c.TenantID == tenantID && c.Ref == ref {
			return *c, nil
		}
	}
	return store.CivicCase{}, store.ErrNotFound
}

func (f *httpFakeCivicStore) ListCivicCases(_ context.Context, tenantID uuid.UUID, filter store.CivicCaseFilter) ([]store.CivicCase, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.CivicCase{}
	for _, c := range f.cases {
		if c.TenantID == tenantID {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (f *httpFakeCivicStore) SaveCivicCase(_ context.Context, c *store.CivicCase) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.cases[c.ID]; !ok {
		return store.ErrNotFound
	}
	cp := *c
	f.cases[c.ID] = &cp
	return nil
}

func (f *httpFakeCivicStore) NextCivicEventSeq(_ context.Context, tenantID, caseID uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cases[caseID]
	c.EventSeq++
	return c.EventSeq, nil
}

func (f *httpFakeCivicStore) CivicCaseStats(_ context.Context, tenantID uuid.UUID) (store.CivicStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stats := store.CivicStats{ByCategory: []store.CivicStatRow{}, ByWard: []store.CivicStatRow{}}
	for _, c := range f.cases {
		if c.TenantID != tenantID || c.MergedInto != nil {
			continue
		}
		if c.Status == store.CivicStatusResolved || c.Status == store.CivicStatusClosed {
			stats.Resolved++
		} else {
			stats.Open++
		}
	}
	return stats, nil
}

func (f *httpFakeCivicStore) DuplicateCivicCaseCandidates(_ context.Context, tenantID, categoryID, excludeID uuid.UUID, at time.Time) ([]store.CivicCase, error) {
	return []store.CivicCase{}, nil
}

func (f *httpFakeCivicStore) EnqueueOutbox(_ context.Context, aggregateID uuid.UUID, topic string, payload []byte) error {
	return nil
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

type civicHTTPFixture struct {
	router   http.Handler
	srv      *server
	svc      *civic.Service
	fake     *httpFakeCivicStore
	tenant   bookingops.TenantInfo
	reportAt func(body string) *httptest.ResponseRecorder
}

func newCivicHTTPFixture(t *testing.T) *civicHTTPFixture {
	t.Helper()
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "ikeja-lga", Name: "Ikeja LGA"}
	fake := newHTTPFakeCivicStore(tenant.ID)
	svc := &civic.Service{Store: fake, EventsTopic: civic.TopicCivicEvents, RatePerHour: 100, RatePerDay: 500}
	d := Deps{
		Logger: zap.NewNop(),
		Civic:  svc,
		TenantBySlug: func(_ context.Context, slug string) (bookingops.TenantInfo, error) {
			if slug == tenant.Slug {
				return tenant, nil
			}
			return bookingops.TenantInfo{}, errors.New("no such tenant")
		},
	}
	return &civicHTTPFixture{
		router: NewRouter(d),
		srv:    &server{d: d},
		svc:    svc,
		fake:   fake,
		tenant: tenant,
	}
}

// submitCase files one report directly through the service.
func (fx *civicHTTPFixture) submitCase(t *testing.T, in civic.ReportInput) store.CivicCase {
	t.Helper()
	c, err := fx.svc.Submit(context.Background(), fx.tenant.ID, fx.tenant.Slug, civic.ChannelWeb, in)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return c
}

func validCivicBody() string {
	return `{"category_slug":"roads","description":"Deep pothole at the junction blocking one lane","ward":"Ward 3","lga":"Ikeja","reporter_phone_e164":"+2348012345678","reporter_name":"Adaeze Obi"}`
}

// operatorReq builds a request with the tenant + roles context injected the
// way tenantMiddleware would (operator handlers invoked directly — the
// middleware itself is covered by the wiring test).
func (fx *civicHTTPFixture) operatorReq(method, target, body string, roles []string) *http.Request {
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	ctx := context.WithValue(req.Context(), ctxTenant, fx.tenant)
	ctx = context.WithValue(ctx, ctxUser, "user-1")
	ctx = context.WithValue(ctx, ctxRoles, roles)
	return req.WithContext(ctx)
}

// withCivicIDParam / withCivicRefParam inject chi URL params for direct
// handler invocation (the router normally does this).
func withCivicIDParam(req *http.Request, id string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func withCivicRefParam(req *http.Request, ref string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("ref", ref)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// ---------------------------------------------------------------------------
// Route wiring: public routes bypass the tenant middleware; operator +
// internal routes require it (the sla-breach route has no Permify guard).
// ---------------------------------------------------------------------------

func TestCivicRoutesWiring(t *testing.T) {
	r := NewRouter(Deps{Logger: zap.NewNop()})

	// Public report: no X-Tenant-Slug needed — 503 (Civic service nil),
	// not the tenant-middleware 400.
	req := httptest.NewRequest(http.MethodPost, "/v1/civic/public/tenants/ikeja-lga/reports", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("public report without tenant header = %d, want 503 (tenant middleware must not run)", rec.Code)
	}

	// Public categories/stats/track: also middleware-free.
	for _, u := range []string{
		"/v1/civic/public/tenants/ikeja-lga/categories",
		"/v1/civic/public/tenants/ikeja-lga/stats",
		"/v1/civic/public/tenants/ikeja-lga/reports/GOV-IKEJA-00-2026-000001?phone=%2B2348012345678",
	} {
		req = httptest.NewRequest(http.MethodGet, u, nil)
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET %s = %d, want 503", u, rec.Code)
		}
	}

	// Operator list: tenant middleware applies (400 without X-Tenant-Slug).
	req = httptest.NewRequest(http.MethodGet, "/v1/civic/cases", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("operator list without tenant header = %d, want 400", rec.Code)
	}

	// Internal sla-breach: tenant middleware applies but NO Permify guard
	// (X-Tenant-Slug only, service-to-service).
	req = httptest.NewRequest(http.MethodPost, "/v1/civic/internal/cases/GOV-IKEJA-00-2026-000001/sla-breach", strings.NewReader(`{"kind":"ack"}`))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("internal sla-breach without tenant header = %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Public intake
// ---------------------------------------------------------------------------

// Happy path + honeypot + validation + unknown tenant.
func TestPublicCivicReportEndpoint(t *testing.T) {
	fx := newCivicHTTPFixture(t)

	// Valid report → 201 {ref, ack_due_at}.
	req := httptest.NewRequest(http.MethodPost, "/v1/civic/public/tenants/ikeja-lga/reports", strings.NewReader(validCivicBody()))
	rec := httptest.NewRecorder()
	fx.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid report = %d: %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ref, _ := out["ref"].(string)
	if !strings.HasPrefix(ref, "GOV-IKEJA-WARD3-") {
		t.Fatalf("ref = %q", ref)
	}
	if out["ack_due_at"] == nil {
		t.Fatalf("ack_due_at missing: %v", out)
	}

	// Honeypot filled → rejected.
	req = httptest.NewRequest(http.MethodPost, "/v1/civic/public/tenants/ikeja-lga/reports",
		strings.NewReader(`{"category_slug":"roads","description":"Deep pothole at the junction blocking one lane","website":"http://spam.example"}`))
	rec = httptest.NewRecorder()
	fx.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("honeypot = %d, want 400", rec.Code)
	}

	// Validation failure (short description) → 400.
	req = httptest.NewRequest(http.MethodPost, "/v1/civic/public/tenants/ikeja-lga/reports",
		strings.NewReader(`{"category_slug":"roads","description":"short"}`))
	rec = httptest.NewRecorder()
	fx.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("short description = %d, want 400", rec.Code)
	}

	// Unknown tenant → 404.
	req = httptest.NewRequest(http.MethodPost, "/v1/civic/public/tenants/nowhere/reports", strings.NewReader(validCivicBody()))
	rec = httptest.NewRecorder()
	fx.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown tenant = %d, want 404", rec.Code)
	}
}

// Throttling: the per-IP hourly cap returns 429 (SPEC-W32 §4.6).
func TestPublicCivicReportThrottled(t *testing.T) {
	fx := newCivicHTTPFixture(t)
	fx.svc.RatePerHour = 2

	post := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/civic/public/tenants/ikeja-lga/reports", strings.NewReader(validCivicBody()))
		rec := httptest.NewRecorder()
		fx.router.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := post(); got != http.StatusCreated {
		t.Fatalf("report 1 = %d", got)
	}
	if got := post(); got != http.StatusCreated {
		t.Fatalf("report 2 = %d", got)
	}
	if got := post(); got != http.StatusTooManyRequests {
		t.Fatalf("report 3 = %d, want 429", got)
	}
}

// Tracking: ref+phone match → status view; mismatch → 404; no operator
// notes / other cases / reporter PII in the payload.
func TestPublicCivicTrackEndpoint(t *testing.T) {
	fx := newCivicHTTPFixture(t)
	c := fx.submitCase(t, civic.ReportInput{
		CategorySlug: "roads", Description: "Deep pothole at the junction blocking one lane",
		Ward: "Ward 3", LGA: "Ikeja", ReporterPhoneE164: "+2348012345678", ReporterName: "Adaeze Obi",
	})

	get := func(url string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		rec := httptest.NewRecorder()
		fx.router.ServeHTTP(rec, req)
		return rec
	}
	rec := get("/v1/civic/public/tenants/ikeja-lga/reports/" + c.Ref + "?phone=%2B2348012345678")
	if rec.Code != http.StatusOK {
		t.Fatalf("track correct phone = %d: %s", rec.Code, rec.Body)
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view["ref"] != c.Ref || view["status"] != "new" || view["category"] != "roads" || view["mda_queue"] != "mda-works" {
		t.Fatalf("track view = %v", view)
	}
	// No reporter PII beyond what the citizen typed, no operator notes.
	for _, k := range []string{"reporter_phone_e164", "reporter_name", "description", "assigned_to", "note"} {
		if _, leaked := view[k]; leaked {
			t.Fatalf("track view leaks %q: %v", k, view)
		}
	}
	// Wrong phone → 404 (no oracle).
	if rec := get("/v1/civic/public/tenants/ikeja-lga/reports/" + c.Ref + "?phone=%2B2348099999999"); rec.Code != http.StatusNotFound {
		t.Fatalf("wrong phone = %d, want 404", rec.Code)
	}
	// Missing phone → 404.
	if rec := get("/v1/civic/public/tenants/ikeja-lga/reports/" + c.Ref); rec.Code != http.StatusNotFound {
		t.Fatalf("missing phone = %d, want 404", rec.Code)
	}
}

// Stats: aggregate-only — no phone leakage (SPEC-W32 §4.1 quality gate).
func TestPublicCivicStatsNoPhoneLeakage(t *testing.T) {
	fx := newCivicHTTPFixture(t)
	fx.submitCase(t, civic.ReportInput{
		CategorySlug: "roads", Description: "Deep pothole at the junction blocking one lane",
		ReporterPhoneE164: "+2348012345678", ReporterName: "Adaeze Obi",
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/civic/public/tenants/ikeja-lga/stats", nil)
	rec := httptest.NewRecorder()
	fx.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"+2348012345678", "Adaeze", "phone", "reporter"} {
		if strings.Contains(body, leak) {
			t.Fatalf("stats leak %q: %s", leak, body)
		}
	}
	var stats store.CivicStats
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stats.Open != 1 {
		t.Fatalf("open = %d, want 1", stats.Open)
	}
}

// Public categories: active categories for the intake form.
func TestPublicCivicCategoriesEndpoint(t *testing.T) {
	fx := newCivicHTTPFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/civic/public/tenants/ikeja-lga/categories", nil)
	rec := httptest.NewRecorder()
	fx.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("categories = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"roads"`) {
		t.Fatalf("categories body = %s", rec.Body)
	}
}

// ---------------------------------------------------------------------------
// Operator console: masking by role (SPEC-W32 §4.4)
// ---------------------------------------------------------------------------

func TestOperatorMaskingByRole(t *testing.T) {
	fx := newCivicHTTPFixture(t)
	phone, name := "+2348012345678", "Adaeze Obi"
	fx.submitCase(t, civic.ReportInput{
		CategorySlug: "roads", Description: "Deep pothole at the junction blocking one lane",
		ReporterPhoneE164: phone, ReporterName: name, Anonymous: true,
	})

	// Staff list: anonymous reporter masked.
	req := fx.operatorReq(http.MethodGet, "/v1/civic/cases", "", []string{"staff"})
	rec := httptest.NewRecorder()
	fx.srv.listCivicCases(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff list = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, phone) || strings.Contains(body, name) {
		t.Fatalf("staff list leaks reporter: %s", body)
	}
	if !strings.Contains(body, "Anonymous") {
		t.Fatalf("staff list should show masked placeholder: %s", body)
	}

	// Owner list sees nothing extra here? List masks anonymous for all
	// non-owner/admin; owner list may reveal — assert role gate instead on
	// detail below. Find the case id.
	var listed struct {
		Cases []struct {
			ID string `json:"id"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || len(listed.Cases) != 1 {
		t.Fatalf("decode list: %v %s", err, rec.Body)
	}
	caseID := listed.Cases[0].ID

	// Detail as staff: masked.
	req = fx.operatorReq(http.MethodGet, "/v1/civic/cases/"+caseID, "", []string{"staff"})
	req = withCivicIDParam(req, caseID)
	rec = httptest.NewRecorder()
	fx.srv.getCivicCase(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff detail = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), phone) {
		t.Fatalf("staff detail leaks phone: %s", rec.Body)
	}

	// Detail as owner: revealed.
	req = fx.operatorReq(http.MethodGet, "/v1/civic/cases/"+caseID, "", []string{"owner"})
	req = withCivicIDParam(req, caseID)
	rec = httptest.NewRecorder()
	fx.srv.getCivicCase(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner detail = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), phone) {
		t.Fatalf("owner detail must reveal reporter: %s", rec.Body)
	}

	// Non-anonymous cases are visible to staff.
	fx.submitCase(t, civic.ReportInput{
		CategorySlug: "roads", Description: "Collapsed streetlight pole across the walkway",
		ReporterPhoneE164: phone, ReporterName: name,
	})
	req = fx.operatorReq(http.MethodGet, "/v1/civic/cases", "", []string{"staff"})
	rec = httptest.NewRecorder()
	fx.srv.listCivicCases(rec, req)
	if !strings.Contains(rec.Body.String(), phone) {
		t.Fatalf("non-anonymous reporter must be visible to staff: %s", rec.Body)
	}
}

// Operator lifecycle end-to-end at the handler level: list filters + SLA
// countdown fields, triage → assign → resolve, internal sla-breach flag.
func TestOperatorCaseLifecycle(t *testing.T) {
	fx := newCivicHTTPFixture(t)
	c := fx.submitCase(t, civic.ReportInput{
		CategorySlug: "roads", Description: "Deep pothole at the junction blocking one lane",
		Ward: "Ward 3", ReporterPhoneE164: "+2348012345678",
	})
	roles := []string{"admin"}

	// List carries SLA countdown fields.
	req := fx.operatorReq(http.MethodGet, "/v1/civic/cases?status=new", "", roles)
	rec := httptest.NewRecorder()
	fx.srv.listCivicCases(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var listed struct {
		Cases []civicCaseView `json:"cases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || len(listed.Cases) != 1 {
		t.Fatalf("decode list: %v", err)
	}
	if listed.Cases[0].AckDueInSeconds == nil || listed.Cases[0].ResolveDueInSeconds == nil {
		t.Fatalf("SLA countdown fields missing: %+v", listed.Cases[0])
	}
	if listed.Cases[0].CategorySlug != "roads" {
		t.Fatalf("category_slug = %q", listed.Cases[0].CategorySlug)
	}

	// Triage.
	req = fx.operatorReq(http.MethodPost, "/v1/civic/cases/"+c.ID.String()+"/triage", `{"ward":"Ward 3"}`, roles)
	req = withCivicIDParam(req, c.ID.String())
	rec = httptest.NewRecorder()
	fx.srv.triageCivicCase(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"triaged"`) {
		t.Fatalf("triage = %d %s", rec.Code, rec.Body)
	}

	// Assign.
	req = fx.operatorReq(http.MethodPost, "/v1/civic/cases/"+c.ID.String()+"/assign", `{"assignee":"crew-7"}`, roles)
	req = withCivicIDParam(req, c.ID.String())
	rec = httptest.NewRecorder()
	fx.srv.assignCivicCase(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"assigned"`) {
		t.Fatalf("assign = %d %s", rec.Code, rec.Body)
	}

	// Resolve.
	req = fx.operatorReq(http.MethodPost, "/v1/civic/cases/"+c.ID.String()+"/status", `{"status":"resolved","note":"patched"}`, roles)
	req = withCivicIDParam(req, c.ID.String())
	rec = httptest.NewRecorder()
	fx.srv.statusCivicCase(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"resolved"`) {
		t.Fatalf("resolve = %d %s", rec.Code, rec.Body)
	}

	// Invalid status target → 400.
	req = fx.operatorReq(http.MethodPost, "/v1/civic/cases/"+c.ID.String()+"/status", `{"status":"triaged"}`, roles)
	req = withCivicIDParam(req, c.ID.String())
	rec = httptest.NewRecorder()
	fx.srv.statusCivicCase(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad status = %d, want 400", rec.Code)
	}

	// Internal sla-breach callback sets the flag (X-Tenant-Slug only route;
	// invoked here with the tenant context the middleware would set).
	req = fx.operatorReq(http.MethodPost, "/v1/civic/internal/cases/"+c.Ref+"/sla-breach", `{"kind":"resolve"}`, nil)
	req = withCivicRefParam(req, c.Ref)
	rec = httptest.NewRecorder()
	fx.srv.civicSLABreach(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sla_breach_resolve":true`) {
		t.Fatalf("sla-breach = %d %s", rec.Code, rec.Body)
	}
	// The breach flag is visible on the operator list filter.
	req = fx.operatorReq(http.MethodGet, "/v1/civic/cases?sla_breach=resolve", "", roles)
	rec = httptest.NewRecorder()
	fx.srv.listCivicCases(rec, req)
	if !strings.Contains(rec.Body.String(), c.Ref) {
		t.Fatalf("sla_breach filter misses flagged case: %s", rec.Body)
	}
}

// Operator merge + duplicates endpoints at the handler level.
func TestOperatorMergeEndpoint(t *testing.T) {
	fx := newCivicHTTPFixture(t)
	roles := []string{"admin"}
	lat, lon := 6.5244, 3.3792
	canonical := fx.submitCase(t, civic.ReportInput{
		CategorySlug: "roads", Description: "Deep pothole at the junction blocking one lane",
		Lat: &lat, Lon: &lon, ReporterPhoneE164: "+2348012345678",
	})
	dup := fx.submitCase(t, civic.ReportInput{
		CategorySlug: "roads", Description: "Same pothole reported again by another citizen",
		Lat: &lat, Lon: &lon, ReporterPhoneE164: "+2348087654321",
	})

	// Duplicates endpoint returns the geo/time/category candidate set
	// (fake store returns none; the service-level matrix covers the rest —
	// here we assert the route shape).
	req := fx.operatorReq(http.MethodGet, "/v1/civic/cases/"+dup.ID.String()+"/duplicates", "", roles)
	req = withCivicIDParam(req, dup.ID.String())
	rec := httptest.NewRecorder()
	fx.srv.civicCaseDuplicates(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"candidates"`) {
		t.Fatalf("duplicates = %d %s", rec.Code, rec.Body)
	}

	// Merge dup → canonical; the response carries merged_into.
	req = fx.operatorReq(http.MethodPost, "/v1/civic/cases/"+dup.ID.String()+"/merge", `{"canonical_id":"`+canonical.ID.String()+`"}`, roles)
	req = withCivicIDParam(req, dup.ID.String())
	rec = httptest.NewRecorder()
	fx.srv.mergeCivicCase(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), canonical.ID.String()) {
		t.Fatalf("merge = %d %s", rec.Code, rec.Body)
	}

	// Merged case stays trackable with a merged_into pointer (§4.3).
	req = httptest.NewRequest(http.MethodGet, "/v1/civic/public/tenants/ikeja-lga/reports/"+dup.Ref+"?phone=%2B2348087654321", nil)
	rec = httptest.NewRecorder()
	fx.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), canonical.Ref) {
		t.Fatalf("merged track = %d %s", rec.Code, rec.Body)
	}

	// Merging into a merged case → 400.
	third := fx.submitCase(t, civic.ReportInput{CategorySlug: "roads", Description: "Yet another pothole duplicate report"})
	req = fx.operatorReq(http.MethodPost, "/v1/civic/cases/"+third.ID.String()+"/merge", `{"canonical_id":"`+dup.ID.String()+`"}`, roles)
	req = withCivicIDParam(req, third.ID.String())
	rec = httptest.NewRecorder()
	fx.srv.mergeCivicCase(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("merge into merged = %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Internal sla-breach → MDA notification via the W11 delivery path
// (SPEC-W32 §3 WS-B contract)
// ---------------------------------------------------------------------------

// TestCivicSLABreachMDANilIncidents: notify_mda requested but the incidents
// service is not wired — the breach flag is still set, the response carries
// mda_notified:false and nothing panics.
func TestCivicSLABreachMDANilIncidents(t *testing.T) {
	fx := newCivicHTTPFixture(t) // Deps.Incidents is nil here
	c := fx.submitCase(t, civic.ReportInput{
		CategorySlug: "roads", Description: "Deep pothole at the junction blocking one lane",
		ReporterPhoneE164: "+2348012345678",
	})
	req := fx.operatorReq(http.MethodPost, "/v1/civic/internal/cases/"+c.Ref+"/sla-breach",
		`{"kind":"ack","notify_mda":true,"mda_queue":"mda-works"}`, nil)
	req = withCivicRefParam(req, c.Ref)
	rec := httptest.NewRecorder()
	fx.srv.civicSLABreach(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("breach = %d %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["sla_breach_ack"] != true || out["mda_notified"] != false {
		t.Fatalf("response = %v", out)
	}
	if _, has := out["deliveries"]; has {
		t.Fatalf("deliveries must be absent when not notified: %v", out)
	}
}

// civicBreachFixture runs the sla-breach handler against a real (embedded)
// Postgres with the incidents service wired (AutoDispatch OFF — the handler
// must dispatch explicitly) and one active dispatch endpoint seeded.
type civicBreachFixture struct {
	srv      *server
	svc      *civic.Service
	store    *store.Store
	tenant   bookingops.TenantInfo
	endpoint string
}

func newCivicBreachFixture(t *testing.T) *civicBreachFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres civic breach test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_civic_breach_test").
		Port(5432).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5432/booking_civic_breach_test?sslmode=disable"
	// Minimal booking schema first (store.New's CRM-column bootstrap ALTERs
	// contacts/bookings); portalTestSchema supplies them + outbox.
	if pool, err := pgxpool.New(ctx, dsn); err != nil {
		t.Fatalf("raw pool: %v", err)
	} else {
		if _, err := pool.Exec(ctx, portalTestSchema); err != nil {
			t.Fatalf("test schema: %v", err)
		}
		pool.Close()
	}
	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)

	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "ikeja-lga"}
	civicSvc := &civic.Service{Store: st, EventsTopic: ""} // no outbox emission needed here
	// AutoDispatch=false: the handler's explicit Dispatch must carry the
	// notification (production posture does not depend on the wiring flag).
	incidentSvc := &incidents.Service{Store: st, AutoDispatch: false}
	fx := &civicBreachFixture{
		srv:      &server{d: Deps{Logger: zap.NewNop(), Civic: civicSvc, Incidents: incidentSvc}},
		svc:      civicSvc,
		store:    st,
		tenant:   tenant,
		endpoint: "https://mda-works.example/hooks/civic",
	}
	if err := st.UpsertDispatchEndpoint(ctx, &store.DispatchEndpoint{
		TenantID: tenant.ID, URL: fx.endpoint, Secret: "whsec", Active: true,
	}); err != nil {
		t.Fatalf("seed dispatch endpoint: %v", err)
	}
	return fx
}

// breachReq POSTs the sla-breach callback body through the handler.
func (fx *civicBreachFixture) breachReq(t *testing.T, ref, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/civic/internal/cases/"+ref+"/sla-breach", strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxTenant, fx.tenant)
	req = withCivicRefParam(req.WithContext(ctx), ref)
	rec := httptest.NewRecorder()
	fx.srv.civicSLABreach(rec, req)
	return rec
}

// breachIncidentID mirrors the handler's deterministic incident id.
func breachIncidentID(tenantID uuid.UUID, ref, kind string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(tenantID.String()+"|civic-sla-breach|"+ref+"|"+kind))
}

// notify_mda:true → a civic_sla_breach incident row exists and the W11
// incident_deliveries ledger has one row per active dispatch endpoint;
// replaying the same breach POST stays idempotent (one incident, one
// ledger row); notify_mda absent → no incident, legacy response shape.
func TestCivicSLABreachMDANotification(t *testing.T) {
	fx := newCivicBreachFixture(t)
	ctx := context.Background()

	c, err := fx.svc.Submit(ctx, fx.tenant.ID, fx.tenant.Slug, civic.ChannelWeb, civic.ReportInput{
		CategorySlug: "roads", Description: "Deep pothole at the junction blocking one lane",
		Ward: "Ward 3", ReporterPhoneE164: "+2348012345678",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	incidentID := breachIncidentID(fx.tenant.ID, c.Ref, "ack")

	t.Run("notify creates incident and delivery ledger", func(t *testing.T) {
		rec := fx.breachReq(t, c.Ref, `{"kind":"ack","notify_mda":true,"mda_queue":"mda-works"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("breach = %d %s", rec.Code, rec.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out["sla_breach_ack"] != true || out["mda_notified"] != true || out["deliveries"] != float64(1) {
			t.Fatalf("response = %v", out)
		}
		inc, err := fx.store.GetIncident(ctx, fx.tenant.ID, incidentID)
		if err != nil {
			t.Fatalf("incident row missing: %v", err)
		}
		if inc.IncidentType != "civic_sla_breach" || inc.Severity != incidents.SeverityHigh ||
			inc.ReferenceNumber != c.Ref {
			t.Fatalf("incident = %+v", inc)
		}
		if !strings.Contains(string(inc.Payload), "mda-works") {
			t.Fatalf("narrative must carry mda_queue: %s", inc.Payload)
		}
		deliveries, err := fx.store.ListIncidentDeliveries(ctx, fx.tenant.ID, incidentID)
		if err != nil {
			t.Fatalf("list deliveries: %v", err)
		}
		if len(deliveries) != 1 || deliveries[0].EndpointURL != fx.endpoint {
			t.Fatalf("deliveries = %+v", deliveries)
		}
		// The case itself carries the breach flag.
		got, err := fx.store.GetCivicCaseByRef(ctx, fx.tenant.ID, c.Ref)
		if err != nil || !got.SLABreachAck {
			t.Fatalf("case breach flag = %+v %v", got, err)
		}
	})

	t.Run("replay is idempotent", func(t *testing.T) {
		rec := fx.breachReq(t, c.Ref, `{"kind":"ack","notify_mda":true,"mda_queue":"mda-works"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("replay = %d %s", rec.Code, rec.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out["mda_notified"] != true || out["deliveries"] != float64(1) {
			t.Fatalf("replay response = %v", out)
		}
		// Exactly one incident, exactly one ledger row.
		inc, err := fx.store.GetIncident(ctx, fx.tenant.ID, incidentID)
		if err != nil {
			t.Fatalf("incident missing after replay: %v", err)
		}
		if inc.ReferenceNumber != c.Ref {
			t.Fatalf("incident = %+v", inc)
		}
		deliveries, err := fx.store.ListIncidentDeliveries(ctx, fx.tenant.ID, incidentID)
		if err != nil {
			t.Fatalf("list deliveries: %v", err)
		}
		if len(deliveries) != 1 {
			t.Fatalf("replay duplicated ledger rows: %+v", deliveries)
		}
	})

	t.Run("notify_mda absent keeps legacy shape and no incident", func(t *testing.T) {
		rec := fx.breachReq(t, c.Ref, `{"kind":"resolve"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("breach = %d %s", rec.Code, rec.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out["sla_breach_resolve"] != true || out["mda_notified"] != false {
			t.Fatalf("response = %v", out)
		}
		if _, has := out["deliveries"]; has {
			t.Fatalf("deliveries must be absent: %v", out)
		}
		resolveIncidentID := breachIncidentID(fx.tenant.ID, c.Ref, "resolve")
		if _, err := fx.store.GetIncident(ctx, fx.tenant.ID, resolveIncidentID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("no incident must be synthesized without notify_mda: %v", err)
		}
	})
}

// X-Forwarded-For: the app-level public-intake throttle buckets by the
// forwarded client IP (first hop), not the shared gateway RemoteAddr
// (FIX W4 — behind APISIX every citizen would otherwise share one bucket).
func TestPublicCivicReportThrottleXForwardedFor(t *testing.T) {
	fx := newCivicHTTPFixture(t)
	fx.svc.RatePerHour = 1

	post := func(xff, phone string) int {
		body := strings.Replace(validCivicBody(), "+2348012345678", phone, 1)
		req := httptest.NewRequest(http.MethodPost, "/v1/civic/public/tenants/ikeja-lga/reports", strings.NewReader(body))
		req.Header.Set("X-Forwarded-For", xff)
		rec := httptest.NewRecorder()
		fx.router.ServeHTTP(rec, req)
		return rec.Code
	}
	// First hop of the XFF chain keys the bucket.
	if got := post("203.0.113.10, 10.0.0.1", "+2348011111111"); got != http.StatusCreated {
		t.Fatalf("first post = %d", got)
	}
	// Same forwarded IP → throttled, even with a different phone.
	if got := post(" 203.0.113.10 ", "+2348022222222"); got != http.StatusTooManyRequests {
		t.Fatalf("same XFF IP = %d, want 429", got)
	}
	// A different forwarded IP gets an independent bucket.
	if got := post("198.51.100.7", "+2348022222222"); got != http.StatusCreated {
		t.Fatalf("different XFF IP = %d, want 201", got)
	}
	// No XFF → RemoteAddr fallback bucket.
	if got := post("", "+2348033333333"); got != http.StatusCreated {
		t.Fatalf("RemoteAddr fallback = %d, want 201", got)
	}
	if got := post("", "+2348044444444"); got != http.StatusTooManyRequests {
		t.Fatalf("RemoteAddr repeat = %d, want 429", got)
	}
}
