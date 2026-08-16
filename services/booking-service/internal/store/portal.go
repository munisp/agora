package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Portal token channels (Wave 5 #7 customer self-service portal).
const (
	PortalChannelSMS   = "sms"
	PortalChannelEmail = "email"
)

// PortalToken mirrors booking.portal_tokens: a one-time 6-digit login code
// for the customer self-service portal. Only the SHA-256 hash of the code is
// stored (token_hash) — the plaintext code travels exclusively through the
// notification outbox event to the customer's phone/email.
type PortalToken struct {
	ID         uuid.UUID  `json:"id"`
	TenantID   uuid.UUID  `json:"tenant_id"`
	ContactID  uuid.UUID  `json:"contact_id"`
	TokenHash  string     `json:"-"`
	Channel    string     `json:"channel"`
	Attempts   int        `json:"attempts"`
	ExpiresAt  time.Time  `json:"expires_at"`
	ConsumedAt *time.Time `json:"consumed_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

const portalTokenCols = `id, tenant_id, contact_id, token_hash, channel, attempts, expires_at, consumed_at, created_at`

func scanPortalToken(row pgx.Row) (PortalToken, error) {
	var t PortalToken
	err := row.Scan(&t.ID, &t.TenantID, &t.ContactID, &t.TokenHash, &t.Channel,
		&t.Attempts, &t.ExpiresAt, &t.ConsumedAt, &t.CreatedAt)
	return t, err
}

// ensurePortalTokensTable bootstraps the portal_tokens table idempotently
// (like ensureWaitlistTable) so upgrades need no manual migration. RLS
// mirrors the infra-managed tables.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensurePortalTokensTable(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS portal_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    contact_id  UUID NOT NULL,
    token_hash  TEXT NOT NULL,
    channel     TEXT NOT NULL CHECK (channel IN ('sms','email')),
    attempts    INTEGER NOT NULL DEFAULT 0,
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_portal_tokens_contact ON portal_tokens (tenant_id, contact_id, created_at DESC);
ALTER TABLE portal_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE portal_tokens FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'portal_tokens' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON portal_tokens
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure portal_tokens table: %w", err)
	}
	return nil
}

// CreatePortalToken inserts a login token, generating id + created_at.
func (s *Store) CreatePortalToken(ctx context.Context, t *PortalToken) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	const q = `INSERT INTO portal_tokens (id, tenant_id, contact_id, token_hash, channel, expires_at)
	           VALUES ($1,$2,$3,$4,$5,$6) RETURNING created_at`
	return s.withTenant(ctx, t.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, t.ID, t.TenantID, t.ContactID, t.TokenHash, t.Channel, t.ExpiresAt).
			Scan(&t.CreatedAt)
	})
}

// GetActivePortalToken returns the newest unconsumed, unexpired token for a
// contact, or ErrNotFound when none exists.
func (s *Store) GetActivePortalToken(ctx context.Context, tenantID, contactID uuid.UUID) (PortalToken, error) {
	const q = `SELECT ` + portalTokenCols + ` FROM portal_tokens
	           WHERE tenant_id=$1 AND contact_id=$2 AND consumed_at IS NULL AND expires_at > now()
	           ORDER BY created_at DESC LIMIT 1`
	var t PortalToken
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		t, err = scanPortalToken(tx.QueryRow(ctx, q, tenantID, contactID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// IncrementPortalTokenAttempts records a failed verification attempt and
// returns the new attempt count (the lockout check lives in the handler).
func (s *Store) IncrementPortalTokenAttempts(ctx context.Context, tenantID, id uuid.UUID) (int, error) {
	var attempts int
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`UPDATE portal_tokens SET attempts = attempts + 1 WHERE tenant_id=$1 AND id=$2 RETURNING attempts`,
			tenantID, id).Scan(&attempts)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return attempts, err
}

// ConsumePortalToken marks a token used (successful verification).
func (s *Store) ConsumePortalToken(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE portal_tokens SET consumed_at=now() WHERE tenant_id=$1 AND id=$2 AND consumed_at IS NULL`,
			tenantID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// GetContactByChannel finds the contact behind a portal login identifier:
// phone number for the sms channel, e-mail (case-insensitive) for email.
func (s *Store) GetContactByChannel(ctx context.Context, tenantID uuid.UUID, channel, value string) (Contact, error) {
	var c Contact
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		col := `phone`
		if channel == PortalChannelEmail {
			col = `lower(email)`
			value = strings.ToLower(value)
		}
		return tx.QueryRow(ctx,
			`SELECT id, tenant_id, name, phone, email, notes FROM contacts
			 WHERE tenant_id=$1 AND `+col+`=$2 ORDER BY name LIMIT 1`,
			tenantID, value).Scan(&c.ID, &c.TenantID, &c.Name, &c.Phone, &c.Email, &c.Notes)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}
