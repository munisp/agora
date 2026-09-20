package store

// Referral agent registry (SPEC-W45 CODER-A item 7, STK O10): field agents
// who refer customers must be REGISTERED (pending → approved|suspended)
// before commissions can flow to them. beneficiary_id links the agent to
// the payments payout-beneficiary registry (SPEC-W44 K7) — commissions to
// an agent require status=approved AND beneficiary_id set (enforced by the
// referrals service at verify time).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Referral agent statuses.
const (
	AgentPending   = "pending"
	AgentApproved  = "approved"
	AgentSuspended = "suspended"
)

// ReferralAgent mirrors booking.referral_agents.
type ReferralAgent struct {
	ID            uuid.UUID  `json:"id"`
	TenantID      uuid.UUID  `json:"tenant_id"`
	Name          string     `json:"name"`
	Phone         string     `json:"phone"`
	Status        string     `json:"status"` // pending|approved|suspended
	BeneficiaryID *uuid.UUID `json:"beneficiary_id,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// ensureReferralAgentTables bootstraps booking.referral_agents idempotently
// (same pattern as ensureReferralTables). RLS: enabled + forced with the
// tenant_isolation policy.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureReferralAgentTables(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS referral_agents (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL,
    name           TEXT NOT NULL,
    phone          TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending','approved','suspended')),
    beneficiary_id UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One registered agent per (tenant, phone).
CREATE UNIQUE INDEX IF NOT EXISTS uq_referral_agents_tenant_phone
    ON referral_agents (tenant_id, phone);
CREATE INDEX IF NOT EXISTS idx_referral_agents_tenant_status
    ON referral_agents (tenant_id, status, created_at);
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

const referralAgentCols = `id, tenant_id, name, phone, status, beneficiary_id, created_at, updated_at`

func scanReferralAgent(row pgx.Row) (ReferralAgent, error) {
	var a ReferralAgent
	err := row.Scan(&a.ID, &a.TenantID, &a.Name, &a.Phone, &a.Status, &a.BeneficiaryID, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// InsertReferralAgent registers one agent (status pending). A duplicate
// (tenant_id, phone) answers ErrConflict (the service maps it to 409).
func (s *Store) InsertReferralAgent(ctx context.Context, a *ReferralAgent) error {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	const q = `INSERT INTO referral_agents (id, tenant_id, name, phone, status, beneficiary_id)
		           VALUES ($1,$2,$3,$4,'pending',$5)
		           RETURNING status, created_at, updated_at`
	return s.withTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, q, a.ID, a.TenantID, a.Name, a.Phone, a.BeneficiaryID).
			Scan(&a.Status, &a.CreatedAt, &a.UpdatedAt)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
}

// GetReferralAgent fetches one agent scoped to a tenant.
func (s *Store) GetReferralAgent(ctx context.Context, tenantID, id uuid.UUID) (ReferralAgent, error) {
	var a ReferralAgent
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		a, err = scanReferralAgent(tx.QueryRow(ctx,
			`SELECT `+referralAgentCols+` FROM referral_agents WHERE tenant_id=$1 AND id=$2`,
			tenantID, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// ListReferralAgents returns agents of a tenant (optional status filter),
// newest first.
func (s *Store) ListReferralAgents(ctx context.Context, tenantID uuid.UUID, status string) ([]ReferralAgent, error) {
	q := `SELECT ` + referralAgentCols + ` FROM referral_agents WHERE tenant_id=$1`
	args := []any{tenantID}
	if status != "" {
		q += ` AND status=$2`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT 500`
	out := []ReferralAgent{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanReferralAgent(rows)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateReferralAgent applies the staff PATCH: status transition
// (admin-gated at the API) and/or beneficiary link. Returns ErrNotFound for
// a missing row.
func (s *Store) UpdateReferralAgent(ctx context.Context, tenantID, id uuid.UUID, status *string, beneficiaryID *uuid.UUID, clearBeneficiary bool) (ReferralAgent, error) {
	var a ReferralAgent
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		a, err = scanReferralAgent(tx.QueryRow(ctx,
			`SELECT `+referralAgentCols+` FROM referral_agents WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
			tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != nil {
			a.Status = *status
		}
		switch {
		case clearBeneficiary:
			a.BeneficiaryID = nil
		case beneficiaryID != nil:
			a.BeneficiaryID = beneficiaryID
		}
		return tx.QueryRow(ctx,
			`UPDATE referral_agents SET status=$3, beneficiary_id=$4, updated_at=now()
			 WHERE tenant_id=$1 AND id=$2
			 RETURNING created_at, updated_at`,
			tenantID, id, a.Status, a.BeneficiaryID).Scan(&a.CreatedAt, &a.UpdatedAt)
	})
	return a, err
}
