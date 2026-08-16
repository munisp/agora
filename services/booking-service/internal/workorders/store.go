package workorders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists work orders. Same packaging idiom as the W16 devices
// package: NewStore wraps an existing pool (tests), DialStore opens a
// small dedicated pool (integrator wiring path — the shared store.Store
// does not expose its pool). maxConns 4: dispatch is an operator-paced,
// low-QPS path.
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

// ensureSchema bootstraps work_orders idempotently (contract §1: RLS
// enabled + forced with the tenant_isolation policy, guarded by a
// pg_policies existence check — mirrors internal/devices/store.go).
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS work_orders (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL,
    contact_id       UUID,
    booking_id       UUID,
    title            TEXT NOT NULL,
    description      TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'created'
                     CHECK (status IN ('created','assigned','en_route','on_site','completed','cancelled')),
    assignee_id      UUID,
    scheduled_start  TIMESTAMPTZ,
    scheduled_end    TIMESTAMPTZ,
    gps_lat          DOUBLE PRECISION,
    gps_lng          DOUBLE PRECISION,
    gps_accuracy     DOUBLE PRECISION,
    checklist        JSONB NOT NULL DEFAULT '[]',
    proof            JSONB NOT NULL DEFAULT '{}',
    field_capture_id TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_work_orders_status ON work_orders (tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_work_orders_assignee ON work_orders (tenant_id, assignee_id);
CREATE INDEX IF NOT EXISTS idx_work_orders_scheduled ON work_orders (tenant_id, scheduled_start);
ALTER TABLE work_orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_orders FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'work_orders' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON work_orders
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure work_orders table: %w", err)
	}
	return nil
}

// withTenant runs fn inside a transaction with `SET LOCAL app.tenant_id`
// applied (mirrors devices.Store.withTenant) so the RLS tenant_isolation
// policy scopes every statement of fn to the given tenant.
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
// devices.ErrNotFound so the API maps it to 404).
var ErrNotFound = errors.New("not found")

// ErrNoAssignee is returned by PickAutoAssignee when the tenant has no
// active team members to dispatch to (409 at the API).
var ErrNoAssignee = errors.New("no active team member available")

const workOrderCols = `id, tenant_id, contact_id, booking_id, title, description,
	status, assignee_id, scheduled_start, scheduled_end,
	gps_lat, gps_lng, gps_accuracy, checklist, proof, field_capture_id,
	created_at, updated_at, completed_at`

func scanWorkOrder(row pgx.Row) (WorkOrder, error) {
	var w WorkOrder
	var checklist, proof []byte
	err := row.Scan(&w.ID, &w.TenantID, &w.ContactID, &w.BookingID, &w.Title, &w.Description,
		&w.Status, &w.AssigneeID, &w.ScheduledStart, &w.ScheduledEnd,
		&w.GPSLat, &w.GPSLng, &w.GPSAccuracy, &checklist, &proof, &w.FieldCaptureID,
		&w.CreatedAt, &w.UpdatedAt, &w.CompletedAt)
	if err != nil {
		return w, err
	}
	w.Checklist = []ChecklistItem{}
	if len(checklist) > 0 {
		if err := json.Unmarshal(checklist, &w.Checklist); err != nil {
			return w, fmt.Errorf("decode checklist: %w", err)
		}
	}
	if len(proof) > 0 {
		if err := json.Unmarshal(proof, &w.Proof); err != nil {
			return w, fmt.Errorf("decode proof: %w", err)
		}
	}
	return w, nil
}

// Create inserts a new work order (status "created" unless the caller
// pre-validated another legal start state). The ID/timestamps are stamped
// back onto w.
func (s *Store) Create(ctx context.Context, w *WorkOrder) error {
	checklist, err := json.Marshal(w.Checklist)
	if err != nil {
		return fmt.Errorf("encode checklist: %w", err)
	}
	proof, err := json.Marshal(w.Proof)
	if err != nil {
		return fmt.Errorf("encode proof: %w", err)
	}
	const q = `INSERT INTO work_orders (` + workOrderCols + `)
		           VALUES (COALESCE($1, gen_random_uuid()), $2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,now(),now(),$17)
		           RETURNING id, created_at, updated_at`
	return s.withTenant(ctx, w.TenantID, func(tx pgx.Tx) error {
		var id any
		if w.ID != uuid.Nil {
			id = w.ID
		}
		return tx.QueryRow(ctx, q, id, w.TenantID, w.ContactID, w.BookingID, w.Title, w.Description,
			w.Status, w.AssigneeID, w.ScheduledStart, w.ScheduledEnd,
			w.GPSLat, w.GPSLng, w.GPSAccuracy, checklist, proof, w.FieldCaptureID, w.CompletedAt).
			Scan(&w.ID, &w.CreatedAt, &w.UpdatedAt)
	})
}

