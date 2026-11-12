package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Booking payment state (SPEC-W48 A3): the public pay leg records the
// Flutterwave checkout reference on the booking row.
//
// SPEC-W49 B1/B3 add the reconciliation half: MarkBookingPaymentPaidTx
// consumes the payments-service FlutterwavePaymentCaptured event (topic
// opendesk.payments.events, matched on payment_ref == tx_ref) and the
// booking_payment_attempts audit table records every initialized checkout
// (supersede chain + terminal paid/amount_mismatch).
//
// SPEC-W50 A1/A2: MarkBookingPaymentPaidByAttemptTx is the superseded-link
// fallback (capture references an OLD checkout ref no longer on the booking
// row — matched via booking_payment_attempts, same single-tx transition
// semantics), and the outbox payload is enriched to the cross-scope
// BookingPaymentConfirmed contract (customer/contact/booking_ref/paid_at,
// amount in MINOR units).
// ---------------------------------------------------------------------------

// Booking payment_status values.
const (
	// PaymentStatusNone / "": no checkout was ever initialized.
	PaymentStatusNone = ""
	// PaymentStatusInitialized: a Flutterwave checkout link was minted and
	// persisted; the webhook capture is still pending.
	PaymentStatusInitialized = "initialized"
	// PaymentStatusPaid: the payment completed (webhook capture). Bookings
	// in this state reject further pay-leg calls with 409.
	PaymentStatusPaid = "paid"
	// PaymentStatusPaidAmountMismatch (SPEC-W49 B1): the capture event
	// arrived but the captured amount/currency does not match the offering
	// price — the money IS captured at the rail, but an operator must
	// reconcile. Terminal for the consumer (acked, alert emitted); staff
	// resolve out-of-band.
	PaymentStatusPaidAmountMismatch = "paid_amount_mismatch"
)

// Booking payment_attempts.status values (SPEC-W49 B3).
const (
	AttemptStatusInitialized    = "initialized"
	AttemptStatusSuperseded     = "superseded"
	AttemptStatusPaid           = "paid"
	AttemptStatusAmountMismatch = "amount_mismatch"
)

