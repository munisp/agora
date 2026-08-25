// Package store implements the Postgres persistence layer for identity-service.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a queried row does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned on unique-constraint violations (e.g. slug taken).
var ErrConflict = errors.New("conflict")

// Tenant mirrors the tenants table (02-identity-schema.sql).
type Tenant struct {
	ID          uuid.UUID       `json:"id"`
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Timezone    string          `json:"timezone"`
	Currency    string          `json:"currency"`
	Locale      string          `json:"locale"`
	Terminology json.RawMessage `json:"terminology"`
	Plan        string          `json:"plan"`
	Industry    string          `json:"industry"`
	Metadata    json.RawMessage `json:"metadata"`
	IsTwin      bool            `json:"is_twin"` // SPEC-W44 W-I-3 / S1-F7-06
	CreatedAt   time.Time       `json:"created_at"`
}

// Membership mirrors the memberships table.
type Membership struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	UserID    string    `json:"user_id"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// Store wraps a pgxpool with the tenant/member queries.
type Store struct {
	pool *pgxpool.Pool
}

// New connects to Postgres and verifies connectivity. The tenancy columns
// (industry/metadata/is_twin) are bootstrapped idempotently below, so no
// schema auto-migration is needed here (SPEC-W43 I-03).
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
		return nil, fmt.Errorf("bootstrap schema: %w", err)
	}
	return s, nil
}

// bootstrap applies the additive tenancy columns the service relies on.
// They ship in 02-identity-schema.sql, but existing deployments created
// before those ALTERs need them applied at boot (idempotent).
//
// NOTE (RLS): this is a superuser migration path, not a tenant query — it
// intentionally runs outside the consent store's withTenant RLS context
// (booking-service ensureSitesTable idiom). SPEC-W43 I-03: each statement
// tolerates insufficient_privilege (42501) with a WARN so the least-
// privilege app_identity role boots cleanly once the infra layer owns DDL.
func (s *Store) bootstrap(ctx context.Context) error {
	stmts := []struct{ name, sql string }{
		{"tenants.industry", `ALTER TABLE tenants ADD COLUMN IF NOT EXISTS industry TEXT NOT NULL DEFAULT 'generic'`},
		{"tenants.metadata", `ALTER TABLE tenants ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'::jsonb`},
		{"tenants.is_twin", `ALTER TABLE tenants ADD COLUMN IF NOT EXISTS is_twin BOOLEAN NOT NULL DEFAULT false`},
		{"tenants is_twin index", `CREATE INDEX IF NOT EXISTS idx_tenants_is_twin ON tenants (is_twin) WHERE is_twin`},
		{"tenants plan check (twin)", `ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_plan_check`},
		{"tenants plan check (add)", `ALTER TABLE tenants ADD CONSTRAINT tenants_plan_check CHECK (plan IN ('free','pro','enterprise','twin'))`},
	}
	for _, st := range stmts {
		if _, err := s.pool.Exec(ctx, st.sql); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "42501" {
				slog.Warn("bootstrap skipped (insufficient privilege — infra-owned DDL assumed)",
					"stmt", st.name)
				continue
			}
			return fmt.Errorf("%s: %w", st.name, err)
		}
	}
	return nil
}

// Close releases the connection pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks database liveness.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

const tenantCols = `id, slug, name, timezone, currency, locale, terminology, plan, industry, metadata, is_twin, created_at`

// GetTenantBySlug fetches one tenant by slug.
func (s *Store) GetTenantBySlug(ctx context.Context, slug string) (Tenant, error) {
	return scanTenant(s.pool.QueryRow(ctx,
		`SELECT `+tenantCols+` FROM tenants WHERE slug = $1`, slug))
}

// GetTenantByID fetches one tenant by uuid (consent resolveTenant).
func (s *Store) GetTenantByID(ctx context.Context, id uuid.UUID) (Tenant, error) {
	return scanTenant(s.pool.QueryRow(ctx,
		`SELECT `+tenantCols+` FROM tenants WHERE id = $1`, id))
}

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	err := row.Scan(&t.ID, &t.Slug, &t.Name, &t.Timezone, &t.Currency, &t.Locale,
		&t.Terminology, &t.Plan, &t.Industry, &t.Metadata, &t.IsTwin, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// CreateTenant inserts a tenant row, generating id/created_at. Defaults for
// terminology/metadata/industry are applied when unset. Duplicate slug
// yields ErrConflict.
func (s *Store) CreateTenant(ctx context.Context, t *Tenant) error {
	if len(t.Terminology) == 0 {
		t.Terminology = json.RawMessage(`{}`)
	}
	if len(t.Metadata) == 0 {
		t.Metadata = json.RawMessage(`{}`)
	}
	if t.Industry == "" {
		t.Industry = "generic"
	}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name, timezone, currency, locale, terminology, plan, industry, metadata, is_twin)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 RETURNING `+tenantCols,
		t.Slug, t.Name, t.Timezone, t.Currency, t.Locale, t.Terminology, t.Plan,
		t.Industry, t.Metadata, t.IsTwin).Scan(
		&t.ID, &t.Slug, &t.Name, &t.Timezone, &t.Currency, &t.Locale,
		&t.Terminology, &t.Plan, &t.Industry, &t.Metadata, &t.IsTwin, &t.CreatedAt)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// DeleteTenant removes a tenant by slug (SPEC-W3 §3 innovation 12).
func (s *Store) DeleteTenant(ctx context.Context, slug string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM tenants WHERE slug = $1`, slug)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MergeTerminology merge-patches a tenant's terminology overrides (patch
// keys win over stored keys) and returns the merged document.
func (s *Store) MergeTerminology(ctx context.Context, slug string, patch json.RawMessage) (json.RawMessage, error) {
	var merged json.RawMessage
	err := s.pool.QueryRow(ctx,
		`UPDATE tenants SET terminology = terminology || $2::jsonb WHERE slug = $1
		 RETURNING terminology`, slug, patch).Scan(&merged)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return merged, err
}

// ListMembers returns all memberships of a tenant.
func (s *Store) ListMembers(ctx context.Context, tenantID uuid.UUID) ([]Membership, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, user_id, role, created_at FROM memberships
		 WHERE tenant_id = $1 ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Membership
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.TenantID, &m.UserID, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddMember inserts a membership (idempotent upsert on tenant+user).
func (s *Store) AddMember(ctx context.Context, m Membership) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1,$2,$3)
		 ON CONFLICT (tenant_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
		m.TenantID, m.UserID, m.Role)
	return err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
