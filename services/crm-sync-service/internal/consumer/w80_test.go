package consumer

// SPEC-W80 WS-A tests: A1 (tenant-scoped person lookups + erase ALL
// per-tenant matches), A2 (reschedule title staleness), A3 (no duplicate
// task after a partial create failure). A1-erase and A2 have redirection
// proofs (FAIL on the pristine base, PASS on the candidate) — these tests
// deliberately use only APIs that already exist on the base.

import (
	"context"
	"encoding/json"
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

// ---------------------------------------------------------------------------
// A2: reschedule must patch title AND dueAt (the title embeds the start time)
// ---------------------------------------------------------------------------

func TestReschedulePatchesTitleAndDueAt(t *testing.T) {
	s, fm, stub := newTestSyncer(t)
	tid := uuid.New()
	bookingID := uuid.NewString()
	if err := fm.Put(context.Background(), KindBooking, bookingID, "task-7", &tid); err != nil {
		t.Fatal(err)
	}
	evt := bookingEvent(events.TypeBookingRescheduled, map[string]any{
		"booking_id":    bookingID,
		"starts_at":     "2026-04-02T15:30:00Z",
		"ends_at":       "2026-04-02T16:00:00Z",
		"status":        "confirmed",
		"offering_name": "Manicure",
	})
	evt.TenantID = tid.String()
	if err := s.HandleBooking(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	var patch *stubReq
	for i, r := range stub.requests {
		if r.method == http.MethodPatch && r.path == "/rest/tasks/task-7" {
			patch = &stub.requests[i]
		}
	}
	if patch == nil {
		t.Fatalf("no PATCH to task-7; requests=%+v", stub.requests)
	}
	if got := patch.body["dueAt"]; got != "2026-04-02T15:30:00Z" {
		t.Errorf("dueAt = %v, want 2026-04-02T15:30:00Z", got)
	}
	// A2 core assertion: the title embeds the start time and MUST be
	// re-rendered — a dueAt-only patch leaves a stale title.
	// (deploy: titles are rendered in the business's local time, CRM_TASK_TIMEZONE, default Africa/Lagos)
	wantTitle := "Manicure \u2014 Thu 2 Apr 2026, 4:30 pm (Lagos)"
	if got := patch.body["title"]; got != wantTitle {
		t.Errorf("title = %v, want %q (stale-title bug)", got, wantTitle)
	}
}

// ---------------------------------------------------------------------------
// A3: task created but person link failed -> mapping persisted -> retry
// patches instead of duplicating
// ---------------------------------------------------------------------------

// flakyTargetStub fails POST /rest/taskTargets with a non-retryable 400 on
// the first call (task itself is created fine), then succeeds.
type flakyTargetStub struct {
	mu           sync.Mutex
	taskCreates  int
	targetCalls  int
	failFirstErr bool
}

func (s *flakyTargetStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		write := func(key string, v any) {
			b, _ := json.Marshal(map[string]any{"data": map[string]any{key: v}})
			_, _ = w.Write(b)
		}
		s.mu.Lock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/people":
			write("people", []map[string]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/rest/people":
			write("createPerson", map[string]any{"id": "person-1"})
		case r.Method == http.MethodPost && r.URL.Path == "/rest/tasks":
			s.taskCreates++
			write("createTask", map[string]any{"id": "task-1"})
		case r.Method == http.MethodPost && r.URL.Path == "/rest/taskTargets":
			s.targetCalls++
			fail := s.failFirstErr
			s.failFirstErr = false
			s.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusBadRequest) // non-retryable: fast test
				_, _ = w.Write([]byte(`{"error":"link boom"}`))
				return
			}
			write("created", map[string]any{"id": "target-1"})
			return
		case r.Method == http.MethodPatch:
			write("updated", map[string]any{"id": "x"})
		default:
			s.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.mu.Unlock()
	})
}

