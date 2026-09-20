package loyalty

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// SPEC-W45 K11 (work order H-7): the Deps.AccrueOnBookingCompleted hook —
// resolves bookings.contact_id, accrues booking_completed with the booking
// id as ref (per-booking idempotency via the ledger anchor), swallows
// no-program / missing-booking outcomes.

// hookFixture bootstraps the minimal contacts/bookings tables the hook
// reads (owned by the base schema in production).
func hookFixture(t *testing.T, st *Store) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(), `
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
    contact_id UUID,
    status TEXT NOT NULL DEFAULT 'confirmed'
);`); err != nil {
		t.Fatalf("hook fixture schema: %v", err)
	}
}

func TestAccrueOnBookingCompleted(t *testing.T) {
	st := newTestStore(t)
	hookFixture(t, st)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := uuid.New()
	bookingID := uuid.New()

	if _, err := st.pool.Exec(ctx,
		`INSERT INTO contacts (id, tenant_id, name) VALUES ($1,$2,'Ada')`, contactID, tenantID); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO bookings (id, tenant_id, contact_id) VALUES ($1,$2,$3)`, bookingID, tenantID, contactID); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	prog := mkProgram(tenantID) // booking_completed → 50 points
	if err := st.CreateProgram(ctx, &prog); err != nil {
		t.Fatalf("create program: %v", err)
	}

	d := &Deps{Store: st}
	d.AccrueOnBookingCompleted(tenantID, bookingID)

	w, err := st.GetWallet(ctx, tenantID, contactID)
	if err != nil {
		t.Fatalf("wallet after hook: %v", err)
	}
	if w.Balance != 50 || w.LifetimeEarned != 50 {
		t.Fatalf("wallet = %+v, want 50 earned", w)
	}

	// Replay (idempotent complete retry): the ledger anchor makes it a
	// no-op — balance must not double.
	d.AccrueOnBookingCompleted(tenantID, bookingID)
	w, err = st.GetWallet(ctx, tenantID, contactID)
	if err != nil {
		t.Fatalf("wallet after replay: %v", err)
	}
	if w.Balance != 50 || w.LifetimeEarned != 50 {
		t.Fatalf("replay must not double-award: %+v", w)
	}

	// A SECOND booking is a distinct ref → another 50.
	booking2 := uuid.New()
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO bookings (id, tenant_id, contact_id) VALUES ($1,$2,$3)`, booking2, tenantID, contactID); err != nil {
		t.Fatalf("seed booking2: %v", err)
	}
	d.AccrueOnBookingCompleted(tenantID, booking2)
	w, err = st.GetWallet(ctx, tenantID, contactID)
	if err != nil || w.Balance != 100 {
		t.Fatalf("second booking: %+v, %v — want 100", w, err)
	}
}

// Swallowed outcomes (best-effort hook): no active program, missing
// booking, nil contact — none of them error or panic the completion path.
func TestAccrueOnBookingCompletedSkips(t *testing.T) {
	st := newTestStore(t)
	hookFixture(t, st)
	ctx := context.Background()
	tenantID := uuid.New()
	d := &Deps{Store: st}

	// No active program at all → ErrNoActiveProgram mapped to a nil outcome.
	d.AccrueOnBookingCompleted(tenantID, uuid.New())

	// Program exists but the booking is unknown → skip.
	prog := mkProgram(tenantID)
	if err := st.CreateProgram(ctx, &prog); err != nil {
		t.Fatalf("create program: %v", err)
	}
	d.AccrueOnBookingCompleted(tenantID, uuid.New())

	// Booking with NULL contact_id → no wallet to accrue to → skip.
	contactless := uuid.New()
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO bookings (id, tenant_id, contact_id) VALUES ($1,$2,NULL)`, contactless, tenantID); err != nil {
		t.Fatalf("seed contactless booking: %v", err)
	}
	d.AccrueOnBookingCompleted(tenantID, contactless)

	// Nothing accrued anywhere: no wallets for the tenant.
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM loyalty_wallets WHERE tenant_id=$1`, tenantID).Scan(&n); err != nil {
		t.Fatalf("count wallets: %v", err)
	}
	if n != 0 {
		t.Fatalf("wallets = %d, want 0 (all hook calls must have skipped)", n)
	}
}
