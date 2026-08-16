package campaignstudio

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists Campaign Studio rows. Same packaging idiom as the W16
// devices.Store: NewStore wraps an existing pool (tests), DialStore opens
// a small dedicated pool (integrator wiring path — the shared store.Store
// does not expose its pool). maxConns 4: studio is an operator-driven
// low-QPS surface (the CRON step endpoint is the hot caller and it is
// batch-bounded).
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

// ensureSchema bootstraps the studio tables idempotently (mirrors the W16
// devices idiom): RLS enabled + forced with the tenant_isolation policy,
// guarded by a pg_policies existence check. The outbox guard row creates
// the shared transactional outbox ONLY when absent (standalone/tests);
// the base schema (infra/postgres init-scripts) already owns it in real
// deployments, with the identical shape, so this is a no-op there.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS studio_segments (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
    name         TEXT NOT NULL,
    definition   JSONB NOT NULL,
    approx_count BIGINT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_studio_segments_tenant ON studio_segments (tenant_id, created_at);
ALTER TABLE studio_segments ENABLE ROW LEVEL SECURITY;
ALTER TABLE studio_segments FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'studio_segments' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON studio_segments
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS studio_journeys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
    name         TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'draft'
                 CHECK (status IN ('draft','active','paused','archived')),
    trigger_kind TEXT NOT NULL DEFAULT 'manual'
                 CHECK (trigger_kind IN ('segment','manual','event')),
    segment_id   UUID,
    steps        JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_studio_journeys_tenant ON studio_journeys (tenant_id, status, created_at);
ALTER TABLE studio_journeys ENABLE ROW LEVEL SECURITY;
ALTER TABLE studio_journeys FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'studio_journeys' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON studio_journeys
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS studio_enrollments (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL,
    journey_id    UUID NOT NULL,
    contact_id    UUID NOT NULL,
    step_idx      INTEGER NOT NULL DEFAULT 0,
    state         TEXT NOT NULL DEFAULT 'active'
                  CHECK (state IN ('active','completed','exited')),
    enrolled_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_step_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    exited_reason TEXT,
    UNIQUE (tenant_id, journey_id, contact_id)
);
CREATE INDEX IF NOT EXISTS idx_studio_enrollments_due ON studio_enrollments (tenant_id, journey_id, state, enrolled_at);
ALTER TABLE studio_enrollments ENABLE ROW LEVEL SECURITY;
ALTER TABLE studio_enrollments FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'studio_enrollments' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON studio_enrollments
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS studio_step_events (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL,
    journey_id    UUID NOT NULL,
    enrollment_id UUID NOT NULL,
    step_idx      INTEGER NOT NULL,
    kind          TEXT NOT NULL,
    payload       JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_studio_step_events_journey ON studio_step_events (tenant_id, journey_id, step_idx, kind);
ALTER TABLE studio_step_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE studio_step_events FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'studio_step_events' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON studio_step_events
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

-- Shared transactional outbox (identical shape to the base schema; the
-- IF NOT EXISTS makes standalone tests self-sufficient and real
-- deployments a no-op). Not RLS-scoped: the dispatcher drains cross-tenant.
CREATE TABLE IF NOT EXISTS outbox (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic        TEXT NOT NULL,
    payload      JSONB NOT NULL,
    sent_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_outbox_unsent ON outbox (id) WHERE sent_at IS NULL;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure campaign studio tables: %w", err)
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
// devices.ErrNotFound so the API maps it to 404 like the sibling stores).
var ErrNotFound = errors.New("not found")

// EnqueueOutbox appends one row to the transactional outbox (mirrors
// referrals.PayoutStore.EnqueueOutbox).
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

// ---------------------------------------------------------------------------
// Segments
// ---------------------------------------------------------------------------

const segmentCols = `id, tenant_id, name, definition, approx_count, created_at, updated_at`

func scanSegment(row pgx.Row) (Segment, error) {
	var seg Segment
	err := row.Scan(&seg.ID, &seg.TenantID, &seg.Name, &seg.Definition,
		&seg.ApproxCount, &seg.CreatedAt, &seg.UpdatedAt)
	return seg, err
}

// CreateSegment inserts one segment (name/definition validated by the
// caller) and returns it with id/timestamps stamped.
func (s *Store) CreateSegment(ctx context.Context, seg *Segment) error {
	const q = `INSERT INTO studio_segments (tenant_id, name, definition)
		           VALUES ($1,$2,$3)
		           RETURNING ` + segmentCols
	return s.withTenant(ctx, seg.TenantID, func(tx pgx.Tx) error {
		row, err := scanSegment(tx.QueryRow(ctx, q, seg.TenantID, seg.Name, seg.Definition))
		if err != nil {
			return fmt.Errorf("insert segment: %w", err)
		}
		*seg = row
		return nil
	})
}

// ListSegments returns the tenant's segments (newest first).
func (s *Store) ListSegments(ctx context.Context, tenantID uuid.UUID) ([]Segment, error) {
	out := []Segment{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+segmentCols+` FROM studio_segments WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT 500`,
			tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			seg, err := scanSegment(rows)
			if err != nil {
				return err
			}
			out = append(out, seg)
		}
		return rows.Err()
	})
	return out, err
}

