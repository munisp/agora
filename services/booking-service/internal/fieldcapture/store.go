package fieldcapture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound mirrors store.ErrNotFound (404 mapping at the API).
var ErrNotFound = errors.New("not found")

// Store persists the field_captures idempotency anchors and the
// field_checkins history. Same packaging idiom as the W14
// referrals.PayoutStore: NewStore wraps an existing pool (tests), DialStore
// opens a small dedicated pool (main wiring path). maxConns 4: capture
// flushes are low-QPS bursts after connectivity restores.
type Store struct {
	pool    *pgxpool.Pool
	ownPool bool
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

// ensureSchema bootstraps field_captures + field_checkins idempotently
// (same pattern as store.ensureLeadTables, SPEC-W13): RLS enabled + forced
// with the tenant_isolation policy, guarded by a pg_policies check.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureSchema(ctx context.Context) error {
	const ddl = `
-- Idempotency anchors (contract §4): one row per (tenant_id, client_id);
-- the pair IS the "field_capture:{client_id}" dedupe key. The applied
-- outcome (lead_id / checkin_id / error) is stored on the row so a replay
-- returns the ORIGINAL result without re-applying side effects.
CREATE TABLE IF NOT EXISTS field_captures (
    tenant_id   UUID NOT NULL,
    client_id   TEXT NOT NULL,
    kind        TEXT NOT NULL
                CHECK (kind IN ('lead_capture','checkin')),
    payload     JSONB NOT NULL DEFAULT '{}',
    gps         JSONB,
    captured_at TIMESTAMPTZ,
    status      TEXT NOT NULL DEFAULT 'processing'
                CHECK (status IN ('processing','applied','error')),
    result      JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, client_id)
);
-- SPEC-W32 WS-A: the kind enum gains 'civic_report'. CREATE TABLE IF NOT
-- EXISTS never updates an existing CHECK constraint, so it is rebuilt
-- idempotently (drop + re-add with the full enum).
ALTER TABLE field_captures DROP CONSTRAINT IF EXISTS field_captures_kind_check;
ALTER TABLE field_captures ADD CONSTRAINT field_captures_kind_check
    CHECK (kind IN ('lead_capture','checkin','civic_report'));
ALTER TABLE field_captures ENABLE ROW LEVEL SECURITY;
ALTER TABLE field_captures FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'field_captures' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON field_captures
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

-- Geo check-in history (contract §4 fallback: the W8 contact_locations
-- store is a last-known upsert with no history, so check-ins live here).
CREATE TABLE IF NOT EXISTS field_checkins (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    contact_id  UUID,
    lat         DOUBLE PRECISION,
    lng         DOUBLE PRECISION,
    accuracy_m  DOUBLE PRECISION,
    note        TEXT NOT NULL DEFAULT '',
    payload     JSONB NOT NULL DEFAULT '{}',
    captured_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_field_checkins_contact ON field_checkins (tenant_id, contact_id, captured_at);
ALTER TABLE field_checkins ENABLE ROW LEVEL SECURITY;
ALTER TABLE field_checkins FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'field_checkins' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON field_checkins
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure field capture tables: %w", err)
	}
	return nil
}

// withTenant mirrors store.Store.withTenant: every tenant-scoped statement
// runs in a transaction with SET LOCAL app.tenant_id so the RLS policies
// scope it to the tenant (parameter-binding-safe set_config).
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

// Anchor inserts the idempotency row for one capture item in 'processing'
// state. fresh=false means the (tenant_id, client_id) anchor already
// existed — a replay; the stored status/result are loaded into the
// out-params so the caller can return the original outcome unchanged.
func (s *Store) Anchor(ctx context.Context, tenantID uuid.UUID, it CaptureItem) (fresh bool, status string, result json.RawMessage, err error) {
	var gps any
	if it.GPS != nil {
		raw, mErr := json.Marshal(it.GPS)
		if mErr != nil {
			return false, "", nil, mErr
		}
		gps = raw
	}
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		scanErr := tx.QueryRow(ctx,
			`INSERT INTO field_captures (tenant_id, client_id, kind, payload, gps, captured_at, status)
			 VALUES ($1,$2,$3,$4,$5,$6,'processing')
			 ON CONFLICT (tenant_id, client_id) DO NOTHING
			 RETURNING status, result`,
			tenantID, it.ClientID, it.Kind, it.Payload, gps, it.CapturedAt).
			Scan(&status, &result)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			fresh = false
			return tx.QueryRow(ctx,
				`SELECT status, result FROM field_captures WHERE tenant_id=$1 AND client_id=$2`,
				tenantID, it.ClientID).Scan(&status, &result)
		}
		if scanErr != nil {
			return fmt.Errorf("anchor field capture: %w", scanErr)
		}
		fresh = true
		return nil
	})
	return fresh, status, result, err
}

// Release deletes a 'processing' anchor after a transient apply failure so
// the client's retry re-applies cleanly (anchors that reached a terminal
// status are never released).
func (s *Store) Release(ctx context.Context, tenantID uuid.UUID, clientID string) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM field_captures WHERE tenant_id=$1 AND client_id=$2 AND status='processing'`,
			tenantID, clientID)
		return err
	})
}

// Resolve records the terminal outcome of a fresh anchor (status
// applied|error + the result JSON returned to future replays).
func (s *Store) Resolve(ctx context.Context, tenantID uuid.UUID, clientID, status string, result ItemResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE field_captures SET status=$3, result=$4 WHERE tenant_id=$1 AND client_id=$2`,
			tenantID, clientID, status, raw)
		return err
	})
}

const checkinCols = `id, tenant_id, contact_id, lat, lng, accuracy_m, note, payload, captured_at, created_at`

// InsertCheckin appends one geo check-in event.
func (s *Store) InsertCheckin(ctx context.Context, c *Checkin) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if len(c.Payload) == 0 {
		c.Payload = json.RawMessage(`{}`)
	}
	const q = `INSERT INTO field_checkins (` + checkinCols + `)
		           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now())
		           RETURNING created_at`
	return s.withTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, c.ID, c.TenantID, c.ContactID, c.Lat, c.Lng,
			c.AccuracyM, c.Note, c.Payload, c.CapturedAt).Scan(&c.CreatedAt)
	})
}

// ListCheckins returns the tenant's check-in history (newest captured
// first), optionally narrowed to one contact. Backs future admin reads;
// exercised by the store tests today.
func (s *Store) ListCheckins(ctx context.Context, tenantID uuid.UUID, contactID *uuid.UUID) ([]Checkin, error) {
	q := `SELECT ` + checkinCols + ` FROM field_checkins WHERE tenant_id=$1`
	args := []any{tenantID}
	if contactID != nil {
		q += ` AND contact_id=$2`
		args = append(args, *contactID)
	}
	q += ` ORDER BY captured_at DESC NULLS LAST, created_at DESC LIMIT 500`
	out := []Checkin{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Checkin
			if err := rows.Scan(&c.ID, &c.TenantID, &c.ContactID, &c.Lat, &c.Lng,
				&c.AccuracyM, &c.Note, &c.Payload, &c.CapturedAt, &c.CreatedAt); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}
