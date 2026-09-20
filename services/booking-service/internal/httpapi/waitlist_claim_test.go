package httpapi

// SPEC-W45 CODER-M route tests (K13 + STK O14 completion):
//   - PUBLIC waitlist claim-info/claim token endpoints (auth matrix vs the
//     tenant routes, token lifecycle 200/400/404/409/410, replay
//     idempotency, legacy /v1/waitlist/{id}/claim compatibility);
//   - team_members.user_id create/list/get roundtrip (+400 on bad uuid).
//
// Embedded-postgres harness (dedicated port 5572; -short skips).

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
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

const w45mTestSchema = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE IF NOT EXISTS offerings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
    duration_min INTEGER NOT NULL, buffer_min INTEGER NOT NULL DEFAULT 0,
    price_cents INTEGER NOT NULL DEFAULT 0, currency CHAR(3) NOT NULL DEFAULT 'USD',
    capacity INTEGER NOT NULL DEFAULT 1, bookable BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS team_members (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL, name TEXT NOT NULL, email TEXT,
    role TEXT NOT NULL DEFAULT 'staff', active BOOLEAN NOT NULL DEFAULT TRUE
);
CREATE TABLE IF NOT EXISTS availability_rules (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL, team_member_id UUID NOT NULL,
    weekday SMALLINT NOT NULL, start_min SMALLINT NOT NULL, end_min SMALLINT NOT NULL,
    effective_from DATE, effective_to DATE
);
CREATE TABLE IF NOT EXISTS contacts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL, name TEXT NOT NULL, phone TEXT, email TEXT, notes TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS bookings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL, offering_id UUID NOT NULL, team_member_id UUID, contact_id UUID,
    starts_at TIMESTAMPTZ NOT NULL, ends_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending', source TEXT NOT NULL DEFAULT 'api',
    idempotency_key TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_bookings_tenant_idempotency_key ON bookings (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(), aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL, payload JSONB NOT NULL, sent_at TIMESTAMPTZ
);`

type w45mFixture struct {
	t      *testing.T
	router http.Handler
	st     *store.Store
	raw    *pgx.Conn // raw SQL for fixture seeds/forced states
	tenant bookingops.TenantInfo
	member store.TeamMember
	offer  store.Offering
}

func newW45MFixture(t *testing.T) *w45mFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres W45-M route test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_w45m_test").
		Port(5572).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5572/booking_w45m_test?sslmode=disable"
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	if _, err := conn.Exec(ctx, w45mTestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}

	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)

	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme-claim", Name: "Acme Claim", Timezone: "UTC"}
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

	f := &w45mFixture{t: t, st: st, raw: conn, tenant: tenant}
	t.Cleanup(func() { conn.Close(context.Background()) }) //nolint:errcheck
	f.router = NewRouter(Deps{
		Logger:   zap.NewNop(),
		Store:    st,
		Resolver: resolver,
		Ops: &bookingops.Service{
			Store: st, EventsTopic: "booking.events", Logger: zap.NewNop(),
		},
		AuthzDisabled: true,
	})

	// Seed: one bookable offering, one active member available 09:00-17:00
	// every weekday, and the tenant's public site row (the token-claim
	// tenant resolution path goes tenant_id → site → slug → identity).
	f.offer = store.Offering{TenantID: tenant.ID, Name: "Consult", Description: "intro call",
		DurationMin: 30, PriceCents: 7500, Currency: "USD", Bookable: true}
	if err := st.CreateOffering(ctx, &f.offer); err != nil {
		t.Fatalf("offering: %v", err)
	}
	f.member = store.TeamMember{TenantID: tenant.ID, Name: "Sam", Email: "sam@acme.test", Role: "staff", Active: true}
	if err := st.CreateTeamMember(ctx, &f.member); err != nil {
		t.Fatalf("member: %v", err)
	}
	rules := make([]store.AvailabilityRule, 0, 7)
	for wd := 0; wd < 7; wd++ {
		rules = append(rules, store.AvailabilityRule{
			TenantID: tenant.ID, TeamMemberID: f.member.ID, Weekday: wd, StartMin: 9 * 60, EndMin: 17 * 60,
		})
	}
	if err := st.SetAvailability(ctx, tenant.ID, f.member.ID, rules); err != nil {
		t.Fatalf("rules: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO sites (tenant_id, tenant_slug, slug, display_name, published)
		 VALUES ($1,$2,$3,$4,TRUE)`, tenant.ID, tenant.Slug, "acme-claim-site", "Acme Claim"); err != nil {
		t.Fatalf("site: %v", err)
	}
	return f
}

