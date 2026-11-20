package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// IntakeSubmission mirrors booking.intake_submissions (SPEC-W95 C5-B): the
// contact's completed pre-visit intake form. One row per (tenant, booking);
// a resubmission replaces the payload and moves submitted_at forward. The
// row is the durable half of the intake state — the workflow's queryable
// state is the ephemeral half (no booking-status change).
type IntakeSubmission struct {
	ID          uuid.UUID       `json:"id"`
	TenantID    uuid.UUID       `json:"tenant_id"`
	BookingID   uuid.UUID       `json:"booking_id"`
	Payload     json.RawMessage `json:"payload"`
	SubmittedAt time.Time       `json:"submitted_at"`
	CreatedAt   time.Time       `json:"created_at"`
}

const intakeSubmissionCols = `id, tenant_id, booking_id, payload, submitted_at, created_at`

func scanIntakeSubmission(row pgx.Row) (IntakeSubmission, error) {
	var sub IntakeSubmission
	err := row.Scan(&sub.ID, &sub.TenantID, &sub.BookingID, &sub.Payload,
		&sub.SubmittedAt, &sub.CreatedAt)
	return sub, err
}

// ensureIntakeSubmissionsTable bootstraps the intake_submissions table
// idempotently (verification_grants idiom) so upgrades need no manual
// migration. RLS mirrors the infra-managed tables.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureIntakeSubmissionsTable(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS intake_submissions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
    booking_id   UUID NOT NULL,
    payload      JSONB NOT NULL,
    submitted_at TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_intake_submissions_booking ON intake_submissions (tenant_id, booking_id);
ALTER TABLE intake_submissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE intake_submissions FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'intake_submissions' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON intake_submissions
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure intake_submissions table: %w", err)
	}
	return nil
}

// SubmitIntakeTx records (or replaces) the intake submission for a booking
// and appends the IntakeCompleted outbox event in the same transaction.
// resub is true when a row already existed (the ON CONFLICT path fired —
// detected via the tuple's xmax, the standard upsert-inserted probe).
func (s *Store) SubmitIntakeTx(ctx context.Context, tenantID, bookingID uuid.UUID, payload json.RawMessage, outboxTopic string, eventPayload []byte) (bool, error) {
	const q = `INSERT INTO intake_submissions (tenant_id, booking_id, payload, submitted_at)
	           VALUES ($1,$2,$3, now())
	           ON CONFLICT (tenant_id, booking_id) DO UPDATE
	           SET payload = EXCLUDED.payload,
	               submitted_at = EXCLUDED.submitted_at
	           RETURNING (xmax <> 0)`
	var resub bool
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, q, tenantID, bookingID, payload).Scan(&resub); err != nil {
			return err
		}
		return insertExtraOutbox(ctx, tx, bookingID, []ExtraOutbox{{Topic: outboxTopic, Payload: eventPayload}})
	})
	return resub, err
}

// GetIntakeSubmission returns the booking's intake submission or
// ErrNotFound when the contact has not submitted yet.
func (s *Store) GetIntakeSubmission(ctx context.Context, tenantID, bookingID uuid.UUID) (IntakeSubmission, error) {
	const q = `SELECT ` + intakeSubmissionCols + ` FROM intake_submissions
	           WHERE tenant_id=$1 AND booking_id=$2`
	var sub IntakeSubmission
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		sub, scanErr = scanIntakeSubmission(tx.QueryRow(ctx, q, tenantID, bookingID))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return IntakeSubmission{}, ErrNotFound
	}
	return sub, err
}
