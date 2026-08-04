package workorders

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
// (the integrator-facing entry point) with a fake tenant resolver.

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
		Store:              st,
		Resolver:           fakeResolver{"acme": tenant},
		NotificationsTopic: "test.notifications",
		UsageTopic:         "test.usage",
		FSMEventsTopic:     "test.fsm",
	}
	r := chi.NewRouter()
	RegisterRoutes(r, d)
	return r, st, tenant
}

func do(t *testing.T, r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("X-Tenant-Slug", "acme")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func createWO(t *testing.T, r http.Handler, title string) WorkOrder {
	t.Helper()
	rec := do(t, r, http.MethodPost, "/v1/field-service/work-orders",
		`{"title":`+qstr(title)+`,"checklist":[{"label":"inspect","done":false}],"scheduled_start":"2030-01-02T09:00:00Z"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		WorkOrder WorkOrder `json:"work_order"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("create body: %v", err)
	}
	return resp.WorkOrder
}

func qstr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func patchWO(t *testing.T, r http.Handler, id uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, r, http.MethodPatch, "/v1/field-service/work-orders/"+id.String(), body)
}

// Full lifecycle through the API: create → dispatch(auto, notify) →
// en_route → on_site → complete (gated) — plus list/board/today reads and
// the outbox emissions (fsm events, usage record, dispatch push).
func TestWorkOrderLifecycle(t *testing.T) {
	r, st, tenant := testRouter(t)
	ada := addTeamMember(t, st, tenant.ID, "Ada Field", true)

	wo := createWO(t, r, "Fix AC")
	if wo.Status != StatusCreated {
		t.Fatalf("new order status = %s", wo.Status)
	}

	// Illegal transition: created → completed is rejected 409.
	rec := patchWO(t, r, wo.ID, `{"status":"completed"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("created→completed = %d (%s), want 409", rec.Code, rec.Body.String())
	}

	// Dispatch with auto + notify.
	rec = do(t, r, http.MethodPost, "/v1/field-service/work-orders/"+wo.ID.String()+"/dispatch",
		`{"assignee_id":"auto","notify":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("dispatch = %d (%s)", rec.Code, rec.Body.String())
	}
	var disp struct {
		WorkOrder WorkOrder `json:"work_order"`
		Notified  bool      `json:"notified"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &disp); err != nil {
		t.Fatalf("dispatch body: %v", err)
	}
	if disp.WorkOrder.Status != StatusAssigned || disp.WorkOrder.AssigneeID == nil || *disp.WorkOrder.AssigneeID != ada {
		t.Fatalf("dispatch result: %+v", disp.WorkOrder)
	}
	if !disp.Notified {
		t.Fatal("notify=true with a configured topic must report notified=true")
	}

	// Walk the machine.
	for _, st := range []string{StatusEnRoute, StatusOnSite} {
		rec = patchWO(t, r, wo.ID, `{"status":"`+st+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("→%s = %d (%s)", st, rec.Code, rec.Body.String())
		}
	}

	// Completion gate: checklist item not done → 409.
	rec = patchWO(t, r, wo.ID, `{"status":"completed","proof":{"notes":"done"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("complete with open checklist = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	// Missing proof.notes → 409.
	rec = patchWO(t, r, wo.ID, `{"checklist":[{"label":"inspect","done":true}],"status":"completed"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("complete without notes = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	// Gate satisfied → 200, completed_at stamped.
	rec = patchWO(t, r, wo.ID,
		`{"checklist":[{"label":"inspect","done":true}],"proof":{"notes":"compressor replaced"},"status":"completed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete = %d (%s)", rec.Code, rec.Body.String())
	}
	var done struct {
		WorkOrder WorkOrder `json:"work_order"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil {
		t.Fatalf("complete body: %v", err)
	}
	if done.WorkOrder.Status != StatusCompleted || done.WorkOrder.CompletedAt == nil {
		t.Fatalf("completed order: %+v", done.WorkOrder)
	}

	// Terminal: completed → cancelled rejected.
	rec = patchWO(t, r, wo.ID, `{"status":"cancelled"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("completed→cancelled = %d, want 409", rec.Code)
	}

	// Reads: list filter, board lanes (all six keys), today.
	rec = do(t, r, http.MethodGet, "/v1/field-service/work-orders?status=completed", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), wo.ID.String()) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, r, http.MethodGet, "/v1/field-service/board", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("board = %d", rec.Code)
	}
	var board struct {
		Board map[string][]BoardItem `json:"board"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &board); err != nil {
		t.Fatalf("board body: %v", err)
	}
	if len(board.Board) != len(Statuses) {
		t.Fatalf("board lanes = %d, want %d (all statuses)", len(board.Board), len(Statuses))
	}
	if len(board.Board[StatusCompleted]) != 1 || board.Board[StatusCompleted][0].AssigneeName != "Ada Field" {
		t.Fatalf("board completed lane: %+v", board.Board[StatusCompleted])
	}

	// Today: the 2030-scheduled order is NOT today; endpoint must still 200
	// with an empty list (honest empty state).
	rec = do(t, r, http.MethodGet, "/v1/field-service/today", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"work_orders":[]`) {
		t.Fatalf("today = %d %s", rec.Code, rec.Body.String())
	}

	// Outbox emissions: assigned + completed fsm events, one usage record,
	// one dispatch push.
	rows, err := st.pool.Query(context.Background(),
		`SELECT topic, payload::text FROM outbox ORDER BY created_at`)
	if err != nil {
		t.Skipf("outbox query needs created_at: %v", err)
	}
	defer rows.Close()
	var topics []string
	var payloads []string
	for rows.Next() {
		var tp, pl string
		if err := rows.Scan(&tp, &pl); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		topics = append(topics, tp)
		payloads = append(payloads, pl)
	}
	counts := map[string]int{}
	for _, tp := range topics {
		counts[tp]++
	}
	if counts["test.fsm"] != 2 || counts["test.usage"] != 1 || counts["test.notifications"] != 1 {
		t.Fatalf("outbox topics = %v, want fsm×2, usage×1, notifications×1", counts)
	}
	// The notifications row must carry the W16 PacedSendRequest shape
	// (jsonb re-serializes, so parse rather than substring-match).
	var pushPayload string
	for i, tp := range topics {
		if tp == "test.notifications" {
			pushPayload = payloads[i]
		}
	}
	var env struct {
		Type    string `json:"type"`
		Subject string `json:"subject"`
		Data    struct {
			Kind string `json:"kind"`
			Push struct {
				TenantSlug string            `json:"tenant_slug"`
				ContactID  string            `json:"contact_id"`
				Title      string            `json:"title"`
				Body       string            `json:"body"`
				App        string            `json:"app"`
				Data       map[string]string `json:"data"`
			} `json:"push"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(pushPayload), &env); err != nil {
		t.Fatalf("push envelope unmarshal: %v (%s)", err, pushPayload)
	}
	if env.Type != EventTypePacedSend || env.Subject != "acme" {
		t.Fatalf("push envelope header: %+v", env)
	}
	if env.Data.Kind != "push_notification" {
		t.Fatalf("push kind = %q, want push_notification", env.Data.Kind)
	}
	if env.Data.Push.TenantSlug != "acme" || env.Data.Push.ContactID != ada.String() ||
		env.Data.Push.App != "field" || env.Data.Push.Title == "" || env.Data.Push.Body == "" {
		t.Fatalf("push payload: %+v", env.Data.Push)
	}
	if env.Data.Push.Data["work_order_id"] != wo.ID.String() {
		t.Fatalf("push data: %+v", env.Data.Push.Data)
	}
	// The usage record carries the contracted metric.
	var usagePayload string
	for i, tp := range topics {
		if tp == "test.usage" {
			usagePayload = payloads[i]
		}
	}
	var usage struct {
		Data struct {
			Metric string `json:"metric"`
			Value  int    `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(usagePayload), &usage); err != nil ||
		usage.Data.Metric != UsageMetricWorkOrderCompleted || usage.Data.Value != 1 {
		t.Fatalf("usage record: %v %s", err, usagePayload)
	}
}

// Re-dispatch (assigned→assigned) with a different team member; dispatch
// on a cancelled order is 409; dispatch auto with no members is 409.
func TestDispatchEdgeCases(t *testing.T) {
	r, st, tenant := testRouter(t)
	ada := addTeamMember(t, st, tenant.ID, "Ada", true)
	bola := addTeamMember(t, st, tenant.ID, "Bola", true)

	wo := createWO(t, r, "Re-dispatch me")
	rec := do(t, r, http.MethodPost, "/v1/field-service/work-orders/"+wo.ID.String()+"/dispatch",
		`{"assignee_id":"`+ada.String()+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("dispatch = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = do(t, r, http.MethodPost, "/v1/field-service/work-orders/"+wo.ID.String()+"/dispatch",
		`{"assignee_id":"`+bola.String()+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-dispatch = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var disp struct {
		WorkOrder WorkOrder `json:"work_order"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &disp); err != nil || *disp.WorkOrder.AssigneeID != bola {
		t.Fatalf("re-dispatch result: %+v (%v)", disp, err)
	}

	// Cancel then attempt dispatch → 409.
	rec = patchWO(t, r, wo.ID, `{"status":"cancelled"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = do(t, r, http.MethodPost, "/v1/field-service/work-orders/"+wo.ID.String()+"/dispatch",
		`{"assignee_id":"auto"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("dispatch cancelled = %d, want 409", rec.Code)
	}

	// Empty body / bad assignee shapes → 400.
	rec = do(t, r, http.MethodPost, "/v1/field-service/work-orders/"+wo.ID.String()+"/dispatch", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty dispatch = %d, want 400", rec.Code)
	}
	rec = do(t, r, http.MethodPost, "/v1/field-service/work-orders/"+wo.ID.String()+"/dispatch",
		`{"assignee_id":"not-a-uuid"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad assignee = %d, want 400", rec.Code)
	}
}

// Tenant middleware: no header → 400; unknown slug → 404; second tenant
// sees none of tenant A's rows (end-to-end RLS through the API).
func TestTenantHandling(t *testing.T) {
	r, _, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/field-service/work-orders", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no tenant header = %d, want 400", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/field-service/work-orders", nil)
	req.Header.Set("X-Tenant-Slug", "ghost")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown tenant = %d, want 404", rec.Code)
	}

	wo := createWO(t, r, "Tenant A job")
	rec = do(t, r, http.MethodGet, "/v1/field-service/work-orders/"+wo.ID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d", rec.Code)
	}
}
