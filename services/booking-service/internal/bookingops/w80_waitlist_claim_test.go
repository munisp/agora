package bookingops

// SPEC-W80 B1 (redirection test — FAILS on the pristine base with the
// uq_contacts_tenant_phone unique violation, PASSES on the candidate):
// claiming a waitlist entry whose phone ALREADY belongs to a tenant
// contact must succeed and reuse that contact, not 500 on a blind
// CreateContact insert. The staff route and the public token path share
// Service.ClaimWaitlist, so one test on the shared path covers both.

import (
	"context"
	"fmt"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// w80ClaimSchema is the sweeper fixture schema plus the team_members +
// availability_rules tables the claim path reads (the portal_test.go
// fixture shape).
const w80ClaimSchema = `
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
CREATE UNIQUE INDEX IF NOT EXISTS uq_bookings_tenant_idempotency_key ON bookings (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    sent_at TIMESTAMPTZ
);`

// newW80ClaimStore boots embedded Postgres (sweeperTestPort, same
// sequential-package idiom as sweeper_test.go) with the claim schema, then
// store.New so every bootstrap migration (waitlist table, contacts dedupe
// unique index, bookings public_ref/crm_notes/payment columns) applies.
func newW80ClaimStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres claim test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_sweeper_test").
		Port(sweeperTestPort).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/booking_sweeper_test?sslmode=disable", sweeperTestPort)
	if pool, err := pgxpool.New(ctx, dsn); err != nil {
		t.Fatalf("raw pool: %v", err)
	} else {
		if _, err := pool.Exec(ctx, w80ClaimSchema); err != nil {
			t.Fatalf("claim schema: %v", err)
		}
		pool.Close()
	}
	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)
	return st, ctx
}

// w80ClaimFixture seeds offering + member + a Mon–Sun 08:00–20:00 rule +
// one waiting entry whose window covers tomorrow (store/waitlist_test.go
// claimFixture idiom).
func w80ClaimFixture(t *testing.T, st *store.Store, ctx context.Context, tenantID uuid.UUID, phone string) (memberID uuid.UUID, entry store.WaitlistEntry) {
	t.Helper()
	offering := store.Offering{TenantID: tenantID, Name: "Cut", DurationMin: 30, BufferMin: 0, Capacity: 1}
	if err := st.CreateOffering(ctx, &offering); err != nil {
		t.Fatalf("offering: %v", err)
	}
	member := store.TeamMember{TenantID: tenantID, Name: "Ana", Active: true}
	if err := st.CreateTeamMember(ctx, &member); err != nil {
		t.Fatalf("member: %v", err)
	}
	rules := make([]store.AvailabilityRule, 0, 7)
	for wd := 0; wd < 7; wd++ {
		rules = append(rules, store.AvailabilityRule{TenantID: tenantID, TeamMemberID: member.ID, Weekday: wd, StartMin: 8 * 60, EndMin: 20 * 60})
	}
	if err := st.SetAvailability(ctx, tenantID, member.ID, rules); err != nil {
		t.Fatalf("rules: %v", err)
	}
	entry = store.WaitlistEntry{
		TenantID:     tenantID,
		OfferingID:   offering.ID,
		ContactName:  "Carl",
		ContactPhone: phone,
		WindowStart:  time.Now().Add(-time.Hour),
		WindowEnd:    time.Now().Add(72 * time.Hour),
	}
	if err := st.CreateWaitlistEntry(ctx, &entry); err != nil {
		t.Fatalf("waitlist entry: %v", err)
	}
	return member.ID, entry
}

// w80NextNoonUTC returns the next 12:00 UTC strictly inside the fixture's
// 08:00–20:00 rule and window.
func w80NextNoonUTC() time.Time {
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	if !start.After(now) {
		start = start.Add(24 * time.Hour)
	}
	return start
}

// B1: the claimant is ALREADY a contact (they booked before) — the claim
// must succeed and attach the booking to the EXISTING contact row.
func TestClaimWaitlistWithPreExistingContact(t *testing.T) {
	st, ctx := newW80ClaimStore(t)
	tenantID := uuid.New()
	phone := "+15550001"
	memberID, entry := w80ClaimFixture(t, st, ctx, tenantID, phone)

	// The pre-existing contact with the SAME (tenant, phone) — the common
	// case that used to 500 on uq_contacts_tenant_phone.
	existing := store.Contact{TenantID: tenantID, Name: "Carl Existing", Phone: phone}
	if err := st.CreateContact(ctx, &existing); err != nil {
		t.Fatalf("seed contact: %v", err)
	}

	svc := &Service{Store: st, EventsTopic: "test.events", Logger: zap.NewNop()}
	booking, claimed, err := svc.ClaimWaitlist(ctx, ClaimInput{
		TenantID:     tenantID,
		TenantSlug:   "acme",
		Timezone:     "UTC",
		EntryID:      entry.ID,
		Token:        entry.ClaimToken,
		TeamMemberID: memberID,
		StartsAt:     w80NextNoonUTC(),
	})
	if err != nil {
		t.Fatalf("claim with pre-existing contact: %v", err)
	}
	if claimed.Status != store.WaitlistClaimed {
		t.Fatalf("entry status = %q, want claimed", claimed.Status)
	}
	if booking.ContactID != existing.ID {
		t.Fatalf("booking contact = %s, want the pre-existing contact %s (no duplicate row)",
			booking.ContactID, existing.ID)
	}
	// The phone still resolves to the SAME row — the claim must not have
	// created a duplicate contact.
	got, err := st.FindContactByPhoneOrEmail(ctx, tenantID, phone, "")
	if err != nil {
		t.Fatalf("resolve contact by phone: %v", err)
	}
	if got.ID != existing.ID {
		t.Fatalf("phone resolves to %s, want the pre-existing contact %s", got.ID, existing.ID)
	}
}
