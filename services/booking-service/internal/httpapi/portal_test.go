package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/events"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// End-to-end portal tests (Wave 5 #7) against a real (embedded) Postgres:
// request-code → verify → portal-JWT → contact-scoped booking operations.
// Set STORE_TEST=0 / -short to skip in constrained environments.

const portalTestSchema = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE IF NOT EXISTS offerings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    duration_min INTEGER NOT NULL,
    buffer_min INTEGER NOT NULL DEFAULT 0,
    price_cents INTEGER NOT NULL DEFAULT 0,
    currency CHAR(3) NOT NULL DEFAULT 'USD',
    capacity INTEGER NOT NULL DEFAULT 1,
    bookable BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS team_members (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    email TEXT,
    role TEXT NOT NULL DEFAULT 'staff',
    active BOOLEAN NOT NULL DEFAULT TRUE
);
CREATE TABLE IF NOT EXISTS availability_rules (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    team_member_id UUID NOT NULL,
    weekday SMALLINT NOT NULL,
    start_min SMALLINT NOT NULL,
    end_min SMALLINT NOT NULL,
    effective_from DATE,
    effective_to DATE
);
CREATE TABLE IF NOT EXISTS contacts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    phone TEXT,
    email TEXT,
    notes TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS bookings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    offering_id UUID NOT NULL,
    team_member_id UUID,
    contact_id UUID,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    source TEXT NOT NULL DEFAULT 'api',
    idempotency_key TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    sent_at TIMESTAMPTZ
);`

// fakePublisher captures published CloudEvents.
type fakePublisher struct {
	events []events.CloudEvent
	err    error
}

func (f *fakePublisher) PublishEvent(_ context.Context, _, _ string, data any) error {
	if f.err != nil {
		return f.err
	}
	if evt, ok := data.(events.CloudEvent); ok {
		f.events = append(f.events, evt)
	}
	return nil
}

type portalFixture struct {
	handler   http.Handler
	store     *store.Store
	publisher *fakePublisher
	tenantID  uuid.UUID
	site      store.Site
	offering  store.Offering
	member    store.TeamMember
	// slotSeq staggers fixture bookings onto distinct slots: the K-01
	// in-transaction overlap re-check rightly rejects two capacity-1
	// bookings on the same slot, even in fixtures.
	slotSeq int
}

func newPortalFixture(t *testing.T) *portalFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres portal test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_portal_test"))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5432/booking_portal_test?sslmode=disable"
	// Apply the minimal booking schema via a raw connection (store.New only
	// bootstraps its own additive tables: sites, waitlist, portal_tokens).
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

	pub := &fakePublisher{}
	ops := &bookingops.Service{Store: st, EventsTopic: "test.events", Logger: zap.NewNop()}
	handler := NewRouter(Deps{
		Store:              st,
		Ops:                ops,
		AuthzDisabled:      true,
		Logger:             zap.NewNop(),
		PortalSecret:       "test-portal-secret",
		PubSubName:         "test-pubsub",
		NotificationsTopic: "test.notifications",
		Publisher:          pub,
		TenantBySlug: func(_ context.Context, slug string) (bookingops.TenantInfo, error) {
			return bookingops.TenantInfo{Slug: slug, Timezone: "UTC"}, nil
		},
	})

	f := &portalFixture{handler: handler, store: st, publisher: pub, tenantID: uuid.New()}

	f.site = store.Site{TenantID: f.tenantID, TenantSlug: "acme", Slug: "acme-books", DisplayName: "Acme"}
	if err := st.CreateSite(ctx, &f.site); err != nil {
		t.Fatalf("site: %v", err)
	}
	f.offering = store.Offering{TenantID: f.tenantID, Name: "Cut", DurationMin: 30, Capacity: 1}
	if err := st.CreateOffering(ctx, &f.offering); err != nil {
		t.Fatalf("offering: %v", err)
	}
	f.member = store.TeamMember{TenantID: f.tenantID, Name: "Ana", Active: true}
	if err := st.CreateTeamMember(ctx, &f.member); err != nil {
		t.Fatalf("member: %v", err)
	}
	rules := make([]store.AvailabilityRule, 0, 7)
	for wd := 0; wd < 7; wd++ {
		rules = append(rules, store.AvailabilityRule{
			TenantID: f.tenantID, TeamMemberID: f.member.ID, Weekday: wd, StartMin: 8 * 60, EndMin: 20 * 60,
		})
	}
	if err := st.SetAvailability(ctx, f.tenantID, f.member.ID, rules); err != nil {
		t.Fatalf("rules: %v", err)
	}
	return f
}

// addContact creates a contact with one confirmed booking at noon tomorrow.
func (f *portalFixture) addContact(t *testing.T, name, phone string) (store.Contact, store.Booking) {
	t.Helper()
	ctx := context.Background()
	c := store.Contact{TenantID: f.tenantID, Name: name, Phone: phone}
	if err := f.store.CreateContact(ctx, &c); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, 1).
		Add(time.Duration(f.slotSeq) * 45 * time.Minute) // stagger: see slotSeq
	f.slotSeq++
	b := store.Booking{
		TenantID: f.tenantID, OfferingID: f.offering.ID, TeamMemberID: f.member.ID,
		ContactID: c.ID, StartsAt: start, EndsAt: start.Add(30 * time.Minute),
		Status: store.StatusConfirmed, Source: "web",
	}
	if err := f.store.CreateBookingTx(ctx, &b, store.SlotGuard{}, "test.events", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	return c, b
}

func doJSON(t *testing.T, h http.Handler, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// requestAndVerify runs the request→verify flow and returns the portal JWT.
func (f *portalFixture) requestAndVerify(t *testing.T, phone string) string {
	t.Helper()
	code, resp := doJSON(t, f.handler, "POST", "/public/sites/acme-books/portal/request", "", map[string]string{"phone": phone})
	if code != http.StatusAccepted {
		t.Fatalf("request code = %d %v", code, resp)
	}
	if len(f.publisher.events) == 0 {
		t.Fatal("no SendPortalCode event published")
	}
	evt := f.publisher.events[len(f.publisher.events)-1]
	if evt.Type != portalEventType {
		t.Fatalf("event type = %q", evt.Type)
	}
	if evt.Data["destination"] != phone || evt.Data["channel"] != "sms" {
		t.Fatalf("event data = %v", evt.Data)
	}
	plainCode, _ := evt.Data["code"].(string)
	if len(plainCode) != 6 {
		t.Fatalf("code = %q, want 6 digits", plainCode)
	}

	code, resp = doJSON(t, f.handler, "POST", "/public/sites/acme-books/portal/verify", "",
		map[string]string{"phone": phone, "code": plainCode})
	if code != http.StatusOK {
		t.Fatalf("verify = %d %v", code, resp)
	}
	token, _ := resp["portal_token"].(string)
	if token == "" {
		t.Fatalf("no portal_token in %v", resp)
	}
	return token
}

func TestPortalLifecycle(t *testing.T) {
	f := newPortalFixture(t)
	_, booking := f.addContact(t, "Pia", "+15550101")

	token := f.requestAndVerify(t, "+15550101")

	// List own bookings.
	code, resp := doJSON(t, f.handler, "GET", "/portal/bookings", token, nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d %v", code, resp)
	}
	bookings, _ := resp["bookings"].([]any)
	if len(bookings) != 1 {
		t.Fatalf("bookings = %d, want 1", len(bookings))
	}

	// Reschedule to 13:00 tomorrow (inside the 08:00–20:00 rules).
	newStart := booking.StartsAt.Add(time.Hour)
	code, resp = doJSON(t, f.handler, "POST", "/portal/bookings/"+booking.ID.String()+"/reschedule", token,
		map[string]string{"starts_at": newStart.Format(time.RFC3339)})
	if code != http.StatusOK {
		t.Fatalf("reschedule = %d %v", code, resp)
	}

	// Cancel.
	code, resp = doJSON(t, f.handler, "POST", "/portal/bookings/"+booking.ID.String()+"/cancel", token, nil)
	if code != http.StatusOK {
		t.Fatalf("cancel = %d %v", code, resp)
	}
	if resp["status"] != store.StatusCancelled {
		t.Fatalf("status = %v, want cancelled", resp["status"])
	}
}

func TestPortalWrongCodeLockout(t *testing.T) {
	f := newPortalFixture(t)
	f.addContact(t, "Kim", "+15550202")

	code, _ := doJSON(t, f.handler, "POST", "/public/sites/acme-books/portal/request", "", map[string]string{"phone": "+15550202"})
	if code != http.StatusAccepted {
		t.Fatalf("request = %d", code)
	}
	// 5 wrong codes → each 401; the 6th attempt is locked out (429), even
	// with the CORRECT code.
	for i := 0; i < portalMaxAttempts; i++ {
		code, _ = doJSON(t, f.handler, "POST", "/public/sites/acme-books/portal/verify", "",
			map[string]string{"phone": "+15550202", "code": "000000"})
		if code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, code)
		}
	}
	realCode, _ := f.publisher.events[len(f.publisher.events)-1].Data["code"].(string)
	code, resp := doJSON(t, f.handler, "POST", "/public/sites/acme-books/portal/verify", "",
		map[string]string{"phone": "+15550202", "code": realCode})
	if code != http.StatusTooManyRequests {
		t.Fatalf("locked attempt = %d %v, want 429", code, resp)
	}
}

func TestPortalContactScoping(t *testing.T) {
	f := newPortalFixture(t)
	f.addContact(t, "Ann", "+15550303")
	_, otherBooking := f.addContact(t, "Bob", "+15550404")

	token := f.requestAndVerify(t, "+15550303")

	// Ann's session lists only Ann's booking.
	code, resp := doJSON(t, f.handler, "GET", "/portal/bookings", token, nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	bookings, _ := resp["bookings"].([]any)
	if len(bookings) != 1 {
		t.Fatalf("bookings = %d, want exactly 1 (contact-scoped)", len(bookings))
	}

	// Bob's booking is invisible: reschedule/cancel both 404.
	code, _ = doJSON(t, f.handler, "POST", "/portal/bookings/"+otherBooking.ID.String()+"/reschedule", token,
		map[string]string{"starts_at": otherBooking.StartsAt.Add(time.Hour).Format(time.RFC3339)})
	if code != http.StatusNotFound {
		t.Fatalf("cross-contact reschedule = %d, want 404", code)
	}
	code, _ = doJSON(t, f.handler, "POST", "/portal/bookings/"+otherBooking.ID.String()+"/cancel", token, nil)
	if code != http.StatusNotFound {
		t.Fatalf("cross-contact cancel = %d, want 404", code)
	}

	// No token at all → 401.
	code, _ = doJSON(t, f.handler, "GET", "/portal/bookings", "", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list = %d, want 401", code)
	}

	// The X-Portal-Token pass-through header (BFF path) works too.
	req := httptest.NewRequest("GET", "/portal/bookings", nil)
	req.Header.Set("X-Portal-Token", token)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("X-Portal-Token list = %d, want 200", rec.Code)
	}
}

func TestPortalRequestRateLimit(t *testing.T) {
	f := newPortalFixture(t)
	f.addContact(t, "Max", "+15550505")

	var code int
	for i := 0; i < portalRateLimit; i++ {
		code, _ = doJSON(t, f.handler, "POST", "/public/sites/acme-books/portal/request", "", map[string]string{"phone": "+15550505"})
		if code != http.StatusAccepted {
			t.Fatalf("request %d = %d, want 202", i+1, code)
		}
	}
	code, _ = doJSON(t, f.handler, "POST", "/public/sites/acme-books/portal/request", "", map[string]string{"phone": "+15550505"})
	if code != http.StatusTooManyRequests {
		t.Fatalf("request %d = %d, want 429", portalRateLimit+1, code)
	}
}

func TestPortalRequestUnknownContactIsIndistinguishable(t *testing.T) {
	f := newPortalFixture(t)
	code, resp := doJSON(t, f.handler, "POST", "/public/sites/acme-books/portal/request", "", map[string]string{"phone": "+19999999"})
	if code != http.StatusAccepted || resp["status"] != "code_sent" {
		t.Fatalf("unknown contact = %d %v, want 202 code_sent", code, resp)
	}
	if len(f.publisher.events) != 0 {
		t.Fatal("no event may be published for unknown contacts")
	}
}

func TestPortalJWTRoundtrip(t *testing.T) {
	claims := portalClaims{
		Sub: uuid.NewString(), TenantID: uuid.NewString(), TenantSlug: "acme",
		Scope: portalClaimScope, IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix(),
	}
	tok, err := signPortalJWT("secret", claims)
	if err != nil {
		t.Fatal(err)
	}
	got, err := verifyPortalJWT("secret", tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Sub != claims.Sub || got.TenantSlug != "acme" {
		t.Fatalf("claims = %+v", got)
	}
	// Wrong secret and tampered tokens are rejected.
	if _, err := verifyPortalJWT("other", tok); err == nil {
		t.Fatal("wrong secret accepted")
	}
	if _, err := verifyPortalJWT("secret", tok[:len(tok)-2]+"xx"); err == nil {
		t.Fatal("tampered token accepted")
	}
	// Expired tokens are rejected.
	claims.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	expired, _ := signPortalJWT("secret", claims)
	if _, err := verifyPortalJWT("secret", expired); err == nil {
		t.Fatal("expired token accepted")
	}
}
