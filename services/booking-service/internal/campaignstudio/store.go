package campaignstudio

// Store: studio_segments / studio_journeys / studio_enrollments /
// studio_step_events, all FORCE-RLS tenant_isolation (the devices/store.go
// idiom: idempotent ensureSchema, pg_policies-guarded policy, SET LOCAL
// app.tenant_id inside withTenant). Packaging mirrors internal/helpdesk:
// NewStore wraps an existing pool (tests), DialStore opens a small
// dedicated pool (main wiring path). maxConns 4: studio is a low-QPS
// operator path (segment counts + step batches, not hot traffic).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a row does not exist (mirrors
// store.ErrNotFound so httpapi can map both to 404).
var ErrNotFound = errors.New("not found")

// segmentCountCeiling bounds one segment-count evaluation: a scan cap of
// 100k rows keeps interactive previews predictable (bounded LIMIT
// subquery); truncated reports the ceiling was hit.
const segmentCountCeiling = 100000

// StepBatchSizeDefault is the default enrollment advance limit per step
// call (STUDIO_STEP_BATCH env overrides via the integrator config).
const StepBatchSizeDefault = 200

// Store persists the campaign-studio tables.
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

// ensureSchema bootstraps the campaign-studio tables idempotently: RLS
// enabled + forced with the tenant_isolation policy, guarded by a
// pg_policies existence check (the devices/store.go pattern).
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS studio_segments (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    name        TEXT NOT NULL,
    definition  JSONB NOT NULL DEFAULT '{}',
    approx_count BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_studio_segments_tenant ON studio_segments (tenant_id);
ALTER TABLE studio_segments ENABLE ROW LEVEL SECURITY;
ALTER TABLE studio_segments FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'studio_segments' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON studio_segments
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
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
    segment_id   UUID REFERENCES studio_segments (id),
    steps        JSONB NOT NULL DEFAULT '[]',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_studio_journeys_tenant ON studio_journeys (tenant_id, status);
ALTER TABLE studio_journeys ENABLE ROW LEVEL SECURITY;
ALTER TABLE studio_journeys FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'studio_journeys' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON studio_journeys
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS studio_enrollments (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL,
    journey_id    UUID NOT NULL REFERENCES studio_journeys (id),
    contact_id    UUID NOT NULL,
    step_idx      INT NOT NULL DEFAULT 0,
    state         TEXT NOT NULL DEFAULT 'active'
                  CHECK (state IN ('active','completed','exited')),
    enrolled_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_step_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    exited_reason TEXT,
    UNIQUE (tenant_id, journey_id, contact_id)
);
CREATE INDEX IF NOT EXISTS idx_studio_enrollments_due ON studio_enrollments (tenant_id, journey_id, state, step_idx);
ALTER TABLE studio_enrollments ENABLE ROW LEVEL SECURITY;
ALTER TABLE studio_enrollments FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'studio_enrollments' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON studio_enrollments
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS studio_step_events (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL,
    journey_id    UUID NOT NULL REFERENCES studio_journeys (id),
    enrollment_id UUID NOT NULL REFERENCES studio_enrollments (id),
    step_idx      INT NOT NULL,
    kind          TEXT NOT NULL,
    payload       JSONB NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_studio_step_events_journey ON studio_step_events (tenant_id, journey_id, step_idx);
ALTER TABLE studio_step_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE studio_step_events FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'studio_step_events' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON studio_step_events
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure campaign-studio tables: %w", err)
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

// EnqueueOutbox appends one row to the shared transactional outbox
// (drained to Kafka by the W5 outbox dispatcher via Dapr; mirrors
// helpdesk.Store.EnqueueOutbox).
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

// CreateSegment inserts one segment after validating its definition.
func (s *Store) CreateSegment(ctx context.Context, seg *Segment) error {
	if err := ValidateSegmentDefinition(&seg.Definition); err != nil {
		return err
	}
	seg.Name = strings.TrimSpace(seg.Name)
	if seg.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if len(seg.Name) > 200 {
		return fmt.Errorf("%w: name exceeds 200 bytes", ErrInvalidInput)
	}
	return s.withTenant(ctx, seg.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO studio_segments (tenant_id, name, definition)
			 VALUES ($1,$2,$3) RETURNING `+segmentCols,
			seg.TenantID, seg.Name, seg.Definition).
			Scan(&seg.ID, &seg.TenantID, &seg.Name, &seg.Definition,
				&seg.ApproxCount, &seg.CreatedAt, &seg.UpdatedAt)
	})
}

// GetSegment fetches one segment by id (tenant-scoped).
func (s *Store) GetSegment(ctx context.Context, tenantID, id uuid.UUID) (Segment, error) {
	var seg Segment
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		seg, err = scanSegment(tx.QueryRow(ctx,
			`SELECT `+segmentCols+` FROM studio_segments WHERE id=$1`, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Segment{}, ErrNotFound
	}
	return seg, err
}

// ListSegments lists all segments of a tenant (newest first).
func (s *Store) ListSegments(ctx context.Context, tenantID uuid.UUID) ([]Segment, error) {
	out := []Segment{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+segmentCols+` FROM studio_segments ORDER BY created_at DESC`)
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

// UpdateSegment replaces name/definition of one segment (the count is a
// derived value — refreshed by CountSegment).
func (s *Store) UpdateSegment(ctx context.Context, seg *Segment) error {
	if err := ValidateSegmentDefinition(&seg.Definition); err != nil {
		return err
	}
	seg.Name = strings.TrimSpace(seg.Name)
	if seg.Name == "" || len(seg.Name) > 200 {
		return fmt.Errorf("%w: name must be 1-200 bytes", ErrInvalidInput)
	}
	return s.withTenant(ctx, seg.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE studio_segments SET name=$2, definition=$3, updated_at=now() WHERE id=$1`,
			seg.ID, seg.Name, seg.Definition)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// CountSegment evaluates a segment definition against contacts and stores
// the result in approx_count. The scan is capped at segmentCountCeiling
// rows (100k) via a bounded subquery; truncated=true reports the ceiling
// was hit so operators can narrow the definition.
func (s *Store) CountSegment(ctx context.Context, tenantID, id uuid.UUID) (count int64, truncated bool, err error) {
	seg, err := s.GetSegment(ctx, tenantID, id)
	if err != nil {
		return 0, false, err
	}
	sql, args, err := buildCountQuery(&seg.Definition, segmentCountCeiling+1)
	if err != nil {
		return 0, false, err
	}
	var n int64
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			return err
		}
		if n > segmentCountCeiling {
			n = segmentCountCeiling
			truncated = true
		}
		if _, err := tx.Exec(ctx,
			`UPDATE studio_segments SET approx_count=$2, updated_at=now() WHERE id=$1`,
			id, n); err != nil {
			return err
		}
		return nil
	})
	return n, truncated, err
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

// CreateJourney inserts one journey (status draft).
func (s *Store) CreateJourney(ctx context.Context, j *Journey) error {
	if err := ValidateSteps(j.Steps); err != nil {
		return err
	}
	j.Name = strings.TrimSpace(j.Name)
	if j.Name == "" || len(j.Name) > 200 {
		return fmt.Errorf("%w: name must be 1-200 bytes", ErrInvalidInput)
	}
	if !validTriggerKind(j.TriggerKind) {
		return fmt.Errorf("%w: trigger_kind %q (want segment|manual|event)", ErrInvalidInput, j.TriggerKind)
	}
	if j.TriggerKind == TriggerSegment && j.SegmentID == nil {
		return fmt.Errorf("%w: segment_id is required for trigger_kind segment", ErrInvalidInput)
	}
	j.Status = StatusDraft
	return s.withTenant(ctx, j.TenantID, func(tx pgx.Tx) error {
		if j.SegmentID != nil {
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM studio_segments WHERE id=$1)`, *j.SegmentID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("%w: segment %s not found", ErrInvalidInput, *j.SegmentID)
			}
		}
		return tx.QueryRow(ctx,
			`INSERT INTO studio_journeys (tenant_id, name, trigger_kind, segment_id, steps)
			 VALUES ($1,$2,$3,$4,$5) RETURNING `+journeyCols,
			j.TenantID, j.Name, j.TriggerKind, j.SegmentID, j.Steps).
			Scan(&j.ID, &j.TenantID, &j.Name, &j.Status, &j.TriggerKind,
				&j.SegmentID, &j.Steps, &j.CreatedAt, &j.UpdatedAt)
	})
}

// GetJourney fetches one journey by id (tenant-scoped).
func (s *Store) GetJourney(ctx context.Context, tenantID, id uuid.UUID) (Journey, error) {
	var j Journey
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		j, err = scanJourney(tx.QueryRow(ctx,
			`SELECT `+journeyCols+` FROM studio_journeys WHERE id=$1`, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Journey{}, ErrNotFound
	}
	return j, err
}

// ListJourneys lists journeys, optionally filtered by status.
func (s *Store) ListJourneys(ctx context.Context, tenantID uuid.UUID, status string) ([]Journey, error) {
	out := []Journey{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+journeyCols+` FROM studio_journeys
			 WHERE ($1='' OR status=$1) ORDER BY created_at DESC`, status)
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

// UpdateJourney replaces the mutable fields of one journey (name,
// trigger_kind, segment_id, steps) — allowed only while draft|paused.
// Status transitions go through SetJourneyStatus.
func (s *Store) UpdateJourney(ctx context.Context, j *Journey) error {
	if err := ValidateSteps(j.Steps); err != nil {
		return err
	}
	j.Name = strings.TrimSpace(j.Name)
	if j.Name == "" || len(j.Name) > 200 {
		return fmt.Errorf("%w: name must be 1-200 bytes", ErrInvalidInput)
	}
	if !validTriggerKind(j.TriggerKind) {
		return fmt.Errorf("%w: trigger_kind %q (want segment|manual|event)", ErrInvalidInput, j.TriggerKind)
	}
	if j.TriggerKind == TriggerSegment && j.SegmentID == nil {
		return fmt.Errorf("%w: segment_id is required for trigger_kind segment", ErrInvalidInput)
	}
	return s.withTenant(ctx, j.TenantID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx,
			`SELECT status FROM studio_journeys WHERE id=$1 FOR UPDATE`, j.ID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != StatusDraft && status != StatusPaused {
			return fmt.Errorf("%w: structural edits require draft|paused (status is %s)", ErrConflict, status)
		}
		if j.SegmentID != nil {
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM studio_segments WHERE id=$1)`, *j.SegmentID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("%w: segment %s not found", ErrInvalidInput, *j.SegmentID)
			}
		}
		_, err = tx.Exec(ctx,
			`UPDATE studio_journeys SET name=$2, trigger_kind=$3, segment_id=$4, steps=$5, updated_at=now()
			 WHERE id=$1`,
			j.ID, j.Name, j.TriggerKind, j.SegmentID, j.Steps)
		return err
	})
}

