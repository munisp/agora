package httpapi

// SPEC-W61 Scope C route tests: the media inbox surface.
//
// Coverage:
//   - ingest auth matrix: BOOKING_INTERNAL_TOKEN unset => 503 (fail-closed),
//     wrong token => 401, deterministic validation failures => 400 (never
//     reaching the store), happy path => 200 {id,state} + idempotent replay
//     (same (tenant,channel,message_id) returns the SAME row).
//   - resolve-or-park through the HTTP surface: digits-normalized match
//     against an UNNORMALIZED stored contact phone (G3) with exactly one
//     in-window booking => anchored; no contact => parked.
//   - staff authz (KYC P5 posture): member / no-roles => 403 on ALL three
//     staff routes; owner/admin pass.
//   - staff list pagination + state filter validation; link happy path
//     (contact_id filled from the booking, anchor_method=staff) + invalid
//     booking (bad uuid => 400, foreign/missing => 404); discard happy +
//     missing => 404.
//   - fail-closed wiring: nil store => 503 (routes stay mounted).
//
// Harness: embedded-postgres + stub identity resolver (w45/w58 idiom),
// dedicated port 5591; -short skips. The media_inbox DDL below mirrors the
// SPEC-W61 pinned table exactly (B's ensure* hook converges it at merge —
// CREATE TABLE IF NOT EXISTS).

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

const w61InternalToken = "itok-w61"

// w61MediaInboxDDL mirrors the SPEC-W61 pinned media_inbox table (columns,
// CHECK, UNIQUE, RLS) plus the G3 functional index on contacts. Idempotent
// so it converges with B's ensure* hook at merge.
const w61MediaInboxDDL = `
CREATE TABLE IF NOT EXISTS media_inbox (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL,
    channel              TEXT NOT NULL,
    provider_message_id  TEXT NOT NULL,
    document_id          UUID NOT NULL,
    sender_phone         TEXT NOT NULL,
    sender_phone_digits  TEXT NOT NULL,
    mime                 TEXT NOT NULL,
    bytes                BIGINT NOT NULL,
    sha256_prefix        TEXT NOT NULL,
    state                TEXT NOT NULL DEFAULT 'parked'
        CHECK (state IN ('parked','anchored','linked','discarded')),
    anchor_method        TEXT,
    booking_id           UUID,
    contact_id           UUID,
    linked_by            TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, channel, provider_message_id)
);
ALTER TABLE media_inbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE media_inbox FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON media_inbox;
CREATE POLICY tenant_isolation ON media_inbox
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE INDEX IF NOT EXISTS idx_contacts_phone_digits ON contacts
    (tenant_id, (regexp_replace(regexp_replace(phone,'\D','','g'),'^00','')));
`

type w61Fixture struct {
	t        *testing.T
	router   http.Handler
	st       *store.Store
	conn     *pgx.Conn
	resolver *bookingops.TenantResolver
	tenant   bookingops.TenantInfo
}

// newW61Fixture boots ONE embedded postgres per test (fixed port 5591 —
// tests in this package run sequentially) and seeds the base + media_inbox
// schema. The default router carries w61InternalToken + the real store;
// routerWith() rebuilds variants (fail-closed cases) against the SAME
// database.
func newW61Fixture(t *testing.T) *w61Fixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres W61 media-inbox route test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_w61_test").
		Port(5591).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5591/booking_w61_test?sslmode=disable"
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(ctx) }) //nolint:errcheck
	if _, err := conn.Exec(ctx, w45TestSchema); err != nil {
		t.Fatalf("base schema: %v", err)
	}
	if _, err := conn.Exec(ctx, w61MediaInboxDDL); err != nil {
		t.Fatalf("media_inbox schema: %v", err)
	}

	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme-w61", Name: "Acme W61"}
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

	f := &w61Fixture{t: t, conn: conn, tenant: tenant, resolver: resolver}
	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)
	f.st = st
	f.router = f.routerWith(w61InternalToken, st)
	return f
}

// routerWith rebuilds the full router against the fixture's database with
// the given token/store (nil store => fail-closed wiring proof).
func (f *w61Fixture) routerWith(internalToken string, st *store.Store) http.Handler {
	return NewRouter(Deps{
		Logger:        zap.NewNop(),
		Store:         st,
		Resolver:      f.resolver,
		AuthzDisabled: true, // isolates the requireOwnerAdmin realm-role gate
		InternalToken: internalToken,
	})
}