// Get loads one work order scoped to the tenant (ErrNotFound when missing
// or cross-tenant).
func (s *Store) Get(ctx context.Context, tenantID, id uuid.UUID) (WorkOrder, error) {
	var w WorkOrder
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+workOrderCols+` FROM work_orders WHERE tenant_id=$1 AND id=$2`, tenantID, id)
		var err error
		w, err = scanWorkOrder(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return w, err
}

// ListFilters scopes List ("" / nil disables a filter).
type ListFilters struct {
	Status   string
	Assignee *uuid.UUID
	Q        string     // case-insensitive substring over title/description
	From     *time.Time // scheduled_start >= from
	To       *time.Time // scheduled_start < to
	Limit    int        // 0 → 200 (default cap)
}

// List returns the tenant's work orders (newest first) per the filters.
// Backs GET /v1/field-service/work-orders.
func (s *Store) List(ctx context.Context, tenantID uuid.UUID, f ListFilters) ([]WorkOrder, error) {
	q := `SELECT ` + workOrderCols + ` FROM work_orders WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if f.Status != "" {
		n++
		q += fmt.Sprintf(` AND status=$%d`, n)
		args = append(args, f.Status)
	}
	if f.Assignee != nil {
		n++
		q += fmt.Sprintf(` AND assignee_id=$%d`, n)
		args = append(args, *f.Assignee)
	}
	if f.Q != "" {
		n++
		q += fmt.Sprintf(` AND (title ILIKE '%%' || $%d || '%%' OR description ILIKE '%%' || $%d || '%%')`, n, n)
		args = append(args, f.Q)
	}
	if f.From != nil {
		n++
		q += fmt.Sprintf(` AND scheduled_start >= $%d`, n)
		args = append(args, *f.From)
	}
	if f.To != nil {
		n++
		q += fmt.Sprintf(` AND scheduled_start < $%d`, n)
		args = append(args, *f.To)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	n++
	q += fmt.Sprintf(` ORDER BY created_at DESC LIMIT $%d`, n)
	args = append(args, limit)

	out := []WorkOrder{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			w, err := scanWorkOrder(rows)
			if err != nil {
				return err
			}
			out = append(out, w)
		}
		return rows.Err()
	})
	return out, err
}

// Update persists every mutable field of w (status transitions are
// validated by the caller — the store is deliberately machine-agnostic so
// tests and future admin repair paths stay possible). updated_at is
// stamped; ErrNotFound when the row is missing/cross-tenant.
func (s *Store) Update(ctx context.Context, w *WorkOrder) error {
	checklist, err := json.Marshal(w.Checklist)
	if err != nil {
		return fmt.Errorf("encode checklist: %w", err)
	}
	proof, err := json.Marshal(w.Proof)
	if err != nil {
		return fmt.Errorf("encode proof: %w", err)
	}
	return s.withTenant(ctx, w.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE work_orders SET
				contact_id=$3, booking_id=$4, title=$5, description=$6, status=$7,
				assignee_id=$8, scheduled_start=$9, scheduled_end=$10,
				gps_lat=$11, gps_lng=$12, gps_accuracy=$13,
				checklist=$14, proof=$15, field_capture_id=$16,
				updated_at=now(), completed_at=$17
			WHERE tenant_id=$1 AND id=$2`,
			w.TenantID, w.ID, w.ContactID, w.BookingID, w.Title, w.Description, w.Status,
			w.AssigneeID, w.ScheduledStart, w.ScheduledEnd,
			w.GPSLat, w.GPSLng, w.GPSAccuracy,
			checklist, proof, w.FieldCaptureID, w.CompletedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// boardSelect joins team_members for the assignee display name. The join
// is tenant-scoped on both sides (team_members carries its own
// tenant_isolation policy — the withTenant context covers it).
const boardSelect = `SELECT w.id, w.tenant_id, w.contact_id, w.booking_id, w.title, w.description,
	w.status, w.assignee_id, w.scheduled_start, w.scheduled_end,
	w.gps_lat, w.gps_lng, w.gps_accuracy, w.checklist, w.proof, w.field_capture_id,
	w.created_at, w.updated_at, w.completed_at, COALESCE(tm.name, '')
	FROM work_orders w
	LEFT JOIN team_members tm ON tm.tenant_id = w.tenant_id AND tm.id = w.assignee_id`

func scanBoardItem(row pgx.Row) (BoardItem, error) {
	var b BoardItem
	var checklist, proof []byte
	err := row.Scan(&b.ID, &b.TenantID, &b.ContactID, &b.BookingID, &b.Title, &b.Description,
		&b.Status, &b.AssigneeID, &b.ScheduledStart, &b.ScheduledEnd,
		&b.GPSLat, &b.GPSLng, &b.GPSAccuracy, &checklist, &proof, &b.FieldCaptureID,
		&b.CreatedAt, &b.UpdatedAt, &b.CompletedAt, &b.AssigneeName)
	if err != nil {
		return b, err
	}
	b.Checklist = []ChecklistItem{}
	if len(checklist) > 0 {
		if err := json.Unmarshal(checklist, &b.Checklist); err != nil {
			return b, fmt.Errorf("decode checklist: %w", err)
		}
	}
	if len(proof) > 0 {
		if err := json.Unmarshal(proof, &b.Proof); err != nil {
			return b, fmt.Errorf("decode proof: %w", err)
		}
	}
	return b, nil
}

// Board returns the tenant's work orders with assignee names, oldest
// scheduled first (dispatch order), capped at 500. Grouping into status
// lanes happens in the handler. Backs GET /v1/field-service/board.
func (s *Store) Board(ctx context.Context, tenantID uuid.UUID) ([]BoardItem, error) {
	const q = boardSelect + ` WHERE w.tenant_id=$1
		ORDER BY w.scheduled_start ASC NULLS LAST, w.created_at ASC LIMIT 500`
	return s.listBoard(ctx, tenantID, q, tenantID)
}

// Today returns the tenant's work orders whose scheduled_start falls
// inside [dayStart, dayEnd) (the tenant-local "today" computed by the
// handler in the tenant timezone), optionally restricted to one assignee.
// Backs GET /v1/field-service/today.
func (s *Store) Today(ctx context.Context, tenantID uuid.UUID, dayStart, dayEnd time.Time, assignee *uuid.UUID) ([]BoardItem, error) {
	q := boardSelect + ` WHERE w.tenant_id=$1 AND w.scheduled_start >= $2 AND w.scheduled_start < $3`
	args := []any{tenantID, dayStart, dayEnd}
	if assignee != nil {
		q += ` AND w.assignee_id = $4`
		args = append(args, *assignee)
	}
	q += ` ORDER BY w.scheduled_start ASC NULLS LAST, w.created_at ASC LIMIT 500`
	return s.listBoard(ctx, tenantID, q, args...)
}

func (s *Store) listBoard(ctx context.Context, tenantID uuid.UUID, q string, args ...any) ([]BoardItem, error) {
	out := []BoardItem{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			b, err := scanBoardItem(rows)
			if err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

// PickAutoAssignee chooses the ACTIVE team member with the fewest open
// (assigned|en_route|on_site) work orders — the field-service mirror of
// the helpdesk "auto → least-open-tickets" rule (SPEC-W19 Agent A).
// Ties break by name for determinism. ErrNoAssignee when the tenant has
// no active team members.
func (s *Store) PickAutoAssignee(ctx context.Context, tenantID uuid.UUID) (uuid.UUID, error) {
	const q = `SELECT tm.id
		           FROM team_members tm
		           LEFT JOIN work_orders wo
		             ON wo.tenant_id = tm.tenant_id
		            AND wo.assignee_id = tm.id
		            AND wo.status = ANY($2)
		           WHERE tm.tenant_id = $1 AND tm.active
		           GROUP BY tm.id, tm.name
		           ORDER BY COUNT(wo.id) ASC, tm.name ASC
		           LIMIT 1`
	var id uuid.UUID
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, q, tenantID, openStatuses).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoAssignee
		}
		return err
	})
	return id, err
}

// AssigneeName resolves one team member's display name ("" when unknown).
// Used to render dispatch notifications.
func (s *Store) AssigneeName(ctx context.Context, tenantID, id uuid.UUID) (string, error) {
	var name string
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT name FROM team_members WHERE tenant_id=$1 AND id=$2`, tenantID, id).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return name, err
}

// EnqueueOutbox appends one row to the transactional outbox (mirrors
// referrals.PayoutStore.EnqueueOutbox; lifecycle events, metered usage
// records and the dispatch push envelope all ride this path — the W5
// outbox dispatcher drains rows to Kafka via Dapr).
//
// NOTE (RLS): the outbox table is not tenant-scoped (no RLS policy — the
// dispatcher drains it cross-tenant by design).
func (s *Store) EnqueueOutbox(ctx context.Context, aggregateID uuid.UUID, topic string, payload []byte) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO outbox (aggregate_id, topic, payload) VALUES ($1,$2,$3)`,
		aggregateID, topic, payload); err != nil {
		return fmt.Errorf("enqueue outbox: %w", err)
	}
	return nil
}
