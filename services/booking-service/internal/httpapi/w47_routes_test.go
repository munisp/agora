package httpapi

// SPEC-W47 (W47-A) route tests: PATCH aliases for offerings/team-members
// (item 1, UIWEB-02/03), staff CSAT note (item 2, UIWEB-05), public
// {offering_id, date} availability aggregate (item 3, UIMOB-01), the
// internal-token-gated ?contact_phone= booking listing (item 4, UIMOB-05)
// and the campaigns lifecycle routes (item 7, DB-07).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"go.uber.org/zap"

	"github.com/opendesk/booking-service/internal/bookingops"
	daprc "github.com/opendesk/booking-service/internal/daprc"
	"github.com/opendesk/booking-service/internal/helpdesk"
	"github.com/opendesk/booking-service/internal/leads"
	"github.com/opendesk/booking-service/internal/store"
)

type w47Fixture struct {
	t      *testing.T
	st     *store.Store
	hs     *helpdesk.Store
	dsn    string
	router http.Handler
	tenant bookingops.TenantInfo
}

func newW47Fixture(t *testing.T, internalToken string) *w47Fixture {
	return newW47FixtureOnPort(t, internalToken, 5573)
}

func newW47FixtureOnPort(t *testing.T, internalToken string, port uint32) *w47Fixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres W47 route test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_w47_test").
		Port(port).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/booking_w47_test?sslmode=disable", port)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	if _, err := conn.Exec(ctx, w45TestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	// team_members + availability_rules are bootstrap-managed by
	// init-scripts in production (not store.New); the W47 tests need them.
	if _, err := conn.Exec(ctx, `
CREATE TABLE IF NOT EXISTS team_members (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL, name TEXT NOT NULL, email TEXT,
    role TEXT NOT NULL DEFAULT 'staff', active BOOLEAN NOT NULL DEFAULT TRUE,
    user_id TEXT
);
CREATE TABLE IF NOT EXISTS availability_rules (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL, team_member_id UUID NOT NULL,
    weekday SMALLINT NOT NULL, start_min SMALLINT NOT NULL, end_min SMALLINT NOT NULL,
    effective_from DATE, effective_to DATE
);`); err != nil {
		t.Fatalf("schema ext: %v", err)
	}
	conn.Close(ctx) //nolint:errcheck

	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)

	hs, err := helpdesk.DialStore(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("helpdesk.DialStore: %v", err)
	}
	t.Cleanup(hs.Close)

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
	resolver := bookingops.NewTenantResolver(daprc.New("localhost", 1), "identity", time.Minute, zap.NewNop(),
		bookingops.WithIdentityBaseURL(identity.URL))

	f := &w47Fixture{t: t, st: st, hs: hs, dsn: dsn, tenant: tenant}
	f.router = NewRouter(Deps{
		Logger:        zap.NewNop(),
		Store:         st,
		Resolver:      resolver,
		Ops:           &bookingops.Service{Store: st, EventsTopic: "booking.events", Logger: zap.NewNop()},
		Leads:         &leads.Service{Store: st},
		Helpdesk:      &helpdesk.Deps{Store: hs, Log: zap.NewNop()},
		AuthzDisabled: true,
		InternalToken: internalToken,
	})
	return f
}

