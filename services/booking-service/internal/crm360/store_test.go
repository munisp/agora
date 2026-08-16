package crm360

import (
	"context"
	"errors"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
)

// SPEC-W20 Agent A store tests run against embedded Postgres (same harness
// as the W19 store tests; dedicated port 5564 avoids the postmaster.pid
// race with sibling packages under `go test ./...`; -short skips).
//
// The harness bootstraps minimal mirrors of the W13–W19 domain tables the
// 360 aggregation JOINS against (owned by the shared store / helpdesk /
// workorders / loyalty packages in production — mirrored here exactly like
// workorders/store_test.go mirrors team_members + outbox).

// testSupportDDL mirrors the real tables the aggregation reads, with the
// columns the queries touch (plus the RLS policies the production DDL
// carries, so the isolation test exercises the real defence-in-depth).
const testSupportDDL = `
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
ALTER TABLE contacts ENABLE ROW LEVEL SECURITY;
ALTER TABLE contacts FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'contacts' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON contacts
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS bookings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    contact_id UUID,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    source TEXT NOT NULL DEFAULT 'voice',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE bookings ENABLE ROW LEVEL SECURITY;
ALTER TABLE bookings FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'bookings' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON bookings
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS tickets (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    contact_id UUID,
    subject TEXT NOT NULL,
    priority TEXT NOT NULL DEFAULT 'normal',
    status TEXT NOT NULL DEFAULT 'open',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE tickets ENABLE ROW LEVEL SECURITY;
ALTER TABLE tickets FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'tickets' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON tickets
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS ticket_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    ticket_id UUID NOT NULL,
    kind TEXT NOT NULL,
    actor TEXT,
    payload JSONB NOT NULL DEFAULT '{}',
    ts TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE ticket_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE ticket_events FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'ticket_events' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON ticket_events
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS work_orders (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    contact_id UUID,
    title TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'created',
    scheduled_start TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);
ALTER TABLE work_orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_orders FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'work_orders' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON work_orders
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS loyalty_wallets (
    tenant_id UUID NOT NULL,
    contact_id UUID NOT NULL,
    balance BIGINT NOT NULL DEFAULT 0,
    tier TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, contact_id)
);
ALTER TABLE loyalty_wallets ENABLE ROW LEVEL SECURITY;
ALTER TABLE loyalty_wallets FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'loyalty_wallets' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON loyalty_wallets
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS loyalty_ledger (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    journal_id UUID NOT NULL,
    account_code INTEGER NOT NULL,
    beneficiary_id TEXT NOT NULL DEFAULT '',
    debit_points BIGINT NOT NULL DEFAULT 0,
    credit_points BIGINT NOT NULL DEFAULT 0,
    ref_type TEXT NOT NULL,
    ref_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE loyalty_ledger ENABLE ROW LEVEL SECURITY;
ALTER TABLE loyalty_ledger FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'loyalty_ledger' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON loyalty_ledger
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at TIMESTAMPTZ
);`

// testContactsOnlyDDL is the DEGRADED-deployment harness: only the base
// contacts table (+ outbox) exists — the W19 app tables are absent, which
// is exactly the "missing optional source" scenario the 360 aggregation
// must survive (SPEC-W20 Agent A).
const testContactsOnlyDDL = `
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
CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at TIMESTAMPTZ
);`

