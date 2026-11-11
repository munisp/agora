package store

// SPEC-W49 (W49-B items B3/B4) store tests — embedded Postgres (newTestStore,
// -short skips): MarkBookingPaymentPaidTx reconciliation (happy path,
// idempotent replay, unknown tx_ref, amount/currency mismatch, tenant
// isolation) and the booking_payment_attempts supersede chain.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// w49SeedBooking inserts one booking row bound to a REAL offering (the
// amount check joins offerings for the expected price).
func w49SeedBooking(t *testing.T, st *Store, tenantID, offeringID uuid.UUID, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx := context.Background()
	err := st.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO bookings (id, tenant_id, offering_id, team_member_id, contact_id,
			       starts_at, ends_at, status, source)
			 VALUES ($1,$2,$3,NULL,NULL,now() + interval '1 hour', now() + interval '2 hours', $4, 'api')`,
			id, tenantID, offeringID, status)
		return err
	})
	if err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	return id
}

// w49Fixture returns (tenantID, offeringID) for a 75.00 NGN offering.
func w49Fixture(t *testing.T, st *Store) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	// The shared test schema creates bookings AFTER store.New ran, so the
	// W48/W49 migrations must be re-applied here (same idiom as
	// ensureCRMColumns in newTestStore). Re-running ALSO proves the W49
	// migration is idempotent.
	if err := st.ensureBookingPaymentColumns(ctx); err != nil {
		t.Fatalf("payment columns: %v", err)
	}
	if err := st.ensureBookingPaymentAttemptsTable(ctx); err != nil {
		t.Fatalf("payment attempts table: %v", err)
	}
	tenantID := uuid.New()
	off := Offering{TenantID: tenantID, Name: "Consult", DurationMin: 30,
		PriceCents: 7500, Currency: "NGN", Bookable: true}
	if err := st.CreateOffering(ctx, &off); err != nil {
		t.Fatalf("offering: %v", err)
	}
	return tenantID, off.ID
}

// w49OutboxBuilder records the outbox rows the tx would commit.
func w49OutboxBuilder(topic string) (PaymentOutboxBuilder, *[]string) {
	var got []string
	return func(outcome PaymentMarkOutcome, bookingID uuid.UUID) (string, []byte, error) {
		got = append(got, string(outcome))
		return topic, []byte(fmt.Sprintf(`{"outcome":%q,"booking_id":%q}`, outcome, bookingID)), nil
	}, &got
}

func w49OutboxCount(t *testing.T, st *Store, bookingID uuid.UUID, topic string) int {
	t.Helper()
	var n int
	if err := st.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE aggregate_id=$1 AND topic=$2`, bookingID, topic).Scan(&n); err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	return n
}

