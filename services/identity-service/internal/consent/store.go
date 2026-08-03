package consent

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when no matching consent record exists.
var ErrNotFound = errors.New("consent not found")

// Repository is the persistence contract the handlers depend on. *Store is
// the Postgres implementation; tests substitute an in-memory fake.
type Repository interface {
	// Capture upserts a consent record, idempotent on
	// (tenant_id, data_subject_id, purpose). A replay keeps the original
	// captured_ts; a re-capture after erasure clears erasure_ts (re-consent).
	Capture(ctx context.Context, rec *Record) error
	// List returns all consent records of one data subject (any erasure
	// state), newest first.
	List(ctx context.Context, tenantID uuid.UUID, subject string) ([]Record, error)
	// Active returns the non-erased consent for (subject, purpose).
	Active(ctx context.Context, tenantID uuid.UUID, subject, purpose string) (Record, error)
	// Erase tombstones matching records (sets erasure_ts = now(), only where
	// not already erased). purpose empty erases ALL of the subject's
	// purposes. Returns the number of records tombstoned by this call.
	Erase(ctx context.Context, tenantID uuid.UUID, subject, purpose string) (int, error)
	// Ping checks database liveness.
	Ping(ctx context.Context) error
}

// Store is the Postgres-backed Repository. RLS: every query runs inside a
// transaction with `SET LOCAL app.tenant_id` (booking-service withTenant
// idiom) so the FORCE ROW LEVEL SECURITY policy on consents enforces tenant
// isolation at the database layer.
type Store struct {
	pool *pgxpool.Pool
}

// New connects to Postgres, verifies connectivity and bootstraps the
// consents table + RLS policy (idempotent; covers fresh and existing
// installs, same role as the identity store's bootstrap ALTERs).
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
		return nil, fmt.Errorf("bootstrap consents schema: %w", err)
	}
	return s, nil
}

// bootstrap creates the consents table with the tenant_isolation RLS policy
// of 02-identity-schema.sql.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant (booking-service
// ensureSitesTable idiom).
func (s *Store) bootstrap(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS consents (
    consent_id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    data_subject_id  TEXT NOT NULL,
    purpose          TEXT NOT NULL,
    captured_ts      TIMESTAMPTZ NOT NULL DEFAULT now(),
    captured_channel TEXT NOT NULL DEFAULT '',
    captured_locale  TEXT NOT NULL DEFAULT '',
    erasure_ts       TIMESTAMPTZ,
    UNIQUE (tenant_id, data_subject_id, purpose)
);
CREATE INDEX IF NOT EXISTS idx_consents_subject ON consents (tenant_id, data_subject_id);
ALTER TABLE consents ENABLE ROW LEVEL SECURITY;
ALTER TABLE consents FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_policies
                   WHERE schemaname = current_schema()
                     AND tablename = 'consents'
                     AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON consents
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END
$$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure consents table: %w", err)
	}
	return nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks database liveness.
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

const recordCols = `consent_id, tenant_id, data_subject_id, purpose, captured_ts, captured_channel, captured_locale, erasure_ts`

func scanRecord(row pgx.Row) (Record, error) {
	var r Record
	err := row.Scan(&r.ConsentID, &r.TenantID, &r.DataSubjectID, &r.Purpose,
		&r.CapturedTS, &r.CapturedChannel, &r.CapturedLocale, &r.ErasureTS)
	return r, err
}

// Capture implements Repository.
func (s *Store) Capture(ctx context.Context, rec *Record) error {
	if rec.ConsentID == uuid.Nil {
		rec.ConsentID = uuid.New()
	}
	// ON CONFLICT: idempotent replay keeps the original captured_ts (first
	// capture wins); channel/locale refresh; a re-capture after an erasure
	// tombstone clears erasure_ts (explicit re-consent).
	const q = `INSERT INTO consents (` + recordCols + `)
	           VALUES ($1,$2,$3,$4,now(),$5,$6,NULL)
	           ON CONFLICT (tenant_id, data_subject_id, purpose) DO UPDATE
	           SET captured_channel = EXCLUDED.captured_channel,
	               captured_locale  = EXCLUDED.captured_locale,
	               erasure_ts       = NULL
	           RETURNING ` + recordCols
	return s.withTenant(ctx, rec.TenantID, func(tx pgx.Tx) error {
		out, err := scanRecord(tx.QueryRow(ctx, q,
			rec.ConsentID, rec.TenantID, rec.DataSubjectID, rec.Purpose,
			rec.CapturedChannel, rec.CapturedLocale))
		if err != nil {
			return fmt.Errorf("capture consent: %w", err)
		}
		*rec = out
		return nil
	})
}

// List implements Repository.
func (s *Store) List(ctx context.Context, tenantID uuid.UUID, subject string) ([]Record, error) {
	const q = `SELECT ` + recordCols + ` FROM consents
	           WHERE tenant_id = $1 AND data_subject_id = $2
	           ORDER BY captured_ts DESC`
	var out []Record
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID, subject)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRecord(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// Active implements Repository.
func (s *Store) Active(ctx context.Context, tenantID uuid.UUID, subject, purpose string) (Record, error) {
	const q = `SELECT ` + recordCols + ` FROM consents
	           WHERE tenant_id = $1 AND data_subject_id = $2 AND purpose = $3 AND erasure_ts IS NULL`
	var r Record
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		r, err = scanRecord(tx.QueryRow(ctx, q, tenantID, subject, purpose))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// Erase implements Repository.
func (s *Store) Erase(ctx context.Context, tenantID uuid.UUID, subject, purpose string) (int, error) {
	var n int
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var tag pgconn.CommandTag
		var err error
		if purpose == "" {
			tag, err = tx.Exec(ctx,
				`UPDATE consents SET erasure_ts = now()
				 WHERE tenant_id = $1 AND data_subject_id = $2 AND erasure_ts IS NULL`,
				tenantID, subject)
		} else {
			tag, err = tx.Exec(ctx,
				`UPDATE consents SET erasure_ts = now()
				 WHERE tenant_id = $1 AND data_subject_id = $2 AND purpose = $3 AND erasure_ts IS NULL`,
				tenantID, subject, purpose)
		}
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		return nil
	})
	return n, err
}