func (f *w47Fixture) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Tenant-Slug", f.tenant.Slug)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// Item 1 (UIWEB-02/03): PATCH is an additive alias of the PUT handlers.
func TestW47PatchAliases(t *testing.T) {
	f := newW47FixtureOnPort(t, "w47-secret", 5580)
	ctx := context.Background()

	offering := store.Offering{TenantID: f.tenant.ID, Name: "Cut", DurationMin: 30, PriceCents: 5000, Currency: "NGN"}
	if err := f.st.CreateOffering(ctx, &offering); err != nil {
		t.Fatal(err)
	}
	rec := f.do(http.MethodPatch, "/v1/offerings/"+offering.ID.String(),
		`{"name":"Deluxe Cut","duration_min":45,"price_cents":7000,"currency":"NGN"}`,
		map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH offering = %d: %s", rec.Code, rec.Body.String())
	}
	got, err := f.st.GetOffering(ctx, f.tenant.ID, offering.ID)
	if err != nil || got.Name != "Deluxe Cut" || got.DurationMin != 45 {
		t.Fatalf("offering after PATCH: %+v %v", got, err)
	}

	member := store.TeamMember{TenantID: f.tenant.ID, Name: "Ada", Role: "staff", Active: true}
	if err := f.st.CreateTeamMember(ctx, &member); err != nil {
		t.Fatal(err)
	}
	rec = f.do(http.MethodPatch, "/v1/team-members/"+member.ID.String(),
		`{"name":"Ada","role":"staff","active":false}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH team-member = %d: %s", rec.Code, rec.Body.String())
	}
	m, err := f.st.GetTeamMember(ctx, f.tenant.ID, member.ID)
	if err != nil || m.Active {
		t.Fatalf("member after PATCH: %+v %v", m, err)
	}
}

// Item 2 (UIWEB-05): staff-side CSAT recording on resolved tickets.
func TestW47StaffCSATNote(t *testing.T) {
	f := newW47FixtureOnPort(t, "w47-secret", 5581)
	ctx := context.Background()

	tk := helpdesk.Ticket{TenantID: f.tenant.ID, Subject: "Cannot book", Channel: "web", Priority: helpdesk.PriorityNormal, Status: helpdesk.StatusOpen}
	if err := f.hs.CreateTicket(ctx, &tk, "test"); err != nil {
		t.Fatal(err)
	}

	// Open ticket → 409 (same state rule as the public token flow).
	rec := f.do(http.MethodPost, "/v1/helpdesk/tickets/"+tk.ID.String()+"/csat-note",
		`{"rating":5}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("csat-note on open ticket = %d: %s, want 409", rec.Code, rec.Body.String())
	}

	resolved := helpdesk.StatusResolved
	if _, err := f.hs.PatchTicket(ctx, f.tenant.ID, tk.ID, helpdesk.PatchInput{Status: &resolved}, "op-1"); err != nil {
		t.Fatalf("resolve ticket: %v", err)
	}
	rec = f.do(http.MethodPost, "/v1/helpdesk/tickets/"+tk.ID.String()+"/csat-note",
		`{"rating":4,"comment":"sorted quickly"}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("csat-note = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ticket struct {
			CSATRating  *int    `json:"csat_rating"`
			CSATComment *string `json:"csat_comment"`
		} `json:"ticket"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Ticket.CSATRating == nil || *body.Ticket.CSATRating != 4 {
		t.Fatalf("csat rating not recorded: %s", rec.Body.String())
	}

	// K24/OOS-11 invariant preserved: the pre-W47 staff drawer route
	// (PATCH /v1/helpdesk/tickets/{id}/csat) stays REMOVED — 404.
	rec = f.do(http.MethodPatch, "/v1/helpdesk/tickets/"+tk.ID.String()+"/csat",
		`{"rating":5}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PATCH csat alias = %d, want 404 (K24: route removed in W45)", rec.Code)
	}
	// Invalid rating → 400; unknown ticket → 404.
	rec = f.do(http.MethodPost, "/v1/helpdesk/tickets/"+tk.ID.String()+"/csat-note",
		`{"rating":9}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad rating = %d, want 400", rec.Code)
	}
	rec = f.do(http.MethodPost, "/v1/helpdesk/tickets/"+uuid.NewString()+"/csat-note",
		`{"rating":3}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown ticket = %d, want 404", rec.Code)
	}
}

// Item 3 (UIMOB-01): public {offering_id, date} availability aggregates
// across active members and answers the {slots:[...]} envelope.
func TestW47PublicAvailabilityAggregate(t *testing.T) {
	f := newW47FixtureOnPort(t, "w47-secret", 5582)
	ctx := context.Background()

	site := store.Site{TenantID: f.tenant.ID, TenantSlug: f.tenant.Slug, Slug: f.tenant.Slug, DisplayName: "Acme"}
	if err := f.st.CreateSite(ctx, &site); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE sites SET published=true WHERE slug=$1`, f.tenant.Slug); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx) //nolint:errcheck

	offering := store.Offering{TenantID: f.tenant.ID, Name: "Cut", DurationMin: 60, PriceCents: 5000, Currency: "NGN", Bookable: true}
	if err := f.st.CreateOffering(ctx, &offering); err != nil {
		t.Fatal(err)
	}
	// Two ACTIVE members available 09:00-12:00 on the target weekday; one
	// INACTIVE member must be excluded from the aggregate.
	day := time.Now().AddDate(0, 0, 2)
	weekday := int(day.Weekday())
	var memberIDs []uuid.UUID
	for _, name := range []string{"Ada", "Bea"} {
		m := store.TeamMember{TenantID: f.tenant.ID, Name: name, Role: "staff", Active: true}
		if err := f.st.CreateTeamMember(ctx, &m); err != nil {
			t.Fatal(err)
		}
		memberIDs = append(memberIDs, m.ID)
		if err := f.st.SetAvailability(ctx, f.tenant.ID, m.ID, []store.AvailabilityRule{{
			Weekday: weekday, StartMin: 9 * 60, EndMin: 12 * 60,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	inactive := store.TeamMember{TenantID: f.tenant.ID, Name: "Zed", Role: "staff", Active: false}
	if err := f.st.CreateTeamMember(ctx, &inactive); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetAvailability(ctx, f.tenant.ID, inactive.ID, []store.AvailabilityRule{{
		Weekday: weekday, StartMin: 9 * 60, EndMin: 12 * 60,
	}}); err != nil {
		t.Fatal(err)
	}

	dateStr := day.Format("2006-01-02")
	rec := f.do(http.MethodGet,
		"/public/sites/"+f.tenant.Slug+"/availability?offering_id="+offering.ID.String()+"&date="+dateStr, ``, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public availability = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		OfferingID string `json:"offering_id"`
		Date       string `json:"date"`
		Slots      []struct {
			StartsAt          time.Time `json:"starts_at"`
			TeamMemberID      string    `json:"team_member_id"`
			RemainingCapacity int       `json:"remaining_capacity"`
		} `json:"slots"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OfferingID != offering.ID.String() || body.Date != dateStr {
		t.Fatalf("envelope echo wrong: %s", rec.Body.String())
	}
	if len(body.Slots) != 3 { // 09, 10, 11 at 60min
		t.Fatalf("slots = %d, want 3: %s", len(body.Slots), rec.Body.String())
	}
	if len(body.Items) != len(body.Slots) {
		t.Fatalf("items mirror missing: %s", rec.Body.String())
	}
	for _, s := range body.Slots {
		ok := false
		for _, id := range memberIDs {
			if s.TeamMemberID == id.String() {
				ok = true
			}
		}
		if !ok || s.TeamMemberID == inactive.ID.String() {
			t.Fatalf("slot carries wrong member: %+v", s)
		}
		if s.RemainingCapacity != 2 { // both active members free
			t.Fatalf("remaining_capacity = %d, want 2 (aggregate of 2 active members)", s.RemainingCapacity)
		}
	}

	// Non-bookable offering stays invisible on the public surface.
	offering.Bookable = false
	offering.ID = uuid.New()
	offering.Name = "Hidden"
	if err := f.st.CreateOffering(ctx, &offering); err != nil {
		t.Fatal(err)
	}
	rec = f.do(http.MethodGet,
		"/public/sites/"+f.tenant.Slug+"/availability?offering_id="+offering.ID.String()+"&date="+dateStr, ``, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-bookable availability = %d, want 404", rec.Code)
	}
	// Bad date → 400; legacy team_member_id path unchanged (from/to needed).
	rec = f.do(http.MethodGet,
		"/public/sites/"+f.tenant.Slug+"/availability?offering_id="+offering.ID.String()+"&date=next-tuesday", ``, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad date = %d, want 400", rec.Code)
	}
	rec = f.do(http.MethodGet,
		"/public/sites/"+f.tenant.Slug+"/availability?offering_id="+offering.ID.String()+
			"&team_member_id="+memberIDs[0].String()+"&date="+dateStr, ``, nil)
	if rec.Code != http.StatusBadRequest { // legacy path requires from/to
		t.Fatalf("legacy path with date = %d, want 400 (from/to required)", rec.Code)
	}
}

// Item 4 (UIMOB-05): ?contact_phone= is internal-token-gated; the plain
// listing is untouched.
func TestW47ListBookingsContactPhone(t *testing.T) {
	f := newW47FixtureOnPort(t, "w47-secret", 5583)
	ctx := context.Background()
	phone := "+2348011112222"
	contact := store.Contact{TenantID: f.tenant.ID, Name: "Caller", Phone: phone}
	if err := f.st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(72 * time.Hour)
	b := store.Booking{TenantID: f.tenant.ID, OfferingID: uuid.New(), ContactID: contact.ID,
		TeamMemberID: uuid.New(), StartsAt: start, EndsAt: start.Add(30 * time.Minute),
		Status: store.StatusConfirmed, Source: "voice"}
	if err := f.st.CreateBookingTx(ctx, &b, store.SlotGuard{}, "test.events", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}

	path := "/v1/bookings?contact_phone=" + url.QueryEscape(phone)
	// No token → 401.
	if rec := f.do(http.MethodGet, path, ``, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}
	// Wrong token → 401.
	if rec := f.do(http.MethodGet, path, ``, map[string]string{"X-Internal-Token": "nope"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d, want 401", rec.Code)
	}
	// Right token → 200, server-side filtered, contact_phone echoed.
	rec := f.do(http.MethodGet, path, ``, map[string]string{"X-Internal-Token": "w47-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("internal token = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Bookings []struct {
			ID           string `json:"id"`
			Status       string `json:"status"`
			ContactPhone string `json:"contact_phone"`
		} `json:"bookings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Bookings) != 1 || body.Bookings[0].ID != b.ID.String() || body.Bookings[0].ContactPhone != phone {
		t.Fatalf("filtered bookings: %s", rec.Body.String())
	}
	// Non-matching phone → empty.
	rec = f.do(http.MethodGet, "/v1/bookings?contact_phone=%2B999", ``, map[string]string{"X-Internal-Token": "w47-secret"})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), b.ID.String()) {
		t.Fatalf("non-matching phone leaked bookings: %d %s", rec.Code, rec.Body.String())
	}
	// Plain listing (no contact_phone) does NOT require the internal token.
	if rec := f.do(http.MethodGet, "/v1/bookings", ``, map[string]string{"X-User-Id": "op-1"}); rec.Code != http.StatusOK {
		t.Fatalf("plain listing = %d, want 200", rec.Code)
	}

	// Fail-closed when BOOKING_INTERNAL_TOKEN is unset → 503.
	f2 := newW47FixtureOnPort(t, "", 5584)
	if rec := f2.do(http.MethodGet, path, ``, map[string]string{"X-Internal-Token": "anything"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unset token = %d, want 503 (fail closed)", rec.Code)
	}
}

// Item 7 (DB-07): campaign update + cancel routes.
func TestW47CampaignRoutes(t *testing.T) {
	f := newW47FixtureOnPort(t, "w47-secret", 5585)

	rec := f.do(http.MethodPost, "/v1/campaigns", `{"name":"Launch","channel":"instagram"}`,
		map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("create campaign = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Campaign struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"campaign"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Campaign.Status != "active" {
		t.Fatalf("new campaign status = %q", created.Campaign.Status)
	}
	id := created.Campaign.ID

	rec = f.do(http.MethodPatch, "/v1/campaigns/"+id, `{"name":"Launch v2","status":"paused"}`,
		map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"paused"`) {
		t.Fatalf("patch campaign = %d: %s", rec.Code, rec.Body.String())
	}
	rec = f.do(http.MethodPatch, "/v1/campaigns/"+id, `{"status":"bogus"}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("bogus status = %d: %s, want 409", rec.Code, rec.Body.String())
	}
	rec = f.do(http.MethodPost, "/v1/campaigns/"+id+"/cancel", `{}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"cancelled"`) {
		t.Fatalf("cancel = %d: %s", rec.Code, rec.Body.String())
	}
	// Idempotent cancel; terminal afterwards.
	rec = f.do(http.MethodPost, "/v1/campaigns/"+id+"/cancel", `{}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("re-cancel = %d: %s", rec.Code, rec.Body.String())
	}
	rec = f.do(http.MethodPatch, "/v1/campaigns/"+id, `{"name":"x"}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("patch after cancel = %d, want 409", rec.Code)
	}
	rec = f.do(http.MethodPost, "/v1/campaigns/"+id+"/spend",
		`{"amount_ngn":100,"channel":"instagram","day":"`+time.Now().Format("2006-01-02")+`"}`,
		map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("spend after cancel = %d: %s, want 409", rec.Code, rec.Body.String())
	}
	// Unknown campaign → 404.
	rec = f.do(http.MethodPatch, "/v1/campaigns/"+uuid.NewString(), `{"name":"x"}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown campaign = %d, want 404", rec.Code)
	}
}
