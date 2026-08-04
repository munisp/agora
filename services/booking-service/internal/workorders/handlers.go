package workorders

// Field-service HTTP API (SPEC-W19 Agent B). Routes (mounted by
// RegisterRoutes on the ROOT chi router):
//
//	GET    /v1/field-service/work-orders?status=&assignee=&q=&from=&to=
//	POST   /v1/field-service/work-orders
//	GET    /v1/field-service/work-orders/{id}
//	PATCH  /v1/field-service/work-orders/{id}
//	POST   /v1/field-service/work-orders/{id}/dispatch
//	GET    /v1/field-service/board
//	GET    /v1/field-service/today?assignee=
//
// AuthZ (SPEC-W19 contract §3): reads are view_analytics, writes are
// manage_bookings. RegisterRoutes applies the variadic mw GROUP-WIDE; the
// INTEGRATOR wires the permission middleware — recommended shape (composed
// from httpapi's existing require()):
//
//	method-aware: GET/HEAD → require("view_analytics"), else require("manage_bookings")
//
// plus the appgate entitlement gate for app_id "field-service".
//
// Tenant context: this package resolves the tenant itself from the
// X-Tenant-Slug header via Deps.Resolver (httpapi's tenantMiddleware ctx
// key is unexported, so the self-contained package cannot reuse it) and
// stores it under its own key; handlers read it via TenantFromContext —
// the small helper contract §3 asks for, consistent with how
// devices.Handlers receives an explicit bookingops.TenantInfo.

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

// Deps are the integration seams the integrator wires (SPEC-W19
// anti-collision contract). All topics are OPTIONAL: empty disables the
// corresponding emission (graceful no-op).
type Deps struct {
	Store    *Store
	Resolver TenantResolver
	Logger   *zap.Logger
	// NotificationsTopic is the dispatch-push target topic
	// (opendesk.notifications.outbox). Empty disables notify=true pushes.
	NotificationsTopic string
	// UsageTopic is the metering topic (opendesk.usage.events). Empty
	// disables workorder_completed metering.
	UsageTopic string
	// FSMEventsTopic is the lifecycle-events topic
	// (opendesk.fsm.events.v1). Empty disables assigned/completed events.
	FSMEventsTopic string
}

// Handlers serves the field-service endpoints. Constructed by
// RegisterRoutes; exported fields allow direct wiring in tests (mirrors
// devices.Handlers).
type Handlers struct {
	Store              *Store
	Log                *zap.Logger
	NotificationsTopic string
	UsageTopic         string
	FSMEventsTopic     string
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

const ctxTenant ctxKey = "workorders-tenant"

// TenantFromContext returns the tenant resolved by the package tenant
// middleware (contract §3 helper). The zero value (ID == uuid.Nil) means
// no tenant context — handlers treat it as 400.
func TenantFromContext(ctx context.Context) bookingops.TenantInfo {
	t, _ := ctx.Value(ctxTenant).(bookingops.TenantInfo)
	return t
}

// RegisterRoutes mounts the field-service API at /v1/field-service on the
// given router (call it on the ROOT router — the /v1 prefix is included).
// mw are applied group-wide AFTER the package tenant middleware (see the
// file comment for the integrator's recommended authZ shape).
func RegisterRoutes(r chi.Router, d *Deps, mw ...func(http.Handler) http.Handler) {
	h := &Handlers{
		Store:              d.Store,
		Log:                d.Logger,
		NotificationsTopic: d.NotificationsTopic,
		UsageTopic:         d.UsageTopic,
		FSMEventsTopic:     d.FSMEventsTopic,
	}
	r.Route("/v1/field-service", func(r chi.Router) {
		r.Use(tenantMiddleware(d.Resolver, d.Logger))
		r.Use(mw...)
		r.Get("/work-orders", h.List)
		r.Post("/work-orders", h.Create)
		r.Get("/work-orders/{id}", h.Get)
		r.Patch("/work-orders/{id}", h.Patch)
		r.Post("/work-orders/{id}/dispatch", h.Dispatch)
		r.Get("/board", h.Board)
		r.Get("/today", h.Today)
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
				writeError(w, http.StatusServiceUnavailable, "field-service unavailable (tenant resolver not wired)")
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
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrCompletionGate), errors.Is(err, ErrNoAssignee):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.log().Error("workorders handler error", zap.Error(err))
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

// ---------------------------------------------------------------------------
// CRUD
// ---------------------------------------------------------------------------

// createRequest is the POST /v1/field-service/work-orders body. A work
// order always starts in "created" — use the dispatch endpoint to assign.
type createRequest struct {
	ContactID      *uuid.UUID      `json:"contact_id,omitempty"`
	BookingID      *uuid.UUID      `json:"booking_id,omitempty"`
	Title          string          `json:"title"`
	Description    string          `json:"description,omitempty"`
	ScheduledStart *time.Time      `json:"scheduled_start,omitempty"`
	ScheduledEnd   *time.Time      `json:"scheduled_end,omitempty"`
	GPS            *gpsInput       `json:"gps,omitempty"`
	Checklist      []ChecklistItem `json:"checklist,omitempty"`
	FieldCaptureID *string         `json:"field_capture_id,omitempty"`
}

// gpsInput carries the optional fix on create/PATCH ({lat,lng,accuracy} —
// accuracy optional). A PATCH with gps:null clears the fix.
type gpsInput struct {
	Lat      float64  `json:"lat"`
	Lng      float64  `json:"lng"`
	Accuracy *float64 `json:"accuracy,omitempty"`
}

// Create (POST /v1/field-service/work-orders) creates a work order in
// status "created". 201.
func (h *Handlers) Create(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	wo := WorkOrder{
		TenantID:       tenant.ID,
		ContactID:      req.ContactID,
		BookingID:      req.BookingID,
		Title:          req.Title,
		Description:    req.Description,
		Status:         StatusCreated,
		ScheduledStart: req.ScheduledStart,
		ScheduledEnd:   req.ScheduledEnd,
		Checklist:      req.Checklist,
		FieldCaptureID: req.FieldCaptureID,
	}
	if wo.Checklist == nil {
		wo.Checklist = []ChecklistItem{}
	}
	if req.GPS != nil {
		wo.GPSLat = &req.GPS.Lat
		wo.GPSLng = &req.GPS.Lng
		wo.GPSAccuracy = req.GPS.Accuracy
	}
	if err := wo.Validate(); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.Create(r.Context(), &wo); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"work_order": wo})
}

// List (GET /v1/field-service/work-orders?status=&assignee=&q=&from=&to=).
func (h *Handlers) List(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var f ListFilters
	if s := q.Get("status"); s != "" {
		if err := ValidateStatus(s); err != nil {
			writeError(w, http.StatusBadRequest, "invalid status filter")
			return
		}
		f.Status = s
	}
	if a := q.Get("assignee"); a != "" {
		id, err := uuid.Parse(a)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid assignee filter")
			return
		}
		f.Assignee = &id
	}
	f.Q = strings.TrimSpace(q.Get("q"))
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid from (RFC3339)")
			return
		}
		f.From = &t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid to (RFC3339)")
			return
		}
		f.To = &t
	}
	orders, err := h.Store.List(r.Context(), tenant.ID, f)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"work_orders": orders})
}

