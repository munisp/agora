package store

// Referral agent registry persistence (SPEC-W45 CODER-A items 6+7, STK
// O10/O18): the referral_agents table is the ONLY valid referrer_id target
// for referrer_type=agent, and an agent must be status=approved with a
// beneficiary_id (the W44 K7 payout-beneficiary link) before commissions
// can flow to it.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReferralAgent mirrors booking.referral_agents — one registered referral
// agent (field marketer / reseller) of a tenant.
type ReferralAgent struct {
	ID            uuid.UUID  `json:"id"`
	TenantID      uuid.UUID  `json:"tenant_id"`
	Name          string     `json:"name"`
	Phone         string     `json:"phone"`
	Status        string     `json:"status"` // pending|approved|suspended
	BeneficiaryID *uuid.UUID `json:"beneficiary_id,omitempty"` // ledger/payout party (K7)
	CreatedAt     string     `json:"created_at"`
}

// Agent statuses.
const (
	AgentPending   = "pending"
	AgentApproved  = "approved"
	AgentSuspended = "suspended"
)

// ensureReferralAgentTable bootstraps booking.referral_agents idempotently
// (same pattern as ensureReferralTables): FORCE RLS tenant_isolation,
// unique (tenant_id, phone).
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureReferralAgentTable(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS referral_agents (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL,
    name           TEXT NOT NULL,
    phone          TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending','approved','suspended')),
    beneficiary_id UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_referral_agents_tenant ON referral_agents (tenant_id, status);
CREATE UNIQUE INDEX IF NOT EXISTS uq_referral_agents_tenant_phone
    ON referral_agents (tenant_id, phone);
ALTER TABLE referral_agents ENABLE ROW LEVEL SECURITY;
ALTER TABLE referral_agents FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'referral_agents' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON referral_agents
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure referral_agents table: %w", err)
	}
	return nil
}

// InsertReferralAgent registers one agent (status pending); (tenant_id,
// phone) is unique — a duplicate returns ErrConflict (HTTP 409).
func (s *Store) InsertReferralAgent(ctx context.Context, a *ReferralAgent) error {
	const q = `INSERT INTO referral_agents (tenant_id, name, phone, status, beneficiary_id)
		           VALUES ($1,$2,$3,$4,'pending')
		           RETURNING status, created_at`
	if a.Status == "" {
		a.Status = AgentPending
	}
	return s.withTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, q, a.TenantID, a.Name, a.Phone, a.BeneficiaryID).
			Scan(&a.Status, &a.CreatedAt)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
}

// GetReferralAgent fetches one agent by id (tenant-scoped).
func (s *Store) GetReferralAgent(ctx context.Context, tenantID, id uuid.UUID) (ReferralAgent, error) {
	var a ReferralAgent
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT tenant_id, name, phone, status, beneficiary_id, created_at
			   FROM referral_agents WHERE tenant_id=$1 AND id=$2`,
			tenantID, id).Scan(&a.TenantID, &a.Name, &a.Phone, &a.Status, &a.BeneficiaryID, &a.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	a.ID = id
	return a, err
}

// ListReferralAgents lists the tenant's agents (status filter optional),
// newest first.
func (s *Store) ListReferralAgents(ctx context.Context, tenantID uuid.UUID, status string) ([]ReferralAgent, error) {
	q := `SELECT id, tenant_id, name, phone, status, beneficiary_id, created_at
	        FROM referral_agents WHERE tenant_id=$1`
	args := []any{tenantID}
	if status != "" {
		q += ` AND status=$2`
		args = append(args, status)
	}
	out := []ReferralAgent{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q+` ORDER BY created_at DESC`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a ReferralAgent
			if err := rows.Scan(&a.ID, &a.TenantID, &a.Name, &a.Phone, &a.Status, &a.BeneficiaryID, &a.CreatedAt); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateReferralAgent applies the admin PATCH: a status transition
// (pending|approved|suspended, validated by the service layer) and/or the
// beneficiary link (set, or cleared with clearBeneficiary). Missing row →
// ErrNotFound.
func (s *Store) UpdateReferralAgent(ctx context.Context, tenantID, id uuid.UUID, status *string, beneficiaryID *uuid.UUID, clearBeneficiary bool) (ReferralAgent, error) {
	var a ReferralAgent
	a.ID = id
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`UPDATE referral_agents
			    SET status = COALESCE($3, status),
			        beneficiary_id = CASE WHEN $4 THEN NULL ELSE COALESCE($5, beneficiary_id) END
			  WHERE tenant_id=$1 AND id=$2
			  RETURNING tenant_id, name, phone, status, beneficiary_id, created_at`,
			tenantID, id, status, clearBeneficiary, beneficiaryID).
			Scan(&a.TenantID, &a.Name, &a.Phone, &a.Status, &a.BeneficiaryID, &a.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return a, err
}
