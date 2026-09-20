package httpapi

// SPEC-W45 CODER-A route tests: booking complete/refund (K11/K12), the QR
// scan ingest + analytics read (item 9), and the referral agent registry
// with the admin-only approval gate + unknown-referrer 422 (items 6+7).
// Embedded-postgres harness (dedicated port 5571; -short skips).

import (
	"context"
	"encoding/json"
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
	"github.com/opendesk/booking-service/internal/referrals"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

const w45TestSchema = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE IF NOT EXISTS offerings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
    duration_min INTEGER NOT NULL, buffer_min INTEGER NOT NULL DEFAULT 0,
    price_cents INTEGER NOT NULL DEFAULT 0, currency CHAR(3) NOT NULL DEFAULT 'USD',
    capacity INTEGER NOT NULL DEFAULT 1, bookable BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
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
CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(), aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL, payload JSONB NOT NULL, sent_at TIMESTAMPTZ
);`

type w45Fixture struct {
	t      *testing.T
	router http.Handler
	st     *store.Store
	dsn    string
	tenant bookingops.TenantInfo
}

func newW45Fixture(t *testing.T, payments *bookingops.PaymentsClient) *w45Fixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres W45 route test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_w45_test").
		Port(5571).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5571/booking_w45_test?sslmode=disable"
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

	f := &w45Fixture{t: t, st: st, dsn: dsn, tenant: tenant}
	f.router = NewRouter(Deps{
		Logger:   zap.NewNop(),
		Store:    st,
		Resolver: resolver,
		Ops: &bookingops.Service{
			Store: st, EventsTopic: "booking.events", Logger: zap.NewNop(), Payments: payments,
		},
		Referrals:     &referrals.Service{Store: st, Ledger: referrals.NewPostgresLedger(st), Log: zap.NewNop()},
		AuthzDisabled: true,
	})
	return f
}

// do issues one tenant-scoped request and returns the recorder.
func (f *w45Fixture) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
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

func (f *w45Fixture) seedBooking(t *testing.T, status string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	offering := store.Offering{TenantID: f.tenant.ID, Name: "Cut", DurationMin: 30, PriceCents: 5000, Currency: "NGN"}
	if err := f.st.CreateOffering(ctx, &offering); err != nil {
		t.Fatal(err)
	}
	contact := store.Contact{TenantID: f.tenant.ID, Name: "Ada", Phone: "+234800"}
	if err := f.st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(72 * time.Hour)
	b := store.Booking{TenantID: f.tenant.ID, OfferingID: offering.ID, ContactID: contact.ID,
		StartsAt: start, EndsAt: start.Add(30 * time.Minute), Status: status, Source: "api"}
	if err := f.st.CreateBookingTx(ctx, &b, store.SlotGuard{}, "test.events", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	return b.ID
}

// K11/K12 routes: complete (200 + capture via the rail stub) and the
// fail-closed refund (503 with the config signal).
func TestW45CompleteAndRefundRoutes(t *testing.T) {
	var capturePath string
	rail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturePath = r.URL.Path
		if r.URL.Path == "/v1/refunds" {
			// K12 RefundResponse shape — the honest rail outcome must
			// surface through the refund endpoint body (V2-advisory).
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "rf-1", "refund_id": "rf-1", "amount": 5000, "amount_cents": 5000,
				"status": "queued_manual", "rail": "none",
				"rail_detail": "no provider transaction id on deposit; manual refund required",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"deposit_id": "dep-1", "result": map[string]any{}})
	}))
	defer rail.Close()

	f := newW45Fixture(t, bookingops.NewPaymentsClient(rail.URL, "tok", zap.NewNop()))
	bookingID := f.seedBooking(t, store.StatusConfirmed)

	// Unauthenticated → 401 from require() before the handler.
	if rec := f.do(http.MethodPost, "/v1/bookings/"+bookingID.String()+"/complete", `{}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-JWT complete = %d, want 401", rec.Code)
	}
	// Complete with a deposit reference → 200, capture hit the rail.
	depositID := uuid.New()
	rec := f.do(http.MethodPost, "/v1/bookings/"+bookingID.String()+"/complete",
		`{"deposit_id":"`+depositID.String()+`"}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("complete = %d: %s", rec.Code, rec.Body.String())
	}
	if capturePath != "/v1/deposits/"+depositID.String()+"/capture" {
		t.Fatalf("rail capture path = %q", capturePath)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["booking"].(map[string]any)["status"] != "completed" {
		t.Fatalf("complete body: %s", rec.Body.String())
	}
	b, err := f.st.GetBooking(context.Background(), f.tenant.ID, bookingID)
	if err != nil || b.Status != store.StatusCompleted {
		t.Fatalf("booking after complete: %q %v", b.Status, err)
	}

	// Refund against the same rail → 201, and the K12 rail outcome
	// (status/rail/rail_detail) surfaces through the endpoint response
	// (V2-advisory: a queued_manual refund must never read as "posted").
	rec = f.do(http.MethodPost, "/v1/bookings/"+bookingID.String()+"/refund",
		`{"amount_cents":5000,"reason":"customer request"}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("refund = %d: %s", rec.Code, rec.Body.String())
	}
	var refundBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &refundBody); err != nil {
		t.Fatal(err)
	}
	if refundBody["status"] != "queued_manual" || refundBody["rail"] != "none" ||
		refundBody["rail_detail"] != "no provider transaction id on deposit; manual refund required" {
		t.Fatalf("refund response masks the rail outcome: %s", rec.Body.String())
	}
}

