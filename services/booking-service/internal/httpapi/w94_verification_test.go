package httpapi

// SPEC-W94 tests: grant revocation endpoint + per-pack grant policy
// (read gate, write gate, TTL override) against embedded Postgres.
// Mirrors the W87 fixture; TenantBySlug returns a MUTABLE TenantInfo so a
// test can flip the pack policy mid-scenario (policy change landing).

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
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

type w94Fixture struct {
	handler    http.Handler
	store      *store.Store
	publisher  *fakePublisher
	tenantID   uuid.UUID
	tenantInfo bookingops.TenantInfo // returned by TenantBySlug (mutable)
}

func newW94Fixture(t *testing.T, internalToken string, grantTTL time.Duration) *w94Fixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres w94 test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_w94_test"))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5432/booking_w94_test?sslmode=disable"
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

	f := &w94Fixture{store: st, publisher: &fakePublisher{}, tenantID: uuid.New()}
	f.tenantInfo = bookingops.TenantInfo{Slug: "acme", Timezone: "UTC"}
	ops := &bookingops.Service{Store: st, EventsTopic: "test.events", Logger: zap.NewNop()}
	f.handler = NewRouter(Deps{
		Store:                st,
		Ops:                  ops,
		AuthzDisabled:        true,
		Logger:               zap.NewNop(),
		PortalSecret:         "test-portal-secret",
		PubSubName:           "test-pubsub",
		NotificationsTopic:   "test.notifications",
		Publisher:            f.publisher,
		InternalToken:        internalToken,
		VerificationGrantTTL: grantTTL,
		TenantBySlug: func(_ context.Context, slug string) (bookingops.TenantInfo, error) {
			return f.tenantInfo, nil
		},
	})

	site := store.Site{TenantID: f.tenantID, TenantSlug: "acme", Slug: "acme-books", DisplayName: "Acme"}
	if err := st.CreateSite(ctx, &site); err != nil {
		t.Fatalf("site: %v", err)
	}
	return f
}

// w94Verify runs the portal OTP request+verify flow for a phone.
func (f *w94Fixture) w94Verify(t *testing.T, phone string) {
	t.Helper()
	ctx := context.Background()
	c := store.Contact{TenantID: f.tenantID, Name: "Ada", Phone: phone}
	if err := f.store.CreateContact(ctx, &c); err != nil {
		t.Fatal(err)
	}
	code, resp := doJSON(t, f.handler, "POST", "/public/sites/acme-books/portal/request", "", map[string]string{"phone": phone})
	if code != http.StatusAccepted {
		t.Fatalf("request code = %d %v", code, resp)
	}
	if len(f.publisher.events) == 0 {
		t.Fatal("no SendPortalCode event published")
	}
	evt := f.publisher.events[len(f.publisher.events)-1]
	plainCode, _ := evt.Data["code"].(string)
	if len(plainCode) != 6 {
		t.Fatalf("code = %q, want 6 digits", plainCode)
	}
	code, resp = doJSON(t, f.handler, "POST", "/public/sites/acme-books/portal/verify", "",
		map[string]string{"phone": phone, "code": plainCode})
	if code != http.StatusOK {
		t.Fatalf("verify = %d %v", code, resp)
	}
}

func w94Call(t *testing.T, h http.Handler, token, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func (f *w94Fixture) grantFor(t *testing.T, phone string) (store.VerificationGrant, error) {
	t.Helper()
	contact, err := f.store.GetContactByChannel(context.Background(), f.tenantID, "phone", phone)
	if err != nil {
		t.Fatalf("contact: %v", err)
	}
	return f.store.GetActiveVerificationGrant(context.Background(), f.tenantID, contact.ID)
}

func TestW94RevokeEndpoint(t *testing.T) {
	f := newW94Fixture(t, "w94-secret", 24*time.Hour)
	f.w94Verify(t, "+15550101")

	const revoke = "/internal/sites/acme-books/verification/revoke"
	const check = "/internal/sites/acme-books/verification/check"

	code, resp := w94Call(t, f.handler, "w94-secret", revoke, map[string]string{"phone": "+15550101"})
	if code != http.StatusOK || resp["revoked"] != true {
		t.Fatalf("first revoke = %d %v", code, resp)
	}

	// The check endpoint immediately stops honoring.
	code, resp = w94Call(t, f.handler, "w94-secret", check, map[string]string{"phone": "+15550101"})
	if code != http.StatusOK || resp["verified"] != false {
		t.Fatalf("check after revoke = %d %v", code, resp)
	}

	// Idempotent: second revoke reports false.
	code, resp = w94Call(t, f.handler, "w94-secret", revoke, map[string]string{"phone": "+15550101"})
	if code != http.StatusOK || resp["revoked"] != false {
		t.Fatalf("second revoke = %d %v", code, resp)
	}

	// Unknown contact: uniform false (anti-enumeration).
	code, resp = w94Call(t, f.handler, "w94-secret", revoke, map[string]string{"phone": "+15559999"})
	if code != http.StatusOK || resp["revoked"] != false {
		t.Fatalf("unknown revoke = %d %v", code, resp)
	}

	// Bad channel request: 400 from portalChannelFor.
	code, _ = w94Call(t, f.handler, "w94-secret", revoke, map[string]string{})
	if code != http.StatusBadRequest {
		t.Fatalf("empty revoke = %d, want 400", code)
	}

	// internAuth gate: no token -> 401.
	code, _ = w94Call(t, f.handler, "", revoke, map[string]string{"phone": "+15550101"})
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated revoke = %d, want 401", code)
	}
}

