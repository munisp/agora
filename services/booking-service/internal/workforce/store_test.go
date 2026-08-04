package workforce

import (
	"context"
	"errors"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SPEC-W20 Agent D store tests run against embedded Postgres (same harness
// as the W19 helpdesk/workorders tests; dedicated port 5564 avoids the
// postmaster.pid race with sibling packages under `go test ./...`; -short
// skips).
//
// The harness also bootstraps the minimal team_members (core booking table
// the package validates agents against — RLS like production), bookings
// (coverage join source) and the shared outbox (no RLS, like production)
// tables — owned by the shared store package in production, mirrored here
// exactly like helpdesk/store_test.go does.

const testSupportDDL = `
CREATE TABLE IF NOT EXISTS team_members (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    email TEXT,
    role TEXT NOT NULL DEFAULT 'staff',
    active BOOLEAN NOT NULL DEFAULT TRUE
);
ALTER TABLE team_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE team_members FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'team_members' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON team_members
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;
CREATE TABLE IF NOT EXISTS bookings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    offering_id UUID,
    team_member_id UUID,
    contact_id UUID,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    source TEXT NOT NULL DEFAULT 'api',
    idempotency_key TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at TIMESTAMPTZ
);`

func newTestStore(t *testing.T) *Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres workforce store test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_workforce_test").
		Port(5564).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })
	st, err := DialStore(context.Background(),
		"postgres://postgres:postgres@localhost:5564/booking_workforce_test?sslmode=disable")
	if err != nil {
		t.Fatalf("DialStore: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.pool.Exec(context.Background(), testSupportDDL); err != nil {
		t.Fatalf("support DDL: %v", err)
	}
	return st
}

// addAgent seeds one team member (superuser connection bypasses RLS for
// seeding; production seeding goes through store.Store).
func addAgent(t *testing.T, st *Store, tenantID uuid.UUID, name string, active bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := st.pool.Exec(context.Background(),
		`INSERT INTO team_members (id, tenant_id, name, active) VALUES ($1,$2,$3,$4)`,
		id, tenantID, name, active)
	if err != nil {
		t.Fatalf("seed team member: %v", err)
	}
	return id
}

func mkShift(tenantID, agentID uuid.UUID, startHour, endHour float64) Shift {
	base := time.Date(2030, 3, 10, 0, 0, 0, 0, time.UTC) // a Monday
	return Shift{
		TenantID: tenantID,
		AgentID:  agentID,
		StartsAt: base.Add(time.Duration(startHour * float64(time.Hour))),
		EndsAt:   base.Add(time.Duration(endHour * float64(time.Hour))),
		Status:   ShiftScheduled,
	}
}