// GetSegment loads one segment scoped to the tenant (ErrNotFound when
// missing or cross-tenant).
func (s *Store) GetSegment(ctx context.Context, tenantID, segmentID uuid.UUID) (Segment, error) {
	var seg Segment
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := scanSegment(tx.QueryRow(ctx,
			`SELECT `+segmentCols+` FROM studio_segments WHERE tenant_id=$1 AND id=$2`,
			tenantID, segmentID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		seg = row
		return err
	})
	return seg, err
}

// UpdateSegment applies a name/definition patch (nil leaves the column
// untouched). ErrNotFound when missing or cross-tenant.
func (s *Store) UpdateSegment(ctx context.Context, tenantID, segmentID uuid.UUID, name *string, def *SegmentDefinition) (Segment, error) {
	var seg Segment
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := scanSegment(tx.QueryRow(ctx,
			`UPDATE studio_segments
			    SET name       = COALESCE($3, name),
			        definition = COALESCE($4, definition),
			        updated_at = now()
			  WHERE tenant_id=$1 AND id=$2
			  RETURNING `+segmentCols,
			tenantID, segmentID, name, def))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		seg = row
		return err
	})
	return seg, err
}

// CountSegment evaluates the segment definition against booking.contacts
// (read-only RLS-safe query; lead_* fields via the EXISTS phone join).
// The evaluation scans at most segmentCountRowCeiling (100k) contacts:
// truncated=true reports the ceiling was hit (approx_count stores the
// bounded value). The fresh count is persisted to approx_count in the
// same transaction (the only write — a cache stamp, not evaluation state).
func (s *Store) CountSegment(ctx context.Context, tenantID, segmentID uuid.UUID) (count int64, truncated bool, err error) {
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		seg, err := scanSegment(tx.QueryRow(ctx,
			`SELECT `+segmentCols+` FROM studio_segments WHERE tenant_id=$1 AND id=$2`,
			tenantID, segmentID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		where, args, err := buildSegmentQuery(&seg.Definition)
		if err != nil {
			return err
		}
		q := `SELECT count(*) FROM (SELECT c.id FROM contacts c
		         WHERE c.tenant_id=$1 AND ` + where +
			fmt.Sprintf(` LIMIT %d) bounded`, segmentCountRowCeiling+1)
		var n int64
		if err := tx.QueryRow(ctx, q, append([]any{tenantID}, args...)...).Scan(&n); err != nil {
			return fmt.Errorf("count segment: %w", err)
		}
		if n > segmentCountRowCeiling {
			n = segmentCountRowCeiling
			truncated = true
		}
		count = n
		_, err = tx.Exec(ctx,
			`UPDATE studio_segments SET approx_count=$3, updated_at=now() WHERE tenant_id=$1 AND id=$2`,
			tenantID, segmentID, n)
		return err
	})
	return count, truncated, err
}

// ---------------------------------------------------------------------------
// Journeys
// ---------------------------------------------------------------------------

const journeyCols = `id, tenant_id, name, status, trigger_kind, segment_id, steps, created_at, updated_at`

func scanJourney(row pgx.Row) (Journey, error) {
	var j Journey
	err := row.Scan(&j.ID, &j.TenantID, &j.Name, &j.Status, &j.TriggerKind,
		&j.SegmentID, &j.Steps, &j.CreatedAt, &j.UpdatedAt)
	return j, err
}

