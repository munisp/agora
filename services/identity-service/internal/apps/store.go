package apps

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a tenant_apps row does not exist (app not
// provisioned for the tenant).
var ErrNotFound = errors.New("tenant app not found")

// ErrUnknownApp is returned when an app_id is not in the platform_apps
// catalog.
var ErrUnknownApp = errors.New("unknown app")

// Repository is the persistence contract the handlers depend on. *Store is
// the Postgres implementation; tests substitute an in-memory fake.
type Repository interface {
	// ListCatalog returns every platform_apps row, ordered by app_id.
	ListCatalog(ctx context.Context) ([]PlatformApp, error)
	// GetApp returns one catalog row or ErrUnknownApp.
	GetApp(ctx context.Context, appID string) (PlatformApp, error)
	// ListTenantApps returns the catalog LEFT JOIN tenant_apps view: every
	// catalog app with the tenant's status (not_provisioned + {} config when
	// no row exists).
	ListTenantApps(ctx context.Context, tenantID uuid.UUID) ([]TenantAppView, error)
	// Provision upserts (tenant, app) to status enabled. Idempotent: a replay
	// keeps the original provisioned_at/provisioned_by. Returns the row, the
	// previous status ("" when newly provisioned) and whether the row was
	// created by this call.
	Provision(ctx context.Context, tenantID uuid.UUID, appID, actor string) (TenantApp, AppStatus, bool, error)
	// Patch applies a partial update: nil status / nil config leave the stored
	// value untouched. Config, when given, REPLACES the whole config document.
	// ErrNotFound when the app was never provisioned.
	Patch(ctx context.Context, tenantID uuid.UUID, appID string, status *AppStatus, config []byte) (TenantApp, error)
	// Disable soft-deletes: status -> disabled, row retained for audit.
	// ErrNotFound when the app was never provisioned.
	Disable(ctx context.Context, tenantID uuid.UUID, appID string) (TenantApp, error)
	// GetTenantApp returns one tenant_apps row or ErrNotFound.
	GetTenantApp(ctx context.Context, tenantID uuid.UUID, appID string) (TenantApp, error)
	// Ping checks database liveness.
	Ping(ctx context.Context) error
}

// Store is the Postgres-backed Repository.
//
// RLS model (mirrors the consent store idiom):
//   - tenant_apps carries ENABLE + FORCE ROW LEVEL SECURITY with the
//     tenant_isolation policy; every tenant-scoped query runs inside
//     withTenant (`SET LOCAL app.tenant_id`) so isolation is enforced at the
//     database layer even if a future caller forgets the WHERE clause.
//   - platform_apps deliberately has NO RLS: it is global reference data
//     (the app catalog — identical for every tenant, zero tenant data).
//     Forcing RLS on it would hide the catalog behind a tenant context and
//     break the "every app shows status|not_provisioned" LEFT JOIN contract,
//     so catalog reads run outside withTenant by design.
type Store struct {
	pool *pgxpool.Pool
}

// New connects to Postgres, verifies connectivity and bootstraps the
// platform_apps + tenant_apps tables and the tenant_isolation RLS policy
// (idempotent; same bootstrap role as the consent store).
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
		return nil, fmt.Errorf("bootstrap apps schema: %w", err)
	}
	return s, nil
}

// bootstrap creates the registry tables (SPEC-W18 §1).
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant (consent store idiom).
func (s *Store) bootstrap(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS platform_apps (
    app_id            TEXT PRIMARY KEY,
    name              TEXT NOT NULL,
    version           TEXT NOT NULL DEFAULT '',
    description       TEXT NOT NULL DEFAULT '',
    category          TEXT NOT NULL DEFAULT '',
    icon              TEXT NOT NULL DEFAULT '',
    nav_route         TEXT NOT NULL DEFAULT '',
    required_perms    TEXT[] NOT NULL DEFAULT '{}',
    default_plan_tier TEXT NOT NULL DEFAULT 'starter',
    backend_note      TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS tenant_apps (
    tenant_id      UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    app_id         TEXT NOT NULL REFERENCES platform_apps (app_id) ON DELETE CASCADE,
    status         TEXT NOT NULL DEFAULT 'enabled'
                   CHECK (status IN ('enabled','disabled','suspended')),
    config         JSONB NOT NULL DEFAULT '{}',
    provisioned_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    provisioned_by TEXT NOT NULL DEFAULT '',
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, app_id)
);
ALTER TABLE tenant_apps ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_apps FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_policies
                   WHERE schemaname = current_schema()
                     AND tablename = 'tenant_apps'
                     AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON tenant_apps
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END
$$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure apps tables: %w", err)
	}
	return nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks database liveness.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// withTenant runs fn inside a transaction with `SET LOCAL app.tenant_id`
