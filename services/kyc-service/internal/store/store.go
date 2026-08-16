// Package store provides Postgres persistence for the kyc DB (SPEC-W12 §5):
// the kyc_audit table (who/what/when/result for every resolution attempt).
//
// RLS: every tenant-scoped query runs inside a transaction that first sets
// `SET LOCAL app.tenant_id` (booking-service withTenant idiom) so the FORCE
// ROW LEVEL SECURITY policy enforces tenant isolation at the database layer.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Audit mirrors the kyc.kyc_audit table. Raw BVN/NIN values are NEVER
// stored — IDValueHash is a SHA-256 hex digest (NDPA data minimization,
// docs/kyc.md).
type Audit struct {
	AuditID      uuid.UUID `json:"audit_id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	Actor        string    `json:"actor"` // who: calling service / api client id
	SubjectPhone string    `json:"subject_phone"`
	IDType       string    `json:"id_type"` // bvn|nin
	IDValueHash  string    `json:"id_value_hash"`
	Status       string    `json:"status"` // verified|mismatch|pending
	Reference    string    `json:"reference"`
	LatencyMS    int64     `json:"latency_ms"`
	CreatedAt    time.Time `json:"created_at"`
}

// Repository is the persistence contract the HTTP handlers depend on.
// *Store is the Postgres implementation; tests substitute an in-memory fake.
type Repository interface {
	InsertAudit(ctx context.Context, a *Audit) error
	Ping(ctx context.Context) error
}

// Store wraps a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New connects to Postgres, verifies connectivity and bootstraps the
// kyc_audit table + RLS policy (idempotent; covers fresh and existing
// installs — booking-service ensureSitesTable role).
func New(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.bootstrap(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("bootstrap kyc schema: %w", err)
	}
	return s, nil
}

// bootstrap creates the kyc_audit table with the tenant_isolation RLS
// policy (02-identity-schema.sql pattern).
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) bootstrap(ctx context.Context) error {
	const ddl = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE IF NOT EXISTS kyc_audit (
    audit_id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL,
    actor         TEXT NOT NULL DEFAULT '',
    subject_phone TEXT NOT NULL,
    id_type       TEXT NOT NULL CHECK (id_type IN ('bvn','nin')),
    id_value_hash TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('verified','mismatch','pending')),
    reference     TEXT NOT NULL,
    latency_ms    BIGINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_kyc_audit_tenant ON kyc_audit (tenant_id, created_at DESC);
ALTER TABLE kyc_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_audit FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_policies
                   WHERE schemaname = current_schema()
                     AND tablename = 'kyc_audit'
                     AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON kyc_audit
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END
$$;
-- SPEC-W34 GF7: kyc_audit is an append-only forensic trail. Defense in
-- depth: (1) REVOKE UPDATE/DELETE from PUBLIC and the application role
-- (the service only ever INSERTs, so nothing legitimate loses rights);
-- (2) a BEFORE UPDATE OR DELETE trigger that raises — this also binds the
-- table owner / superuser paths that REVOKE cannot touch.
REVOKE UPDATE, DELETE ON kyc_audit FROM PUBLIC;
DO $$
BEGIN
    EXECUTE format('REVOKE UPDATE, DELETE ON kyc_audit FROM %I', current_user);
END
$$;
CREATE OR REPLACE FUNCTION kyc_audit_append_only() RETURNS trigger AS $fn$
BEGIN
    RAISE EXCEPTION 'kyc_audit is append-only: % is forbidden (SPEC-W34 GF7)', TG_OP;
END;
$fn$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS kyc_audit_append_only ON kyc_audit;
CREATE TRIGGER kyc_audit_append_only
    BEFORE UPDATE OR DELETE ON kyc_audit
    FOR EACH ROW EXECUTE FUNCTION kyc_audit_append_only();`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure kyc_audit table: %w", err)
	}
	return nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks database liveness (used by /healthz).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// withTenant runs fn inside a transaction with `SET LOCAL app.tenant_id`
// applied (set_config(..., true): parameter binding keeps this
// injection-safe), so the RLS policy scopes every statement of fn to the
// given tenant.
func (s *Store) withTenant(ctx context.Context, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// InsertAudit records one resolution attempt. Every /v1/kyc/resolve call
// (any outcome) must produce exactly one audit row — the request fails if
// the audit write fails (no audit, no resolution).
func (s *Store) InsertAudit(ctx context.Context, a *Audit) error {
	if a.AuditID == uuid.Nil {
		a.AuditID = uuid.New()
	}
	const q = `INSERT INTO kyc_audit
	           (audit_id, tenant_id, actor, subject_phone, id_type, id_value_hash, status, reference, latency_ms)
	           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at`
	return s.withTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, q,
			a.AuditID, a.TenantID, a.Actor, a.SubjectPhone, a.IDType,
			a.IDValueHash, a.Status, a.Reference, a.LatencyMS).Scan(&a.CreatedAt); err != nil {
			return fmt.Errorf("insert kyc audit: %w", err)
		}
		return nil
	})
}