// Get (GET /v1/field-service/work-orders/{id}).
func (h *Handlers) Get(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid work order id")
		return
	}
	wo, err := h.Store.Get(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"work_order": wo})
}

// patchRequest is the PATCH /v1/field-service/work-orders/{id} body. Every
// field is optional; only present fields change. Status changes run the
// state machine (and the completion gate for →completed). Assigning an
// assignee to a "created" order transitions it to "assigned" (the dispatch
// endpoint with notify=true is the richer path); re-assigning an
// "assigned" order keeps it in the assigned lane.
type patchRequest struct {
	Status         *string          `json:"status,omitempty"`
	Title          *string          `json:"title,omitempty"`
	Description    *string          `json:"description,omitempty"`
	ContactID      *uuid.UUID       `json:"contact_id,omitempty"`
	BookingID      *uuid.UUID       `json:"booking_id,omitempty"`
	AssigneeID     *uuid.UUID       `json:"assignee_id,omitempty"`
	ScheduledStart *time.Time       `json:"scheduled_start,omitempty"`
	ScheduledEnd   *time.Time       `json:"scheduled_end,omitempty"`
	GPS            *json.RawMessage `json:"gps,omitempty"` // null clears; object sets
	Checklist      []ChecklistItem  `json:"checklist,omitempty"`
	Proof          *Proof           `json:"proof,omitempty"`
	FieldCaptureID *string          `json:"field_capture_id,omitempty"`
}

