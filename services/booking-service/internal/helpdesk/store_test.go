package helpdesk

import (
	"context"
	"strings"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SPEC-W19 Agent A store tests run against embedded Postgres (same harness
// as the devices/leads tests; dedicated port 5563 avoids the postmaster.pid
// race with sibling packages under `go test ./...`; -short skips). The
// harness additionally creates the core team_members table (auto-assignment
// reads it) and the shared outbox table (metering/event enqueue), mirroring
// their production DDL.

func newTestStore(t *testing.T) *Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres helpdesk store test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_helpdesk_test").
		Port(5563).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })
	st, err := DialStore(context.Background(),
		"postgres://postgres:postgres@localhost:5563/booking_helpdesk_test?sslmode=disable", 4)
	if err != nil {
		t.Fatalf("DialStore: %v", err)
	}
	t.Cleanup(st.Close)

	// Core tables the helpdesk package reads/writes but does not own:
	// team_members (core booking table, RLS like production) and the shared
	// outbox (no RLS, like production).
	const aux = `
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
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;
CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    sent_at TIMESTAMPTZ
);`
	if _, err := st.pool.Exec(context.Background(), aux); err != nil {
		t.Fatalf("aux schema: %v", err)
	}
	return st
}

// addMember seeds one active team member (superuser connection bypasses RLS
// for seeding; production seeding goes through store.Store).
func addMember(t *testing.T, st *Store, tenantID uuid.UUID, name string, active bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := st.pool.Exec(context.Background(),
		`INSERT INTO team_members (id, tenant_id, name, email, active) VALUES ($1,$2,$3,$4,$5)`,
		id, tenantID, name, strings.ToLower(strings.ReplaceAll(name, " ", "."))+"@example.com", active)
	if err != nil {
		t.Fatalf("seed team member: %v", err)
	}
	return id
}

func strptr(s string) *string { return &s }

func mkTicket(tenantID uuid.UUID, subject, priority string) Ticket {
	return Ticket{TenantID: tenantID, Subject: subject, Channel: "email", Priority: priority, Status: StatusOpen}
}

func mkPolicy(tenantID uuid.UUID, name, priority string, frMin, rsMin int) SLAPolicy {
	return SLAPolicy{TenantID: tenantID, Name: name, Priority: priority,
		FirstResponseMinute: frMin, ResolveMinutes: rsMin, Active: true}
}

// Policy CRUD + validation.
func TestPolicyCRUD(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	p := mkPolicy(tenantID, "Urgent tier", PriorityUrgent, 15, 120)
	if err := st.CreatePolicy(ctx, &p); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	if p.ID == uuid.Nil || p.CreatedAt.IsZero() {
		t.Fatalf("policy not stamped: %+v", p)
	}

	all, err := st.ListPolicies(ctx, tenantID)
	if err != nil || len(all) != 1 {
		t.Fatalf("list policies: %+v, %v", all, err)
	}

	newFR := 30
	inactive := false
	upd, err := st.UpdatePolicy(ctx, tenantID, p.ID, nil, nil, &newFR, nil, &inactive)
	if err != nil {
		t.Fatalf("update policy: %v", err)
	}
	if upd.FirstResponseMinute != 30 || upd.Active || upd.ResolveMinutes != 120 || upd.Name != "Urgent tier" {
		t.Fatalf("partial update wrong: %+v", upd)
	}
	if _, err := st.UpdatePolicy(ctx, tenantID, uuid.New(), nil, nil, nil, nil, nil); err != ErrNotFound {
		t.Fatalf("update missing = %v, want ErrNotFound", err)
	}

	// Cross-tenant reads are empty (app-level + RLS belt-and-braces).
	cross, err := st.ListPolicies(ctx, uuid.New())
	if err != nil || len(cross) != 0 {
		t.Fatalf("cross-tenant policies: %+v, %v", cross, err)
	}
}

