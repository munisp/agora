// Package store provides Postgres persistence for the identity DB
// (tenants + memberships per SPEC §7).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/opendesk/identity-service/internal/poolx"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned on unique-constraint violations (e.g. slug taken).
var ErrConflict = errors.New("conflict")

// Store wraps two pgx pools (SPEC-W43 I-03 / SPEC-W44 W-I):
//   - pool: the request-scoped pool (DATABASE_URL, app_identity_login in
//     least-privilege deploys). Membership queries run inside a transaction
//     with `SET LOCAL app.tenant_id` (set_config(..., true)) so the
//     fail-closed RLS policy scopes them to one tenant.
//   - tenants: the pool used for tenants-table access (GetTenantBySlug,
//     CreateTenant, DeleteTenant, MergeTerminology). The tenants RLS policy is
//     keyed on the tenant's own id, which cannot pre-exist for a lookup or a
//     create — these paths need the app_identity_internal escape role
//     (05-app-roles.sql; policy escape via pg_has_role, billing 0002 idiom).
//     INTERNAL_DATABASE_URL provides that connection; when unset it aliases
//     the main pool (dev superuser bypasses RLS entirely).
type Store struct {
	pool    *pgxpool.Pool
	tenants *pgxpool.Pool
}

// New connects to Postgres and verifies connectivity. internalURL is
// optional (INTERNAL_DATABASE_URL): when given (and different), tenant-table
// operations use a second pool connected as the internal escape role.
func New(ctx context.Context, databaseURL string, internalURL ...string) (*Store, error) {
	// SPEC-W46 PERF-08: budgeted pools (poolx); DATABASE_URL pool_* params win.
	pool, err := poolx.New(ctx, databaseURL, poolx.MaxConns, poolx.MinConns)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	s := &Store{pool: pool, tenants: pool}
	if len(internalURL) > 0 && internalURL[0] != "" && internalURL[0] != databaseURL {
		ipool, err := poolx.New(ctx, internalURL[0], poolx.InternalMaxConns, poolx.InternalMinConns)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("connect internal postgres: %w", err)
		}
		if err := ipool.Ping(ctx); err != nil {
			ipool.Close()
			pool.Close()
			return nil, fmt.Errorf("ping internal postgres: %w", err)
		}
		s.tenants = ipool
	}
	if err := s.bootstrap(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("bootstrap schema: %w", err)
	}
	return s, nil
}

// bootstrap applies idempotent schema evolution for existing installs (fresh
// installs get the same columns from 02-identity-schema.sql /
// 08-code-bootstrap-parity.sql). SPEC-W43 I-03: every statement tolerates
// insufficient_privilege (42501) with a WARN so the least-privilege
// app_identity role boots cleanly once the infra layer owns the DDL; with
// the bootstrap superuser the statements apply for real.
func (s *Store) bootstrap(ctx context.Context) error {
	stmts := []struct{ name, sql string }{
		// SPEC-CRM §C1: industry pack id per tenant.
		{"add tenants.industry",
			`ALTER TABLE tenants ADD COLUMN IF NOT EXISTS industry TEXT NOT NULL DEFAULT 'salon'`},
		// SPEC-W3 §3 innovation 12: free-form tenant metadata (digital twins
		// carry {"twin_of": "<source slug>"}).
		{"add tenants.metadata",
			`ALTER TABLE tenants ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'`},
		// SPEC-W44 W-I-3 (S1-F7-06): authoritative twin flag. Replaces the
		// slug-substring heuristic; set by createTwin only.
		{"add tenants.is_twin",
			`ALTER TABLE tenants ADD COLUMN IF NOT EXISTS is_twin BOOLEAN NOT NULL DEFAULT false`},
		// Backfill rows provisioned by the old heuristic so the guard is
		// exact for pre-existing twins too (marker is unique to twin slugs).
		{"backfill tenants.is_twin",
			`UPDATE tenants SET is_twin = true
			 WHERE NOT is_twin AND (slug LIKE '%-twin-%' OR plan = 'twin')`},
		// SPEC-W44 F4 (V2-D1): widen the tenants.plan CHECK to accept 'twin'
		// on installs created before 02-identity-schema.sql included it
		// (createTwin INSERTs plan='twin' → 23514 → 500 under the old CHECK).
		// Drop + re-add is idempotent on fresh installs (same constraint).
		{"rewrite tenants.plan CHECK (allow twin)", `
ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_plan_check;
ALTER TABLE tenants ADD CONSTRAINT tenants_plan_check
    CHECK (plan IN ('free','pro','enterprise','twin'))`},
		// SPEC-W43 I-03: fail-closed RLS with the internal-role escape
		// (pg_has_role cannot be forged by a request-scoped GUC —
		// billing 0002 idiom). tenants is keyed on its own id.
		{"rewrite tenants.tenant_isolation policy", `
DROP POLICY IF EXISTS tenant_isolation ON tenants;
CREATE POLICY tenant_isolation ON tenants
    USING (id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_identity_internal', 'member'))`},
		{"rewrite memberships.tenant_isolation policy", `
DROP POLICY IF EXISTS tenant_isolation ON memberships;
CREATE POLICY tenant_isolation ON memberships
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_identity_internal', 'member'))`},
		// SPEC-W45 K17: tenant API keys (hashed server-side; the secret is
		// shown once at creation). Same fail-closed RLS idiom as memberships;
		// hash validation is cross-tenant and runs on the internal pool.
		{"ensure tenant_api_keys", `
CREATE TABLE IF NOT EXISTS tenant_api_keys (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    prefix     TEXT NOT NULL,
    key_hash   TEXT NOT NULL UNIQUE,
    scopes     TEXT[] NOT NULL DEFAULT '{}',
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_tenant_api_keys_tenant ON tenant_api_keys (tenant_id);
CREATE INDEX IF NOT EXISTS idx_tenant_api_keys_hash ON tenant_api_keys (key_hash) WHERE revoked_at IS NULL;
ALTER TABLE tenant_api_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_api_keys FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON tenant_api_keys;
CREATE POLICY tenant_isolation ON tenant_api_keys
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_identity_internal', 'member'))`},
	}
	for _, st := range stmts {
		if _, err := s.pool.Exec(ctx, st.sql); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "42501" {
				slog.Warn("identity bootstrap skipped (insufficient privilege — infra-owned DDL assumed)",
					"stmt", st.name)
				continue
			}
			return fmt.Errorf("%s: %w", st.name, err)
		}
	}
	return nil
}

