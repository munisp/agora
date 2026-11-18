package loyalty

// SPEC-W80 B2: the first_txn earn rule — the contact's FIRST completed
// booking accrues first_txn points ON TOP of booking_completed (the rule
// was dead: nothing ever fired the event); the second completed booking
// accrues only booking_completed; replays never double-award.

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestAccrueOnBookingCompletedFirstTxn(t *testing.T) {
	st := newTestStore(t)
	hookFixture(t, st)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := uuid.New()

	if _, err := st.pool.Exec(ctx,
		`INSERT INTO contacts (id, tenant_id, name) VALUES ($1,$2,'Ada')`, contactID, tenantID); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	prog := mkProgram(tenantID) // booking_completed → 50, first_txn → 100
	if err := st.CreateProgram(ctx, &prog); err != nil {
		t.Fatalf("create program: %v", err)
	}

	// First booking completes (status flip is COMMITTED before the hook
	// fires — the fixture mirrors that by seeding status='completed').
	booking1 := uuid.New()
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO bookings (id, tenant_id, contact_id, status) VALUES ($1,$2,$3,'completed')`,
		booking1, tenantID, contactID); err != nil {
		t.Fatalf("seed booking1: %v", err)
	}
	d := &Deps{Store: st}
	d.AccrueOnBookingCompleted(tenantID, booking1)

	w, err := st.GetWallet(ctx, tenantID, contactID)
	if err != nil {
		t.Fatalf("wallet after first completion: %v", err)
	}
	// 50 (booking_completed) + 100 (first_txn) — the dead rule is alive.
	if w.Balance != 150 || w.LifetimeEarned != 150 {
		t.Fatalf("wallet = %+v, want 150 earned (50 booking_completed + 100 first_txn)", w)
	}

	// Replay of the same completion: both anchors dedupe — no movement.
	d.AccrueOnBookingCompleted(tenantID, booking1)
	w, err = st.GetWallet(ctx, tenantID, contactID)
	if err != nil {
		t.Fatalf("wallet after replay: %v", err)
	}
	if w.Balance != 150 {
		t.Fatalf("replay must not double-award: %+v", w)
	}

	// Second completed booking: only booking_completed (first_txn already
	// fired for this contact — anchored to the contact, not the booking).
	booking2 := uuid.New()
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO bookings (id, tenant_id, contact_id, status) VALUES ($1,$2,$3,'completed')`,
		booking2, tenantID, contactID); err != nil {
		t.Fatalf("seed booking2: %v", err)
	}
	d.AccrueOnBookingCompleted(tenantID, booking2)
	w, err = st.GetWallet(ctx, tenantID, contactID)
	if err != nil {
		t.Fatalf("wallet after second completion: %v", err)
	}
	if w.Balance != 200 {
		t.Fatalf("wallet = %+v, want 200 (second completion adds only 50 booking_completed)", w)
	}

	// No active program: the whole hook is a nil outcome (booking
	// completion unaffected) — covered by the ErrNoActiveProgram branch.
	otherTenant := uuid.New()
	otherContact := uuid.New()
	booking3 := uuid.New()
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO contacts (id, tenant_id, name) VALUES ($1,$2,'Bea')`, otherContact, otherTenant); err != nil {
		t.Fatalf("seed other contact: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO bookings (id, tenant_id, contact_id, status) VALUES ($1,$2,$3,'completed')`,
		booking3, otherTenant, otherContact); err != nil {
		t.Fatalf("seed booking3: %v", err)
	}
	d.AccrueOnBookingCompleted(otherTenant, booking3) // must not panic
	if _, err := st.GetWallet(ctx, otherTenant, otherContact); err == nil {
		t.Fatal("wallet must not exist for a tenant without an active program")
	}
}