// Shift create → get round-trip; overlap guard (409 contract: the error
// carries the conflicting shift id); back-to-back is legal; cancelled
// shifts never block.
func TestShiftOverlapGuard(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	agent := addAgent(t, st, tenantID, "Ada Lovelace", true)

	morning := mkShift(tenantID, agent, 9, 13) // 09:00–13:00
	if err := st.CreateShift(ctx, &morning); err != nil {
		t.Fatalf("create morning shift: %v", err)
	}
	if morning.ID == uuid.Nil || morning.CreatedAt.IsZero() || morning.UpdatedAt.IsZero() {
		t.Fatalf("id/timestamps not stamped: %+v", morning)
	}

	got, err := st.GetShift(ctx, tenantID, morning.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AgentID != agent || got.Status != ShiftScheduled || !got.StartsAt.Equal(morning.StartsAt) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Overlapping 12:00–17:00 → OverlapError carrying the morning shift id.
	clash := mkShift(tenantID, agent, 12, 17)
	err = st.CreateShift(ctx, &clash)
	var overlap OverlapError
	if !errors.As(err, &overlap) {
		t.Fatalf("overlap err = %v, want OverlapError", err)
	}
	if overlap.ConflictShiftID != morning.ID {
		t.Fatalf("conflict id = %s, want %s", overlap.ConflictShiftID, morning.ID)
	}
	if !errors.Is(err, ErrShiftOverlap) {
		t.Fatalf("errors.Is(ErrShiftOverlap) failed for %v", err)
	}

	// Back-to-back 13:00–17:00 is legal (half-open windows).
	afternoon := mkShift(tenantID, agent, 13, 17)
	if err := st.CreateShift(ctx, &afternoon); err != nil {
		t.Fatalf("back-to-back shift rejected: %v", err)
	}

	// Moving the afternoon shift onto the morning shift → overlap on update.
	afternoon.StartsAt = morning.StartsAt.Add(-time.Hour)
	afternoon.EndsAt = morning.EndsAt.Add(-time.Hour)
	err = st.UpdateShift(ctx, &afternoon)
	if !errors.As(err, &overlap) || overlap.ConflictShiftID != morning.ID {
		t.Fatalf("update overlap = %v (conflict %s)", err, overlap.ConflictShiftID)
	}

	// Cancelling the morning shift frees the window: the same move succeeds.
	morning.Status = ShiftCancelled
	if err := st.UpdateShift(ctx, &morning); err != nil {
		t.Fatalf("cancel morning: %v", err)
	}
	if err := st.UpdateShift(ctx, &afternoon); err != nil {
		t.Fatalf("move onto cancelled window rejected: %v", err)
	}

	// A cancelled shift may itself overlap anything (guard skips cancelled).
	dup := mkShift(tenantID, agent, 9, 13)
	dup.Status = ShiftCancelled
	if err := st.CreateShift(ctx, &dup); err != nil {
		t.Fatalf("cancelled shift create rejected: %v", err)
	}
}

// Cross-tenant reads are empty (app-level + RLS belt-and-braces).
func TestShiftCrossTenant(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	agentA := addAgent(t, st, tenantA, "Ada A", true)

	sh := mkShift(tenantA, agentA, 9, 12)
	if err := st.CreateShift(ctx, &sh); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.GetShift(ctx, tenantB, sh.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant get = %v, want ErrNotFound", err)
	}
	list, err := st.ListShifts(ctx, tenantB, ShiftFilters{})
	if err != nil || len(list) != 0 {
		t.Fatalf("cross-tenant list: %+v, %v", list, err)
	}
}

// Agent validation mirrors the helpdesk team-member lookup: shifts,
// clock-ins and leave requests require an ACTIVE team member.
func TestAgentValidation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	ghost := uuid.New() // never seeded
	inactive := addAgent(t, st, tenantID, "Dormant Dan", false)
	crossTenant := addAgent(t, st, uuid.New(), "Other Org", true)

	for _, agent := range []uuid.UUID{ghost, inactive, crossTenant} {
		sh := mkShift(tenantID, agent, 9, 12)
		if err := st.CreateShift(ctx, &sh); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("shift for %s = %v, want ErrInvalidInput", agent, err)
		}
		e := TimeEntry{TenantID: tenantID, AgentID: agent, Method: MethodWeb}
		if err := st.ClockIn(ctx, &e); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("clock-in for %s = %v, want ErrInvalidInput", agent, err)
		}
		l := LeaveRequest{TenantID: tenantID, AgentID: agent, Kind: LeaveAnnual,
			StartsOn: time.Date(2030, 4, 1, 0, 0, 0, 0, time.UTC), EndsOn: time.Date(2030, 4, 2, 0, 0, 0, 0, time.UTC)}
		if err := st.CreateLeave(ctx, &l); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("leave for %s = %v, want ErrInvalidInput", agent, err)
		}
	}
}

