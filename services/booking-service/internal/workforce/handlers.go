package workforce

// Workforce HTTP API (SPEC-W20 Agent D). Routes (mounted by RegisterRoutes
// on the ROOT chi router — the /v1 prefix is included):
//
//	GET    /v1/workforce/shifts?agent_id=&status=&from=&to=
//	POST   /v1/workforce/shifts
//	PATCH  /v1/workforce/shifts/{id}
//	GET    /v1/workforce/shifts/week?agent_id=&from=        7-day grid
//	POST   /v1/workforce/time/clock-in                      {agent_id, method, gps_lat?, gps_lng?}
//	POST   /v1/workforce/time/clock-out                     {agent_id}
//	GET    /v1/workforce/time/entries?agent_id=&from=&to=
//	GET    /v1/workforce/leave?status=&agent_id=
//	POST   /v1/workforce/leave
//	PATCH  /v1/workforce/leave/{id}                         {action: approve|decline}
//	GET    /v1/workforce/coverage?from=&to=                 per-day agents scheduled vs bookings
//	GET    /v1/workforce/utilization?from=&to=              per-agent scheduled/clocked hours
//	GET    /v1/workforce/team-members                       agent picker (active team members)
//
// AuthZ (SPEC-W20 contract §3): reads are view_analytics, writes are
// manage_bookings. RegisterRoutes applies the variadic mw GROUP-WIDE; the
// INTEGRATOR wires the permission middleware — recommended shape (composed
// from httpapi's existing require()):
//
//	method-aware: GET/HEAD → require("view_analytics"), else require("manage_bookings")
//
// plus the appgate entitlement gate for app_id "workforce".
//
// Tenant context: this package resolves the tenant itself from the
// X-Tenant-Slug header via Deps.Resolver (httpapi's tenantMiddleware ctx
// key is unexported, so the self-contained package cannot reuse it) and
// stores it under its own key; handlers read it via TenantFromContext —
// the small helper contract §3 asks for (the workorders idiom).
//
// Caller identity: leave decisions record decided_by from the JWT sub.
// httpapi's user ctx key is likewise unexported, so the integrator passes
// Deps.UserFromContext (httpapi's user accessor) — with an X-User-Id
// header fallback for non-gateway deployments.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"go.uber.org/zap"
)

// TenantResolver resolves a tenant by slug (bookingops.TenantResolver
// satisfies it).
type TenantResolver interface {
	BySlug(ctx context.Context, slug string) (bookingops.TenantInfo, error)
}

// Deps are the integration seams the integrator wires (SPEC-W20
// anti-collision contract). EventsTopic is OPTIONAL: empty disables
// emission (graceful no-op). There is NO usage/metering topic — workforce
// is an internal-ops app (contract §4).
type Deps struct {
	Store    *Store
	Resolver TenantResolver
	Logger   *zap.Logger
	// UserFromContext extracts the caller subject (JWT sub) for the
	// leave_requests.decided_by column; may be nil (X-User-Id header
	// fallback, then "").
	UserFromContext func(ctx context.Context) string
	// EventsTopic is the CloudEvents topic (WORKFORCE_EVENTS_TOPIC,
	// default opendesk.workforce.events.v1). Empty disables
	// shift-assigned / leave-decided events.
	EventsTopic string
}

// Handlers serves the /v1/workforce route group. Constructed by
// RegisterRoutes; exported fields allow direct wiring in tests (mirrors
// devices.Handlers).
type Handlers struct {
	Store           *Store
	Log             *zap.Logger
	UserFromContext func(ctx context.Context) string
	EventsTopic     string
}

func (h *Handlers) log() *zap.Logger {
	if h.Log != nil {
		return h.Log
	}
	return zap.NewNop()
}

// ctxKey is the package-private context key type (httpapi's key is
// unexported — see the file comment).
type ctxKey string

const ctxTenant ctxKey = "workforce-tenant"

// TenantFromContext returns the tenant resolved by the package tenant
// middleware (contract §3 helper). The zero value (ID == uuid.Nil) means
// no tenant context — handlers treat it as 400.
func TenantFromContext(ctx context.Context) bookingops.TenantInfo {
	t, _ := ctx.Value(ctxTenant).(bookingops.TenantInfo)
	return t
}

