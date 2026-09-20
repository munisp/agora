package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/crm-sync-service/internal/events"
	"github.com/opendesk/crm-sync-service/internal/metrics"
	"github.com/opendesk/crm-sync-service/internal/syncmap"
	"github.com/opendesk/crm-sync-service/internal/twentyc"
	"go.uber.org/zap"
)

// fakeMap is an in-memory MapStore.
type fakeMap struct {
	mu   sync.Mutex
	rows map[string]syncmap.Mapping
}

func newFakeMap() *fakeMap { return &fakeMap{rows: map[string]syncmap.Mapping{}} }

func key(kind, odID string, tid *uuid.UUID) string {
	t := "nil"
	if tid != nil {
		t = tid.String()
	}
	return kind + "|" + odID + "|" + t
}

func (f *fakeMap) Get(_ context.Context, kind, odID string, tid *uuid.UUID) (syncmap.Mapping, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.rows[key(kind, odID, tid)]
	if !ok {
		return m, syncmap.ErrNotFound
	}
	return m, nil
}

// GetByTwentyID mirrors syncmap.Store.GetByTwentyID (reverse lookup, latest
// updated_at wins).
func (f *fakeMap) GetByTwentyID(_ context.Context, kind, twentyID string) (syncmap.Mapping, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best syncmap.Mapping
	found := false
	for _, m := range f.rows {
		if m.Kind == kind && m.TwentyID == twentyID && (!found || m.UpdatedAt.After(best.UpdatedAt)) {
			best, found = m, true
		}
	}
	if !found {
		return syncmap.Mapping{}, syncmap.ErrNotFound
	}
	return best, nil
}

func (f *fakeMap) Put(_ context.Context, kind, odID, twentyID string, tid *uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	f.rows[key(kind, odID, tid)] = syncmap.Mapping{
		Kind: kind, OpenDeskID: odID, TwentyID: twentyID, TenantID: tid,
		UpdatedAt: now, LastSyncedAt: &now, // mirrors syncmap.Put's last_synced_at stamp
	}
	return nil
}

// seed injects a mapping row with explicit timestamps (test fixture helper).
func (f *fakeMap) seed(kind, odID, twentyID string, tid *uuid.UUID, updatedAt time.Time, lastSyncedAt *time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[key(kind, odID, tid)] = syncmap.Mapping{
		Kind: kind, OpenDeskID: odID, TwentyID: twentyID, TenantID: tid,
		UpdatedAt: updatedAt, LastSyncedAt: lastSyncedAt,
	}
}

func (f *fakeMap) DeleteByTwentyID(_ context.Context, twentyID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var removed int64
	for k, m := range f.rows {
		if m.TwentyID == twentyID {
			delete(f.rows, k)
			removed++
		}
	}
	return removed, nil
}

// disabled tracks tenant tombstones (SPEC-W45 K9/K21 DisableTenant).
func (f *fakeMap) DisableTenant(_ context.Context, tenantID uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for k, m := range f.rows {
		if m.TenantID != nil && *m.TenantID == tenantID {
			delete(f.rows, k) // fake: disabled rows behave as unmapped
			n++
		}
	}
	return n, nil
}

// twentyStub records requests and answers with Twenty-style envelopes.
type twentyStub struct {
	mu        sync.Mutex
	requests  []stubReq
	personSeq int
	taskSeq   int
}

type stubReq struct {
	method, path, filter string
	body                 map[string]any
}

