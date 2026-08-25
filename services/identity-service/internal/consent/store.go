package consent

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
	// When n > 0 the call ALSO enqueues one consent_events_outbox row in the
	// same transaction (SPEC-W43 I-04: the erasure events are durable —
	// the Relay publishes them).
	Erase(ctx context.Context, tenantID uuid.UUID, subject, purpose string) (int, error)
	// FetchUnsentOutbox returns unpublished outbox rows (oldest first).
	FetchUnsentOutbox(ctx context.Context, limit int) ([]OutboxEvent, error)
	// MarkOutboxSent marks an outbox row published.
	MarkOutboxSent(ctx context.Context, id int64) error
	// Ping checks database liveness.
	Ping(ctx context.Context) error
}

// OutboxEvent is one consent_events_outbox row — everything the Relay needs
// to rebuild both erasure CloudEvents (K4: ErasureRequested on the consent
// topic + PrivacyEraseRequested on opendesk.privacy.events).
type OutboxEvent struct {
	ID            int64
	TenantID      uuid.UUID
	DataSubjectID string
	Purpose       string
	ErasedRecords int
	Synthetic     bool
	CreatedAt     time.Time
}

// Store is the Postgres-backed Repository. RLS: every query runs inside a
// transaction with `SET LOCAL app.tenant_id` (booking-service withTenant
// idiom) so the FORCE ROW LEVEL SECURITY policy on consents enforces tenant
// isolation at the database layer.
//
// Outbox (SPEC-W43 I-04 / SPEC-W44 W-I): Erase writes a consent_events_outbox
// row in the SAME transaction as the tombstone; the Relay publishes from it.
// The relay sweeps across tenants, so outbox fetch/mark run on the internal
// pool (INTERNAL_DATABASE_URL — app_identity_internal escape role, billing
// 0002 idiom; aliases the main pool when unset).
type Store struct {
	pool     *pgxpool.Pool
	internal *pgxpool.Pool
}

// New connects to Postgres, verifies connectivity and bootstraps the
// consents table + RLS policy (idempotent; covers fresh and existing
// installs, same role as the identity store's bootstrap ALTERs). internalURL
// is optional (see Store docs).
func New(ctx context.Context, databaseURL string, internalURL ...string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	s := &Store{pool: pool, internal: pool}
	if len(internalURL) > 0 && internalURL[0] != "" && internalURL[0] != databaseURL {
		ipool, err := pgxpool.New(ctx, internalURL[0])
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("connect internal postgres: %w", err)
		}
		if err := ipool.Ping(ctx); err != nil {
			ipool.Close()
			pool.Close()
			return nil, fmt.Errorf("ping internal postgres: %w", err)
		}
		s.internal = ipool
	}
	if err := s.bootstrap(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("bootstrap consents schema: %w", err)
	}
	return s, nil
}

// bootstrap creates the consents table + consent_events_outbox with the
// tenant_isolation RLS policy of 02-identity-schema.sql.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant (booking-service
// ensureSitesTable idiom). SPEC-W43 I-03: statements tolerate
// insufficient_privilege (42501) with a WARN so the least-privilege
// app_identity role boots once the infra layer owns the DDL.
func (s *Store) bootstrap(ctx context.Context) error {
	stmts := []struct{ name, sql string }{
		{"ensure consents table", `
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
DROP POLICY IF EXISTS tenant_isolation ON consents;
CREATE POLICY tenant_isolation ON consents
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_identity_internal', 'member'))`},
		// SPEC-W43 I-04 / SPEC-W44 W-I-1+K4: durable erasure outbox. One row
		// per erasure call; the Relay publishes ErasureRequested (consent
		// topic) + PrivacyEraseRequested (opendesk.privacy.events) from it
		// and marks sent_at. Relay reads are cross-tenant => internal-role
		// escape in the policy.
		{"ensure consent_events_outbox", `
CREATE TABLE IF NOT EXISTS consent_events_outbox (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       UUID NOT NULL,
    data_subject_id TEXT NOT NULL,
    purpose         TEXT NOT NULL DEFAULT '',
    erased_records  INT NOT NULL DEFAULT 0,
    synthetic       BOOLEAN NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at         TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_consent_outbox_unsent
    ON consent_events_outbox (id) WHERE sent_at IS NULL;
ALTER TABLE consent_events_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE consent_events_outbox FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON consent_events_outbox;
CREATE POLICY tenant_isolation ON consent_events_outbox
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR pg_has_role(current_user, 'app_identity_internal', 'member'))`},
	}
	for _, st := range stmts {
		if _, err := s.pool.Exec(ctx, st.sql); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "42501" {
				slog.Warn("consent bootstrap skipped (insufficient privilege — infra-owned DDL assumed)",
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
	if s.internal != s.pool {
		s.internal.Close()
	}
}

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

// Erase implements Repository: tombstone + outbox row in ONE transaction
// (SPEC-W43 I-04 — a crash between tombstone and publish can no longer lose
// the erasure events; the Relay republishes until MarkOutboxSent).
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
		if n == 0 {
			return nil
		}
		// Durable outbox row (same tx). Synthetic is recomputed
		// deterministically from the subject (EvaluateErasureEligibility).
		if _, err := tx.Exec(ctx,
			`INSERT INTO consent_events_outbox (tenant_id, data_subject_id, purpose, erased_records, synthetic)
			 VALUES ($1,$2,$3,$4,$5)`,
			tenantID, subject, purpose, n, EvaluateErasureEligibility(subject).Synthetic); err != nil {
			return fmt.Errorf("enqueue erasure outbox: %w", err)
		}
		return nil
	})
	return n, err
}

// FetchUnsentOutbox implements Repository. Cross-tenant by design (the relay
// is a platform job): runs on the internal pool, which in least-privilege
// deploys connects as an app_identity_internal member allowed by the policy
// escape.
func (s *Store) FetchUnsentOutbox(ctx context.Context, limit int) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	const q = `SELECT id, tenant_id, data_subject_id, purpose, erased_records, synthetic, created_at
	           FROM consent_events_outbox WHERE sent_at IS NULL ORDER BY id LIMIT $1`
	rows, err := s.internal.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("fetch unsent outbox: %w", err)
	}
	defer rows.Close()
	var out []OutboxEvent
	for rows.Next() {
		var e OutboxEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.DataSubjectID, &e.Purpose, &e.ErasedRecords, &e.Synthetic, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkOutboxSent implements Repository.
func (s *Store) MarkOutboxSent(ctx context.Context, id int64) error {
	if _, err := s.internal.Exec(ctx,
		`UPDATE consent_events_outbox SET sent_at = now() WHERE id = $1 AND sent_at IS NULL`, id); err != nil {
		return fmt.Errorf("mark outbox sent: %w", err)
	}
	return nil
}
