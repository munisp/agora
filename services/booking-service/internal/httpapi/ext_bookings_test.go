package httpapi

// SPEC-W45 K17 (booking half) route tests: the X-Api-Key ext bookings
// surface (/v1/ext/bookings) — 401 no/bad key, 403 insufficient scope,
// happy path with tenant-isolation proof (a key for tenant A cannot read
// tenant B), per-key rate limit, and validator-down / unconfigured 503
// fail-closed postures. Embedded-postgres harness (dedicated port 5573;
// -short skips).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/opendesk/booking-service/internal/apikey"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

const extInternalToken = "itok-test"

// extKeyEntry is one mock identity api-key binding.
type extKeyEntry struct {
	TenantID   uuid.UUID
	TenantSlug string
	Scopes     []string
}

// extFixture bundles the embedded store, the mock identity validator and
// the seeded tenants.
type extFixture struct {
	t       *testing.T
	st      *store.Store
	tenantA uuid.UUID
	tenantB uuid.UUID
	keys    map[string]extKeyEntry
	idURL   string
	// validateCalls counts identity validator hits (cache observability).
	validateCalls *atomic.Int32
}

func newExtFixture(t *testing.T) *extFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres K17 ext route test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_ext_test").
		Port(5573).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5573/booking_ext_test?sslmode=disable"
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	if _, err := conn.Exec(ctx, w45TestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	conn.Close(ctx) //nolint:errcheck

	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)

	f := &extFixture{t: t, st: st, tenantA: uuid.New(), tenantB: uuid.New(), validateCalls: &atomic.Int32{}}
	f.keys = map[string]extKeyEntry{
		"odk_keya.secret-a":    {TenantID: f.tenantA, TenantSlug: "acme-a", Scopes: []string{"bookings:read"}},
		"odk_keyb.secret-b":    {TenantID: f.tenantB, TenantSlug: "acme-b", Scopes: []string{"bookings:read"}},
		"odk_limited.secret-l": {TenantID: f.tenantA, TenantSlug: "acme-a", Scopes: []string{"other:scope"}},
		"odk_noscope.secret-n": {TenantID: f.tenantA, TenantSlug: "acme-a", Scopes: []string{}},
	}

	// Mock identity POST /internal/api-keys/validate (K2: X-Internal-Token
	// required; 401 unknown key; 200 {tenant_slug, tenant_id, scopes,...}).
	idSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/api-keys/validate" {
			http.Error(w, "no such identity route: "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("X-Internal-Token") != extInternalToken {
			http.Error(w, "invalid internal token", http.StatusUnauthorized)
			return
		}
		f.validateCalls.Add(1)
		var req struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		e, ok := f.keys[req.Key]
		if !ok {
			http.Error(w, "invalid api key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tenant_slug": e.TenantSlug, "tenant_id": e.TenantID.String(),
			"scopes": e.Scopes, "key_id": uuid.NewString(), "prefix": "odk_mock",
		})
	}))
	t.Cleanup(idSrv.Close)
	f.idURL = idSrv.URL
	return f
}

// router builds the booking router with the given apikey validator.
func (f *extFixture) router(v *apikey.Validator) http.Handler {
	return NewRouter(Deps{
		Logger:  zap.NewNop(),
		Store:   f.st,
		Ops:     nil,
		ExtKeys: v,
	})
}

// do issues one ext request with the given key ("" = no header).
func (f *extFixture) do(r http.Handler, path, key string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// seedExtBooking inserts one booking for the tenant at the given start.
func (f *extFixture) seedExtBooking(tenantID uuid.UUID, status string, start time.Time) store.Booking {
	f.t.Helper()
	b := store.Booking{
		TenantID: tenantID, OfferingID: uuid.New(), ContactID: uuid.New(),
		StartsAt: start, EndsAt: start.Add(30 * time.Minute),
		Status: status, Source: "api",
	}
	if err := f.st.CreateBookingTx(context.Background(), &b, store.SlotGuard{}, "test.events", []byte(`{}`)); err != nil {
		f.t.Fatal(err)
	}
	return b
}

func decodeExtList(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list body: %v (%s)", err, rec.Body.String())
	}
	return body
}

// 401: missing key and identity-rejected key.
func TestExtBookingsUnauthorized(t *testing.T) {
	f := newExtFixture(t)
	r := f.router(apikey.New(f.idURL, extInternalToken, zap.NewNop()))

	if rec := f.do(r, "/v1/ext/bookings", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key = %d: %s, want 401", rec.Code, rec.Body.String())
	}
	if rec := f.do(r, "/v1/ext/bookings", "odk_bogus.secret-x"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad key = %d: %s, want 401", rec.Code, rec.Body.String())
	}
	// The 401 path is never cached: a second bad-key call validates again.
	if rec := f.do(r, "/v1/ext/bookings/"+uuid.NewString(), "odk_bogus.secret-x"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad key on detail = %d, want 401", rec.Code)
	}
}