func TestW94PolicyDisablesHonoringAtRead(t *testing.T) {
	f := newW94Fixture(t, "w94-secret", 24*time.Hour)
	f.w94Verify(t, "+15550101")
	const check = "/internal/sites/acme-books/verification/check"

	code, resp := w94Call(t, f.handler, "w94-secret", check, map[string]string{"phone": "+15550101"})
	if code != http.StatusOK || resp["verified"] != true {
		t.Fatalf("baseline check = %d %v", code, resp)
	}

	// Policy lands: the pack disables grants -> honoring stops even though
	// the grant row is still inside its TTL.
	f.tenantInfo.Pack = &bookingops.PackSummary{
		ID:                 "support-desk",
		VerificationGrants: &bookingops.GrantPolicySummary{Enabled: false},
	}
	code, resp = w94Call(t, f.handler, "w94-secret", check, map[string]string{"phone": "+15550101"})
	if code != http.StatusOK || resp["verified"] != false {
		t.Fatalf("disabled-policy check = %d %v", code, resp)
	}
}

func TestW94PolicyAbsentKeepsBaseline(t *testing.T) {
	f := newW94Fixture(t, "w94-secret", 24*time.Hour)
	f.w94Verify(t, "+15550101")
	// Pack present but no grant policy block -> env baseline honored.
	f.tenantInfo.Pack = &bookingops.PackSummary{ID: "logistics"}
	code, resp := w94Call(t, f.handler, "w94-secret", "/internal/sites/acme-books/verification/check", map[string]string{"phone": "+15550101"})
	if code != http.StatusOK || resp["verified"] != true {
		t.Fatalf("absent-policy check = %d %v", code, resp)
	}
}

func TestW94PolicyDisablesGrantWrite(t *testing.T) {
	f := newW94Fixture(t, "w94-secret", 24*time.Hour)
	f.tenantInfo.Pack = &bookingops.PackSummary{
		ID:                 "support-desk",
		VerificationGrants: &bookingops.GrantPolicySummary{Enabled: false},
	}
	f.w94Verify(t, "+15550101") // OTP itself must still succeed
	if _, err := f.grantFor(t, "+15550101"); err != store.ErrNotFound {
		t.Fatalf("disabled pack must not mint a grant, got %v", err)
	}
}

func TestW94PolicyTTLOverridesWrite(t *testing.T) {
	f := newW94Fixture(t, "w94-secret", 24*time.Hour)
	f.tenantInfo.Pack = &bookingops.PackSummary{
		ID:                 "logistics",
		VerificationGrants: &bookingops.GrantPolicySummary{Enabled: true, TTLHours: 8},
	}
	f.w94Verify(t, "+15550101")
	g, err := f.grantFor(t, "+15550101")
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// ~8h out, not ~24h: assert inside [7h59m, 8h1m].
	lo, hi := time.Now().Add(7*time.Hour+59*time.Minute), time.Now().Add(8*time.Hour+time.Minute)
	if g.ExpiresAt.Before(lo) || g.ExpiresAt.After(hi) {
		t.Fatalf("expires_at %v not ~8h out (pack override)", g.ExpiresAt)
	}
}

func TestW94EffectiveGrantPolicy(t *testing.T) {
	pack := func(pol *bookingops.GrantPolicySummary) *bookingops.PackSummary {
		return &bookingops.PackSummary{ID: "p", VerificationGrants: pol}
	}
	cases := []struct {
		name        string
		pack        *bookingops.PackSummary
		envTTL      time.Duration
		wantEnabled bool
		wantTTL     time.Duration
	}{
		{"env only", nil, 24 * time.Hour, true, 24 * time.Hour},
		{"env disabled", nil, 0, false, 0},
		{"nil policy keeps env", pack(nil), 24 * time.Hour, true, 24 * time.Hour},
		{"pack kills", pack(&bookingops.GrantPolicySummary{Enabled: false}), 24 * time.Hour, false, 24 * time.Hour},
		{"pack ttl override", pack(&bookingops.GrantPolicySummary{Enabled: true, TTLHours: 8}), 24 * time.Hour, true, 8 * time.Hour},
		{"pack zero ttl keeps env", pack(&bookingops.GrantPolicySummary{Enabled: true, TTLHours: 0}), 24 * time.Hour, true, 24 * time.Hour},
		{"pack cannot revive env-killed", pack(&bookingops.GrantPolicySummary{Enabled: true, TTLHours: 8}), 0, false, 8 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enabled, ttl := effectiveGrantPolicy(tc.pack, tc.envTTL)
			if enabled != tc.wantEnabled || ttl != tc.wantTTL {
				t.Fatalf("effectiveGrantPolicy = (%v, %v), want (%v, %v)", enabled, ttl, tc.wantEnabled, tc.wantTTL)
			}
		})
	}
}