// Close releases the pool(s).
func (s *Store) Close() {
	s.pool.Close()
	if s.tenants != s.pool {
		s.tenants.Close()
	}
}

// Ping checks database liveness (used by /healthz).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Tenant mirrors the identity.tenants table.
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
	// IsTwin marks digital-twin tenants (SPEC-W3 §3 innovation 12). It is the
	// ONLY deletion-guard source (SPEC-W44 W-I-3 / S1-F7-06): the old
	// slug-substring check let any tenant named "*-twin-*" be deleted by
	// anyone. Set by createTwin; never settable from the public API.
	IsTwin    bool      `json:"is_twin"`
	CreatedAt time.Time `json:"created_at"`
}

// Membership mirrors identity.memberships.
type Membership struct {
	TenantID uuid.UUID `json:"tenant_id"`
	UserID   string    `json:"user_id"`
	Role     string    `json:"role"`
}

// CreateTenant inserts a tenant row.
func (s *Store) CreateTenant(ctx context.Context, t *Tenant) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	if t.Industry == "" {
		t.Industry = "salon"
	}
	if t.Plan == "" {
		// The column DEFAULT 'free' is defeated by the explicit INSERT
		// param, and '' violates the tenants.plan CHECK (V2-D1 parity).
		t.Plan = "free"
	}
	if len(t.Metadata) == 0 {
		t.Metadata = json.RawMessage(`{}`)
	}
	if len(t.Terminology) == 0 {
		t.Terminology = json.RawMessage(`{}`)
	}
	const q = `INSERT INTO tenants (id, slug, name, timezone, currency, locale, terminology, plan, industry, metadata, is_twin)
	           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING created_at`
	err := s.tenants.QueryRow(ctx, q, t.ID, t.Slug, t.Name, t.Timezone, t.Currency, t.Locale, t.Terminology, t.Plan, t.Industry, t.Metadata, t.IsTwin).
		Scan(&t.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrConflict
		}
		return fmt.Errorf("insert tenant: %w", err)
	}
	return nil
}