// CreateJourney inserts one journey in draft status.
func (s *Store) CreateJourney(ctx context.Context, j *Journey) error {
	const q = `INSERT INTO studio_journeys (tenant_id, name, trigger_kind, segment_id, steps)
		           VALUES ($1,$2,$3,$4,$5)
		           RETURNING ` + journeyCols
	return s.withTenant(ctx, j.TenantID, func(tx pgx.Tx) error {
		row, err := scanJourney(tx.QueryRow(ctx, q, j.TenantID, j.Name, j.TriggerKind, j.SegmentID, j.Steps))
		if err != nil {
			return fmt.Errorf("insert journey: %w", err)
		}
		*j = row
		return nil
	})
}

// ListJourneys returns the tenant's journeys (newest first), optionally
// filtered by status ("" disables the filter).
func (s *Store) ListJourneys(ctx context.Context, tenantID uuid.UUID, status string) ([]Journey, error) {
	out := []Journey{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		q := `SELECT ` + journeyCols + ` FROM studio_journeys WHERE tenant_id=$1`
		args := []any{tenantID}
		if status != "" {
			q += ` AND status=$2`
			args = append(args, status)
		}
		q += ` ORDER BY created_at DESC LIMIT 500`
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			j, err := scanJourney(rows)
			if err != nil {
				return err
			}
			out = append(out, j)
		}
		return rows.Err()
	})
	return out, err
}

// GetJourney loads one journey scoped to the tenant.
func (s *Store) GetJourney(ctx context.Context, tenantID, journeyID uuid.UUID) (Journey, error) {
	var j Journey
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := scanJourney(tx.QueryRow(ctx,
			`SELECT `+journeyCols+` FROM studio_journeys WHERE tenant_id=$1 AND id=$2`,
			tenantID, journeyID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		j = row
		return err
	})
	return j, err
}

// UpdateJourney applies a mutable-fields patch (name / trigger_kind /
// segment_id / steps; nil leaves the column untouched). The caller
// enforces the edit-state rule (draft|paused only for structural edits).
func (s *Store) UpdateJourney(ctx context.Context, tenantID, journeyID uuid.UUID, name *string, triggerKind *string, segmentID **uuid.UUID, steps *Steps) (Journey, error) {
	var j Journey
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := scanJourney(tx.QueryRow(ctx,
			`UPDATE studio_journeys
			    SET name         = COALESCE($3, name),
			        trigger_kind = COALESCE($4, trigger_kind),
			        segment_id   = COALESCE($5, segment_id),
			        steps        = COALESCE($6, steps),
			        updated_at   = now()
			  WHERE tenant_id=$1 AND id=$2
			  RETURNING `+journeyCols,
			tenantID, journeyID, name, triggerKind, segmentID, steps))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		j = row
		return err
	})
	return j, err
}

