package workorders

import (
	"context"
	"errors"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
)

// SPEC-W19 Agent B store tests run against embedded Postgres (same harness
// as the devices/leads tests; dedicated port 5562 avoids the postmaster.pid
// race with sibling packages under `go test ./...`; -short skips).
//
// The harness also bootstraps the minimal team_members + outbox tables the
// package JOINS/enqueues against (owned by the shared store package in
// production — mirrored here exactly like store/waitlist_test.go does).

const testSupportDDL = `
CREATE TABLE IF NOT EXISTS team_members (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    email TEXT,
    role TEXT NOT NULL DEFAULT 'staff',
    active BOOLEAN NOT NULL DEFAULT TRUE
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
		t.Skip("skipping embedded-postgres workorders store test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_workorders_test").
		Port(5562).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })
	st, err := DialStore(context.Background(),
		"postgres://postgres:postgres@localhost:5562/booking_workorders_test?sslmode=disable")
	if err != nil {
		t.Fatalf("DialStore: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.pool.Exec(context.Background(), testSupportDDL); err != nil {
		t.Fatalf("support DDL: %v", err)
	}
	return st
}

func mkOrder(tenantID uuid.UUID, title string) WorkOrder {
	return WorkOrder{
		TenantID:  tenantID,
		Title:     title,
		Status:    StatusCreated,
		Checklist: []ChecklistItem{{Label: "inspect", Done: false}, {Label: "repair", Done: false}},
	}
}

func addTeamMember(t *testing.T, st *Store, tenantID uuid.UUID, name string, active bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := st.pool.Exec(context.Background(),
		`INSERT INTO team_members (id, tenant_id, name, active) VALUES ($1,$2,$3,$4)`,
		id, tenantID, name, active)
	if err != nil {
		t.Fatalf("insert team member: %v", err)
	}
	return id
}

// Create → Get round-trip; checklist/proof jsonb fidelity; timestamps stamped.
func TestCreateGet(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	wo := mkOrder(tenantID, "Fix AC")
	lat, lng := 6.5244, 3.3792
	wo.GPSLat, wo.GPSLng = &lat, &lng
	capID := "9f2b4d2a-9e32-4b0d-9a4a-2f9a4f5b7c01"
	wo.FieldCaptureID = &capID
	if err := st.Create(ctx, &wo); err != nil {
		t.Fatalf("create: %v", err)
	}
	if wo.ID == uuid.Nil || wo.CreatedAt.IsZero() || wo.UpdatedAt.IsZero() {
		t.Fatalf("id/timestamps not stamped: %+v", wo)
	}

	got, err := st.Get(ctx, tenantID, wo.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "Fix AC" || got.Status != StatusCreated {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if len(got.Checklist) != 2 || got.Checklist[0].Label != "inspect" || got.Checklist[0].Done {
		t.Fatalf("checklist fidelity: %+v", got.Checklist)
	}
	if got.GPSLat == nil || *got.GPSLat != lat || got.GPSLng == nil || *got.GPSLng != lng {
		t.Fatalf("gps fidelity: %+v", got)
	}
	if got.FieldCaptureID == nil || *got.FieldCaptureID != capID {
		t.Fatalf("field_capture_id fidelity: %+v", got.FieldCaptureID)
	}

	if _, err := st.Get(ctx, tenantID, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v, want ErrNotFound", err)
	}
	if _, err := st.Get(ctx, uuid.New(), wo.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant get = %v, want ErrNotFound", err)
	}
}

// RLS tenant isolation: tenant B cannot read/update tenant A's rows even
// when it knows the IDs (FORCE RLS tenant_isolation, contract §1).
func TestRLSIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()

	wo := mkOrder(tenantA, "Tenant A job")
	if err := st.Create(ctx, &wo); err != nil {
		t.Fatalf("create: %v", err)
	}

	listB, err := st.List(ctx, tenantB, ListFilters{})
	if err != nil || len(listB) != 0 {
		t.Fatalf("tenant B list leaked: %+v, %v", listB, err)
	}
	boardB, err := st.Board(ctx, tenantB)
	if err != nil || len(boardB) != 0 {
		t.Fatalf("tenant B board leaked: %+v, %v", boardB, err)
	}

	// Cross-tenant update must hit 0 rows → ErrNotFound, and the row must
	// survive untouched under tenant A.
	wo.Title = "hijacked"
	wo.TenantID = tenantB
	if err := st.Update(ctx, &wo); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant update = %v, want ErrNotFound", err)
	}
	again, err := st.Get(ctx, tenantA, wo.ID)
	if err != nil || again.Title != "Tenant A job" {
		t.Fatalf("row changed by cross-tenant update: %+v, %v", again, err)
	}
}

// List filters: status / assignee / q.
func TestListFilters(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	assignee := uuid.New()

	a := mkOrder(tenantID, "AC repair lekki")
	b := mkOrder(tenantID, "Generator service")
	c := mkOrder(tenantID, "AC install")
	for _, wo := range []*WorkOrder{&a, &b, &c} {
		if err := st.Create(ctx, wo); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	b.Status = StatusAssigned
	b.AssigneeID = &assignee
	if err := st.Update(ctx, &b); err != nil {
		t.Fatalf("update: %v", err)
	}

	created, err := st.List(ctx, tenantID, ListFilters{Status: StatusCreated})
	if err != nil || len(created) != 2 {
		t.Fatalf("status filter: %+v, %v", created, err)
	}
	byAssignee, err := st.List(ctx, tenantID, ListFilters{Assignee: &assignee})
	if err != nil || len(byAssignee) != 1 || byAssignee[0].ID != b.ID {
		t.Fatalf("assignee filter: %+v, %v", byAssignee, err)
	}
	ac, err := st.List(ctx, tenantID, ListFilters{Q: "ac"})
	if err != nil || len(ac) != 2 {
		t.Fatalf("q filter: %+v, %v", ac, err)
	}
}

// Board groups with assignee names; Today windows on scheduled_start.
func TestBoardAndToday(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	ada := addTeamMember(t, st, tenantID, "Ada Field", true)

	// SPEC-W24 WS-A1: pin the whole scenario to ONE explicit location.
	// Store.Today windows on an explicit [dayStart, dayEnd) instant range
	// (the HTTP handler buckets it in the tenant location — see
	// Handlers.Today), so the test must derive BOTH the window and the
	// scheduled instants from the same explicit time.Location. The old
	// version mixed time.Now()'s implicit local-time date components with
	// a time.UTC midnight (flaky under TZ=Asia/Shanghai: the local date
	// is not the UTC date around the day boundary) and anchored the
	// schedule to now (flaky near midnight under ANY TZ: now+2h can fall
	// into the next day). Anchoring the instants to the pinned window
	// makes the test deterministic regardless of process TZ and wall time.
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.AddDate(0, 0, 1)
	start := dayStart.Add(12 * time.Hour) // midday — always inside the window
	tomorrow := dayEnd.Add(2 * time.Hour) // always outside the window

	todayWO := mkOrder(tenantID, "Today job")
	todayWO.ScheduledStart = &start
	futureWO := mkOrder(tenantID, "Tomorrow job")
	futureWO.ScheduledStart = &tomorrow
	for _, wo := range []*WorkOrder{&todayWO, &futureWO} {
		if err := st.Create(ctx, wo); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	todayWO.Status = StatusAssigned
	todayWO.AssigneeID = &ada
	if err := st.Update(ctx, &todayWO); err != nil {
		t.Fatalf("update: %v", err)
	}

	board, err := st.Board(ctx, tenantID)
	if err != nil || len(board) != 2 {
		t.Fatalf("board: %+v, %v", board, err)
	}
	var named *BoardItem
	for i := range board {
		if board[i].ID == todayWO.ID {
			named = &board[i]
		}
	}
	if named == nil || named.AssigneeName != "Ada Field" {
		t.Fatalf("assignee name join: %+v", board)
	}

	today, err := st.Today(ctx, tenantID, dayStart, dayEnd, nil)
	if err != nil {
		t.Fatalf("today: %v", err)
	}
	if len(today) != 1 || today[0].ID != todayWO.ID {
		t.Fatalf("today window: %+v", today)
	}
	none, err := st.Today(ctx, tenantID, dayStart, dayEnd, &[]uuid.UUID{uuid.New()}[0])
	if err != nil || len(none) != 0 {
		t.Fatalf("today assignee filter: %+v, %v", none, err)
	}
}

// PickAutoAssignee: least-open-orders active member; inactive skipped;
// deterministic name tie-break; ErrNoAssignee when nobody is active.
func TestPickAutoAssignee(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	if _, err := st.PickAutoAssignee(ctx, tenantID); !errors.Is(err, ErrNoAssignee) {
		t.Fatalf("no members = %v, want ErrNoAssignee", err)
	}

	ada := addTeamMember(t, st, tenantID, "Ada", true)
	bola := addTeamMember(t, st, tenantID, "Bola", true)
	addTeamMember(t, st, tenantID, "Chidi", false) // inactive — never picked

	// Ada carries one open order → Bola (zero) is next.
	wo := mkOrder(tenantID, "Open job")
	if err := st.Create(ctx, &wo); err != nil {
		t.Fatalf("create: %v", err)
	}
	wo.Status = StatusAssigned
	wo.AssigneeID = &ada
	if err := st.Update(ctx, &wo); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := st.PickAutoAssignee(ctx, tenantID)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got != bola {
		t.Fatalf("picked %v, want Bola %v (fewest open)", got, bola)
	}

	// Terminal orders do not count as load: cancel Ada's order → tie at 0,
	// name tie-break picks Ada.
	wo.Status = StatusCancelled
	if err := st.Update(ctx, &wo); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got, err = st.PickAutoAssignee(ctx, tenantID)
	if err != nil || got != ada {
		t.Fatalf("tie-break picked %v, want Ada %v (err %v)", got, ada, err)
	}
}

// EnqueueOutbox: a row lands on the outbox for the dispatcher (events,
// metering and the dispatch push all ride this path).
func TestEnqueueOutbox(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	agg := uuid.New()
	if err := st.EnqueueOutbox(ctx, agg, "opendesk.fsm.events.v1", []byte(`{"type":"com.opendesk.fsm.WorkOrderAssigned"}`)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var topic, payload string
	err := st.pool.QueryRow(ctx, `SELECT topic, payload::text FROM outbox WHERE aggregate_id=$1`, agg).Scan(&topic, &payload)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if topic != "opendesk.fsm.events.v1" || payload == "" {
		t.Fatalf("outbox row: %q %q", topic, payload)
	}
}
