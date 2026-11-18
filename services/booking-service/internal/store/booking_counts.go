package store

// SPEC-W80 (B2 first_txn / B3 booking→lead conversion): completed-booking
// counts per contact. Lives in its own file because bookings.go is a
// BYTE-IDENTICAL invariant (W57) — no new methods may land there.

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CountCompletedBookingsForContact returns how many of the contact's
// bookings currently sit in status `completed`, tenant-scoped under RLS.
// Callers that run it right after a completion flip see the just-completed
// booking included — a result of exactly 1 means "first completion".
func (s *Store) CountCompletedBookingsForContact(ctx context.Context, tenantID, contactID uuid.UUID) (int, error) {
	var count int
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM bookings
			  WHERE tenant_id=$1 AND contact_id=$2 AND status=$3`,
			tenantID, contactID, StatusCompleted).Scan(&count)
	})
	return count, err
}
