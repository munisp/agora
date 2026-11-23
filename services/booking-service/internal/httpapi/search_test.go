package httpapi

// SPEC-W119 §5: handler tests with a fake Searcher — happy paths for all
// three entities, param plumbing + date normalization, nil-Searcher 503,
// backend-error 503, window 400, junk dates 400, limit clamp, tenant from
// context (never query params), and chi static-beats-wildcard for
// /{entity}/search vs /{id}. No database: the search routes never touch
// the store.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/search"
)

type fakeSearcher struct {
	err       error
	got       map[string]search.Params
	gotTenant map[string]string
}

func newFakeSearcher() *fakeSearcher {
	return &fakeSearcher{got: map[string]search.Params{}, gotTenant: map[string]string{}}
}

func (f *fakeSearcher) record(entity, tenantID string, p search.Params) (search.Response, error) {
	if f.err != nil {
		return search.Response{}, f.err
	}
	f.got[entity] = p
	f.gotTenant[entity] = tenantID
	return search.Response{
		Hits:  []json.RawMessage{json.RawMessage(`{"entity":"` + entity + `"}`)},
		Total: 7,
	}, nil
}

func (f *fakeSearcher) SearchBookings(ctx context.Context, tenantID string, p search.Params) (search.Response, error) {
	return f.record("bookings", tenantID, p)
}
func (f *fakeSearcher) SearchContacts(ctx context.Context, tenantID string, p search.Params) (search.Response, error) {
	return f.record("contacts", tenantID, p)
}
func (f *fakeSearcher) SearchLeads(ctx context.Context, tenantID string, p search.Params) (search.Response, error) {
	return f.record("leads", tenantID, p)
}

type searchFixture struct {
	router http.Handler
	tenant bookingops.TenantInfo
}

func newSearchFixture(t *testing.T, searcher Searcher) *searchFixture {
	t.Helper()
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme-ng", Name: "Acme NG"}
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tenants/"+tenant.Slug {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tenant)
	}))
	t.Cleanup(identity.Close)
	resolver := bookingops.NewTenantResolver(nil, "", 0, zap.NewNop(),
		bookingops.WithIdentityBaseURL(identity.URL))
	return &searchFixture{
		router: NewRouter(Deps{
			Logger:        zap.NewNop(),
			Resolver:      resolver,
			Searcher:      searcher,
			AuthzDisabled: true,
		}),
		tenant: tenant,
	}
}

func (f *searchFixture) get(t *testing.T, path string, withTenantHeader bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if withTenantHeader {
		req.Header.Set("X-Tenant-Slug", f.tenant.Slug)
		req.Header.Set("X-User-Id", uuid.New().String())
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func TestSearchBookingsHappyPathParamPlumbing(t *testing.T) {
	fake := newFakeSearcher()
	f := newSearchFixture(t, fake)
	rec := f.get(t, "/v1/bookings/search?q=ada&status=confirmed&source=web&from_ts=2026-01-02&to_ts=2026-01-31T03:04:05%2B02:00&limit=50&offset=10", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	p := fake.got["bookings"]
	if p.Q != "ada" || p.Status != "confirmed" || p.Source != "web" {
		t.Fatalf("params = %#v", p)
	}
	if p.FromTS != "2026-01-02T00:00:00Z" {
		t.Fatalf("from_ts normalized = %q", p.FromTS)
	}
	if p.ToTS != "2026-01-31T01:04:05Z" { // +02:00 -> UTC
		t.Fatalf("to_ts normalized = %q", p.ToTS)
	}
	if p.Limit != 50 || p.Offset != 10 {
		t.Fatalf("limit/offset = %d/%d", p.Limit, p.Offset)
	}
	if fake.gotTenant["bookings"] != f.tenant.ID.String() {
		t.Fatalf("tenant = %q, want context tenant", fake.gotTenant["bookings"])
	}
	var resp search.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 7 || len(resp.Hits) != 1 {
		t.Fatalf("resp = %s", rec.Body.String())
	}
}

func TestSearchContactsHappyPath(t *testing.T) {
	fake := newFakeSearcher()
	f := newSearchFixture(t, fake)
	rec := f.get(t, "/v1/contacts/search?q=ada&source=crm", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	p := fake.got["contacts"]
	if p.Q != "ada" || p.Source != "crm" || p.Limit != search.DefaultLimit {
		t.Fatalf("params = %#v", p)
	}
}

func TestSearchLeadsHappyPath(t *testing.T) {
	fake := newFakeSearcher()
	f := newSearchFixture(t, fake)
	rec := f.get(t, "/v1/leads/search?q=%2B23480&status=new&channel=whatsapp&from_ts=2026-02-01", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	p := fake.got["leads"]
	if p.Q != "+23480" || p.Status != "new" || p.Channel != "whatsapp" || p.FromTS != "2026-02-01T00:00:00Z" {
		t.Fatalf("params = %#v", p)
	}
}

func TestSearchNilSearcherFailsClosed503(t *testing.T) {
	f := newSearchFixture(t, nil)
	for _, path := range []string{"/v1/bookings/search", "/v1/contacts/search", "/v1/leads/search"} {
		rec := f.get(t, path, true)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d", path, rec.Code)
		}
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body["error"] != "search_unavailable" {
			t.Fatalf("%s: body = %s", path, rec.Body.String())
		}
	}
}

func TestSearchBackendErrorIs503(t *testing.T) {
	fake := newFakeSearcher()
	fake.err = errors.New("cluster down")
	f := newSearchFixture(t, fake)
	rec := f.get(t, "/v1/bookings/search?q=x", true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestSearchResultWindow400(t *testing.T) {
	fake := newFakeSearcher()
	f := newSearchFixture(t, fake)
	rec := f.get(t, "/v1/bookings/search?limit=100&offset=9901", true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, called := fake.got["bookings"]; called {
		t.Fatal("backend must not be called beyond the result window")
	}
}

func TestSearchJunkDates400(t *testing.T) {
	fake := newFakeSearcher()
	f := newSearchFixture(t, fake)
	for _, path := range []string{
		"/v1/bookings/search?from_ts=junk",
		"/v1/contacts/search?to_ts=32-13-99",
		"/v1/leads/search?to_ts=2026/01/01",
	} {
		rec := f.get(t, path, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d (%s)", path, rec.Code, rec.Body.String())
		}
	}
}

func TestSearchLimitClamped(t *testing.T) {
	fake := newFakeSearcher()
	f := newSearchFixture(t, fake)
	rec := f.get(t, "/v1/contacts/search?limit=500", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if fake.got["contacts"].Limit != search.MaxLimit {
		t.Fatalf("limit = %d, want clamped %d", fake.got["contacts"].Limit, search.MaxLimit)
	}
}

func TestSearchRequiresTenantHeader(t *testing.T) {
	fake := newFakeSearcher()
	f := newSearchFixture(t, fake)
	rec := f.get(t, "/v1/bookings/search?q=x", false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(fake.got) != 0 {
		t.Fatal("backend must not be called without a resolved tenant")
	}
}

func TestSearchStaticBeatsWildcard(t *testing.T) {
	// /v1/bookings/search must hit the search handler, NOT getBooking
	// (which would try to parse "search" as a UUID and 400).
	fake := newFakeSearcher()
	f := newSearchFixture(t, fake)
	for _, path := range []string{"/v1/bookings/search", "/v1/contacts/search", "/v1/leads/search"} {
		rec := f.get(t, path, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (%s)", path, rec.Code, rec.Body.String())
		}
	}
	if len(fake.got) != 3 {
		t.Fatalf("called entities = %#v", fake.got)
	}
}
