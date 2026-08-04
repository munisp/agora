package campaignstudio

import (
	"context"
	"errors"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SPEC-W19 store tests run against embedded Postgres (same harness as the
// devices/leads tests; dedicated port 5563 avoids the postmaster.pid race
// with sibling packages under `go test ./...`; -short skips).

// testSchema provides the contacts/leads fixture tables (owned by the
// base schema / W13 in real deployments — mirrored here WITHOUT their RLS
// so fixtures are easy to seed; the studio tables' own RLS comes from
// ensureSchema and is exercised by TestRLSIsolation).
const testSchema = `
CREATE TABLE IF NOT EXISTS contacts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    phone TEXT,
    email TEXT,
    notes TEXT NOT NULL DEFAULT '',
    source TEXT,
    external_id TEXT
);
CREATE TABLE IF NOT EXISTS leads (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL,
    phone_e164             TEXT NOT NULL,
    channel_of_first_touch TEXT NOT NULL,
    campaign_id            UUID,
    promo_code             TEXT,
    utm                    JSONB,
    lga_id                 INTEGER,
    status                 TEXT NOT NULL DEFAULT 'new',
    consent_id             UUID,
    dedupe_key             TEXT NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, dedupe_key)
);`

func newTestStore(t *testing.T) *Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres campaign studio store test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_studio_test").
		Port(5563).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })
	st, err := DialStore(context.Background(),
		"postgres://postgres:postgres@localhost:5563/booking_studio_test?sslmode=disable")
	if err != nil {
		t.Fatalf("DialStore: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.pool.Exec(context.Background(), testSchema); err != nil {
		t.Fatalf("fixture schema: %v", err)
	}
	return st
}

func seedContact(t *testing.T, st *Store, tenantID uuid.UUID, name, phone, email, source string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := st.pool.Exec(context.Background(),
		`INSERT INTO contacts (id, tenant_id, name, phone, email, source) VALUES ($1,$2,$3,$4,$5,$6)`,
		id, tenantID, name, phone, email, source)
	if err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	return id
}

func seedLead(t *testing.T, st *Store, tenantID uuid.UUID, phone, channel, status string) {
	t.Helper()
	_, err := st.pool.Exec(context.Background(),
		`INSERT INTO leads (tenant_id, phone_e164, channel_of_first_touch, status, dedupe_key)
		 VALUES ($1,$2,$3,$4,$5)`,
		tenantID, phone, channel, status, uuid.NewString())
	if err != nil {
		t.Fatalf("seed lead: %v", err)
	}
}