// ensureBookingPaymentColumns adds the payment_ref / payment_status columns
// to the bookings table on existing databases (fresh installs get them from
// the same idempotent DDL — the base init script predates W48). Idempotent.
//
// NOTE (RLS): superuser bootstrap path, intentionally outside withTenant
// (same posture as ensureTeamMemberUserIDColumn).
func (s *Store) ensureBookingPaymentColumns(ctx context.Context) error {
	const ddl = `
DO $$
BEGIN
    IF to_regclass('public.bookings') IS NOT NULL THEN
        ALTER TABLE bookings ADD COLUMN IF NOT EXISTS payment_ref TEXT NOT NULL DEFAULT '';
        ALTER TABLE bookings ADD COLUMN IF NOT EXISTS payment_status TEXT NOT NULL DEFAULT '';
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure bookings payment columns: %w", err)
	}
	return nil
}

// ensureBookingPaymentAttemptsTable bootstraps the booking_payment_attempts
// audit table (SPEC-W49 B3) idempotently — same pattern as
// ensureBookingReviewsTable, with RLS mirroring the other tenant-scoped
// tables. tx_ref is UNIQUE: it is the rail-side checkout reference and the
// reconciliation join key (bookings.payment_ref == tx_ref).
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureBookingPaymentAttemptsTable(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS booking_payment_attempts (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL,
    booking_id UUID NOT NULL,
    tx_ref     TEXT NOT NULL UNIQUE,
    link       TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL CHECK (status IN ('initialized','superseded','paid','amount_mismatch')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_booking_payment_attempts_booking ON booking_payment_attempts (booking_id);
ALTER TABLE booking_payment_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE booking_payment_attempts FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'booking_payment_attempts' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON booking_payment_attempts
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure booking_payment_attempts table: %w", err)
	}
	return nil
}

// BookingPaymentAttempt is one row of the booking_payment_attempts audit
// table (SPEC-W49 B3).
type BookingPaymentAttempt struct {
	ID        uuid.UUID `json:"id"`
	TenantID  uuid.UUID `json:"tenant_id"`
	BookingID uuid.UUID `json:"booking_id"`
	TxRef     string    `json:"tx_ref"`
	Link      string    `json:"link"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListBookingPaymentAttempts returns the attempt audit chain of a booking,
// oldest first (audit/reconciliation read; the pay-leg fast path stays on
// bookings.payment_ref/payment_status).
func (s *Store) ListBookingPaymentAttempts(ctx context.Context, tenantID, bookingID uuid.UUID) ([]BookingPaymentAttempt, error) {
	var out []BookingPaymentAttempt
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, booking_id, tx_ref, link, status, created_at, updated_at
			 FROM booking_payment_attempts WHERE tenant_id=$1 AND booking_id=$2 ORDER BY created_at, id`,
			tenantID, bookingID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a BookingPaymentAttempt
			if err := rows.Scan(&a.ID, &a.TenantID, &a.BookingID, &a.TxRef, &a.Link, &a.Status, &a.CreatedAt, &a.UpdatedAt); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// InitializeBookingPaymentTx atomically records a freshly initialized
// checkout on the booking (SPEC-W48 A3): locks the row FOR UPDATE, rejects
// already-paid bookings with ErrConflict (409 — double-pay guard), then
// stamps payment_ref + payment_status='initialized'. Re-initialization over
// an earlier "initialized" ref is allowed: the payments side is idempotent
// on the caller-supplied key, and an abandoned checkout must be retryable.
//
// SPEC-W49 B3 (same transaction): every prior 'initialized' attempt row of
// the booking is marked 'superseded' (except the current tx_ref, so an
// idempotent rail replay re-initializing the SAME reference stays current),
// and the new attempt is appended — ON CONFLICT (tx_ref) DO NOTHING keeps a
// same-ref replay idempotent.
//
// Errors: ErrNotFound (unknown booking — or cross-tenant, the handler
// resolves the booking under the site tenant first), ErrConflict (already
// paid).
func (s *Store) InitializeBookingPaymentTx(ctx context.Context, tenantID, bookingID uuid.UUID, paymentRef, link string) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx,
			`SELECT payment_status FROM bookings WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
			tenantID, bookingID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status == PaymentStatusPaid {
			return fmt.Errorf("%w: booking already paid", ErrConflict)
		}
		// W49 B3: supersede the previous open attempts, then append the new
		// one — all in the same tx as the fast-path state flip.
		if _, err := tx.Exec(ctx,
			`UPDATE booking_payment_attempts SET status=$3, updated_at=now()
			 WHERE tenant_id=$1 AND booking_id=$2 AND status='initialized' AND tx_ref <> $4`,
			tenantID, bookingID, AttemptStatusSuperseded, paymentRef); err != nil {
			return fmt.Errorf("supersede prior payment attempts: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO booking_payment_attempts (tenant_id, booking_id, tx_ref, link, status)
			 VALUES ($1,$2,$3,$4,'initialized')
			 ON CONFLICT (tx_ref) DO NOTHING`,
			tenantID, bookingID, paymentRef, link); err != nil {
			return fmt.Errorf("insert payment attempt: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE bookings SET payment_ref=$3, payment_status=$4, updated_at=now()
			 WHERE tenant_id=$1 AND id=$2`,
			tenantID, bookingID, paymentRef, PaymentStatusInitialized); err != nil {
			return fmt.Errorf("mark booking payment initialized: %w", err)
		}
		return nil
	})
}

