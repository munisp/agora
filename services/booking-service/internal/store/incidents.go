package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Incidents (SPEC-W11 Part B): incidents + incident_deliveries +
// incident_dispatch_endpoints, all tenant-scoped with RLS.
// ---------------------------------------------------------------------------

// Incident mirrors booking.incidents. Payload holds the full Incident Data
// Packet (SPEC-W11 canonical IDP JSON). ResolvedBy/ResolvedAt record the
// W48-A resolve transition (operator + timestamp; resolved_at NULL while
// unresolved).
type Incident struct {
	ID              uuid.UUID       `json:"id"`
	TenantID        uuid.UUID       `json:"tenant_id"`
	ReferenceNumber string          `json:"reference_number"`
	IncidentType    string          `json:"incident_type"`
	Severity        string          `json:"severity"`
	Payload         json.RawMessage `json:"payload"`
	Status          string          `json:"status"`
	CreatedAt       time.Time       `json:"created_at"`
	DispatchedAt    *time.Time      `json:"dispatched_at,omitempty"`
	ResolvedBy      string          `json:"resolved_by,omitempty"`
	ResolvedAt      *time.Time      `json:"resolved_at,omitempty"`
}

// IncidentDelivery mirrors booking.incident_deliveries: the per-endpoint
// dispatch ledger row (one per incident × active endpoint).
type IncidentDelivery struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       uuid.UUID  `json:"tenant_id"`
	IncidentID     uuid.UUID  `json:"incident_id"`
	EndpointURL    string     `json:"endpoint_url"`
	Status         string     `json:"status"` // pending | retrying | delivered | dlq
	Attempts       int        `json:"attempts"`
	LastStatusCode *int       `json:"last_status_code,omitempty"`
	LastError      string     `json:"last_error"`
	CreatedAt      time.Time  `json:"created_at"`
	DeliveredAt    *time.Time `json:"delivered_at,omitempty"`
}

// DispatchEndpoint mirrors booking.incident_dispatch_endpoints.
type DispatchEndpoint struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	URL       string    `json:"url"`
	Secret    string    `json:"secret,omitempty"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

// ensureIncidentTables bootstraps the SPEC-W11 Part B tables idempotently
// (same pattern as ensureWaitlistTable). RLS mirrors the infra-managed
// tables: enabled + forced with the tenant_isolation policy, guarded by a
// pg_policies existence check (CREATE POLICY has no IF NOT EXISTS).
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureIncidentTables(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS incidents (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL,
    reference_number TEXT NOT NULL DEFAULT '',
    incident_type    TEXT NOT NULL DEFAULT 'other',
    severity         TEXT NOT NULL DEFAULT 'medium'
                     CHECK (severity IN ('critical','high','medium','low')),
    payload          JSONB NOT NULL,
    status           TEXT NOT NULL DEFAULT 'new'
                     CHECK (status IN ('new','dispatched','acknowledged','resolved','closed')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    dispatched_at    TIMESTAMPTZ,
    resolved_by      TEXT NOT NULL DEFAULT '',
    resolved_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_incidents_tenant_status ON incidents (tenant_id, status, created_at);
ALTER TABLE incidents ENABLE ROW LEVEL SECURITY;
ALTER TABLE incidents FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'incidents' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON incidents
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS incident_deliveries (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL,
    incident_id      UUID NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    endpoint_url     TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','retrying','delivered','dlq')),
    attempts         INTEGER NOT NULL DEFAULT 0,
    last_status_code INTEGER,
    last_error       TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_incident_deliveries_incident ON incident_deliveries (tenant_id, incident_id);
ALTER TABLE incident_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE incident_deliveries FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'incident_deliveries' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON incident_deliveries
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS incident_dispatch_endpoints (
    tenant_id  UUID NOT NULL,
    url        TEXT NOT NULL,
    secret     TEXT NOT NULL DEFAULT '',
    active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, url)
);
ALTER TABLE incident_dispatch_endpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE incident_dispatch_endpoints FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'incident_dispatch_endpoints' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON incident_dispatch_endpoints
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure incident tables: %w", err)
	}
	// SPEC-W48 (W48-A item A1): in-place migration for existing databases —
	// the CREATE TABLE above only shapes fresh installs. Adds the resolve
	// audit columns and widens the status CHECK with 'resolved'. Idempotent:
	// the constraint is only swapped when its definition predates 'resolved'
	// (a fresh install's inline CHECK already includes it).
	const migrate = `
