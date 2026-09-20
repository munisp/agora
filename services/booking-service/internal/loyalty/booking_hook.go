package loyalty

// Booking-completion accrual hook (SPEC-W45 K11, work order H-7): the
// booking core (CODER-A, bookingops.Complete) detects this method on Deps
// by interface assertion (bookingops.BookingCompletedAccruer) and calls it
// after a booking transitions confirmed|checked_in → completed — only on
// the NON-replay path. Deps is asserted (not Handlers/Service) so the
// integrator wiring in cmd/server/main.go needs no further change.
//
// SIGNATURE NOTE: the interface compiled into bookingops
// (complete.go, CODER-A) is AccrueOnBookingCompleted(tenantID, bookingID)
// with NO error return — the hook is best-effort, so failures are logged
// here, never propagated (a loyalty outage must not fail booking
// completion).
//
// Behavior: resolve bookings.contact_id (a booking without a contact has
// no wallet to accrue to → skip), then Service.Accrue with event
// booking_completed and ref_id = booking id. Idempotency rides the ledger
// anchor ref_id "booking_completed:<booking_id>" (UNIQUE
// (tenant_id, ref_type, ref_id, account_code) on loyalty_ledger), so a
// retried complete can never double-award. ErrNoActiveProgram (tenant has
// no active program) is a nil outcome, not an error.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"
)

// accrualHookTimeout bounds the hook's DB work — it runs synchronously on
// the booking-complete path and must never hang the HTTP handler.
const accrualHookTimeout = 10 * time.Second

// AccrueOnBookingCompleted satisfies bookingops.BookingCompletedAccruer
// (SPEC-W45 K11). Best-effort: every failure is logged and swallowed.
func (d *Deps) AccrueOnBookingCompleted(tenantID, bookingID uuid.UUID) {
	log := d.Log
	if log == nil {
		log = zap.NewNop()
	}
	if d.Store == nil || tenantID == uuid.Nil || bookingID == uuid.Nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), accrualHookTimeout)
	defer cancel()

	contactID, err := d.Store.bookingContactID(ctx, tenantID, bookingID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Booking missing (or no contact): nothing to accrue — not an
			// error on the completion path.
			return
		}
		log.Error("loyalty hook: booking contact lookup failed",
			zap.String("tenant_id", tenantID.String()),
			zap.String("booking_id", bookingID.String()), zap.Error(err))
		return
	}
	svc := &Service{
		Store:       d.Store,
		Ledger:      NewPostgresLedger(d.Store),
		EventsTopic: d.EventsTopic,
		UsageTopic:  d.UsageTopic,
		Log:         d.Log,
	}
	// ref_id = booking id → the ledger anchor (event:ref_id) makes the
	// accrual idempotent PER BOOKING.
	if _, err := svc.Accrue(ctx, tenantID, contactID, EventBookingCompleted, bookingID.String()); err != nil {
		if errors.Is(err, ErrNoActiveProgram) {
			return // tenant has no active program — accrual simply does not apply
		}
		if errors.Is(err, ErrInvalidInput) {
			// The active program does not award booking_completed — a config
			// outcome, not a hook failure.
			log.Debug("loyalty hook: active program does not award booking_completed",
				zap.String("tenant_id", tenantID.String()))
			return
		}
		log.Error("loyalty hook: accrual failed (booking completed unaffected)",
			zap.String("tenant_id", tenantID.String()),
			zap.String("booking_id", bookingID.String()),
			zap.String("contact_id", contactID.String()), zap.Error(err))
	}
}

// bookingContactID resolves bookings.contact_id inside the tenant's RLS
// transaction. Bookings without a contact (contact_id nullable) and
// missing bookings both surface as ErrNotFound → the hook skips.
func (s *Store) bookingContactID(ctx context.Context, tenantID, bookingID uuid.UUID) (uuid.UUID, error) {
	var contactID uuid.UUID
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT contact_id FROM bookings
			  WHERE tenant_id=$1 AND id=$2 AND contact_id IS NOT NULL`,
			tenantID, bookingID).Scan(&contactID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return contactID, err
}
