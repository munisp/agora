package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// SPEC-W43 K-01 regression tests: the double-booking TOCTOU window is
// closed by the in-transaction overlap re-check under a per-team-member
// advisory lock. These run against a real (embedded) Postgres via
// newTestStore (waitlist_test.go idiom) — the race semantics cannot be
// exercised against a mock.

// Two concurrent creates for the same member/slot: exactly one commits.
func TestCreateBookingTxConcurrentSameSlot(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID, offeringID, memberID, _, offering := claimFixture(t, st)

	contact := Contact{TenantID: tenantID, Name: "Race", Phone: "+1"}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}
	startsAt := nextNoonUTC()
	guard := SlotGuard{Buffer: time.Duration(offering.BufferMin) * time.Minute, Capacity: offering.Capacity}
	mk := func() *Booking {
		return &Booking{
			TenantID: tenantID, OfferingID: offeringID, TeamMemberID: memberID, ContactID: contact.ID,
			StartsAt: startsAt, EndsAt: startsAt.Add(30 * time.Minute),
			Status: StatusPending, Source: "api",
		}
	}

	const racers = 2
	errs := make([]error, racers)
	var wg sync.WaitGroup
	barrier := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-barrier
			errs[i] = st.CreateBookingTx(ctx, mk(), guard, "t", []byte(`{}`))
		}(i)
	}
	close(barrier)
	wg.Wait()

	var ok, conflict int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrSlotConflict):
			conflict++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || conflict != racers-1 {
		t.Fatalf("ok=%d conflict=%d, want exactly one winner", ok, conflict)
	}

	bookings, err := st.ListBookings(ctx, tenantID, BookingFilter{TeamMemberID: &memberID})
	if err != nil {
		t.Fatal(err)
	}
	if len(bookings) != 1 {
		t.Fatalf("bookings = %d, want exactly 1 (no double-booking)", len(bookings))
	}
}

