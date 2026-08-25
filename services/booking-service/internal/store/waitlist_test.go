package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
)

// Store-level waitlist tests run against a real (embedded) Postgres so the
// claim race semantics (SELECT ... FOR UPDATE as the claim mutex) are
// exercised for real, not mocked. Set STORE_TEST=0 to skip in constrained
// environments.

// testSchema is the minimal slice of 01-booking-schema.sql the waitlist
// tests need (the init script itself contains \c meta-commands and cannot
// be replayed through pgx).
const testSchema = `
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

func newTestStore(t *testing.T) *Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres store test in -short mode")
	}
	// Dedicated port + data dir: httpapi's portal fixture and the incidents
	// service tests each run their own embedded Postgres in parallel
	// packages; DefaultConfig shares ONE port (5432) and ONE data dir, which
	// races (postmaster.pid lock) under `go test ./...`.
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_test").
		Port(5433).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	st, err := New(ctx, "postgres://postgres:postgres@localhost:5433/booking_test?sslmode=disable", 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.pool.Exec(ctx, testSchema); err != nil {
		t.Fatalf("test schema: %v", err)
	}
	// store.New ran before the tables existed, so the reverse-CRM columns
	// (contacts.source/external_id, bookings.crm_notes) are ensured again here.
	if err := st.ensureCRMColumns(ctx); err != nil {
		t.Fatalf("crm columns: %v", err)
	}
	return st
}

// claimFixture creates offering + member + a Mon–Sun 08:00–20:00 rule + one
// waiting entry whose window covers tomorrow.
func claimFixture(t *testing.T, st *Store) (tenantID, offeringID, memberID uuid.UUID, entry WaitlistEntry, offering Offering) {
	t.Helper()
	ctx := context.Background()
	tenantID = uuid.New()

	offering = Offering{TenantID: tenantID, Name: "Cut", DurationMin: 30, BufferMin: 0, Capacity: 1}
	if err := st.CreateOffering(ctx, &offering); err != nil {
		t.Fatalf("offering: %v", err)
	}
	offeringID = offering.ID

	member := TeamMember{TenantID: tenantID, Name: "Ana", Active: true}
	if err := st.CreateTeamMember(ctx, &member); err != nil {
		t.Fatalf("member: %v", err)
	}
	memberID = member.ID

	rules := make([]AvailabilityRule, 0, 7)
	for wd := 0; wd < 7; wd++ {
		rules = append(rules, AvailabilityRule{TenantID: tenantID, TeamMemberID: memberID, Weekday: wd, StartMin: 8 * 60, EndMin: 20 * 60})
	}
	if err := st.SetAvailability(ctx, tenantID, memberID, rules); err != nil {
		t.Fatalf("rules: %v", err)
	}

	entry = WaitlistEntry{
		TenantID:     tenantID,
		OfferingID:   offeringID,
		ContactName:  "Carl",
		ContactPhone: "+15550001",
		WindowStart:  time.Now().Add(-time.Hour),
		WindowEnd:    time.Now().Add(72 * time.Hour),
	}
	if err := st.CreateWaitlistEntry(ctx, &entry); err != nil {
		t.Fatalf("waitlist entry: %v", err)
	}
	return tenantID, offeringID, memberID, entry, offering
}

func claimBooking(tenantID, offeringID, memberID, contactID uuid.UUID, startsAt time.Time) *Booking {
	return &Booking{
		TenantID:       tenantID,
		OfferingID:     offeringID,
		TeamMemberID:   memberID,
		ContactID:      contactID,
		StartsAt:       startsAt,
		EndsAt:         startsAt.Add(30 * time.Minute),
		Status:         StatusPending,
		Source:         "web",
		IdempotencyKey: "waitlist-claim:test",
	}
}

func TestClaimWaitlistTxHappyPath(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID, offeringID, memberID, entry, offering := claimFixture(t, st)

	contact := Contact{TenantID: tenantID, Name: entry.ContactName, Phone: entry.ContactPhone}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}
	startsAt := nextNoonUTC()
	b := claimBooking(tenantID, offeringID, memberID, contact.ID, startsAt)

	claimed, err := st.ClaimWaitlistTx(ctx, tenantID, entry.ID, entry.ClaimToken, offering, b, time.UTC, "test.topic", []byte(`{"ok":true}`))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.Status != WaitlistClaimed {
		t.Fatalf("entry status = %q, want claimed", claimed.Status)
	}
	got, err := st.GetBooking(ctx, tenantID, b.ID)
	if err != nil {
		t.Fatalf("booking not persisted: %v", err)
	}
	if got.Status != StatusPending {
		t.Fatalf("booking status = %q", got.Status)
	}
	// Second claim of the same entry must conflict.
	b2 := claimBooking(tenantID, offeringID, memberID, contact.ID, startsAt)
	b2.IdempotencyKey = "waitlist-claim:test2"
	if _, err := st.ClaimWaitlistTx(ctx, tenantID, entry.ID, entry.ClaimToken, offering, b2, time.UTC, "test.topic", []byte(`{}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("re-claim err = %v, want ErrConflict", err)
	}
}