// Segment count evaluates the definition against the contacts/leads
// fixture rows (SPEC-W19: read-only evaluation, RLS-safe).
func TestCountSegment(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	otherTenant := uuid.New()

	// Fixture: 4 contacts in tenant, 1 cross-tenant decoy.
	seedContact(t, st, tenantID, "Ada Lovelace", "+2348011111111", "ada@example.com", "twenty")
	seedContact(t, st, tenantID, "Grace Hopper", "+2348022222222", "grace@example.com", "field")
	seedContact(t, st, tenantID, "Alan Turing", "+2348033333333", "alan@other.com", "twenty")
	seedContact(t, st, tenantID, "No Phone", "", "nophone@example.com", "twenty")
	seedContact(t, st, otherTenant, "Decoy", "+2348099999999", "decoy@example.com", "twenty")
	seedLead(t, st, tenantID, "+2348011111111", "web", "qualified")
	seedLead(t, st, tenantID, "+2348022222222", "sms", "new")

	def := SegmentDefinition{Filters: []SegmentFilter{
		{Field: FieldSource, Op: OpEq, Value: "twenty"},
	}}
	seg := Segment{TenantID: tenantID, Name: "CRM contacts", Definition: def}
	if err := st.CreateSegment(ctx, &seg); err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}

	count, truncated, err := st.CountSegment(ctx, tenantID, seg.ID)
	if err != nil {
		t.Fatalf("CountSegment: %v", err)
	}
	if truncated {
		t.Fatal("small fixture must not truncate")
	}
	if count != 3 { // Ada + Alan + No Phone (decoy is cross-tenant)
		t.Fatalf("count = %d, want 3", count)
	}

	// approx_count stamped.
	got, err := st.GetSegment(ctx, tenantID, seg.ID)
	if err != nil || got.ApproxCount != 3 {
		t.Fatalf("approx_count = %+v, %v", got, err)
	}

	// Lead-joined filters (EXISTS phone join) + contains + in + neq.
	cases := []struct {
		name string
		def  SegmentDefinition
		want int64
	}{
		{"lead status", SegmentDefinition{Filters: []SegmentFilter{{Field: FieldLeadStatus, Op: OpEq, Value: "qualified"}}}, 1},
		{"lead channel in", SegmentDefinition{Filters: []SegmentFilter{{Field: FieldLeadChannel, Op: OpIn, Value: []any{"web", "sms"}}}}, 2},
		{"email contains", SegmentDefinition{Filters: []SegmentFilter{{Field: FieldEmail, Op: OpContains, Value: "@EXAMPLE.com"}}}, 3},
		{"source neq", SegmentDefinition{Filters: []SegmentFilter{{Field: FieldSource, Op: OpNeq, Value: "twenty"}}}, 1},
		{"and combined", SegmentDefinition{Filters: []SegmentFilter{
			{Field: FieldSource, Op: OpEq, Value: "twenty"},
			{Field: FieldLeadStatus, Op: OpEq, Value: "qualified"},
		}}, 1},
		{"no match", SegmentDefinition{Filters: []SegmentFilter{{Field: FieldLeadStatus, Op: OpEq, Value: "lost"}}}, 0},
	}
	for _, tc := range cases {
		s := Segment{TenantID: tenantID, Name: tc.name, Definition: tc.def}
		if err := st.CreateSegment(ctx, &s); err != nil {
			t.Fatalf("CreateSegment %s: %v", tc.name, err)
		}
		n, _, err := st.CountSegment(ctx, tenantID, s.ID)
		if err != nil {
			t.Fatalf("CountSegment %s: %v", tc.name, err)
		}
		if n != tc.want {
			t.Fatalf("%s: count = %d, want %d", tc.name, n, tc.want)
		}
	}

	// Cross-tenant count: the segment does not exist for another tenant.
	if _, _, err := st.CountSegment(ctx, otherTenant, seg.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant count = %v, want ErrNotFound", err)
	}
}

// Status machine through the store (draft→active→paused↔active→archived).
func TestJourneyStatusMachine(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	j := Journey{TenantID: tenantID, Name: "Winback", TriggerKind: TriggerManual, Steps: Steps{
		{Type: StepSend, Kind: KindSMS, Template: "Hi {name}"},
	}}
	if err := st.CreateJourney(ctx, &j); err != nil {
		t.Fatalf("CreateJourney: %v", err)
	}
	if j.Status != StatusDraft {
		t.Fatalf("new journey status = %q, want draft", j.Status)
	}

	// draft → paused is illegal.
	if _, err := st.TransitionJourney(ctx, tenantID, j.ID, StatusPaused); !errors.Is(err, ErrConflict) {
		t.Fatalf("draft → paused = %v, want ErrConflict", err)
	}

	for _, to := range []string{StatusActive, StatusPaused, StatusActive, StatusArchived} {
		got, err := st.TransitionJourney(ctx, tenantID, j.ID, to)
		if err != nil {
			t.Fatalf("transition → %s: %v", to, err)
		}
		if got.Status != to {
			t.Fatalf("status = %q, want %q", got.Status, to)
		}
	}
	// archived is terminal.
	if _, err := st.TransitionJourney(ctx, tenantID, j.ID, StatusActive); !errors.Is(err, ErrConflict) {
		t.Fatalf("archived → active = %v, want ErrConflict", err)
	}
	// Cross-tenant transition: not found.
	if _, err := st.TransitionJourney(ctx, uuid.New(), j.ID, StatusActive); err != ErrNotFound {
		t.Fatalf("cross-tenant transition = %v, want ErrNotFound", err)
	}
}