// applied (set_config(..., true): parameter binding keeps this
// injection-safe), so the tenant_isolation RLS policy scopes every statement
// of fn to the given tenant (consent store idiom).
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

const platformCols = `app_id, name, version, description, category, icon, nav_route, required_perms, default_plan_tier, backend_note, created_at`

func scanPlatformApp(row pgx.Row) (PlatformApp, error) {
	var a PlatformApp
	err := row.Scan(&a.AppID, &a.Name, &a.Version, &a.Description, &a.Category,
		&a.Icon, &a.NavRoute, &a.RequiredPerms, &a.DefaultPlanTier, &a.BackendNote, &a.CreatedAt)
	return a, err
}

const tenantCols = `tenant_id, app_id, status, config, provisioned_at, provisioned_by, updated_at`

func scanTenantApp(row pgx.Row) (TenantApp, error) {
	var t TenantApp
	err := row.Scan(&t.TenantID, &t.AppID, &t.Status, &t.Config,
		&t.ProvisionedAt, &t.ProvisionedBy, &t.UpdatedAt)
	return t, err
}

// EnsureCatalog idempotently upserts the embedded catalog.yaml rows into
// platform_apps (boot path, SPEC-W18 §3). Content fields refresh on every
// boot; created_at of existing rows is preserved. Returns the row count.
func (s *Store) EnsureCatalog(ctx context.Context, apps []PlatformApp) (int, error) {
	const q = `INSERT INTO platform_apps (` + platformCols + `)
		           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now())
		           ON CONFLICT (app_id) DO UPDATE
		           SET name             = EXCLUDED.name,
		               version          = EXCLUDED.version,
		               description      = EXCLUDED.description,
		               category         = EXCLUDED.category,
		               icon             = EXCLUDED.icon,
		               nav_route        = EXCLUDED.nav_route,
		               required_perms   = EXCLUDED.required_perms,
		               default_plan_tier = EXCLUDED.default_plan_tier,
		               backend_note     = EXCLUDED.backend_note`
	// platform_apps is global reference data (no RLS) — a plain pool tx, no
	// tenant context by design (see Store doc).
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, a := range apps {
		if a.DefaultPlanTier == "" {
			a.DefaultPlanTier = "starter"
		}
		if a.RequiredPerms == nil {
			a.RequiredPerms = []string{}
		}
		if _, err := tx.Exec(ctx, q, a.AppID, a.Name, a.Version, a.Description,
			a.Category, a.Icon, a.NavRoute, a.RequiredPerms, a.DefaultPlanTier, a.BackendNote); err != nil {
			return 0, fmt.Errorf("upsert catalog app %q: %w", a.AppID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(apps), nil
}

// ListCatalog implements Repository.
func (s *Store) ListCatalog(ctx context.Context) ([]PlatformApp, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+platformCols+` FROM platform_apps ORDER BY app_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlatformApp{}
	for rows.Next() {
		a, err := scanPlatformApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetApp implements Repository.
func (s *Store) GetApp(ctx context.Context, appID string) (PlatformApp, error) {
	a, err := scanPlatformApp(s.pool.QueryRow(ctx,
		`SELECT `+platformCols+` FROM platform_apps WHERE app_id = $1`, appID))
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrUnknownApp
	}
	return a, err
}

// ListTenantApps implements Repository (catalog LEFT JOIN tenant_apps).
func (s *Store) ListTenantApps(ctx context.Context, tenantID uuid.UUID) ([]TenantAppView, error) {
	const q = `SELECT p.app_id, p.name, p.version, p.description, p.category,
		              p.icon, p.nav_route, p.required_perms, p.default_plan_tier, p.backend_note, p.created_at,
		              ta.status, ta.config, ta.provisioned_at, ta.provisioned_by, ta.updated_at
		           FROM platform_apps p
		           LEFT JOIN tenant_apps ta
		             ON ta.app_id = p.app_id AND ta.tenant_id = $1
		           ORDER BY p.app_id`
	out := []TenantAppView{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v TenantAppView
			var status *AppStatus
			var config []byte
			var provAt, updAt *time.Time
			var provBy *string
			if err := rows.Scan(&v.AppID, &v.Name, &v.Version, &v.Description, &v.Category,
				&v.Icon, &v.NavRoute, &v.RequiredPerms, &v.DefaultPlanTier, &v.BackendNote, &v.CreatedAt,
				&status, &config, &provAt, &provBy, &updAt); err != nil {
				return err
			}
			if status == nil {
				v.Status = StatusNotProvisioned
				v.Config = []byte(`{}`)
			} else {
				v.Status = *status
				v.Config = config
				v.ProvisionedAt = provAt
				v.UpdatedAt = updAt
				if provBy != nil {
					v.ProvisionedBy = *provBy
				}
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

// Provision implements Repository (idempotent provision+enable upsert).
func (s *Store) Provision(ctx context.Context, tenantID uuid.UUID, appID, actor string) (TenantApp, AppStatus, bool, error) {
	var out TenantApp
	var prev AppStatus
	created := false
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		// Read the previous state first so the handler can pick the right
		// lifecycle event (AppProvisioned vs AppStatusChanged) and so a replay
		// keeps the original provisioned_at/provisioned_by.
		var err error
		prev, err = currentStatus(ctx, tx, tenantID, appID)
		if err != nil {
			return err
		}
		if prev == "" {
			created = true
			out, err = scanTenantApp(tx.QueryRow(ctx,
				`INSERT INTO tenant_apps (`+tenantCols+`)
				 VALUES ($1,$2,'enabled','{}',now(),$3,now())
				 RETURNING `+tenantCols,
				tenantID, appID, actor))
			if err != nil {
				return fmt.Errorf("provision app: %w", err)
			}
			return nil
		}
		// Replay / re-enable: status -> enabled; provisioned_* first-wins.
		out, err = scanTenantApp(tx.QueryRow(ctx,
			`UPDATE tenant_apps SET status = 'enabled', updated_at = now()
			 WHERE tenant_id = $1 AND app_id = $2
			 RETURNING `+tenantCols, tenantID, appID))
		return err
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return out, prev, false, ErrUnknownApp // FK to platform_apps
		}
		return out, prev, false, err
	}
	return out, prev, created, nil
}

// currentStatus returns the stored status of (tenant, app) or "" when no row.
func currentStatus(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, appID string) (AppStatus, error) {
	var st AppStatus
	err := tx.QueryRow(ctx,
		`SELECT status FROM tenant_apps WHERE tenant_id = $1 AND app_id = $2 FOR UPDATE`,
		tenantID, appID).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return st, err
}

// Patch implements Repository (partial {status?, config?} update).
func (s *Store) Patch(ctx context.Context, tenantID uuid.UUID, appID string, status *AppStatus, config []byte) (TenantApp, error) {
	var out TenantApp
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		// COALESCE keeps the stored column when the patch field is NULL
		// (partial semantics); config, when present, replaces the document.
		out, err = scanTenantApp(tx.QueryRow(ctx,
			`UPDATE tenant_apps
			 SET status = COALESCE($3::text, status),
			     config = COALESCE($4::jsonb, config),
			     updated_at = now()
			 WHERE tenant_id = $1 AND app_id = $2
			 RETURNING `+tenantCols,
			tenantID, appID, status, config))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	return out, err
}

// Disable implements Repository (soft delete: status -> disabled, row kept).
func (s *Store) Disable(ctx context.Context, tenantID uuid.UUID, appID string) (TenantApp, error) {
	var out TenantApp
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanTenantApp(tx.QueryRow(ctx,
			`UPDATE tenant_apps SET status = 'disabled', updated_at = now()
			 WHERE tenant_id = $1 AND app_id = $2
			 RETURNING `+tenantCols, tenantID, appID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	return out, err
}

// GetTenantApp implements Repository.
func (s *Store) GetTenantApp(ctx context.Context, tenantID uuid.UUID, appID string) (TenantApp, error) {
	var out TenantApp
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanTenantApp(tx.QueryRow(ctx,
			`SELECT `+tenantCols+` FROM tenant_apps WHERE tenant_id = $1 AND app_id = $2`,
			tenantID, appID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	return out, err
}