DO $$
BEGIN
    IF to_regclass('public.incidents') IS NOT NULL THEN
        ALTER TABLE incidents ADD COLUMN IF NOT EXISTS resolved_by TEXT NOT NULL DEFAULT '';
        ALTER TABLE incidents ADD COLUMN IF NOT EXISTS resolved_at TIMESTAMPTZ;
        IF EXISTS (
            SELECT 1 FROM pg_constraint
             WHERE conname = 'incidents_status_check'
               AND conrelid = 'public.incidents'::regclass
               AND pg_get_constraintdef(oid) NOT LIKE '%''resolved''%'
        ) THEN
            ALTER TABLE incidents DROP CONSTRAINT incidents_status_check;
            ALTER TABLE incidents ADD CONSTRAINT incidents_status_check
                CHECK (status IN ('new','dispatched','acknowledged','resolved','closed'));
        END IF;
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, migrate); err != nil {
		return fmt.Errorf("migrate incident resolve columns: %w", err)
	}
	return nil
}

const incidentCols = `id, tenant_id, reference_number, incident_type, severity, payload, status, created_at, dispatched_at, resolved_by, resolved_at`

func scanIncident(row pgx.Row) (Incident, error) {
	var i Incident
	err := row.Scan(&i.ID, &i.TenantID, &i.ReferenceNumber, &i.IncidentType, &i.Severity,
		&i.Payload, &i.Status, &i.CreatedAt, &i.DispatchedAt, &i.ResolvedBy, &i.ResolvedAt)
	return i, err
}

// InsertIncident persists one incident. Idempotent on the incident id (the
// IDP incident_id): a duplicate insert is a no-op and created reports false.
func (s *Store) InsertIncident(ctx context.Context, in *Incident) (created bool, err error) {
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	const q = `INSERT INTO incidents (` + incidentCols + `)
	           VALUES ($1,$2,$3,$4,$5,$6,'new',now(),NULL,'',NULL)
	           ON CONFLICT (id) DO NOTHING
	           RETURNING created_at`
	err = s.withTenant(ctx, in.TenantID, func(tx pgx.Tx) error {
		scanErr := tx.QueryRow(ctx, q, in.ID, in.TenantID, in.ReferenceNumber, in.IncidentType,
			in.Severity, in.Payload).Scan(&in.CreatedAt)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			created = false
			return nil
		}
		if scanErr != nil {
			return fmt.Errorf("insert incident: %w", scanErr)
		}
		created = true
		in.Status = "new"
		return nil
	})
	return created, err
}