// TransitionJourney moves a journey through the status machine
// (CanTransition). Illegal transitions yield ErrConflict (409 at the API);
// missing/cross-tenant rows yield ErrNotFound.
func (s *Store) TransitionJourney(ctx context.Context, tenantID, journeyID uuid.UUID, to string) (Journey, error) {
	var j Journey
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanJourney(tx.QueryRow(ctx,
			`SELECT `+journeyCols+` FROM studio_journeys WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
			tenantID, journeyID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.Status == to {
			j = cur
			return nil // same-state no-op
		}
		if !CanTransition(cur.Status, to) {
			return fmt.Errorf("%w: journey status %s → %s is not allowed", ErrConflict, cur.Status, to)
		}
		row, err := scanJourney(tx.QueryRow(ctx,
			`UPDATE studio_journeys SET status=$3, updated_at=now()
			  WHERE tenant_id=$1 AND id=$2 RETURNING `+journeyCols,
			tenantID, journeyID, to))
		if err != nil {
			return err
		}
		j = row
		return nil
	})
	return j, err
}

// ---------------------------------------------------------------------------
// Enrollments
// ---------------------------------------------------------------------------

const enrollmentCols = `id, tenant_id, journey_id, contact_id, step_idx, state, enrolled_at, last_step_at, exited_reason`

func scanEnrollment(row pgx.Row) (Enrollment, error) {
	var e Enrollment
	err := row.Scan(&e.ID, &e.TenantID, &e.JourneyID, &e.ContactID, &e.StepIdx,
		&e.State, &e.EnrolledAt, &e.LastStepAt, &e.ExitedReason)
	return e, err
}

// Enroll inserts enrollments idempotently per (journey, contact): the
// UNIQUE(tenant_id, journey_id, contact_id) anchor + ON CONFLICT DO
// NOTHING make replays safe. Returns the NEWLY created enrollments
// (callers meter/event exactly those, so a replayed enroll can never
// double-meter) plus the count of pre-existing ones.
func (s *Store) Enroll(ctx context.Context, tenantID, journeyID uuid.UUID, contactIDs []uuid.UUID) (created []Enrollment, existing int, err error) {
	created = []Enrollment{}
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		for _, cid := range contactIDs {
			row, err := scanEnrollment(tx.QueryRow(ctx,
				`INSERT INTO studio_enrollments (tenant_id, journey_id, contact_id)
				 VALUES ($1,$2,$3)
				 ON CONFLICT (tenant_id, journey_id, contact_id) DO NOTHING
				 RETURNING `+enrollmentCols,
				tenantID, journeyID, cid))
			if errors.Is(err, pgx.ErrNoRows) {
				existing++
				continue
			}
			if err != nil {
				return fmt.Errorf("enroll contact %s: %w", cid, err)
			}
			created = append(created, row)
		}
		return nil
	})
	return created, existing, err
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

// StepStat is the per-step breakdown of GET /journeys/{id}/stats.
type StepStat struct {
	StepIdx    int    `json:"step_idx"`
	Type       string `json:"type"`
	Active     int64  `json:"active"`     // enrollments currently waiting at this step
	Passed     int64  `json:"passed"`     // wait_passed + branch_true + send_queued/skipped events
	Sent       int64  `json:"sent"`       // send_sent outcomes (paced dispatch confirmed)
	Suppressed int64  `json:"suppressed"` // send_suppressed (DND guard)
	Skipped    int64  `json:"skipped"`    // send_skipped (ussd / missing address)
	Failed     int64  `json:"failed"`     // send_failed
	Exited     int64  `json:"exited"`     // branch_false / exited events at this step
}

// JourneyStats is the response shape of GET /journeys/{id}/stats.
type JourneyStats struct {
	Enrolled  int64      `json:"enrolled"`
	Active    int64      `json:"active"`
	Completed int64      `json:"completed"`
	Exited    int64      `json:"exited"`
	PerStep   []StepStat `json:"per_step"`
}

// Stats aggregates enrollment totals + per-step counts for one journey.
func (s *Store) Stats(ctx context.Context, tenantID uuid.UUID, j Journey) (JourneyStats, error) {
	stats := JourneyStats{PerStep: []StepStat{}}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*),
			        count(*) FILTER (WHERE state='active'),
			        count(*) FILTER (WHERE state='completed'),
			        count(*) FILTER (WHERE state='exited')
			   FROM studio_enrollments WHERE tenant_id=$1 AND journey_id=$2`,
			tenantID, j.ID).Scan(&stats.Enrolled, &stats.Active, &stats.Completed, &stats.Exited); err != nil {
			return err
		}

		perStep := map[int]*StepStat{}
		for i, st := range j.Steps {
			perStep[i] = &StepStat{StepIdx: i, Type: st.Type}
		}
		rows, err := tx.Query(ctx,
			`SELECT step_idx, count(*) FROM studio_enrollments
			  WHERE tenant_id=$1 AND journey_id=$2 AND state='active'
			  GROUP BY step_idx`,
			tenantID, j.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var idx int
			var n int64
			if err := rows.Scan(&idx, &n); err != nil {
				rows.Close()
				return err
			}
			if ss, ok := perStep[idx]; ok {
				ss.Active = n
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		evRows, err := tx.Query(ctx,
			`SELECT step_idx, kind, count(*) FROM studio_step_events
			  WHERE tenant_id=$1 AND journey_id=$2
			  GROUP BY step_idx, kind`,
			tenantID, j.ID)
		if err != nil {
			return err
		}
		for evRows.Next() {
			var idx int
			var kind string
			var n int64
			if err := evRows.Scan(&idx, &kind, &n); err != nil {
				evRows.Close()
				return err
			}
			ss, ok := perStep[idx]
			if !ok {
				continue
			}
			switch kind {
			case EventWaitPassed, EventBranchTrue, EventSendQueued:
				ss.Passed += n
			case EventSendSent:
				ss.Sent += n
			case EventSendSuppressed:
				ss.Suppressed += n
			case EventSendSkipped:
				ss.Skipped += n
				ss.Passed += n
			case EventSendFailed:
				ss.Failed += n
			case EventBranchFalse, EventExited:
				ss.Exited += n
			}
		}
		evRows.Close()
		if err := evRows.Err(); err != nil {
			return err
		}

		for i := range j.Steps {
			stats.PerStep = append(stats.PerStep, *perStep[i])
		}
		return nil
	})
	return stats, err
}
