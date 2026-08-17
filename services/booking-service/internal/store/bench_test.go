package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
)

// Store benchmarks run against a REAL embedded Postgres (same idiom as
// waitlist_test.go) so the measured ns/op reflect the production query path:
// withTenant transaction + SET LOCAL app.tenant_id + the actual INSERT/SELECT
// plans. They back the budgets in docs/performance-budgets.md (SPEC-W41 W41-5).
// Skipped under -short like every other embedded-postgres test in this package.

// benchPort is dedicated to this file: the collision set used by other
// packages/tests is 5432/5433/5434/5544/5546/5547/5548/5561-5566.
const benchPort = 5570

func newBenchStore(b *testing.B) *Store {
	b.Helper()
	if testing.Short() {
		b.Skip("skipping embedded-postgres store benchmark in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_bench").
		Port(benchPort).
		DataPath(b.TempDir()).
		RuntimePath(b.TempDir()))
	if err := ep.Start(); err != nil {
		b.Skipf("embedded postgres unavailable: %v", err)
	}
	b.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/booking_bench?sslmode=disable", benchPort)
	st, err := New(ctx, dsn, 0)
	if err != nil {
		b.Fatalf("store.New: %v", err)
	}
	b.Cleanup(st.Close)
	// Minimal slice of 01-booking-schema.sql (shared with waitlist_test.go);
	// the outbox table is infra-managed (init script lines 84-91), so the
	// fixture creates it manually like internal/incidents/service_test.go.
	if _, err := st.pool.Exec(ctx, testSchema); err != nil {
		b.Fatalf("bench schema: %v", err)
	}
	// store.New ran before the tables existed, so the reverse-CRM columns
	// (contacts.source/external_id, bookings.crm_notes) are ensured again here.
	if err := st.ensureCRMColumns(ctx); err != nil {
		b.Fatalf("crm columns: %v", err)
	}
	return st
}

// benchFixture seeds one tenant with an offering, a team member and a
// contact — the minimal referential set every booking row points at.
func benchFixture(b *testing.B, st *Store) (tenantID, offeringID, memberID, contactID uuid.UUID) {
	b.Helper()
	ctx := context.Background()
	tenantID = uuid.New()

	offering := Offering{TenantID: tenantID, Name: "Bench Cut", DurationMin: 30, Capacity: 1}
	if err := st.CreateOffering(ctx, &offering); err != nil {
		b.Fatalf("offering: %v", err)
	}
	offeringID = offering.ID

	member := TeamMember{TenantID: tenantID, Name: "Bench Ana", Active: true}
	if err := st.CreateTeamMember(ctx, &member); err != nil {
		b.Fatalf("member: %v", err)
	}
	memberID = member.ID

	contact := Contact{TenantID: tenantID, Name: "Bench Carl", Phone: "+15559900"}
	if err := st.CreateContact(ctx, &contact); err != nil {
		b.Fatalf("contact: %v", err)
	}
	contactID = contact.ID
	return tenantID, offeringID, memberID, contactID
}

func benchBooking(tenantID, offeringID, memberID, contactID uuid.UUID, i int) *Booking {
	startsAt := time.Now().UTC().Add(time.Duration(i%2000) * time.Hour)
	return &Booking{
		TenantID:       tenantID,
		OfferingID:     offeringID,
		TeamMemberID:   memberID,
		ContactID:      contactID,
		StartsAt:       startsAt,
		EndsAt:         startsAt.Add(30 * time.Minute),
		Status:         StatusPending,
		Source:         "api",
		IdempotencyKey: fmt.Sprintf("bench-%d", i),
	}
}

// BenchmarkCreateBookingTx measures the hot booking-create path: one
// transaction = SET LOCAL app.tenant_id + INSERT bookings + INSERT outbox
// (transactional outbox, SPEC §6/§9).
func BenchmarkCreateBookingTx(b *testing.B) {
	st := newBenchStore(b)
	ctx := context.Background()
	tenantID, offeringID, memberID, contactID := benchFixture(b, st)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bk := benchBooking(tenantID, offeringID, memberID, contactID, i)
		if err := st.CreateBookingTx(ctx, bk, "bench.booking.created", []byte(`{"bench":true}`)); err != nil {
			b.Fatalf("create booking %d: %v", i, err)
		}
	}
}

// BenchmarkListBookings measures the tenant-scoped list path (RLS tx +
// filtered SELECT ... ORDER BY starts_at DESC LIMIT) over a realistic result
// set of pre-seeded bookings.
func BenchmarkListBookings(b *testing.B) {
	st := newBenchStore(b)
	ctx := context.Background()
	tenantID, offeringID, memberID, contactID := benchFixture(b, st)

	const seeded = 200
	for i := 0; i < seeded; i++ {
		bk := benchBooking(tenantID, offeringID, memberID, contactID, i)
		if err := st.CreateBookingTx(ctx, bk, "bench.booking.created", []byte(`{"bench":true}`)); err != nil {
			b.Fatalf("seed booking %d: %v", i, err)
		}
	}
	filter := BookingFilter{TeamMemberID: &memberID, Limit: 50}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bookings, err := st.ListBookings(ctx, tenantID, filter)
		if err != nil {
			b.Fatalf("list bookings: %v", err)
		}
		if len(bookings) == 0 {
			b.Fatal("list bookings returned 0 rows against seeded fixture")
		}
	}
}
