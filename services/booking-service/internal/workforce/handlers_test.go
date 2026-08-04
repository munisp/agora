package workforce

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
)

// Handler tests boot the same embedded-Postgres harness as the store tests
// and exercise the real HTTP request/response cycle through RegisterRoutes
// (the integrator-facing entry point) with a fake tenant resolver and a
// fake UserFromContext (the JWT sub the integrator would wire).

type fakeResolver map[string]bookingops.TenantInfo

func (f fakeResolver) BySlug(_ context.Context, slug string) (bookingops.TenantInfo, error) {
	t, ok := f[slug]
	if !ok {
		return bookingops.TenantInfo{}, fmt.Errorf("unknown tenant %q", slug)
	}
	return t, nil
}

func testRouter(t *testing.T) (http.Handler, *Store, bookingops.TenantInfo) {
	t.Helper()
	st := newTestStore(t)
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme", Timezone: "Africa/Lagos"}
	d := &Deps{
		Store:           st,
		Resolver:        fakeResolver{"acme": tenant},
		UserFromContext: func(context.Context) string { return "manager-7" },
		EventsTopic:     "test.workforce.events",
	}
	r := chi.NewRouter()
	RegisterRoutes(r, d)
	return r, st, tenant
}

func do(t *testing.T, r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rd := strings.NewReader(body)
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("X-Tenant-Slug", "acme")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func qstr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func createShift(t *testing.T, r http.Handler, agent uuid.UUID, start, end string) Shift {
	t.Helper()
	rec := do(t, r, http.MethodPost, "/v1/workforce/shifts",
		`{"agent_id":`+qstr(agent.String())+`,"starts_at":`+qstr(start)+`,"ends_at":`+qstr(end)+`,"role":"front desk"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create shift = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Shift Shift `json:"shift"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("create shift body: %v", err)
	}
	return resp.Shift
}

func outboxEvents(t *testing.T, st *Store, topic, eventType string) int {
	t.Helper()
	var n int
	err := st.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM outbox WHERE topic=$1 AND payload->>'type'=$2`, topic, eventType).Scan(&n)
	if err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	return n
}

// Shift lifecycle over HTTP: create (+ assigned event) → overlap 409 WITH
// the conflicting shift id → move 409 → cancel frees the window →
// re-assign emits a second assigned event.
func TestShiftAPI(t *testing.T) {
	r, st, tenant := testRouter(t)
	ada := addAgent(t, st, tenant.ID, "Ada API", true)
	bob := addAgent(t, st, tenant.ID, "Bob API", true)

	first := createShift(t, r, ada, "2030-03-10T09:00:00Z", "2030-03-10T13:00:00Z")
	if first.Status != ShiftScheduled || first.Role != "front desk" {
		t.Fatalf("new shift: %+v", first)
	}
	if outboxEvents(t, st, "test.workforce.events", EventTypeShiftAssigned) != 1 {
		t.Fatalf("shift assigned event not enqueued")
	}

	// Overlapping create → 409 with conflicting_shift_id.
	rec := do(t, r, http.MethodPost, "/v1/workforce/shifts",
		`{"agent_id":`+qstr(ada.String())+`,"starts_at":"2030-03-10T12:00:00Z","ends_at":"2030-03-10T17:00:00Z"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("overlap create = %d (%s)", rec.Code, rec.Body.String())
	}
	var conflict struct {
		Error              string `json:"error"`
		ConflictingShiftID string `json:"conflicting_shift_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &conflict); err != nil {
		t.Fatalf("conflict body: %v", err)
	}
	if conflict.ConflictingShiftID != first.ID.String() {
		t.Fatalf("conflicting_shift_id = %s, want %s", conflict.ConflictingShiftID, first.ID)
	}

	// Legal second shift, then moving it onto the first → 409 too.
	second := createShift(t, r, ada, "2030-03-10T13:00:00Z", "2030-03-10T17:00:00Z")
	rec = do(t, r, http.MethodPatch, "/v1/workforce/shifts/"+second.ID.String(),
		`{"starts_at":"2030-03-10T10:00:00Z","ends_at":"2030-03-10T14:00:00Z"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("overlap move = %d (%s)", rec.Code, rec.Body.String())
	}

	// Status machine: scheduled → confirmed → completed; completed is
	// terminal (completed → scheduled is a 409).
	rec = do(t, r, http.MethodPatch, "/v1/workforce/shifts/"+second.ID.String(), `{"status":"confirmed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = do(t, r, http.MethodPatch, "/v1/workforce/shifts/"+second.ID.String(), `{"status":"scheduled"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("confirmed→scheduled = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}

	// Re-assign the second shift to Bob → fresh assigned event.
	rec = do(t, r, http.MethodPatch, "/v1/workforce/shifts/"+second.ID.String(),
		`{"agent_id":`+qstr(bob.String())+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-assign = %d (%s)", rec.Code, rec.Body.String())
	}
	if outboxEvents(t, st, "test.workforce.events", EventTypeShiftAssigned) != 3 {
		t.Fatalf("assigned events = %d, want 3", outboxEvents(t, st, "test.workforce.events", EventTypeShiftAssigned))
	}

	// Cancel the first shift, then an overlapping create succeeds.
	rec = do(t, r, http.MethodPatch, "/v1/workforce/shifts/"+first.ID.String(), `{"status":"cancelled"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = do(t, r, http.MethodPost, "/v1/workforce/shifts",
		`{"agent_id":`+qstr(ada.String())+`,"starts_at":"2030-03-10T09:30:00Z","ends_at":"2030-03-10T12:00:00Z"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create over cancelled = %d (%s)", rec.Code, rec.Body.String())
	}

	// Unknown agent → 400.
	rec = do(t, r, http.MethodPost, "/v1/workforce/shifts",
		`{"agent_id":`+qstr(uuid.NewString())+`,"starts_at":"2030-03-11T09:00:00Z","ends_at":"2030-03-11T12:00:00Z"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown agent = %d (%s)", rec.Code, rec.Body.String())
	}

	// List filters.
	rec = do(t, r, http.MethodGet, "/v1/workforce/shifts?agent_id="+ada.String()+"&status=cancelled", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d (%s)", rec.Code, rec.Body.String())
	}
	var list struct {
		Shifts []Shift `json:"shifts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list body: %v", err)
	}
	if len(list.Shifts) != 1 || list.Shifts[0].ID != first.ID {
		t.Fatalf("filtered list: %+v", list.Shifts)
	}
}

// Week grid endpoint: stable 7-day shape + agent names.
func TestWeekGridAPI(t *testing.T) {
	r, st, tenant := testRouter(t)
	ada := addAgent(t, st, tenant.ID, "Ada Grid", true)
	createShift(t, r, ada, "2030-03-10T09:00:00Z", "2030-03-10T17:00:00Z")

	rec := do(t, r, http.MethodGet, "/v1/workforce/shifts/week?from=2030-03-10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("week = %d (%s)", rec.Code, rec.Body.String())
	}
	var grid struct {
		WeekStart string      `json:"week_start"`
		Days      []string    `json:"days"`
		Shifts    []ShiftView `json:"shifts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &grid); err != nil {
		t.Fatalf("week body: %v", err)
	}
	if grid.WeekStart != "2030-03-10" || len(grid.Days) != 7 || grid.Days[6] != "2030-03-16" {
		t.Fatalf("grid shape: %+v", grid)
	}
	if len(grid.Shifts) != 1 || grid.Shifts[0].AgentName != "Ada Grid" {
		t.Fatalf("grid shifts: %+v", grid.Shifts)
	}

	// Bad date → 400.
	rec = do(t, r, http.MethodGet, "/v1/workforce/shifts/week?from=not-a-date", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad from = %d", rec.Code)
	}
}

// Clock guards over HTTP: 201 → 409 (open_entry_id) → 200 → 404.
func TestClockAPI(t *testing.T) {
	r, st, tenant := testRouter(t)
	ada := addAgent(t, st, tenant.ID, "Ada Clock", true)

	rec := do(t, r, http.MethodPost, "/v1/workforce/time/clock-in",
		`{"agent_id":`+qstr(ada.String())+`,"method":"field_pwa","gps_lat":6.5244,"gps_lng":3.3792}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("clock-in = %d (%s)", rec.Code, rec.Body.String())
	}
	var inResp struct {
		Entry TimeEntry `json:"time_entry"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &inResp); err != nil {
		t.Fatalf("clock-in body: %v", err)
	}
	if inResp.Entry.Method != MethodFieldPWA || inResp.Entry.GPSLat == nil || inResp.Entry.ClockOutAt != nil {
		t.Fatalf("clock-in entry: %+v", inResp.Entry)
	}

	// Second clock-in → 409 with open_entry_id.
	rec = do(t, r, http.MethodPost, "/v1/workforce/time/clock-in",
		`{"agent_id":`+qstr(ada.String())+`,"method":"web"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second clock-in = %d (%s)", rec.Code, rec.Body.String())
	}
	var openConflict struct {
		OpenEntryID string `json:"open_entry_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &openConflict); err != nil {
		t.Fatalf("409 body: %v", err)
	}
	if openConflict.OpenEntryID != inResp.Entry.ID.String() {
		t.Fatalf("open_entry_id = %s, want %s", openConflict.OpenEntryID, inResp.Entry.ID)
	}

	// GPS half-set → 400; bad method → 400.
	rec = do(t, r, http.MethodPost, "/v1/workforce/time/clock-in",
		`{"agent_id":`+qstr(ada.String())+`,"method":"web","gps_lat":6.5}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("half gps = %d", rec.Code)
	}
	rec = do(t, r, http.MethodPost, "/v1/workforce/time/clock-in",
		`{"agent_id":`+qstr(ada.String())+`,"method":"carrier_pigeon"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad method = %d", rec.Code)
	}

	// Clock-out → 200 with clock_out_at; second → 404.
	rec = do(t, r, http.MethodPost, "/v1/workforce/time/clock-out", `{"agent_id":`+qstr(ada.String())+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clock-out = %d (%s)", rec.Code, rec.Body.String())
	}
	var outResp struct {
		Entry TimeEntry `json:"time_entry"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &outResp); err != nil {
		t.Fatalf("clock-out body: %v", err)
	}
	if outResp.Entry.ClockOutAt == nil {
		t.Fatalf("clock_out_at not stamped: %+v", outResp.Entry)
	}
	rec = do(t, r, http.MethodPost, "/v1/workforce/time/clock-out", `{"agent_id":`+qstr(ada.String())+`}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second clock-out = %d, want 404", rec.Code)
	}

	// Entries list.
	rec = do(t, r, http.MethodGet, "/v1/workforce/time/entries?agent_id="+ada.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("entries = %d", rec.Code)
	}
	var entries struct {
		TimeEntries []TimeEntry `json:"time_entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("entries body: %v", err)
	}
	if len(entries.TimeEntries) != 1 {
		t.Fatalf("entries = %+v", entries.TimeEntries)
	}
}

// Leave flow over HTTP: file → approve (decided_by from the JWT sub via
// UserFromContext) → re-decide 409; leave-decided event enqueued.
func TestLeaveAPI(t *testing.T) {
	r, st, tenant := testRouter(t)
	ada := addAgent(t, st, tenant.ID, "Ada Leave", true)

	rec := do(t, r, http.MethodPost, "/v1/workforce/leave",
		`{"agent_id":`+qstr(ada.String())+`,"kind":"annual","starts_on":"2030-05-04","ends_on":"2030-05-08","reason":"family"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create leave = %d (%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		Leave LeaveRequest `json:"leave_request"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create body: %v", err)
	}
	if created.Leave.Status != LeavePending {
		t.Fatalf("new leave status = %s", created.Leave.Status)
	}

	// ends_on before starts_on → 400.
	rec = do(t, r, http.MethodPost, "/v1/workforce/leave",
		`{"agent_id":`+qstr(ada.String())+`,"kind":"sick","starts_on":"2030-05-08","ends_on":"2030-05-04"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("inverted range = %d", rec.Code)
	}

	// Approve → decided_by is the JWT sub (UserFromContext = "manager-7").
	rec = do(t, r, http.MethodPatch, "/v1/workforce/leave/"+created.Leave.ID.String(), `{"action":"approve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d (%s)", rec.Code, rec.Body.String())
	}
	var decided struct {
		Leave LeaveRequest `json:"leave_request"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decided); err != nil {
		t.Fatalf("approve body: %v", err)
	}
	if decided.Leave.Status != LeaveApproved || decided.Leave.DecidedBy != "manager-7" || decided.Leave.DecidedAt == nil {
		t.Fatalf("decision wrong: %+v", decided.Leave)
	}
	if outboxEvents(t, st, "test.workforce.events", EventTypeLeaveDecided) != 1 {
		t.Fatalf("leave decided event not enqueued")
	}

	// Re-decide → 409; bad action → 400; missing → 404.
	rec = do(t, r, http.MethodPatch, "/v1/workforce/leave/"+created.Leave.ID.String(), `{"action":"decline"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-decide = %d, want 409", rec.Code)
	}
	rec = do(t, r, http.MethodPatch, "/v1/workforce/leave/"+created.Leave.ID.String(), `{"action":"shrug"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad action = %d, want 400", rec.Code)
	}
	rec = do(t, r, http.MethodPatch, "/v1/workforce/leave/"+uuid.NewString(), `{"action":"approve"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing = %d, want 404", rec.Code)
	}

	// Queue filter.
	rec = do(t, r, http.MethodGet, "/v1/workforce/leave?status=pending", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("leave list = %d", rec.Code)
	}
	var queue struct {
		Requests []LeaveRequest `json:"leave_requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &queue); err != nil {
		t.Fatalf("leave list body: %v", err)
	}
	if len(queue.Requests) != 0 {
		t.Fatalf("pending queue not empty: %+v", queue.Requests)
	}
}

// Coverage / utilization / team-members endpoints answer 200 with the
// documented shapes (honest empty states included).
func TestReportingAPI(t *testing.T) {
	r, st, tenant := testRouter(t)
	ada := addAgent(t, st, tenant.ID, "Ada Report", true)
	createShift(t, r, ada, "2030-06-02T09:00:00Z", "2030-06-02T17:00:00Z")

	rec := do(t, r, http.MethodGet, "/v1/workforce/coverage?from=2030-06-02&to=2030-06-04", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("coverage = %d (%s)", rec.Code, rec.Body.String())
	}
	var cov struct {
		Coverage []CoverageDay `json:"coverage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cov); err != nil {
		t.Fatalf("coverage body: %v", err)
	}
	if len(cov.Coverage) != 2 || cov.Coverage[0].AgentsScheduled != 1 || cov.Coverage[1].AgentsScheduled != 0 {
		t.Fatalf("coverage rows: %+v", cov.Coverage)
	}

	rec = do(t, r, http.MethodGet, "/v1/workforce/utilization?from=2030-06-02&to=2030-06-03", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("utilization = %d (%s)", rec.Code, rec.Body.String())
	}
	var util struct {
		Utilization []UtilizationRow `json:"utilization"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &util); err != nil {
		t.Fatalf("utilization body: %v", err)
	}
	if len(util.Utilization) != 1 || util.Utilization[0].ScheduledHours != 8 ||
		util.Utilization[0].UtilizationPct == nil || *util.Utilization[0].UtilizationPct != 0 {
		t.Fatalf("utilization rows: %+v", util.Utilization)
	}

	// Range guard: from after to → 400.
	rec = do(t, r, http.MethodGet, "/v1/workforce/coverage?from=2030-06-04&to=2030-06-02", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("inverted range = %d", rec.Code)
	}

	rec = do(t, r, http.MethodGet, "/v1/workforce/team-members", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("team-members = %d", rec.Code)
	}
	var members struct {
		TeamMembers []TeamMemberView `json:"team_members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &members); err != nil {
		t.Fatalf("team-members body: %v", err)
	}
	if len(members.TeamMembers) != 1 || members.TeamMembers[0].ID != ada {
		t.Fatalf("team members: %+v", members.TeamMembers)
	}
}

// Tenant middleware contract: missing slug → 400, unknown slug → 404.
func TestTenantMiddleware(t *testing.T) {
	r, _, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/workforce/shifts", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no slug = %d, want 400", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/workforce/shifts", nil)
	req.Header.Set("X-Tenant-Slug", "ghost")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown slug = %d, want 404", rec.Code)
	}
}