// Ticket creation auto-attaches the active policy for the priority tier and
// computes due_* from created_at; the created event lands in the timeline.
func TestCreateTicketAutoPolicyAndDues(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	p := mkPolicy(tenantID, "High tier", PriorityHigh, 10, 60)
	if err := st.CreatePolicy(ctx, &p); err != nil {
		t.Fatalf("create policy: %v", err)
	}

	tk := mkTicket(tenantID, "Cannot log in", PriorityHigh)
	if err := st.CreateTicket(ctx, &tk, "agent-1"); err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	if tk.SLAPolicyID == nil || *tk.SLAPolicyID != p.ID {
		t.Fatalf("policy not auto-attached: %+v", tk)
	}
	if tk.DueFirstResponseAt == nil || tk.DueResolveAt == nil {
		t.Fatalf("dues not computed: %+v", tk)
	}
	if got := tk.CreatedAt.Add(10 * time.Minute); !tk.DueFirstResponseAt.Equal(got) {
		t.Fatalf("due_first_response_at = %v, want %v", tk.DueFirstResponseAt, got)
	}
	if got := tk.CreatedAt.Add(60 * time.Minute); !tk.DueResolveAt.Equal(got) {
		t.Fatalf("due_resolve_at = %v, want %v", tk.DueResolveAt, got)
	}

	events, err := st.ListEvents(ctx, tenantID, tk.ID)
	if err != nil || len(events) != 1 || events[0].Kind != EventCreated || events[0].Actor != "agent-1" {
		t.Fatalf("timeline: %+v, %v", events, err)
	}

	// Low priority has no policy → no dues, no attach.
	low := mkTicket(tenantID, "When is the sale?", PriorityLow)
	if err := st.CreateTicket(ctx, &low, ""); err != nil {
		t.Fatalf("create low ticket: %v", err)
	}
	if low.SLAPolicyID != nil || low.DueResolveAt != nil {
		t.Fatalf("low ticket must have no policy: %+v", low)
	}
}

// PatchTicket: assignment (explicit/auto/unassign), status machine,
// first-response stamping, reopen clearing, due recompute on priority
// change — all with timeline events.
func TestPatchTicketLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	high := mkPolicy(tenantID, "High tier", PriorityHigh, 10, 60)
	if err := st.CreatePolicy(ctx, &high); err != nil {
		t.Fatalf("policy: %v", err)
	}
	urgent := mkPolicy(tenantID, "Urgent tier", PriorityUrgent, 5, 30)
	if err := st.CreatePolicy(ctx, &urgent); err != nil {
		t.Fatalf("policy: %v", err)
	}

	tk := mkTicket(tenantID, "Payments failing", PriorityHigh)
	if err := st.CreateTicket(ctx, &tk, "agent-1"); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 1. Note → first_response_at stamped + note/first_response events.
	note := "We are looking into it"
	res, err := st.PatchTicket(ctx, tenantID, tk.ID, PatchInput{Note: &note}, "agent-2")
	if err != nil {
		t.Fatalf("patch note: %v", err)
	}
	if !res.FirstResponseNow || res.Ticket.FirstResponseAt == nil {
		t.Fatalf("first response not stamped: %+v", res.Ticket)
	}

	// 2. Priority bump → policy switches to urgent tier, dues recomputed
	//    from created_at.
	res, err = st.PatchTicket(ctx, tenantID, tk.ID, PatchInput{Priority: strptr(PriorityUrgent)}, "agent-2")
	if err != nil {
		t.Fatalf("patch priority: %v", err)
	}
	if res.Ticket.Priority != PriorityUrgent || res.Ticket.SLAPolicyID == nil || *res.Ticket.SLAPolicyID != urgent.ID {
		t.Fatalf("policy not switched: %+v", res.Ticket)
	}
	if got := res.Ticket.CreatedAt.Add(5 * time.Minute); !res.Ticket.DueFirstResponseAt.Equal(got) {
		t.Fatalf("recomputed due_first_response = %v, want %v", res.Ticket.DueFirstResponseAt, got)
	}
	// First response survives later patches.
	if res.Ticket.FirstResponseAt == nil || res.FirstResponseNow {
		t.Fatalf("first response must be sticky: %+v", res)
	}

	// 3. Resolve → resolved_at + resolved event + ResolvedNow (meter hook).
	res, err = st.PatchTicket(ctx, tenantID, tk.ID, PatchInput{Status: strptr(StatusResolved)}, "agent-2")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.ResolvedNow || res.Ticket.ResolvedAt == nil || res.Ticket.Status != StatusResolved {
		t.Fatalf("resolution not tracked: %+v", res)
	}

	// 4. Idempotent re-resolve → no duplicate metering signal.
	res, err = st.PatchTicket(ctx, tenantID, tk.ID, PatchInput{Status: strptr(StatusResolved)}, "agent-2")
	if err != nil {
		t.Fatalf("re-resolve: %v", err)
	}
	if res.ResolvedNow {
		t.Fatal("re-resolve must not re-meter (ResolvedNow)")
	}

	// 5. Reopen → resolved_at cleared + reopened event.
	res, err = st.PatchTicket(ctx, tenantID, tk.ID, PatchInput{Status: strptr(StatusOpen)}, "agent-3")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if res.Ticket.ResolvedAt != nil {
		t.Fatalf("reopen must clear resolved_at: %+v", res.Ticket)
	}

	events, err := st.ListEvents(ctx, tenantID, tk.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	kinds := map[string]int{}
	for _, e := range events {
		kinds[e.Kind]++
	}
	for _, want := range []string{EventCreated, EventNote, EventFirstResponse, EventStatusChanged, EventResolved, EventReopened} {
		if kinds[want] == 0 {
			t.Fatalf("timeline missing %q: %v", want, kinds)
		}
	}
}