// Patch (PATCH /v1/field-service/work-orders/{id}) applies a partial
// update with state-machine enforcement. Lifecycle events + metering fire
// on the assigned/completed transitions (best-effort, post-commit).
func (h *Handlers) Patch(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid work order id")
		return
	}
	var req patchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	wo, err := h.Store.Get(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}

	prevStatus := wo.Status
	becameAssigned, becameCompleted := false, false

	if req.Title != nil {
		wo.Title = *req.Title
	}
	if req.Description != nil {
		wo.Description = *req.Description
	}
	if req.ContactID != nil {
		wo.ContactID = req.ContactID
	}
	if req.BookingID != nil {
		wo.BookingID = req.BookingID
	}
	if req.ScheduledStart != nil {
		wo.ScheduledStart = req.ScheduledStart
	}
	if req.ScheduledEnd != nil {
		wo.ScheduledEnd = req.ScheduledEnd
	}
	if req.FieldCaptureID != nil {
		wo.FieldCaptureID = req.FieldCaptureID
	}
	if req.Checklist != nil {
		wo.Checklist = req.Checklist
	}
	if req.Proof != nil {
		wo.Proof = *req.Proof
	}
	if req.GPS != nil {
		if string(*req.GPS) == "null" {
			wo.GPSLat, wo.GPSLng, wo.GPSAccuracy = nil, nil, nil
		} else {
			var g gpsInput
			if err := json.Unmarshal(*req.GPS, &g); err != nil {
				writeError(w, http.StatusBadRequest, "invalid gps (want {lat,lng,accuracy?} or null)")
				return
			}
			wo.GPSLat = &g.Lat
			wo.GPSLng = &g.Lng
			wo.GPSAccuracy = g.Accuracy
		}
	}
	if req.AssigneeID != nil {
		wo.AssigneeID = req.AssigneeID
		// Direct assignment is a dispatch without notification: a created
		// order moves to assigned; an assigned order stays (re-dispatch).
		if wo.Status == StatusCreated {
			wo.Status = StatusAssigned
			becameAssigned = true
		} else if wo.Status == StatusAssigned {
			becameAssigned = true
		}
	}
	if req.Status != nil && *req.Status != prevStatus {
		if err := ValidateTransition(prevStatus, *req.Status); err != nil {
			h.mapErr(w, err)
			return
		}
		if *req.Status == StatusCompleted {
			wo.Status = StatusCompleted // gate reads the new checklist/proof
			if err := wo.checkCompletionGate(); err != nil {
				h.mapErr(w, err)
				return
			}
			now := time.Now().UTC()
			wo.CompletedAt = &now
			becameCompleted = true
		} else {
			wo.Status = *req.Status
		}
	}
	if err := wo.Validate(); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.Update(r.Context(), &wo); err != nil {
		h.mapErr(w, err)
		return
	}
	if becameAssigned && !becameCompleted {
		h.publishAssigned(r.Context(), tenant.Slug, wo)
	}
	if becameCompleted {
		h.publishCompleted(r.Context(), tenant.Slug, wo)
	}
	writeJSON(w, http.StatusOK, map[string]any{"work_order": wo})
}

// ---------------------------------------------------------------------------
// dispatch / board / today
// ---------------------------------------------------------------------------

// dispatchRequest is the POST /v1/field-service/work-orders/{id}/dispatch
// body: assignee_id is a team_members id or the literal "auto"
// (least-open-orders active team member — mirrors the helpdesk auto rule).
// notify=true enqueues a paced push_notification to the assignee (W16
// contract; graceful when the notifications topic is disabled).
type dispatchRequest struct {
	AssigneeID string `json:"assignee_id"`
	Notify     bool   `json:"notify,omitempty"`
}

// Dispatch (POST /v1/field-service/work-orders/{id}/dispatch) assigns the
// order and moves it to the assigned lane (created→assigned, or
// assigned→assigned for re-dispatch). 200 with {work_order, notified}.
func (h *Handlers) Dispatch(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid work order id")
		return
	}
	var req dispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.AssigneeID = strings.TrimSpace(req.AssigneeID)
	if req.AssigneeID == "" {
		writeError(w, http.StatusBadRequest, "assignee_id is required (team member id or \"auto\")")
		return
	}
	var assigneeID uuid.UUID
	if strings.EqualFold(req.AssigneeID, "auto") {
		assigneeID, err = h.Store.PickAutoAssignee(r.Context(), tenant.ID)
		if err != nil {
			h.mapErr(w, err)
			return
		}
	} else {
		assigneeID, err = uuid.Parse(req.AssigneeID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid assignee_id (want a team member uuid or \"auto\")")
			return
		}
	}

	wo, err := h.Store.Get(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if err := ValidateTransition(wo.Status, StatusAssigned); err != nil {
		h.mapErr(w, err)
		return
	}
	wo.Status = StatusAssigned
	wo.AssigneeID = &assigneeID
	if err := wo.Validate(); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.Update(r.Context(), &wo); err != nil {
		h.mapErr(w, err)
		return
	}
	h.publishAssigned(r.Context(), tenant.Slug, wo)

	notified := false
	if req.Notify {
		name, _ := h.Store.AssigneeName(r.Context(), tenant.ID, assigneeID)
		notified = h.notifyDispatch(r.Context(), tenant.Slug, tenant.ID, wo, assigneeID, name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"work_order": wo, "notified": notified})
}

// Board (GET /v1/field-service/board) returns the tenant's work orders
// grouped by status with assignee names — one key per status (always all
// six, empty lanes included) so the UI renders stable lanes.
func (h *Handlers) Board(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	items, err := h.Store.Board(r.Context(), tenant.ID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	lanes := map[string][]BoardItem{}
	for _, st := range Statuses {
		lanes[st] = []BoardItem{}
	}
	for _, it := range items {
		lanes[it.Status] = append(lanes[it.Status], it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"board": lanes})
}

// Today (GET /v1/field-service/today?assignee=) returns today's scheduled
// work orders (tenant-local day in the tenant timezone), optionally
// restricted to one assignee.
func (h *Handlers) Today(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	var assignee *uuid.UUID
	if a := r.URL.Query().Get("assignee"); a != "" {
		id, err := uuid.Parse(a)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid assignee filter")
			return
		}
		assignee = &id
	}
	now := time.Now().In(tenant.Location())
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tenant.Location())
	dayEnd := dayStart.AddDate(0, 0, 1)
	items, err := h.Store.Today(r.Context(), tenant.ID, dayStart, dayEnd, assignee)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"work_orders": items,
		"day_start":   dayStart,
		"day_end":     dayEnd,
	})
}