// GetBookingPayment returns the (payment_status, payment_ref) pair of a
// booking — the pay leg's pre-flight read (InitializeBookingPaymentTx
// re-checks under lock; this is only for honest 409s before calling the
// rail).
func (s *Store) GetBookingPayment(ctx context.Context, tenantID, bookingID uuid.UUID) (status, ref string, err error) {
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT payment_status, payment_ref FROM bookings WHERE tenant_id=$1 AND id=$2`,
			tenantID, bookingID).Scan(&status, &ref)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return status, ref, err
}

// ---------------------------------------------------------------------------
// Payment-capture reconciliation (SPEC-W49 B1)
// ---------------------------------------------------------------------------

// PaymentMarkOutcome identifies what MarkBookingPaymentPaidTx did.
type PaymentMarkOutcome string

const (
	// PaymentMarkPaid: payment_status flipped initialized → paid, attempt
	// marked paid, BookingPaymentConfirmed outbox row written.
	PaymentMarkPaid PaymentMarkOutcome = "paid"
	// PaymentMarkAlreadyPaid: replay — the booking was already paid; pure
	// no-op (no second outbox row), the consumer acks.
	PaymentMarkAlreadyPaid PaymentMarkOutcome = "already_paid"
	// PaymentMarkAmountMismatch: the captured amount/currency does not match
	// the offering price — payment_status='paid_amount_mismatch', attempt
	// marked amount_mismatch, alert outbox row written. Still acked.
	PaymentMarkAmountMismatch PaymentMarkOutcome = "amount_mismatch"
	// PaymentMarkAlreadyAmountMismatch: replay of a mismatch — no-op (the
	// alert was emitted by the first delivery).
	PaymentMarkAlreadyAmountMismatch PaymentMarkOutcome = "already_amount_mismatch"
)

// PaymentTransitionInfo carries the booking-row context the outbox builder
// needs for the enriched BookingPaymentConfirmed /
// BookingPaymentAmountMismatch payload (SPEC-W50 A2, cross-scope sacred
// contract). The customer/contact fields are read from the booking's
// contact INSIDE the same transaction that holds the booking row lock;
// missing contact fields are EMPTY STRINGS, never nil. PaidAt is the time
// of the transition (UTC). BookingRef is the human booking reference —
// bookings carry no separate human ref column, so it is the booking UUID
// string (notification-worker falls back to booking_id identically).
type PaymentTransitionInfo struct {
	BookingID    uuid.UUID
	BookingRef   string
	CustomerName string
	ContactPhone string // E.164, "" when unknown
	ContactEmail string // "" when unknown
	PaidAt       time.Time
}

// PaymentOutboxBuilder builds the outbox row (topic + CloudEvent payload)
// for a paid/amount_mismatch transition. It runs INSIDE the transaction
// (after the row lock) so the event commits atomically with the state flip;
// returning ("",
// nil, nil) skips the outbox write. Not called for the idempotent
// replay outcomes.
type PaymentOutboxBuilder func(outcome PaymentMarkOutcome, info PaymentTransitionInfo) (topic string, payload []byte, err error)

// MarkBookingPaymentPaidTx reconciles one FlutterwavePaymentCaptured event
// (SPEC-W49 B1): finds the booking by payment_ref == txRef within the
// tenant (row locked FOR UPDATE), then:
//   - already paid / already flagged mismatch → idempotent no-op;
//   - captured amount (major units) or currency differs from the offering
//     price → payment_status='paid_amount_mismatch' + attempt marked
//     amount_mismatch + alert outbox row (via buildOutbox);
//   - otherwise → payment_status='paid' + attempt marked paid +
//     BookingPaymentConfirmed outbox row (via buildOutbox).
//
// The booking STATUS is deliberately untouched — payment_status only.
// ErrNotFound means "unknown tx_ref within this tenant": the consumer logs
// a structured warning and ACKS (never retry-loops a poison event).
func (s *Store) MarkBookingPaymentPaidTx(ctx context.Context, tenantID uuid.UUID, txRef string, amountMajor float64, currency string, buildOutbox PaymentOutboxBuilder) (PaymentMarkOutcome, uuid.UUID, error) {
	var outcome PaymentMarkOutcome
	var bookingID uuid.UUID
	outboxWritten := false
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		var offeringID uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT id, payment_status, offering_id FROM bookings
			 WHERE tenant_id=$1 AND payment_ref=$2 FOR UPDATE`,
			tenantID, txRef).Scan(&bookingID, &status, &offeringID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch status {
		case PaymentStatusPaid:
			outcome = PaymentMarkAlreadyPaid
			return nil
		case PaymentStatusPaidAmountMismatch:
			outcome = PaymentMarkAlreadyAmountMismatch
			return nil
		}
		var werr error
		outcome, outboxWritten, werr = s.paymentTransitionLocked(ctx, tx, tenantID, bookingID, offeringID, txRef, false, amountMajor, currency, buildOutbox)
		return werr
	})
	if err != nil {
		return "", uuid.Nil, err
	}
	if outboxWritten {
		s.signalOutboxFlush() // W46-A item 6: flush-on-commit
	}
	return outcome, bookingID, nil
}