// Clock guards (SPEC-W20): one open entry per agent — clock-in 409 while
// open (error carries the open entry id), clock-out 404 when none.
func TestClockGuards(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	agent := addAgent(t, st, tenantID, "Field Farah", true)

	lat, lng := 6.5244, 3.3792
	first := TimeEntry{TenantID: tenantID, AgentID: agent, Method: MethodFieldPWA, GPSLat: &lat, GPSLng: &lng}
	if err := st.ClockIn(ctx, &first); err != nil {
		t.Fatalf("clock-in: %v", err)
	}
	if first.ID == uuid.Nil || first.ClockInAt.IsZero() {
		t.Fatalf("entry not stamped: %+v", first)
	}

	// Second clock-in while open → OpenEntryError with the open entry id.
	second := TimeEntry{TenantID: tenantID, AgentID: agent, Method: MethodWeb}
	err := st.ClockIn(ctx, &second)
	var openErr OpenEntryError
	if !errors.As(err, &openErr) {
		t.Fatalf("second clock-in = %v, want OpenEntryError", err)
	}
	if openErr.EntryID != first.ID {
		t.Fatalf("open entry id = %s, want %s", openErr.EntryID, first.ID)
	}
	if !errors.Is(err, ErrOpenEntry) {
		t.Fatalf("errors.Is(ErrOpenEntry) failed for %v", err)
	}

	// Clock-out closes the open entry; a second clock-out → ErrNoOpenEntry.
	closed, err := st.ClockOut(ctx, tenantID, agent)
	if err != nil {
		t.Fatalf("clock-out: %v", err)
	}
	if closed.ID != first.ID || closed.ClockOutAt == nil {
		t.Fatalf("closed entry wrong: %+v", closed)
	}
	if closed.Method != MethodFieldPWA || closed.GPSLat == nil || *closed.GPSLat != lat {
		t.Fatalf("method/gps fidelity: %+v", closed)
	}
	if _, err := st.ClockOut(ctx, tenantID, agent); !errors.Is(err, ErrNoOpenEntry) {
		t.Fatalf("second clock-out = %v, want ErrNoOpenEntry", err)
	}

	// After clock-out the agent can clock in again.
	third := TimeEntry{TenantID: tenantID, AgentID: agent, Method: MethodWeb}
	if err := st.ClockIn(ctx, &third); err != nil {
		t.Fatalf("re-clock-in: %v", err)
	}

	// Entries list honors filters.
	entries, err := st.ListTimeEntries(ctx, tenantID, TimeEntryFilters{AgentID: &agent})
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries: %+v, %v", entries, err)
	}
}

// Leave lifecycle: pending → approved (decided_by/at recorded); a second
// decision → ErrInvalidTransition; missing id → ErrNotFound.
func TestLeaveDecision(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	agent := addAgent(t, st, tenantID, "Leave Larry", true)

	l := LeaveRequest{
		TenantID: tenantID, AgentID: agent, Kind: LeaveSick,
		StartsOn: time.Date(2030, 5, 4, 0, 0, 0, 0, time.UTC),
		EndsOn:   time.Date(2030, 5, 6, 0, 0, 0, 0, time.UTC),
		Reason:   "flu",
	}
	if err := st.CreateLeave(ctx, &l); err != nil {
		t.Fatalf("create leave: %v", err)
	}
	if l.ID == uuid.Nil || l.Status != LeavePending {
		t.Fatalf("leave not pending: %+v", l)
	}

	decided, err := st.DecideLeave(ctx, tenantID, l.ID, LeaveApproved, "manager-42")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if decided.Status != LeaveApproved || decided.DecidedBy != "manager-42" || decided.DecidedAt == nil {
		t.Fatalf("decision not recorded: %+v", decided)
	}

	if _, err := st.DecideLeave(ctx, tenantID, l.ID, LeaveDeclined, "manager-42"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("re-decide = %v, want ErrInvalidTransition", err)
	}
	if _, err := st.DecideLeave(ctx, tenantID, uuid.New(), LeaveApproved, "manager-42"); err != ErrNotFound {
		t.Fatalf("missing = %v, want ErrNotFound", err)
	}

	pending, err := st.ListLeave(ctx, tenantID, LeaveFilters{Status: LeavePending})
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending list: %+v, %v", pending, err)
	}
	approved, err := st.ListLeave(ctx, tenantID, LeaveFilters{Status: LeaveApproved})
	if err != nil || len(approved) != 1 || approved[0].Kind != LeaveSick {
		t.Fatalf("approved list: %+v, %v", approved, err)
	}
}

