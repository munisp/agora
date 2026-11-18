package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/crm-sync-service/internal/events"
	"github.com/opendesk/crm-sync-service/internal/metrics"
	"github.com/opendesk/crm-sync-service/internal/twentyc"
	"go.uber.org/zap"
)

// fakeInvoker records Dapr service invocations (booking-service internal
// endpoints) without a sidecar.
type fakeInvoker struct {
	mu      sync.Mutex
	invokes []invoke
	err     error
}

type invoke struct {
	appID, method string
	headers       map[string]string
	payload       map[string]any
}

func (f *fakeInvoker) InvokeService(_ context.Context, appID, method string, headers map[string]string, payload, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var p map[string]any
	if payload != nil {
		b, _ := json.Marshal(payload)
		_ = json.Unmarshal(b, &p)
	}
	f.invokes = append(f.invokes, invoke{appID, method, headers, p})
	return f.err
}

func (f *fakeInvoker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.invokes)
}

func (f *fakeInvoker) last() invoke {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.invokes[len(f.invokes)-1]
}

// reverseTwentyStub serves the GET-by-id Twenty endpoints the reverse worker
// uses. people/companies/tasks are keyed by id; a nil entry yields an empty
// envelope (-> ErrNotFound). PATCH /rest/tasks/{id} is recorded (SPEC-W80 A9).
type reverseTwentyStub struct {
	people    map[string]map[string]any
	companies map[string]map[string]any
	tasks     map[string]map[string]any

	mu      sync.Mutex
	patches []stubPatch
}

type stubPatch struct {
	path string
	body map[string]any
}