// SetJourneyStatus applies a status-machine transition (409 on illegal
// edges; same-status is a no-op success).
func (s *Store) SetJourneyStatus(ctx context.Context, tenantID, id uuid.UUID, to string) (Journey, error) {
	var j Journey
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var from string
		err := tx.QueryRow(ctx,
			`SELECT status FROM studio_journeys WHERE id=$1 FOR UPDATE`, id).Scan(&from)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if from == to {
			var err error
			j, err = scanJourney(tx.QueryRow(ctx,
				`SELECT `+journeyCols+` FROM studio_journeys WHERE id=$1`, id))
			return err
		}
		if !CanTransition(from, to) {
			return fmt.Errorf("%w: journey status %s → %s", ErrConflict, from, to)
		}
		return tx.QueryRow(ctx,
			`UPDATE studio_journeys SET status=$2, updated_at=now() WHERE id=$1 RETURNING `+journeyCols,
			id, to).
			Scan(&j.ID, &j.TenantID, &j.Name, &j.Status, &j.TriggerKind,
				&j.SegmentID, &j.Steps, &j.CreatedAt, &j.UpdatedAt)
	})
	return j, err
}

// ---------------------------------------------------------------------------
// Enrollments
// ---------------------------------------------------------------------------

// Enroll inserts enrollments for the given contacts, idempotently
// (UNIQUE(tenant_id, journey_id, contact_id) + ON CONFLICT DO NOTHING).
// Returns (enrolled, existing) counts. The journey must be active.
func (s *Store) Enroll(ctx context.Context, tenantID, journeyID uuid.UUID, contactIDs []uuid.UUID) (enrolled, existing int, err error) {
	if len(contactIDs) == 0 {
		return 0, 0, fmt.Errorf("%w: at least one contact_id is required", ErrInvalidInput)
	}
	if len(contactIDs) > 10000 {
		return 0, 0, fmt.Errorf("%w: at most 10000 contacts per call", ErrInvalidInput)
	}
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx,
			`SELECT status FROM studio_journeys WHERE id=$1 FOR UPDATE`, journeyID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != StatusActive {
			return fmt.Errorf("%w: enrollments require an active journey (status is %s)", ErrConflict, status)
		}
		for _, cid := range contactIDs {
			tag, err := tx.Exec(ctx,
				`INSERT INTO studio_enrollments (tenant_id, journey_id, contact_id)
				 VALUES ($1,$2,$3) ON CONFLICT (tenant_id, journey_id, contact_id) DO NOTHING`,
				tenantID, journeyID, cid)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				enrolled++
			} else {
				existing++
			}
		}
		return nil
	})
	return enrolled, existing, err
}

