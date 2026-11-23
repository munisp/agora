package store

// SPEC-W119 §5: sync-store tests against embedded Postgres (newTestStore
// idiom) — watermark advance, contact hash diff, deletion sweep, cursor
// resume, and explicit cross-tenant isolation of every sync query.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func insertSyncContact(t *testing.T, st *Store, tenantID uuid.UUID, name, phone string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := st.pool.Exec(context.Background(),
		`INSERT INTO contacts (id, tenant_id, name, phone, email, notes) VALUES ($1,$2,$3,$4,'','')`,
		id, tenantID, name, phone); err != nil {
		t.Fatalf("insert contact: %v", err)
	}
	return id
}

func insertSyncBooking(t *testing.T, st *Store, tenantID, contactID uuid.UUID, status string, updatedAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := st.pool.Exec(context.Background(),
		`INSERT INTO bookings (id, tenant_id, offering_id, contact_id, starts_at, ends_at, status, source, updated_at)
		 VALUES ($1,$2,$3,$4, now() + interval '1 day', now() + interval '1 day 1 hour', $5, 'web', $6)`,
		id, tenantID, uuid.New(), contactID, status, updatedAt); err != nil {
		t.Fatalf("insert booking: %v", err)
	}
	return id
}

func TestEnsureSearchSyncSchemaIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.EnsureSearchSyncSchema(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := st.EnsureSearchSyncSchema(ctx); err != nil {
		t.Fatalf("ensure (second run): %v", err)
	}
}

func TestSyncCursorRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.EnsureSearchSyncSchema(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	tid := uuid.New()
	wm, lastID, err := st.GetSyncCursor(ctx, tid, "bookings")
	if err != nil {
		t.Fatalf("get (absent): %v", err)
	}
	if !wm.IsZero() || lastID != uuid.Nil {
		t.Fatalf("zero cursor = %v/%v", wm, lastID)
	}
	want := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	id := uuid.New()
	if err := st.SetSyncCursor(ctx, tid, "bookings", want, id); err != nil {
		t.Fatalf("set: %v", err)
	}
	wm, lastID, err = st.GetSyncCursor(ctx, tid, "bookings")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !wm.Equal(want) || lastID != id {
		t.Fatalf("cursor = %v/%v", wm, lastID)
	}
	// Upsert advances the same row (no duplicate key).
	later := want.Add(time.Hour)
	if err := st.SetSyncCursor(ctx, tid, "bookings", later, id); err != nil {
		t.Fatalf("set (advance): %v", err)
	}
	wm, _, err = st.GetSyncCursor(ctx, tid, "bookings")
	if err != nil || !wm.Equal(later) {
		t.Fatalf("advanced cursor = %v (%v)", wm, err)
	}
}

func TestFetchBookingsForSyncWatermarkAndTenantIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	contactA := insertSyncContact(t, st, tenantA, "Ada", "+234801")
	_ = insertSyncContact(t, st, tenantB, "Mallam", "+234899")

	base := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
	b1 := insertSyncBooking(t, st, tenantA, contactA, "confirmed", base)
	b2 := insertSyncBooking(t, st, tenantA, contactA, "pending", base.Add(time.Hour))
	_ = insertSyncBooking(t, st, tenantB, uuid.Nil, "confirmed", base.Add(30*time.Minute))

	rows, err := st.FetchBookingsForSync(ctx, tenantA, time.Time{}, uuid.Nil, 500)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (tenant-B row must be invisible)", len(rows))
	}
	if rows[0].ID != b1 || rows[1].ID != b2 {
		t.Fatalf("order = %v,%v", rows[0].ID, rows[1].ID)
	}
	if rows[0].ContactName != "Ada" || rows[0].ContactPhone != "+234801" {
		t.Fatalf("contact denormalization = %#v", rows[0])
	}

	// Resume from the first row's cursor -> only b2.
	rows, err = st.FetchBookingsForSync(ctx, tenantA, rows[0].UpdatedAt, rows[0].ID, 500)
	if err != nil {
		t.Fatalf("fetch (resume): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != b2 {
		t.Fatalf("resume rows = %#v", rows)
	}
}