// GetIncident fetches one incident scoped to a tenant.
func (s *Store) GetIncident(ctx context.Context, tenantID, id uuid.UUID) (Incident, error) {
	const q = `SELECT ` + incidentCols + ` FROM incidents WHERE tenant_id=$1 AND id=$2`
	var i Incident
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		i, err = scanIncident(tx.QueryRow(ctx, q, tenantID, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return i, ErrNotFound
	}
	return i, err
}

// ListIncidents returns incidents of a tenant, newest first, with optional
// status / created_at [from,to] filters (zero values disable a filter).
func (s *Store) ListIncidents(ctx context.Context, tenantID uuid.UUID, status string, from, to *time.Time) ([]Incident, error) {
	q := `SELECT ` + incidentCols + ` FROM incidents WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if status != "" {
		n++
		q += fmt.Sprintf(` AND status=$%d`, n)
		args = append(args, status)
	}
	if from != nil {
		n++
		q += fmt.Sprintf(` AND created_at >= $%d`, n)
		args = append(args, *from)
	}
	if to != nil {
		n++
		q += fmt.Sprintf(` AND created_at <= $%d`, n)
		args = append(args, *to)
	}
	q += ` ORDER BY created_at DESC LIMIT 500`
	var out []Incident
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			i, err := scanIncident(rows)
			if err != nil {
				return err
			}
			out = append(out, i)
		}
		return rows.Err()
	})
	return out, err
}

// MarkIncidentDispatched flips an incident to dispatched and stamps
// dispatched_at (idempotent: an already-dispatched row just updates the
// timestamp again).
func (s *Store) MarkIncidentDispatched(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE incidents SET status='dispatched', dispatched_at=now() WHERE tenant_id=$1 AND id=$2`,
			tenantID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// resolvableIncidentStatuses are the states from which POST
// /v1/incidents/{id}/resolve may transition (SPEC-W48 A1). resolved/closed
// are terminal for this route (409).
var resolvableIncidentStatuses = map[string]bool{
	"new":          true,
	"dispatched":   true,
	"acknowledged": true,
}

// ResolveIncidentTx atomically transitions an incident from
// new|dispatched|acknowledged → resolved, stamping resolved_by/resolved_at
// (SPEC-W48 A1). Inside ONE transaction it:
//
//  1. locks the row FOR UPDATE (the resolve race mutex: two concurrent
//     resolves serialize, the loser sees status='resolved' → 409);
//  2. validates the transition (already resolved/closed → ErrConflict);
//  3. flips status + stamps resolved_by/resolved_at;
//  4. appends the audit row: the resolve CloudEvent written to the
//     transactional outbox in the same tx (the durable audit trail —
//     mirrors the claim path's booking+outbox pattern; the row carries
//     actor/note/timestamps, so the resolution can never be lost or half
//     recorded). Skipped when outboxTopic is empty.
//
// Errors: ErrNotFound (unknown incident), ErrConflict (already
// resolved/closed — mapped to HTTP 409 by the handler).
func (s *Store) ResolveIncidentTx(ctx context.Context, tenantID, id uuid.UUID, actorID, note, outboxTopic string, eventPayload []byte) (Incident, error) {
	var inc Incident
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		inc, err = scanIncident(tx.QueryRow(ctx,
			`SELECT `+incidentCols+` FROM incidents WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
			tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !resolvableIncidentStatuses[inc.Status] {
			return fmt.Errorf("%w: incident already %s", ErrConflict, inc.Status)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE incidents SET status='resolved', resolved_by=$3, resolved_at=now()
			 WHERE tenant_id=$1 AND id=$2`,
			tenantID, id, actorID); err != nil {
			return fmt.Errorf("resolve incident: %w", err)
		}
		if outboxTopic != "" && len(eventPayload) > 0 {
			if _, err := tx.Exec(ctx,
				`INSERT INTO outbox (aggregate_id, topic, payload) VALUES ($1,$2,$3)`,
				inc.ID, outboxTopic, eventPayload); err != nil {
				return fmt.Errorf("insert resolve audit outbox row: %w", err)
			}
		}
		inc.Status = "resolved"
		inc.ResolvedBy = actorID
		now := time.Now().UTC()
		inc.ResolvedAt = &now
		_ = note // the note rides the audit outbox payload (see caller)
		return nil
	})
	if err == nil && outboxTopic != "" && len(eventPayload) > 0 {
		s.signalOutboxFlush() // W46-A item 6: flush-on-commit
	}
	return inc, err
}

// ---------------------------------------------------------------------------
// Dispatch endpoints
// ---------------------------------------------------------------------------

// UpsertDispatchEndpoint creates or re-activates a tenant dispatch endpoint
// (PK tenant_id+url; an existing row gets secret/active replaced).
func (s *Store) UpsertDispatchEndpoint(ctx context.Context, ep *DispatchEndpoint) error {
	const q = `INSERT INTO incident_dispatch_endpoints (tenant_id, url, secret, active, created_at)
	           VALUES ($1,$2,$3,$4,now())
	           ON CONFLICT (tenant_id, url) DO UPDATE SET secret=EXCLUDED.secret, active=EXCLUDED.active
	           RETURNING created_at`
	return s.withTenant(ctx, ep.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, ep.TenantID, ep.URL, ep.Secret, ep.Active).Scan(&ep.CreatedAt)
	})
}

// ListDispatchEndpoints returns tenant endpoints; activeOnly filters to the
// dispatchable subset.
func (s *Store) ListDispatchEndpoints(ctx context.Context, tenantID uuid.UUID, activeOnly bool) ([]DispatchEndpoint, error) {
	q := `SELECT tenant_id, url, secret, active, created_at FROM incident_dispatch_endpoints WHERE tenant_id=$1`
	if activeOnly {
		q += ` AND active`
	}
	q += ` ORDER BY created_at LIMIT 500`
	var out []DispatchEndpoint
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ep DispatchEndpoint
			if err := rows.Scan(&ep.TenantID, &ep.URL, &ep.Secret, &ep.Active, &ep.CreatedAt); err != nil {
				return err
			}
			out = append(out, ep)
		}
		return rows.Err()
	})
	return out, err
}

