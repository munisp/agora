package httpapi

// SPEC-W48 (W48-A item A5) route tests: incident resolve, waitlist
// notify/expire, public pay leg (stubbed payments rail), public review leg,
// authz fail-closed. Embedded-postgres harness (dedicated port 5573;
// -short skips) mirroring waitlist_claim_test.go.

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/opendesk/booking-service/internal/incidents"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

const w48TestPort = 5573

type w48Fixture struct {
	t           *testing.T
	router      http.Handler
	st          *store.Store
	raw         *pgx.Conn // superuser SQL for fixture seeds/forced states
	tenant      bookingops.TenantInfo
	offer       store.Offering
	siteSlug    string
	identityURL string
	seedCount   int
	payments    *httptest.Server
	paymentsReq map[string]any // last initialize body the stub saw
}

// newW48Fixture builds the router over a real store. paymentsURL empty
// leaves Ops.Payments nil (the 503 fail-closed posture); pass a stub URL to
// exercise the pay leg.
func newW48Fixture(t *testing.T, paymentsURL string) *w48Fixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres W48 route test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_w48_test").
		Port(w48TestPort).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/booking_w48_test?sslmode=disable", w48TestPort)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	// Minimal base schema BEFORE store.New (portal_test.go idiom) so the
	// W48 migrations (bookings payment columns) see the table.
	if _, err := conn.Exec(ctx, w45mTestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}

	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)

	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme-w48", Name: "Acme W48",
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

	f := &w48Fixture{t: t, st: st, raw: conn, tenant: tenant, siteSlug: "acme-w48-site", identityURL: identity.URL}
	t.Cleanup(func() { conn.Close(context.Background()) }) //nolint:errcheck

	var paymentsClient *bookingops.PaymentsClient
	if paymentsURL != "" {
		paymentsClient = bookingops.NewPaymentsClient(paymentsURL, "tok", zap.NewNop())
	}
	f.router = f.buildRouter(Deps{AuthzDisabled: true}, paymentsClient, f.newResolver())

	// Seed: one bookable offering (priced) + the tenant's published site row.
	f.offer = store.Offering{TenantID: tenant.ID, Name: "Consult", DurationMin: 30,
		PriceCents: 7500, Currency: "NGN", Bookable: true}
	if err := st.CreateOffering(ctx, &f.offer); err != nil {
		t.Fatalf("offering: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO sites (tenant_id, tenant_slug, slug, display_name, published)
		 VALUES ($1,$2,$3,$4,TRUE)`, tenant.ID, tenant.Slug, f.siteSlug, "Acme W48"); err != nil {
		t.Fatalf("site: %v", err)
	}
	return f
}

// newResolver builds a tenant resolver against the fixture identity stub.
func (f *w48Fixture) newResolver() *bookingops.TenantResolver {
	return bookingops.NewTenantResolver(daprc.New("localhost", 1), "identity", time.Minute, zap.NewNop(),
		bookingops.WithIdentityBaseURL(f.identityURL))
}

// buildRouter composes the router; the overlay's authz posture replaces the
// default (AuthzDisabled=true) so fail-closed tests can inject a failing
// Permify.
func (f *w48Fixture) buildRouter(overlay Deps, payments *bookingops.PaymentsClient, resolver *bookingops.TenantResolver) http.Handler {
	d := Deps{
		Logger:   zap.NewNop(),
		Store:    f.st,
		Resolver: resolver,
		Ops: &bookingops.Service{
			Store: f.st, EventsTopic: "booking.events", Logger: zap.NewNop(),
			Payments: payments,
		},
		Incidents:         &incidents.Service{Store: f.st, EventsTopic: "booking.events", Log: zap.NewNop()},
		Authz:             overlay.Authz,
		AuthzDisabled:     overlay.AuthzDisabled,
		AuthzOutagePolicy: overlay.AuthzOutagePolicy,
		// SPEC-W49 B2: pay-leg redirect_url allow-list overlay (nil = the
		// fail-closed posture).
		PayRedirectAllowHosts: overlay.PayRedirectAllowHosts,
	}
	return NewRouter(d)
}

func (f *w48Fixture) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
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

// tenantDo issues a staff request (tenant header + operator id).
func (f *w48Fixture) tenantDo(method, path, body string) *httptest.ResponseRecorder {
	return f.do(method, path, body, map[string]string{"X-Tenant-Slug": f.tenant.Slug, "X-User-Id": "op-1"})
}

// seedBooking inserts one booking for the fixture tenant via the real tx.
// Distinct offsets keep fixture slots disjoint (the K-01 in-transaction
// overlap guard rejects same-slot seeds).
func (f *w48Fixture) seedBooking(status string) uuid.UUID {
	f.t.Helper()
	f.seedCount++
	start := time.Now().UTC().Add(time.Duration(48+f.seedCount*2) * time.Hour)
	b := store.Booking{
		TenantID: f.tenant.ID, OfferingID: f.offer.ID,
		StartsAt: start, EndsAt: start.Add(30 * time.Minute),
		Status: status, Source: "web",
	}
	if err := f.st.CreateBookingTx(context.Background(), &b, store.SlotGuard{}, "test.events", []byte(`{}`)); err != nil {
		f.t.Fatalf("seed booking: %v", err)
	}
	return b.ID
}

// ---------------------------------------------------------------------------
// Incident resolve (A1)
// ---------------------------------------------------------------------------

func (f *w48Fixture) seedIncident() uuid.UUID {
	f.t.Helper()
	inc := store.Incident{TenantID: f.tenant.ID, ReferenceNumber: "INC-1",
		IncidentType: "iot", Severity: "high", Payload: []byte(`{"source":"test"}`)}
	if _, err := f.st.InsertIncident(context.Background(), &inc); err != nil {
		f.t.Fatalf("seed incident: %v", err)
	}
	return inc.ID
}

func TestW48IncidentResolveRoute(t *testing.T) {
	f := newW48Fixture(t, "")
	id := f.seedIncident()

	// Happy path: 200 with the resolved row + actor stamped.
	rec := f.tenantDo(http.MethodPost, "/v1/incidents/"+id.String()+"/resolve", `{"note":"fixed on site"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "resolved" || body["resolved_by"] != "op-1" || body["resolved_at"] == nil {
		t.Fatalf("resolved body: %v", body)
	}
	// 409: double resolve.
	if rec := f.tenantDo(http.MethodPost, "/v1/incidents/"+id.String()+"/resolve", `{}`); rec.Code != http.StatusConflict {
		t.Fatalf("double resolve = %d, want 409", rec.Code)
	}
	// 404: unknown incident.
	if rec := f.tenantDo(http.MethodPost, "/v1/incidents/"+uuid.NewString()+"/resolve", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown resolve = %d, want 404", rec.Code)
	}
}

// Authz fail-closed (A1): with Permify erroring and the default policy, the
// resolve route answers 502 and never reaches the handler.
func TestW48IncidentResolveAuthzFailClosed(t *testing.T) {
	f := newW48Fixture(t, "")
	// Rebuild the router with a failing Permify (fail-closed default).
	f.router = f.buildRouter(Deps{
		Authz:             failingAuthz{err: errors.New("permify: connection refused")},
		AuthzOutagePolicy: AuthzFailClosed,
	}, nil, f.newResolver())
	id := f.seedIncident()
	rec := f.tenantDo(http.MethodPost, "/v1/incidents/"+id.String()+"/resolve", `{}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("resolve with permify down = %d (%s), want 502", rec.Code, rec.Body.String())
	}
	// The incident was NOT resolved (fail-closed never reached the store).
	got, err := f.st.GetIncident(context.Background(), f.tenant.ID, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "new" {
		t.Fatalf("status = %q, want new (fail-closed)", got.Status)
	}
}

// ---------------------------------------------------------------------------
// Waitlist notify/expire (A2)
// ---------------------------------------------------------------------------

func (f *w48Fixture) seedWaitlistEntry() store.WaitlistEntry {
	f.t.Helper()
	e := store.WaitlistEntry{
		TenantID: f.tenant.ID, OfferingID: f.offer.ID, ContactName: "Ada", ContactPhone: "+234800",
		WindowStart: time.Now().UTC().Add(time.Hour), WindowEnd: time.Now().UTC().Add(2 * time.Hour),
	}
	if err := f.st.CreateWaitlistEntry(context.Background(), &e); err != nil {
		f.t.Fatalf("seed entry: %v", err)
	}
	return e
}

func TestW48WaitlistNotifyExpireRoutes(t *testing.T) {
	f := newW48Fixture(t, "")
	e := f.seedWaitlistEntry()

	// Notify: 200, status notified, WaitlistEntryNotified outbox row.
	rec := f.tenantDo(http.MethodPost, "/v1/waitlist/"+e.ID.String()+"/notify", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("notify = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var entry store.WaitlistEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entry); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if entry.Status != store.WaitlistNotified {
		t.Fatalf("status = %q, want notified", entry.Status)
	}
	var n int
	if err := f.raw.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE aggregate_id=$1 AND topic='booking.events'`, e.ID).Scan(&n); err != nil {
		t.Fatalf("outbox: %v", err)
	}
	if n != 1 {
		t.Fatalf("notify outbox rows = %d, want 1", n)
	}
	var evt map[string]any
	if err := f.raw.QueryRow(context.Background(),
		`SELECT payload FROM outbox WHERE aggregate_id=$1`, e.ID).Scan(&evt); err != nil {
		t.Fatalf("outbox payload: %v", err)
	}
	if evt["type"] != bookingops.EventTypeWaitlistEntryNotified {
		t.Fatalf("event type = %v, want %s", evt["type"], bookingops.EventTypeWaitlistEntryNotified)
	}
	// 409: notify is only valid from waiting.
	if rec := f.tenantDo(http.MethodPost, "/v1/waitlist/"+e.ID.String()+"/notify", ""); rec.Code != http.StatusConflict {
		t.Fatalf("double notify = %d, want 409", rec.Code)
	}
	// Expire: 200, status expired.
	if rec := f.tenantDo(http.MethodPost, "/v1/waitlist/"+e.ID.String()+"/expire", ""); rec.Code != http.StatusOK {
		t.Fatalf("expire = %d, want 200", rec.Code)
	}
	// 409: already expired.
	if rec := f.tenantDo(http.MethodPost, "/v1/waitlist/"+e.ID.String()+"/expire", ""); rec.Code != http.StatusConflict {
		t.Fatalf("double expire = %d, want 409", rec.Code)
	}
	// 404: unknown entry.
	if rec := f.tenantDo(http.MethodPost, "/v1/waitlist/"+uuid.NewString()+"/expire", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown expire = %d, want 404", rec.Code)
	}
	// The staff routes require the tenant middleware.
	if rec := f.do(http.MethodPost, "/v1/waitlist/"+e.ID.String()+"/notify", "", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("notify without tenant header = %d, want 400 (tenant middleware)", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Public pay leg (A3)
// ---------------------------------------------------------------------------

// paymentsStub answers POST /v1/payments/flutterwave/initialize with the
// payments contract and records the last body.
func (f *w48Fixture) startPaymentsStub() {
	f.t.Helper()
	f.payments = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/payments/flutterwave/initialize" {
			http.Error(w, "no such route: "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("X-Internal-Token") != "tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		f.paymentsReq = map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&f.paymentsReq); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tx_ref": "flw-tx-1", "link": "https://pay.test/checkout", "deposit_id": "dep-1",
		})
	}))
	f.t.Cleanup(f.payments.Close)
}

func TestW48PublicPayLeg(t *testing.T) {
	f := newW48Fixture(t, "")
	f.startPaymentsStub()
	// Rebuild with the payments client wired at the stub.
	resolver := f.newResolver()
	f.router = f.buildRouter(Deps{AuthzDisabled: true},
		bookingops.NewPaymentsClient(f.payments.URL, "tok", zap.NewNop()), resolver)

	bookingID := f.seedBooking(store.StatusPending)
	path := "/public/sites/" + f.siteSlug + "/bookings/" + bookingID.String() + "/pay"

	// 200: the checkout link + tx_ref come back; the idempotency key is
	// agora-<booking_id>-<unix>.
	rec := f.do(http.MethodPost, path, `{"email":"ada@example.com"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pay = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["link"] != "https://pay.test/checkout" || body["tx_ref"] != "flw-tx-1" {
		t.Fatalf("pay body: %v", body)
	}
	if key, _ := f.paymentsReq["idempotency_key"].(string); !strings.HasPrefix(key, "agora-"+bookingID.String()+"-") {
		t.Fatalf("idempotency_key = %q, want agora-<booking_id>-<unix>", key)
	}
	if f.paymentsReq["booking_id"] != bookingID.String() || f.paymentsReq["tenant_id"] != f.tenant.ID.String() {
		t.Fatalf("payments body: %v", f.paymentsReq)
	}
	// Persisted: payment_ref + payment_status='initialized'.
	status, ref, err := f.st.GetBookingPayment(context.Background(), f.tenant.ID, bookingID)
	if err != nil || status != store.PaymentStatusInitialized || ref != "flw-tx-1" {
		t.Fatalf("payment state = %q/%q, %v", status, ref, err)
	}

	// 409: already paid.
	if _, err := f.raw.Exec(context.Background(),
		`UPDATE bookings SET payment_status='paid' WHERE id=$1`, bookingID); err != nil {
		t.Fatalf("force paid: %v", err)
	}
	if rec := f.do(http.MethodPost, path, `{"email":"ada@example.com"}`, nil); rec.Code != http.StatusConflict {
		t.Fatalf("paid pay = %d, want 409", rec.Code)
	}

	// Tenant isolation: a booking of ANOTHER tenant under this site slug is 404.
	otherBooking := uuid.New()
	if _, err := f.raw.Exec(context.Background(),
		`INSERT INTO bookings (id, tenant_id, offering_id, team_member_id, contact_id, starts_at, ends_at, status, source)
		 VALUES ($1,$2,$3,NULL,NULL,now(),now()+interval '30 min','pending','api')`,
		otherBooking, uuid.New(), f.offer.ID); err != nil {
		t.Fatalf("seed other-tenant booking: %v", err)
	}
	if rec := f.do(http.MethodPost,
		"/public/sites/"+f.siteSlug+"/bookings/"+otherBooking.String()+"/pay",
		`{"email":"ada@example.com"}`, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant pay = %d, want 404", rec.Code)
	}

	// 400: email required.
	booking2 := f.seedBooking(store.StatusPending)
	if rec := f.do(http.MethodPost,
		"/public/sites/"+f.siteSlug+"/bookings/"+booking2.String()+"/pay",
		`{}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("email-less pay = %d, want 400", rec.Code)
	}

	// 503 fail-closed when the payments rail is not configured.
	f.router = f.buildRouter(Deps{AuthzDisabled: true}, nil, resolver)
	if rec := f.do(http.MethodPost,
		"/public/sites/"+f.siteSlug+"/bookings/"+booking2.String()+"/pay",
		`{"email":"ada@example.com"}`, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured pay = %d, want 503 (fail closed)", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Public review leg (A4)
// ---------------------------------------------------------------------------

func TestW48PublicReviewLeg(t *testing.T) {
	f := newW48Fixture(t, "")
	completed := f.seedBooking(store.StatusCompleted)
	reviewPath := "/public/sites/" + f.siteSlug + "/bookings/" + completed.String() + "/review"

	// 400: rating out of bounds.
	if rec := f.do(http.MethodPost, reviewPath, `{"rating":6}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("rating 6 = %d, want 400", rec.Code)
	}
	if rec := f.do(http.MethodPost, reviewPath, `{"rating":0}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("rating 0 = %d, want 400", rec.Code)
	}
	// 409: only completed bookings can be reviewed.
	pending := f.seedBooking(store.StatusPending)
	if rec := f.do(http.MethodPost,
		"/public/sites/"+f.siteSlug+"/bookings/"+pending.String()+"/review",
		`{"rating":5}`, nil); rec.Code != http.StatusConflict {
		t.Fatalf("pending review = %d, want 409", rec.Code)
	}
	// 201: happy path.
	rec := f.do(http.MethodPost, reviewPath, `{"rating":5,"comment":"great","reviewer_name":"Ada"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("review = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	var review store.BookingReview
	if err := json.Unmarshal(rec.Body.Bytes(), &review); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if review.Rating != 5 || review.Comment != "great" || review.BookingID != completed {
		t.Fatalf("review: %+v", review)
	}
	// 409: one review per booking.
	if rec := f.do(http.MethodPost, reviewPath, `{"rating":1}`, nil); rec.Code != http.StatusConflict {
		t.Fatalf("dup review = %d, want 409", rec.Code)
	}
	// 404: a booking of another tenant.
	if rec := f.do(http.MethodPost,
		"/public/sites/"+f.siteSlug+"/bookings/"+uuid.NewString()+"/review",
		`{"rating":4}`, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown review = %d, want 404", rec.Code)
	}

	// Aggregate: add a second completed booking with rating 3.
	completed2 := f.seedBooking(store.StatusCompleted)
	if rec := f.do(http.MethodPost,
		"/public/sites/"+f.siteSlug+"/bookings/"+completed2.String()+"/review",
		`{"rating":3}`, nil); rec.Code != http.StatusCreated {
		t.Fatalf("review 2 = %d, want 201", rec.Code)
	}
	rec = f.do(http.MethodGet, "/public/sites/"+f.siteSlug+"/reviews?limit=50&offset=0", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reviews = %d, want 200", rec.Code)
	}
	var agg struct {
		Reviews []store.BookingReview `json:"reviews"`
		Average float64               `json:"average"`
		Count   int                    `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &agg); err != nil {
		t.Fatalf("decode agg: %v", err)
	}
	if agg.Count != 2 || len(agg.Reviews) != 2 || agg.Average != 4.0 {
		t.Fatalf("aggregate = count %d len %d avg %v, want 2/2/4.0", agg.Count, len(agg.Reviews), agg.Average)
	}
	// Limit above the cap still answers 200 (capped server-side).
	if rec := f.do(http.MethodGet, "/public/sites/"+f.siteSlug+"/reviews?limit=5000", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("capped reviews = %d, want 200", rec.Code)
	}
}