// RegisterRoutes mounts the workforce API at /v1/workforce on the given
// router. mw are applied group-wide AFTER the package tenant middleware
// (see the file comment for the integrator's recommended authZ shape).
func RegisterRoutes(r chi.Router, d *Deps, mw ...func(http.Handler) http.Handler) {
	h := &Handlers{
		Store:           d.Store,
		Log:             d.Logger,
		UserFromContext: d.UserFromContext,
		EventsTopic:     d.EventsTopic,
	}
	r.Route("/v1/workforce", func(r chi.Router) {
		r.Use(tenantMiddleware(d.Resolver, d.Logger))
		r.Use(mw...)
		r.Get("/shifts", h.ListShifts)
		r.Post("/shifts", h.CreateShift)
		r.Get("/shifts/week", h.WeekGrid)
		r.Patch("/shifts/{id}", h.PatchShift)
		r.Post("/time/clock-in", h.ClockIn)
		r.Post("/time/clock-out", h.ClockOut)
		r.Get("/time/entries", h.ListTimeEntries)
		r.Get("/leave", h.ListLeave)
		r.Post("/leave", h.CreateLeave)
		r.Patch("/leave/{id}", h.DecideLeave)
		r.Get("/coverage", h.Coverage)
		r.Get("/utilization", h.Utilization)
		r.Get("/team-members", h.TeamMembers)
	})
}

// tenantMiddleware resolves X-Tenant-Slug via the resolver and stores the
// TenantInfo under the package key. 503 when no resolver is wired (partial
// deployment), 400 without a slug, 404 on resolution failure — mirroring
// httpapi.tenantMiddleware's status codes.
func tenantMiddleware(resolver TenantResolver, log *zap.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = zap.NewNop()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if resolver == nil {
				writeError(w, http.StatusServiceUnavailable, "workforce unavailable (tenant resolver not wired)")
				return
			}
			slug := strings.TrimSpace(r.Header.Get("X-Tenant-Slug"))
			if slug == "" {
				writeError(w, http.StatusBadRequest, "X-Tenant-Slug header is required")
				return
			}
			tenant, err := resolver.BySlug(r.Context(), slug)
			if err != nil || tenant.ID == uuid.Nil {
				log.Warn("tenant resolution failed", zap.String("slug", slug), zap.Error(err))
				writeError(w, http.StatusNotFound, "tenant not found")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxTenant, tenant)))
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handlers) mapErr(w http.ResponseWriter, err error) {
	var overlap OverlapError
	var openEntry OpenEntryError
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNoOpenEntry):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.As(err, &overlap):
		// SPEC-W20: shift overlap → 409 WITH the conflicting shift id.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":                overlap.Error(),
			"conflicting_shift_id": overlap.ConflictShiftID.String(),
		})
	case errors.As(err, &openEntry):
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":         openEntry.Error(),
			"open_entry_id": openEntry.EntryID.String(),
		})
	case errors.Is(err, ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrInvalidTransition):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.log().Error("workforce handler error", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// tenantOr400 extracts the tenant context or writes 400.
func (h *Handlers) tenantOr400(w http.ResponseWriter, r *http.Request) (bookingops.TenantInfo, bool) {
	t := TenantFromContext(r.Context())
	if t.ID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "tenant context required")
		return t, false
	}
	return t, true
}

// callerSub resolves the caller subject for leave decided_by: JWT sub via
// the integrator-wired UserFromContext, X-User-Id fallback, else "".
func (h *Handlers) callerSub(r *http.Request) string {
	if h.UserFromContext != nil {
		if sub := strings.TrimSpace(h.UserFromContext(r.Context())); sub != "" {
			return sub
		}
	}
	return strings.TrimSpace(r.Header.Get("X-User-Id"))
}

// parseTimeParam accepts RFC3339 or a bare YYYY-MM-DD date (UTC midnight).
func parseTimeParam(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", v)
}

// rangeParams parses from/to query params with defaults (from = today UTC
// midnight, to = from+days) and a hard cap so reporting endpoints stay
// cheap. Returns inclusive-from / exclusive-to.
func rangeParams(w http.ResponseWriter, r *http.Request, defaultDays, maxDays int) (time.Time, time.Time, bool) {
	q := r.URL.Query()
	var from, to time.Time
	if v := q.Get("from"); v != "" {
		t, err := parseTimeParam(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid from (RFC3339 or YYYY-MM-DD)")
			return from, to, false
		}
		from = t
	} else {
		now := time.Now().UTC()
		from = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	}
	if v := q.Get("to"); v != "" {
		t, err := parseTimeParam(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid to (RFC3339 or YYYY-MM-DD)")
			return from, to, false
		}
		to = t
	} else {
		to = from.AddDate(0, 0, defaultDays)
	}
	if !to.After(from) {
		writeError(w, http.StatusBadRequest, "to must be after from")
		return from, to, false
	}
	if to.Sub(from) > time.Duration(maxDays)*24*time.Hour {
		writeError(w, http.StatusBadRequest, "range too large (max 62 days)")
		return from, to, false
	}
	return from, to, true
}