// Enrollment idempotency (SPEC-W19: idempotent per journey+contact).
func TestEnrollIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	c1, c2 := uuid.New(), uuid.New()

	j := mkActiveJourney(t, st, tenantID, Steps{{Type: StepWait, WaitHours: 1}})

	created, existing, err := st.Enroll(ctx, tenantID, j.ID, []uuid.UUID{c1, c2})
	if err != nil || len(created) != 2 || existing != 0 {
		t.Fatalf("first enroll: created=%d existing=%d err=%v", len(created), existing, err)
	}
	// Replay: nothing new, everything existing.
	created, existing, err = st.Enroll(ctx, tenantID, j.ID, []uuid.UUID{c1, c2, c1})
	if err != nil || len(created) != 0 || existing != 3 {
		t.Fatalf("replayed enroll: created=%d existing=%d err=%v", len(created), existing, err)
	}
	stats, err := st.Stats(ctx, tenantID, j)
	if err != nil || stats.Enrolled != 2 || stats.Active != 2 {
		t.Fatalf("stats after replay = %+v, %v", stats, err)
	}
}

func mkActiveJourney(t *testing.T, st *Store, tenantID uuid.UUID, steps Steps) Journey {
	t.Helper()
	j := Journey{TenantID: tenantID, Name: "J-" + uuid.NewString()[:8], TriggerKind: TriggerManual, Steps: steps}
	if err := st.CreateJourney(context.Background(), &j); err != nil {
		t.Fatalf("CreateJourney: %v", err)
	}
	j, err := st.TransitionJourney(context.Background(), tenantID, j.ID, StatusActive)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	return j
}