// DeleteTenant removes a tenant and its memberships (SPEC-W3 §3 innovation
// 12, digital-twin cleanup). Bookings/conversations/knowledge of the tenant
// are NOT cascaded here — cross-service data expires with the twin's short
// lifetime and is cleaned up by the owning services' own retention (see
// README "Digital twins").
func (s *Store) DeleteTenant(ctx context.Context, slug string) error {
	tx, err := s.tenants.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx,
		`DELETE FROM memberships WHERE tenant_id = (SELECT id FROM tenants WHERE slug = $1)`, slug); err != nil {
		return fmt.Errorf("delete memberships: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM tenants WHERE slug = $1`, slug)
	if err != nil {
		return fmt.Errorf("delete tenant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

// GetTenantBySlug fetches a tenant by slug.
func (s *Store) GetTenantBySlug(ctx context.Context, slug string) (Tenant, error) {
	const q = `SELECT id, slug, name, timezone, currency, locale, terminology, plan, industry, metadata, is_twin, created_at
	           FROM tenants WHERE slug = $1`
	var t Tenant
	err := s.tenants.QueryRow(ctx, q, slug).Scan(
		&t.ID, &t.Slug, &t.Name, &t.Timezone, &t.Currency, &t.Locale, &t.Terminology, &t.Plan, &t.Industry, &t.Metadata, &t.IsTwin, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, fmt.Errorf("get tenant: %w", err)
	}
	return t, nil
}

// GetTenantByID fetches a tenant by id (SPEC-W44 F4 / V2-D3): the consent
// gate needs the slug to check X-Tenant-Slugs binding when the request
// carries the tenant reference as a uuid (X-Tenant-ID / tenant_id).
func (s *Store) GetTenantByID(ctx context.Context, id uuid.UUID) (Tenant, error) {
	const q = `SELECT id, slug, name, timezone, currency, locale, terminology, plan, industry, metadata, is_twin, created_at
	           FROM tenants WHERE id = $1`
	var t Tenant
	err := s.tenants.QueryRow(ctx, q, id).Scan(
		&t.ID, &t.Slug, &t.Name, &t.Timezone, &t.Currency, &t.Locale, &t.Terminology, &t.Plan, &t.Industry, &t.Metadata, &t.IsTwin, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, fmt.Errorf("get tenant by id: %w", err)
	}
	return t, nil
}

// MergeTerminology merge-patches tenant.terminology (jsonb || — patch keys
// win) and returns the resulting terminology document. Used by the onboarding
// ApplyIndustryPack activity via POST /internal/tenants/{slug}/terminology.
func (s *Store) MergeTerminology(ctx context.Context, slug string, patch json.RawMessage) (json.RawMessage, error) {
	const q = `UPDATE tenants
	           SET terminology = COALESCE(terminology, '{}'::jsonb) || $2::jsonb
	           WHERE slug = $1
	           RETURNING terminology`
	var out json.RawMessage
	err := s.tenants.QueryRow(ctx, q, slug, patch).Scan(&out)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("merge terminology: %w", err)
	}
	return out, nil
}

// withTenant runs fn inside a transaction with `SET LOCAL app.tenant_id`
// applied (set_config(..., true): parameter binding keeps this
// injection-safe), so the fail-closed tenant_isolation RLS policy on
// memberships scopes every statement of fn to the given tenant
// (SPEC-W43 I-03; booking-service idiom).
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

// ListMembers returns all memberships for a tenant.
func (s *Store) ListMembers(ctx context.Context, tenantID uuid.UUID) ([]Membership, error) {
	const q = `SELECT tenant_id, user_id, role FROM memberships WHERE tenant_id = $1 ORDER BY user_id`
	var out []Membership
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m Membership
			if err := rows.Scan(&m.TenantID, &m.UserID, &m.Role); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	return out, nil
}

// AddMember inserts a membership row (idempotent on tenant/user pair).
func (s *Store) AddMember(ctx context.Context, m Membership) error {
	const q = `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1,$2,$3)
	           ON CONFLICT (tenant_id, user_id) DO UPDATE SET role = EXCLUDED.role`
	return s.withTenant(ctx, m.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, q, m.TenantID, m.UserID, m.Role); err != nil {
			return fmt.Errorf("add member: %w", err)
		}
		return nil
	})
}

// GetMember fetches one membership (SPEC-W45 K16 member lifecycle: role
// change/removal need the CURRENT role to scope the owner-only guard and to
// publish the old→new transition).
func (s *Store) GetMember(ctx context.Context, tenantID uuid.UUID, userID string) (Membership, error) {
	const q = `SELECT tenant_id, user_id, role FROM memberships WHERE tenant_id = $1 AND user_id = $2`
	var m Membership
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, tenantID, userID).Scan(&m.TenantID, &m.UserID, &m.Role)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, fmt.Errorf("get member: %w", err)
	}
	return m, nil
}

// RemoveMember deletes one membership row (SPEC-W45 K16). ErrNotFound when
// the pair does not exist.
func (s *Store) RemoveMember(ctx context.Context, tenantID uuid.UUID, userID string) error {
	const q = `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, q, tenantID, userID)
		if err != nil {
			return fmt.Errorf("remove member: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// CountMembers returns the tenant's membership count (SPEC-W45 K18 plan
// member-limit gate in inviteMember).
func (s *Store) CountMembers(ctx context.Context, tenantID uuid.UUID) (int, error) {
	var n int
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE tenant_id = $1`, tenantID).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("count members: %w", err)
	}
	return n, nil
}