// Fail-closed money posture end-to-end: PAYMENTS_URL unset (nil client) →
// complete-with-deposit and refund answer 503 carrying the config signal.
func TestW45MoneyEndpointsFailClosedWithoutRail(t *testing.T) {
	f := newW45Fixture(t, nil)
	bookingID := f.seedBooking(t, store.StatusConfirmed)

	rec := f.do(http.MethodPost, "/v1/bookings/"+bookingID.String()+"/complete",
		`{"deposit_id":"`+uuid.NewString()+`"}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "PAYMENTS_URL") {
		t.Fatalf("complete-with-deposit without rail = %d %q, want 503 + config signal", rec.Code, rec.Body.String())
	}
	rec = f.do(http.MethodPost, "/v1/bookings/"+bookingID.String()+"/refund",
		`{"amount_cents":100}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "PAYMENTS_URL") {
		t.Fatalf("refund without rail = %d %q, want 503 + config signal", rec.Code, rec.Body.String())
	}
	// Completing WITHOUT a deposit reference stays possible rail-less.
	rec = f.do(http.MethodPost, "/v1/bookings/"+bookingID.String()+"/complete", `{}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("rail-less complete without deposit = %d: %s", rec.Code, rec.Body.String())
	}
}

// QR scan ingest (public, slug-bound, rate-limited) + the analytics read.
func TestW45QRScanIngestAndRead(t *testing.T) {
	f := newW45Fixture(t, nil)
	ctx := context.Background()
	site := store.Site{TenantID: f.tenant.ID, TenantSlug: f.tenant.Slug, Slug: f.tenant.Slug, DisplayName: "Acme"}
	if err := f.st.CreateSite(ctx, &site); err != nil {
		t.Fatal(err)
	}
	// Publish so the public slug resolution (GetSiteBySlug) finds it.
	conn, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx) //nolint:errcheck
	if _, err := conn.Exec(ctx, `UPDATE sites SET published=true WHERE slug=$1`, f.tenant.Slug); err != nil {
		t.Fatal(err)
	}

	// Unknown slug → 404 (no tenant middleware on this public route). The
	// validation probes below use a DISTINCT source IP: the ingest rate
	// limiter is per-IP and counts requests before slug validation, so
	// probes from the default IP would eat into the 30-scan budget asserted
	// below.
	req := httptest.NewRequest(http.MethodPost, "/v1/qr/scan", strings.NewReader(`{"slug":"nope-ng"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.7:4321"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown slug scan = %d, want 404", rec.Code)
	}
	// Bad body → 400.
	badReq := httptest.NewRequest(http.MethodPost, "/v1/qr/scan", strings.NewReader(`{}`))
	badReq.RemoteAddr = "203.0.113.7:4321"
	rec = httptest.NewRecorder()
	f.router.ServeHTTP(rec, badReq)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty slug = %d, want 400", rec.Code)
	}
	// 30 scans accepted, the 31st rate-limited.
	for i := 0; i < 30; i++ {
		rec = httptest.NewRecorder()
		f.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/qr/scan",
			strings.NewReader(`{"slug":"acme-ng","ref":"poster-a"}`)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("scan %d = %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	rec = httptest.NewRecorder()
	f.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/qr/scan", strings.NewReader(`{"slug":"acme-ng"}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("31st scan = %d, want 429", rec.Code)
	}
	// The analytics read (tenant-scoped).
	rec = f.do(http.MethodGet, "/v1/qr/scans", ``, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/qr/scans = %d: %s", rec.Code, rec.Body.String())
	}
	var sum struct {
		TotalScans int64 `json:"total_scans"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.TotalScans != 30 {
		t.Fatalf("total_scans = %d, want 30", sum.TotalScans)
	}
}

// Referral agent registry routes: staff registration, ADMIN-ONLY approval,
// and the unknown-referrer 422 on referral create.
func TestW45ReferralAgentRoutes(t *testing.T) {
	f := newW45Fixture(t, nil)

	// Unknown referrer → 422.
	rec := f.do(http.MethodPost, "/v1/referrals",
		`{"referrer_type":"contact","referrer_id":"`+uuid.NewString()+`","referee_phone":"+2348011112222"}`,
		map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown referrer = %d: %s, want 422", rec.Code, rec.Body.String())
	}

	// Staff registers an agent (pending).
	rec = f.do(http.MethodPost, "/v1/referrals/agents",
		`{"name":"Field Agent","phone":"+2347099990001"}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create agent = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Agent struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Agent.Status != "pending" {
		t.Fatalf("agent status = %q, want pending", created.Agent.Status)
	}

	// PATCH without the admin role → 403.
	rec = f.do(http.MethodPatch, "/v1/referrals/agents/"+created.Agent.ID,
		`{"status":"approved","beneficiary_id":"`+uuid.NewString()+`"}`, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin PATCH = %d: %s, want 403", rec.Code, rec.Body.String())
	}
	// PATCH with the admin role (X-User-Roles fallback, no JWT roles) → 200.
	rec = f.do(http.MethodPatch, "/v1/referrals/agents/"+created.Agent.ID,
		`{"status":"approved","beneficiary_id":"`+uuid.NewString()+`"}`,
		map[string]string{"X-User-Id": "op-1", "X-User-Roles": "admin"})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin PATCH = %d: %s", rec.Code, rec.Body.String())
	}
	var updated struct {
		Agent struct {
			Status        string `json:"status"`
			BeneficiaryID string `json:"beneficiary_id"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Agent.Status != "approved" || updated.Agent.BeneficiaryID == "" {
		t.Fatalf("approved agent: %s", rec.Body.String())
	}

	// List shows the approved agent.
	rec = f.do(http.MethodGet, "/v1/referrals/agents?status=approved", ``, map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Field Agent") {
		t.Fatalf("list approved = %d: %s", rec.Code, rec.Body.String())
	}

	// The approved agent now passes BOTH referral gates (create + verify).
	rec = f.do(http.MethodPost, "/v1/referrals",
		`{"referrer_type":"agent","referrer_id":"`+created.Agent.ID+`","referee_phone":"+2348011113333"}`,
		map[string]string{"X-User-Id": "op-1"})
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("agent referral create = %d: %s", rec.Code, rec.Body.String())
	}
}