// ---------------------------------------------------------------------------
// shifts
// ---------------------------------------------------------------------------

// createShiftRequest is the POST /v1/workforce/shifts body. A shift always
// starts in "scheduled".
type createShiftRequest struct {
	AgentID  uuid.UUID `json:"agent_id"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	Role     string    `json:"role,omitempty"`
}

// CreateShift (POST /v1/workforce/shifts) creates a scheduled shift. 201;
// 409 with conflicting_shift_id when it overlaps another non-cancelled
// shift of the same agent. Emits the shift-assigned event.
func (h *Handlers) CreateShift(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	var req createShiftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	sh := Shift{
		TenantID: tenant.ID,
		AgentID:  req.AgentID,
		StartsAt: req.StartsAt,
		EndsAt:   req.EndsAt,
		Role:     req.Role,
		Status:   ShiftScheduled,
	}
	if err := sh.Validate(); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.CreateShift(r.Context(), &sh); err != nil {
		h.mapErr(w, err)
		return
	}
	h.publishShiftAssigned(r.Context(), tenant.Slug, sh)
	writeJSON(w, http.StatusCreated, map[string]any{"shift": sh})
}

// ListShifts (GET /v1/workforce/shifts?agent_id=&status=&from=&to=).
func (h *Handlers) ListShifts(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var f ShiftFilters
	if a := q.Get("agent_id"); a != "" {
		id, err := uuid.Parse(a)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid agent_id filter")
			return
		}
		f.AgentID = &id
	}
	if s := q.Get("status"); s != "" {
		if err := ValidateShiftStatus(s); err != nil {
			writeError(w, http.StatusBadRequest, "invalid status filter")
			return
		}
		f.Status = s
	}
	if v := q.Get("from"); v != "" {
		t, err := parseTimeParam(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid from (RFC3339 or YYYY-MM-DD)")
			return
		}
		f.From = &t
	}
	if v := q.Get("to"); v != "" {
		t, err := parseTimeParam(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid to (RFC3339 or YYYY-MM-DD)")
			return
		}
		f.To = &t
	}
	shifts, err := h.Store.ListShifts(r.Context(), tenant.ID, f)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shifts": shifts})
}

// patchShiftRequest is the PATCH /v1/workforce/shifts/{id} body. Every
// field is optional; only present fields change. Status changes run the
// state machine (scheduled→confirmed→completed|no_show|cancelled).
// Re-assigning agent_id re-runs the overlap guard and emits a fresh
// shift-assigned event.
type patchShiftRequest struct {
	AgentID  *uuid.UUID `json:"agent_id,omitempty"`
	StartsAt *time.Time `json:"starts_at,omitempty"`
	EndsAt   *time.Time `json:"ends_at,omitempty"`
	Role     *string    `json:"role,omitempty"`
	Status   *string    `json:"status,omitempty"`
}

// PatchShift (PATCH /v1/workforce/shifts/{id}) applies a partial update
// with state-machine + overlap enforcement. 200.
func (h *Handlers) PatchShift(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid shift id")
		return
	}
	var req patchShiftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	sh, err := h.Store.GetShift(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}

	reassigned := false
	if req.AgentID != nil && *req.AgentID != sh.AgentID {
		sh.AgentID = *req.AgentID
		reassigned = true
	}
	if req.StartsAt != nil {
		sh.StartsAt = *req.StartsAt
	}
	if req.EndsAt != nil {
		sh.EndsAt = *req.EndsAt
	}
	if req.Role != nil {
		sh.Role = *req.Role
	}
	if req.Status != nil && *req.Status != sh.Status {
		if err := ValidateShiftTransition(sh.Status, *req.Status); err != nil {
			h.mapErr(w, err)
			return
		}
		sh.Status = *req.Status
	}
	if err := sh.Validate(); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.UpdateShift(r.Context(), &sh); err != nil {
		h.mapErr(w, err)
		return
	}
	if reassigned && sh.Status != ShiftCancelled {
		h.publishShiftAssigned(r.Context(), tenant.Slug, sh)
	}
	writeJSON(w, http.StatusOK, map[string]any{"shift": sh})
}

// WeekGrid (GET /v1/workforce/shifts/week?agent_id=&from=) returns the
// 7-day grid: shifts overlapping [weekStart, weekStart+7d) with agent
// names, plus the 7 day dates so the UI renders stable columns. from is a
// date (YYYY-MM-DD) or RFC3339; default is today in the tenant timezone.
func (h *Handlers) WeekGrid(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	loc := tenant.Location()
	var weekStart time.Time
	if v := r.URL.Query().Get("from"); v != "" {
		t, err := parseTimeParam(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid from (YYYY-MM-DD or RFC3339)")
			return
		}
		weekStart = time.Date(t.In(loc).Year(), t.In(loc).Month(), t.In(loc).Day(), 0, 0, 0, 0, loc)
	} else {
		now := time.Now().In(loc)
		weekStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	}
	var agentID *uuid.UUID
	if a := r.URL.Query().Get("agent_id"); a != "" {
		id, err := uuid.Parse(a)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid agent_id filter")
			return
		}
		agentID = &id
	}
	weekEnd := weekStart.AddDate(0, 0, 7)
	shifts, err := h.Store.WeekShifts(r.Context(), tenant.ID, weekStart, weekEnd, agentID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	days := make([]string, 0, 7)
	for i := 0; i < 7; i++ {
		days = append(days, weekStart.AddDate(0, 0, i).Format("2006-01-02"))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"week_start": weekStart.Format("2006-01-02"),
		"days":       days,
		"shifts":     shifts,
	})
}

// ---------------------------------------------------------------------------
// time tracking
// ---------------------------------------------------------------------------

// clockInRequest is the POST /v1/workforce/time/clock-in body. GPS is
// optional (both coordinates together).
type clockInRequest struct {
	AgentID uuid.UUID `json:"agent_id"`
	Method  string    `json:"method"`
	GPSLat  *float64  `json:"gps_lat,omitempty"`
	GPSLng  *float64  `json:"gps_lng,omitempty"`
}

// ClockIn (POST /v1/workforce/time/clock-in) opens a time entry. 201; 409
// with open_entry_id when the agent is already clocked in.
func (h *Handlers) ClockIn(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	var req clockInRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Method == "" {
		req.Method = MethodWeb
	}
	e := TimeEntry{
		TenantID: tenant.ID,
		AgentID:  req.AgentID,
		Method:   req.Method,
		GPSLat:   req.GPSLat,
		GPSLng:   req.GPSLng,
	}
	if e.AgentID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	if err := ValidateMethod(e.Method); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := e.ValidateGPS(); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.ClockIn(r.Context(), &e); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"time_entry": e})
}

// clockOutRequest is the POST /v1/workforce/time/clock-out body.
type clockOutRequest struct {
	AgentID uuid.UUID `json:"agent_id"`
}

// ClockOut (POST /v1/workforce/time/clock-out) closes the agent's open
// entry. 200 with the closed entry; 404 when no open entry exists.
func (h *Handlers) ClockOut(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	var req clockOutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.AgentID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	e, err := h.Store.ClockOut(r.Context(), tenant.ID, req.AgentID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"time_entry": e})
}

// ListTimeEntries (GET /v1/workforce/time/entries?agent_id=&from=&to=).
func (h *Handlers) ListTimeEntries(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var f TimeEntryFilters
	if a := q.Get("agent_id"); a != "" {
		id, err := uuid.Parse(a)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid agent_id filter")
			return
		}
		f.AgentID = &id
	}
	if v := q.Get("from"); v != "" {
		t, err := parseTimeParam(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid from (RFC3339 or YYYY-MM-DD)")
			return
		}
		f.From = &t
	}
	if v := q.Get("to"); v != "" {
		t, err := parseTimeParam(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid to (RFC3339 or YYYY-MM-DD)")
			return
		}
		f.To = &t
	}
	entries, err := h.Store.ListTimeEntries(r.Context(), tenant.ID, f)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"time_entries": entries})
}

// ---------------------------------------------------------------------------
// leave
// ---------------------------------------------------------------------------

// createLeaveRequest is the POST /v1/workforce/leave body. starts_on /
// ends_on are dates (YYYY-MM-DD).
type createLeaveRequest struct {
	AgentID  uuid.UUID `json:"agent_id"`
	Kind     string    `json:"kind"`
	StartsOn string    `json:"starts_on"`
	EndsOn   string    `json:"ends_on"`
	Reason   string    `json:"reason,omitempty"`
}

// CreateLeave (POST /v1/workforce/leave) files a pending leave request.
// 201.
func (h *Handlers) CreateLeave(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	var req createLeaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	startsOn, err := time.Parse("2006-01-02", req.StartsOn)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid starts_on (YYYY-MM-DD)")
		return
	}
	endsOn, err := time.Parse("2006-01-02", req.EndsOn)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ends_on (YYYY-MM-DD)")
		return
	}
	l := LeaveRequest{
		TenantID: tenant.ID,
		AgentID:  req.AgentID,
		Kind:     req.Kind,
		StartsOn: startsOn,
		EndsOn:   endsOn,
		Reason:   req.Reason,
	}
	if err := l.Validate(); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.CreateLeave(r.Context(), &l); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"leave_request": l})
}

// ListLeave (GET /v1/workforce/leave?status=&agent_id=).
func (h *Handlers) ListLeave(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var f LeaveFilters
	if s := q.Get("status"); s != "" {
		if s != LeavePending && s != LeaveApproved && s != LeaveDeclined {
			writeError(w, http.StatusBadRequest, "invalid status filter")
			return
		}
		f.Status = s
	}
	if a := q.Get("agent_id"); a != "" {
		id, err := uuid.Parse(a)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid agent_id filter")
			return
		}
		f.AgentID = &id
	}
	requests, err := h.Store.ListLeave(r.Context(), tenant.ID, f)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"leave_requests": requests})
}

// decideLeaveRequest is the PATCH /v1/workforce/leave/{id} body.
type decideLeaveRequest struct {
	Action string `json:"action"` // approve | decline
}

// DecideLeave (PATCH /v1/workforce/leave/{id}) approves or declines a
// pending request, recording decided_by from the JWT sub (contract §3).
// 200; 409 when already decided. Emits the leave-decided event.
func (h *Handlers) DecideLeave(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid leave request id")
		return
	}
	var req decideLeaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var decision string
	switch req.Action {
	case "approve":
		decision = LeaveApproved
	case "decline":
		decision = LeaveDeclined
	default:
		writeError(w, http.StatusBadRequest, `action must be "approve" or "decline"`)
		return
	}
	l, err := h.Store.DecideLeave(r.Context(), tenant.ID, id, decision, h.callerSub(r))
	if err != nil {
		h.mapErr(w, err)
		return
	}
	h.publishLeaveDecided(r.Context(), tenant.Slug, l)
	writeJSON(w, http.StatusOK, map[string]any{"leave_request": l})
}

// ---------------------------------------------------------------------------
// reporting
// ---------------------------------------------------------------------------

// Coverage (GET /v1/workforce/coverage?from=&to=) — per day, distinct
// agents scheduled vs bookings count. Range: from/to dates or RFC3339
// (defaults: today .. today+7d exclusive; max 62 days).
func (h *Handlers) Coverage(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	from, to, ok := rangeParams(w, r, 7, 62)
	if !ok {
		return
	}
	// generate_series over dates is inclusive — subtract one day from the
	// exclusive to bound.
	days, err := h.Store.Coverage(r.Context(), tenant.ID, from, to.Add(-24*time.Hour))
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"coverage": days})
}

// Utilization (GET /v1/workforce/utilization?from=&to=) — per agent:
// scheduled hours, clocked hours, utilization %; open entries counted to
// now and flagged. Range semantics match Coverage.
func (h *Handlers) Utilization(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	from, to, ok := rangeParams(w, r, 7, 62)
	if !ok {
		return
	}
	rows, err := h.Store.Utilization(r.Context(), tenant.ID, from, to)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"utilization": rows, "from": from, "to": to})
}

// TeamMembers (GET /v1/workforce/team-members) — active team members for
// the agent pickers (read-only projection of the core team_members table;
// mirrors the helpdesk assignee picker).
func (h *Handlers) TeamMembers(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	members, err := h.Store.ListTeamMembers(r.Context(), tenant.ID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"team_members": members})
}
