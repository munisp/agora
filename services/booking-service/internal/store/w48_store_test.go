package store

// SPEC-W48 (W48-A item A5) store tests — embedded Postgres (newTestStore,
// -short skips): incident resolve tx, waitlist notify/expire txs, the
// auto-expiry sweep SQL, booking payment columns and booking_reviews.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// w48SeedIncident inserts one fresh incident (status new) for tenantID.
func w48SeedIncident(t *testing.T, st *Store, tenantID uuid.UUID) Incident {
	t.Helper()
	inc := Incident{TenantID: tenantID, ReferenceNumber: "INC-1", IncidentType: "iot", Severity: "high",
		Payload: []byte(`{"source":"test"}`)}
	created, err := st.InsertIncident(context.Background(), &inc)
	if err != nil || !created {
		t.Fatalf("seed incident: created=%v err=%v", created, err)
	}
	return inc
}

func TestResolveIncidentTx(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	inc := w48SeedIncident(t, st, tenantID)

	// Happy path: new → resolved with actor + timestamp + audit outbox row.
	out, err := st.ResolveIncidentTx(ctx, tenantID, inc.ID, "op-1", "fixed on site", "test.events", []byte(`{"k":1}`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if out.Status != "resolved" || out.ResolvedBy != "op-1" || out.ResolvedAt == nil {
		t.Fatalf("resolved row: %+v", out)
	}
	// The state is durable.
	back, err := st.GetIncident(ctx, tenantID, inc.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if back.Status != "resolved" || back.ResolvedBy != "op-1" || back.ResolvedAt == nil {
		t.Fatalf("persisted row: %+v", back)
	}
	// Audit row: exactly one outbox event for the resolve.
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE aggregate_id=$1 AND topic='test.events'`, inc.ID).Scan(&n); err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	if n != 1 {
		t.Fatalf("audit outbox rows = %d, want 1", n)
	}

	// 409: a second resolve conflicts (resolved is terminal for this route).
	if _, err := st.ResolveIncidentTx(ctx, tenantID, inc.ID, "op-2", "", "test.events", []byte(`{"k":2}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("double resolve err = %v, want ErrConflict", err)
	}
	// 404: unknown incident.
	if _, err := st.ResolveIncidentTx(ctx, tenantID, uuid.New(), "op-1", "", "test.events", []byte(`{}`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown resolve err = %v, want ErrNotFound", err)
	}
	// Tenant isolation: the same incident under another tenant is a 404.
	if _, err := st.ResolveIncidentTx(ctx, uuid.New(), inc.ID, "op-1", "", "test.events", []byte(`{}`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant resolve err = %v, want ErrNotFound", err)
	}
	// closed is terminal too.
	inc2 := w48SeedIncident(t, st, tenantID)
	if err := st.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE incidents SET status='closed' WHERE id=$1`, inc2.ID)
		return err
	}); err != nil {
		t.Fatalf("force closed: %v", err)
	}
	if _, err := st.ResolveIncidentTx(ctx, tenantID, inc2.ID, "op-1", "", "", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("closed resolve err = %v, want ErrConflict", err)
	}
}

// w48SeedEntry inserts one waitlist entry with the given window.
func w48SeedEntry(t *testing.T, st *Store, tenantID uuid.UUID, start, end time.Time) WaitlistEntry {
	t.Helper()
	e := WaitlistEntry{TenantID: tenantID, OfferingID: uuid.New(), ContactName: "Ada",
		ContactPhone: "+234800", WindowStart: start, WindowEnd: end}
	if err := st.CreateWaitlistEntry(context.Background(), &e); err != nil {
		t.Fatalf("seed entry: %v", err)
	}
	return e
}

func TestMarkWaitlistNotifiedTx(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	entry := w48SeedEntry(t, st, tenantID, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))

	out, err := st.MarkWaitlistNotifiedTx(ctx, tenantID, entry.ID, "test.events", []byte(`{"entry_id":"x"}`))
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	if out.Status != WaitlistNotified {
		t.Fatalf("status = %q, want notified", out.Status)
	}
	// Outbox row committed atomically with the flip.
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE aggregate_id=$1 AND topic='test.events'`, entry.ID).Scan(&n); err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	if n != 1 {
		t.Fatalf("notify outbox rows = %d, want 1", n)
	}
	// 409: notify is only valid from waiting.
	if _, err := st.MarkWaitlistNotifiedTx(ctx, tenantID, entry.ID, "test.events", []byte(`{}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("double notify err = %v, want ErrConflict", err)
	}
	// 404: unknown entry.
	if _, err := st.MarkWaitlistNotifiedTx(ctx, tenantID, uuid.New(), "test.events", []byte(`{}`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown notify err = %v, want ErrNotFound", err)
	}
}

func TestExpireWaitlistTx(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	entry := w48SeedEntry(t, st, tenantID, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))

	out, err := st.ExpireWaitlistTx(ctx, tenantID, entry.ID, "op-1", "test.events", []byte(`{}`))
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if out.Status != WaitlistExpired {
		t.Fatalf("status = %q, want expired", out.Status)
	}
	// 409: already expired.
	if _, err := st.ExpireWaitlistTx(ctx, tenantID, entry.ID, "op-1", "", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("double expire err = %v, want ErrConflict", err)
	}
	// 409: claimed is not expirable.
	claimed := w48SeedEntry(t, st, tenantID, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))
	if err := st.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE waitlist SET status='claimed' WHERE id=$1`, claimed.ID)
		return err
	}); err != nil {
		t.Fatalf("force claimed: %v", err)
	}
	if _, err := st.ExpireWaitlistTx(ctx, tenantID, claimed.ID, "op-1", "", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("claimed expire err = %v, want ErrConflict", err)
	}
}

func TestExpireStaleWaitlist(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	now := time.Now()
	pastWaiting := w48SeedEntry(t, st, tenantID, now.Add(-3*time.Hour), now.Add(-time.Hour))
	pastNotified := w48SeedEntry(t, st, tenantID, now.Add(-3*time.Hour), now.Add(-time.Hour))
	if err := st.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE waitlist SET status='notified' WHERE id=$1`, pastNotified.ID)
		return err
	}); err != nil {
		t.Fatalf("force notified: %v", err)
	}
	future := w48SeedEntry(t, st, tenantID, now.Add(time.Hour), now.Add(2*time.Hour))

	expired, err := st.ExpireStaleWaitlist(ctx, 10)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(expired) != 2 {
		t.Fatalf("expired = %d, want 2 (past-window waiting+notified only)", len(expired))
	}
	// Only past-window rows flipped.
	for _, tc := range []struct {
		id   uuid.UUID
		want string
	}{
		{pastWaiting.ID, WaitlistExpired},
		{pastNotified.ID, WaitlistExpired},
		{future.ID, WaitlistWaiting},
	} {
		got, err := st.GetWaitlistEntry(ctx, tenantID, tc.id)
		if err != nil {
			t.Fatalf("get %s: %v", tc.id, err)
		}
		if got.Status != tc.want {
			t.Fatalf("entry %s status = %q, want %q", tc.id, got.Status, tc.want)
		}
	}
	// A second sweep is a no-op (idempotent).
	again, err := st.ExpireStaleWaitlist(ctx, 10)
	if err != nil || len(again) != 0 {
		t.Fatalf("second sweep = %d, %v; want 0, nil", len(again), err)
	}
}

// w48SeedBooking inserts one booking row directly (payment/review tests
// need no availability machinery).
func w48SeedBooking(t *testing.T, st *Store, tenantID uuid.UUID, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx := context.Background()
	err := st.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO bookings (id, tenant_id, offering_id, team_member_id, contact_id,
			       starts_at, ends_at, status, source)
			 VALUES ($1,$2,$3,NULL,NULL,now() + interval '1 hour', now() + interval '2 hours', $4, 'api')`,
			id, tenantID, uuid.New(), status)
		return err
	})
	if err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	return id
}

