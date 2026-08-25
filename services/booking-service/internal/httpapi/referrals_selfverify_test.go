package httpapi

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
	"github.com/opendesk/booking-service/internal/leads"
	"github.com/opendesk/booking-service/internal/referrals"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// SPEC-W44 W-B/S1-F7-06 self-verify guard: the referrer must not verify
// their own referral (409 + audit Warn); a DIFFERENT verifier passes. The
// verifier is the caller identity — JWT sub resolved by the tenant
// middleware, X-User-Id header fallback (callerIdentity in referrals.go).
// Embedded-postgres harness (same pattern as the referrals service tests;
// dedicated port 5570; -short skips).
func TestReferralSelfVerifyGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-postgres self-verify test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_selfverify_test").
		Port(5570).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5570/booking_selfverify_test?sslmode=disable"
	// The outbox table is infra-managed; create the minimal shape the
	// funnel/verify path writes (same posture as the referrals service
	// tests).
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    sent_at TIMESTAMPTZ
)`); err != nil {
		t.Fatalf("outbox ddl: %v", err)
	}
	conn.Close(ctx) //nolint:errcheck
	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)

	svc := &referrals.Service{
		Store:          st,
		Ledger:         referrals.NewPostgresLedger(st),
		Leads:          &leads.Service{Store: st, CACEventsTopic: "cac.events", Log: zap.NewNop()},
		CACEventsTopic: "cac.events",
		Log:            zap.NewNop(),
	}

	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme-ng", Name: "Acme NG"}
	ref, created, err := svc.Create(ctx, referrals.CreateInput{
		TenantID: tenant.ID, ReferrerType: referrals.ReferrerAgent,
		ReferrerID: "agent-1", RefereePhone: "+2348099990001",
	})
	if err != nil || !created {
		t.Fatalf("create referral: created=%v err=%v", created, err)
	}

	// Tenant middleware resolves via Deps.Resolver → stub identity-service.
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tenants/"+tenant.Slug {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tenant)
	}))
	defer identity.Close()
	resolver := bookingops.NewTenantResolver(daprc.New("localhost", 1), "identity", time.Minute, zap.NewNop(),
		bookingops.WithIdentityBaseURL(identity.URL))

	r := NewRouter(Deps{
		Logger:        zap.NewNop(),
		Referrals:     svc,
		Resolver:      resolver,
		AuthzDisabled: true,
	})

	verify := func(xUserID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/referrals/"+ref.ID.String()+"/verify",
			strings.NewReader(`{"trigger":"signup_verified"}`))
		req.Header.Set("X-Tenant-Slug", tenant.Slug)
		if xUserID != "" {
			req.Header.Set("X-User-Id", xUserID)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	// Referrer == verifier (X-User-Id path) → 409, and the referral must
	// stay pending (no verify side-effects).
	if rec := verify("agent-1"); rec.Code != http.StatusConflict {
		t.Fatalf("self-verify = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	got, err := svc.Store.GetReferral(ctx, tenant.ID, ref.ID)
	if err != nil {
		t.Fatalf("reload referral: %v", err)
	}
	if got.Status != referrals.StatusPending {
		t.Fatalf("self-verify must not transition the referral: status = %s", got.Status)
	}

	// A different verifier passes → 200 verified.
	if rec := verify("agent-2"); rec.Code != http.StatusOK {
		t.Fatalf("different verifier = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	got, err = svc.Store.GetReferral(ctx, tenant.ID, ref.ID)
	if err != nil {
		t.Fatalf("reload referral: %v", err)
	}
	if got.Status != referrals.StatusVerified {
		t.Fatalf("verified referral: status = %s", got.Status)
	}

	// No caller identity at all → guard skipped, verify proceeds (idempotent
	// replay on the now-verified referral).
	if rec := verify(""); rec.Code != http.StatusOK {
		t.Fatalf("identity-less verify replay = %d (%s), want 200", rec.Code, rec.Body.String())
	}
}