func (s *reverseTwentyStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		write := func(key string, v any) {
			if v == nil {
				// Twenty-style "no record": envelope without a usable record.
				_, _ = w.Write([]byte(`{"data":{}}`))
				return
			}
			b, _ := json.Marshal(map[string]any{"data": map[string]any{key: v}})
			_, _ = w.Write(b)
		}
		var id string
		switch {
		case r.Method == http.MethodGet && len(r.URL.Path) > len("/rest/people/") && r.URL.Path[:len("/rest/people/")] == "/rest/people/":
			id = r.URL.Path[len("/rest/people/"):]
			write("person", s.people[id])
		case r.Method == http.MethodGet && len(r.URL.Path) > len("/rest/companies/") && r.URL.Path[:len("/rest/companies/")] == "/rest/companies/":
			id = r.URL.Path[len("/rest/companies/"):]
			write("company", s.companies[id])
		case r.Method == http.MethodGet && len(r.URL.Path) > len("/rest/tasks/") && r.URL.Path[:len("/rest/tasks/")] == "/rest/tasks/":
			id = r.URL.Path[len("/rest/tasks/"):]
			write("task", s.tasks[id])
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/rest/tasks/"):
			var body map[string]any
			if r.Body != nil {
				_ = json.NewDecoder(r.Body).Decode(&body)
			}
			s.mu.Lock()
			s.patches = append(s.patches, stubPatch{path: r.URL.Path, body: body})
			s.mu.Unlock()
			write("updated", map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/rest/tasks/")})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (s *reverseTwentyStub) taskPatches() []stubPatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubPatch(nil), s.patches...)
}

func newTestReverse(t *testing.T, stub *reverseTwentyStub) (*ReverseSyncer, *fakeMap, *fakeInvoker) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	fm := newFakeMap()
	inv := &fakeInvoker{}
	r := &ReverseSyncer{
		Twenty:       twentyc.New(srv.URL, "k", 0),
		Map:          fm,
		Invoker:      inv,
		BookingAppID: "booking",
		EchoWindow:   DefaultEchoWindow,
		Metrics:      metrics.New(),
		Log:          zap.NewNop(),
	}
	return r, fm, inv
}

func crmEvent(eventType string, record map[string]any) events.CloudEvent {
	return events.CloudEvent{
		SpecVersion: "1.0",
		ID:          uuid.NewString(),
		Source:      "crm-sync-service",
		Type:        eventType,
		Subject:     "twenty",
		Time:        time.Now().UTC(),
		Data: map[string]any{
			"event":  "person.updated",
			"record": record,
		},
	}
}

func personRecord(id, first, last, email, phone, companyID string) map[string]any {
	rec := map[string]any{
		"id":     id,
		"name":   map[string]any{"firstName": first, "lastName": last},
		"emails": map[string]any{"primaryEmail": email},
		"phones": map[string]any{"primaryPhoneNumber": phone},
	}
	if companyID != "" {
		rec["companyId"] = companyID
	}
	return rec
}

func companyRecord(id, domainURL string) map[string]any {
	return map[string]any{
		"id":         id,
		"name":       "Acme",
		"domainName": map[string]any{"primaryLinkUrl": domainURL},
	}
}

// seedTenantChain wires sync_map so tenant uuid -> company-9 -> acme-salon slug.
func seedTenantChain(fm *fakeMap, tid uuid.UUID) {
	old := time.Now().Add(-time.Hour)
	fm.seed(KindTenant, tid.String(), "company-9", &tid, old, &old)
}

func TestReversePersonUpdatedUpsertsContact(t *testing.T) {
	stub := &reverseTwentyStub{
		people:    map[string]map[string]any{"person-1": personRecord("person-1", "Jane", "Doe", "jane@x.com", "+1555", "company-9")},
		companies: map[string]map[string]any{"company-9": companyRecord("company-9", "https://acme-salon.opendesk.local")},
	}
	r, fm, inv := newTestReverse(t, stub)
	tid := uuid.New()
	seedTenantChain(fm, tid)
	// Existing contact mapping, stale (last sync an hour ago -> not an echo).
	old := time.Now().Add(-time.Hour)
	fm.seed(KindContact, uuid.NewString(), "person-1", &tid, old, &old)

	evt := crmEvent(TypeTwentyPersonUpdated, map[string]any{"id": "person-1"})
	if err := r.HandleCRMEvent(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	if inv.count() != 1 {
		t.Fatalf("invokes = %d, want 1", inv.count())
	}
	got := inv.last()
	if got.appID != "booking" || got.method != "internal/contacts/upsert" {
		t.Errorf("invoke = %s/%s", got.appID, got.method)
	}
	if got.headers["X-Tenant-Slug"] != "acme-salon" {
		t.Errorf("tenant slug header = %q", got.headers["X-Tenant-Slug"])
	}
	if got.payload["external_id"] != "person-1" || got.payload["external_source"] != "twenty" {
		t.Errorf("payload external fields = %v", got.payload)
	}
	if got.payload["name"] != "Jane Doe" || got.payload["email"] != "jane@x.com" || got.payload["phone"] != "+1555" {
		t.Errorf("payload = %v", got.payload)
	}
}

func TestReversePersonEchoSuppressed(t *testing.T) {
	stub := &reverseTwentyStub{
		people: map[string]map[string]any{"person-1": personRecord("person-1", "Jane", "Doe", "jane@x.com", "+1555", "")},
	}
	r, fm, inv := newTestReverse(t, stub)
	tid := uuid.New()
	// Forward sync wrote this person just now -> the webhook is our own echo.
	fm.Put(context.Background(), KindContact, uuid.NewString(), "person-1", &tid) //nolint:errcheck

	evt := crmEvent(TypeTwentyPersonUpdated, map[string]any{"id": "person-1"})
	if err := r.HandleCRMEvent(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	if inv.count() != 0 {
		t.Fatalf("echo should be suppressed; invokes = %d", inv.count())
	}
}

func TestReversePersonEchoWindowExpiredProcesses(t *testing.T) {
	stub := &reverseTwentyStub{
		people:    map[string]map[string]any{"person-1": personRecord("person-1", "Jane", "Doe", "jane@x.com", "+1555", "")},
		companies: map[string]map[string]any{"company-9": companyRecord("company-9", "https://acme-salon.opendesk.local")},
	}
	r, fm, inv := newTestReverse(t, stub)
	tid := uuid.New()
	seedTenantChain(fm, tid)
	// Same mapping, but the forward sync ran long ago -> genuine Twenty edit.
	old := time.Now().Add(-time.Minute)
	fm.seed(KindContact, uuid.NewString(), "person-1", &tid, old, &old)

	evt := crmEvent(TypeTwentyPersonUpdated, map[string]any{"id": "person-1"})
	if err := r.HandleCRMEvent(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	if inv.count() != 1 {
		t.Fatalf("stale mapping must not suppress; invokes = %d", inv.count())
	}
}

func TestReversePersonCompanyDomainFallback(t *testing.T) {
	// No contact mapping: tenant resolved via the person's company domain
	// (+ kind=tenant sync_map validation).
	stub := &reverseTwentyStub{
		people:    map[string]map[string]any{"person-7": personRecord("person-7", "Bob", "Ray", "bob@x.com", "", "company-9")},
		companies: map[string]map[string]any{"company-9": companyRecord("company-9", "https://acme-salon.opendesk.local")},
	}
	r, fm, inv := newTestReverse(t, stub)
	seedTenantChain(fm, uuid.New())

	evt := crmEvent(TypeTwentyPersonCreated, map[string]any{"id": "person-7"})
	if err := r.HandleCRMEvent(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	if inv.count() != 1 {
		t.Fatalf("invokes = %d, want 1", inv.count())
	}
	got := inv.last()
	if got.headers["X-Tenant-Slug"] != "acme-salon" {
		t.Errorf("slug = %q", got.headers["X-Tenant-Slug"])
	}
	if got.payload["name"] != "Bob Ray" {
		t.Errorf("payload = %v", got.payload)
	}
}

func TestReversePersonUnresolvableIsAcked(t *testing.T) {
	stub := &reverseTwentyStub{
		// Person without a company and no contact mapping anywhere.
		people: map[string]map[string]any{"person-x": personRecord("person-x", "X", "Y", "x@x.com", "", "")},
	}
	r, _, inv := newTestReverse(t, stub)
	evt := crmEvent(TypeTwentyPersonUpdated, map[string]any{"id": "person-x"})
	if err := r.HandleCRMEvent(context.Background(), evt); err != nil {
		t.Fatalf("unresolvable tenant must be acked, got %v", err)
	}
	if inv.count() != 0 {
		t.Fatalf("no invoke expected; got %d", inv.count())
	}
}

func TestReverseTaskCompletedAppendsCRMNote(t *testing.T) {
	stub := &reverseTwentyStub{
		companies: map[string]map[string]any{"company-9": companyRecord("company-9", "https://acme-salon.opendesk.local")},
	}
	r, fm, inv := newTestReverse(t, stub)
	tid := uuid.New()
	seedTenantChain(fm, tid)
	bookingID := uuid.NewString()
	old := time.Now().Add(-time.Hour)
	fm.seed(KindBookingTask, bookingID, "task-7", &tid, old, &old)

	evt := crmEvent(TypeTwentyTaskUpdated, map[string]any{"id": "task-7", "status": "DONE"})
	if err := r.HandleCRMEvent(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	if inv.count() != 1 {
		t.Fatalf("invokes = %d, want 1", inv.count())
	}
	got := inv.last()
	if got.method != "internal/bookings/"+bookingID+"/crm-note" {
		t.Errorf("method = %q", got.method)
	}
	if got.headers["X-Tenant-Slug"] != "acme-salon" {
		t.Errorf("slug = %q", got.headers["X-Tenant-Slug"])
	}
	if got.payload["text"] == "" || got.payload["source"] != "twenty" {
		t.Errorf("payload = %v", got.payload)
	}
}

func TestReverseTaskNotDoneSkipped(t *testing.T) {
	r, fm, inv := newTestReverse(t, &reverseTwentyStub{})
	tid := uuid.New()
	old := time.Now().Add(-time.Hour)
	fm.seed(KindBookingTask, uuid.NewString(), "task-7", &tid, old, &old)

	evt := crmEvent(TypeTwentyTaskUpdated, map[string]any{"id": "task-7", "status": "IN_PROGRESS"})
	if err := r.HandleCRMEvent(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	if inv.count() != 0 {
		t.Fatalf("non-DONE task must be skipped; invokes = %d", inv.count())
	}
}

func TestReverseTaskDoneWithoutMappingAcked(t *testing.T) {
	r, _, inv := newTestReverse(t, &reverseTwentyStub{})
	evt := crmEvent(TypeTwentyTaskUpdated, map[string]any{"id": "task-foreign", "status": "DONE"})
	if err := r.HandleCRMEvent(context.Background(), evt); err != nil {
		t.Fatalf("unmapped task must be acked, got %v", err)
	}
	if inv.count() != 0 {
		t.Fatalf("no invoke expected; got %d", inv.count())
	}
}

func TestReverseTaskStatusFetchedWhenAbsent(t *testing.T) {
	stub := &reverseTwentyStub{
		tasks:     map[string]map[string]any{"task-7": {"id": "task-7", "status": "DONE"}},
		companies: map[string]map[string]any{"company-9": companyRecord("company-9", "https://acme-salon.opendesk.local")},
	}
	r, fm, inv := newTestReverse(t, stub)
	tid := uuid.New()
	seedTenantChain(fm, tid)
	bookingID := uuid.NewString()
	old := time.Now().Add(-time.Hour)
	fm.seed(KindBookingTask, bookingID, "task-7", &tid, old, &old)

	// Webhook record carries no status -> worker must GET the task.
	evt := crmEvent(TypeTwentyTaskUpdated, map[string]any{"id": "task-7"})
	if err := r.HandleCRMEvent(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	if inv.count() != 1 {
		t.Fatalf("invokes = %d, want 1 (status fetched from Twenty)", inv.count())
	}
}

func TestReverseMissingRecordIDIsPermanent(t *testing.T) {
	r, _, _ := newTestReverse(t, &reverseTwentyStub{})
	evt := crmEvent(TypeTwentyPersonUpdated, map[string]any{})
	err := r.HandleCRMEvent(context.Background(), evt)
	if !errors.Is(err, errPermanent) {
		t.Fatalf("missing record.id err = %v, want permanent", err)
	}
}

// ---------------------------------------------------------------------------
// SPEC-W80 A9: com.opendesk.crm.TicketEscalated -> Twenty task priority HIGH
// ---------------------------------------------------------------------------

func TestTicketEscalatedPatchesTaskPriorityHigh(t *testing.T) {
	stub := &reverseTwentyStub{}
	r, fm, _ := newTestReverse(t, stub)
	tid := uuid.New()
	bookingID := uuid.NewString()
	old := time.Now().Add(-time.Hour)
	fm.seed(KindBooking, bookingID, "task-9", &tid, old, &old)

	evt := crmEvent(TypeTicketEscalated, nil)
	evt.Data = map[string]any{
		"tenant_slug": "acme-salon",
		"tenant_id":   tid.String(),
		"ticket_id":   bookingID,
		"priority":    "high",
		"reason":      "first_response_sla_breach",
		"sla":         "4h0m0s",
	}
	if err := r.HandleCRMEvent(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	patches := stub.taskPatches()
	if len(patches) != 1 {
		t.Fatalf("task PATCHes = %d, want 1", len(patches))
	}
	if patches[0].path != "/rest/tasks/task-9" {
		t.Errorf("PATCH path = %q, want /rest/tasks/task-9", patches[0].path)
	}
	if patches[0].body["priority"] != "HIGH" {
		t.Errorf("priority = %v, want HIGH", patches[0].body["priority"])
	}
}

// No booking mapping -> graceful no-op (debug log), never an error/DLQ.
func TestTicketEscalatedWithoutMappingIsAcked(t *testing.T) {
	stub := &reverseTwentyStub{}
	r, _, _ := newTestReverse(t, stub)
	evt := crmEvent(TypeTicketEscalated, nil)
	evt.Data = map[string]any{
		"tenant_id": uuid.NewString(),
		"ticket_id": uuid.NewString(),
		"priority":  "high",
	}
	if err := r.HandleCRMEvent(context.Background(), evt); err != nil {
		t.Fatalf("unmapped escalation must be acked, got %v", err)
	}
	if n := len(stub.taskPatches()); n != 0 {
		t.Fatalf("no PATCH expected without a mapping; got %d", n)
	}
}

// Missing ticket_id is a poison payload (permanent, straight to DLQ).
func TestTicketEscalatedMissingTicketIDIsPermanent(t *testing.T) {
	r, _, _ := newTestReverse(t, &reverseTwentyStub{})
	evt := crmEvent(TypeTicketEscalated, nil)
	evt.Data = map[string]any{"tenant_id": uuid.NewString()}
	if err := r.HandleCRMEvent(context.Background(), evt); !errors.Is(err, errPermanent) {
		t.Fatalf("missing ticket_id err = %v, want permanent", err)
	}
}

// ---------------------------------------------------------------------------
// SPEC-W80 A11 (B7 caller side): reverse invokes carry X-Internal-Token
// ---------------------------------------------------------------------------

func TestReverseInvokesCarryInternalToken(t *testing.T) {
	stub := &reverseTwentyStub{
		people:    map[string]map[string]any{"person-1": personRecord("person-1", "Jane", "Doe", "jane@x.com", "+1555", "company-9")},
		companies: map[string]map[string]any{"company-9": companyRecord("company-9", "https://acme-salon.opendesk.local")},
	}
	r, fm, inv := newTestReverse(t, stub)
	r.BookingInternalToken = "test-booking-internal-token"
	seedTenantChain(fm, uuid.New())

	// contacts/upsert path.
	if err := r.HandleCRMEvent(context.Background(), crmEvent(TypeTwentyPersonUpdated, map[string]any{"id": "person-1"})); err != nil {
		t.Fatal(err)
	}
	// crm-note path.
	tid := uuid.New()
	seedTenantChain(fm, tid)
	bookingID := uuid.NewString()
	old := time.Now().Add(-time.Hour)
	fm.seed(KindBookingTask, bookingID, "task-7", &tid, old, &old)
	if err := r.HandleCRMEvent(context.Background(), crmEvent(TypeTwentyTaskUpdated, map[string]any{"id": "task-7", "status": "DONE"})); err != nil {
		t.Fatal(err)
	}
	if inv.count() != 2 {
		t.Fatalf("invokes = %d, want 2", inv.count())
	}
	for _, call := range inv.invokes {
		if got := call.headers["X-Internal-Token"]; got != "test-booking-internal-token" {
			t.Errorf("A11: %s missing X-Internal-Token, got %q (booking internAuth fails closed)", call.method, got)
		}
	}
}

// Unconfigured token => header omitted (the peer then fails closed; main
// logs the misconfig at boot). Guards against sending an EMPTY header.
func TestReverseInvokesOmitTokenHeaderWhenUnset(t *testing.T) {
	stub := &reverseTwentyStub{
		people:    map[string]map[string]any{"person-1": personRecord("person-1", "Jane", "Doe", "jane@x.com", "+1555", "company-9")},
		companies: map[string]map[string]any{"company-9": companyRecord("company-9", "https://acme-salon.opendesk.local")},
	}
	r, fm, inv := newTestReverse(t, stub)
	seedTenantChain(fm, uuid.New())
	if err := r.HandleCRMEvent(context.Background(), crmEvent(TypeTwentyPersonUpdated, map[string]any{"id": "person-1"})); err != nil {
		t.Fatal(err)
	}
	if inv.count() != 1 {
		t.Fatalf("invokes = %d, want 1", inv.count())
	}
	if _, present := inv.last().headers["X-Internal-Token"]; present {
		t.Errorf("unset token must OMIT the header, got %q", inv.last().headers["X-Internal-Token"])
	}
}