// Step advancement: wait-time logic, branch eval, send queueing, skips,
// completion — through Store.AdvanceDue (SPEC-W19 step semantics).
func TestAdvanceDue(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	now := time.Now().UTC()

	contactID := seedContact(t, st, tenantID, "Ada Lovelace", "+2348011111111", "ada@example.com", "twenty")
	seedLead(t, st, tenantID, "+2348011111111", "web", "qualified")
	noPhoneID := seedContact(t, st, tenantID, "No Phone", "", "np@example.com", "field")

	cond := &SegmentDefinition{Filters: []SegmentFilter{{Field: FieldLeadStatus, Op: OpEq, Value: "qualified"}}}
	steps := Steps{
		{Type: StepWait, WaitHours: 2},                           // 0
		{Type: StepSend, Kind: KindSMS, Template: "Hi {name}"},   // 1
		{Type: StepBranch, Condition: cond},                      // 2
		{Type: StepSend, Kind: KindPushMarketing, Template: "P"}, // 3
	}
	j := mkActiveJourney(t, st, tenantID, steps)
	if _, _, err := st.Enroll(ctx, tenantID, j.ID, []uuid.UUID{contactID, noPhoneID}); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	// 1) wait not due yet → nothing moves.
	res, err := st.AdvanceDue(ctx, tenantID, j, now, 100, true)
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if res.WaitNotDue != 2 || res.Advanced != 0 {
		t.Fatalf("advance 1 = %+v, want 2 wait_not_due", res)
	}

	// 2) after wait_hours, the wait passes → both at the send step.
	res, err = st.AdvanceDue(ctx, tenantID, j, now.Add(3*time.Hour), 100, true)
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if res.Advanced != 2 {
		t.Fatalf("advance 2 = %+v, want 2 advanced", res)
	}

	// 3) send step with dispatch DISABLED → deferred, nothing queued.
	res, err = st.AdvanceDue(ctx, tenantID, j, now.Add(3*time.Hour), 100, false)
	if err != nil {
		t.Fatalf("advance 3: %v", err)
	}
	if !res.SendsDeferred || len(res.Sends) != 0 || res.Advanced != 0 {
		t.Fatalf("advance 3 = %+v, want sends_deferred with no movement", res)
	}

	// 4) send step with dispatch → Ada queued; NoPhone skipped (missing
	// phone); both move to the branch step.
	res, err = st.AdvanceDue(ctx, tenantID, j, now.Add(3*time.Hour), 100, true)
	if err != nil {
		t.Fatalf("advance 4: %v", err)
	}
	if len(res.Sends) != 1 || res.Skipped != 1 || res.Advanced != 2 {
		t.Fatalf("advance 4 = %+v, want 1 send 1 skip 2 advanced", res)
	}
	qs := res.Sends[0]
	if qs.Kind != KindSMS || qs.Phone != "+2348011111111" || qs.Text != "Hi Ada Lovelace" || qs.StepIdx != 1 {
		t.Fatalf("queued send mismatch: %+v", qs)
	}

	// 5) branch step: Ada's lead is qualified → advances to the push step;
	// NoPhone has no lead → condition false → exits.
	res, err = st.AdvanceDue(ctx, tenantID, j, now.Add(3*time.Hour), 100, true)
	if err != nil {
		t.Fatalf("advance 5: %v", err)
	}
	if res.Advanced != 1 || res.Exited != 1 {
		t.Fatalf("advance 5 = %+v, want 1 advanced 1 exited", res)
	}

	// 6) push step → Ada queued; advancing past the last step completes.
	res, err = st.AdvanceDue(ctx, tenantID, j, now.Add(3*time.Hour), 100, true)
	if err != nil {
		t.Fatalf("advance 6: %v", err)
	}
	if len(res.Sends) != 1 || res.Completed != 1 || len(res.CompletedEnrollments) != 1 {
		t.Fatalf("advance 6 = %+v, want 1 send 1 completed", res)
	}
	if res.Sends[0].Kind != KindPushMarketing {
		t.Fatalf("last send kind = %q, want push_marketing", res.Sends[0].Kind)
	}

	// Stats: 2 enrolled, 1 completed, 1 exited; per-step counts consistent.
	stats, err := st.Stats(ctx, tenantID, j)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Enrolled != 2 || stats.Completed != 1 || stats.Exited != 1 || stats.Active != 0 {
		t.Fatalf("stats totals = %+v", stats)
	}
	if len(stats.PerStep) != 4 {
		t.Fatalf("per_step = %+v, want 4 entries", stats.PerStep)
	}
	if stats.PerStep[0].Passed != 2 { // wait passed by both
		t.Fatalf("step0 passed = %d, want 2", stats.PerStep[0].Passed)
	}
	if stats.PerStep[1].Passed != 2 || stats.PerStep[1].Skipped != 1 { // send queued + skip
		t.Fatalf("step1 = %+v, want passed 2 skipped 1", stats.PerStep[1])
	}
	if stats.PerStep[2].Exited != 1 { // branch false exit
		t.Fatalf("step2 = %+v, want exited 1", stats.PerStep[2])
	}
	if stats.PerStep[3].Passed != 1 { // push queued
		t.Fatalf("step3 = %+v, want passed 1", stats.PerStep[3])
	}

	// 7) idempotent step: no active enrollments left → all zeros.
	res, err = st.AdvanceDue(ctx, tenantID, j, now.Add(3*time.Hour), 100, true)
	if err != nil || res.Scanned != 0 || len(res.Sends) != 0 {
		t.Fatalf("advance 7 = %+v, %v — replay must be a no-op", res, err)
	}
}