// do issues a request WITHOUT a tenant header (public path posture).
func (f *w45mFixture) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
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

// tenantDo issues a tenant-scoped request (management API posture).
func (f *w45mFixture) tenantDo(method, path, body string) *httptest.ResponseRecorder {
	return f.do(method, path, body, map[string]string{"X-Tenant-Slug": f.tenant.Slug, "X-User-Id": "op-1"})
}

// seedEntry inserts a waitlist entry whose window lies fully inside the
// seeded 09:00-17:00 availability (start = next day's 10:00 UTC).
func (f *w45mFixture) seedEntry(t *testing.T) store.WaitlistEntry {
	t.Helper()
	start := time.Now().UTC().Add(48 * time.Hour)
	start = time.Date(start.Year(), start.Month(), start.Day(), 10, 0, 0, 0, time.UTC)
	entry := store.WaitlistEntry{
		TenantID:     f.tenant.ID,
		OfferingID:   f.offer.ID,
		ContactName:  "Ada",
		ContactPhone: "+15550001",
		WindowStart:  start,
		WindowEnd:    start.Add(2 * time.Hour),
	}
	if err := f.st.CreateWaitlistEntry(context.Background(), &entry); err != nil {
		t.Fatalf("entry: %v", err)
	}
	return entry
}

// seedExpiredEntry inserts an entry whose window already passed.
func (f *w45mFixture) seedExpiredEntry(t *testing.T) store.WaitlistEntry {
	t.Helper()
	end := time.Now().UTC().Add(-time.Hour)
	entry := store.WaitlistEntry{
		TenantID:     f.tenant.ID,
		OfferingID:   f.offer.ID,
		ContactName:  "Late Larry",
		ContactPhone: "+15550002",
		WindowStart:  end.Add(-2 * time.Hour),
		WindowEnd:    end,
	}
	if err := f.st.CreateWaitlistEntry(context.Background(), &entry); err != nil {
		t.Fatalf("expired entry: %v", err)
	}
	return entry
}

// seedNightEntry inserts a waiting entry whose window (22:00-23:00) falls
// outside every availability rule — no slot can ever be picked.
func (f *w45mFixture) seedNightEntry(t *testing.T) store.WaitlistEntry {
	t.Helper()
	start := time.Now().UTC().Add(48 * time.Hour)
	start = time.Date(start.Year(), start.Month(), start.Day(), 22, 0, 0, 0, time.UTC)
	entry := store.WaitlistEntry{
		TenantID:     f.tenant.ID,
		OfferingID:   f.offer.ID,
		ContactName:  "Night Owl",
		ContactPhone: "+15550003",
		WindowStart:  start,
		WindowEnd:    start.Add(time.Hour),
	}
	if err := f.st.CreateWaitlistEntry(context.Background(), &entry); err != nil {
		t.Fatalf("night entry: %v", err)
	}
	return entry
}

