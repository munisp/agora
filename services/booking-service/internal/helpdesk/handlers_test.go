package helpdesk

import (
	"context"
	"encoding/json"
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
// (the anti-collision contract entry point) with a test tenant accessor —
// httpapi wiring itself is the integrator's gate.

// testRouter mounts RegisterRoutes exactly as the integrator will (tenant +
// user accessors reading request-scoped values, no authz mw — the perms
// chain is httpapi's), with events + metering topics enabled so the outbox
// emission is observable.
func testRouter(t *testing.T, tenant bookingops.TenantInfo) (http.Handler, *Store) {
	t.Helper()
	st := newTestStore(t)
	r := chi.NewRouter()
	RegisterRoutes(r, &Deps{
		Store: st,
		TenantFromContext: func(ctx context.Context) (bookingops.TenantInfo, bool) {
			return tenant, true
		},
		UserFromContext: func(ctx context.Context) string { return "agent-1" },
		EventsTopic:     "opendesk.helpdesk.events.v1",
		UsageTopic:      "opendesk.usage.events",
	})
	return r, st
}

func do(t *testing.T, r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.ServeHTTP(rec, req)
	return rec
}

func decodeEnv[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return v
}

// Full ticket journey over HTTP: policy → create (auto-attach + dues) →
// note (first response) → auto-assign → resolve (metering + event) → CSAT.
func TestTicketJourneyEndpoints(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, st := testRouter(t, tenant)
	member := addMember(t, st, tenant.ID, "Helpful Hank", true)

	// SLA policy for the high tier.
	rec := do(t, r, http.MethodPost, "/v1/helpdesk/sla-policies",
		`{"name":"High tier","priority":"high","first_response_minutes":15,"resolve_minutes":240}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create policy = %d (%s)", rec.Code, rec.Body.String())
	}
	pol := decodeEnv[struct {
		Policy SLAPolicy `json:"policy"`
	}](t, rec)

	// Create a high-priority ticket → policy auto-attached, dues computed.
	rec = do(t, r, http.MethodPost, "/v1/helpdesk/tickets",
		`{"subject":"POS terminal offline","channel":"voice","priority":"high"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create ticket = %d (%s)", rec.Code, rec.Body.String())
	}
	created := decodeEnv[struct {
		Ticket Ticket `json:"ticket"`
	}](t, rec)
	tk := created.Ticket
	if tk.SLAPolicyID == nil || *tk.SLAPolicyID != pol.Policy.ID {
		t.Fatalf("policy not auto-attached: %+v", tk)
	}
	if tk.DueFirstResponseAt == nil || tk.DueResolveAt == nil || tk.Status != StatusOpen {
		t.Fatalf("dues/status wrong: %+v", tk)
	}

	// Detail includes the created timeline event.
	rec = do(t, r, http.MethodGet, "/v1/helpdesk/tickets/"+tk.ID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get ticket = %d", rec.Code)
	}
	detail := decodeEnv[struct {
		Ticket Ticket        `json:"ticket"`
		Events []TicketEvent `json:"events"`
	}](t, rec)
	if len(detail.Events) != 1 || detail.Events[0].Kind != EventCreated || detail.Events[0].Actor != "agent-1" {
		t.Fatalf("timeline: %+v", detail.Events)
	}

	// Note → first_response_at stamped.
	rec = do(t, r, http.MethodPatch, "/v1/helpdesk/tickets/"+tk.ID.String(),
		`{"note":"Technician dispatched"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("note = %d (%s)", rec.Code, rec.Body.String())
	}
	noted := decodeEnv[struct {
		Ticket Ticket `json:"ticket"`
	}](t, rec)
	if noted.Ticket.FirstResponseAt == nil {
		t.Fatalf("first_response_at not stamped: %+v", noted.Ticket)
	}

	// Auto-assign → the only active member.
	rec = do(t, r, http.MethodPatch, "/v1/helpdesk/tickets/"+tk.ID.String(),
		`{"assignee_id":"auto"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("auto assign = %d (%s)", rec.Code, rec.Body.String())
	}
	assigned := decodeEnv[struct {
		Ticket Ticket `json:"ticket"`
	}](t, rec)
	if assigned.Ticket.AssigneeID == nil || *assigned.Ticket.AssigneeID != member {
		t.Fatalf("auto assign picked %+v, want %s", assigned.Ticket.AssigneeID, member)
	}

	// Resolve → resolved_at; metering + lifecycle event hit the outbox.
	rec = do(t, r, http.MethodPatch, "/v1/helpdesk/tickets/"+tk.ID.String(),
		`{"status":"resolved"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve = %d (%s)", rec.Code, rec.Body.String())
	}
	resolved := decodeEnv[struct {
		Ticket Ticket `json:"ticket"`
	}](t, rec)
	if resolved.Ticket.ResolvedAt == nil || resolved.Ticket.Status != StatusResolved {
		t.Fatalf("resolution: %+v", resolved.Ticket)
	}

	var metered, evtCreated, evtResolved int
	if err := st.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FILTER (WHERE topic='opendesk.usage.events'
		            AND payload->'data'->>'metric'='ticket_resolved'),
		        COUNT(*) FILTER (WHERE topic='opendesk.helpdesk.events.v1'
		            AND payload->'data'->>'event_name'='ticket_created'),
		        COUNT(*) FILTER (WHERE topic='opendesk.helpdesk.events.v1'
		            AND payload->'data'->>'event_name'='ticket_resolved')
		 FROM outbox`).Scan(&metered, &evtCreated, &evtResolved); err != nil {
		t.Fatalf("outbox query: %v", err)
	}
	if metered != 1 || evtCreated != 1 || evtResolved != 1 {
		t.Fatalf("outbox rows: metered=%d created=%d resolved=%d (want 1/1/1)", metered, evtCreated, evtResolved)
	}

	// CSAT after resolution.
	rec = do(t, r, http.MethodPatch, "/v1/helpdesk/tickets/"+tk.ID.String()+"/csat",
		`{"rating":5,"comment":"fast fix"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("csat = %d (%s)", rec.Code, rec.Body.String())
	}
	rated := decodeEnv[struct {
		Ticket Ticket `json:"ticket"`
	}](t, rec)
	if rated.Ticket.CSATRating == nil || *rated.Ticket.CSATRating != 5 {
		t.Fatalf("csat: %+v", rated.Ticket)
	}

	// Stats + list + team members endpoints answer.
	rec = do(t, r, http.MethodGet, "/v1/helpdesk/stats", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats = %d", rec.Code)
	}
	stats := decodeEnv[struct {
		Stats Stats `json:"stats"`
	}](t, rec)
	if stats.Stats.Resolved30d != 1 || stats.Stats.OpenCount != 0 {
		t.Fatalf("stats: %+v", stats.Stats)
	}

	rec = do(t, r, http.MethodGet, "/v1/helpdesk/tickets?status=resolved&q=POS", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	list := decodeEnv[struct {
		Tickets []Ticket `json:"tickets"`
	}](t, rec)
	if len(list.Tickets) != 1 {
		t.Fatalf("filtered list: %+v", list.Tickets)
	}

	rec = do(t, r, http.MethodGet, "/v1/helpdesk/team-members", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("team members = %d", rec.Code)
	}
	members := decodeEnv[struct {
		Members []TeamMemberView `json:"team_members"`
	}](t, rec)
	if len(members.Members) != 1 || members.Members[0].ID != member {
		t.Fatalf("team members: %+v", members.Members)
	}
}

// Validation + error mapping over HTTP.
func TestEndpointValidation(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, _ := testRouter(t, tenant)
	someID := uuid.NewString()

	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"missing subject", http.MethodPost, "/v1/helpdesk/tickets", `{"priority":"high"}`, 400},
		{"bad priority", http.MethodPost, "/v1/helpdesk/tickets", `{"subject":"x","priority":"p1"}`, 400},
		{"bad json", http.MethodPost, "/v1/helpdesk/tickets", `{`, 400},
		{"bad status filter", http.MethodGet, "/v1/helpdesk/tickets?status=weird", "", 400},
		{"bad assignee filter", http.MethodGet, "/v1/helpdesk/tickets?assignee_id=zzz", "", 400},
		{"missing ticket", http.MethodGet, "/v1/helpdesk/tickets/" + someID, "", 404},
		{"bad ticket id", http.MethodGet, "/v1/helpdesk/tickets/nope", "", 400},
		{"patch bad status", http.MethodPatch, "/v1/helpdesk/tickets/" + someID, `{"status":"weird"}`, 400},
		{"patch bad assignee", http.MethodPatch, "/v1/helpdesk/tickets/" + someID, `{"assignee_id":"zzz"}`, 400},
		{"patch missing ticket", http.MethodPatch, "/v1/helpdesk/tickets/" + someID, `{"note":"hi"}`, 404},
		{"csat bad rating", http.MethodPatch, "/v1/helpdesk/tickets/" + someID + "/csat", `{"rating":9}`, 400},
		{"policy missing name", http.MethodPost, "/v1/helpdesk/sla-policies", `{"priority":"high","first_response_minutes":5,"resolve_minutes":60}`, 400},
		{"policy bad minutes", http.MethodPost, "/v1/helpdesk/sla-policies", `{"name":"x","priority":"high","first_response_minutes":0,"resolve_minutes":60}`, 400},
		{"policy patch missing", http.MethodPatch, "/v1/helpdesk/sla-policies/" + someID, `{"name":"x"}`, 404},
	} {
		rec := do(t, r, tc.method, tc.path, tc.body)
		if rec.Code != tc.want {
			t.Fatalf("%s = %d (%s), want %d", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

// CSAT before resolution → 409; auto-assign with no members → 409.
func TestConflictPaths(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, _ := testRouter(t, tenant)

	rec := do(t, r, http.MethodPost, "/v1/helpdesk/tickets", `{"subject":"open one"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}
	created := decodeEnv[struct {
		Ticket Ticket `json:"ticket"`
	}](t, rec)
	id := created.Ticket.ID.String()

	if rec := do(t, r, http.MethodPatch, "/v1/helpdesk/tickets/"+id+"/csat", `{"rating":4}`); rec.Code != http.StatusConflict {
		t.Fatalf("csat on open = %d, want 409", rec.Code)
	}
	if rec := do(t, r, http.MethodPatch, "/v1/helpdesk/tickets/"+id, `{"assignee_id":"auto"}`); rec.Code != http.StatusConflict {
		t.Fatalf("auto assign without members = %d (%s), want 409", rec.Code, rec.Body.String())
	}
}

// Cross-tenant isolation through the HTTP surface: a ticket created as
// tenant A is 404 / invisible as tenant B (app-level scoping; the DB-level
// RLS proof lives in TestRLSIsolation).
func TestCrossTenantHTTP(t *testing.T) {
	tenantA := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	rA, st := testRouter(t, tenantA)

	rec := do(t, rA, http.MethodPost, "/v1/helpdesk/tickets", `{"subject":"secret of A"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}
	created := decodeEnv[struct {
		Ticket Ticket `json:"ticket"`
	}](t, rec)
	id := created.Ticket.ID.String()

	// Tenant B handler over the SAME store/pool.
	tenantB := bookingops.TenantInfo{ID: uuid.New(), Slug: "beta"}
	rB := chi.NewRouter()
	RegisterRoutes(rB, &Deps{
		Store: st,
		TenantFromContext: func(ctx context.Context) (bookingops.TenantInfo, bool) {
			return tenantB, true
		},
	})

	if rec := do(t, rB, http.MethodGet, "/v1/helpdesk/tickets/"+id, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get = %d, want 404", rec.Code)
	}
	rec = do(t, rB, http.MethodGet, "/v1/helpdesk/tickets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cross-tenant list = %d", rec.Code)
	}
	list := decodeEnv[struct {
		Tickets []Ticket `json:"tickets"`
	}](t, rec)
	if len(list.Tickets) != 0 {
		t.Fatalf("tenant B sees %d tickets", len(list.Tickets))
	}
	if rec := do(t, rB, http.MethodPatch, "/v1/helpdesk/tickets/"+id, `{"status":"resolved"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant patch = %d, want 404", rec.Code)
	}
}

// Policy patch over HTTP (partial update semantics).
func TestPolicyPatchEndpoint(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, _ := testRouter(t, tenant)

	rec := do(t, r, http.MethodPost, "/v1/helpdesk/sla-policies",
		`{"name":"Normal tier","priority":"normal","first_response_minutes":60,"resolve_minutes":480}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	pol := decodeEnv[struct {
		Policy SLAPolicy `json:"policy"`
	}](t, rec)

	rec = do(t, r, http.MethodPatch, "/v1/helpdesk/sla-policies/"+pol.Policy.ID.String(),
		`{"resolve_minutes":240,"active":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d (%s)", rec.Code, rec.Body.String())
	}
	upd := decodeEnv[struct {
		Policy SLAPolicy `json:"policy"`
	}](t, rec)
	if upd.Policy.ResolveMinutes != 240 || upd.Policy.Active || upd.Policy.FirstResponseMinute != 60 {
		t.Fatalf("partial patch wrong: %+v", upd.Policy)
	}

	rec = do(t, r, http.MethodGet, "/v1/helpdesk/sla-policies", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	list := decodeEnv[struct {
		Policies []SLAPolicy `json:"policies"`
	}](t, rec)
	if len(list.Policies) != 1 {
		t.Fatalf("policies: %+v", list.Policies)
	}
}

// Graceful no-op: empty topics skip emission entirely (contract §5).
func TestEmissionDisabledIsNoOp(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	st := newTestStore(t)
	r := chi.NewRouter()
	RegisterRoutes(r, &Deps{
		Store: st,
		TenantFromContext: func(ctx context.Context) (bookingops.TenantInfo, bool) {
			return tenant, true
		},
		// EventsTopic / UsageTopic intentionally empty.
	})

	rec := do(t, r, http.MethodPost, "/v1/helpdesk/tickets", `{"subject":"quiet"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}
	created := decodeEnv[struct {
		Ticket Ticket `json:"ticket"`
	}](t, rec)
	if rec := do(t, r, http.MethodPatch, "/v1/helpdesk/tickets/"+created.Ticket.ID.String(), `{"status":"resolved"}`); rec.Code != http.StatusOK {
		t.Fatalf("resolve = %d", rec.Code)
	}
	var n int
	if err := st.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM outbox`).Scan(&n); err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	if n != 0 {
		t.Fatalf("disabled emission wrote %d outbox rows", n)
	}
}
