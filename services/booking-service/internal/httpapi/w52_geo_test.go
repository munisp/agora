package httpapi

// SPEC-W52 Scope B route tests: coverage-zone enforcement at the PUBLIC
// booking create (marker wiring, 422 GEO_OUTSIDE_COVERAGE body contract,
// PostGIS-absent 503 idiom, tenant-API exemption) and NewRouter's geo
// enrichment registration. Embedded-postgres harness (dedicated port 5591;
// -short skips) mirroring w48_routes_test.go. The embedded Postgres has no
// PostGIS, so coverage counts come from a fake CoverageCounter — the SQL
// itself is pinned in bookingops/w52_coverage_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/daprc"
	"github.com/opendesk/booking-service/internal/geo"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

const w52TestPort = 5591

// w52FakeCounter mirrors the bookingops one (distinct package).
type w52FakeCounter struct {
	areas, points, covered int
	err                    error
}

func (f *w52FakeCounter) AudienceCount(_ context.Context, _ uuid.UUID, q string, _ []any) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	switch {
	case strings.Contains(q, "ST_Covers"):
		return f.covered, nil
	case strings.Contains(q, "FROM contact_locations"):
		return f.points, nil
	default:
		return f.areas, nil
	}
}

type w52Fixture struct {
	t       *testing.T
	router  http.Handler
	st      *store.Store
	tenant  bookingops.TenantInfo
	offer   store.Offering
	member  store.TeamMember
	siteSlug string
	start   time.Time
}