// Send outcome recording (the workflow activity's store path) powers the
// sent/suppressed/failed per-step counters.
func TestRecordSendOutcome(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := seedContact(t, st, tenantID, "Ada", "+2348011111111", "a@b.c", "")

	j := mkActiveJourney(t, st, tenantID, Steps{
		{Type: StepSend, Kind: KindSMS, Template: "Hi"},
		{Type: StepWait, WaitHours: 1},
	})
	created, _, err := st.Enroll(ctx, tenantID, j.ID, []uuid.UUID{contactID})
	if err != nil || len(created) != 1 {
		t.Fatalf("enroll: %v", err)
	}
	res, err := st.AdvanceDue(ctx, tenantID, j, time.Now().UTC(), 10, true)
	if err != nil || len(res.Sends) != 1 {
		t.Fatalf("advance: %+v %v", res, err)
	}
	e := created[0]
	for _, outcome := range []struct{ kind, reason string }{
		{EventSendSent, ""},
		{EventSendSuppressed, "global_dnd"},
		{EventSendFailed, "provider boom"},
	} {
		if err := st.RecordSendOutcome(ctx, tenantID, j.ID, e.ID, 0, outcome.kind, outcome.reason); err != nil {
			t.Fatalf("record %s: %v", outcome.kind, err)
		}
	}
	stats, err := st.Stats(ctx, tenantID, j)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.PerStep[0].Sent != 1 || stats.PerStep[0].Suppressed != 1 || stats.PerStep[0].Failed != 1 {
		t.Fatalf("step0 = %+v, want sent 1 suppressed 1 failed 1", stats.PerStep[0])
	}
}

// RLS isolation: tenant B can never read/write tenant A's studio rows
// (app-level scoping + the forced tenant_isolation policy).
func TestRLSIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()

	seg := Segment{TenantID: tenantA, Name: "A-seg", Definition: SegmentDefinition{Filters: []SegmentFilter{
		{Field: FieldName, Op: OpContains, Value: "a"},
	}}}
	if err := st.CreateSegment(ctx, &seg); err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	j := Journey{TenantID: tenantA, Name: "A-journey", TriggerKind: TriggerManual, Steps: Steps{{Type: StepWait, WaitHours: 1}}}
	if err := st.CreateJourney(ctx, &j); err != nil {
		t.Fatalf("CreateJourney: %v", err)
	}

	// Cross-tenant reads: not found / empty.
	if _, err := st.GetSegment(ctx, tenantB, seg.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant GetSegment = %v, want ErrNotFound", err)
	}
	if _, err := st.GetJourney(ctx, tenantB, j.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant GetJourney = %v, want ErrNotFound", err)
	}
	segsB, err := st.ListSegments(ctx, tenantB)
	if err != nil || len(segsB) != 0 {
		t.Fatalf("cross-tenant ListSegments = %+v, %v", segsB, err)
	}
	jB, err := st.ListJourneys(ctx, tenantB, "")
	if err != nil || len(jB) != 0 {
		t.Fatalf("cross-tenant ListJourneys = %+v, %v", jB, err)
	}

	// Cross-tenant writes hit the same isolation (no row updated).
	if _, err := st.UpdateSegment(ctx, tenantB, seg.ID, nil, &seg.Definition); err != ErrNotFound {
		t.Fatalf("cross-tenant UpdateSegment = %v, want ErrNotFound", err)
	}

	// Belt-and-braces: verify the RLS policy actually exists and is
	// forced on every studio table (embedded postgres connects as a
	// superuser, which BYPASSES RLS — so row-level proof lives in the
	// cross-tenant checks above; here we assert the DDL posture).
	var unprotected string
	if err := st.pool.QueryRow(ctx,
		`SELECT tablename FROM pg_tables t
		  WHERE t.tablename LIKE 'studio\_%'
		    AND NOT EXISTS (SELECT 1 FROM pg_policies p
		                     WHERE p.tablename = t.tablename AND p.policyname = 'tenant_isolation')
		  LIMIT 1`).Scan(&unprotected); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("policy check: %v", err)
	}
	if unprotected != "" {
		t.Fatalf("studio table %q lacks the tenant_isolation policy", unprotected)
	}
}

// Outbox: the metering/events path writes drainable rows.
func TestEnqueueOutbox(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.EnqueueOutbox(ctx, uuid.New(), "opendesk.usage.events", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE sent_at IS NULL`).Scan(&n); err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	if n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
}