// staffReq stamps a tenant-scoped staff request (gateway/middleware idiom:
// X-Tenant-Slug + X-User-Id + X-User-Roles) against the given router.
func (f *w61Fixture) staffReq(router http.Handler, method, path, body, roles string) *httptest.ResponseRecorder {
	f.t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("X-Tenant-Slug", f.tenant.Slug)
	r.Header.Set("X-User-Id", "user-"+roles)
	if roles != "" {
		r.Header.Set("X-User-Roles", roles)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

// ingest posts to the machine route with the given token.
func (f *w61Fixture) ingest(router http.Handler, token, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/media/inbox/ingest", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("X-Internal-Token", token)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

func (f *w61Fixture) validIngestBody(messageID string) string {
	return fmt.Sprintf(`{"tenant_id":%q,"channel":"whatsapp","message_id":%q,`+
		`"document_id":%q,"sender_phone":"+234 801 234 5678","sender_phone_digits":"2348012345678",`+
		`"mime":"image/jpeg","bytes":1234,"sha256_prefix":"0123456789abcdef"}`,
		f.tenant.ID.String(), messageID, uuid.NewString())
}

// seedContactBooking inserts a contact (+ optional in-window booking) via
// raw SQL and returns (contactID, bookingID).
func (f *w61Fixture) seedContactBooking(phone string, withBooking bool) (uuid.UUID, uuid.UUID) {
	f.t.Helper()
	ctx := context.Background()
	var contactID, bookingID uuid.UUID
	if err := f.conn.QueryRow(ctx,
		`INSERT INTO contacts (tenant_id, name, phone) VALUES ($1,'Ada Lovelace',$2) RETURNING id`,
		f.tenant.ID, phone).Scan(&contactID); err != nil {
		f.t.Fatalf("seed contact: %v", err)
	}
	if withBooking {
		offeringID := uuid.New()
		if _, err := f.conn.Exec(ctx,
			`INSERT INTO offerings (id, tenant_id, name, duration_min, price_cents, currency)
			 VALUES ($1,$2,'Cut',30,5000,'NGN')`, offeringID, f.tenant.ID); err != nil {
			f.t.Fatalf("seed offering: %v", err)
		}
		if err := f.conn.QueryRow(ctx,
			`INSERT INTO bookings (tenant_id, offering_id, contact_id, starts_at, ends_at, status)
			 VALUES ($1,$2,$3, now() + interval '2 days', now() + interval '2 days' + interval '30 minutes', 'confirmed')
			 RETURNING id`,
			f.tenant.ID, offeringID, contactID).Scan(&bookingID); err != nil {
			f.t.Fatalf("seed booking: %v", err)
		}
	}
	return contactID, bookingID
}

// --- ingest auth matrix -----------------------------------------------------

func TestMediaInboxIngestAuthMatrix(t *testing.T) {
	f := newW61Fixture(t)

	// 503 fail-closed: BOOKING_INTERNAL_TOKEN unset (router variant over
	// the SAME database — a second embedded postgres would collide on the
	// fixed test port).
	routerNoTok := f.routerWith("", f.st)
	if rec := f.ingest(routerNoTok, w61InternalToken, f.validIngestBody("m-503")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unset token must fail closed 503, got %d (%s)", rec.Code, rec.Body.String())
	}

	router := f.router

	// 401: missing + wrong token.
	if rec := f.ingest(router, "", f.validIngestBody("m-401a")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token must 401, got %d", rec.Code)
	}
	if rec := f.ingest(router, "wrong", f.validIngestBody("m-401b")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token must 401, got %d", rec.Code)
	}

	// 400 deterministic validation failures (never reach the store).
	bad := []struct{ name, body string }{
		{"malformed json", `{"channel":`},
		{"bad tenant_id", `{"tenant_id":"nope","channel":"whatsapp","message_id":"m","document_id":"` + uuid.NewString() + `","sender_phone":"+1","mime":"image/jpeg","bytes":1,"sha256_prefix":"0123456789abcdef"}`},
		{"missing tenant", `{"channel":"whatsapp","message_id":"m","document_id":"` + uuid.NewString() + `","sender_phone":"+1","mime":"image/jpeg","bytes":1,"sha256_prefix":"0123456789abcdef"}`},
		{"missing channel", `{"tenant_id":"` + f.tenant.ID.String() + `","message_id":"m","document_id":"` + uuid.NewString() + `","sender_phone":"+1","mime":"image/jpeg","bytes":1,"sha256_prefix":"0123456789abcdef"}`},
		{"missing message_id", `{"tenant_id":"` + f.tenant.ID.String() + `","channel":"whatsapp","document_id":"` + uuid.NewString() + `","sender_phone":"+1","mime":"image/jpeg","bytes":1,"sha256_prefix":"0123456789abcdef"}`},
		{"bad document_id", `{"tenant_id":"` + f.tenant.ID.String() + `","channel":"whatsapp","message_id":"m","document_id":"not-a-uuid","sender_phone":"+1","mime":"image/jpeg","bytes":1,"sha256_prefix":"0123456789abcdef"}`},
		{"missing sender_phone", `{"tenant_id":"` + f.tenant.ID.String() + `","channel":"whatsapp","message_id":"m","document_id":"` + uuid.NewString() + `","mime":"image/jpeg","bytes":1,"sha256_prefix":"0123456789abcdef"}`},
		{"bad sender_phone_digits", `{"tenant_id":"` + f.tenant.ID.String() + `","channel":"whatsapp","message_id":"m","document_id":"` + uuid.NewString() + `","sender_phone":"+1","sender_phone_digits":"+234","mime":"image/jpeg","bytes":1,"sha256_prefix":"0123456789abcdef"}`},
		{"negative bytes", `{"tenant_id":"` + f.tenant.ID.String() + `","channel":"whatsapp","message_id":"m","document_id":"` + uuid.NewString() + `","sender_phone":"+1","mime":"image/jpeg","bytes":-1,"sha256_prefix":"0123456789abcdef"}`},
		{"bad sha256_prefix", `{"tenant_id":"` + f.tenant.ID.String() + `","channel":"whatsapp","message_id":"m","document_id":"` + uuid.NewString() + `","sender_phone":"+1","mime":"image/jpeg","bytes":1,"sha256_prefix":"XYZ!"}`},
	}
	for _, tc := range bad {
		if rec := f.ingest(router, w61InternalToken, tc.body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: must 400, got %d (%s)", tc.name, rec.Code, rec.Body.String())
		}
	}

	// 200 happy path: no contact seeded => parked; id returned.
	rec := f.ingest(router, w61InternalToken, f.validIngestBody("m-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("happy path must 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 200 body: %v", err)
	}
	if resp.State != "parked" || resp.ID == "" {
		t.Fatalf("want parked + id, got %s", rec.Body.String())
	}

	// Idempotent replay (N-05 redelivery): same (tenant,channel,message_id)
	// returns the EXISTING row.
	rec2 := f.ingest(router, w61InternalToken, f.validIngestBody("m-1"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("replay must 200, got %d", rec2.Code)
	}
	var resp2 struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode replay body: %v", err)
	}
	if resp2.ID != resp.ID || resp2.State != "parked" {
		t.Fatalf("replay must return the existing row: %s vs %s", rec2.Body.String(), rec.Body.String())
	}
}

// G3 through the HTTP surface: stored contact phone is UNNORMALIZED
// ('+234 801 234 5678'); the sender needle is digits-only; exactly one
// in-window booking => anchored with booking_id.
func TestMediaInboxIngestAnchorsOnUniqueCandidate(t *testing.T) {
	f := newW61Fixture(t)
	_, bookingID := f.seedContactBooking("+234 801 234 5678", true)

	body := fmt.Sprintf(`{"tenant_id":%q,"channel":"whatsapp","message_id":"m-anchor",`+
		`"document_id":%q,"sender_phone":"2348012345678","sender_phone_digits":"2348012345678",`+
		`"mime":"image/jpeg","bytes":10,"sha256_prefix":"0123456789abcdef"}`,
		f.tenant.ID.String(), uuid.NewString())
	rec := f.ingest(f.router, w61InternalToken, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("anchor ingest must 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID        string `json:"id"`
		State     string `json:"state"`
		BookingID string `json:"booking_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.State != "anchored" || resp.BookingID != bookingID.String() {
		t.Fatalf("want anchored to %s, got %s", bookingID, rec.Body.String())
	}
}

// --- staff authz (KYC P5 posture) ------------------------------------------

func TestMediaInboxStaffAuthz(t *testing.T) {
	f := newW61Fixture(t)
	rowID := uuid.NewString()
	for _, roles := range []string{"member", ""} {
		if rec := f.staffReq(f.router, http.MethodGet, "/v1/media/inbox", "", roles); rec.Code != http.StatusForbidden {
			t.Fatalf("list roles=%q must 403, got %d", roles, rec.Code)
		}
		if rec := f.staffReq(f.router, http.MethodPost, "/v1/media/inbox/"+rowID+"/link", `{"booking_id":"`+uuid.NewString()+`"}`, roles); rec.Code != http.StatusForbidden {
			t.Fatalf("link roles=%q must 403, got %d", roles, rec.Code)
		}
		if rec := f.staffReq(f.router, http.MethodPost, "/v1/media/inbox/"+rowID+"/discard", `{}`, roles); rec.Code != http.StatusForbidden {
			t.Fatalf("discard roles=%q must 403, got %d", roles, rec.Code)
		}
	}
	// owner + admin pass the gate (list answers 200 with an empty page).
	for _, roles := range []string{"owner", "admin"} {
		if rec := f.staffReq(f.router, http.MethodGet, "/v1/media/inbox", "", roles); rec.Code != http.StatusOK {
			t.Fatalf("list roles=%q must 200, got %d (%s)", roles, rec.Code, rec.Body.String())
		}
	}
}

// --- staff list / link / discard --------------------------------------------

func (f *w61Fixture) seedInboxRow(t *testing.T, messageID, phone string) store.MediaInboxItem {
	t.Helper()
	it, err := f.st.IngestMediaInbox(context.Background(), store.MediaInboxIngest{
		TenantID:          f.tenant.ID,
		Channel:           "whatsapp",
		ProviderMessageID: messageID,
		DocumentID:        uuid.New(),
		SenderPhone:       phone,
		Mime:              "image/jpeg",
		Bytes:             10,
		SHA256Prefix:      "0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("seed inbox row: %v", err)
	}
	return it
}

func TestMediaInboxStaffList(t *testing.T) {
	f := newW61Fixture(t)
	a := f.seedInboxRow(t, "l-1", "+111")
	time.Sleep(5 * time.Millisecond) // deterministic created_at ordering
	b := f.seedInboxRow(t, "l-2", "+222")
	c := f.seedInboxRow(t, "l-3", "+333")
	if err := f.st.DiscardMediaInbox(context.Background(), f.tenant.ID, c.ID, "user-owner"); err != nil {
		t.Fatalf("discard seed: %v", err)
	}

	// Default page: all three rows, newest first.
	rec := f.staffReq(f.router, http.MethodGet, "/v1/media/inbox", "", "owner")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"items"`
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(page.Items) != 3 || page.Items[0].ID != c.ID.String() || page.Items[2].ID != a.ID.String() {
		t.Fatalf("want 3 rows newest-first, got %s", rec.Body.String())
	}

	// State filter: parked only => a, b.
	rec = f.staffReq(f.router, http.MethodGet, "/v1/media/inbox?state=parked", "", "owner")
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Items) != 2 {
		t.Fatalf("state=parked must return 2 rows, got %s", rec.Body.String())
	}
	if page.Items[0].ID != b.ID.String() || page.Items[1].ID != a.ID.String() {
		t.Fatalf("parked order wrong: %s", rec.Body.String())
	}

	// Pagination: limit=1&offset=1 over parked => a.
	rec = f.staffReq(f.router, http.MethodGet, "/v1/media/inbox?state=parked&limit=1&offset=1", "", "owner")
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Items) != 1 || page.Items[0].ID != a.ID.String() {
		t.Fatalf("limit/offset page wrong: %s", rec.Body.String())
	}

	// Validation: bad state / limit / offset => 400.
	for _, path := range []string{
		"/v1/media/inbox?state=nope",
		"/v1/media/inbox?limit=0",
		"/v1/media/inbox?limit=201",
		"/v1/media/inbox?limit=abc",
		"/v1/media/inbox?offset=-1",
	} {
		if rec := f.staffReq(f.router, http.MethodGet, path, "", "owner"); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s must 400, got %d", path, rec.Code)
		}
	}
}

func TestMediaInboxLinkAndDiscard(t *testing.T) {
	f := newW61Fixture(t)
	contactID, bookingID := f.seedContactBooking("+2348019998877", true)
	row := f.seedInboxRow(t, "link-1", "+2348019998877")

	// Link happy path: state=linked, anchor_method=staff, contact_id from
	// the booking, linked_by=caller.
	rec := f.staffReq(f.router, http.MethodPost, "/v1/media/inbox/"+row.ID.String()+"/link",
		`{"booking_id":"`+bookingID.String()+`"}`, "owner")
	if rec.Code != http.StatusOK {
		t.Fatalf("link must 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var linked struct {
		State        string `json:"state"`
		AnchorMethod string `json:"anchor_method"`
		BookingID    string `json:"booking_id"`
		ContactID    string `json:"contact_id"`
		LinkedBy     string `json:"linked_by"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &linked); err != nil {
		t.Fatalf("decode link: %v", err)
	}
	if linked.State != "linked" || linked.AnchorMethod != "staff" ||
		linked.BookingID != bookingID.String() || linked.ContactID != contactID.String() ||
		linked.LinkedBy != "user-owner" {
		t.Fatalf("linked projection wrong: %s", rec.Body.String())
	}

	// Invalid booking: bad uuid => 400; well-formed but foreign => 404.
	row2 := f.seedInboxRow(t, "link-2", "+2348019998877")
	if rec := f.staffReq(f.router, http.MethodPost, "/v1/media/inbox/"+row2.ID.String()+"/link",
		`{"booking_id":"not-a-uuid"}`, "owner"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad booking_id must 400, got %d", rec.Code)
	}
	if rec := f.staffReq(f.router, http.MethodPost, "/v1/media/inbox/"+row2.ID.String()+"/link",
		`{"booking_id":"`+uuid.NewString()+`"}`, "owner"); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign booking must 404, got %d", rec.Code)
	}
	// Missing inbox row => 404.
	if rec := f.staffReq(f.router, http.MethodPost, "/v1/media/inbox/"+uuid.NewString()+"/link",
		`{"booking_id":"`+bookingID.String()+`"}`, "owner"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing row must 404, got %d", rec.Code)
	}

	// Discard happy path + missing => 404.
	rec = f.staffReq(f.router, http.MethodPost, "/v1/media/inbox/"+row2.ID.String()+"/discard", `{}`, "admin")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"discarded"`) {
		t.Fatalf("discard must 200 discarded, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.staffReq(f.router, http.MethodPost, "/v1/media/inbox/"+uuid.NewString()+"/discard", `{}`, "owner"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing discard row must 404, got %d", rec.Code)
	}
	// Bad row id => 400 on both actions.
	if rec := f.staffReq(f.router, http.MethodPost, "/v1/media/inbox/nope/link", `{"booking_id":"`+uuid.NewString()+`"}`, "owner"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id link must 400, got %d", rec.Code)
	}
	if rec := f.staffReq(f.router, http.MethodPost, "/v1/media/inbox/nope/discard", `{}`, "owner"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id discard must 400, got %d", rec.Code)
	}
}

// Fail-closed wiring proof: nil store => the routes stay mounted and
// answer 503 (never 404, never panic).
func TestMediaInboxRoutesWiredFailClosed(t *testing.T) {
	f := newW61Fixture(t)
	routerNilStore := f.routerWith(w61InternalToken, nil)
	f.router = routerNilStore
	if rec := f.staffReq(f.router, http.MethodGet, "/v1/media/inbox", "", "owner"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("list with nil store must 503, got %d", rec.Code)
	}
	if rec := f.staffReq(f.router, http.MethodPost, "/v1/media/inbox/"+uuid.NewString()+"/discard", `{}`, "owner"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("discard with nil store must 503, got %d", rec.Code)
	}
	if rec := f.ingest(routerNilStore, w61InternalToken, f.validIngestBody("m-nil")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ingest with nil store must 503, got %d", rec.Code)
	}
}