// DeleteDispatchEndpoint removes one endpoint by URL (PK tenant_id+url).
func (s *Store) DeleteDispatchEndpoint(ctx context.Context, tenantID uuid.UUID, url string) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM incident_dispatch_endpoints WHERE tenant_id=$1 AND url=$2`, tenantID, url)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Delivery ledger
// ---------------------------------------------------------------------------

const deliveryCols = `id, tenant_id, incident_id, endpoint_url, status, attempts, last_status_code, last_error, created_at, delivered_at`

func scanDelivery(row pgx.Row) (IncidentDelivery, error) {
	var d IncidentDelivery
	err := row.Scan(&d.ID, &d.TenantID, &d.IncidentID, &d.EndpointURL, &d.Status,
		&d.Attempts, &d.LastStatusCode, &d.LastError, &d.CreatedAt, &d.DeliveredAt)
	return d, err
}

// InsertIncidentDelivery appends one pending ledger row (idempotent on the
// delivery id chosen by the dispatcher, so a re-dispatch retry cannot
// duplicate rows).
func (s *Store) InsertIncidentDelivery(ctx context.Context, d *IncidentDelivery) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	const q = `INSERT INTO incident_deliveries (` + deliveryCols + `)
	           VALUES ($1,$2,$3,$4,'pending',0,NULL,'',now(),NULL)
	           ON CONFLICT (id) DO NOTHING
	           RETURNING created_at`
	return s.withTenant(ctx, d.TenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, q, d.ID, d.TenantID, d.IncidentID, d.EndpointURL).Scan(&d.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // duplicate delivery id: ledger row already exists
		}
		if err != nil {
			return fmt.Errorf("insert incident delivery: %w", err)
		}
		d.Status = "pending"
		return nil
	})
}

// ListIncidentDeliveries returns the ledger rows of one incident.
func (s *Store) ListIncidentDeliveries(ctx context.Context, tenantID, incidentID uuid.UUID) ([]IncidentDelivery, error) {
	const q = `SELECT ` + deliveryCols + ` FROM incident_deliveries WHERE tenant_id=$1 AND incident_id=$2 ORDER BY created_at LIMIT 500`
	var out []IncidentDelivery
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID, incidentID)
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

// EnqueueOutbox appends one row to the transactional outbox (drained by the
// outbox dispatcher to Kafka). Used for the best-effort incident outreach
// usage-metering record (SPEC-W11 Part B §5).
//
// NOTE (RLS): the outbox table is not tenant-scoped (no RLS policy — the
// dispatcher drains it cross-tenant by design).
func (s *Store) EnqueueOutbox(ctx context.Context, aggregateID uuid.UUID, topic string, payload []byte) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO outbox (aggregate_id, topic, payload) VALUES ($1,$2,$3)`,
		aggregateID, topic, payload); err != nil {
		return fmt.Errorf("enqueue outbox: %w", err)
	}
	s.signalOutboxFlush() // W46-A item 6: flush-on-commit
	return nil
}

// UpdateIncidentDelivery records one delivery attempt outcome (retrying /
// delivered / dlq) written by the Wave-5 WebhookDeliveryWorkflow via the
// internal update endpoint. delivered sets delivered_at.
//
// NOTE (RLS): internal service-to-service path (notification-worker →
// /internal/incidents/deliveries/{id}); the delivery id is an unguessable
// UUID minted by the dispatcher, so — like the outbox dispatcher — this
// runs outside withTenant and scopes by id only.
func (s *Store) UpdateIncidentDelivery(ctx context.Context, id uuid.UUID, status string, attempts int, statusCode *int, lastErr string) error {
	const q = `UPDATE incident_deliveries
	           SET status=$2, attempts=$3, last_status_code=$4, last_error=$5,
	               delivered_at = CASE WHEN $2='delivered' THEN now() ELSE delivered_at END
	           WHERE id=$1`
	tag, err := s.pool.Exec(ctx, q, id, status, attempts, statusCode, lastErr)
	if err != nil {
		return fmt.Errorf("update incident delivery: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
