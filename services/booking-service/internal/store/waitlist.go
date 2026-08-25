package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/opendesk/booking-service/internal/availability"
)

// Waitlist status values (SPEC-W3 §3 innovation 7).
const (
	WaitlistWaiting = "waiting"
	WaitlistClaimed = "claimed"
)

// WaitlistEntry mirrors booking.waitlist: a contact who wants a booking in
// [WindowStart, WindowEnd] for one offering. ClaimToken is the capability
// secret emailed/SMSed on backfill — possessing it authorizes the claim.
type WaitlistEntry struct {
	ID           uuid.UUID `json:"id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	OfferingID   uuid.UUID `json:"offering_id"`
	ContactName  string    `json:"contact_name"`
	ContactPhone string    `json:"contact_phone"`
	WindowStart  time.Time `json:"window_start"`
	WindowEnd    time.Time `json:"window_end"`
	Status       string    `json:"status"`
	ClaimToken   uuid.UUID `json:"claim_token"`
	CreatedAt    time.Time `json:"created_at"`
}

const waitlistCols = `id, tenant_id, offering_id, contact_name, contact_phone, window_start, window_end, status, claim_token, created_at`

func scanWaitlist(row pgx.Row) (WaitlistEntry, error) {
	var w WaitlistEntry
	err := row.Scan(&w.ID, &w.TenantID, &w.OfferingID, &w.ContactName, &w.ContactPhone,
		&w.WindowStart, &w.WindowEnd, &w.Status, &w.ClaimToken, &w.CreatedAt)
	return w, err
}

// ensureWaitlistTable bootstraps the waitlist table idempotently (like
// ensureSitesTable) so upgrades need no manual migration. RLS mirrors the
// infra-managed tables: enabled + forced with the tenant_isolation policy,
// guarded by a pg_policies existence check (CREATE POLICY has no
// IF NOT EXISTS).
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureWaitlistTable(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS waitlist (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL,
    offering_id   UUID NOT NULL,
    contact_name  TEXT NOT NULL DEFAULT '',
    contact_phone TEXT NOT NULL DEFAULT '',
    window_start  TIMESTAMPTZ NOT NULL,
    window_end    TIMESTAMPTZ NOT NULL,
    status        TEXT NOT NULL DEFAULT 'waiting'
                  CHECK (status IN ('waiting','claimed','expired')),
    claim_token   UUID NOT NULL DEFAULT gen_random_uuid(),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (window_end > window_start)
);
CREATE INDEX IF NOT EXISTS idx_waitlist_tenant_offering_status ON waitlist (tenant_id, offering_id, status);
ALTER TABLE waitlist ENABLE ROW LEVEL SECURITY;
ALTER TABLE waitlist FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'waitlist' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON waitlist
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure waitlist table: %w", err)
	}
	return nil
}

// CreateWaitlistEntry inserts a waitlist entry, generating id + claim_token.
func (s *Store) CreateWaitlistEntry(ctx context.Context, w *WaitlistEntry) error {
	if w.ID == uuid.Nil {
		w.ID = uuid.New()
	}
	if w.ClaimToken == uuid.Nil {
		w.ClaimToken = uuid.New()
	}
	const q = `INSERT INTO waitlist (` + waitlistCols + `)
	           VALUES ($1,$2,$3,$4,$5,$6,$7,'waiting',$8,now()) RETURNING created_at`
	return s.withTenant(ctx, w.TenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, q, w.ID, w.TenantID, w.OfferingID, w.ContactName, w.ContactPhone,
			w.WindowStart, w.WindowEnd, w.ClaimToken).Scan(&w.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert waitlist entry: %w", err)
		}
		w.Status = WaitlistWaiting
		return nil
	})
}

// WaitlistFilter narrows ListWaitlist.
type WaitlistFilter struct {
	OfferingID *uuid.UUID
	Status     string
	Limit      int
}

// ListWaitlist returns entries oldest-first (FIFO backfill order).
func (s *Store) ListWaitlist(ctx context.Context, tenantID uuid.UUID, f WaitlistFilter) ([]WaitlistEntry, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q := `SELECT ` + waitlistCols + ` FROM waitlist WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if f.OfferingID != nil {
		n++
		q += fmt.Sprintf(` AND offering_id=$%d`, n)
		args = append(args, *f.OfferingID)
	}
	if f.Status != "" {
		n++
		q += fmt.Sprintf(` AND status=$%d`, n)
		args = append(args, f.Status)
	}
	n++
	q += fmt.Sprintf(` ORDER BY created_at LIMIT $%d`, n)
	args = append(args, f.Limit)

	var out []WaitlistEntry
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			w, err := scanWaitlist(rows)
			if err != nil {
				return err
			}
			out = append(out, w)
		}
		return rows.Err()
	})
	return out, err
}