func TestW45MClaimInfoLifecycle(t *testing.T) {
	f := newW45MFixture(t)
	entry := f.seedEntry(t)

	// 400 — malformed token.
	if rec := f.do(http.MethodGet, "/v1/waitlist/claim-info?token=not-a-uuid", ``, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed token = %d, want 400", rec.Code)
	}
	// 404 — unknown token.
	if rec := f.do(http.MethodGet, "/v1/waitlist/claim-info?token="+uuid.NewString(), ``, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token = %d, want 404", rec.Code)
	}
	// 200 — public (NO tenant header, NO JWT), claimable with the seeded
	// 09:00-17:00 rules; defensive keys entry/offering/tenant/claimable.
	rec := f.do(http.MethodGet, "/v1/waitlist/claim-info?token="+entry.ClaimToken.String(), ``, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim-info = %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"entry", "offering", "tenant", "claimable"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("claim-info missing key %q: %s", key, rec.Body.String())
		}
	}
	if body["claimable"] != true {
		t.Fatalf("claimable = %v, want true", body["claimable"])
	}
	e := body["entry"].(map[string]any)
	if e["id"] != entry.ID.String() || e["contact_name"] != "Ada" {
		t.Fatalf("entry projection: %s", rec.Body.String())
	}
	if _, leaked := e["claim_token"]; leaked {
		t.Fatalf("claim_token must not leak: %s", rec.Body.String())
	}
	if _, leaked := e["contact_phone"]; leaked {
		t.Fatalf("contact_phone must not leak: %s", rec.Body.String())
	}
	o := body["offering"].(map[string]any)
	if o["name"] != "Consult" || o["duration_min"] != 30.0 || o["price_cents"] != 7500.0 {
		t.Fatalf("offering projection: %s", rec.Body.String())
	}
	tn := body["tenant"].(map[string]any)
	if tn["slug"] != f.tenant.Slug || tn["name"] != f.tenant.Name {
		t.Fatalf("tenant projection: %s", rec.Body.String())
	}

	// claimable=false — the night window has no open slot (slot gone), but
	// the token itself is still valid → 200 with claimable=false.
	night := f.seedNightEntry(t)
	rec = f.do(http.MethodGet, "/v1/waitlist/claim-info?token="+night.ClaimToken.String(), ``, nil)
	if rec.Code != http.StatusOK || rec.Body.String() == "" {
		t.Fatalf("night claim-info = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["claimable"] != false {
		t.Fatalf("night claimable = %v, want false", body["claimable"])
	}

	// 410 — expired window.
	expired := f.seedExpiredEntry(t)
	if rec := f.do(http.MethodGet, "/v1/waitlist/claim-info?token="+expired.ClaimToken.String(), ``, nil); rec.Code != http.StatusGone {
		t.Fatalf("expired claim-info = %d, want 410", rec.Code)
	}
}

