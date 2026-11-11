package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Booking payment state (SPEC-W48 A3): the public pay leg records the
// Flutterwave checkout reference on the booking row.
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

// InitializeBookingPaymentTx atomically records a freshly initialized
// checkout on the booking (SPEC-W48 A3): locks the row FOR UPDATE, rejects
// already-paid bookings with ErrConflict (409 — double-pay guard), then
// stamps payment_ref + payment_status='initialized'. Re-initialization over
// an earlier "initialized" ref is allowed: the payments side is idempotent
// on the caller-supplied key, and an abandoned checkout must be retryable.
//
// Errors: ErrNotFound (unknown booking — or cross-tenant, the handler
// resolves the booking under the site tenant first), ErrConflict (already
// paid).
func (s *Store) InitializeBookingPaymentTx(ctx context.Context, tenantID, bookingID uuid.UUID, paymentRef string) error {
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