func TestClaimWaitlistTxRejectsBadTokenAndTakenSlot(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID, offeringID, memberID, entry, offering := claimFixture(t, st)

	contact := Contact{TenantID: tenantID, Name: "X", Phone: "+1"}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}
	startsAt := nextNoonUTC()

	// Bad token.
	b := claimBooking(tenantID, offeringID, memberID, contact.ID, startsAt)
	if _, err := st.ClaimWaitlistTx(ctx, tenantID, entry.ID, uuid.New(), offering, b, time.UTC, "t", []byte(`{}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("bad token err = %v, want ErrConflict", err)
	}

	// Occupy the slot with a regular booking; the claim must now conflict
	// on the availability re-check.
	existing := claimBooking(tenantID, offeringID, memberID, contact.ID, startsAt)
	existing.IdempotencyKey = "regular"
	if err := st.CreateBookingTx(ctx, existing, SlotGuard{}, "t", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	b2 := claimBooking(tenantID, offeringID, memberID, contact.ID, startsAt)
	if _, err := st.ClaimWaitlistTx(ctx, tenantID, entry.ID, entry.ClaimToken, offering, b2, time.UTC, "t", []byte(`{}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("taken slot err = %v, want ErrConflict", err)
	}
	// Failed claim must not have marked the entry claimed nor inserted a booking.
	got, err := st.GetWaitlistEntry(ctx, tenantID, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != WaitlistWaiting {
		t.Fatalf("entry status = %q after failed claim, want waiting", got.Status)
	}
}

// TestClaimWaitlistTxRace is the core race-semantics check: N concurrent
// claims of ONE entry produce exactly one winner; all losers get
// ErrConflict and exactly one booking row exists afterwards.
func TestClaimWaitlistTxRace(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID, offeringID, memberID, entry, offering := claimFixture(t, st)

	contact := Contact{TenantID: tenantID, Name: "R", Phone: "+1"}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}
	startsAt := nextNoonUTC()

	const racers = 8
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b := claimBooking(tenantID, offeringID, memberID, contact.ID, startsAt)
			b.IdempotencyKey = "" // no idempotency shield: the FOR UPDATE lock must decide
			_, errs[i] = st.ClaimWaitlistTx(ctx, tenantID, entry.ID, entry.ClaimToken, offering, b, time.UTC, "t", []byte(`{}`))
		}(i)
	}
	wg.Wait()

	wins := 0
	for i, err := range errs {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatalf("racer %d unexpected err: %v", i, err)
		}
	}
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins)
	}
	bookings, err := st.ListBookings(ctx, tenantID, BookingFilter{TeamMemberID: &memberID})
	if err != nil {
		t.Fatal(err)
	}
	if len(bookings) != 1 {
		t.Fatalf("bookings = %d, want exactly 1", len(bookings))
	}
	got, err := st.GetWaitlistEntry(ctx, tenantID, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != WaitlistClaimed {
		t.Fatalf("entry status = %q, want claimed", got.Status)
	}
}

// nextNoonUTC returns 12:00 UTC tomorrow — always inside the fixture's
// 08:00–20:00 availability window regardless of the host timezone.
func nextNoonUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}