func startEmbedded(t *testing.T) *Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres crm360 store test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_crm360_test").
		Port(5564).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })
	st, err := DialStore(context.Background(),
		"postgres://postgres:postgres@localhost:5564/booking_crm360_test?sslmode=disable")
	if err != nil {
		t.Fatalf("DialStore: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// newTestStore boots embedded Postgres with the full support schema.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	st := startEmbedded(t)
	if _, err := st.pool.Exec(context.Background(), testSupportDDL); err != nil {
		t.Fatalf("support DDL: %v", err)
	}
	return st
}

// newDegradedStore boots embedded Postgres with ONLY contacts + outbox —
// every optional 360 source is absent.
func newDegradedStore(t *testing.T) *Store {
	t.Helper()
	st := startEmbedded(t)
	if _, err := st.pool.Exec(context.Background(), testContactsOnlyDDL); err != nil {
		t.Fatalf("contacts-only DDL: %v", err)
	}
	return st
}

// addContact inserts a contact directly (superuser session — bypasses RLS,
// same as the W19 test helpers inserting team_members).
func addContact(t *testing.T, st *Store, tenantID uuid.UUID, name, phone, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := st.pool.Exec(context.Background(),
		`INSERT INTO contacts (id, tenant_id, name, phone, email) VALUES ($1,$2,$3,$4,$5)`,
		id, tenantID, name, phone, email)
	if err != nil {
		t.Fatalf("insert contact: %v", err)
	}
	return id
}

func addBooking(t *testing.T, st *Store, tenantID, contactID uuid.UUID, status string, startsAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := st.pool.Exec(context.Background(),
		`INSERT INTO bookings (id, tenant_id, contact_id, starts_at, ends_at, status)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		id, tenantID, contactID, startsAt, startsAt.Add(time.Hour), status)
	if err != nil {
		t.Fatalf("insert booking: %v", err)
	}
	return id
}

func addTicket(t *testing.T, st *Store, tenantID, contactID uuid.UUID, subject, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := st.pool.Exec(context.Background(),
		`INSERT INTO tickets (id, tenant_id, contact_id, subject, status) VALUES ($1,$2,$3,$4,$5)`,
		id, tenantID, contactID, subject, status)
	if err != nil {
		t.Fatalf("insert ticket: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Notes
// ---------------------------------------------------------------------------

// Create → Get/List round-trip; pinned ordering; Update stamps updated_at.
func TestNotesCRUD(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := addContact(t, st, tenantID, "Ada Lovelace", "+2348000000001", "ada@example.com")

	first := Note{TenantID: tenantID, ContactID: contactID, Author: "agent-1", Body: "first note"}
	if err := first.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := st.CreateNote(ctx, &first); err != nil {
		t.Fatalf("create: %v", err)
	}
	if first.ID == uuid.Nil || first.CreatedAt.IsZero() {
		t.Fatalf("id/timestamps not stamped: %+v", first)
	}

	time.Sleep(5 * time.Millisecond) // deterministic created_at ordering
	second := Note{TenantID: tenantID, ContactID: contactID, Author: "agent-2", Body: "pinned note", Pinned: true}
	if err := st.CreateNote(ctx, &second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	notes, err := st.ListNotes(ctx, tenantID, contactID)
	if err != nil || len(notes) != 2 {
		t.Fatalf("list: %+v, %v", notes, err)
	}
	if !notes[0].Pinned || notes[0].Body != "pinned note" {
		t.Fatalf("pinned note must sort first: %+v", notes)
	}

	// Edit body + unpin.
	second.Body = "edited body"
	second.Pinned = false
	before := second.UpdatedAt
	time.Sleep(5 * time.Millisecond)
	if err := st.UpdateNote(ctx, &second); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !second.UpdatedAt.After(before) {
		t.Fatalf("updated_at not stamped: %v vs %v", second.UpdatedAt, before)
	}
	got, err := st.GetNote(ctx, tenantID, second.ID)
	if err != nil || got.Body != "edited body" || got.Pinned {
		t.Fatalf("get after update: %+v, %v", got, err)
	}

	// Missing note → ErrNotFound.
	if _, err := st.GetNote(ctx, tenantID, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing note = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Tags
// ---------------------------------------------------------------------------

func TestTags(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := addContact(t, st, tenantID, "Grace Hopper", "", "")

	if err := st.AddTag(ctx, tenantID, contactID, "vip"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.AddTag(ctx, tenantID, contactID, "gold-tier"); err != nil {
		t.Fatalf("add second: %v", err)
	}
	// Idempotent replay: re-adding is a no-op, not an error.
	if err := st.AddTag(ctx, tenantID, contactID, "vip"); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	tags, err := st.ListTags(ctx, tenantID, contactID)
	if err != nil || len(tags) != 2 || tags[0] != "gold-tier" || tags[1] != "vip" {
		t.Fatalf("tags: %+v, %v", tags, err)
	}

	if err := st.RemoveTag(ctx, tenantID, contactID, "vip"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := st.RemoveTag(ctx, tenantID, contactID, "vip"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove missing = %v, want ErrNotFound", err)
	}
	tags, err = st.ListTags(ctx, tenantID, contactID)
	if err != nil || len(tags) != 1 || tags[0] != "gold-tier" {
		t.Fatalf("tags after remove: %+v, %v", tags, err)
	}
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

func TestSearchContacts(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	ada := addContact(t, st, tenantID, "Ada Lovelace", "+2348011111111", "ada@example.com")
	grace := addContact(t, st, tenantID, "Grace Hopper", "+2348022222222", "grace@example.com")
	_ = addContact(t, st, tenantID, "Alan Turing", "+447700900123", "alan@example.co.uk")
	if err := st.AddTag(ctx, tenantID, ada, "vip"); err != nil {
		t.Fatalf("tag ada: %v", err)
	}
	if err := st.AddTag(ctx, tenantID, grace, "vip"); err != nil {
		t.Fatalf("tag grace: %v", err)
	}

	// Name prefix (case-insensitive).
	res, err := st.SearchContacts(ctx, tenantID, "ada", "", 0)
	if err != nil || len(res) != 1 || res[0].ID != ada {
		t.Fatalf("name prefix: %+v, %v", res, err)
	}
	if len(res[0].Tags) != 1 || res[0].Tags[0] != "vip" {
		t.Fatalf("tags attached to result: %+v", res[0])
	}
	// Phone prefix.
	res, err = st.SearchContacts(ctx, tenantID, "+23480", "", 0)
	if err != nil || len(res) != 2 {
		t.Fatalf("phone prefix: %+v, %v", res, err)
	}
	// Email prefix.
	res, err = st.SearchContacts(ctx, tenantID, "alan@", "", 0)
	if err != nil || len(res) != 1 {
		t.Fatalf("email prefix: %+v, %v", res, err)
	}
	// Substring must NOT match (prefix semantics).
	res, err = st.SearchContacts(ctx, tenantID, "lovelace", "", 0)
	if err != nil || len(res) != 0 {
		t.Fatalf("substring must not match: %+v, %v", res, err)
	}
	// Tag filter alone.
	res, err = st.SearchContacts(ctx, tenantID, "", "vip", 0)
	if err != nil || len(res) != 2 {
		t.Fatalf("tag filter: %+v, %v", res, err)
	}
	// Tag filter + q.
	res, err = st.SearchContacts(ctx, tenantID, "grace", "vip", 0)
	if err != nil || len(res) != 1 || res[0].ID != grace {
		t.Fatalf("tag+q: %+v, %v", res, err)
	}
	// Limit.
	res, err = st.SearchContacts(ctx, tenantID, "", "vip", 1)
	if err != nil || len(res) != 1 {
		t.Fatalf("limit: %+v, %v", res, err)
	}
}

// ---------------------------------------------------------------------------
// 360 profile
// ---------------------------------------------------------------------------

func TestProfile360(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := addContact(t, st, tenantID, "Ada Lovelace", "+2348011111111", "ada@example.com")

	// Tickets: 2 open, 1 closed.
	addTicket(t, st, tenantID, contactID, "POS offline", "open")
	addTicket(t, st, tenantID, contactID, "Refund query", "pending")
	addTicket(t, st, tenantID, contactID, "Old issue", "closed")
	// Bookings.
	addBooking(t, st, tenantID, contactID, "completed", time.Now().Add(-48*time.Hour))
	addBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(24*time.Hour))
	// Work orders: one active, one completed (must not appear).
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO work_orders (tenant_id, contact_id, title, status) VALUES ($1,$2,$3,$4)`,
		tenantID, contactID, "Install terminal", "assigned"); err != nil {
		t.Fatalf("insert work order: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO work_orders (tenant_id, contact_id, title, status, completed_at) VALUES ($1,$2,$3,$4,now())`,
		tenantID, contactID, "Old job", "completed"); err != nil {
		t.Fatalf("insert completed work order: %v", err)
	}
	// Wallet.
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO loyalty_wallets (tenant_id, contact_id, balance, tier) VALUES ($1,$2,$3,$4)`,
		tenantID, contactID, 1250, "gold"); err != nil {
		t.Fatalf("insert wallet: %v", err)
	}
	if err := st.AddTag(ctx, tenantID, contactID, "vip"); err != nil {
		t.Fatalf("tag: %v", err)
	}

	p, err := st.Profile360(ctx, tenantID, contactID)
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	if p.Contact.Name != "Ada Lovelace" || p.Contact.Email != "ada@example.com" {
		t.Fatalf("contact: %+v", p.Contact)
	}
	if len(p.Tags) != 1 || p.Tags[0] != "vip" {
		t.Fatalf("tags: %+v", p.Tags)
	}
	if p.OpenTicketCount != 2 {
		t.Fatalf("open ticket count = %d, want 2", p.OpenTicketCount)
	}
	if len(p.Tickets) != 3 {
		t.Fatalf("tickets section: %+v", p.Tickets)
	}
	if len(p.Bookings) != 2 {
		t.Fatalf("bookings section: %+v", p.Bookings)
	}
	// Latest booking first (future confirmed).
	if p.Bookings[0].Status != "confirmed" {
		t.Fatalf("booking ordering: %+v", p.Bookings)
	}
	if len(p.WorkOrders) != 1 || p.WorkOrders[0].Title != "Install terminal" {
		t.Fatalf("active work orders: %+v", p.WorkOrders)
	}
	if p.Wallet == nil || p.Wallet.Balance != 1250 || p.Wallet.Tier != "gold" {
		t.Fatalf("wallet: %+v", p.Wallet)
	}
	if p.Consent != nil {
		t.Fatalf("consent must be null without a resolver: %+v", p.Consent)
	}

	// Contact without a wallet → wallet null, sections empty (not error).
	other := addContact(t, st, tenantID, "No History", "", "")
	p2, err := st.Profile360(ctx, tenantID, other)
	if err != nil {
		t.Fatalf("profile empty contact: %v", err)
	}
	if p2.Wallet != nil || p2.OpenTicketCount != 0 || len(p2.Tickets) != 0 || len(p2.Bookings) != 0 || len(p2.WorkOrders) != 0 {
		t.Fatalf("empty contact sections: %+v", p2)
	}

	// Unknown contact → ErrNotFound.
	if _, err := st.Profile360(ctx, tenantID, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown contact = %v, want ErrNotFound", err)
	}
}

// Degraded deployment: helpdesk/workorders/loyalty tables absent — the
// profile and timeline must degrade to empty sections, NEVER error
// (SPEC-W20 Agent A).
func TestProfile360DegradedSources(t *testing.T) {
	st := newDegradedStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := addContact(t, st, tenantID, "Solo Contact", "", "")

	p, err := st.Profile360(ctx, tenantID, contactID)
	if err != nil {
		t.Fatalf("degraded profile must not error: %v", err)
	}
	if p.Contact.Name != "Solo Contact" {
		t.Fatalf("contact: %+v", p.Contact)
	}
	if p.OpenTicketCount != 0 || len(p.Tickets) != 0 || len(p.Bookings) != 0 ||
		len(p.WorkOrders) != 0 || p.Wallet != nil {
		t.Fatalf("degraded sections must be empty: %+v", p)
	}

	// Timeline also survives with only crm_notes present.
	n := Note{TenantID: tenantID, ContactID: contactID, Author: "a", Body: "lone note"}
	if err := st.CreateNote(ctx, &n); err != nil {
		t.Fatalf("create note: %v", err)
	}
	items, err := st.Timeline(ctx, tenantID, contactID, 50)
	if err != nil {
		t.Fatalf("degraded timeline must not error: %v", err)
	}
	if len(items) != 1 || items[0].Kind != KindNote {
		t.Fatalf("degraded timeline: %+v", items)
	}
}

// ---------------------------------------------------------------------------
// Timeline
// ---------------------------------------------------------------------------

func TestTimeline(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := addContact(t, st, tenantID, "Ada Lovelace", "", "")

	addBooking(t, st, tenantID, contactID, "confirmed", time.Now().Add(24*time.Hour))
	ticketID := addTicket(t, st, tenantID, contactID, "POS offline", "open")
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO ticket_events (tenant_id, ticket_id, kind, actor) VALUES ($1,$2,$3,$4)`,
		tenantID, ticketID, "created", "agent-1"); err != nil {
		t.Fatalf("insert ticket event: %v", err)
	}
	woID := uuid.New()
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO work_orders (id, tenant_id, contact_id, title, status, completed_at)
		 VALUES ($1,$2,$3,$4,$5,now())`, woID, tenantID, contactID, "Install terminal", "completed"); err != nil {
		t.Fatalf("insert work order: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO loyalty_ledger (tenant_id, journal_id, account_code, beneficiary_id, credit_points, ref_type, ref_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		tenantID, uuid.New(), 400, contactID.String(), 100, "booking_completed", uuid.NewString()); err != nil {
		t.Fatalf("insert ledger entry: %v", err)
	}
	n := Note{TenantID: tenantID, ContactID: contactID, Author: "agent-1", Body: "follow up Friday", Pinned: true}
	if err := st.CreateNote(ctx, &n); err != nil {
		t.Fatalf("create note: %v", err)
	}

	items, err := st.Timeline(ctx, tenantID, contactID, 50)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	// Expect: booking + ticket_event + work order created + work order
	// completed + loyalty + note = 6 items.
	if len(items) != 6 {
		t.Fatalf("timeline len = %d, want 6: %+v", len(items), items)
	}
	kinds := map[string]int{}
	for _, it := range items {
		kinds[it.Kind]++
		if it.RefID == "" || it.Summary == "" || it.TS.IsZero() {
			t.Fatalf("malformed item: %+v", it)
		}
	}
	if kinds[KindBooking] != 1 || kinds[KindTicketEvent] != 1 || kinds[KindWorkOrder] != 2 ||
		kinds[KindLoyalty] != 1 || kinds[KindNote] != 1 {
		t.Fatalf("kind mix: %+v", kinds)
	}
	// Newest-first ordering.
	for i := 1; i < len(items); i++ {
		if items[i-1].TS.Before(items[i].TS) {
			t.Fatalf("not newest-first: %+v", items)
		}
	}

	// Limit cap.
	capped, err := st.Timeline(ctx, tenantID, contactID, 2)
	if err != nil || len(capped) != 2 {
		t.Fatalf("limit cap: %+v, %v", capped, err)
	}
}

// ---------------------------------------------------------------------------
// RLS tenant isolation (contract §1)
// ---------------------------------------------------------------------------

// The crm tables carry FORCE RLS tenant_isolation (verified against
// pg_policies), and tenant B cannot read/write tenant A's rows even when
// it knows the IDs.
func TestRLSIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// The DDL must have installed the policies.
	for _, table := range []string{"crm_notes", "crm_tags"} {
		var n int
		if err := st.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM pg_policies WHERE tablename=$1 AND policyname='tenant_isolation'`,
			table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("tenant_isolation policy on %s: n=%d, %v", table, n, err)
		}
	}

	tenantA, tenantB := uuid.New(), uuid.New()
	contactA := addContact(t, st, tenantA, "Tenant A Contact", "+2348011111111", "a@example.com")
	_ = addContact(t, st, tenantB, "Tenant B Contact", "+2348099999999", "b@example.com")

	note := Note{TenantID: tenantA, ContactID: contactA, Author: "agent", Body: "A secret note"}
	if err := st.CreateNote(ctx, &note); err != nil {
		t.Fatalf("create note: %v", err)
	}
	if err := st.AddTag(ctx, tenantA, contactA, "vip"); err != nil {
		t.Fatalf("add tag: %v", err)
	}

	// Tenant B reads: nothing leaks.
	notesB, err := st.ListNotes(ctx, tenantB, contactA)
	if err != nil || len(notesB) != 0 {
		t.Fatalf("tenant B notes leaked: %+v, %v", notesB, err)
	}
	tagsB, err := st.ListTags(ctx, tenantB, contactA)
	if err != nil || len(tagsB) != 0 {
		t.Fatalf("tenant B tags leaked: %+v, %v", tagsB, err)
	}
	if _, err := st.GetNote(ctx, tenantB, note.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant B get note = %v, want ErrNotFound", err)
	}
	if _, err := st.GetContact(ctx, tenantB, contactA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant B get contact = %v, want ErrNotFound", err)
	}
	if _, err := st.Profile360(ctx, tenantB, contactA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant B profile = %v, want ErrNotFound", err)
	}
	searchB, err := st.SearchContacts(ctx, tenantB, "Tenant A", "", 0)
	if err != nil || len(searchB) != 0 {
		t.Fatalf("tenant B search leaked: %+v, %v", searchB, err)
	}
	tlB, err := st.Timeline(ctx, tenantB, contactA, 50)
	if err != nil || len(tlB) != 0 {
		t.Fatalf("tenant B timeline leaked: %+v, %v", tlB, err)
	}

	// Cross-tenant writes must hit 0 rows / fail, and tenant A's data must
	// survive untouched.
	note.TenantID = tenantB
	note.Body = "hijacked"
	if err := st.UpdateNote(ctx, &note); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant update = %v, want ErrNotFound", err)
	}
	if err := st.RemoveTag(ctx, tenantB, contactA, "vip"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant tag remove = %v, want ErrNotFound", err)
	}
	again, err := st.GetNote(ctx, tenantA, note.ID)
	if err != nil || again.Body != "A secret note" {
		t.Fatalf("row changed by cross-tenant update: %+v, %v", again, err)
	}
	tagsA, err := st.ListTags(ctx, tenantA, contactA)
	if err != nil || len(tagsA) != 1 || tagsA[0] != "vip" {
		t.Fatalf("tag changed by cross-tenant remove: %+v, %v", tagsA, err)
	}
}
