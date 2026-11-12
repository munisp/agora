package store

// SPEC-W51 (W51-B item B1) store tests — embedded Postgres (newTestStore,
// -short skips): public_ref format + determinism, migration idempotency,
// deterministic backfill of legacy rows, create-path assignment, the
// collision counter-regeneration (retry) path, per-tenant uniqueness under
// concurrent inserts, and the defense-in-depth UUID fallback in the payment
// transition info for a legacy row without a ref.

import (
	"context"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var publicRefRe = regexp.MustCompile(`^BK-[0-9A-Z]{8}$`)

// assertPublicRefFormat enforces the SPEC-W51 contract shape BK-XXXXXXXX
// (Crockford base32, uppercase — the alphabet itself excludes I/L/O/U).
func assertPublicRefFormat(t *testing.T, ref string) {
	t.Helper()
	if !publicRefRe.MatchString(ref) {
		t.Fatalf("public_ref %q does not match ^BK-[0-9A-Z]{8}$", ref)
	}
	for _, c := range ref[3:] {
		switch c {
		case 'I', 'L', 'O', 'U':
			t.Fatalf("public_ref %q contains ambiguous char %q (not Crockford base32)", ref, c)
		}
	}
}

func TestPublicRefForBooking(t *testing.T) {
	id := uuid.New()
	a, b := PublicRefForBooking(id), PublicRefForBooking(id)
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
	assertPublicRefFormat(t, a)
	// Counter candidates: same shape, deterministic, and different from
	// candidate 0 (sha256-mixed).
	c1, c1b := publicRefCandidate(id, 1), publicRefCandidate(id, 1)
	if c1 != c1b || c1 == a {
		t.Fatalf("counter candidate broken: %q/%q vs %q", c1, c1b, a)
	}
	assertPublicRefFormat(t, c1)
	// 1000 random IDs: 40-bit space — no collisions expected at this scale.
	seen := map[string]uuid.UUID{}
	for i := 0; i < 1000; i++ {
		rid := uuid.New()
		ref := PublicRefForBooking(rid)
		if prev, dup := seen[ref]; dup {
			t.Fatalf("public_ref collision %q for %s and %s", ref, prev, rid)
		}
		seen[ref] = rid
	}
}

// w51RawInsertBooking inserts a bare booking row with an explicit public_ref
// ("" simulates a pre-W51 legacy row awaiting backfill).
func w51RawInsertBooking(t *testing.T, st *Store, tenantID, bookingID uuid.UUID, publicRef string) {
	t.Helper()
	ctx := context.Background()
	err := st.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO bookings (id, tenant_id, offering_id, starts_at, ends_at, status, source, public_ref)
			 VALUES ($1,$2,$3,now() + interval '1 hour', now() + interval '2 hours', 'pending', 'api', $4)`,
			bookingID, tenantID, uuid.New(), publicRef)
		return err
	})
	if err != nil {
		t.Fatalf("raw insert booking: %v", err)
	}
}

func w51PublicRefOf(t *testing.T, st *Store, tenantID, bookingID uuid.UUID) string {
	t.Helper()
	b, err := st.GetBooking(context.Background(), tenantID, bookingID)
	if err != nil {
		t.Fatalf("get booking: %v", err)
	}
	return b.PublicRef
}

// TestEnsureBookingPublicRef_BackfillDeterministic: legacy '' rows are
// backfilled to the deterministic candidate refs; a forced collision advances
// the counter; re-running the migration changes nothing (idempotent).
func TestEnsureBookingPublicRef_BackfillDeterministic(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	plain := uuid.New() // no collision → candidate 0
	collidee := uuid.New()
	blocker := uuid.New() // pre-holds collidee's candidate-0 ref
	w51RawInsertBooking(t, st, tenantID, plain, "")
	w51RawInsertBooking(t, st, tenantID, blocker, PublicRefForBooking(collidee))
	w51RawInsertBooking(t, st, tenantID, collidee, "")

	if err := st.ensureBookingPublicRef(ctx); err != nil {
		t.Fatalf("ensure (backfill): %v", err)
	}
	if got, want := w51PublicRefOf(t, st, tenantID, plain), PublicRefForBooking(plain); got != want {
		t.Fatalf("backfilled ref = %q, want deterministic %q", got, want)
	}
	// The collidee's candidate 0 was already taken by blocker → counter 1.
	if got, want := w51PublicRefOf(t, st, tenantID, collidee), publicRefCandidate(collidee, 1); got != want {
		t.Fatalf("collision backfill = %q, want counter-1 %q", got, want)
	}
	assertPublicRefFormat(t, w51PublicRefOf(t, st, tenantID, blocker))

	// Snapshot every ref, re-run the migration twice, assert byte-identical.
	before := map[uuid.UUID]string{
		plain:     w51PublicRefOf(t, st, tenantID, plain),
		collidee:  w51PublicRefOf(t, st, tenantID, collidee),
		blocker:   w51PublicRefOf(t, st, tenantID, blocker),
	}
	for i := 0; i < 2; i++ {
		if err := st.ensureBookingPublicRef(ctx); err != nil {
			t.Fatalf("ensure re-run %d: %v", i, err)
		}
	}
	for id, want := range before {
		if got := w51PublicRefOf(t, st, tenantID, id); got != want {
			t.Fatalf("re-run changed ref of %s: %q → %q", id, want, got)
		}
	}
}

// TestCreateBookingPublicRef: the create path assigns the canonical ref,
// persists it and reads it back through GetBooking/ListBookings.
func TestCreateBookingPublicRef(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	b := &Booking{
		TenantID:   tenantID,
		OfferingID: uuid.New(),
		StartsAt:   time.Now().Add(24 * time.Hour).Truncate(time.Second),
		EndsAt:     time.Now().Add(25 * time.Hour).Truncate(time.Second),
		Status:     StatusPending,
		Source:     "api",
	}
	if err := st.CreateBookingTx(ctx, b, SlotGuard{}, "test.events", []byte(`{"type":"BookingCreated"}`)); err != nil {
		t.Fatalf("create: %v", err)
	}
	want := PublicRefForBooking(b.ID)
	if b.PublicRef != want {
		t.Fatalf("assigned public_ref = %q, want %q", b.PublicRef, want)
	}
	assertPublicRefFormat(t, b.PublicRef)
	got, err := st.GetBooking(ctx, tenantID, b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PublicRef != want {
		t.Fatalf("persisted public_ref = %q, want %q", got.PublicRef, want)
	}
	list, err := st.ListBookings(ctx, tenantID, BookingFilter{})
	if err != nil || len(list) != 1 || list[0].PublicRef != want {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

// TestCreateBookingPublicRefCollisionRetry: the in-tx counter loop
// regenerates when the canonical ref is already taken inside the tenant,
// and the same ref remains legal for ANOTHER tenant (uniqueness is
// (tenant_id, public_ref)).
func TestCreateBookingPublicRefCollisionRetry(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	victim := &Booking{
		ID:         uuid.New(),
		TenantID:   tenantID,
		OfferingID: uuid.New(),
		StartsAt:   time.Now().Add(48 * time.Hour).Truncate(time.Second),
		EndsAt:     time.Now().Add(49 * time.Hour).Truncate(time.Second),
		Status:     StatusPending,
		Source:     "api",
	}
	// A blocking row already owns the victim's canonical ref.
	w51RawInsertBooking(t, st, tenantID, uuid.New(), PublicRefForBooking(victim.ID))

	if err := st.CreateBookingTx(ctx, victim, SlotGuard{}, "test.events", []byte(`{"type":"BookingCreated"}`)); err != nil {
		t.Fatalf("create with collision: %v", err)
	}
	if want := publicRefCandidate(victim.ID, 1); victim.PublicRef != want {
		t.Fatalf("collision retry ref = %q, want counter-1 %q", victim.PublicRef, want)
	}
	assertPublicRefFormat(t, victim.PublicRef)

	// Another tenant may legitimately hold the SAME ref string.
	other := &Booking{
		TenantID:   uuid.New(),
		OfferingID: uuid.New(),
		StartsAt:   time.Now().Add(48 * time.Hour).Truncate(time.Second),
		EndsAt:     time.Now().Add(49 * time.Hour).Truncate(time.Second),
		Status:     StatusPending,
		Source:     "api",
		PublicRef:  PublicRefForBooking(victim.ID),
	}
	if err := st.CreateBookingTx(ctx, other, SlotGuard{}, "test.events", []byte(`{"type":"BookingCreated"}`)); err != nil {
		t.Fatalf("cross-tenant same ref must be allowed: %v", err)
	}
	if other.PublicRef != PublicRefForBooking(victim.ID) {
		t.Fatalf("cross-tenant ref rewritten: %q", other.PublicRef)
	}
}

// TestCreateBookingPublicRefConcurrent: N concurrent creates in one tenant
// all land with distinct, well-formed refs (the unique index + retry loop
// hold under the race).
func TestCreateBookingPublicRefConcurrent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	const n = 8
	bookings := make([]*Booking, n)
	var wg sync.WaitGroup
	errs := make([]error, n)
	base := time.Now().Add(96 * time.Hour).Truncate(time.Second)
	for i := 0; i < n; i++ {
		bookings[i] = &Booking{
			TenantID:   tenantID,
			OfferingID: uuid.New(),
			StartsAt:   base.Add(time.Duration(i) * 2 * time.Hour),
			EndsAt:     base.Add(time.Duration(i)*2*time.Hour + time.Hour),
			Status:     StatusPending,
			Source:     "api",
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = st.CreateBookingTx(ctx, bookings[i], SlotGuard{}, "test.events", []byte(`{"type":"BookingCreated"}`))
		}(i)
	}
	wg.Wait()
	refs := map[string]uuid.UUID{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent create %d: %v", i, err)
		}
		assertPublicRefFormat(t, bookings[i].PublicRef)
		if prev, dup := refs[bookings[i].PublicRef]; dup {
			t.Fatalf("duplicate public_ref %q (%s and %s)", bookings[i].PublicRef, prev, bookings[i].ID)
		}
		refs[bookings[i].PublicRef] = bookings[i].ID
	}
}

// TestPaymentTransitionPublicRefFallback: a legacy row whose public_ref is
// still '' (pre-backfill, defense-in-depth) keeps the UUID booking_ref in
// the transition info — the event is never blocked by the missing ref.
func TestPaymentTransitionPublicRefFallback(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID, offeringID := w49Fixture(t, st)

	bookingID := w49SeedBooking(t, st, tenantID, offeringID, StatusPending) // public_ref ''
	if err := st.InitializeBookingPaymentTx(ctx, tenantID, bookingID, "tx-legacy", ""); err != nil {
		t.Fatalf("init: %v", err)
	}
	builder, got := w50InfoBuilder("test.events")
	outcome, _, err := st.MarkBookingPaymentPaidTx(ctx, tenantID, "tx-legacy", 75.00, "NGN", builder)
	if err != nil || outcome != PaymentMarkPaid {
		t.Fatalf("legacy mark = %q/%v, want paid/nil", outcome, err)
	}
	if len(*got) != 1 {
		t.Fatalf("builder calls = %d, want 1", len(*got))
	}
	info := (*got)[0]
	if info.BookingRef != bookingID.String() {
		t.Fatalf("legacy fallback booking_ref = %q, want UUID %s", info.BookingRef, bookingID)
	}
	if info.ExpectedAmountMinor != 7500 {
		t.Fatalf("ExpectedAmountMinor = %d, want 7500", info.ExpectedAmountMinor)
	}

	// After the (mandatory) backfill runs, the SAME row reports its ref.
	if err := st.ensureBookingPublicRef(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if ref := w51PublicRefOf(t, st, tenantID, bookingID); ref != PublicRefForBooking(bookingID) {
		t.Fatalf("backfilled legacy ref = %q, want %q", ref, PublicRefForBooking(bookingID))
	}
	// Replay is a no-op (already paid) but must not error.
	outcome, _, err = st.MarkBookingPaymentPaidTx(ctx, tenantID, "tx-legacy", 75.00, "NGN", builder)
	if err != nil || outcome != PaymentMarkAlreadyPaid {
		t.Fatalf("replay = %q/%v, want already_paid/nil", outcome, err)
	}
}