// Sequential variant: the in-tx re-check catches an already-committed
// overlap even without a race (defence when the fast pre-check is bypassed).
func TestCreateBookingTxOverlapRecheckSequential(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID, offeringID, memberID, _, offering := claimFixture(t, st)

	contact := Contact{TenantID: tenantID, Name: "Seq", Phone: "+1"}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}
	startsAt := nextNoonUTC()
	guard := SlotGuard{Buffer: time.Duration(offering.BufferMin) * time.Minute, Capacity: offering.Capacity}
	mk := func(start time.Time) *Booking {
		return &Booking{
			TenantID: tenantID, OfferingID: offeringID, TeamMemberID: memberID, ContactID: contact.ID,
			StartsAt: start, EndsAt: start.Add(30 * time.Minute),
			Status: StatusPending, Source: "api",
		}
	}
	if err := st.CreateBookingTx(ctx, mk(startsAt), guard, "t", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	// Overlapping: ErrSlotConflict.
	if err := st.CreateBookingTx(ctx, mk(startsAt.Add(15*time.Minute)), guard, "t", []byte(`{}`)); !errors.Is(err, ErrSlotConflict) {
		t.Fatalf("overlap err = %v, want ErrSlotConflict", err)
	}
	// Adjacent (no buffer): allowed.
	if err := st.CreateBookingTx(ctx, mk(startsAt.Add(30*time.Minute)), guard, "t", []byte(`{}`)); err != nil {
		t.Fatalf("adjacent err = %v, want nil", err)
	}
	// A cancelled booking does NOT block the slot.
	if err := st.CreateBookingTx(ctx, mk(startsAt.Add(2*time.Hour)), guard, "t", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	bookings, _ := st.ListBookings(ctx, tenantID, BookingFilter{TeamMemberID: &memberID})
	var cancelledID uuid.UUID
	for _, b := range bookings {
		if b.StartsAt.Equal(startsAt.Add(2 * time.Hour)) {
			cancelledID = b.ID
		}
	}
	if err := st.SetBookingStatus(ctx, tenantID, cancelledID, StatusCancelled, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateBookingTx(ctx, mk(startsAt.Add(2*time.Hour)), guard, "t", []byte(`{}`)); err != nil {
		t.Fatalf("slot of cancelled booking must be reusable: %v", err)
	}
}

// Two concurrent reschedules of different bookings onto the SAME free slot:
// exactly one wins; the loser keeps its original times.
func TestRescheduleBookingConcurrentSameTarget(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID, offeringID, memberID, _, offering := claimFixture(t, st)

	contact := Contact{TenantID: tenantID, Name: "Move", Phone: "+1"}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}
	guard := SlotGuard{Buffer: time.Duration(offering.BufferMin) * time.Minute, Capacity: offering.Capacity}
	base := nextNoonUTC()
	mk := func(start time.Time) *Booking {
		return &Booking{
			TenantID: tenantID, OfferingID: offeringID, TeamMemberID: memberID, ContactID: contact.ID,
			StartsAt: start, EndsAt: start.Add(30 * time.Minute),
			Status: StatusConfirmed, Source: "api",
		}
	}
	a, b := mk(base), mk(base.Add(2*time.Hour))
	if err := st.CreateBookingTx(ctx, a, guard, "t", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateBookingTx(ctx, b, guard, "t", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	target := base.Add(4 * time.Hour)

	errs := make([]error, 2)
	var wg sync.WaitGroup
	barrier := make(chan struct{})
	for i, id := range []uuid.UUID{a.ID, b.ID} {
		wg.Add(1)
		go func(i int, id uuid.UUID) {
			defer wg.Done()
			<-barrier
			errs[i] = st.RescheduleBooking(ctx, tenantID, id, memberID, target, target.Add(30*time.Minute), guard, "t", []byte(`{}`))
		}(i, id)
	}
	close(barrier)
	wg.Wait()

	var ok, conflict int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrSlotConflict):
			conflict++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("ok=%d conflict=%d, want exactly one reschedule to win", ok, conflict)
	}
	gotA, err := st.GetBooking(ctx, tenantID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := st.GetBooking(ctx, tenantID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	moved := 0
	for _, got := range []Booking{gotA, gotB} {
		if got.StartsAt.Equal(target) {
			moved++
		}
	}
	if moved != 1 {
		t.Fatalf("moved=%d, want exactly one booking on the target slot", moved)
	}
}

// SPEC-W43 K-02: empty idempotency keys are stored as NULL (never deduped);
// the unique scope is (tenant_id, key) — the same key in two tenants is
// legal, within one tenant it conflicts, and the replay lookup still works.
func TestIdempotencyKeyNullAndTenantScope(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID, offeringID, memberID, _, _ := claimFixture(t, st)
	contact := Contact{TenantID: tenantID, Name: "Idem", Phone: "+1"}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}
	base := nextNoonUTC()
	mk := func(tid uuid.UUID, key string, i int) *Booking {
		return &Booking{
			TenantID: tid, OfferingID: offeringID, TeamMemberID: memberID, ContactID: contact.ID,
			StartsAt: base.Add(time.Duration(i) * time.Hour), EndsAt: base.Add(time.Duration(i)*time.Hour + 30*time.Minute),
			Status: StatusPending, Source: "api", IdempotencyKey: key,
		}
	}
	// Two keyless bookings must both insert (NULL keys are not deduped).
	if err := st.CreateBookingTx(ctx, mk(tenantID, "", 0), SlotGuard{}, "t", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	keyless2 := mk(tenantID, "", 1)
	if err := st.CreateBookingTx(ctx, keyless2, SlotGuard{}, "t", []byte(`{}`)); err != nil {
		t.Fatalf("second keyless booking: %v", err)
	}
	got, err := st.GetBooking(ctx, tenantID, keyless2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.IdempotencyKey != "" {
		t.Fatalf("empty key round-trip = %q, want empty (stored NULL)", got.IdempotencyKey)
	}

	// Same key, same tenant => ErrConflict; replay lookup returns the row.
	first := mk(tenantID, "cmd-1", 2)
	if err := st.CreateBookingTx(ctx, first, SlotGuard{}, "t", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateBookingTx(ctx, mk(tenantID, "cmd-1", 3), SlotGuard{}, "t", []byte(`{}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate key err = %v, want ErrConflict", err)
	}
	replay, err := st.GetBookingByIdempotencyKey(ctx, tenantID, "cmd-1")
	if err != nil {
		t.Fatal(err)
	}
	if replay.ID != first.ID {
		t.Fatalf("replay id = %v, want original %v", replay.ID, first.ID)
	}

	// Same key, DIFFERENT tenant => allowed under the composite index.
	other := mk(uuid.New(), "cmd-1", 4)
	// Cross-tenant fixture rows (FK-less test schema) suffice to exercise
	// the index scope.
	if err := st.CreateBookingTx(ctx, other, SlotGuard{}, "t", []byte(`{}`)); err != nil {
		t.Fatalf("same key in another tenant must be allowed: %v", err)
	}
}
