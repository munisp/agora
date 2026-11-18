package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/opendesk/crm-sync-service/internal/metrics"
	"github.com/opendesk/crm-sync-service/internal/syncmap"
	"github.com/opendesk/crm-sync-service/internal/twentyc"
	"go.uber.org/zap"
)

// fakeMap is an in-memory MapReader.
type fakeMap struct {
	mu   sync.Mutex
	rows map[string]string
}

func (f *fakeMap) put(kind, odID string, tid *uuid.UUID, twentyID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := "nil"
	if tid != nil {
		t = tid.String()
	}
	f.rows[kind+"|"+odID+"|"+t] = twentyID
}

func (f *fakeMap) Get(_ context.Context, kind, odID string, tid *uuid.UUID) (syncmap.Mapping, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := "nil"
	if tid != nil {
		t = tid.String()
	}
	if id, ok := f.rows[kind+"|"+odID+"|"+t]; ok {
		return syncmap.Mapping{Kind: kind, OpenDeskID: odID, TwentyID: id}, nil
	}
	return syncmap.Mapping{}, syncmap.ErrNotFound
}

// ListByOpendeskID mirrors the store's cross-tenant enumeration (SPEC-W80
// A1), deterministic by key (first-synced ordering is the store's concern).
func (f *fakeMap) ListByOpendeskID(_ context.Context, kind, odID string) ([]syncmap.Mapping, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for k := range f.rows {
		if strings.HasPrefix(k, kind+"|"+odID+"|") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]syncmap.Mapping, 0, len(keys))
	for _, k := range keys {
		out = append(out, syncmap.Mapping{Kind: kind, OpenDeskID: odID, TwentyID: f.rows[k]})
	}
	return out, nil
}

// twentyRecorder answers like Twenty and records calls.
type twentyRecorder struct {
	mu       sync.Mutex
	requests []string
	people   []map[string]any // returned for GET /rest/people
}

