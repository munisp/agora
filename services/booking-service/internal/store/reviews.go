package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Booking reviews (SPEC-W48 A4): one public review per completed booking,
// submitted via POST /public/sites/{slug}/bookings/{id}/review and listed
// via GET /public/sites/{slug}/reviews.
// ---------------------------------------------------------------------------

// BookingReview mirrors booking.booking_reviews. One row per booking
// (UNIQUE(booking_id)); reviewer_name is the public display name.
type BookingReview struct {
	ID           uuid.UUID `json:"id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	BookingID    uuid.UUID `json:"booking_id"`
	Rating       int       `json:"rating"`
	Comment      string    `json:"comment"`
	ReviewerName string    `json:"reviewer_name"`
	CreatedAt    time.Time `json:"created_at"`
}

const reviewCols = `id, tenant_id, booking_id, rating, comment, reviewer_name, created_at`

func scanReview(row pgx.Row) (BookingReview, error) {
	var r BookingReview
	err := row.Scan(&r.ID, &r.TenantID, &r.BookingID, &r.Rating, &r.Comment, &r.ReviewerName, &r.CreatedAt)
	return r, err
}

// ensureBookingReviewsTable bootstraps the booking_reviews table
// idempotently (same pattern as ensureWaitlistTable), with RLS mirroring
// the other tenant-scoped tables.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureBookingReviewsTable(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS booking_reviews (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL,
    booking_id    UUID NOT NULL,
    rating        SMALLINT NOT NULL CHECK (rating BETWEEN 1 AND 5),
    comment       TEXT NOT NULL DEFAULT '',
    reviewer_name TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (booking_id)
);
CREATE INDEX IF NOT EXISTS idx_booking_reviews_tenant_created ON booking_reviews (tenant_id, created_at DESC);
ALTER TABLE booking_reviews ENABLE ROW LEVEL SECURITY;
ALTER TABLE booking_reviews FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'booking_reviews' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON booking_reviews
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure booking_reviews table: %w", err)
	}
	return nil
}

// CreateBookingReview inserts one review. The UNIQUE(booking_id) constraint
// enforces one review per booking; a duplicate maps to ErrConflict (HTTP
// 409 at the handler).
func (s *Store) CreateBookingReview(ctx context.Context, r *BookingReview) error {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	const q = `INSERT INTO booking_reviews (` + reviewCols + `)
	           VALUES ($1,$2,$3,$4,$5,$6,now()) RETURNING created_at`
	return s.withTenant(ctx, r.TenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, q, r.ID, r.TenantID, r.BookingID, r.Rating, r.Comment, r.ReviewerName).Scan(&r.CreatedAt)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: booking already has a review", ErrConflict)
		}
		if err != nil {
			return fmt.Errorf("insert booking review: %w", err)
		}
		return nil
	})
}

// ListBookingReviews returns one page of reviews (newest first) of the
// tenant's COMPLETED bookings, plus the aggregate over the full filtered
// set (average rating + total count) for the public reviews endpoint
// (SPEC-W48 A4). limit is capped at 50 (<=0 defaults to 20); offset < 0
// clamps to 0.
func (s *Store) ListBookingReviews(ctx context.Context, tenantID uuid.UUID, limit, offset int) (reviews []BookingReview, average float64, count int, err error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT coalesce(avg(r.rating), 0)::float8, count(*)::int
			   FROM booking_reviews r
			   JOIN bookings b ON b.tenant_id = r.tenant_id AND b.id = r.booking_id
			  WHERE r.tenant_id = $1 AND b.status = 'completed'`, tenantID).
			Scan(&average, &count); err != nil {
			return err
		}
		rows, err := tx.Query(ctx,
			`SELECT r.id, r.tenant_id, r.booking_id, r.rating, r.comment, r.reviewer_name, r.created_at
			   FROM booking_reviews r
			   JOIN bookings b ON b.tenant_id = r.tenant_id AND b.id = r.booking_id
			  WHERE r.tenant_id = $1 AND b.status = 'completed'
			  ORDER BY r.created_at DESC LIMIT $2 OFFSET $3`, tenantID, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			rv, err := scanReview(rows)
			if err != nil {
				return err
			}
			reviews = append(reviews, rv)
		}
		return rows.Err()
	})
	return reviews, average, count, err
}