// newW52Fixture builds the router over a real store with the coverage gate
// configured per the overlay.
func newW52Fixture(t *testing.T, enforce bool, counter bookingops.CoverageCounter) *w52Fixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres W52 route test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_w52_test").
		Port(w52TestPort).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/booking_w52_test?sslmode=disable", w52TestPort)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) }) //nolint:errcheck
	// Minimal base schema BEFORE store.New (portal_test.go idiom).
	if _, err := conn.Exec(ctx, w45mTestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)

	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme-w52", Name: "Acme W52",
		Timezone: "UTC", Currency: "NGN"}
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tenants/"+tenant.Slug {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tenant)
	}))
	t.Cleanup(identity.Close)

	resolver := bookingops.NewTenantResolver(daprc.New("localhost", 1), "identity", time.Minute, zap.NewNop(),
		bookingops.WithIdentityBaseURL(identity.URL))

	f := &w52Fixture{t: t, st: st, tenant: tenant, siteSlug: "acme-w52-site"}
	// A bookable offering + active member with a 09:00–12:00 rule on the
	// target weekday (two days out) so passing creates land cleanly.
	f.offer = store.Offering{TenantID: tenant.ID, Name: "Consult", DurationMin: 30,
		PriceCents: 0, Currency: "NGN", Bookable: true}
	if err := st.CreateOffering(ctx, &f.offer); err != nil {
		t.Fatalf("offering: %v", err)
	}
	f.member = store.TeamMember{TenantID: tenant.ID, Name: "Ada", Role: "staff", Active: true}
	if err := st.CreateTeamMember(ctx, &f.member); err != nil {
		t.Fatalf("member: %v", err)
	}
	day := time.Now().UTC().AddDate(0, 0, 2)
	f.start = time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, time.UTC)
	if err := st.SetAvailability(ctx, tenant.ID, f.member.ID, []store.AvailabilityRule{{
		Weekday: int(day.Weekday()), StartMin: 9 * 60, EndMin: 12 * 60,
	}}); err != nil {
		t.Fatalf("availability: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO sites (tenant_id, tenant_slug, slug, display_name, published)
		 VALUES ($1,$2,$3,'Acme W52',TRUE)`, tenant.ID, tenant.Slug, f.siteSlug); err != nil {
		t.Fatalf("site: %v", err)
	}

	f.router = NewRouter(Deps{
		Logger:            zap.NewNop(),
		Store:             st,
		Resolver:          resolver,
		AuthzDisabled:     true,
		AuthzOutagePolicy: AuthzFailClosed,
		Ops: &bookingops.Service{
			Store: st, EventsTopic: "test.events", Logger: zap.NewNop(),
			Coverage: counter,
		},
		BookingGeoEnforce: enforce,
	})
	return f
}

func (f *w52Fixture) bookingBody() string {
	return fmt.Sprintf(`{"offering_id":%q,"team_member_id":%q,"starts_at":%q,"contact":{"name":"Ada Lovelace","phone":"+2348012345678"}}`,
		f.offer.ID, f.member.ID, f.start.Format(time.RFC3339))
}

func (f *w52Fixture) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func TestW52PublicBookingCoverageEnforcement(t *testing.T) {
	t.Run("outside coverage → 422 GEO_OUTSIDE_COVERAGE", func(t *testing.T) {
		f := newW52Fixture(t, true, &w52FakeCounter{areas: 1, points: 1, covered: 0})
		rec := f.do(http.MethodPost, "/public/sites/"+f.siteSlug+"/bookings", f.bookingBody(), nil)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d (%s), want 422", rec.Code, rec.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["code"] != "GEO_OUTSIDE_COVERAGE" {
			t.Fatalf("body = %v, want code GEO_OUTSIDE_COVERAGE", body)
		}
	})

	t.Run("PostGIS absent → 503 (geoHandler idiom)", func(t *testing.T) {
		f := newW52Fixture(t, true, &w52FakeCounter{err: store.ErrPostGISUnavailable})
		rec := f.do(http.MethodPost, "/public/sites/"+f.siteSlug+"/bookings", f.bookingBody(), nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d (%s), want 503", rec.Code, rec.Body.String())
		}
	})

	t.Run("inside coverage → 201", func(t *testing.T) {
		f := newW52Fixture(t, true, &w52FakeCounter{areas: 1, points: 1, covered: 1})
		rec := f.do(http.MethodPost, "/public/sites/"+f.siteSlug+"/bookings", f.bookingBody(), nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d (%s), want 201", rec.Code, rec.Body.String())
		}
	})

	t.Run("no zones → 201 (documented pass)", func(t *testing.T) {
		f := newW52Fixture(t, true, &w52FakeCounter{areas: 0})
		rec := f.do(http.MethodPost, "/public/sites/"+f.siteSlug+"/bookings", f.bookingBody(), nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d (%s), want 201", rec.Code, rec.Body.String())
		}
	})

	t.Run("enforce disabled → 201 even outside", func(t *testing.T) {
		f := newW52Fixture(t, false, &w52FakeCounter{areas: 1, points: 1, covered: 0})
		rec := f.do(http.MethodPost, "/public/sites/"+f.siteSlug+"/bookings", f.bookingBody(), nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d (%s), want 201", rec.Code, rec.Body.String())
		}
	})

	t.Run("tenant API path is never gated", func(t *testing.T) {
		f := newW52Fixture(t, true, &w52FakeCounter{areas: 1, points: 1, covered: 0})
		rec := f.do(http.MethodPost, "/v1/bookings", f.bookingBody(),
			map[string]string{"X-Tenant-Slug": f.tenant.Slug, "X-User-Id": "op-1"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d (%s), want 201 (tenant API ungated)", rec.Code, rec.Body.String())
		}
	})
}

// TestW52RouterWiresCivicEnrichment proves the NewRouter wiring: a Deps
// geo enricher is registered on the store, so civic inserts self-enrich
// post-commit (fail-open, fill-only-when-empty — the full semantics are
// covered in internal/store/w52_geo_test.go).
func TestW52RouterWiresCivicEnrichment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-postgres W52 route test in -short mode")
	}
	f := newW52Fixture(t, false, nil) // gate off; enrichment on

	geoSvc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/geo/enrich" {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		// The enrich client forwards the tenant UUID verbatim on
		// X-Tenant-Slug (geo-service parses it as a uuid for the RLS GUC).
		if got := r.Header.Get("X-Tenant-Slug"); got != f.tenant.ID.String() {
			t.Errorf("X-Tenant-Slug = %q, want tenant uuid %s", got, f.tenant.ID)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"Lagos","lga":"Eti-Osa","ward":"VI I","boundaries":[]}`))
	}))
	defer geoSvc.Close()
	f.router = NewRouter(Deps{
		Logger:            zap.NewNop(),
		Store:             f.st,
		Resolver:          bookingops.NewTenantResolver(daprc.New("localhost", 1), "identity", time.Minute, zap.NewNop()),
		AuthzDisabled:     true,
		AuthzOutagePolicy: AuthzFailClosed,
		Ops:               &bookingops.Service{Store: f.st, EventsTopic: "test.events", Logger: zap.NewNop()},
		GeoEnrich:         geo.NewEnrichClient(geoSvc.URL, zap.NewNop()),
	})
	t.Cleanup(func() { store.RegisterCivicGeoEnricher(f.st, nil, nil) })

	lat, lon := 6.5244, 3.3792
	c := store.CivicCase{
		TenantID:    f.tenant.ID,
		Ref:         "GOV-TEST-W52-HTTP",
		CategoryID:  uuid.New(),
		Status:      store.CivicStatusNew,
		Description: "flooded drainage channel behind the market stalls",
		Lat:         &lat,
		Lon:         &lon,
		Channel:     "web",
	}
	if err := f.st.InsertCivicCase(context.Background(), &c); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := f.st.GetCivicCase(context.Background(), f.tenant.ID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LGA != "Eti-Osa" || got.Ward != "VI I" {
		t.Fatalf("enriched via router wiring: lga/ward = %q/%q", got.LGA, got.Ward)
	}
}
