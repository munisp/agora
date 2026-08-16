// Package store provides Postgres persistence for the outbound webhook
// platform (Wave 5 #10): webhook_subscriptions (per-tenant endpoint +
// signing secret + event filter) and webhook_deliveries (one row per
// subscription×event attempt, driven by WebhookDeliveryWorkflow).
//
// The tables live in the `notifications` database (00-create-dbs.sql) and
// are bootstrapped idempotently here so upgrades need no manual migration.
//
// Tenant isolation (SQL-003): every table carries ENABLE + FORCE ROW LEVEL
// SECURITY with a tenant_isolation policy keyed on the app.tenant_id GUC
// (identity-service consent/apps store idiom). Tenant-scoped webhook
// queries run inside withTenant (`SET LOCAL app.tenant_id`) so isolation is
// enforced at the database layer even if a future caller forgets the WHERE
// clause. The policies deliberately treat an UNSET app.tenant_id as
// "no database-level scoping" (the pre-existing application-level
// filtering posture): several paths legitimately run without a single
// tenant context (UpdateDelivery by delivery id, the slug-keyed DND
// lookups, the global NCC 2442 import). Once a session sets app.tenant_id,
// cross-tenant rows are invisible and cross-tenant writes are rejected.
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
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_sub ON webhook_deliveries (sub_id, created_at DESC);
ALTER TABLE webhook_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_subscriptions FORCE ROW LEVEL SECURITY;
ALTER TABLE webhook_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_deliveries FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_policies
                   WHERE schemaname = current_schema()
                     AND tablename = 'webhook_subscriptions'
                     AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON webhook_subscriptions
            USING (CASE
                WHEN current_setting('app.tenant_id', true) IS NULL THEN true
                ELSE tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
            END);
    END IF;
    IF NOT EXISTS (SELECT FROM pg_policies
                   WHERE schemaname = current_schema()
                     AND tablename = 'webhook_deliveries'
                     AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON webhook_deliveries
            USING (CASE
                WHEN current_setting('app.tenant_id', true) IS NULL THEN true
                ELSE tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
            END);
    END IF;
END
$$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure webhook tables: %w", err)
	}
	return nil
}

// withTenant runs fn inside a transaction with `SET LOCAL app.tenant_id`
// applied (set_config(..., true): parameter binding keeps this
// injection-safe), so the tenant_isolation RLS policies scope every
// statement of fn to the given tenant (identity-service consent store
// idiom). tenantID is a string because civic_notifications.tenant_id is
// TEXT; the UUID-keyed tables receive tenantID.String().
func (s *Store) withTenant(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
	return s.withTenant(ctx, sub.TenantID.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, sub.ID, sub.TenantID, sub.TenantSlug, sub.URL, sub.Secret, sub.Events, sub.Active).
			Scan(&sub.CreatedAt)
	})
}

// ListSubscriptions returns a tenant's subscriptions, newest first.
func (s *Store) ListSubscriptions(ctx context.Context, tenantID uuid.UUID) ([]WebhookSubscription, error) {
	var out []WebhookSubscription
	err := s.withTenant(ctx, tenantID.String(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+subCols+` FROM webhook_subscriptions WHERE tenant_id=$1 ORDER BY created_at DESC`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			sub, err := scanSub(rows)
			if err != nil {
				return err
			}
			out = append(out, sub)
		}
		return rows.Err()
	})
	return out, err
}

// GetSubscription fetches one subscription scoped to a tenant.
func (s *Store) GetSubscription(ctx context.Context, tenantID, id uuid.UUID) (WebhookSubscription, error) {
	var sub WebhookSubscription
	err := s.withTenant(ctx, tenantID.String(), func(tx pgx.Tx) error {
		var err error
		sub, err = scanSub(tx.QueryRow(ctx,
			`SELECT `+subCols+` FROM webhook_subscriptions WHERE tenant_id=$1 AND id=$2`, tenantID, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return sub, ErrNotFound
	}
	return sub, err
}

// DeleteSubscription removes a subscription (deliveries cascade).
func (s *Store) DeleteSubscription(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.withTenant(ctx, tenantID.String(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM webhook_subscriptions WHERE tenant_id=$1 AND id=$2`, tenantID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ActiveSubscriptions returns all active subscriptions of a tenant; event
// matching happens in Go (webhooks.EventMatches) to keep wildcard rules in
// one tested place.
func (s *Store) ActiveSubscriptions(ctx context.Context, tenantID uuid.UUID) ([]WebhookSubscription, error) {
	var out []WebhookSubscription
	err := s.withTenant(ctx, tenantID.String(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+subCols+` FROM webhook_subscriptions WHERE tenant_id=$1 AND active ORDER BY created_at`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			sub, err := scanSub(rows)
			if err != nil {
				return err
			}
			out = append(out, sub)
		}
		return rows.Err()
	})
	return out, err
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
	return s.withTenant(ctx, d.TenantID.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, d.ID, d.SubID, d.TenantID, d.EventID, d.EventType).
			Scan(&d.CreatedAt, &d.UpdatedAt)
	})
}

// ListDeliveries returns a subscription's deliveries (tenant-scoped),
// newest first.
func (s *Store) ListDeliveries(ctx context.Context, tenantID, subID uuid.UUID, limit int) ([]WebhookDelivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []WebhookDelivery
	err := s.withTenant(ctx, tenantID.String(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+deliveryCols+` FROM webhook_deliveries
			 WHERE tenant_id=$1 AND sub_id=$2 ORDER BY created_at DESC LIMIT $3`, tenantID, subID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDelivery(rows)
			if err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateDelivery records the outcome of one delivery attempt (called by the
// UpdateWebhookDelivery activity after every attempt: retrying → schedules
// the next timer, delivered/dlq are terminal). It is keyed by delivery id
// only (the workflow carries no tenant id), so it runs without a tenant
// context — the tenant_isolation policy's unset-context clause keeps the
// application-level posture for this path (see package header).
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
