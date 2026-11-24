package search

// SPEC-W119 §5: syncer behavior tests with a fake SyncStore + the
// osIndex/osDelete seams — watermark advance, idempotent re-sweep, contact
// hash diff, deletion purge, poison-tenant skip. No database, no cluster.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opendesk/booking-service/internal/store"
)

type cursor struct {
	wm time.Time
	id uuid.UUID
}

type fakeSyncStore struct {
	tenants     []uuid.UUID
	bookings    map[uuid.UUID][]store.SyncBooking
	leads       map[uuid.UUID][]store.SyncLead
	contacts    map[uuid.UUID][]store.SyncContact
	cursors     map[string]cursor
	hashes      map[uuid.UUID]map[uuid.UUID]string
	failTenants map[uuid.UUID]bool
}

func newFakeSyncStore(tenants ...uuid.UUID) *fakeSyncStore {
	return &fakeSyncStore{
		tenants:     tenants,
		bookings:    map[uuid.UUID][]store.SyncBooking{},
		leads:       map[uuid.UUID][]store.SyncLead{},
		contacts:    map[uuid.UUID][]store.SyncContact{},
		cursors:     map[string]cursor{},
		hashes:      map[uuid.UUID]map[uuid.UUID]string{},
		failTenants: map[uuid.UUID]bool{},
	}
}

func (f *fakeSyncStore) EnsureSearchSyncSchema(ctx context.Context) error { return nil }
func (f *fakeSyncStore) ListSyncTenantIDs(ctx context.Context) ([]uuid.UUID, error) {
	return f.tenants, nil
}
func (f *fakeSyncStore) GetSyncCursor(ctx context.Context, tid uuid.UUID, entity string) (time.Time, uuid.UUID, error) {
	if f.failTenants[tid] {
		return time.Time{}, uuid.Nil, errors.New("poison tenant")
	}
	c := f.cursors[tid.String()+"/"+entity]
	return c.wm, c.id, nil
}
func (f *fakeSyncStore) SetSyncCursor(ctx context.Context, tid uuid.UUID, entity string, wm time.Time, lastID uuid.UUID) error {
	f.cursors[tid.String()+"/"+entity] = cursor{wm: wm, id: lastID}
	return nil
}

func afterCursor(updated time.Time, id uuid.UUID, wm time.Time, lastID uuid.UUID) bool {
	if updated.After(wm) {
		return true
	}
	return updated.Equal(wm) && strings.Compare(id.String(), lastID.String()) > 0
}

