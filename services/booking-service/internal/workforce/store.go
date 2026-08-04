package workforce

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Store persists workforce rows. Same packaging idiom as the W16 devices /
// W19 workorders packages: NewStore wraps an existing pool (tests),
// DialStore opens a small dedicated pool (integrator wiring path — the
// shared store.Store does not expose its pool). maxConns 4: rostering is
// an operator-paced, low-QPS path.
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

// ensureSchema bootstraps the three workforce tables idempotently
// (contract §1: RLS enabled + forced with the tenant_isolation policy,
// guarded by a pg_policies existence check — mirrors
// internal/devices/store.go).
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS shifts (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL,
    agent_id   UUID NOT NULL,
    starts_at  TIMESTAMPTZ NOT NULL,
    ends_at    TIMESTAMPTZ NOT NULL,
    role       TEXT,
    status     TEXT NOT NULL DEFAULT 'scheduled'
               CHECK (status IN ('scheduled','confirmed','completed','no_show','cancelled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);
CREATE INDEX IF NOT EXISTS idx_shifts_agent ON shifts (tenant_id, agent_id, starts_at);
CREATE INDEX IF NOT EXISTS idx_shifts_window ON shifts (tenant_id, starts_at);
ALTER TABLE shifts ENABLE ROW LEVEL SECURITY;
ALTER TABLE shifts FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'shifts' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON shifts
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;
CREATE TABLE IF NOT EXISTS time_entries (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    agent_id    UUID NOT NULL,
    clock_in_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    clock_out_at TIMESTAMPTZ,
    method      TEXT NOT NULL DEFAULT 'web'
                CHECK (method IN ('web','field_pwa')),
    gps_lat     DOUBLE PRECISION,
    gps_lng     DOUBLE PRECISION
);
CREATE INDEX IF NOT EXISTS idx_time_entries_agent ON time_entries (tenant_id, agent_id, clock_in_at);
-- One open entry per agent, enforced at the database level (SPEC-W20) so
-- concurrent clock-ins cannot race past the application-level guard.
CREATE UNIQUE INDEX IF NOT EXISTS ux_time_entries_open ON time_entries (tenant_id, agent_id) WHERE clock_out_at IS NULL;
ALTER TABLE time_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE time_entries FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'time_entries' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON time_entries
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;
CREATE TABLE IF NOT EXISTS leave_requests (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL,
    agent_id   UUID NOT NULL,
    kind       TEXT NOT NULL
               CHECK (kind IN ('annual','sick','unpaid')),
    starts_on  DATE NOT NULL,
    ends_on    DATE NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending','approved','declined')),
    reason     TEXT,
    decided_by TEXT,
    decided_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (ends_on >= starts_on)
);
CREATE INDEX IF NOT EXISTS idx_leave_requests_agent ON leave_requests (tenant_id, agent_id, starts_on);
CREATE INDEX IF NOT EXISTS idx_leave_requests_status ON leave_requests (tenant_id, status);
ALTER TABLE leave_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_requests FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'leave_requests' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON leave_requests
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure workforce tables: %w", err)
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

// ---------------------------------------------------------------------------
// team members (agents) — the core booking table, read-only here (mirrors
// the helpdesk auto-assign lookup: agent ids MUST resolve to active team
// members of the tenant)
// ---------------------------------------------------------------------------

// agentActive reports whether agentID is an ACTIVE team member of the
// tenant. Runs inside the caller's withTenant transaction (team_members
// carries the same tenant_isolation RLS).
func agentActive(ctx context.Context, tx pgx.Tx, tenantID, agentID uuid.UUID) (bool, error) {
	var one int
	err := tx.QueryRow(ctx,
		`SELECT 1 FROM team_members WHERE tenant_id=$1 AND id=$2 AND active`,
		tenantID, agentID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// requireAgent validates that agentID resolves to an active team member
// (ErrInvalidInput otherwise — the API answers 400, like helpdesk's
// explicit-assignee validation).
func requireAgent(ctx context.Context, tx pgx.Tx, tenantID, agentID uuid.UUID) error {
	ok, err := agentActive(ctx, tx, tenantID, agentID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: agent_id %s is not an active team member", ErrInvalidInput, agentID)
	}
	return nil
}

// ListTeamMembers returns the tenant's active team members for the agent
// pickers (GET /v1/workforce/team-members — read-only projection of the
// core team_members table; mirrors helpdesk.Store.ListTeamMembers).
func (s *Store) ListTeamMembers(ctx context.Context, tenantID uuid.UUID) ([]TeamMemberView, error) {
	out := []TeamMemberView{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, name, COALESCE(email, '') FROM team_members
			 WHERE tenant_id=$1 AND active ORDER BY name`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m TeamMemberView
			if err := rows.Scan(&m.ID, &m.Name, &m.Email); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// shifts
// ---------------------------------------------------------------------------

const shiftCols = `id, tenant_id, agent_id, starts_at, ends_at, COALESCE(role, ''), status, created_at, updated_at`

func scanShift(row pgx.Row) (Shift, error) {
	var s Shift
	err := row.Scan(&s.ID, &s.TenantID, &s.AgentID, &s.StartsAt, &s.EndsAt, &s.Role, &s.Status, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// findOverlap returns the id of a NON-CANCELLED shift of the same agent
// overlapping [startsAt, endsAt) (excluding excludeID — the shift being
// moved). uuid.Nil + nil error means no conflict. Half-open overlap:
// [a_start, a_end) ∩ [b_start, b_end) ≠ ∅ ⟺ a_start < b_end ∧ a_end >
// b_start (back-to-back shifts are legal).
func findOverlap(ctx context.Context, tx pgx.Tx, tenantID, agentID, excludeID uuid.UUID, startsAt, endsAt time.Time) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id FROM shifts
		  WHERE tenant_id=$1 AND agent_id=$2 AND status != 'cancelled'
		    AND id != $3 AND starts_at < $5 AND ends_at > $4
		  ORDER BY starts_at LIMIT 1`,
		tenantID, agentID, excludeID, startsAt, endsAt).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// CreateShift inserts a new shift after validating the agent (active team
// member) and the overlap guard (SPEC-W20: overlap → OverlapError carrying
// the conflicting shift id). The ID/timestamps are stamped back onto sh.
func (s *Store) CreateShift(ctx context.Context, sh *Shift) error {
	return s.withTenant(ctx, sh.TenantID, func(tx pgx.Tx) error {
		if err := requireAgent(ctx, tx, sh.TenantID, sh.AgentID); err != nil {
			return err
		}
		if sh.Status != ShiftCancelled {
			conflict, err := findOverlap(ctx, tx, sh.TenantID, sh.AgentID, uuid.Nil, sh.StartsAt, sh.EndsAt)
			if err != nil {
				return err
			}
			if conflict != uuid.Nil {
				return OverlapError{ConflictShiftID: conflict}
			}
		}
		const q = `INSERT INTO shifts (id, tenant_id, agent_id, starts_at, ends_at, role, status, created_at, updated_at)
			           VALUES (COALESCE($1, gen_random_uuid()), $2,$3,$4,$5,NULLIF($6,''),$7,now(),now())
			           RETURNING id, created_at, updated_at`
		var id any
		if sh.ID != uuid.Nil {
			id = sh.ID
		}
		return tx.QueryRow(ctx, q, id, sh.TenantID, sh.AgentID, sh.StartsAt, sh.EndsAt, sh.Role, sh.Status).
			Scan(&sh.ID, &sh.CreatedAt, &sh.UpdatedAt)
	})
}

// GetShift loads one shift scoped to the tenant (ErrNotFound when missing
// or cross-tenant).
func (s *Store) GetShift(ctx context.Context, tenantID, id uuid.UUID) (Shift, error) {
	var sh Shift
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+shiftCols+` FROM shifts WHERE tenant_id=$1 AND id=$2`, tenantID, id)
		var err error
		sh, err = scanShift(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return sh, err
}

// ShiftFilters scopes ListShifts ("" / nil disables a filter).
type ShiftFilters struct {
	AgentID *uuid.UUID
	Status  string
	From    *time.Time // shifts overlapping [from, to): starts_at < to
	To      *time.Time // and ends_at > from
	Limit   int        // 0 → 200 (default cap)
}

// ListShifts returns the tenant's shifts (soonest first) per the filters.
// Backs GET /v1/workforce/shifts.
func (s *Store) ListShifts(ctx context.Context, tenantID uuid.UUID, f ShiftFilters) ([]Shift, error) {
	q := `SELECT ` + shiftCols + ` FROM shifts WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if f.AgentID != nil {
		n++
		q += fmt.Sprintf(` AND agent_id=$%d`, n)
		args = append(args, *f.AgentID)
	}
	if f.Status != "" {
		n++
		q += fmt.Sprintf(` AND status=$%d`, n)
		args = append(args, f.Status)
	}
	if f.From != nil {
		n++
		q += fmt.Sprintf(` AND ends_at > $%d`, n)
		args = append(args, *f.From)
	}
	if f.To != nil {
		n++
		q += fmt.Sprintf(` AND starts_at < $%d`, n)
		args = append(args, *f.To)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	n++
	q += fmt.Sprintf(` ORDER BY starts_at ASC LIMIT $%d`, n)
	args = append(args, limit)

	out := []Shift{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			sh, err := scanShift(rows)
			if err != nil {
				return err
			}
			out = append(out, sh)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateShift persists every mutable field of sh (status transitions are
// validated by the caller). When the resulting shift is non-cancelled the
// overlap guard re-runs (excluding sh itself — moving a shift onto another
// of the same agent's shifts is a 409). ErrNotFound when the row is
// missing/cross-tenant.
func (s *Store) UpdateShift(ctx context.Context, sh *Shift) error {
	return s.withTenant(ctx, sh.TenantID, func(tx pgx.Tx) error {
		if err := requireAgent(ctx, tx, sh.TenantID, sh.AgentID); err != nil {
			return err
		}
		if sh.Status != ShiftCancelled {
			conflict, err := findOverlap(ctx, tx, sh.TenantID, sh.AgentID, sh.ID, sh.StartsAt, sh.EndsAt)
			if err != nil {
				return err
			}
			if conflict != uuid.Nil {
				return OverlapError{ConflictShiftID: conflict}
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE shifts SET
				agent_id=$3, starts_at=$4, ends_at=$5, role=NULLIF($6,''), status=$7, updated_at=now()
			WHERE tenant_id=$1 AND id=$2`,
			sh.TenantID, sh.ID, sh.AgentID, sh.StartsAt, sh.EndsAt, sh.Role, sh.Status)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// WeekShifts returns the shifts overlapping [start, end) with agent names
// (LEFT JOIN team_members — the withTenant context covers its RLS),
// ordered by agent name then start. Backs GET /v1/workforce/shifts/week.
func (s *Store) WeekShifts(ctx context.Context, tenantID uuid.UUID, start, end time.Time, agentID *uuid.UUID) ([]ShiftView, error) {
	q := `SELECT s.id, s.tenant_id, s.agent_id, s.starts_at, s.ends_at, COALESCE(s.role, ''), s.status,
	             s.created_at, s.updated_at, COALESCE(tm.name, '')
	      FROM shifts s
	      LEFT JOIN team_members tm ON tm.tenant_id = s.tenant_id AND tm.id = s.agent_id
	      WHERE s.tenant_id=$1 AND s.starts_at < $3 AND s.ends_at > $2`
	args := []any{tenantID, start, end}
	if agentID != nil {
		q += ` AND s.agent_id = $4`
		args = append(args, *agentID)
	}
	q += ` ORDER BY tm.name ASC NULLS LAST, s.starts_at ASC LIMIT 1000`

	out := []ShiftView{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v ShiftView
			if err := rows.Scan(&v.ID, &v.TenantID, &v.AgentID, &v.StartsAt, &v.EndsAt, &v.Role, &v.Status,
				&v.CreatedAt, &v.UpdatedAt, &v.AgentName); err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// time entries (clock in/out)
// ---------------------------------------------------------------------------

const timeEntryCols = `id, tenant_id, agent_id, clock_in_at, clock_out_at, method, gps_lat, gps_lng`

func scanTimeEntry(row pgx.Row) (TimeEntry, error) {
	var e TimeEntry
	err := row.Scan(&e.ID, &e.TenantID, &e.AgentID, &e.ClockInAt, &e.ClockOutAt, &e.Method, &e.GPSLat, &e.GPSLng)
	return e, err
}

// ClockIn opens a new time entry for the agent (SPEC-W20: one open entry
// per agent — a second clock-in while open → OpenEntryError carrying the
// open entry id, 409 at the API). The agent must be an active team member.
func (s *Store) ClockIn(ctx context.Context, e *TimeEntry) error {
	return s.withTenant(ctx, e.TenantID, func(tx pgx.Tx) error {
		if err := requireAgent(ctx, tx, e.TenantID, e.AgentID); err != nil {
			return err
		}
		var openID uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT id FROM time_entries
			  WHERE tenant_id=$1 AND agent_id=$2 AND clock_out_at IS NULL
			  ORDER BY clock_in_at DESC LIMIT 1`,
			e.TenantID, e.AgentID).Scan(&openID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			return OpenEntryError{EntryID: openID}
		}
		if e.ClockInAt.IsZero() {
			e.ClockInAt = time.Now().UTC()
		}
		const q = `INSERT INTO time_entries (` + timeEntryCols + `)
			           VALUES (gen_random_uuid(), $1,$2,$3,NULL,$4,$5,$6)
			           RETURNING id`
		err = tx.QueryRow(ctx, q, e.TenantID, e.AgentID, e.ClockInAt, e.Method, e.GPSLat, e.GPSLng).Scan(&e.ID)
		if isUniqueViolation(err) {
			// Lost the race against a concurrent clock-in (ux_time_entries_open)
			// — resolve the winner so the 409 carries the open entry id.
			var winner uuid.UUID
			if serr := tx.QueryRow(ctx,
				`SELECT id FROM time_entries
				  WHERE tenant_id=$1 AND agent_id=$2 AND clock_out_at IS NULL
				  ORDER BY clock_in_at DESC LIMIT 1`,
				e.TenantID, e.AgentID).Scan(&winner); serr == nil {
				return OpenEntryError{EntryID: winner}
			}
			return OpenEntryError{}
		}
		return err
	})
}

// ClockOut closes the agent's open time entry (ErrNoOpenEntry when none —
// 404 at the API). The closed entry is returned with clock_out_at stamped.
func (s *Store) ClockOut(ctx context.Context, tenantID, agentID uuid.UUID) (TimeEntry, error) {
	var e TimeEntry
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`UPDATE time_entries SET clock_out_at=now()
			  WHERE id = (SELECT id FROM time_entries
			               WHERE tenant_id=$1 AND agent_id=$2 AND clock_out_at IS NULL
			               ORDER BY clock_in_at DESC LIMIT 1)
			RETURNING `+timeEntryCols, tenantID, agentID)
		var err error
		e, err = scanTimeEntry(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoOpenEntry
		}
		return err
	})
	return e, err
}

// TimeEntryFilters scopes ListTimeEntries.
type TimeEntryFilters struct {
	AgentID *uuid.UUID
	From    *time.Time // clock_in_at >= from
	To      *time.Time // clock_in_at < to
	Limit   int        // 0 → 200
}

// ListTimeEntries returns time entries (newest first) per the filters.
// Backs GET /v1/workforce/time/entries.
func (s *Store) ListTimeEntries(ctx context.Context, tenantID uuid.UUID, f TimeEntryFilters) ([]TimeEntry, error) {
	q := `SELECT ` + timeEntryCols + ` FROM time_entries WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if f.AgentID != nil {
		n++
		q += fmt.Sprintf(` AND agent_id=$%d`, n)
		args = append(args, *f.AgentID)
	}
	if f.From != nil {
		n++
		q += fmt.Sprintf(` AND clock_in_at >= $%d`, n)
		args = append(args, *f.From)
	}
	if f.To != nil {
		n++
		q += fmt.Sprintf(` AND clock_in_at < $%d`, n)
		args = append(args, *f.To)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	n++
	q += fmt.Sprintf(` ORDER BY clock_in_at DESC LIMIT $%d`, n)
	args = append(args, limit)

	out := []TimeEntry{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanTimeEntry(rows)
			if err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// leave requests
// ---------------------------------------------------------------------------

const leaveCols = `id, tenant_id, agent_id, kind, starts_on, ends_on, status, COALESCE(reason, ''),
	COALESCE(decided_by, ''), decided_at, created_at, updated_at`

func scanLeave(row pgx.Row) (LeaveRequest, error) {
	var l LeaveRequest
	err := row.Scan(&l.ID, &l.TenantID, &l.AgentID, &l.Kind, &l.StartsOn, &l.EndsOn, &l.Status, &l.Reason,
		&l.DecidedBy, &l.DecidedAt, &l.CreatedAt, &l.UpdatedAt)
	return l, err
}

// CreateLeave inserts a pending leave request (the agent must be an active
// team member). ID/timestamps are stamped back onto l.
func (s *Store) CreateLeave(ctx context.Context, l *LeaveRequest) error {
	return s.withTenant(ctx, l.TenantID, func(tx pgx.Tx) error {
		if err := requireAgent(ctx, tx, l.TenantID, l.AgentID); err != nil {
			return err
		}
		const q = `INSERT INTO leave_requests (tenant_id, agent_id, kind, starts_on, ends_on, status, reason)
			           VALUES ($1,$2,$3,$4,$5,'pending',NULLIF($6,''))
			           RETURNING id, status, created_at, updated_at`
		return tx.QueryRow(ctx, q, l.TenantID, l.AgentID, l.Kind, l.StartsOn, l.EndsOn, l.Reason).
			Scan(&l.ID, &l.Status, &l.CreatedAt, &l.UpdatedAt)
	})
}

// LeaveFilters scopes ListLeave.
type LeaveFilters struct {
	AgentID *uuid.UUID
	Status  string
	Limit   int // 0 → 200
}

// ListLeave returns leave requests (newest starts_on first) per the
// filters. Backs GET /v1/workforce/leave.
func (s *Store) ListLeave(ctx context.Context, tenantID uuid.UUID, f LeaveFilters) ([]LeaveRequest, error) {
	q := `SELECT ` + leaveCols + ` FROM leave_requests WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if f.AgentID != nil {
		n++
		q += fmt.Sprintf(` AND agent_id=$%d`, n)
		args = append(args, *f.AgentID)
	}
	if f.Status != "" {
		n++
		q += fmt.Sprintf(` AND status=$%d`, n)
		args = append(args, f.Status)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	n++
	q += fmt.Sprintf(` ORDER BY starts_on DESC LIMIT $%d`, n)
	args = append(args, limit)

	out := []LeaveRequest{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			l, err := scanLeave(rows)
			if err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}

// GetLeave loads one leave request scoped to the tenant.
func (s *Store) GetLeave(ctx context.Context, tenantID, id uuid.UUID) (LeaveRequest, error) {
	var l LeaveRequest
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+leaveCols+` FROM leave_requests WHERE tenant_id=$1 AND id=$2`, tenantID, id)
		var err error
		l, err = scanLeave(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return l, err
}

// DecideLeave records an approve|decline decision on a PENDING request
// (SPEC-W20: decided_by is the JWT sub, decided_at is now). Atomic on the
// pending guard: ErrNotFound when the row is missing, ErrInvalidTransition
// when it was already decided (409 at the API).
func (s *Store) DecideLeave(ctx context.Context, tenantID, id uuid.UUID, decision, decidedBy string) (LeaveRequest, error) {
	var l LeaveRequest
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`UPDATE leave_requests SET status=$3, decided_by=NULLIF($4,''), decided_at=now(), updated_at=now()
			  WHERE tenant_id=$1 AND id=$2 AND status='pending'
			RETURNING `+leaveCols, tenantID, id, decision, decidedBy)
		var err error
		l, err = scanLeave(row)
		if errors.Is(err, pgx.ErrNoRows) {
			// Distinguish missing from already-decided.
			var status string
			serr := tx.QueryRow(ctx,
				`SELECT status FROM leave_requests WHERE tenant_id=$1 AND id=$2`, tenantID, id).Scan(&status)
			if errors.Is(serr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if serr != nil {
				return serr
			}
			return fmt.Errorf("%w: leave request already %s", ErrInvalidTransition, status)
		}
		return err
	})
	return l, err
}

// ---------------------------------------------------------------------------
// coverage & utilization (read-only reporting)
// ---------------------------------------------------------------------------

// Coverage returns one row per day in [from, to] (inclusive dates,
// tenant-local days computed by the handler): distinct agents with a
// non-cancelled shift overlapping the day vs bookings starting that day
// (cancelled bookings excluded). Read-only join against the core bookings
// table; when the bookings table is absent (partial deployment) the
// bookings count degrades to 0 instead of erroring.
func (s *Store) Coverage(ctx context.Context, tenantID uuid.UUID, from, to time.Time) ([]CoverageDay, error) {
	out := []CoverageDay{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		hasBookings, err := tableExists(ctx, tx, "bookings")
		if err != nil {
			return err
		}
		bookingsExpr := `0`
		if hasBookings {
			bookingsExpr = `(SELECT COUNT(*) FROM bookings b
			                  WHERE b.tenant_id=$1 AND b.status != 'cancelled'
			                    AND b.starts_at >= d AND b.starts_at < d + interval '1 day')`
		}
		// Days are UTC midnights (the handler parses from/to as UTC);
		// labelling pins to UTC explicitly so the session timezone of the
		// server can never shift a day boundary.
		q := `SELECT to_char(d AT TIME ZONE 'UTC', 'YYYY-MM-DD'),
		       (SELECT COUNT(DISTINCT s.agent_id) FROM shifts s
		         WHERE s.tenant_id=$1 AND s.status != 'cancelled'
		           AND s.starts_at < d + interval '1 day' AND s.ends_at > d),
		       ` + bookingsExpr + `
		FROM generate_series($2::timestamptz, $3::timestamptz, interval '1 day') AS d
		ORDER BY d`
		rows, err := tx.Query(ctx, q, tenantID, from, to)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var day CoverageDay
			if err := rows.Scan(&day.Date, &day.AgentsScheduled, &day.Bookings); err != nil {
				return err
			}
			out = append(out, day)
		}
		return rows.Err()
	})
	return out, err
}

// tableExists reports whether a public-schema table is present (defensive
// read for optional join sources).
func tableExists(ctx context.Context, tx pgx.Tx, name string) (bool, error) {
	var reg *string
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1)::text`, "public."+name).Scan(&reg); err != nil {
		return false, err
	}
	return reg != nil, nil
}

// Utilization returns one row per active agent with scheduled or clocked
// activity in [from, to): scheduled hours (non-cancelled shifts, clipped
// to the range), clocked hours (time entries, clipped; entries still open
// are counted to NOW and flagged via OpenEntries — SPEC-W20), and the
// utilization percentage (null when scheduled == 0).
func (s *Store) Utilization(ctx context.Context, tenantID uuid.UUID, from, to time.Time) ([]UtilizationRow, error) {
	const q = `
WITH sched AS (
    SELECT agent_id,
           SUM(EXTRACT(EPOCH FROM (LEAST(ends_at, $3) - GREATEST(starts_at, $2)))) / 3600.0 AS hours
      FROM shifts
     WHERE tenant_id=$1 AND status != 'cancelled'
       AND starts_at < $3 AND ends_at > $2
     GROUP BY agent_id
),
clocked AS (
    SELECT agent_id,
           SUM(EXTRACT(EPOCH FROM (LEAST(COALESCE(clock_out_at, now()), $3) - GREATEST(clock_in_at, $2)))) / 3600.0 AS hours,
           COUNT(*) FILTER (WHERE clock_out_at IS NULL) AS open
      FROM time_entries
     WHERE tenant_id=$1
       AND clock_in_at < $3 AND COALESCE(clock_out_at, now()) > $2
     GROUP BY agent_id
)
SELECT tm.id, tm.name, COALESCE(s.hours, 0), COALESCE(c.hours, 0), COALESCE(c.open, 0)
  FROM team_members tm
  LEFT JOIN sched s ON s.agent_id = tm.id
  LEFT JOIN clocked c ON c.agent_id = tm.id
 WHERE tm.tenant_id=$1 AND tm.active
   AND (s.hours IS NOT NULL OR c.hours IS NOT NULL)
 ORDER BY tm.name`
	out := []UtilizationRow{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID, from, to)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r UtilizationRow
			if err := rows.Scan(&r.AgentID, &r.AgentName, &r.ScheduledHours, &r.ClockedHours, &r.OpenEntries); err != nil {
				return err
			}
			if r.ScheduledHours > 0 {
				pct := r.ClockedHours / r.ScheduledHours * 100
				r.UtilizationPct = &pct
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// EnqueueOutbox appends one row to the transactional outbox (mirrors
// workorders.Store.EnqueueOutbox; lifecycle events ride this path — the
// W5 outbox dispatcher drains rows to Kafka via Dapr).
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
