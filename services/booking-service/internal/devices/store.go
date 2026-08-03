package devices

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists device tokens. Same packaging idiom as the W14
// referrals.PayoutStore: NewStore wraps an existing pool (tests), DialStore
// opens a small dedicated pool (main wiring path — the shared store.Store
// does not expose its pool). maxConns 4: device registration is a low-QPS
// path (app start / token refresh + the occasional notification fan-out
// lookup).
type Store struct {
	pool    *pgxpool.Pool
	ownPool bool // true when opened via DialStore
}

// NewStore wraps an existing pool and ensures the schema.
func NewStore(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	s := &Store{pool: pool}
	if err := s.ensureSchema(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// DialStore opens a small dedicated pool and ensures the schema.
func DialStore(ctx context.Context, databaseURL string) (*Store, error) {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}
	poolCfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	s, err := NewStore(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	s.ownPool = true
	return s, nil
}

// Close releases the pool when this store opened it.
func (s *Store) Close() {
	if s.ownPool {
		s.pool.Close()
	}
}

// ensureSchema bootstraps device_tokens idempotently (same pattern as
// store.ensureLeadTables, SPEC-W13): RLS enabled + forced with the
// tenant_isolation policy, guarded by a pg_policies existence check.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS device_tokens (
    tenant_id    UUID NOT NULL,
    contact_id   UUID,
    token        TEXT NOT NULL,
    platform     TEXT NOT NULL
                 CHECK (platform IN ('android','ios','web')),
    app          TEXT NOT NULL
                 CHECK (app IN ('admin','field')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, token)
);
CREATE INDEX IF NOT EXISTS idx_device_tokens_contact ON device_tokens (tenant_id, contact_id);
ALTER TABLE device_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE device_tokens FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'device_tokens' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON device_tokens
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure device_tokens table: %w", err)
	}
	return nil
}

// withTenant runs fn inside a transaction with `SET LOCAL app.tenant_id`
// applied (mirrors store.Store.withTenant — same parameter-binding-safe
// set_config call) so the RLS tenant_isolation policy scopes every
// statement of fn to the given tenant.
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

// ErrNotFound is returned when a row does not exist (mirrors
// store.ErrNotFound so httpapi can map both to 404).
var ErrNotFound = errors.New("not found")

const deviceCols = `tenant_id, contact_id, token, platform, app, created_at, last_seen_at`

func scanDevice(row pgx.Row) (DeviceToken, error) {
	var d DeviceToken
	err := row.Scan(&d.TenantID, &d.ContactID, &d.Token, &d.Platform, &d.App,
		&d.CreatedAt, &d.LastSeenAt)
	return d, err
}

// Upsert registers (or re-registers) a device token: INSERT ... ON CONFLICT
// (tenant_id, token) DO UPDATE refreshes contact_id/platform/app and stamps
// last_seen_at, so the mobile client's re-registration on every FCM/APNs
// token refresh is idempotent. created=false on the refresh path.
func (s *Store) Upsert(ctx context.Context, d *DeviceToken) (created bool, err error) {
	const q = `INSERT INTO device_tokens (` + deviceCols + `)
		           VALUES ($1,$2,$3,$4,$5,now(),now())
		           ON CONFLICT (tenant_id, token) DO UPDATE
		             SET contact_id=EXCLUDED.contact_id,
		                 platform=EXCLUDED.platform,
		                 app=EXCLUDED.app,
		                 last_seen_at=now()
		           RETURNING created_at, last_seen_at, (xmax = 0)`
	err = s.withTenant(ctx, d.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, d.TenantID, d.ContactID, d.Token, d.Platform, d.App).
			Scan(&d.CreatedAt, &d.LastSeenAt, &created)
	})
	if err != nil {
		return false, fmt.Errorf("upsert device token: %w", err)
	}
	return created, nil
}

// Delete removes one device token scoped to a tenant (device unregistered
// / push permission revoked). Idempotent-shaped: missing rows → ErrNotFound
// (the API maps it to 404 like the sibling stores).
func (s *Store) Delete(ctx context.Context, tenantID uuid.UUID, token string) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM device_tokens WHERE tenant_id=$1 AND token=$2`, tenantID, token)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ListByContact returns all device tokens linked to one contact — the
// contract §1 lookup the notification-worker's SendPushNotification
// activity performs (GET /internal/devices?contact_id=).
func (s *Store) ListByContact(ctx context.Context, tenantID, contactID uuid.UUID) ([]DeviceToken, error) {
	const q = `SELECT ` + deviceCols + ` FROM device_tokens
		           WHERE tenant_id=$1 AND contact_id=$2
		           ORDER BY last_seen_at DESC LIMIT 100`
	return s.list(ctx, tenantID, q, tenantID, contactID)
}

// List returns the tenant's device tokens (newest-seen first) with optional
// platform/app filters ("" disables a filter). Backs GET /v1/devices.
func (s *Store) List(ctx context.Context, tenantID uuid.UUID, platform, app string) ([]DeviceToken, error) {
	q := `SELECT ` + deviceCols + ` FROM device_tokens WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if platform != "" {
		n++
		q += fmt.Sprintf(` AND platform=$%d`, n)
		args = append(args, platform)
	}
	if app != "" {
		n++
		q += fmt.Sprintf(` AND app=$%d`, n)
		args = append(args, app)
	}
	q += ` ORDER BY last_seen_at DESC LIMIT 500`
	return s.list(ctx, tenantID, q, args...)
}

// Touch refreshes last_seen_at for one token (app heartbeat). Not exposed
// as an endpoint in W16; kept for the notification-worker failure-sweep
// (token pruning decisions use last_seen_at).
func (s *Store) Touch(ctx context.Context, tenantID uuid.UUID, token string, at time.Time) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE device_tokens SET last_seen_at=$3 WHERE tenant_id=$1 AND token=$2`,
			tenantID, token, at.UTC())
		return err
	})
}

func (s *Store) list(ctx context.Context, tenantID uuid.UUID, q string, args ...any) ([]DeviceToken, error) {
	out := []DeviceToken{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDevice(rows)
			if err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}