func (s *twentyStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		s.requests = append(s.requests, stubReq{r.Method, r.URL.Path, r.URL.Query().Get("filter"), body})
		n := len(s.requests)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		write := func(key string, v any) {
			b, _ := json.Marshal(map[string]any{"data": map[string]any{key: v}})
			_, _ = w.Write(b)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/people":
			write("people", []map[string]any{}) // always "not found" -> create path
		case r.Method == http.MethodPost && r.URL.Path == "/rest/people":
			write("createPerson", map[string]any{"id": fmt.Sprintf("person-%d", n)})
		case r.Method == http.MethodPost && r.URL.Path == "/rest/tasks":
			write("createTask", map[string]any{"id": fmt.Sprintf("task-%d", n)})
		case r.Method == http.MethodPost && (r.URL.Path == "/rest/taskTargets" || r.URL.Path == "/rest/noteTargets"):
			write("created", map[string]any{"id": "target-1"})
		case r.Method == http.MethodPost && r.URL.Path == "/rest/notes":
			write("createNote", map[string]any{"id": "note-1"})
		case r.Method == http.MethodPost && r.URL.Path == "/rest/companies":
			write("createCompany", map[string]any{"id": "company-1"})
		case r.Method == http.MethodGet && r.URL.Path == "/rest/companies":
			write("companies", []map[string]any{})
		case r.Method == http.MethodPatch:
			write("updated", map[string]any{"id": "x"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newTestSyncer(t *testing.T) (*Syncer, *fakeMap, *twentyStub) {
	t.Helper()
	stub := &twentyStub{}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	fm := newFakeMap()
	s := &Syncer{
		Twenty:  twentyc.New(srv.URL, "k", 0),
		Map:     fm,
		Metrics: metrics.New(),
		Log:     zap.NewNop(),
	}
	return s, fm, stub
}

func bookingEvent(eventType string, data map[string]any) events.CloudEvent {
	return events.CloudEvent{
		SpecVersion: "1.0",
		ID:          uuid.NewString(),
		Source:      "booking-service",
		Type:        eventType,
		Subject:     "acme-salon",
		Time:        time.Now().UTC(),
		TenantID:    uuid.NewString(),
		Data:        data,
	}
}

func TestHandleBookingCreatedSyncsPersonAndTask(t *testing.T) {
	s, fm, stub := newTestSyncer(t)
	tenantID := uuid.NewString()
	contactID := uuid.NewString()
	bookingID := uuid.NewString()
	evt := bookingEvent(events.TypeBookingCreated, map[string]any{
		"booking_id":     bookingID,
		"starts_at":      "2026-03-01T10:00:00Z",
		"ends_at":        "2026-03-01T10:30:00Z",
		"status":         "pending",
		"source":         "voice",
		"offering_id":    uuid.NewString(),
		"offering_name":  "Haircut",
		"team_member_id": uuid.NewString(),
		"contact_id":     contactID,
		"contact_name":   "Jane Doe",
		"phone":          "+1555000111",
		"email":          "jane@example.com",
		"price_cents":    5000,
		"currency":       "USD",
	})
	evt.TenantID = tenantID

	if err := s.HandleBooking(context.Background(), evt); err != nil {
		t.Fatal(err)
	}

	// Person upsert: GET (miss) + POST /rest/people.
	var sawPersonCreate, sawTaskCreate bool
	for _, r := range stub.requests {
		if r.method == http.MethodPost && r.path == "/rest/people" {
			sawPersonCreate = true
			name, _ := r.body["name"].(map[string]any)
			if name["firstName"] != "Jane" || name["lastName"] != "Doe" {
				t.Errorf("person name = %v", name)
			}
		}
		if r.method == http.MethodPost && r.path == "/rest/tasks" {
			sawTaskCreate = true
			if r.body["title"] != "Haircut appointment at 2026-03-01T10:00:00Z" {
				t.Errorf("task title = %v", r.body["title"])
			}
			if r.body["dueAt"] != "2026-03-01T10:00:00Z" {
				t.Errorf("task dueAt = %v", r.body["dueAt"])
			}
		}
	}
	if !sawPersonCreate || !sawTaskCreate {
		t.Fatalf("person create=%v task create=%v; requests=%+v", sawPersonCreate, sawTaskCreate, stub.requests)
	}

	// sync_map holds contact + booking mappings.
	tid, _ := uuid.Parse(tenantID)
	if _, err := fm.Get(context.Background(), KindContact, contactID, &tid); err != nil {
		t.Errorf("contact mapping missing: %v", err)
	}
	if _, err := fm.Get(context.Background(), KindBooking, bookingID, &tid); err != nil {
		t.Errorf("booking mapping missing: %v", err)
	}
	// booking -> person edge for the /v1/tasks helper.
	bc, err := fm.Get(context.Background(), syncmap.KindBookingContact, bookingID, &tid)
	if err != nil {
		t.Fatalf("booking_contact mapping missing: %v", err)
	}
	if bc.TwentyID == "" {
		t.Error("booking_contact should point at the Twenty person id")
	}
	// It must equal the contact mapping's person id.
	cm, _ := fm.Get(context.Background(), KindContact, contactID, &tid)
	if bc.TwentyID != cm.TwentyID {
		t.Errorf("booking_contact person %q != contact person %q", bc.TwentyID, cm.TwentyID)
	}

	// Reverse-sync contract: kind=booking_task maps the booking to the task
	// (used to resolve task.updated DONE webhooks) ...
	bt, err := fm.Get(context.Background(), KindBookingTask, bookingID, &tid)
	if err != nil {
		t.Fatalf("booking_task mapping missing: %v", err)
	}
	bm, _ := fm.Get(context.Background(), KindBooking, bookingID, &tid)
	if bt.TwentyID != bm.TwentyID {
		t.Errorf("booking_task task %q != booking task %q", bt.TwentyID, bm.TwentyID)
	}
	// ... and the contact mapping is stamped last_synced_at for echo
	// suppression of the inbound webhook our person write triggers.
	if cm.LastSyncedAt == nil || time.Since(*cm.LastSyncedAt) > time.Minute {
		t.Errorf("contact mapping last_synced_at not stamped: %+v", cm)
	}
}

// TestSyncPersonRefreshStampsLastSynced covers the forward update path: when
// a contact mapping already exists, the person PATCH must refresh
// last_synced_at (echo suppression input).
func TestSyncPersonRefreshStampsLastSynced(t *testing.T) {
	s, fm, _ := newTestSyncer(t)
	tid := uuid.New()
	contactID := uuid.NewString()
	old := time.Now().Add(-time.Hour)
	fm.seed(KindContact, contactID, "person-9", &tid, old, &old)

	evt := bookingEvent(events.TypeBookingCreated, map[string]any{
		"booking_id":   uuid.NewString(),
		"starts_at":    "2026-03-01T10:00:00Z",
		"ends_at":      "2026-03-01T10:30:00Z",
		"status":       "pending",
		"source":       "web",
		"offering_id":  uuid.NewString(),
		"contact_id":   contactID,
		"contact_name": "Jane Doe",
		"email":        "jane@example.com",
	})
	evt.TenantID = tid.String()

	if err := s.HandleBooking(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	m, err := fm.Get(context.Background(), KindContact, contactID, &tid)
	if err != nil {
		t.Fatal(err)
	}
	if m.LastSyncedAt == nil || time.Since(*m.LastSyncedAt) > time.Minute {
		t.Errorf("last_synced_at not refreshed on person update path: %+v", m)
	}
}

func TestHandleBookingCancelledClosesTask(t *testing.T) {
	s, fm, stub := newTestSyncer(t)
	tid := uuid.New()
	bookingID := uuid.NewString()
	_ = fm.Put(context.Background(), KindBooking, bookingID, "task-42", &tid)

	evt := bookingEvent(events.TypeBookingCancelled, map[string]any{
		"booking_id":  bookingID,
		"starts_at":   "2026-03-01T10:00:00Z",
		"ends_at":     "2026-03-01T10:30:00Z",
		"status":      "cancelled",
		"source":      "web",
		"offering_id": uuid.NewString(),
		"reason":      "customer sick",
		"phone":       "+1555000111",
		"email":       "",
	})
	evt.TenantID = tid.String()

	if err := s.HandleBooking(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	var sawPatch bool
	for _, r := range stub.requests {
		if r.method == http.MethodPatch && r.path == "/rest/tasks/task-42" {
			sawPatch = true
			if r.body["status"] != "DONE" {
				t.Errorf("patched status = %v", r.body["status"])
			}
		}
	}
	if !sawPatch {
		t.Fatalf("no PATCH to task-42; requests=%+v", stub.requests)
	}
}

func TestHandleBookingCancelledWithoutMappingIsAcked(t *testing.T) {
	s, _, _ := newTestSyncer(t)
	evt := bookingEvent(events.TypeBookingCancelled, map[string]any{
		"booking_id": uuid.NewString(),
		"starts_at":  "2026-03-01T10:00:00Z",
		"reason":     "x",
	})
	if err := s.HandleBooking(context.Background(), evt); err != nil {
		t.Fatalf("expected ack, got %v", err)
	}
}

func TestHandleTenantProvisionedUpsertsCompany(t *testing.T) {
	s, fm, stub := newTestSyncer(t)
	tenantID := uuid.NewString()
	evt := bookingEvent(events.TypeTenantProvisioned, map[string]any{
		"tenant_id": tenantID,
		"slug":      "acme-salon",
		"name":      "Acme Salon",
		"plan":      "free",
	})
	if err := s.HandleIdentity(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	var sawCreate bool
	for _, r := range stub.requests {
		if r.method == http.MethodPost && r.path == "/rest/companies" {
			sawCreate = true
			if r.body["name"] != "Acme Salon" {
				t.Errorf("company name = %v", r.body["name"])
			}
			dn, _ := r.body["domainName"].(map[string]any)
			if dn["primaryLinkUrl"] != "https://acme-salon.opendesk.local" {
				t.Errorf("domainName = %v", dn)
			}
		}
	}
	if !sawCreate {
		t.Fatalf("no company create; requests=%+v", stub.requests)
	}
	tid, _ := uuid.Parse(tenantID)
	m, err := fm.Get(context.Background(), KindTenant, tenantID, &tid)
	if err != nil || m.TwentyID != "company-1" {
		t.Errorf("tenant mapping = %+v, %v", m, err)
	}
}

func TestHandleToolInvokedSkipsWithoutIdentifiers(t *testing.T) {
	s, _, stub := newTestSyncer(t)
	evt := bookingEvent(events.TypeToolInvoked, map[string]any{
		"conversationId": "conv-1",
		"tool":           "book_appointment",
		"status":         "accepted",
		"detail":         map[string]any{"offering_id": "o-1", "starts_at": "2026-03-01T10:00:00Z"},
	})
	if err := s.HandleConversation(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	for _, r := range stub.requests {
		if r.path == "/rest/notes" {
			t.Fatal("note should not be created without contact identifiers")
		}
	}
}

func TestHandleToolInvokedWithPhoneCreatesNote(t *testing.T) {
	s, _, stub := newTestSyncer(t)
	evt := bookingEvent(events.TypeToolInvoked, map[string]any{
		"conversationId": "conv-1",
		"tool":           "book_appointment",
		"status":         "accepted",
		"detail":         map[string]any{"phone": "+1555000111"},
	})
	// Default stub returns empty people list -> FindPerson ErrNotFound -> skip, ack.
	if err := s.HandleConversation(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	for _, r := range stub.requests {
		if r.method == http.MethodGet && r.path == "/rest/people" {
			return // lookup happened; note skipped because person unknown — accepted
		}
	}
	t.Fatal("expected a people lookup")
}

func TestPermanentErrorWrapping(t *testing.T) {
	err := permanent(errors.New("bad payload"))
	if !errors.Is(err, errPermanent) {
		t.Fatal("permanent() should wrap errPermanent")
	}
}

// ---------------------------------------------------------------------------
// SPEC-W45 K21 (OOS-09/OOS-25): person upsert keyed (tenant_id, email|phone)
// ---------------------------------------------------------------------------

// The same email under TWO tenants must create TWO Twenty persons (no global
// cross-tenant merge), while a repeat event for the same tenant reuses the
// tenant's existing person (update, not create).
func TestSyncPersonTenantKeyedNoCrossTenantMerge(t *testing.T) {
	s, fm, stub := newTestSyncer(t)
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	booking := func(id string) map[string]any {
		return map[string]any{
			"booking_id": id, "starts_at": "2026-03-01T10:00:00Z",
			"ends_at": "2026-03-01T10:30:00Z", "status": "pending",
			"offering_name": "Haircut", "contact_name": "Jane Doe",
			"phone": "+1555000111", "email": "jane@example.com",
		}
	}
	evtA := bookingEvent(events.TypeBookingCreated, booking("bk-a1"))
	evtA.TenantID = tenantA
	if err := s.HandleBooking(context.Background(), evtA); err != nil {
		t.Fatal(err)
	}
	// Repeat: same tenant, same email (different booking) -> person REUSED.
	evtA2 := bookingEvent(events.TypeBookingCreated, booking("bk-a2"))
	evtA2.TenantID = tenantA
	if err := s.HandleBooking(context.Background(), evtA2); err != nil {
		t.Fatal(err)
	}
	// Same email, DIFFERENT tenant -> a NEW person must be created.
	evtB := bookingEvent(events.TypeBookingCreated, booking("bk-b1"))
	evtB.TenantID = tenantB
	if err := s.HandleBooking(context.Background(), evtB); err != nil {
		t.Fatal(err)
	}

	var creates, patches int
	var createdIDs []string
	for _, r := range stub.requests {
		if r.method == http.MethodPost && r.path == "/rest/people" {
			creates++
		}
		if r.method == http.MethodPatch && strings.HasPrefix(r.path, "/rest/people/") {
			patches++
		}
	}
	if creates != 2 {
		t.Errorf("person creates = %d, want 2 (one per tenant; no global merge)", creates)
	}
	if patches != 1 {
		t.Errorf("person patches = %d, want 1 (repeat event reuses the tenant's person)", patches)
	}
	// Distinct persons per tenant via the natural-key mappings.
	tidA, _ := uuid.Parse(tenantA)
	tidB, _ := uuid.Parse(tenantB)
	mA, err := fm.Get(context.Background(), syncmap.KindContactIdentity, "email:jane@example.com", &tidA)
	if err != nil {
		t.Fatalf("tenant A identity mapping missing: %v", err)
	}
	mB, err := fm.Get(context.Background(), syncmap.KindContactIdentity, "email:jane@example.com", &tidB)
	if err != nil {
		t.Fatalf("tenant B identity mapping missing: %v", err)
	}
	if mA.TwentyID == mB.TwentyID {
		t.Errorf("cross-tenant merge: both tenants map to person %q", mA.TwentyID)
	}
	createdIDs = append(createdIDs, mA.TwentyID, mB.TwentyID)
	// The phone natural key is mapped too (email|phone per K21).
	if _, err := fm.Get(context.Background(), syncmap.KindContactIdentity, "phone:+1555000111", &tidA); err != nil {
		t.Errorf("tenant A phone identity mapping missing: %v", err)
	}
}

// The second tenant's events must resolve persons only through ITS OWN
// mappings: a phone-only event for tenant A after the email-keyed sync must
// reuse tenant A's person (the phone natural key was recorded).
func TestSyncPersonPhoneKeyReuseWithinTenant(t *testing.T) {
	s, _, stub := newTestSyncer(t)
	tenantA := uuid.NewString()
	evt := bookingEvent(events.TypeBookingCreated, map[string]any{
		"booking_id": "bk-1", "starts_at": "2026-03-01T10:00:00Z",
		"ends_at": "2026-03-01T10:30:00Z", "offering_name": "Cut",
		"contact_name": "Jane Doe", "phone": "+1555000111", "email": "jane@example.com",
	})
	evt.TenantID = tenantA
	if err := s.HandleBooking(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	// Phone-only follow-up event, same tenant.
	evt2 := bookingEvent(events.TypeBookingCreated, map[string]any{
		"booking_id": "bk-2", "starts_at": "2026-03-02T10:00:00Z",
		"ends_at": "2026-03-02T10:30:00Z", "offering_name": "Cut",
		"contact_name": "Jane Doe", "phone": "+1555000111",
	})
	evt2.TenantID = tenantA
	if err := s.HandleBooking(context.Background(), evt2); err != nil {
		t.Fatal(err)
	}
	var creates int
	for _, r := range stub.requests {
		if r.method == http.MethodPost && r.path == "/rest/people" {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("person creates = %d, want 1 (phone key reuses the tenant's person)", creates)
	}
}

// ---------------------------------------------------------------------------
// SPEC-W45 K9: TenantDeleted disables the tenant's sync_map entries
// ---------------------------------------------------------------------------

func TestTenantDeletedDisablesSyncMap(t *testing.T) {
	s, fm, _ := newTestSyncer(t)
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	tidA, _ := uuid.Parse(tenantA)
	tidB, _ := uuid.Parse(tenantB)
	now := time.Now()
	fm.seed(KindBooking, "bk-a", "task-a", &tidA, now, &now)
	fm.seed(syncmap.KindContactIdentity, "email:jane@example.com", "person-a", &tidA, now, &now)
	fm.seed(KindBooking, "bk-b", "task-b", &tidB, now, &now)

	evt := events.CloudEvent{
		SpecVersion: "1.0", ID: uuid.NewString(), Source: "identity-service",
		Type: events.TypeTenantDeleted, Time: now,
		Data: map[string]any{"tenant_id": tenantA, "tenant_slug": "acme",
			"deleted_at": now.Format(time.RFC3339), "actor": "owner@acme"},
	}
	if err := s.HandleIdentity(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	// Tenant A rows are disabled (invisible to Get)...
	if _, err := fm.Get(context.Background(), KindBooking, "bk-a", &tidA); !errors.Is(err, syncmap.ErrNotFound) {
		t.Errorf("deleted tenant mapping must be invisible, got %v", err)
	}
	if _, err := fm.Get(context.Background(), syncmap.KindContactIdentity, "email:jane@example.com", &tidA); !errors.Is(err, syncmap.ErrNotFound) {
		t.Errorf("deleted tenant identity mapping must be invisible, got %v", err)
	}
	// ...tenant B untouched.
	if _, err := fm.Get(context.Background(), KindBooking, "bk-b", &tidB); err != nil {
		t.Errorf("tenant B mapping must survive: %v", err)
	}
	// Idempotent: a redelivery is a no-op success.
	if err := s.HandleIdentity(context.Background(), evt); err != nil {
		t.Errorf("TenantDeleted redelivery must be idempotent: %v", err)
	}
	// Missing tenant_id is a poison payload (permanent, no panic).
	bad := events.CloudEvent{SpecVersion: "1.0", ID: uuid.NewString(),
		Type: events.TypeTenantDeleted, Time: now, Data: map[string]any{"tenant_slug": "x"}}
	if err := s.HandleIdentity(context.Background(), bad); err == nil {
		t.Error("TenantDeleted without tenant_id must error (DLQ)")
	}
}
