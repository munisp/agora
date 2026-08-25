package bookingops

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// SPEC-W43 K-08 sweeper tests run against a real (embedded) Postgres so the
// stale-pending scan exercises the actual query. Skipped under -short like
// every embedded-postgres test in this repo. Port 5571 is dedicated to this
// file (collision set: 5432/5433/5434/5544/5546/5547/5548/5561-5566/5570).
const sweeperTestPort = 5571

const sweeperTestSchema = `
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
CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    sent_at TIMESTAMPTZ
);`

// recordingSaga is a fake SagaStarter capturing every StartBookingSaga call.
type recordingSaga struct {
	mu    sync.Mutex
	calls []SagaInput
	err   error
}

func (r *recordingSaga) StartBookingSaga(_ context.Context, in SagaInput) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, in)
	if r.err != nil {
		return "", r.err
	}
	return "run-1", nil
}

func (r *recordingSaga) started() []SagaInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SagaInput(nil), r.calls...)
}

func newSweeperFixture(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres sweeper test in -short mode")
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
	// Apply the minimal schema via a raw connection first (portal_test.go
	// idiom): store.New only bootstraps its own additive tables.
	if pool, err := pgxpool.New(ctx, dsn); err != nil {
		t.Fatalf("raw pool: %v", err)
	} else {
		if _, err := pool.Exec(ctx, sweeperTestSchema); err != nil {
			t.Fatalf("sweeper schema: %v", err)
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

// addSweeperBooking seeds one booking (status configurable) offsetHours in
// the future — distinct offsets keep fixture slots disjoint (the K-01
// in-transaction overlap guard rejects same-slot seeds).
func addSweeperBooking(t *testing.T, ctx context.Context, st *store.Store, tenantID uuid.UUID, offering store.Offering, contact store.Contact, status string, offsetHours int) store.Booking {
	t.Helper()
	start := time.Now().UTC().Add(time.Duration(offsetHours) * time.Hour)
	b := store.Booking{
		TenantID: tenantID, OfferingID: offering.ID, ContactID: contact.ID,
		StartsAt: start, EndsAt: start.Add(30 * time.Minute),
		Status: status, Source: "api",
	}
	if err := st.CreateBookingTx(ctx, &b, store.SlotGuard{}, "test.events", []byte(`{}`)); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	return b
}

func TestSweeperRedrivesStalePendingBookings(t *testing.T) {
	st, ctx := newSweeperFixture(t)
	tenantID := uuid.New()
	offering := store.Offering{TenantID: tenantID, Name: "Cut", DurationMin: 30, PriceCents: 5000, Currency: "NGN"}
	if err := st.CreateOffering(ctx, &offering); err != nil {
		t.Fatal(err)
	}
	contact := store.Contact{TenantID: tenantID, Name: "Ada", Phone: "+234800"}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}

	pending := addSweeperBooking(t, ctx, st, tenantID, offering, contact, store.StatusPending, 1)
	confirmed := addSweeperBooking(t, ctx, st, tenantID, offering, contact, store.StatusConfirmed, 2)

	saga := &recordingSaga{}
	sw := &Sweeper{Store: st, Saga: saga, Log: zap.NewNop(), MinAge: time.Millisecond}
	time.Sleep(5 * time.Millisecond) // let the seeded rows age past MinAge

	n, err := sw.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("redriven = %d, want 1 (only the pending booking)", n)
	}
	calls := saga.started()
	if len(calls) != 1 {
		t.Fatalf("saga starts = %d, want 1", len(calls))
	}
	if calls[0].BookingID != pending.ID.String() {
		t.Fatalf("redriven booking = %s, want %s", calls[0].BookingID, pending.ID)
	}
	if calls[0].BookingID == confirmed.ID.String() {
		t.Fatal("confirmed booking must never be swept")
	}
	if calls[0].PriceCents != 5000 || calls[0].Currency != "NGN" || calls[0].ContactPhone != "+234800" {
		t.Fatalf("saga input = %+v", calls[0])
	}
}

// A transient saga-start failure is logged and skipped, not fatal: the row
// stays pending and is retried by the next sweep.
func TestSweeperToleratesSagaFailure(t *testing.T) {
	st, ctx := newSweeperFixture(t)
	tenantID := uuid.New()
	offering := store.Offering{TenantID: tenantID, Name: "Cut", DurationMin: 30}
	if err := st.CreateOffering(ctx, &offering); err != nil {
		t.Fatal(err)
	}
	contact := store.Contact{TenantID: tenantID, Name: "Bayo", Phone: "+234801"}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}
	b := addSweeperBooking(t, ctx, st, tenantID, offering, contact, store.StatusPending, 1)

	saga := &recordingSaga{err: fmt.Errorf("temporal down")}
	sw := &Sweeper{Store: st, Saga: saga, Log: zap.NewNop(), MinAge: time.Millisecond}
	time.Sleep(5 * time.Millisecond)

	n, err := sw.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("redriven = %d, want 0 (saga start failed)", n)
	}
	got, err := st.GetBooking(ctx, tenantID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusPending {
		t.Fatalf("status = %q, want pending after failed re-drive", got.Status)
	}
	// Next sweep retries the same row (idempotent workflow id on the Temporal side).
	saga.err = nil
	n, err = sw.SweepOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("retry sweep n=%d err=%v, want 1/nil", n, err)
	}
}

// Without a Temporal client (nil Saga) the sweeper is a safe no-op.
func TestSweeperNilSagaNoop(t *testing.T) {
	st, ctx := newSweeperFixture(t)
	sw := &Sweeper{Store: st, Log: zap.NewNop(), MinAge: time.Millisecond}
	n, err := sw.SweepOnce(ctx)
	if err != nil || n != 0 {
		t.Fatalf("nil-saga sweep n=%d err=%v, want 0/nil", n, err)
	}
}
