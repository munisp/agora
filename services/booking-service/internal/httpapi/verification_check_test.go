package httpapi

// SPEC-W87 tests: the verification-grant check endpoint and grant writing
// on portal OTP success, against a real (embedded) Postgres. Mirrors the
// portal fixture (newPortalFixture) with InternalToken +
// VerificationGrantTTL added to Deps.

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

type w87Fixture struct {
	handler   http.Handler
	store     *store.Store
	publisher *fakePublisher
	tenantID  uuid.UUID
}

func newW87Fixture(t *testing.T, internalToken string, grantTTL time.Duration) *w87Fixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres verification-check test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_w87_test"))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5432/booking_w87_test?sslmode=disable"
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
		Store:                st,
		Ops:                  ops,
		AuthzDisabled:        true,
		Logger:               zap.NewNop(),
		PortalSecret:         "test-portal-secret",
		PubSubName:           "test-pubsub",
		NotificationsTopic:   "test.notifications",
		Publisher:            pub,
		InternalToken:        internalToken,
		VerificationGrantTTL: grantTTL,
		TenantBySlug: func(_ context.Context, slug string) (bookingops.TenantInfo, error) {
			return bookingops.TenantInfo{Slug: slug, Timezone: "UTC"}, nil
		},
	})

	f := &w87Fixture{handler: handler, store: st, publisher: pub, tenantID: uuid.New()}
	site := store.Site{TenantID: f.tenantID, TenantSlug: "acme", Slug: "acme-books", DisplayName: "Acme"}
	if err := st.CreateSite(ctx, &site); err != nil {
		t.Fatalf("site: %v", err)
	}
	return f
}

// w87Check calls the check endpoint with an explicit X-Internal-Token
// (empty token = header absent).
func w87Check(t *testing.T, h http.Handler, token string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/internal/sites/acme-books/verification/check", &buf)
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

// w87Verify runs the request→verify OTP flow for a phone (no booking
// needed) and returns the verify response.
func (f *w87Fixture) w87Verify(t *testing.T, phone string) {
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

func TestW87VerifyWritesGrant(t *testing.T) {
	f := newW87Fixture(t, "w87-secret", 24*time.Hour)
	f.w87Verify(t, "+15550101")

	code, resp := w87Check(t, f.handler, "w87-secret", map[string]string{"phone": "+15550101"})
	if code != http.StatusOK {
		t.Fatalf("check = %d %v", code, resp)
	}
	if resp["verified"] != true {
		t.Fatalf("verified = %v (%v)", resp["verified"], resp)
	}
	if resp["method"] != "otp" {
		t.Fatalf("method = %v", resp["method"])
	}
	exp, _ := resp["expires_at"].(string)
	if exp == "" {
		t.Fatalf("no expires_at in %v", resp)
	}
	if _, err := time.Parse(time.RFC3339, exp); err != nil {
		t.Fatalf("expires_at not RFC3339: %q (%v)", exp, err)
	}
}

func TestW87CheckUnknownAndGrantlessUniform(t *testing.T) {
	f := newW87Fixture(t, "w87-secret", 24*time.Hour)
	// Known contact WITHOUT a grant.
	ctx := context.Background()
	c := store.Contact{TenantID: f.tenantID, Name: "Ada", Phone: "+15550202"}
	if err := f.store.CreateContact(ctx, &c); err != nil {
		t.Fatal(err)
	}

	code1, resp1 := w87Check(t, f.handler, "w87-secret", map[string]string{"phone": "+15550202"})
	code2, resp2 := w87Check(t, f.handler, "w87-secret", map[string]string{"phone": "+15559999"})
	if code1 != http.StatusOK || code2 != http.StatusOK {
		t.Fatalf("codes = %d/%d", code1, code2)
	}
	if resp1["verified"] != false || resp2["verified"] != false {
		t.Fatalf("verified = %v/%v (want uniform false)", resp1["verified"], resp2["verified"])
	}
	// Uniformity: no field distinguishes "unknown contact" from "no grant".
	if len(resp1) != len(resp2) {
		t.Fatalf("response shapes differ: %v vs %v", resp1, resp2)
	}
}

func TestW87CheckAuth(t *testing.T) {
	f := newW87Fixture(t, "w87-secret", 24*time.Hour)
	if code, _ := w87Check(t, f.handler, "", map[string]string{"phone": "+15550101"}); code != http.StatusUnauthorized {
		t.Fatalf("missing token = %d, want 401", code)
	}
	if code, _ := w87Check(t, f.handler, "wrong", map[string]string{"phone": "+15550101"}); code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d, want 401", code)
	}
	// Unset server token fails closed with 503. internAuth runs BEFORE any
	// store access, so a second router on the same store suffices (no
	// second embedded Postgres — the default port is a singleton).
	h2 := NewRouter(Deps{
		Store:                f.store,
		AuthzDisabled:        true,
		Logger:               zap.NewNop(),
		PortalSecret:         "test-portal-secret",
		Publisher:            &fakePublisher{},
		VerificationGrantTTL: 24 * time.Hour,
	})
	if code, _ := w87Check(t, h2, "anything", map[string]string{"phone": "+15550101"}); code != http.StatusServiceUnavailable {
		t.Fatalf("unset server token = %d, want 503", code)
	}
}

func TestW87TTLZeroDisables(t *testing.T) {
	f := newW87Fixture(t, "w87-secret", 0)
	f.w87Verify(t, "+15550101")
	code, resp := w87Check(t, f.handler, "w87-secret", map[string]string{"phone": "+15550101"})
	if code != http.StatusOK || resp["verified"] != false {
		t.Fatalf("TTL=0: check = %d %v, want verified:false", code, resp)
	}
}