func TestPartialTaskCreateFailureNoDuplicateOnRetry(t *testing.T) {
	stub := &flakyTargetStub{failFirstErr: true}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	fm := newFakeMap()
	s := &Syncer{
		Twenty:  twentyc.New(srv.URL, "k", 0),
		Map:     fm,
		Metrics: metrics.New(),
		Log:     zap.NewNop(),
	}
	tid := uuid.New()
	bookingID := uuid.NewString()
	mkEvt := func() events.CloudEvent {
		evt := bookingEvent(events.TypeBookingCreated, map[string]any{
			"booking_id": bookingID, "starts_at": "2026-03-01T10:00:00Z",
			"ends_at": "2026-03-01T10:30:00Z", "status": "pending",
			"offering_name": "Cut", "contact_name": "Jane Doe",
			"email": "jane@example.com",
		})
		evt.TenantID = tid.String()
		return evt
	}
	// First delivery: task is created in Twenty but the taskTarget link
	// fails -> the handler MUST surface an error (retryable)...
	if err := s.HandleBooking(context.Background(), mkEvt()); err == nil {
		t.Fatal("first delivery must error (link failed)")
	}
	// ...AND the task mapping must already be persisted, so the redelivery
	// takes the patch path instead of creating a second task.
	if _, err := fm.Get(context.Background(), KindBooking, bookingID, &tid); err != nil {
		t.Fatalf("A3: task mapping must be persisted after partial failure: %v", err)
	}
	if err := s.HandleBooking(context.Background(), mkEvt()); err != nil {
		t.Fatalf("redelivery must succeed via the patch path: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.taskCreates != 1 {
		t.Fatalf("A3: tasks created = %d, want 1 (no duplicate after retry)", stub.taskCreates)
	}
}

// ---------------------------------------------------------------------------
// A1: two tenants share an e-mail — notes must land on EACH TENANT'S OWN
// person record (tenant-scoped resolution, no global first-match)
// ---------------------------------------------------------------------------

func TestNoteRoutingTwoTenantsSameEmail(t *testing.T) {
	s, fm, stub := newTestSyncer(t)
	tidA, tidB := uuid.New(), uuid.New()
	if err := fm.Put(context.Background(), syncmap.KindContactIdentity, "email:jane@example.com", "person-A", &tidA); err != nil {
		t.Fatal(err)
	}
	if err := fm.Put(context.Background(), syncmap.KindContactIdentity, "email:jane@example.com", "person-B", &tidB); err != nil {
		t.Fatal(err)
	}
	mkEvt := func(tid uuid.UUID, convID string) events.CloudEvent {
		evt := bookingEvent(events.TypeToolInvoked, map[string]any{
			"conversationId": convID,
			"tool":           "book_appointment",
			"status":         "accepted",
			"detail":         map[string]any{"email": "jane@example.com"},
		})
		evt.TenantID = tid.String()
		return evt
	}
	if err := s.HandleConversation(context.Background(), mkEvt(tidA, "conv-a")); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleConversation(context.Background(), mkEvt(tidB, "conv-b")); err != nil {
		t.Fatal(err)
	}
	var links []string
	for _, r := range stub.requests {
		if r.method == http.MethodPost && r.path == "/rest/noteTargets" {
			pid, _ := r.body["personId"].(string)
			links = append(links, pid)
		}
	}
	if len(links) != 2 || links[0] != "person-A" || links[1] != "person-B" {
		t.Fatalf("A1: note links = %v, want [person-A person-B] (each tenant's own person)", links)
	}
}

// ---------------------------------------------------------------------------
// A1: GDPR erase deletes ALL per-tenant person matches (not just the first)
// ---------------------------------------------------------------------------

// eraseStub records DELETEs; its global people list returns only the FIRST
// match (Twenty's real first-match semantics — the base implementation
// deletes only that one).
type eraseStub struct {
	mu      sync.Mutex
	deleted []string
}

func (s *eraseStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		write := func(key string, v any) {
			b, _ := json.Marshal(map[string]any{"data": map[string]any{key: v}})
			_, _ = w.Write(b)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/people":
			if strings.Contains(r.URL.Query().Get("filter"), "jane@example.com") {
				// First-match global find: BOTH tenants' persons carry the
				// e-mail, but the global listFirst returns only person-A.
				write("people", []map[string]any{{"id": "person-A"}, {"id": "person-B"}})
				return
			}
			write("people", []map[string]any{})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/rest/people/"):
			id := strings.TrimPrefix(r.URL.Path, "/rest/people/")
			s.mu.Lock()
			s.deleted = append(s.deleted, id)
			s.mu.Unlock()
			write("deleted", map[string]any{"id": id})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestGDPREraseDeletesAllTenantMatches(t *testing.T) {
	stub := &eraseStub{}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	fm := newFakeMap()
	s := &Syncer{
		Twenty:  twentyc.New(srv.URL, "k", 0),
		Map:     fm,
		Metrics: metrics.New(),
		Log:     zap.NewNop(),
	}
	tidA, tidB := uuid.New(), uuid.New()
	// Two tenants synced the same e-mail (K21: one person per tenant).
	if err := fm.Put(context.Background(), syncmap.KindContactIdentity, "email:jane@example.com", "person-A", &tidA); err != nil {
		t.Fatal(err)
	}
	if err := fm.Put(context.Background(), syncmap.KindContactIdentity, "email:jane@example.com", "person-B", &tidB); err != nil {
		t.Fatal(err)
	}
	evt := events.CloudEvent{
		SpecVersion: "1.0",
		ID:          uuid.NewString(),
		Source:      "notification-worker",
		Type:        PrivacyEventType,
		Time:        time.Now().UTC(),
		Data:        map[string]any{"email": "jane@example.com", "tenant_id": tidA.String()},
	}
	if err := s.HandlePrivacy(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	deleted := append([]string(nil), stub.deleted...)
	stub.mu.Unlock()
	if len(deleted) != 2 {
		t.Fatalf("A1: erased persons = %v, want BOTH [person-A person-B] (incomplete erasure is a compliance bug)", deleted)
	}
	// Both tenants' sync_map rows for the erased persons are cleaned up.
	if _, err := fm.Get(context.Background(), syncmap.KindContactIdentity, "email:jane@example.com", &tidA); err != syncmap.ErrNotFound {
		t.Errorf("tenant A identity row must be gone: %v", err)
	}
	if _, err := fm.Get(context.Background(), syncmap.KindContactIdentity, "email:jane@example.com", &tidB); err != syncmap.ErrNotFound {
		t.Errorf("tenant B identity row must be gone: %v", err)
	}
}