func w49AttemptStatus(t *testing.T, st *Store, tenantID uuid.UUID, txRef string) string {
	t.Helper()
	var status string
	err := st.withTenant(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT status FROM booking_payment_attempts WHERE tenant_id=$1 AND tx_ref=$2`,
			tenantID, txRef).Scan(&status)
	})
	if err != nil {
		t.Fatalf("attempt status for %s: %v", txRef, err)
	}
	return status
}

func TestMarkBookingPaymentPaidTx(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID, offeringID := w49Fixture(t, st)
	const topic = "test.booking.events"

	// --- Happy path: initialized → paid, attempt marked, outbox written.
	bookingID := w49SeedBooking(t, st, tenantID, offeringID, StatusPending)
	if err := st.InitializeBookingPaymentTx(ctx, tenantID, bookingID, "tx-100", "https://pay.test/x"); err != nil {
		t.Fatalf("init payment: %v", err)
	}
	builder, got := w49OutboxBuilder(topic)
	outcome, bid, err := st.MarkBookingPaymentPaidTx(ctx, tenantID, "tx-100", 75.00, "NGN", builder)
	if err != nil || outcome != PaymentMarkPaid || bid != bookingID {
		t.Fatalf("mark paid = %q/%s/%v, want paid/%s/nil", outcome, bid, err, bookingID)
	}
	status, ref, err := st.GetBookingPayment(ctx, tenantID, bookingID)
	if err != nil || status != PaymentStatusPaid || ref != "tx-100" {
		t.Fatalf("payment state = %q/%q, %v; want paid/tx-100", status, ref, err)
	}
	if s := w49AttemptStatus(t, st, tenantID, "tx-100"); s != AttemptStatusPaid {
		t.Fatalf("attempt status = %q, want paid", s)
	}
	if len(*got) != 1 || (*got)[0] != string(PaymentMarkPaid) {
		t.Fatalf("builder calls = %v, want [paid]", *got)
	}
	if n := w49OutboxCount(t, st, bookingID, topic); n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
	// Booking STATUS is untouched (payment_status only).
	b, err := st.GetBooking(ctx, tenantID, bookingID)
	if err != nil || b.Status != StatusPending {
		t.Fatalf("booking status = %q, %v; want pending (untouched)", b.Status, err)
	}

	// --- Idempotent replay: already paid → no-op, no second outbox row.
	outcome, _, err = st.MarkBookingPaymentPaidTx(ctx, tenantID, "tx-100", 75.00, "NGN", builder)
	if err != nil || outcome != PaymentMarkAlreadyPaid {
		t.Fatalf("replay = %q/%v, want already_paid/nil", outcome, err)
	}
	if len(*got) != 1 || w49OutboxCount(t, st, bookingID, topic) != 1 {
		t.Fatalf("replay must not emit: builder=%v outbox=%d", *got, w49OutboxCount(t, st, bookingID, topic))
	}

	// --- Unknown tx_ref → ErrNotFound (the consumer acks it; never retried).
	if _, _, err := st.MarkBookingPaymentPaidTx(ctx, tenantID, "tx-unknown", 75.00, "NGN", builder); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown tx_ref err = %v, want ErrNotFound", err)
	}

	// --- Tenant isolation: the same tx_ref under another tenant is unknown.
	if _, _, err := st.MarkBookingPaymentPaidTx(ctx, uuid.New(), "tx-100", 75.00, "NGN", builder); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant tx_ref err = %v, want ErrNotFound", err)
	}

	// --- Amount mismatch: captured 70.00 vs 75.00 → paid_amount_mismatch +
	// alert outbox, attempt flagged; replay is a no-op (one alert only).
	booking2 := w49SeedBooking(t, st, tenantID, offeringID, StatusPending)
	if err := st.InitializeBookingPaymentTx(ctx, tenantID, booking2, "tx-200", ""); err != nil {
		t.Fatalf("init payment 2: %v", err)
	}
	outcome, bid, err = st.MarkBookingPaymentPaidTx(ctx, tenantID, "tx-200", 70.00, "NGN", builder)
	if err != nil || outcome != PaymentMarkAmountMismatch || bid != booking2 {
		t.Fatalf("mismatch = %q/%s/%v, want amount_mismatch", outcome, bid, err)
	}
	status, _, err = st.GetBookingPayment(ctx, tenantID, booking2)
	if err != nil || status != PaymentStatusPaidAmountMismatch {
		t.Fatalf("mismatch state = %q, %v; want paid_amount_mismatch", status, err)
	}
	if s := w49AttemptStatus(t, st, tenantID, "tx-200"); s != AttemptStatusAmountMismatch {
		t.Fatalf("mismatch attempt = %q, want amount_mismatch", s)
	}
	if n := w49OutboxCount(t, st, booking2, topic); n != 1 {
		t.Fatalf("mismatch outbox rows = %d, want 1 alert", n)
	}
	outcome, _, err = st.MarkBookingPaymentPaidTx(ctx, tenantID, "tx-200", 70.00, "NGN", builder)
	if err != nil || outcome != PaymentMarkAlreadyAmountMismatch {
		t.Fatalf("mismatch replay = %q/%v, want already_amount_mismatch/nil", outcome, err)
	}
	if n := w49OutboxCount(t, st, booking2, topic); n != 1 {
		t.Fatalf("mismatch replay outbox rows = %d, want still 1", n)
	}

	// --- Currency mismatch cannot be compared → flagged too.
	booking3 := w49SeedBooking(t, st, tenantID, offeringID, StatusPending)
	if err := st.InitializeBookingPaymentTx(ctx, tenantID, booking3, "tx-300", ""); err != nil {
		t.Fatalf("init payment 3: %v", err)
	}
	outcome, _, err = st.MarkBookingPaymentPaidTx(ctx, tenantID, "tx-300", 75.00, "USD", builder)
	if err != nil || outcome != PaymentMarkAmountMismatch {
		t.Fatalf("currency mismatch = %q/%v, want amount_mismatch", outcome, err)
	}
}

func TestPaymentAttemptsSupersedeChain(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID, offeringID := w49Fixture(t, st)
	bookingID := w49SeedBooking(t, st, tenantID, offeringID, StatusPending)

	// Three successive checkouts: each supersedes the prior open attempt.
	for _, ref := range []string{"tx-a", "tx-b", "tx-c"} {
		if err := st.InitializeBookingPaymentTx(ctx, tenantID, bookingID, ref, "https://pay.test/"+ref); err != nil {
			t.Fatalf("init %s: %v", ref, err)
		}
	}
	attempts, err := st.ListBookingPaymentAttempts(ctx, tenantID, bookingID)
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attempts) != 3 {
		t.Fatalf("attempts = %d, want 3", len(attempts))
	}
	want := map[string]string{"tx-a": AttemptStatusSuperseded, "tx-b": AttemptStatusSuperseded, "tx-c": AttemptStatusInitialized}
	for _, a := range attempts {
		if want[a.TxRef] != a.Status {
			t.Fatalf("attempt %s status = %q, want %q", a.TxRef, a.Status, want[a.TxRef])
		}
	}
	// Fast path still reflects the latest attempt.
	status, ref, err := st.GetBookingPayment(ctx, tenantID, bookingID)
	if err != nil || status != PaymentStatusInitialized || ref != "tx-c" {
		t.Fatalf("fast path = %q/%q, %v; want initialized/tx-c", status, ref, err)
	}

	// Idempotent rail replay re-initializing the SAME ref: no duplicate
	// attempt row, the current attempt stays initialized (not superseded).
	if err := st.InitializeBookingPaymentTx(ctx, tenantID, bookingID, "tx-c", "https://pay.test/tx-c"); err != nil {
		t.Fatalf("same-ref re-init: %v", err)
	}
	attempts, err = st.ListBookingPaymentAttempts(ctx, tenantID, bookingID)
	if err != nil || len(attempts) != 3 {
		t.Fatalf("after same-ref re-init attempts = %d, %v; want 3", len(attempts), err)
	}
	if s := w49AttemptStatus(t, st, tenantID, "tx-c"); s != AttemptStatusInitialized {
		t.Fatalf("tx-c after same-ref re-init = %q, want initialized", s)
	}

	// The audit chain is tenant-scoped (RLS + app filter).
	attempts, err = st.ListBookingPaymentAttempts(ctx, uuid.New(), bookingID)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("cross-tenant attempts = %d, %v; want 0", len(attempts), err)
	}
}
