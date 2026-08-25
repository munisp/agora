// Package syncmap persists the OpenDesk <-> Twenty id mapping in the
// crm_sync Postgres database (SPEC-CRM §B). The table is bootstrapped
// idempotently on startup.
package syncmap

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ddl is the idempotent bootstrap statement (SPEC-CRM §B schema). The ALTER
// adds last_synced_at (reverse-sync echo suppression: when the forward syncer
// touches a person mapping, inbound webhooks for that person are suppressed
// for a short window); the index backs the twenty_id reverse lookups.
// webhook_events_seen (SPEC-W43 K-13): replay dedupe for the Twenty webhook
// intake — a redelivered event id inside the 24h window is ignored instead
// of re-emitted.
const ddl = `CREATE TABLE IF NOT EXISTS sync_map (
	id SERIAL PRIMARY KEY,
	kind TEXT NOT NULL,
	opendesk_id TEXT NOT NULL,
	twenty_id TEXT NOT NULL,
	tenant_id UUID,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (kind, opendesk_id, tenant_id)
);
ALTER TABLE sync_map ADD COLUMN IF NOT EXISTS last_synced_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS sync_map_twenty_idx ON sync_map (kind, twenty_id);
CREATE TABLE IF NOT EXISTS webhook_events_seen (
	event_id TEXT PRIMARY KEY,
	seen_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS webhook_events_seen_seen_at_idx ON webhook_events_seen (seen_at)`

// Store wraps the pgx pool.
type Store struct {
	pool *pgxpool.Pool
}

// Mapping is one sync_map row.
type Mapping struct {
	ID           int64
	Kind         string
	OpenDeskID   string
	TwentyID     string
	TenantID     *uuid.UUID
	UpdatedAt    time.Time
	LastSyncedAt *time.Time // set by forward-sync Put; nil for rows predating the column
}

// ErrNotFound is returned when no mapping exists.
var ErrNotFound = errors.New("sync_map: mapping not found")

// KindBookingContact maps a booking's OpenDesk id to the Twenty person id of
// its contact (written alongside the kind=booking task mapping). It lets the
// /v1/tasks helper resolve "the person of booking X" without a Twenty query.
const KindBookingContact = "booking_contact"

// KindContactPhone maps a contact phone number (as confirmed by the caller
// during an AI voice session) to the Twenty person id. Written alongside the
// kind=contact mapping at booking sync; read by the SessionEnded call-quality
// note path when the Twenty people lookup by phone misses (e.g. the person
// record's phone formatting differs from the confirmed number).
const KindContactPhone = "contact_phone"

// normTenant maps nil to uuid.Nil (stored as a regular value, never NULL).
func normTenant(t *uuid.UUID) uuid.UUID {
	if t == nil {
		return uuid.Nil
	}
	return *t
}

// New connects and bootstraps the schema (idempotent).
func New(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if _, err := pool.Exec(ctx, ddl); err != nil {
		pool.Close()
		return nil, fmt.Errorf("bootstrap sync_map: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping verifies DB liveness (used by /healthz).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

const mappingCols = `id, kind, opendesk_id, twenty_id, tenant_id, updated_at, last_synced_at`

func (s *Store) scanMapping(row pgx.Row) (Mapping, error) {
	var m Mapping
	err := row.Scan(&m.ID, &m.Kind, &m.OpenDeskID, &m.TwentyID, &m.TenantID, &m.UpdatedAt, &m.LastSyncedAt)
	return m, err
}

// Get looks up a mapping by (kind, opendesk_id, tenant_id). A nil tenantID
// is normalized to uuid.Nil so the UNIQUE constraint dedupes correctly
// (Postgres treats NULLs as distinct, which would break ON CONFLICT).
func (s *Store) Get(ctx context.Context, kind, opendeskID string, tenantID *uuid.UUID) (Mapping, error) {
	m, err := s.scanMapping(s.pool.QueryRow(ctx,
		`SELECT `+mappingCols+` FROM sync_map
		     WHERE kind = $1 AND opendesk_id = $2 AND tenant_id = $3`,
		kind, opendeskID, normTenant(tenantID)))
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, fmt.Errorf("sync_map get: %w", err)
	}
	return m, nil
}

// GetByTwentyID is the reverse lookup used by the Twenty -> OpenDesk sync:
// find the (single) mapping of a kind pointing at a Twenty record — e.g.
// kind=contact by person id, kind=booking_task by task id, kind=tenant by
// company id. Returns ErrNotFound when unmapped.
func (s *Store) GetByTwentyID(ctx context.Context, kind, twentyID string) (Mapping, error) {
	m, err := s.scanMapping(s.pool.QueryRow(ctx,
		`SELECT `+mappingCols+` FROM sync_map
		     WHERE kind = $1 AND twenty_id = $2
		     ORDER BY updated_at DESC LIMIT 1`, kind, twentyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, fmt.Errorf("sync_map get by twenty_id: %w", err)
	}
	return m, nil
}

// DeleteByTwentyID removes every mapping pointing at a Twenty record
// (GDPR erasure cleanup after the person is deleted in Twenty, SPEC-W3 §2).
// Returns rows removed.
func (s *Store) DeleteByTwentyID(ctx context.Context, twentyID string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sync_map WHERE twenty_id = $1`, twentyID)
	if err != nil {
		return 0, fmt.Errorf("sync_map delete by twenty_id: %w", err)
	}
	return tag.RowsAffected(), nil
}

// MarkWebhookSeen records a Twenty webhook event id (SPEC-W43 K-13 replay
// dedupe). It returns true when the id is NEW (first delivery) and false
// when it was already seen — the caller then answers 200 "ignored" without
// re-publishing. Rows older than 24h are pruned in the same call (the
// replay window), so a same-id delivery a day later is processed again.
func (s *Store) MarkWebhookSeen(ctx context.Context, eventID string) (bool, error) {
	if eventID == "" {
		return true, nil // no id to dedupe on — process (fail-open is impossible to dedupe anyway)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("webhook seen tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var inserted bool
	if err := tx.QueryRow(ctx,
		`INSERT INTO webhook_events_seen (event_id) VALUES ($1)
		 ON CONFLICT (event_id) DO NOTHING
		 RETURNING true`, eventID).Scan(&inserted); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			inserted = false // duplicate
		} else {
			return false, fmt.Errorf("webhook seen insert: %w", err)
		}
	}
	// 24h replay-window prune (indexed; webhook volume is low).
	if _, err := tx.Exec(ctx,
		`DELETE FROM webhook_events_seen WHERE seen_at < now() - interval '24 hours'`); err != nil {
		return false, fmt.Errorf("webhook seen prune: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("webhook seen commit: %w", err)
	}
	return inserted, nil
}

// Put inserts or updates (kind, opendesk_id, tenant_id) -> twenty_id. Every
// Put also stamps last_synced_at: the forward syncer is the only writer, and
// the reverse worker uses that timestamp for echo suppression.
func (s *Store) Put(ctx context.Context, kind, opendeskID, twentyID string, tenantID *uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO sync_map (kind, opendesk_id, twenty_id, tenant_id, updated_at, last_synced_at)
		VALUES ($1, $2, $3, $4, now(), now())
		ON CONFLICT (kind, opendesk_id, tenant_id)
		DO UPDATE SET twenty_id = EXCLUDED.twenty_id, updated_at = now(), last_synced_at = now()`,
		kind, opendeskID, twentyID, normTenant(tenantID))
	if err != nil {
		return fmt.Errorf("sync_map put: %w", err)
	}
	return nil
}