func (tr *twentyRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.mu.Lock()
		tr.requests = append(tr.requests, r.Method+" "+r.URL.Path)
		tr.mu.Unlock()
		write := func(key string, v any) {
			b, _ := json.Marshal(map[string]any{"data": map[string]any{key: v}})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/people":
			write("people", tr.people)
		case r.Method == http.MethodPost && r.URL.Path == "/rest/tasks":
			write("createTask", map[string]any{"id": "task-1"})
		case r.Method == http.MethodPost && r.URL.Path == "/rest/taskTargets":
			write("created", map[string]any{"id": "tt-1"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (tr *twentyRecorder) called(sub string) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, r := range tr.requests {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

// testInternalToken is the harness X-Internal-Token (SPEC-W45 K21 gate).
const testInternalToken = "test-crm-sync-internal-token"

func newTestServer(t *testing.T) (*Server, *fakeMap, *twentyRecorder) {
	t.Helper()
	rec := &twentyRecorder{}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	fm := &fakeMap{rows: map[string]string{}}
	return &Server{
		Twenty:        twentyc.New(srv.URL, "k", 0),
		Map:           fm,
		Metrics:       metrics.New(),
		InternalToken: testInternalToken,
		Log:           zap.NewNop(),
	}, fm, rec
}

func postTasks(t *testing.T, s *Server, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(body))
	req.Header.Set("X-Internal-Token", testInternalToken)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestCreateTaskStaffAlertUnlinked(t *testing.T) {
	s, _, rec := newTestServer(t)
	code, out := postTasks(t, s, `{
		"tenant_slug":"acme-clinic","tenant_id":"`+uuid.NewString()+`",
		"kind":"staff_alert","title":"Intake form incomplete — visit b-1",
		"body":"Patient has not completed intake.","booking_id":"b-1",
		"due_at":"2026-03-01T10:00:00Z"}`)
	if code != http.StatusCreated {
		t.Fatalf("code = %d, out = %v", code, out)
	}
	if out["linked"] != false {
		t.Fatalf("linked = %v, want false", out)
	}
	if out["taskId"] != "task-1" {
		t.Fatalf("taskId = %v", out)
	}
	if rec.called("/rest/taskTargets") {
		t.Fatal("unlinked task must not create a taskTarget")
	}
	if !rec.called("POST /rest/tasks") {
		t.Fatal("task not created")
	}
}

func TestCreateTaskFollowUpResolvesPersonViaBooking(t *testing.T) {
	s, fm, rec := newTestServer(t)
	tid := uuid.New()
	fm.put(syncmap.KindBookingContact, "b-9", &tid, "person-77")
	code, out := postTasks(t, s, `{
		"tenant_slug":"acme-consult","tenant_id":"`+tid.String()+`",
		"kind":"follow_up","title":"Follow up with Jane","body":"Send proposal.",
		"booking_id":"b-9","due_at":"2026-03-08T10:00:00Z"}`)
	if code != http.StatusCreated {
		t.Fatalf("code = %d, out = %v", code, out)
	}
	if out["personId"] != "person-77" || out["linked"] != true {
		t.Fatalf("out = %v, want linked to person-77", out)
	}
	if !rec.called("/rest/taskTargets") {
		t.Fatal("linked task should create a taskTarget")
	}
}

func TestCreateTaskFollowUpDegradesToUnlinked(t *testing.T) {
	s, _, rec := newTestServer(t)
	rec.people = []map[string]any{} // no person anywhere
	code, out := postTasks(t, s, `{
		"tenant_slug":"acme-consult","tenant_id":"`+uuid.NewString()+`",
		"kind":"follow_up","title":"Follow up","booking_id":"b-unknown",
		"phone":"+1555","due_at":"2026-03-08T10:00:00Z"}`)
	if code != http.StatusCreated {
		t.Fatalf("follow_up must degrade, not 4xx: code = %d, out = %v", code, out)
	}
	if out["linked"] != false {
		t.Fatalf("linked = %v", out)
	}
}

func TestCreateTaskCanonicalShapeStillWorks(t *testing.T) {
	s, _, rec := newTestServer(t)
	rec.people = []map[string]any{{"id": "person-1"}}
	code, out := postTasks(t, s, `{"email":"jane@example.com","title":"Call Jane","dueAt":"2026-03-02T09:00:00Z"}`)
	if code != http.StatusCreated || out["personId"] != "person-1" {
		t.Fatalf("code = %d out = %v", code, out)
	}
	if !rec.called("/rest/taskTargets") {
		t.Fatal("expected taskTarget link")
	}
}

func TestCreateTaskValidations(t *testing.T) {
	s, _, _ := newTestServer(t)
	if code, _ := postTasks(t, s, `{"body":"no title"}`); code != http.StatusBadRequest {
		t.Fatalf("missing title: code = %d", code)
	}
	if code, _ := postTasks(t, s, `{"title":"x","due_at":"not-a-date"}`); code != http.StatusBadRequest {
		t.Fatalf("bad due_at: code = %d", code)
	}
	if code, _ := postTasks(t, s, `{invalid`); code != http.StatusBadRequest {
		t.Fatalf("bad json: code = %d", code)
	}
}

func TestResolveDueAt(t *testing.T) {
	cases := []struct{ dueAt, snake, want string }{
		{"2026-03-01T10:00:00Z", "", "2026-03-01T10:00:00Z"},
		{"", "2026-03-01T10:00:00Z", "2026-03-01T10:00:00Z"},
		{"2026-03-01T11:00:00Z", "2026-03-01T10:00:00Z", "2026-03-01T11:00:00Z"}, // dueAt wins
		{"", "", ""},
	}
	for _, c := range cases {
		got, err := resolveDueAt(c.dueAt, c.snake)
		if err != nil {
			t.Fatalf("resolveDueAt(%q,%q): %v", c.dueAt, c.snake, err)
		}
		if got != c.want {
			t.Errorf("resolveDueAt(%q,%q) = %q, want %q", c.dueAt, c.snake, got, c.want)
		}
	}
	if _, err := resolveDueAt("", "tomorrow"); err == nil {
		t.Fatal("expected error for non-RFC3339 due_at")
	}
}

// ---------------------------------------------------------------------------
// SPEC-W45 K21 (OOS-09): X-Internal-Token gate on /v1/tasks + /v1/people/lookup
// ---------------------------------------------------------------------------

func TestInternalTokenMatrix(t *testing.T) {
	s, _, rec := newTestServer(t)
	rec.people = []map[string]any{{"id": "person-1"}}
	router := s.Router()
	do := func(method, path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(`{"title":"x"}`))
		if token != "" {
			req.Header.Set("X-Internal-Token", token)
		}
		r := httptest.NewRecorder()
		router.ServeHTTP(r, req)
		return r
	}
	for _, path := range []string{"/v1/tasks", "/v1/people/lookup?email=jane@example.com"} {
		method := http.MethodPost
		if strings.HasPrefix(path, "/v1/people") {
			method = http.MethodGet
		}
		if r := do(method, path, ""); r.Code != http.StatusUnauthorized {
			t.Errorf("%s missing token: code = %d, want 401", path, r.Code)
		}
		if r := do(method, path, "wrong"); r.Code != http.StatusUnauthorized {
			t.Errorf("%s wrong token: code = %d, want 401", path, r.Code)
		}
		if r := do(method, path, testInternalToken); r.Code == http.StatusUnauthorized || r.Code == http.StatusServiceUnavailable {
			t.Errorf("%s correct token must pass: code = %d", path, r.Code)
		}
	}
}

func TestInternalTokenUnsetFailClosed503(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.InternalToken = "" // misconfigured deployment
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(`{"title":"x"}`))
	req.Header.Set("X-Internal-Token", testInternalToken)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unset server token: code = %d, want 503 (fail-closed)", rec.Code)
	}
}

// The webhook intake and probes are NOT behind the internal token (Twenty
// calls the webhook path; it has its own HMAC gate).
func TestGatedSurfaceDoesNotSwallowWebhookRoute(t *testing.T) {
	s, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/twenty", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	// 401 from the HMAC verifier (no signature), NOT the internal-token gate.
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("webhook without signature: code = %d, want 401 (HMAC)", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("healthz: code = %d, want 200", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// SPEC-W80 A1: /v1/people/lookup resolves via tenant-scoped sync_map keys
// ---------------------------------------------------------------------------

func getLookup(t *testing.T, s *Server, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Internal-Token", testInternalToken)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestPeopleLookupTenantScoped(t *testing.T) {
	s, fm, _ := newTestServer(t)
	tidA, tidB := uuid.New(), uuid.New()
	// Two tenants share the e-mail (K21: one person per tenant).
	fm.put(syncmap.KindContactIdentity, "email:jane@example.com", &tidA, "person-A")
	fm.put(syncmap.KindContactIdentity, "email:jane@example.com", &tidB, "person-B")

	code, out := getLookup(t, s, "/v1/people/lookup?email=jane@example.com&tenant_id="+tidA.String())
	if code != http.StatusOK {
		t.Fatalf("code = %d, out = %v", code, out)
	}
	if p, _ := out["person"].(map[string]any); p["id"] != "person-A" {
		t.Errorf("tenant A lookup = %v, want person-A", out["person"])
	}
	code, out = getLookup(t, s, "/v1/people/lookup?email=jane@example.com&tenant_id="+tidB.String())
	if code != http.StatusOK {
		t.Fatalf("code = %d, out = %v", code, out)
	}
	if p, _ := out["person"].(map[string]any); p["id"] != "person-B" {
		t.Errorf("tenant B lookup = %v, want person-B (tenant scoping)", out["person"])
	}
	// Unknown tenant for the key -> person null.
	code, out = getLookup(t, s, "/v1/people/lookup?email=jane@example.com&tenant_id="+uuid.NewString())
	if code != http.StatusOK || out["person"] != nil {
		t.Errorf("unknown tenant: code = %d person = %v, want 200/null", code, out["person"])
	}
	// Malformed tenant_id -> 400.
	if code, _ := getLookup(t, s, "/v1/people/lookup?email=jane@example.com&tenant_id=nope"); code != http.StatusBadRequest {
		t.Errorf("bad tenant_id: code = %d, want 400", code)
	}
}

// Legacy unscoped contract (GDPR collector): no tenant_id -> the natural
// key is enumerated across tenants via sync_map (NEVER the global Twenty
// find) and a mapped person is returned.
func TestPeopleLookupUnscopedEnumeratesSyncMap(t *testing.T) {
	s, fm, rec := newTestServer(t)
	tidA, tidB := uuid.New(), uuid.New()
	fm.put(syncmap.KindContactIdentity, "email:jane@example.com", &tidA, "person-A")
	fm.put(syncmap.KindContactIdentity, "email:jane@example.com", &tidB, "person-B")
	code, out := getLookup(t, s, "/v1/people/lookup?email=jane@example.com")
	if code != http.StatusOK {
		t.Fatalf("code = %d, out = %v", code, out)
	}
	p, _ := out["person"].(map[string]any)
	if p["id"] != "person-A" && p["id"] != "person-B" {
		t.Errorf("unscoped lookup = %v, want one of the mapped persons", out["person"])
	}
	if rec.called("GET /rest/people") {
		t.Error("A1: no global Twenty people find may happen anymore")
	}
	// Phone path via contact_phone.
	fm.put(syncmap.KindContactPhone, "+1555", &tidA, "person-C")
	code, out = getLookup(t, s, "/v1/people/lookup?phone=%2B1555")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if p, _ := out["person"].(map[string]any); p["id"] != "person-C" {
		t.Errorf("phone lookup = %v, want person-C", out["person"])
	}
	// Nothing mapped -> person null.
	code, out = getLookup(t, s, "/v1/people/lookup?email=nobody@example.com")
	if code != http.StatusOK || out["person"] != nil {
		t.Errorf("unmapped: code = %d person = %v, want 200/null", code, out["person"])
	}
}

// /v1/tasks phone/email resolution honors the tenant scope: with a
// tenant_id present, an unmapped contact degrades to unlinked even when a
// GLOBAL Twenty record exists (no cross-tenant task linking).
func TestCreateTaskTenantScopedResolutionSkipsGlobalFind(t *testing.T) {
	s, _, rec := newTestServer(t)
	rec.people = []map[string]any{{"id": "person-global"}} // exists globally, but not in THIS tenant's map
	code, out := postTasks(t, s, `{
		"tenant_slug":"acme-consult","tenant_id":"`+uuid.NewString()+`",
		"kind":"follow_up","title":"Follow up","email":"jane@example.com",
		"due_at":"2026-03-08T10:00:00Z"}`)
	if code != http.StatusCreated {
		t.Fatalf("code = %d, out = %v", code, out)
	}
	if out["linked"] != false || out["personId"] != "" {
		t.Errorf("out = %v, want unlinked (tenant-scoped resolution must not leak the global record)", out)
	}
}
