package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// VerificationGrant mirrors booking.verification_grants (SPEC-W87): a
// time-boxed record that a contact completed a genuine portal OTP
// verification. Future voice sessions on the PSTN channel (SIP
// carrier-asserted caller id) honor an active grant instead of
// re-prompting the OTP; the grant is strictly stronger than the removed
// SIP bypass because it requires a real possession proof within the TTL.
type VerificationGrant struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenant_id"`
	ContactID  uuid.UUID `json:"contact_id"`
	Method     string    `json:"method"`
	VerifiedAt time.Time `json:"verified_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	CreatedAt  time.Time `json:"created_at"`
}

const verificationGrantCols = `id, tenant_id, contact_id, method, verified_at, expires_at, created_at`

func scanVerificationGrant(row pgx.Row) (VerificationGrant, error) {
	var g VerificationGrant
	err := row.Scan(&g.ID, &g.TenantID, &g.ContactID, &g.Method,
		&g.VerifiedAt, &g.ExpiresAt, &g.CreatedAt)
	return g, err
}

// ensureVerificationGrantsTable bootstraps the verification_grants table
// idempotently (like ensurePortalTokensTable) so upgrades need no manual
// migration. RLS mirrors the infra-managed tables (W32 idiom).
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureVerificationGrantsTable(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS verification_grants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    contact_id  UUID NOT NULL,
    method      TEXT NOT NULL,
    verified_at TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_verification_grants_contact ON verification_grants (tenant_id, contact_id);
ALTER TABLE verification_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE verification_grants FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'verification_grants' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON verification_grants
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure verification_grants table: %w", err)
	}
	return nil
}

// UpsertVerificationGrant records (or refreshes) the active grant for a
// contact: one grant per (tenant, contact); a re-verification moves
// verified_at/expires_at forward and updates the method.
func (s *Store) UpsertVerificationGrant(ctx context.Context, tenantID, contactID uuid.UUID, method string, ttl time.Duration) error {
	const q = `INSERT INTO verification_grants (tenant_id, contact_id, method, verified_at, expires_at)
	           VALUES ($1,$2,$3, now(), now() + $4::interval)
	           ON CONFLICT (tenant_id, contact_id) DO UPDATE
	           SET method = EXCLUDED.method,
	               verified_at = EXCLUDED.verified_at,
	               expires_at = EXCLUDED.expires_at`
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, q, tenantID, contactID, method, fmt.Sprintf("%d seconds", int64(ttl.Seconds())))
		return err
	})
}

// RevokeVerificationGrant deletes the contact's grant row (SPEC-W94).
// Idempotent: true iff a row was deleted, false when none existed. Active
// revocation complements the TTL — a pack policy change, a fraud signal,
// or an internal reset no longer waits for expiry.
func (s *Store) RevokeVerificationGrant(ctx context.Context, tenantID, contactID uuid.UUID) (bool, error) {
	const q = `DELETE FROM verification_grants WHERE tenant_id=$1 AND contact_id=$2`
	var n int64
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, execErr := tx.Exec(ctx, q, tenantID, contactID)
		if execErr != nil {
			return execErr
		}
		n = tag.RowsAffected()
		return nil
	})
	return n > 0, err
}

// GetActiveVerificationGrant returns the contact's grant when it is still
// inside its TTL, or ErrNotFound when none exists or it has expired.
func (s *Store) GetActiveVerificationGrant(ctx context.Context, tenantID, contactID uuid.UUID) (VerificationGrant, error) {
	const q = `SELECT ` + verificationGrantCols + ` FROM verification_grants
	           WHERE tenant_id=$1 AND contact_id=$2 AND expires_at > now()`
	var g VerificationGrant
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		g, scanErr = scanVerificationGrant(tx.QueryRow(ctx, q, tenantID, contactID))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return VerificationGrant{}, ErrNotFound
	}
	return g, err
}