// GetWaitlistEntry fetches one entry scoped to a tenant.
func (s *Store) GetWaitlistEntry(ctx context.Context, tenantID, id uuid.UUID) (WaitlistEntry, error) {
	const q = `SELECT ` + waitlistCols + ` FROM waitlist WHERE tenant_id=$1 AND id=$2`
	var w WaitlistEntry
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		w, err = scanWaitlist(tx.QueryRow(ctx, q, tenantID, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return w, ErrNotFound
	}
	return w, err
}

// ClaimWaitlistTx atomically claims a waitlist entry and books the slot
// (SPEC-W3 §3 innovation 7). Inside ONE transaction it:
//
//  1. locks the entry FOR UPDATE (this is the claim race mutex: the loser
//     blocks until the winner commits, then sees status='claimed');
//  2. verifies status, claim token and that the requested start lies inside
//     the entry's window;
//  3. re-checks the slot with the same availability engine the booking
//     write path uses (weekly rules + overlapping bookings + buffers);
//  4. inserts the booking + its outbox event and marks the entry claimed.
//
// Any check failure rolls the whole thing back — a failed claim never
// leaves a booking or a claimed entry behind. Errors: ErrNotFound (unknown
// entry), ErrConflict (already claimed, bad/expired token or window, slot
// no longer free — all mapped to HTTP 409 by the handler).
func (s *Store) ClaimWaitlistTx(ctx context.Context, tenantID, entryID uuid.UUID, token uuid.UUID,
	offering Offering, booking *Booking, loc *time.Location, outboxTopic string, eventPayload []byte) (WaitlistEntry, error) {

	if booking.ID == uuid.Nil {
		booking.ID = uuid.New()
	}
	var entry WaitlistEntry
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		entry, err = scanWaitlist(tx.QueryRow(ctx,
			`SELECT `+waitlistCols+` FROM waitlist WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
			tenantID, entryID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if entry.Status != WaitlistWaiting {
			return fmt.Errorf("%w: entry already %s", ErrConflict, entry.Status)
		}
		if entry.ClaimToken != token {
			return fmt.Errorf("%w: invalid claim token", ErrConflict)
		}
		if booking.StartsAt.Before(entry.WindowStart) || booking.StartsAt.After(entry.WindowEnd) {
			return fmt.Errorf("%w: requested start outside the waitlist window", ErrConflict)
		}
		if time.Now().After(entry.WindowEnd) {
			return fmt.Errorf("%w: waitlist window has passed", ErrConflict)
		}

		// Slot re-check with the availability engine (weekly rules first).
		rows, err := tx.Query(ctx,
			`SELECT weekday, start_min, end_min, effective_from, effective_to
			 FROM availability_rules WHERE tenant_id=$1 AND team_member_id=$2`,
			tenantID, booking.TeamMemberID)
		if err != nil {
			return err
		}
		var rules []availability.Rule
		for rows.Next() {
			var r AvailabilityRule
			if err := rows.Scan(&r.Weekday, &r.StartMin, &r.EndMin, &r.EffectiveFrom, &r.EffectiveTo); err != nil {
				rows.Close()
				return err
			}
			rules = append(rules, availability.Rule{
				Weekday:       time.Weekday(r.Weekday),
				StartMin:      r.StartMin,
				EndMin:        r.EndMin,
				EffectiveFrom: r.EffectiveFrom,
				EffectiveTo:   r.EffectiveTo,
			})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if loc == nil {
			loc = time.UTC
		}
		if !availability.Covers(rules, loc, booking.StartsAt, booking.EndsAt) {
			return fmt.Errorf("%w: slot no longer available", ErrConflict)
		}

		from := booking.StartsAt.Add(-time.Duration(offering.BufferMin+offering.DurationMin) * time.Minute)
		to := booking.EndsAt.Add(time.Duration(offering.BufferMin+offering.DurationMin) * time.Minute)
		brows, err := tx.Query(ctx,
			`SELECT starts_at, ends_at FROM bookings
			 WHERE tenant_id=$1 AND team_member_id=$2 AND status NOT IN ('cancelled')
			   AND starts_at < $4 AND ends_at > $3`,
			tenantID, booking.TeamMemberID, from, to)
		if err != nil {
			return err
		}
		var existing []availability.Booking
		for brows.Next() {
			var b availability.Booking
			if err := brows.Scan(&b.StartsAt, &b.EndsAt); err != nil {
				brows.Close()
				return err
			}
			existing = append(existing, b)
		}
		brows.Close()
		if err := brows.Err(); err != nil {
			return err
		}
		if !availability.Fits(booking.StartsAt, booking.EndsAt,
			time.Duration(offering.BufferMin)*time.Minute, offering.Capacity, existing) {
			return fmt.Errorf("%w: slot no longer available", ErrConflict)
		}

		const ins = `INSERT INTO bookings (` + bookingCols + `)
		             VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),now(),now())
		             RETURNING created_at, updated_at`
		err = tx.QueryRow(ctx, ins, booking.ID, booking.TenantID, booking.OfferingID, booking.TeamMemberID,
			booking.ContactID, booking.StartsAt, booking.EndsAt, booking.Status, booking.Source,
			booking.IdempotencyKey).Scan(&booking.CreatedAt, &booking.UpdatedAt)
		if err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return fmt.Errorf("insert booking: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO outbox (aggregate_id, topic, payload) VALUES ($1,$2,$3)`,
			booking.ID, outboxTopic, eventPayload); err != nil {
			return fmt.Errorf("insert outbox: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE waitlist SET status=$3 WHERE tenant_id=$1 AND id=$2`,
			tenantID, entryID, WaitlistClaimed); err != nil {
			return fmt.Errorf("mark waitlist claimed: %w", err)
		}
		entry.Status = WaitlistClaimed
		return nil
	})
	return entry, err
}