// JourneyStats is the GET /journeys/{id}/stats payload.
type JourneyStats struct {
	Enrolled  int64             `json:"enrolled"`
	Active    int64             `json:"active"`
	Completed int64             `json:"completed"`
	Exited    int64             `json:"exited"`
	PerStep   []JourneyStepStat `json:"per_step"`
}

// JourneyStepStat aggregates per-step outcomes from studio_step_events +
// the active residencies from studio_enrollments.
type JourneyStepStat struct {
	StepIdx    int    `json:"step_idx"`
	Type       string `json:"type"`
	Active     int64  `json:"active"`
	Passed     int64  `json:"passed"`
	Sent       int64  `json:"sent"`
	Suppressed int64  `json:"suppressed"`
	Skipped    int64  `json:"skipped"`
	Failed     int64  `json:"failed"`
	Exited     int64  `json:"exited"`
}

// Stats aggregates the journey funnel: enrollment states + per-step event
// counts (send_sent / send_suppressed / send_skipped / send_failed /
// wait_passed / branch_* / exited).
func (s *Store) Stats(ctx context.Context, tenantID, journeyID uuid.UUID) (JourneyStats, error) {
	var out JourneyStats
	out.PerStep = []JourneyStepStat{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		j, err := scanJourney(tx.QueryRow(ctx,
			`SELECT `+journeyCols+` FROM studio_journeys WHERE id=$1`, journeyID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx,
			`SELECT state, COUNT(*) FROM studio_enrollments WHERE journey_id=$1 GROUP BY state`,
			journeyID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var state string
			var n int64
			if err := rows.Scan(&state, &n); err != nil {
				return err
			}
			out.Enrolled += n
			switch state {
			case EnrollActive:
				out.Active = n
			case EnrollCompleted:
				out.Completed = n
			case EnrollExited:
				out.Exited = n
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}

		// Per-step: event counts joined with active residencies, in step order.
		eventRows, err := tx.Query(ctx,
			`SELECT step_idx,
			        COUNT(*) FILTER (WHERE kind IN ('wait_passed','branch_true','branch_false')) AS passed,
			        COUNT(*) FILTER (WHERE kind = 'send_sent') AS sent,
			        COUNT(*) FILTER (WHERE kind = 'send_suppressed') AS suppressed,
			        COUNT(*) FILTER (WHERE kind = 'send_skipped') AS skipped,
			        COUNT(*) FILTER (WHERE kind = 'send_failed') AS failed,
			        COUNT(*) FILTER (WHERE kind = 'exited') AS exited
			   FROM studio_step_events WHERE journey_id=$1 GROUP BY step_idx`,
			journeyID)
		if err != nil {
			return err
		}
		defer eventRows.Close()
		byStep := map[int]*JourneyStepStat{}
		for eventRows.Next() {
			st := JourneyStepStat{}
			if err := eventRows.Scan(&st.StepIdx, &st.Passed, &st.Sent, &st.Suppressed,
				&st.Skipped, &st.Failed, &st.Exited); err != nil {
				return err
			}
			byStep[st.StepIdx] = &st
		}
		if err := eventRows.Err(); err != nil {
			return err
		}
		resRows, err := tx.Query(ctx,
			`SELECT step_idx, COUNT(*) FROM studio_enrollments
			   WHERE journey_id=$1 AND state='active' GROUP BY step_idx`,
			journeyID)
		if err != nil {
			return err
		}
		defer resRows.Close()
		for resRows.Next() {
			var idx int
			var n int64
			if err := resRows.Scan(&idx, &n); err != nil {
				return err
			}
			st := byStep[idx]
			if st == nil {
				st = &JourneyStepStat{StepIdx: idx}
				byStep[idx] = st
			}
			st.Active = n
		}
		if err := resRows.Err(); err != nil {
			return err
		}
		for i, step := range j.Steps {
			st := byStep[i]
			if st == nil {
				st = &JourneyStepStat{StepIdx: i}
			}
			st.Type = step.Type
			out.PerStep = append(out.PerStep, *st)
		}
		return nil
	})
	return out, err
}