// SetTenantPlan updates tenants.plan (SPEC-W45 K18 PATCH
// /v1/tenants/{slug}/plan). Runs on the tenants pool (internal escape role —
// the tenants RLS policy is keyed on the row's own id). ErrNotFound for an
// unknown slug.
func (s *Store) SetTenantPlan(ctx context.Context, slug, plan string) error {
	tag, err := s.tenants.Exec(ctx, `UPDATE tenants SET plan = $2 WHERE slug = $1`, slug, plan)
	if err != nil {
		return fmt.Errorf("set tenant plan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Tenant API keys (SPEC-W45 K17)
// ---------------------------------------------------------------------------

// APIKey mirrors identity.tenant_api_keys. KeyHash is the SHA-256 hex of the
// full key (prefix.secret) and is NEVER serialised to API clients.
type APIKey struct {
	ID        uuid.UUID  `json:"id"`
	TenantID  uuid.UUID  `json:"tenant_id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	KeyHash   string     `json:"-"`
	Scopes    []string   `json:"scopes"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// CreateAPIKey inserts a key row (the caller generates prefix/hash and keeps
// the plaintext secret out of the store entirely).
func (s *Store) CreateAPIKey(ctx context.Context, k *APIKey) error {
	if k.ID == uuid.Nil {
		k.ID = uuid.New()
	}
	const q = `INSERT INTO tenant_api_keys (id, tenant_id, name, prefix, key_hash, scopes, created_by)
	           VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`
	return s.withTenant(ctx, k.TenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, q, k.ID, k.TenantID, k.Name, k.Prefix, k.KeyHash, k.Scopes, k.CreatedBy).
			Scan(&k.CreatedAt); err != nil {
			return fmt.Errorf("create api key: %w", err)
		}
		return nil
	})
}

// ListAPIKeys returns all keys of a tenant (revoked included — audit
// visibility), newest first. Hashes are selected only into the struct and
// never leave the service.
func (s *Store) ListAPIKeys(ctx context.Context, tenantID uuid.UUID) ([]APIKey, error) {
	const q = `SELECT id, tenant_id, name, prefix, key_hash, scopes, created_by, created_at, revoked_at
	           FROM tenant_api_keys WHERE tenant_id = $1 ORDER BY created_at DESC`
	var out []APIKey
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k APIKey
			if err := rows.Scan(&k.ID, &k.TenantID, &k.Name, &k.Prefix, &k.KeyHash,
				&k.Scopes, &k.CreatedBy, &k.CreatedAt, &k.RevokedAt); err != nil {
				return err
			}
			out = append(out, k)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	return out, nil
}

// RevokeAPIKey sets revoked_at (soft delete — the row stays for audit).
// ErrNotFound when the (tenant, id) pair is unknown or already revoked.
func (s *Store) RevokeAPIKey(ctx context.Context, tenantID, keyID uuid.UUID) error {
	const q = `UPDATE tenant_api_keys SET revoked_at = now()
	           WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL`
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, q, tenantID, keyID)
		if err != nil {
			return fmt.Errorf("revoke api key: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// GetAPIKeyByHash resolves an ACTIVE key by its SHA-256 hash and returns the
// key plus the owning tenant's slug (the /internal/api-keys/validate
// contract). Cross-tenant by design (the validator does not know the tenant
// upfront), so it runs on the internal pool with the RLS escape; the tenants
// join is covered by the same escape. ErrNotFound for unknown/revoked keys.
func (s *Store) GetAPIKeyByHash(ctx context.Context, keyHash string) (APIKey, string, error) {
	const q = `SELECT k.id, k.tenant_id, k.name, k.prefix, k.key_hash, k.scopes, k.created_by, k.created_at, k.revoked_at,
	                  t.slug
	           FROM tenant_api_keys k JOIN tenants t ON t.id = k.tenant_id
	           WHERE k.key_hash = $1 AND k.revoked_at IS NULL`
	var k APIKey
	var slug string
	err := s.tenants.QueryRow(ctx, q, keyHash).Scan(&k.ID, &k.TenantID, &k.Name, &k.Prefix, &k.KeyHash,
		&k.Scopes, &k.CreatedBy, &k.CreatedAt, &k.RevokedAt, &slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return k, "", ErrNotFound
	}
	if err != nil {
		return k, "", fmt.Errorf("get api key by hash: %w", err)
	}
	return k, slug, nil
}