// Auto-assignment picks the active member with the fewest open tickets.
func TestAutoAssignLeastLoaded(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	busy := addMember(t, st, tenantID, "Busy Bee", true)
	idle := addMember(t, st, tenantID, "Idle Ida", true)
	addMember(t, st, tenantID, "Gone Gary", false) // inactive: never picked

	// Two open tickets on Busy Bee.
	for _, subj := range []string{"t1", "t2"} {
		tk := mkTicket(tenantID, subj, PriorityNormal)
		tk.AssigneeID = &busy
		if err := st.CreateTicket(ctx, &tk, ""); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	tk := mkTicket(tenantID, "auto me", PriorityNormal)
	if err := st.CreateTicket(ctx, &tk, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := st.PatchTicket(ctx, tenantID, tk.ID, PatchInput{AutoAssign: true}, "dispatcher")
	if err != nil {
		t.Fatalf("auto assign: %v", err)
	}
	if res.AutoAssignedTo == nil || *res.AutoAssignedTo != idle {
		t.Fatalf("auto-assigned to %+v, want Idle Ida (%s)", res.AutoAssignedTo, idle)
	}

	// Unassign clears the assignee with an event.
	res, err = st.PatchTicket(ctx, tenantID, tk.ID, PatchInput{Unassign: true}, "dispatcher")
	if err != nil || res.Ticket.AssigneeID != nil {
		t.Fatalf("unassign: %+v, %v", res.Ticket, err)
	}

	// Tenant with no members → ErrNoAssignee.
	lonely := mkTicket(uuid.New(), "nobody here", PriorityNormal)
	if err := st.CreateTicket(ctx, &lonely, ""); err != nil {
		t.Fatalf("create lonely: %v", err)
	}
	if _, err := st.PatchTicket(ctx, lonely.TenantID, lonely.ID, PatchInput{AutoAssign: true}, ""); err != ErrNoAssignee {
		t.Fatalf("auto assign with no members = %v, want ErrNoAssignee", err)
	}
}

// CSAT: only resolved|closed tickets; rating persisted.
func TestRecordCSAT(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	tk := mkTicket(tenantID, "rate me", PriorityNormal)
	if err := st.CreateTicket(ctx, &tk, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.RecordCSAT(ctx, tenantID, tk.ID, 5, "great"); err == nil {
		t.Fatal("csat on open ticket must fail")
	}
	if _, err := st.PatchTicket(ctx, tenantID, tk.ID, PatchInput{Status: strptr(StatusResolved)}, "a"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got, err := st.RecordCSAT(ctx, tenantID, tk.ID, 5, "great")
	if err != nil {
		t.Fatalf("csat: %v", err)
	}
	if got.CSATRating == nil || *got.CSATRating != 5 || got.CSATAt == nil {
		t.Fatalf("csat not persisted: %+v", got)
	}
	if got.CSATComment == nil || *got.CSATComment != "great" {
		t.Fatalf("csat comment: %+v", got)
	}
}

// Stats + breaches: open-by-priority counts, breach detection, 30d averages.
func TestStatsAndBreaches(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	// Policy with a 1-minute first-response SLA so the breach is observable.
	p := mkPolicy(tenantID, "instant", PriorityHigh, 1, 1)
	if err := st.CreatePolicy(ctx, &p); err != nil {
		t.Fatalf("policy: %v", err)
	}
	open1 := mkTicket(tenantID, "breaching", PriorityHigh)
	if err := st.CreateTicket(ctx, &open1, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Force the dues into the past (superuser UPDATE bypasses RLS for the
	// fixture; the app never does this).
	if _, err := st.pool.Exec(ctx,
		`UPDATE tickets SET due_first_response_at = now() - interval '1 hour', due_resolve_at = now() - interval '30 minutes'
		 WHERE id=$1`, open1.ID); err != nil {
		t.Fatalf("backdate dues: %v", err)
	}
	okTk := mkTicket(tenantID, "fine", PriorityNormal)
	if err := st.CreateTicket(ctx, &okTk, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	resTk := mkTicket(tenantID, "done", PriorityNormal)
	if err := st.CreateTicket(ctx, &resTk, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.PatchTicket(ctx, tenantID, resTk.ID, PatchInput{Status: strptr(StatusResolved)}, "a"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := st.RecordCSAT(ctx, tenantID, resTk.ID, 4, ""); err != nil {
		t.Fatalf("csat: %v", err)
	}

	stats, err := st.Stats(ctx, tenantID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.OpenCount != 2 || stats.OpenByPriority[PriorityHigh] != 1 || stats.OpenByPriority[PriorityNormal] != 1 {
		t.Fatalf("open counts wrong: %+v", stats)
	}
	if stats.BreachedCount != 1 {
		t.Fatalf("breached = %d, want 1", stats.BreachedCount)
	}
	if stats.Resolved30d != 1 {
		t.Fatalf("resolved_30d = %d, want 1", stats.Resolved30d)
	}
	if stats.AvgResolveMinutes30d == nil || stats.AvgCSAT30d == nil || *stats.AvgCSAT30d != 4 {
		t.Fatalf("30d averages wrong: %+v", stats)
	}

	breaches, err := st.ListBreaches(ctx, tenantID)
	if err != nil || len(breaches) != 1 {
		t.Fatalf("breaches: %+v, %v", breaches, err)
	}
	if breaches[0].ID != open1.ID || !breaches[0].BreachedFirstResponse || !breaches[0].BreachedResolve {
		t.Fatalf("breach flags wrong: %+v", breaches[0])
	}

	// Resolved tickets never breach even with past dues.
	if _, err := st.pool.Exec(ctx,
		`UPDATE tickets SET due_resolve_at = now() - interval '1 hour' WHERE id=$1`, resTk.ID); err != nil {
		t.Fatalf("backdate resolved: %v", err)
	}
	breaches, err = st.ListBreaches(ctx, tenantID)
	if err != nil || len(breaches) != 1 {
		t.Fatalf("resolved ticket must not breach: %+v, %v", breaches, err)
	}
}

// ListTickets filters: status / priority / assignee / channel / q.
func TestListTicketFilters(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	member := addMember(t, st, tenantID, "Filt Fred", true)

	a := mkTicket(tenantID, "Refund request", PriorityHigh)
	a.Channel = "email"
	a.AssigneeID = &member
	if err := st.CreateTicket(ctx, &a, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	b := mkTicket(tenantID, "Login broken", PriorityNormal)
	b.Channel = "chat"
	if err := st.CreateTicket(ctx, &b, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.PatchTicket(ctx, tenantID, b.ID, PatchInput{Status: strptr(StatusPending)}, ""); err != nil {
		t.Fatalf("patch: %v", err)
	}

	for name, tc := range map[string]struct {
		f    TicketFilters
		want int
	}{
		"all":      {TicketFilters{}, 2},
		"status":   {TicketFilters{Status: StatusPending}, 1},
		"priority": {TicketFilters{Priority: PriorityHigh}, 1},
		"assignee": {TicketFilters{AssigneeID: &member}, 1},
		"channel":  {TicketFilters{Channel: "chat"}, 1},
		"search":   {TicketFilters{Q: "refund"}, 1},
		"nomatch":  {TicketFilters{Q: "zzz-nothing"}, 0},
	} {
		got, err := st.ListTickets(ctx, tenantID, tc.f)
		if err != nil || len(got) != tc.want {
			t.Fatalf("%s: got %d (%v), want %d", name, len(got), err, tc.want)
		}
	}
}

// RLS isolation: a non-superuser role sees ONLY the rows of the tenant set
// via app.tenant_id — the database itself enforces isolation even if the
// application-level tenant_id filter were ever dropped (contract §1).
func TestRLSIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()

	ta := mkTicket(tenantA, "tenant A secret", PriorityNormal)
	if err := st.CreateTicket(ctx, &ta, ""); err != nil {
		t.Fatalf("create A: %v", err)
	}
	tb := mkTicket(tenantB, "tenant B secret", PriorityNormal)
	if err := st.CreateTicket(ctx, &tb, ""); err != nil {
		t.Fatalf("create B: %v", err)
	}

	// Restricted role with table privileges but no RLS bypass.
	if _, err := st.pool.Exec(ctx, `
		DO $$ BEGIN
		    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'helpdesk_rls') THEN
		        CREATE ROLE helpdesk_rls LOGIN PASSWORD 'helpdesk_rls';
		    END IF;
		END $$;
		GRANT USAGE ON SCHEMA public TO helpdesk_rls;
		GRANT SELECT, INSERT, UPDATE, DELETE ON tickets, ticket_events, sla_policies TO helpdesk_rls;`); err != nil {
		t.Fatalf("create rls role: %v", err)
	}
	pool, err := pgxpool.New(ctx,
		"postgres://helpdesk_rls:helpdesk_rls@localhost:5563/booking_helpdesk_test?sslmode=disable")
	if err != nil {
		t.Fatalf("dial rls role: %v", err)
	}
	defer pool.Close()

	// No tenant context → zero rows (policy compares against NULL).
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM tickets`).Scan(&n); err != nil {
		t.Fatalf("count without tenant: %v", err)
	}
	if n != 0 {
		t.Fatalf("rows visible without tenant context: %d", n)
	}

	// Tenant A context → exactly A's row; B's row invisible to SELECT and
	// untouchable by UPDATE.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA.String()); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM tickets`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("tenant A sees %d rows (%v), want 1", n, err)
	}
	var subject string
	if err := tx.QueryRow(ctx, `SELECT subject FROM tickets WHERE id=$1`, ta.ID).Scan(&subject); err != nil {
		t.Fatalf("read own row: %v", err)
	}
	var cross string
	if err := tx.QueryRow(ctx, `SELECT subject FROM tickets WHERE id=$1`, tb.ID).Scan(&cross); err == nil {
		t.Fatalf("cross-tenant row visible: %q", cross)
	}
	tag, err := tx.Exec(ctx, `UPDATE tickets SET subject='pwned' WHERE id=$1`, tb.ID)
	if err != nil {
		t.Fatalf("cross update: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("cross-tenant UPDATE affected %d rows", tag.RowsAffected())
	}
}
