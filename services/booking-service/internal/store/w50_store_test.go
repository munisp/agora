package store

// SPEC-W50 (W50-A items A1/A2) store tests — embedded Postgres (newTestStore,
// -short skips): MarkBookingPaymentPaidByAttemptTx superseded-link fallback
// (same single-tx transition semantics as the fast path, payment_ref advance,
// attempt marking, exactly-once replay, mismatch path, strict tenant
// isolation) and the enriched PaymentTransitionInfo (customer/contact/
// booking_ref/paid_at) handed to the outbox builder.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// w50SeedBookingWithContact inserts a contact (name/phone/email) and a
// booking bound to it, returning the booking ID.
func w50SeedBookingWithContact(t *testing.T, st *Store, tenantID, offeringID uuid.UUID, name, phone, email string) uuid.UUID {
	t.Helper()
	bookingID := uuid.New()
	ctx := context.Background()
	err := st.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		contactID := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO contacts (id, tenant_id, name, phone, email) VALUES ($1,$2,$3,$4,$5)`,
			contactID, tenantID, name, phone, email); err != nil {
			return err
		}
		// SPEC-W51 B1/B2: the seed carries the canonical public_ref (the
		// backfill normally owns legacy rows; raw test inserts set it
		// explicitly) so the transition info assertions exercise the new
		// booking_ref contract, not the UUID fallback.
		_, err := tx.Exec(ctx,
			`INSERT INTO bookings (id, tenant_id, offering_id, team_member_id, contact_id,
			       starts_at, ends_at, status, source, public_ref)
			 VALUES ($1,$2,$3,NULL,$4,now() + interval '1 hour', now() + interval '2 hours', 'pending', 'api', $5)`,
			bookingID, tenantID, offeringID, contactID, PublicRefForBooking(bookingID))
		return err
	})
	if err != nil {
		t.Fatalf("seed booking with contact: %v", err)
	}
	return bookingID
}

// w50InfoBuilder captures the PaymentTransitionInfo the tx hands to the
// outbox builder (and writes a real outbox row like the consumer's builder).
func w50InfoBuilder(topic string) (PaymentOutboxBuilder, *[]PaymentTransitionInfo) {
	var got []PaymentTransitionInfo
	return func(outcome PaymentMarkOutcome, info PaymentTransitionInfo) (string, []byte, error) {
		got = append(got, info)
		return topic, []byte(`{"outcome":"` + string(outcome) + `"}`), nil
	}, &got
}

func TestMarkBookingPaymentPaidByAttemptTx(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID, offeringID := w49Fixture(t, st)
	const topic = "test.booking.events"

	// --- Superseded-link capture: paid through the OLD ref after
	// re-initializing to a new one.
	bookingID := w50SeedBookingWithContact(t, st, tenantID, offeringID,
		"Ada Lovelace", "+2348012345678", "ada@example.com")
	if err := st.InitializeBookingPaymentTx(ctx, tenantID, bookingID, "tx-old", "https://pay.test/old"); err != nil {
		t.Fatalf("init old: %v", err)
	}
	if err := st.InitializeBookingPaymentTx(ctx, tenantID, bookingID, "tx-new", "https://pay.test/new"); err != nil {
		t.Fatalf("init new: %v", err)
	}
	// Fast path misses tx-old (payment_ref advanced to tx-new) — that is
	// the precondition of the fallback.
	if _, _, err := st.MarkBookingPaymentPaidTx(ctx, tenantID, "tx-old", 75.00, "NGN", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fast path on superseded ref err = %v, want ErrNotFound", err)
	}

	builder, got := w50InfoBuilder(topic)
	outcome, bid, err := st.MarkBookingPaymentPaidByAttemptTx(ctx, tenantID, "tx-old", 75.00, "NGN", builder)
	if err != nil || outcome != PaymentMarkPaid || bid != bookingID {
		t.Fatalf("fallback mark = %q/%s/%v, want paid/%s/nil", outcome, bid, err, bookingID)
	}
	// Booking paid AND payment_ref advanced to the capture's tx_ref.
	status, ref, err := st.GetBookingPayment(ctx, tenantID, bookingID)
	if err != nil || status != PaymentStatusPaid || ref != "tx-old" {
		t.Fatalf("payment state = %q/%q, %v; want paid/tx-old", status, ref, err)
	}
	if s := w49AttemptStatus(t, st, tenantID, "tx-old"); s != AttemptStatusPaid {
		t.Fatalf("old attempt = %q, want paid", s)
	}
	// Enriched transition info (SPEC-W50 A2, SPEC-W51 B2): contact fields
	// from the booking's contact, booking_ref = the public_ref (BK-XXXXXXXX,
	// not the UUID), expected_amount_minor = the offering price (75.00 NGN),
	// paid_at set.
	if len(*got) != 1 {
		t.Fatalf("builder calls = %d, want 1", len(*got))
	}
	info := (*got)[0]
	if info.BookingID != bookingID || info.BookingRef != PublicRefForBooking(bookingID) ||
		info.CustomerName != "Ada Lovelace" || info.ContactPhone != "+2348012345678" ||
		info.ContactEmail != "ada@example.com" || info.PaidAt.IsZero() ||
		info.ExpectedAmountMinor != 7500 {
		t.Fatalf("transition info = %+v", info)
	}
	assertPublicRefFormat(t, info.BookingRef)
	if n := w49OutboxCount(t, st, bookingID, topic); n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}

	// --- Exactly-once replay: attempt already paid → no-op, no 2nd outbox.
	outcome, _, err = st.MarkBookingPaymentPaidByAttemptTx(ctx, tenantID, "tx-old", 75.00, "NGN", builder)
	if err != nil || outcome != PaymentMarkAlreadyPaid {
		t.Fatalf("replay = %q/%v, want already_paid/nil", outcome, err)
	}
	if len(*got) != 1 || w49OutboxCount(t, st, bookingID, topic) != 1 {
		t.Fatalf("replay must not emit: builder=%d outbox=%d", len(*got), w49OutboxCount(t, st, bookingID, topic))
	}

	// --- Unknown tx_ref (no attempt row at all) → ErrNotFound (warn+ack).
	if _, _, err := st.MarkBookingPaymentPaidByAttemptTx(ctx, tenantID, "tx-nope", 75.00, "NGN", builder); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown tx_ref err = %v, want ErrNotFound", err)
	}

	// --- Tenant isolation: the real tx_ref under a WRONG tenant is not found.
	if _, _, err := st.MarkBookingPaymentPaidByAttemptTx(ctx, uuid.New(), "tx-old", 75.00, "NGN", builder); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant attempt err = %v, want ErrNotFound", err)
	}

	// --- Mismatch on the fallback path: paid_amount_mismatch + alert, ref
	// advanced, attempt flagged; replay no-op.
	booking2 := w49SeedBooking(t, st, tenantID, offeringID, StatusPending)
	if err := st.InitializeBookingPaymentTx(ctx, tenantID, booking2, "tx-m1", ""); err != nil {
		t.Fatalf("init m1: %v", err)
	}
	if err := st.InitializeBookingPaymentTx(ctx, tenantID, booking2, "tx-m2", ""); err != nil {
		t.Fatalf("init m2: %v", err)
	}
	outcome, bid, err = st.MarkBookingPaymentPaidByAttemptTx(ctx, tenantID, "tx-m1", 70.00, "NGN", builder)
	if err != nil || outcome != PaymentMarkAmountMismatch || bid != booking2 {
		t.Fatalf("fallback mismatch = %q/%s/%v, want amount_mismatch", outcome, bid, err)
	}
	// SPEC-W51 contract delta: the mismatch transition info pins the expected
	// amount (offering price 75.00 NGN = 7500 minor) even though the capture
	// was 70.00.
	if m := (*got)[len(*got)-1]; m.ExpectedAmountMinor != 7500 {
		t.Fatalf("mismatch ExpectedAmountMinor = %d, want 7500", m.ExpectedAmountMinor)
	}
	status, ref, err = st.GetBookingPayment(ctx, tenantID, booking2)
	if err != nil || status != PaymentStatusPaidAmountMismatch || ref != "tx-m1" {
		t.Fatalf("mismatch state = %q/%q, %v; want paid_amount_mismatch/tx-m1", status, ref, err)
	}
	if s := w49AttemptStatus(t, st, tenantID, "tx-m1"); s != AttemptStatusAmountMismatch {
		t.Fatalf("mismatch attempt = %q, want amount_mismatch", s)
	}
	if n := w49OutboxCount(t, st, booking2, topic); n != 1 {
		t.Fatalf("mismatch outbox = %d, want 1 alert", n)
	}
	outcome, _, err = st.MarkBookingPaymentPaidByAttemptTx(ctx, tenantID, "tx-m1", 70.00, "NGN", builder)
	if err != nil || outcome != PaymentMarkAlreadyAmountMismatch {
		t.Fatalf("mismatch replay = %q/%v, want already_amount_mismatch/nil", outcome, err)
	}
	if n := w49OutboxCount(t, st, booking2, topic); n != 1 {
		t.Fatalf("mismatch replay outbox = %d, want still 1", n)
	}

	// --- Missing contact → empty strings, never an error (info still built).
	booking3 := w49SeedBooking(t, st, tenantID, offeringID, StatusPending) // contact_id NULL
	if err := st.InitializeBookingPaymentTx(ctx, tenantID, booking3, "tx-nc1", ""); err != nil {
		t.Fatalf("init nc1: %v", err)
	}
	if err := st.InitializeBookingPaymentTx(ctx, tenantID, booking3, "tx-nc2", ""); err != nil {
		t.Fatalf("init nc2: %v", err)
	}
	before := len(*got)
	outcome, _, err = st.MarkBookingPaymentPaidByAttemptTx(ctx, tenantID, "tx-nc1", 75.00, "NGN", builder)
	if err != nil || outcome != PaymentMarkPaid {
		t.Fatalf("no-contact fallback = %q/%v, want paid/nil", outcome, err)
	}
	info = (*got)[before]
	if info.CustomerName != "" || info.ContactPhone != "" || info.ContactEmail != "" {
		t.Fatalf("no-contact info = %+v, want empty contact strings", info)
	}
}