// 403: a VALID key without the bookings:read scope (both scope variants).
func TestExtBookingsForbiddenScope(t *testing.T) {
	f := newExtFixture(t)
	r := f.router(apikey.New(f.idURL, extInternalToken, zap.NewNop()))
	for _, key := range []string{"odk_limited.secret-l", "odk_noscope.secret-n"} {
		if rec := f.do(r, "/v1/ext/bookings", key); rec.Code != http.StatusForbidden {
			t.Fatalf("key %s = %d: %s, want 403", key, rec.Code, rec.Body.String())
		}
	}
}

// Happy path: list/detail with pagination + filters, and the tenant
// isolation proof — key A cannot read tenant B's rows (list scoping AND
// detail 404, no existence oracle).
func TestExtBookingsHappyPathTenantIsolation(t *testing.T) {
	f := newExtFixture(t)
	r := f.router(apikey.New(f.idURL, extInternalToken, zap.NewNop()))

	now := time.Now().UTC().Truncate(time.Second)
	a1 := f.seedExtBooking(f.tenantA, store.StatusConfirmed, now.Add(24*time.Hour))
	a2 := f.seedExtBooking(f.tenantA, store.StatusConfirmed, now.Add(48*time.Hour))
	a3 := f.seedExtBooking(f.tenantA, store.StatusPending, now.Add(72*time.Hour))
	b1 := f.seedExtBooking(f.tenantB, store.StatusConfirmed, now.Add(24*time.Hour))

	// Full list for tenant A: 3 rows, all stamped tenant A.
	rec := f.do(r, "/v1/ext/bookings", "odk_keya.secret-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeExtList(t, rec)
	bookings := body["bookings"].([]any)
	if len(bookings) != 3 {
		t.Fatalf("tenant A list = %d bookings, want 3: %s", len(bookings), rec.Body.String())
	}
	for _, bi := range bookings {
		if bi.(map[string]any)["tenant_id"] != f.tenantA.String() {
			t.Fatalf("foreign booking leaked into tenant A list: %s", rec.Body.String())
		}
	}

	// Pagination: limit=2 page 1, offset=2 page 2 — disjoint, union = 3.
	rec = f.do(r, "/v1/ext/bookings?limit=2", "odk_keya.secret-a")
	page1 := decodeExtList(t, rec)["bookings"].([]any)
	rec = f.do(r, "/v1/ext/bookings?limit=2&offset=2", "odk_keya.secret-a")
	page2 := decodeExtList(t, rec)["bookings"].([]any)
	if len(page1) != 2 || len(page2) != 1 {
		t.Fatalf("pagination pages = %d + %d, want 2 + 1", len(page1), len(page2))
	}
	seen := map[string]bool{}
	for _, p := range append(page1, page2...) {
		id := p.(map[string]any)["id"].(string)
		if seen[id] {
			t.Fatalf("booking %s repeated across pages", id)
		}
		seen[id] = true
	}
	for _, id := range []string{a1.ID.String(), a2.ID.String(), a3.ID.String()} {
		if !seen[id] {
			t.Fatalf("booking %s missing from paginated union", id)
		}
	}

	// Filters: status, from, to (starts_at DESC ordering).
	rec = f.do(r, "/v1/ext/bookings?status=confirmed", "odk_keya.secret-a")
	if got := len(decodeExtList(t, rec)["bookings"].([]any)); got != 2 {
		t.Fatalf("status=confirmed = %d, want 2", got)
	}
	rec = f.do(r, "/v1/ext/bookings?from="+now.Add(60*time.Hour).Format(time.RFC3339), "odk_keya.secret-a")
	if got := len(decodeExtList(t, rec)["bookings"].([]any)); got != 1 {
		t.Fatalf("from filter = %d, want 1 (only the +72h booking)", got)
	}
	rec = f.do(r, "/v1/ext/bookings?to="+now.Add(60*time.Hour).Format(time.RFC3339), "odk_keya.secret-a")
	if got := len(decodeExtList(t, rec)["bookings"].([]any)); got != 2 {
		t.Fatalf("to filter = %d, want 2", got)
	}
	// Newest-first ordering.
	rec = f.do(r, "/v1/ext/bookings?limit=2", "odk_keya.secret-a")
	page := decodeExtList(t, rec)["bookings"].([]any)
	if page[0].(map[string]any)["id"] != a3.ID.String() {
		t.Fatalf("newest-first order broken: first = %v, want %s", page[0].(map[string]any)["id"], a3.ID)
	}

	// Bad pagination params → 400.
	if rec := f.do(r, "/v1/ext/bookings?limit=-1", "odk_keya.secret-a"); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=-1 = %d, want 400", rec.Code)
	}
	if rec := f.do(r, "/v1/ext/bookings?offset=-5", "odk_keya.secret-a"); rec.Code != http.StatusBadRequest {
		t.Fatalf("offset=-5 = %d, want 400", rec.Code)
	}
	if rec := f.do(r, "/v1/ext/bookings?from=not-a-time", "odk_keya.secret-a"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad from = %d, want 400", rec.Code)
	}

	// Detail: tenant A key reads tenant A booking.
	rec = f.do(r, "/v1/ext/bookings/"+a1.ID.String(), "odk_keya.secret-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body.String())
	}
	var got store.Booking
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != a1.ID || got.TenantID != f.tenantA {
		t.Fatalf("get body: %+v", got)
	}

	// TENANT ISOLATION PROOF: key A cannot read tenant B's booking (404 —
	// same as nonexistent, no oracle), and key B's list sees ONLY B's row.
	rec = f.do(r, "/v1/ext/bookings/"+b1.ID.String(), "odk_keya.secret-a")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get = %d: %s, want 404", rec.Code, rec.Body.String())
	}
	rec = f.do(r, "/v1/ext/bookings/"+a1.ID.String(), "odk_keyb.secret-b")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reverse cross-tenant get = %d: %s, want 404", rec.Code, rec.Body.String())
	}
	rec = f.do(r, "/v1/ext/bookings", "odk_keyb.secret-b")
	bList := decodeExtList(t, rec)["bookings"].([]any)
	if len(bList) != 1 || bList[0].(map[string]any)["id"] != b1.ID.String() {
		t.Fatalf("tenant B list = %s, want exactly tenant B's booking", rec.Body.String())
	}

	// Positive-validation cache: the repeated key-A calls above validated
	// against identity far fewer times than requests were made.
	if calls := f.validateCalls.Load(); calls >= 10 {
		t.Fatalf("identity validated %d times — positive cache (≤60s) not working", calls)
	}
}