func TestW45MClaimLifecycleAndReplay(t *testing.T) {
	f := newW45MFixture(t)
	entry := f.seedEntry(t)

	// 404 — unknown token.
	rec := f.do(http.MethodPost, "/v1/waitlist/claim", `{"token":"`+uuid.NewString()+`"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown claim = %d, want 404", rec.Code)
	}
	// 400 — malformed token.
	if rec := f.do(http.MethodPost, "/v1/waitlist/claim", `{"token":"x"}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed claim = %d, want 400", rec.Code)
	}

	// 200 — public claim succeeds; the earliest window slot is picked.
	rec = f.do(http.MethodPost, "/v1/waitlist/claim", `{"token":"`+entry.ClaimToken.String()+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim = %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	booking := body["booking"].(map[string]any)
	bookingID := booking["id"].(string)
	if bookingID == "" || body["replayed"] != false {
		t.Fatalf("claim body: %s", rec.Body.String())
	}
	if booking["team_member_id"] != f.member.ID.String() {
		t.Fatalf("claim picked member %v, want %s", booking["team_member_id"], f.member.ID)
	}
	startsAt, err := time.Parse(time.RFC3339Nano, booking["starts_at"].(string))
	if err != nil || !startsAt.Equal(entry.WindowStart) {
		t.Fatalf("starts_at = %v (%v), want window start %v", booking["starts_at"], err, entry.WindowStart)
	}
	if got := body["entry"].(map[string]any)["status"]; got != store.WaitlistClaimed {
		t.Fatalf("entry status = %v, want claimed", got)
	}

	// Idempotent replay: same token returns the ORIGINAL booking.
	rec = f.do(http.MethodPost, "/v1/waitlist/claim", `{"token":"`+entry.ClaimToken.String()+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["booking"].(map[string]any)["id"] != bookingID || body["replayed"] != true {
		t.Fatalf("replay body: %s", rec.Body.String())
	}

	// Consumed token → claim-info now reports 410.
	if rec := f.do(http.MethodGet, "/v1/waitlist/claim-info?token="+entry.ClaimToken.String(), ``, nil); rec.Code != http.StatusGone {
		t.Fatalf("consumed claim-info = %d, want 410", rec.Code)
	}

	// 410 — expired window on claim.
	expired := f.seedExpiredEntry(t)
	if rec := f.do(http.MethodPost, "/v1/waitlist/claim", `{"token":"`+expired.ClaimToken.String()+`"}`, nil); rec.Code != http.StatusGone {
		t.Fatalf("expired claim = %d, want 410", rec.Code)
	}

	// 409 — slot lost (waiting entry, no open slot in the window).
	night := f.seedNightEntry(t)
	if rec := f.do(http.MethodPost, "/v1/waitlist/claim", `{"token":"`+night.ClaimToken.String()+`"}`, nil); rec.Code != http.StatusConflict {
		t.Fatalf("slot-lost claim = %d: %s, want 409", rec.Code, rec.Body.String())
	}

	// 409 — replay mismatch: entry flipped to claimed behind our back with
	// no booking to replay.
	orphan := f.seedEntry(t)
	if _, err := f.raw.Exec(context.Background(),
		`UPDATE waitlist SET status='claimed' WHERE tenant_id=$1 AND id=$2`, f.tenant.ID, orphan.ID); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(http.MethodPost, "/v1/waitlist/claim", `{"token":"`+orphan.ClaimToken.String()+`"}`, nil); rec.Code != http.StatusConflict {
		t.Fatalf("replay-mismatch claim = %d: %s, want 409", rec.Code, rec.Body.String())
	}
}

// The legacy tenant-scoped claim route stays for compatibility.
func TestW45MLegacyClaimRouteCompat(t *testing.T) {
	f := newW45MFixture(t)
	entry := f.seedEntry(t)
	body := fmt.Sprintf(`{"token":%q,"team_member_id":%q,"starts_at":%q}`,
		entry.ClaimToken.String(), f.member.ID.String(), entry.WindowStart.UTC().Format(time.RFC3339Nano))
	rec := f.tenantDo(http.MethodPost, "/v1/waitlist/"+entry.ID.String()+"/claim", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy claim = %d: %s", rec.Code, rec.Body.String())
	}
}

// Auth matrix: the token endpoints are public; the management routes keep
// the tenant middleware.
func TestW45MClaimAuthMatrix(t *testing.T) {
	f := newW45MFixture(t)
	entry := f.seedEntry(t)

	// Tenant route without X-Tenant-Slug → 400 from tenantMiddleware.
	if rec := f.do(http.MethodGet, "/v1/waitlist", ``, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("tenant waitlist without slug = %d, want 400", rec.Code)
	}
	// Public endpoints need NO headers at all.
	if rec := f.do(http.MethodGet, "/v1/waitlist/claim-info?token="+entry.ClaimToken.String(), ``, nil); rec.Code != http.StatusOK {
		t.Fatalf("public claim-info without headers = %d: %s", rec.Code, rec.Body.String())
	}
	// Tenant route WITH the slug works (manage path intact).
	if rec := f.tenantDo(http.MethodGet, "/v1/waitlist", ``); rec.Code != http.StatusOK {
		t.Fatalf("tenant waitlist with slug = %d: %s", rec.Code, rec.Body.String())
	}
}

// STK O14 completion: user_id roundtrip on team members.
func TestW45MTeamMemberUserID(t *testing.T) {
	f := newW45MFixture(t)
	uid := uuid.New()

	// 400 — malformed user_id.
	if rec := f.tenantDo(http.MethodPost, "/v1/team-members", `{"name":"Bad","user_id":"nope"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad user_id = %d, want 400", rec.Code)
	}

	// 201 — create with user_id, echoed in the response.
	rec := f.tenantDo(http.MethodPost, "/v1/team-members", `{"name":"Linked","user_id":"`+uid.String()+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created["user_id"] != uid.String() {
		t.Fatalf("create response user_id = %v, want %s", created["user_id"], uid)
	}
	memberID := created["id"].(string)

	// GET list carries user_id.
	rec = f.tenantDo(http.MethodGet, "/v1/team-members", ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var list struct {
		TeamMembers []map[string]any `json:"team_members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range list.TeamMembers {
		if m["id"] == memberID {
			found = true
			if m["user_id"] != uid.String() {
				t.Fatalf("list user_id = %v", m["user_id"])
			}
		}
	}
	if !found {
		t.Fatalf("created member missing from list: %s", rec.Body.String())
	}

	// GET detail carries user_id.
	rec = f.tenantDo(http.MethodGet, "/v1/team-members/"+memberID, ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["user_id"] != uid.String() {
		t.Fatalf("get user_id = %v", got["user_id"])
	}

	// Create WITHOUT user_id stays valid and unlinked.
	rec = f.tenantDo(http.MethodPost, "/v1/team-members", `{"name":"Plain"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("plain create = %d: %s", rec.Code, rec.Body.String())
	}
	var plain map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &plain); err != nil {
		t.Fatal(err)
	}
	if v, present := plain["user_id"]; present && v != nil {
		t.Fatalf("user_id should be absent/null for unlinked member: %s", rec.Body.String())
	}
}