func TestFetchLeadsForSyncWatermark(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()

	l1 := Lead{TenantID: tenantA, PhoneE164: "+234801000001", ChannelOfFirstTouch: "whatsapp", DedupeKey: "k1"}
	if _, err := st.InsertLead(ctx, &l1); err != nil {
		t.Fatalf("insert lead 1: %v", err)
	}
	l2 := Lead{TenantID: tenantA, PhoneE164: "+234801000002", ChannelOfFirstTouch: "qr", DedupeKey: "k2"}
	if _, err := st.InsertLead(ctx, &l2); err != nil {
		t.Fatalf("insert lead 2: %v", err)
	}
	lB := Lead{TenantID: tenantB, PhoneE164: "+234809000001", ChannelOfFirstTouch: "web", DedupeKey: "k1"}
	if _, err := st.InsertLead(ctx, &lB); err != nil {
		t.Fatalf("insert lead B: %v", err)
	}

	rows, err := st.FetchLeadsForSync(ctx, tenantA, time.Time{}, uuid.Nil, 500)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (tenant-B lead invisible)", len(rows))
	}
	// Resume past the first row.
	rows, err = st.FetchLeadsForSync(ctx, tenantA, rows[0].UpdatedAt, rows[0].ID, 500)
	if err != nil {
		t.Fatalf("fetch (resume): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("resume rows = %d", len(rows))
	}
}

func TestScanContactsForSyncAndHashes(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.EnsureSearchSyncSchema(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	tenantA, tenantB := uuid.New(), uuid.New()
	c1 := insertSyncContact(t, st, tenantA, "Ada", "+234801")
	c2 := insertSyncContact(t, st, tenantA, "Bola", "+234802")
	_ = insertSyncContact(t, st, tenantB, "Mallam", "+234899")

	rows, err := st.ScanContactsForSync(ctx, tenantA, uuid.Nil, 500)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (tenant-B contact invisible)", len(rows))
	}

	// Hash lifecycle: empty map -> upsert -> present -> delete -> gone.
	hashes, err := st.ContactHashMap(ctx, tenantA)
	if err != nil || len(hashes) != 0 {
		t.Fatalf("hashes = %v %v", hashes, err)
	}
	if err := st.UpsertContactHash(ctx, tenantA, c1, "h1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.UpsertContactHash(ctx, tenantA, c2, "h2"); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	hashes, err = st.ContactHashMap(ctx, tenantA)
	if err != nil || len(hashes) != 2 || hashes[c1] != "h1" {
		t.Fatalf("hashes = %v %v", hashes, err)
	}
	if err := st.DeleteContactHash(ctx, c1); err != nil {
		t.Fatalf("delete hash: %v", err)
	}
	hashes, _ = st.ContactHashMap(ctx, tenantA)
	if len(hashes) != 1 {
		t.Fatalf("hashes after delete = %v", hashes)
	}
}

func TestDeletedContactIDsAntiJoin(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.EnsureSearchSyncSchema(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	tid := uuid.New()
	c1 := insertSyncContact(t, st, tid, "Ada", "+234801")
	c2 := insertSyncContact(t, st, tid, "Bola", "+234802")
	if err := st.UpsertContactHash(ctx, tid, c1, "h1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.UpsertContactHash(ctx, tid, c2, "h2"); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}

	// Nothing deleted yet.
	ids, err := st.DeletedContactIDs(ctx, tid)
	if err != nil || len(ids) != 0 {
		t.Fatalf("deleted = %v %v", ids, err)
	}

	// Hard-delete c2 -> the anti-join surfaces exactly c2.
	if _, err := st.pool.Exec(ctx, `DELETE FROM contacts WHERE id=$1`, c2); err != nil {
		t.Fatalf("hard delete: %v", err)
	}
	ids, err = st.DeletedContactIDs(ctx, tid)
	if err != nil {
		t.Fatalf("deleted: %v", err)
	}
	if len(ids) != 1 || ids[0] != c2 {
		t.Fatalf("deleted = %v, want [%v]", ids, c2)
	}
}