func TestInitializeBookingPaymentTx(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	// The shared test schema creates bookings AFTER store.New ran, so the
	// W48 payment-column migration must be re-applied here (same idiom as
	// ensureCRMColumns in newTestStore).
	if err := st.ensureBookingPaymentColumns(ctx); err != nil {
		t.Fatalf("payment columns: %v", err)
	}
	tenantID := uuid.New()
	bookingID := w48SeedBooking(t, st, tenantID, StatusPending)

	// Happy path: stamps ref + initialized.
	if err := st.InitializeBookingPaymentTx(ctx, tenantID, bookingID, "tx-1"); err != nil {
		t.Fatalf("init payment: %v", err)
	}
	status, ref, err := st.GetBookingPayment(ctx, tenantID, bookingID)
	if err != nil || status != PaymentStatusInitialized || ref != "tx-1" {
		t.Fatalf("payment state = %q/%q, %v", status, ref, err)
	}
	// Re-initialization over "initialized" is allowed (abandoned checkout).
	if err := st.InitializeBookingPaymentTx(ctx, tenantID, bookingID, "tx-2"); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	// 409: paid is terminal.
	if err := st.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE bookings SET payment_status='paid' WHERE id=$1`, bookingID)
		return err
	}); err != nil {
		t.Fatalf("force paid: %v", err)
	}
	if err := st.InitializeBookingPaymentTx(ctx, tenantID, bookingID, "tx-3"); !errors.Is(err, ErrConflict) {
		t.Fatalf("paid init err = %v, want ErrConflict", err)
	}
	// Tenant isolation: another tenant cannot see/initialize the booking.
	if err := st.InitializeBookingPaymentTx(ctx, uuid.New(), bookingID, "tx-4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant init err = %v, want ErrNotFound", err)
	}
	if _, _, err := st.GetBookingPayment(ctx, uuid.New(), bookingID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant get err = %v, want ErrNotFound", err)
	}
}

func TestBookingReviews(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	completed1 := w48SeedBooking(t, st, tenantID, StatusCompleted)
	completed2 := w48SeedBooking(t, st, tenantID, StatusCompleted)
	pending := w48SeedBooking(t, st, tenantID, StatusPending)

	// Happy path.
	r1 := BookingReview{TenantID: tenantID, BookingID: completed1, Rating: 5, Comment: "great", ReviewerName: "Ada"}
	if err := st.CreateBookingReview(ctx, &r1); err != nil {
		t.Fatalf("create review: %v", err)
	}
	// 409: one review per booking (UNIQUE(booking_id)).
	dup := BookingReview{TenantID: tenantID, BookingID: completed1, Rating: 1}
	if err := st.CreateBookingReview(ctx, &dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("dup review err = %v, want ErrConflict", err)
	}
	// Rating bounds are DB-enforced too (CHECK 1..5).
	bad := BookingReview{TenantID: tenantID, BookingID: completed2, Rating: 0}
	if err := st.CreateBookingReview(ctx, &bad); err == nil || errors.Is(err, ErrConflict) {
		t.Fatalf("rating-0 err = %v, want a CHECK violation", err)
	}
	// Aggregate math: 5 + 3 over completed bookings; the pending booking's
	// review (inserted directly — the handler blocks this) is excluded.
	r2 := BookingReview{TenantID: tenantID, BookingID: completed2, Rating: 3}
	if err := st.CreateBookingReview(ctx, &r2); err != nil {
		t.Fatalf("create review 2: %v", err)
	}
	r3 := BookingReview{TenantID: tenantID, BookingID: pending, Rating: 1}
	if err := st.CreateBookingReview(ctx, &r3); err != nil {
		t.Fatalf("create review 3: %v", err)
	}
	reviews, avg, count, err := st.ListBookingReviews(ctx, tenantID, 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if count != 2 || len(reviews) != 2 {
		t.Fatalf("count = %d len = %d, want 2/2 (completed only)", count, len(reviews))
	}
	if avg != 4.0 { // (5+3)/2 — the pending booking's review is excluded
		t.Fatalf("average = %v, want 4.0", avg)
	}
	// Limit cap (50) and pagination stay sane.
	_, _, _, err = st.ListBookingReviews(ctx, tenantID, 5000, -1)
	if err != nil {
		t.Fatalf("capped list: %v", err)
	}
	// Tenant isolation: another tenant sees nothing.
	_, _, count, err = st.ListBookingReviews(ctx, uuid.New(), 0, 0)
	if err != nil || count != 0 {
		t.Fatalf("cross-tenant list count = %d, %v; want 0, nil", count, err)
	}
}