// MarkBookingPaymentPaidByAttemptTx is the SPEC-W50 A1 superseded-link
// fallback: the capture's tx_ref no longer sits on bookings.payment_ref
// (the customer re-initialized the checkout and a NEW link superseded the
// one they actually paid through), so reconciliation joins through the
// booking_payment_attempts audit table instead. Same single-transaction
// semantics as the fast path:
//   - attempt looked up by tx_ref STRICTLY within the tenant (RLS +
//     predicate) and locked FOR UPDATE; unknown → ErrNotFound (the consumer
//     falls through to its warn+ack poison posture);
//   - attempt already paid / amount_mismatch → idempotent no-op outcome
//     (exactly-once under replay: the outbox row was committed by the
//     first delivery);
//   - booking already in a terminal payment state (paid / mismatch via
//     another ref — a second genuine capture on a stale link) → same
//     idempotent no-op outcomes as the fast path: exactly one confirmation
//     per booking;
//   - otherwise the SAME transition as the fast path runs against the
//     attempt's booking (amount check vs the offering price in minor
//     units, mismatch path, enriched outbox payload) AND
//     bookings.payment_ref is advanced to this tx_ref — the paid
//     reference becomes the booking's current payment ref.
func (s *Store) MarkBookingPaymentPaidByAttemptTx(ctx context.Context, tenantID uuid.UUID, txRef string, amountMajor float64, currency string, buildOutbox PaymentOutboxBuilder) (PaymentMarkOutcome, uuid.UUID, error) {
	var outcome PaymentMarkOutcome
	var bookingID uuid.UUID
	outboxWritten := false
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var attemptStatus string
		err := tx.QueryRow(ctx,
			`SELECT booking_id, status FROM booking_payment_attempts
			 WHERE tenant_id=$1 AND tx_ref=$2 FOR UPDATE`,
			tenantID, txRef).Scan(&bookingID, &attemptStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch attemptStatus {
		case AttemptStatusPaid:
			outcome = PaymentMarkAlreadyPaid
			return nil
		case AttemptStatusAmountMismatch:
			outcome = PaymentMarkAlreadyAmountMismatch
			return nil
		}
		// attempt initialized|superseded: lock the booking row and run the
		// transition. A cross-tenant booking cannot be reached (the attempt
		// itself was tenant-scoped); a missing booking is poison → not found.
		var status string
		var offeringID uuid.UUID
		err = tx.QueryRow(ctx,
			`SELECT payment_status, offering_id FROM bookings
			 WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
			tenantID, bookingID).Scan(&status, &offeringID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// SAME transition semantics as the fast path: a booking already in
		// a terminal payment state (paid via ANOTHER ref — e.g. the newer
		// link's capture landed first, making this a second genuine
		// capture) is an idempotent no-op here. Exactly one confirmation
		// per booking; the duplicate capture is acked (the money trail is
		// already durable rail-side, ops reconcile out-of-band — same
		// posture as the mismatch path).
		switch status {
		case PaymentStatusPaid:
			outcome = PaymentMarkAlreadyPaid
			return nil
		case PaymentStatusPaidAmountMismatch:
			outcome = PaymentMarkAlreadyAmountMismatch
			return nil
		}
		var werr error
		outcome, outboxWritten, werr = s.paymentTransitionLocked(ctx, tx, tenantID, bookingID, offeringID, txRef, true, amountMajor, currency, buildOutbox)
		return werr
	})
	if err != nil {
		return "", uuid.Nil, err
	}
	if outboxWritten {
		s.signalOutboxFlush() // W46-A item 6: flush-on-commit
	}
	return outcome, bookingID, nil
}

// paymentTransitionLocked performs the paid / amount_mismatch transition
// shared by the payment_ref fast path and the W50-A attempt fallback. The
// caller MUST hold the booking row lock (FOR UPDATE) in tx. advanceRef=true
// (fallback) additionally moves bookings.payment_ref to the capture's
// tx_ref; the fast path leaves it (it already equals txRef).
func (s *Store) paymentTransitionLocked(ctx context.Context, tx pgx.Tx, tenantID, bookingID, offeringID uuid.UUID, txRef string, advanceRef bool, amountMajor float64, currency string, buildOutbox PaymentOutboxBuilder) (PaymentMarkOutcome, bool, error) {
	// Amount check: the event carries MAJOR units (contract), the
	// offering price is minor units (cents). Compare in minor units;
	// a currency disagreement cannot be compared → mismatch.
	var priceCents int64
	var offeringCurrency string
	err := tx.QueryRow(ctx,
		`SELECT price_cents, currency FROM offerings WHERE tenant_id=$1 AND id=$2`,
		tenantID, offeringID).Scan(&priceCents, &offeringCurrency)
	if err != nil {
		return "", false, fmt.Errorf("load offering for amount check: %w", err)
	}
	capturedMinor := int64(math.Round(amountMajor * 100))
	mismatch := capturedMinor != priceCents ||
		!strings.EqualFold(strings.TrimSpace(offeringCurrency), strings.TrimSpace(currency))

	newStatus := PaymentStatusPaid
	attemptStatus := AttemptStatusPaid
	outcome := PaymentMarkPaid
	if mismatch {
		newStatus = PaymentStatusPaidAmountMismatch
		attemptStatus = AttemptStatusAmountMismatch
		outcome = PaymentMarkAmountMismatch
	}

	// SPEC-W50 A2: enriched payload context — customer/contact fields from
	// the booking's contact row (same tx, booking row already locked);
	// absent contact → empty strings, never NULL.
	info := PaymentTransitionInfo{
		BookingID:  bookingID,
		BookingRef: bookingID.String(),
		PaidAt:     time.Now().UTC(),
	}
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(c.name,''), COALESCE(c.phone,''), COALESCE(c.email,'')
		 FROM bookings b LEFT JOIN contacts c
		   ON c.tenant_id=b.tenant_id AND c.id=b.contact_id
		 WHERE b.tenant_id=$1 AND b.id=$2`,
		tenantID, bookingID).Scan(&info.CustomerName, &info.ContactPhone, &info.ContactEmail)
	if err != nil {
		return "", false, fmt.Errorf("load booking contact for confirmation payload: %w", err)
	}

	if advanceRef {
		if _, err := tx.Exec(ctx,
			`UPDATE bookings SET payment_ref=$3, payment_status=$4, updated_at=now()
			 WHERE tenant_id=$1 AND id=$2`,
			tenantID, bookingID, txRef, newStatus); err != nil {
			return "", false, fmt.Errorf("mark booking payment %s: %w", newStatus, err)
		}
	} else {
		if _, err := tx.Exec(ctx,
			`UPDATE bookings SET payment_status=$3, updated_at=now()
			 WHERE tenant_id=$1 AND id=$2`,
			tenantID, bookingID, newStatus); err != nil {
			return "", false, fmt.Errorf("mark booking payment %s: %w", newStatus, err)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE booking_payment_attempts SET status=$3, updated_at=now()
		 WHERE tenant_id=$1 AND tx_ref=$2`,
		tenantID, txRef, attemptStatus); err != nil {
		return "", false, fmt.Errorf("mark payment attempt %s: %w", attemptStatus, err)
	}
	outboxWritten := false
	if buildOutbox != nil {
		topic, payload, berr := buildOutbox(outcome, info)
		if berr != nil {
			return "", false, fmt.Errorf("build %s outbox event: %w", outcome, berr)
		}
		if topic != "" && payload != nil {
			if _, err := tx.Exec(ctx,
				`INSERT INTO outbox (aggregate_id, topic, payload) VALUES ($1,$2,$3)`,
				bookingID, topic, payload); err != nil {
				return "", false, fmt.Errorf("insert %s outbox: %w", outcome, err)
			}
			outboxWritten = true
		}
	}
	return outcome, outboxWritten, nil
}