// Week grid joins agent names and clips to the 7-day window.
func TestWeekShifts(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	ada := addAgent(t, st, tenantID, "Ada Week", true)
	bob := addAgent(t, st, tenantID, "Bob Week", true)

	weekStart := time.Date(2030, 3, 10, 0, 0, 0, 0, time.UTC) // Monday
	weekEnd := weekStart.AddDate(0, 0, 7)

	in := mkShift(tenantID, ada, 9, 12)
	if err := st.CreateShift(ctx, &in); err != nil {
		t.Fatalf("in-week shift: %v", err)
	}
	spanning := Shift{TenantID: tenantID, AgentID: bob,
		StartsAt: weekStart.Add(-26 * time.Hour), EndsAt: weekStart.Add(2 * time.Hour), Status: ShiftConfirmed}
	if err := st.CreateShift(ctx, &spanning); err != nil {
		t.Fatalf("spanning shift: %v", err)
	}
	outside := mkShift(tenantID, bob, 9, 12)
	outside.StartsAt = weekEnd.Add(24 * time.Hour)
	outside.EndsAt = weekEnd.Add(26 * time.Hour)
	if err := st.CreateShift(ctx, &outside); err != nil {
		t.Fatalf("outside shift: %v", err)
	}

	rows, err := st.WeekShifts(ctx, tenantID, weekStart, weekEnd, nil)
	if err != nil {
		t.Fatalf("week: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("week rows = %d, want 2: %+v", len(rows), rows)
	}
	names := map[uuid.UUID]string{}
	for _, r := range rows {
		names[r.AgentID] = r.AgentName
	}
	if names[ada] != "Ada Week" || names[bob] != "Bob Week" {
		t.Fatalf("agent names not joined: %+v", names)
	}

	onlyAda, err := st.WeekShifts(ctx, tenantID, weekStart, weekEnd, &ada)
	if err != nil || len(onlyAda) != 1 || onlyAda[0].AgentID != ada {
		t.Fatalf("agent filter: %+v, %v", onlyAda, err)
	}
}

// Utilization: scheduled vs clocked hours; an entry with null clock_out is
// counted to NOW and flagged open (SPEC-W20).
func TestUtilization(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	agent := addAgent(t, st, tenantID, "Util Uma", true)

	now := time.Now().UTC()
	from := now.Add(-24 * time.Hour)
	to := now.Add(24 * time.Hour)

	// 8 scheduled hours ending 2h from now (fully inside the range).
	sh := Shift{TenantID: tenantID, AgentID: agent,
		StartsAt: now.Add(-10 * time.Hour), EndsAt: now.Add(-2 * time.Hour), Status: ShiftScheduled}
	if err := st.CreateShift(ctx, &sh); err != nil {
		t.Fatalf("shift: %v", err)
	}
	// A cancelled shift must not count.
	cancelled := Shift{TenantID: tenantID, AgentID: agent,
		StartsAt: now.Add(-30 * time.Hour), EndsAt: now.Add(-26 * time.Hour), Status: ShiftCancelled}
	if err := st.CreateShift(ctx, &cancelled); err != nil {
		t.Fatalf("cancelled shift: %v", err)
	}

	// 6 closed hours + one OPEN entry started 1h ago (counted to now).
	closedAt := now.Add(-3 * time.Hour)
	closedEntry := TimeEntry{TenantID: tenantID, AgentID: agent, Method: MethodWeb,
		ClockInAt: now.Add(-9 * time.Hour), ClockOutAt: &closedAt}
	insertEntry(t, st, closedEntry)
	openEntry := TimeEntry{TenantID: tenantID, AgentID: agent, Method: MethodWeb, ClockInAt: now.Add(-time.Hour)}
	if err := st.ClockIn(ctx, &openEntry); err != nil {
		t.Fatalf("open entry: %v", err)
	}

	rows, err := st.Utilization(ctx, tenantID, from, to)
	if err != nil {
		t.Fatalf("utilization: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.AgentID != agent || r.AgentName != "Util Uma" {
		t.Fatalf("row identity: %+v", r)
	}
	if r.ScheduledHours < 7.9 || r.ScheduledHours > 8.1 {
		t.Fatalf("scheduled hours = %v, want ~8", r.ScheduledHours)
	}
	// Clocked: 6 closed + ~1 open (to now) ≈ 7.
	if r.ClockedHours < 6.9 || r.ClockedHours > 7.2 {
		t.Fatalf("clocked hours = %v, want ~7 (open entry counted to now)", r.ClockedHours)
	}
	if r.OpenEntries != 1 {
		t.Fatalf("open entries = %d, want 1 (flagged)", r.OpenEntries)
	}
	if r.UtilizationPct == nil || *r.UtilizationPct < 86 || *r.UtilizationPct > 90 {
		t.Fatalf("utilization pct = %v, want ~87.5", r.UtilizationPct)
	}
}

// insertEntry bypasses the clock-guard for fixture seeding (closed entries
// can never be produced by ClockIn alone).
func insertEntry(t *testing.T, st *Store, e TimeEntry) {
	t.Helper()
	_, err := st.pool.Exec(context.Background(),
		`INSERT INTO time_entries (tenant_id, agent_id, clock_in_at, clock_out_at, method)
		 VALUES ($1,$2,$3,$4,$5)`,
		e.TenantID, e.AgentID, e.ClockInAt, e.ClockOutAt, e.Method)
	if err != nil {
		t.Fatalf("seed entry: %v", err)
	}
}

// Coverage: per-day distinct scheduled agents vs bookings count.
func TestCoverage(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	ada := addAgent(t, st, tenantID, "Cov Ada", true)
	bob := addAgent(t, st, tenantID, "Cov Bob", true)

	day := time.Date(2030, 6, 2, 0, 0, 0, 0, time.UTC) // Monday
	mk := func(agent uuid.UUID, startH, endH float64, status string) {
		sh := Shift{TenantID: tenantID, AgentID: agent,
			StartsAt: day.Add(time.Duration(startH * float64(time.Hour))),
			EndsAt:   day.Add(time.Duration(endH * float64(time.Hour))), Status: status}
		if err := st.CreateShift(ctx, &sh); err != nil {
			t.Fatalf("coverage shift: %v", err)
		}
	}
	mk(ada, 9, 17, ShiftScheduled)
	mk(bob, 12, 20, ShiftConfirmed)

	// Two bookings on the day + one cancelled (excluded) + one next day.
	addBooking := func(start time.Time, status string) {
		_, err := st.pool.Exec(ctx,
			`INSERT INTO bookings (tenant_id, starts_at, ends_at, status) VALUES ($1,$2,$3,$4)`,
			tenantID, start, start.Add(time.Hour), status)
		if err != nil {
			t.Fatalf("seed booking: %v", err)
		}
	}
	addBooking(day.Add(10*time.Hour), "confirmed")
	addBooking(day.Add(14*time.Hour), "pending")
	addBooking(day.Add(16*time.Hour), "cancelled")
	addBooking(day.Add(30*time.Hour), "confirmed")

	to := day.AddDate(0, 0, 2) // exclusive → Monday + Tuesday
	days, err := st.Coverage(ctx, tenantID, day, to.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("days = %d, want 2: %+v", len(days), days)
	}
	if days[0].Date != "2030-06-02" || days[0].AgentsScheduled != 2 || days[0].Bookings != 2 {
		t.Fatalf("day 1 wrong: %+v", days[0])
	}
	if days[1].Date != "2030-06-03" || days[1].AgentsScheduled != 0 || days[1].Bookings != 1 {
		t.Fatalf("day 2 wrong: %+v", days[1])
	}
}

// Outbox enqueue round-trips (events ride this path).
func TestEnqueueOutbox(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.EnqueueOutbox(ctx, uuid.New(), "opendesk.workforce.events.v1", []byte(`{"k":"v"}`)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox WHERE topic='opendesk.workforce.events.v1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("outbox rows = %d, %v", n, err)
	}
}

// RLS isolation: a non-superuser role sees ONLY the rows of the tenant set
// via app.tenant_id — the database itself enforces isolation even if the
// application-level tenant_id filter were ever dropped (contract §1).
func TestRLSIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	agentA := addAgent(t, st, tenantA, "RLS Ada", true)
	agentB := addAgent(t, st, tenantB, "RLS Bob", true)

	sa := mkShift(tenantA, agentA, 9, 12)
	if err := st.CreateShift(ctx, &sa); err != nil {
		t.Fatalf("create A: %v", err)
	}
	sb := mkShift(tenantB, agentB, 9, 12)
	if err := st.CreateShift(ctx, &sb); err != nil {
		t.Fatalf("create B: %v", err)
	}
	entryB := TimeEntry{TenantID: tenantB, AgentID: agentB, Method: MethodWeb}
	if err := st.ClockIn(ctx, &entryB); err != nil {
		t.Fatalf("clock-in B: %v", err)
	}
	leaveA := LeaveRequest{TenantID: tenantA, AgentID: agentA, Kind: LeaveAnnual,
		StartsOn: time.Date(2030, 7, 1, 0, 0, 0, 0, time.UTC), EndsOn: time.Date(2030, 7, 2, 0, 0, 0, 0, time.UTC)}
	if err := st.CreateLeave(ctx, &leaveA); err != nil {
		t.Fatalf("leave A: %v", err)
	}

	// Restricted role with table privileges but no RLS bypass.
	if _, err := st.pool.Exec(ctx, `
		DO $$ BEGIN
		    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'workforce_rls') THEN
		        CREATE ROLE workforce_rls LOGIN PASSWORD 'workforce_rls';
		    END IF;
		END $$;
		GRANT USAGE ON SCHEMA public TO workforce_rls;
		GRANT SELECT, INSERT, UPDATE, DELETE ON shifts, time_entries, leave_requests TO workforce_rls;`); err != nil {
		t.Fatalf("create rls role: %v", err)
	}
	pool, err := pgxpool.New(ctx,
		"postgres://workforce_rls:workforce_rls@localhost:5564/booking_workforce_test?sslmode=disable")
	if err != nil {
		t.Fatalf("dial rls role: %v", err)
	}
	defer pool.Close()

	// No tenant context → zero rows (policy compares against NULL).
	for _, table := range []string{"shifts", "time_entries", "leave_requests"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s without tenant: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s rows visible without tenant context: %d", table, n)
		}
	}

	// Tenant A context → exactly A's rows; B's rows invisible to SELECT and
	// untouchable by UPDATE.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA.String()); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM shifts`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("tenant A sees %d shifts (%v), want 1", n, err)
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM time_entries`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("tenant A sees %d time entries (%v), want 0", n, err)
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM leave_requests`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("tenant A sees %d leave requests (%v), want 1", n, err)
	}
	var role string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(role,'') FROM shifts WHERE id=$1`, sa.ID).Scan(&role); err != nil {
		t.Fatalf("read own row: %v", err)
	}
	var cross uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM shifts WHERE id=$1`, sb.ID).Scan(&cross); err == nil {
		t.Fatalf("cross-tenant row visible: %s", cross)
	}
	tag, err := tx.Exec(ctx, `UPDATE shifts SET role='pwned' WHERE id=$1`, sb.ID)
	if err != nil {
		t.Fatalf("cross update: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("cross-tenant UPDATE affected %d rows", tag.RowsAffected())
	}
}