func (f *fakeSyncStore) FetchBookingsForSync(ctx context.Context, tid uuid.UUID, after time.Time, afterID uuid.UUID, limit int) ([]store.SyncBooking, error) {
	var out []store.SyncBooking
	for _, b := range f.bookings[tid] {
		if afterCursor(b.UpdatedAt, b.ID, after, afterID) {
			out = append(out, b)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeSyncStore) FetchLeadsForSync(ctx context.Context, tid uuid.UUID, after time.Time, afterID uuid.UUID, limit int) ([]store.SyncLead, error) {
	var out []store.SyncLead
	for _, l := range f.leads[tid] {
		if afterCursor(l.UpdatedAt, l.ID, after, afterID) {
			out = append(out, l)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeSyncStore) ScanContactsForSync(ctx context.Context, tid uuid.UUID, afterID uuid.UUID, limit int) ([]store.SyncContact, error) {
	var out []store.SyncContact
	for _, c := range f.contacts[tid] {
		if strings.Compare(c.ID.String(), afterID.String()) > 0 {
			out = append(out, c)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeSyncStore) ContactHashMap(ctx context.Context, tid uuid.UUID) (map[uuid.UUID]string, error) {
	if f.hashes[tid] == nil {
		return map[uuid.UUID]string{}, nil
	}
	return f.hashes[tid], nil
}
func (f *fakeSyncStore) UpsertContactHash(ctx context.Context, tid, cid uuid.UUID, h string) error {
	if f.hashes[tid] == nil {
		f.hashes[tid] = map[uuid.UUID]string{}
	}
	f.hashes[tid][cid] = h
	return nil
}
func (f *fakeSyncStore) DeleteContactHash(ctx context.Context, cid uuid.UUID) error {
	for tid := range f.hashes {
		delete(f.hashes[tid], cid)
	}
	return nil
}
func (f *fakeSyncStore) DeletedContactIDs(ctx context.Context, tid uuid.UUID) ([]uuid.UUID, error) {
	live := map[uuid.UUID]bool{}
	for _, c := range f.contacts[tid] {
		live[c.ID] = true
	}
	var out []uuid.UUID
	for cid := range f.hashes[tid] {
		if !live[cid] {
			out = append(out, cid)
		}
	}
	return out, nil
}

type captured struct {
	indexed map[string]map[string]any
	deleted []string
}

func newTestSyncer(f *fakeSyncStore) (*Syncer, *captured) {
	cap := &captured{indexed: map[string]map[string]any{}}
	s := &Syncer{
		Store:     f,
		BatchSize: DefaultBatchSize,
		osIndex: func(ctx context.Context, index, id string, doc any) error {
			cap.indexed[index+"/"+id] = doc.(map[string]any)
			return nil
		},
		osDelete: func(ctx context.Context, index, id string) error {
			cap.deleted = append(cap.deleted, index+"/"+id)
			delete(cap.indexed, index+"/"+id)
			return nil
		},
	}
	return s, cap
}

func TestSyncBookingsWatermarkAdvanceAndIdempotentResweep(t *testing.T) {
	tid := uuid.New()
	f := newFakeSyncStore(tid)
	b1 := store.SyncBooking{ID: uuid.New(), TenantID: tid, OfferingID: uuid.New(), Status: "confirmed", Source: "web", UpdatedAt: time.Now().Add(-time.Hour)}
	b2 := store.SyncBooking{ID: uuid.New(), TenantID: tid, OfferingID: uuid.New(), Status: "pending", Source: "api", UpdatedAt: time.Now()}
	f.bookings[tid] = []store.SyncBooking{b1, b2}
	s, cap := newTestSyncer(f)

	s.SweepOnce(context.Background())
	if len(cap.indexed) != 2 {
		t.Fatalf("indexed = %d, want 2", len(cap.indexed))
	}
	doc := cap.indexed[BookingsIndex+"/"+b2.ID.String()]
	if doc == nil || doc["tenant_id"] != tid.String() || doc["status"] != "pending" {
		t.Fatalf("doc = %#v", doc)
	}
	// Cursor advanced to the newest row.
	wm, lastID, _ := f.GetSyncCursor(context.Background(), tid, entityBookings)
	if !wm.Equal(b2.UpdatedAt) || lastID != b2.ID {
		t.Fatalf("cursor = %v/%v", wm, lastID)
	}

	// Re-sweep indexes NOTHING (idempotent resume).
	s.SweepOnce(context.Background())
	if len(cap.indexed) != 2 {
		t.Fatalf("after re-sweep indexed = %d, want still 2 (no re-index)", len(cap.indexed))
	}

	// A tombstone UPDATE (same id, newer updated_at) re-indexes the row.
	b2.Status = "cancelled"
	b2.UpdatedAt = time.Now().Add(time.Minute)
	f.bookings[tid] = []store.SyncBooking{b1, b2}
	s.SweepOnce(context.Background())
	doc = cap.indexed[BookingsIndex+"/"+b2.ID.String()]
	if doc["status"] != "cancelled" {
		t.Fatalf("tombstone update not re-indexed: %#v", doc)
	}
}

func TestSyncLeadsWatermark(t *testing.T) {
	tid := uuid.New()
	f := newFakeSyncStore(tid)
	l1 := store.SyncLead{ID: uuid.New(), TenantID: tid, PhoneE164: "+234801", ChannelOfFirstTouch: "whatsapp", Status: "new", UpdatedAt: time.Now()}
	promo := "LAGOS20"
	l2 := store.SyncLead{ID: uuid.New(), TenantID: tid, PhoneE164: "+234802", ChannelOfFirstTouch: "qr", Status: "new", PromoCode: &promo, UpdatedAt: time.Now()}
	f.leads[tid] = []store.SyncLead{l1, l2}
	s, cap := newTestSyncer(f)

	s.SweepOnce(context.Background())
	doc1 := cap.indexed[LeadsIndex+"/"+l1.ID.String()]
	doc2 := cap.indexed[LeadsIndex+"/"+l2.ID.String()]
	if doc1 == nil || doc2 == nil {
		t.Fatalf("leads not indexed: %#v", cap.indexed)
	}
	if _, hasPromo := doc1["promo_code"]; hasPromo {
		t.Fatalf("nil promo must be omitted: %#v", doc1)
	}
	if doc2["promo_code"] != "LAGOS20" {
		t.Fatalf("promo doc = %#v", doc2)
	}
}

func TestSyncContactsHashDiffAndDeletionPurge(t *testing.T) {
	tid := uuid.New()
	f := newFakeSyncStore(tid)
	c1 := store.SyncContact{ID: uuid.New(), TenantID: tid, Name: "Ada", Phone: "+234801"}
	c2 := store.SyncContact{ID: uuid.New(), TenantID: tid, Name: "Bola", Email: "bola@x.ng"}
	f.contacts[tid] = []store.SyncContact{c1, c2}
	s, cap := newTestSyncer(f)

	s.SweepOnce(context.Background())
	if len(cap.indexed) != 2 {
		t.Fatalf("indexed = %d, want 2", len(cap.indexed))
	}
	if len(f.hashes[tid]) != 2 {
		t.Fatalf("hashes = %d", len(f.hashes[tid]))
	}

	// Change c1's name -> exactly one re-index.
	c1.Name = "Adaeze"
	f.contacts[tid] = []store.SyncContact{c1, c2}
	s.SweepOnce(context.Background())
	doc := cap.indexed[ContactsIndex+"/"+c1.ID.String()]
	if doc["name"] != "Adaeze" {
		t.Fatalf("changed contact not re-indexed: %#v", doc)
	}

	// Hard-delete c2 -> doc purged + hash row dropped.
	f.contacts[tid] = []store.SyncContact{c1}
	s.SweepOnce(context.Background())
	if _, stillThere := cap.indexed[ContactsIndex+"/"+c2.ID.String()]; stillThere {
		t.Fatalf("deleted contact doc not purged")
	}
	if _, hashThere := f.hashes[tid][c2.ID]; hashThere {
		t.Fatalf("deleted contact hash not dropped")
	}
	found := false
	for _, d := range cap.deleted {
		if d == ContactsIndex+"/"+c2.ID.String() {
			found = true
		}
	}
	if !found {
		t.Fatalf("delete not issued: %#v", cap.deleted)
	}
}

func TestPoisonTenantSkippedOthersContinue(t *testing.T) {
	bad, good := uuid.New(), uuid.New()
	f := newFakeSyncStore(bad, good)
	f.failTenants[bad] = true
	f.contacts[good] = []store.SyncContact{{ID: uuid.New(), TenantID: good, Name: "Chi"}}
	s, cap := newTestSyncer(f)

	s.SweepOnce(context.Background()) // must not panic/abort on the poison tenant
	if len(cap.indexed) != 1 {
		t.Fatalf("good tenant skipped after poison: %#v", cap.indexed)
	}
}

func TestContactContentHashStableAndFieldSeparated(t *testing.T) {
	c := store.SyncContact{Name: "Ada", Phone: "+234801", Email: "", Notes: "vip", Source: "crm", ExternalID: "x1"}
	if contactContentHash(c) != contactContentHash(c) {
		t.Fatal("hash not deterministic")
	}
	// \x1f separation: ("ab","c") must differ from ("a","bc").
	a := store.SyncContact{Name: "ab", Phone: "c"}
	b := store.SyncContact{Name: "a", Phone: "bc"}
	if contactContentHash(a) == contactContentHash(b) {
		t.Fatal("hash ambiguous across field boundaries")
	}
	if contactContentHash(c) == contactContentHash(store.SyncContact{Name: "Ada", Phone: "+234801", Notes: "vip", Source: "crm", ExternalID: "x2"}) {
		t.Fatal("content change did not flip hash")
	}
}