// Per-key rate limit: budget 3/min (test override) — the 4th request is
// 429 while a DIFFERENT key keeps its own budget.
func TestExtBookingsRateLimit(t *testing.T) {
	f := newExtFixture(t)
	r := f.router(apikey.New(f.idURL, extInternalToken, zap.NewNop(),
		apikey.WithRateLimit(3, time.Minute)))

	for i := 1; i <= 3; i++ {
		if rec := f.do(r, "/v1/ext/bookings", "odk_keya.secret-a"); rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d: %s, want 200", i, rec.Code, rec.Body.String())
		}
	}
	if rec := f.do(r, "/v1/ext/bookings", "odk_keya.secret-a"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th request = %d: %s, want 429", rec.Code, rec.Body.String())
	}
	// The budget is per KEY, not global: key B is unaffected.
	if rec := f.do(r, "/v1/ext/bookings", "odk_keyb.secret-b"); rec.Code != http.StatusOK {
		t.Fatalf("key B after key A exhausted = %d: %s, want 200", rec.Code, rec.Body.String())
	}
}

// Validator-down 503 (fail closed, never a guessed validation) and the
// unconfigured 503 (IDENTITY_BASE_URL/IDENTITY_INTERNAL_TOKEN unset).
func TestExtBookingsValidatorDown(t *testing.T) {
	f := newExtFixture(t)

	// Identity unreachable (closed server) → 503.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	r := f.router(apikey.New(deadURL, extInternalToken, zap.NewNop()))
	if rec := f.do(r, "/v1/ext/bookings", "odk_keya.secret-a"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("validator down = %d: %s, want 503", rec.Code, rec.Body.String())
	}

	// Unconfigured validator (empty base/token) → 503.
	r = f.router(apikey.New("", "", zap.NewNop()))
	if rec := f.do(r, "/v1/ext/bookings", "odk_keya.secret-a"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured = %d: %s, want 503", rec.Code, rec.Body.String())
	}

	// Nil Deps.ExtKeys → the router substitutes the fail-closed validator.
	r = NewRouter(Deps{Logger: zap.NewNop(), Store: f.st})
	if rec := f.do(r, "/v1/ext/bookings", "odk_keya.secret-a"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil ExtKeys = %d: %s, want 503 (fail closed)", rec.Code, rec.Body.String())
	}

	// The ext group stays OUTSIDE the JWT/tenant group: a request WITHOUT
	// X-Tenant-Slug must NOT 400 on tenant resolution — it reaches the
	// apikey middleware (401 no key here).
	r = f.router(apikey.New(f.idURL, extInternalToken, zap.NewNop()))
	req := httptest.NewRequest(http.MethodGet, "/v1/ext/bookings", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), "X-Tenant-Slug") {
		t.Fatalf("ext route leaked into the tenant group: %d %s", rec.Code, rec.Body.String())
	}
}
