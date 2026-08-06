// Package store provides Postgres persistence for the outbound webhook
// platform (Wave 5 #10): webhook_subscriptions (per-tenant endpoint +
// signing secret + event filter) and webhook_deliveries (one row per
// subscription×event attempt, driven by WebhookDeliveryWorkflow).
//
// The tables live in the `notifications` database (00-create-dbs.sql) and
// are bootstrapped idempotently here so upgrades need no manual migration.
// Tenant isolation is application-level (every query filters tenant_id);
// unlike the booking DB there are no FORCE RLS policies on this database.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Delivery statuses.
const (
	StatusPending   = "pending"
	StatusRetrying  = "retrying"
	StatusDelivered = "delivered"
	StatusDLQ       = "dlq"
)

// Store wraps a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New connects to Postgres and ensures the webhook tables exist.
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
	if err := s.ensureSchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	// SPEC-W12 Agent B: DND registry (dnd.go), same bootstrap pattern.
	if err := s.ensureDNDSchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	// SPEC-W32 WS-B: civic delivery ledger (civic_ledger.go), same pattern.
	if err := s.ensureCivicLedgerSchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// ensureSchema bootstraps the webhook tables idempotently.
func (s *Store) ensureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS webhook_subscriptions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    tenant_slug TEXT NOT NULL DEFAULT '',
    url         TEXT NOT NULL,
    secret      TEXT NOT NULL DEFAULT '',
    events      TEXT[] NOT NULL DEFAULT '{}',
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_webhook_subs_tenant ON webhook_subscriptions (tenant_id) WHERE active;
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sub_id           UUID NOT NULL REFERENCES webhook_subscriptions(id) ON DELETE CASCADE,
    tenant_id        UUID NOT NULL,
    event_id         TEXT NOT NULL DEFAULT '',
    event_type       TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','retrying','delivered','dlq')),
    attempts         INTEGER NOT NULL DEFAULT 0,
    last_status_code INTEGER,
    next_retry_at    TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_sub ON webhook_deliveries (sub_id, created_at DESC);`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure webhook tables: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Subscriptions
// ---------------------------------------------------------------------------

// WebhookSubscription mirrors webhook_subscriptions.
type WebhookSubscription struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenant_id"`
	TenantSlug string    `json:"tenant_slug"`
	URL        string    `json:"url"`
	Secret     string    `json:"-"` // never serialized after creation
	Events     []string  `json:"events"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"created_at"`
}

const subCols = `id, tenant_id, tenant_slug, url, secret, events, active, created_at`

func scanSub(row pgx.Row) (WebhookSubscription, error) {
	var s WebhookSubscription
	err := row.Scan(&s.ID, &s.TenantID, &s.TenantSlug, &s.URL, &s.Secret, &s.Events, &s.Active, &s.CreatedAt)
	return s, err
}

// CreateSubscription inserts a subscription, generating id + created_at.
func (s *Store) CreateSubscription(ctx context.Context, sub *WebhookSubscription) error {
	if sub.ID == uuid.Nil {
		sub.ID = uuid.New()
	}
	sub.Active = true
	const q = `INSERT INTO webhook_subscriptions (id, tenant_id, tenant_slug, url, secret, events, active)
	           VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`
	return s.pool.QueryRow(ctx, q, sub.ID, sub.TenantID, sub.TenantSlug, sub.URL, sub.Secret, sub.Events, sub.Active).
		Scan(&sub.CreatedAt)
}

// ListSubscriptions returns a tenant's subscriptions, newest first.
func (s *Store) ListSubscriptions(ctx context.Context, tenantID uuid.UUID) ([]WebhookSubscription, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+subCols+` FROM webhook_subscriptions WHERE tenant_id=$1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebhookSubscription
	for rows.Next() {
		sub, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// GetSubscription fetches one subscription scoped to a tenant.
func (s *Store) GetSubscription(ctx context.Context, tenantID, id uuid.UUID) (WebhookSubscription, error) {
	sub, err := scanSub(s.pool.QueryRow(ctx,
		`SELECT `+subCols+` FROM webhook_subscriptions WHERE tenant_id=$1 AND id=$2`, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return sub, ErrNotFound
	}
	return sub, err
}

// DeleteSubscription removes a subscription (deliveries cascade).
func (s *Store) DeleteSubscription(ctx context.Context, tenantID, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM webhook_subscriptions WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ActiveSubscriptions returns all active subscriptions of a tenant; event
// matching happens in Go (webhooks.EventMatches) to keep wildcard rules in
// one tested place.
func (s *Store) ActiveSubscriptions(ctx context.Context, tenantID uuid.UUID) ([]WebhookSubscription, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+subCols+` FROM webhook_subscriptions WHERE tenant_id=$1 AND active ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebhookSubscription
	for rows.Next() {
		sub, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Deliveries
// ---------------------------------------------------------------------------

// WebhookDelivery mirrors webhook_deliveries.
type WebhookDelivery struct {
	ID             uuid.UUID  `json:"id"`
	SubID          uuid.UUID  `json:"sub_id"`
	TenantID       uuid.UUID  `json:"tenant_id"`
	EventID        string     `json:"event_id"`
	EventType      string     `json:"event_type"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	LastStatusCode *int       `json:"last_status_code,omitempty"`
	NextRetryAt    *time.Time `json:"next_retry_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

const deliveryCols = `id, sub_id, tenant_id, event_id, event_type, status, attempts, last_status_code, next_retry_at, created_at, updated_at`

func scanDelivery(row pgx.Row) (WebhookDelivery, error) {
	var d WebhookDelivery
	err := row.Scan(&d.ID, &d.SubID, &d.TenantID, &d.EventID, &d.EventType, &d.Status,
		&d.Attempts, &d.LastStatusCode, &d.NextRetryAt, &d.CreatedAt, &d.UpdatedAt)
	return d, err
}

// CreateDelivery inserts a pending delivery row.
func (s *Store) CreateDelivery(ctx context.Context, d *WebhookDelivery) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	d.Status = StatusPending
	const q = `INSERT INTO webhook_deliveries (id, sub_id, tenant_id, event_id, event_type, status)
	           VALUES ($1,$2,$3,$4,$5,'pending') RETURNING created_at, updated_at`
	return s.pool.QueryRow(ctx, q, d.ID, d.SubID, d.TenantID, d.EventID, d.EventType).
		Scan(&d.CreatedAt, &d.UpdatedAt)
}

// ListDeliveries returns a subscription's deliveries (tenant-scoped),
// newest first.
func (s *Store) ListDeliveries(ctx context.Context, tenantID, subID uuid.UUID, limit int) ([]WebhookDelivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+deliveryCols+` FROM webhook_deliveries
		 WHERE tenant_id=$1 AND sub_id=$2 ORDER BY created_at DESC LIMIT $3`, tenantID, subID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebhookDelivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDelivery records the outcome of one delivery attempt (called by the
// UpdateWebhookDelivery activity after every attempt: retrying → schedules
// the next timer, delivered/dlq are terminal).
func (s *Store) UpdateDelivery(ctx context.Context, id uuid.UUID, status string, attempts int, statusCode *int, nextRetryAt *time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE webhook_deliveries
		 SET status=$2, attempts=$3, last_status_code=$4, next_retry_at=$5, updated_at=now()
		 WHERE id=$1`, id, status, attempts, statusCode, nextRetryAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
