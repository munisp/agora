package helpdesk

// Helpdesk HTTP API (SPEC-W19 Agent A). The package is self-contained per
// the anti-collision contract: RegisterRoutes mounts everything under
// /v1/helpdesk and the INTEGRATOR supplies the middleware chain (tenant
// resolution → JWT auth → appgate app_id "helpdesk" → perms) via mw, plus
// the TenantFromContext/UserFromContext accessors that read httpapi's
// request-scoped values (the same values httpapi's tenantMiddleware injects
// for the devices/fieldcapture handlers).
//
//	GET    /v1/helpdesk/tickets                 list (status/priority/assignee_id/channel/q filters)
//	POST   /v1/helpdesk/tickets                 create (+ created event, SLA auto-attach by priority)
//	GET    /v1/helpdesk/tickets/{id}            one ticket + events timeline
//	PATCH  /v1/helpdesk/tickets/{id}            assign ("auto" = least-open-tickets) / status /
//	                                            note / priority / sla_policy_id (timeline events,
//	                                            first_response_at / resolved_at tracking, due recompute)
//	PATCH  /v1/helpdesk/tickets/{id}/csat       {rating 1-5, comment} (resolved|closed only)
//	GET    /v1/helpdesk/sla-policies            list
//	POST   /v1/helpdesk/sla-policies            create
//	PATCH  /v1/helpdesk/sla-policies/{id}       partial update
//	GET    /v1/helpdesk/stats                   open by priority, breaches, 30d averages
//	GET    /v1/helpdesk/breaches                currently SLA-breached tickets
//	GET    /v1/helpdesk/team-members            active team members (assignee picker)

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"go.uber.org/zap"
)

// Deps is the integrator-facing wiring bundle (SPEC-W19 anti-collision
// contract). The integrator builds it in httpapi/server.go:
//
//	helpdesk.RegisterRoutes(r, &helpdesk.Deps{
//	    Store:             helpdeskStore,          // NewStore/DialStore
//	    Log:               logger,
//	    TenantFromContext: httpapi tenant accessor, // reads ctxTenant
//	    UserFromContext:   httpapi user accessor,   // reads ctxUser (JWT sub)
//	    EventsTopic:       cfg.HelpdeskEventsTopic, // default opendesk.helpdesk.events.v1
//	    UsageTopic:        cfg.HelpdeskUsageTopic,  // default opendesk.usage.events
//	}, tenantMw, appgateMw("helpdesk"), require("manage_bookings"|"view_analytics"))
type Deps struct {
	Store *Store
	Log   *zap.Logger
	// TenantFromContext extracts the resolved tenant injected by the
	// integrator's tenant middleware (consistent with the explicit
	// bookingops.TenantInfo the devices handlers receive).
	TenantFromContext func(ctx context.Context) (bookingops.TenantInfo, bool)
	// UserFromContext extracts the caller subject (JWT sub / X-User-Id) for
	// the ticket_events actor column; may be nil (actor recorded as "").
	UserFromContext func(ctx context.Context) string
	// EventsTopic is the CloudEvents topic (HELPDESK_EVENTS_TOPIC, default
	// opendesk.helpdesk.events.v1). Empty disables emission.
	EventsTopic string
	// UsageTopic is the usage-metering topic (HELPDESK_USAGE_TOPIC, default
	// opendesk.usage.events). Empty disables metering.
	UsageTopic string
}

// Handlers serves the /v1/helpdesk route group.
type Handlers struct {
	Store             *Store
	Log               *zap.Logger
	TenantFromContext func(ctx context.Context) (bookingops.TenantInfo, bool)
	UserFromContext   func(ctx context.Context) string
	EventsTopic       string
	UsageTopic        string
}

func (h *Handlers) log() *zap.Logger {
	if h.Log != nil {
		return h.Log
	}
	return zap.NewNop()
}

// RegisterRoutes mounts the full /v1/helpdesk route group (SPEC-W19
// anti-collision contract). mw is applied to the whole group in order — the
// integrator passes tenant resolution, appgate and perms middleware; reads
// vs writes are distinguished by method inside the group if the integrator
// wants per-permission chains (the conventional wiring is view_analytics for
// GET, manage_bookings for POST/PATCH).
func RegisterRoutes(r chi.Router, d *Deps, mw ...func(http.Handler) http.Handler) {
	h := &Handlers{
		Store:             d.Store,
		Log:               d.Log,
		TenantFromContext: d.TenantFromContext,
		UserFromContext:   d.UserFromContext,
		EventsTopic:       d.EventsTopic,
		UsageTopic:        d.UsageTopic,
	}
	r.Route("/v1/helpdesk", func(r chi.Router) {
		r.Use(mw...)
		r.Get("/tickets", h.ListTickets)
		r.Post("/tickets", h.CreateTicket)
		r.Get("/tickets/{id}", h.GetTicket)
		r.Patch("/tickets/{id}", h.PatchTicket)
		r.Patch("/tickets/{id}/csat", h.RecordCSAT)
		r.Get("/sla-policies", h.ListPolicies)
		r.Post("/sla-policies", h.CreatePolicy)
		r.Patch("/sla-policies/{id}", h.UpdatePolicy)
		r.Get("/stats", h.Stats)
		r.Get("/breaches", h.ListBreaches)
		r.Get("/team-members", h.ListTeamMembers)
	})
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
	case errors.Is(err, ErrNoAssignee), errors.Is(err, ErrInvalidTransition):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.log().Error("helpdesk handler error", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// tenant resolves the request tenant via the integrator-supplied accessor.
func (h *Handlers) tenant(w http.ResponseWriter, r *http.Request) (bookingops.TenantInfo, bool) {
	if h.TenantFromContext == nil {
		writeError(w, http.StatusInternalServerError, "tenant accessor not wired")
		return bookingops.TenantInfo{}, false
	}
	t, ok := h.TenantFromContext(r.Context())
	if !ok || t.ID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "tenant context required (X-Tenant-Slug middleware)")
		return bookingops.TenantInfo{}, false
	}
	return t, true
}

// actor is the caller subject recorded on ticket_events rows.
func (h *Handlers) actor(r *http.Request) string {
	if h.UserFromContext == nil {
		return ""
	}
	return h.UserFromContext(r.Context())
}

func parseIDParam(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid "+name)
		return uuid.Nil, false
	}
	return id, true
}

// ---------------------------------------------------------------------------
// Tickets
// ---------------------------------------------------------------------------

// ListTickets (GET /v1/helpdesk/tickets) — filters status/priority/
// assignee_id/channel plus q subject search (SPEC-W19 Agent A).
func (h *Handlers) ListTickets(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := TicketFilters{
		Status:   q.Get("status"),
		Priority: q.Get("priority"),
		Channel:  q.Get("channel"),
		Q:        q.Get("q"),
	}
	if f.Status != "" {
		if err := ValidateStatus(f.Status); err != nil {
			writeError(w, http.StatusBadRequest, "invalid status filter")
			return
		}
	}
	if f.Priority != "" {
		if err := ValidatePriority(f.Priority); err != nil {
			writeError(w, http.StatusBadRequest, "invalid priority filter")
			return
		}
	}
	if raw := q.Get("assignee_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid assignee_id filter")
			return
		}
		f.AssigneeID = &id
	}
	tickets, err := h.Store.ListTickets(r.Context(), tenant.ID, f)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tickets": tickets})
}

// createTicketRequest is the POST /v1/helpdesk/tickets body.
type createTicketRequest struct {
	Subject        string     `json:"subject"`
	Channel        string     `json:"channel"`
	Priority       string     `json:"priority"`
	ContactID      *uuid.UUID `json:"contact_id,omitempty"`
	ConversationID *uuid.UUID `json:"conversation_id,omitempty"`
	AssigneeID     *uuid.UUID `json:"assignee_id,omitempty"`
	SLAPolicyID    *uuid.UUID `json:"sla_policy_id,omitempty"`
}

// CreateTicket (POST /v1/helpdesk/tickets) — 201 with the created ticket;
// emits the ticket_created CloudEvent.
func (h *Handlers) CreateTicket(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	var req createTicketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	t := Ticket{
		TenantID:       tenant.ID,
		ContactID:      req.ContactID,
		ConversationID: req.ConversationID,
		Subject:        req.Subject,
		Channel:        req.Channel,
		Priority:       req.Priority,
		AssigneeID:     req.AssigneeID,
		SLAPolicyID:    req.SLAPolicyID,
	}
	if err := ValidateTicket(&t); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.CreateTicket(r.Context(), &t, h.actor(r)); err != nil {
		h.mapErr(w, err)
		return
	}
	h.emit(r.Context(), t, EventNameTicketCreated, tenant.Slug)
	writeJSON(w, http.StatusCreated, map[string]any{"ticket": t})
}

// GetTicket (GET /v1/helpdesk/tickets/{id}) — ticket + events timeline.
func (h *Handlers) GetTicket(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id")
	if !ok {
		return
	}
	t, err := h.Store.GetTicket(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	events, err := h.Store.ListEvents(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticket": t, "events": events})
}

// patchTicketRequest keeps assignee_id / sla_policy_id as raw JSON so the
// three-way semantics (absent / explicit null / value) survive decoding.
type patchTicketRequest struct {
	AssigneeID  *json.RawMessage `json:"assignee_id"`
	Status      *string          `json:"status"`
	Note        *string          `json:"note"`
	Priority    *string          `json:"priority"`
	SLAPolicyID *json.RawMessage `json:"sla_policy_id"`
}

// PatchTicket (PATCH /v1/helpdesk/tickets/{id}) — operator mutation with
// timeline writes. assignee_id accepts a team-member UUID, "auto"
// (least-open-tickets assignment) or null (unassign); sla_policy_id accepts
// a policy UUID or null (detach). On the resolved transition it meters
// ticket_resolved and emits ticket_resolved.
func (h *Handlers) PatchTicket(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id")
	if !ok {
		return
	}
	var req patchTicketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	in := PatchInput{}
	if req.AssigneeID != nil {
		raw := strings.TrimSpace(string(*req.AssigneeID))
		switch {
		case raw == "null":
			in.Unassign = true
		default:
			var s string
			if err := json.Unmarshal(*req.AssigneeID, &s); err != nil {
				writeError(w, http.StatusBadRequest, "assignee_id must be a UUID string, \"auto\" or null")
				return
			}
			if s == "auto" {
				in.AutoAssign = true
			} else {
				aid, err := uuid.Parse(s)
				if err != nil {
					writeError(w, http.StatusBadRequest, "assignee_id must be a UUID string, \"auto\" or null")
					return
				}
				in.AssigneeID = &aid
			}
		}
	}
	if req.SLAPolicyID != nil {
		raw := strings.TrimSpace(string(*req.SLAPolicyID))
		if raw == "null" {
			in.DetachPolicy = true
		} else {
			var s string
			if err := json.Unmarshal(*req.SLAPolicyID, &s); err != nil {
				writeError(w, http.StatusBadRequest, "sla_policy_id must be a UUID string or null")
				return
			}
			pid, err := uuid.Parse(s)
			if err != nil {
				writeError(w, http.StatusBadRequest, "sla_policy_id must be a UUID string or null")
				return
			}
			in.SLAPolicyID = &pid
		}
	}
	if req.Status != nil {
		if err := ValidateStatus(*req.Status); err != nil {
			h.mapErr(w, err)
			return
		}
		in.Status = req.Status
	}
	if req.Priority != nil {
		if err := ValidatePriority(*req.Priority); err != nil {
			h.mapErr(w, err)
			return
		}
		in.Priority = req.Priority
	}
	if req.Note != nil && len(*req.Note) > maxNoteLen {
		writeError(w, http.StatusBadRequest, "note exceeds maximum length")
		return
	}
	in.Note = req.Note

	res, err := h.Store.PatchTicket(r.Context(), tenant.ID, id, in, h.actor(r))
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if res.ResolvedNow {
		h.meterResolved(r.Context(), res.Ticket, tenant.Slug)
		h.emit(r.Context(), res.Ticket, EventNameTicketResolved, tenant.Slug)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticket": res.Ticket})
}

// csatRequest is the PATCH /v1/helpdesk/tickets/{id}/csat body.
type csatRequest struct {
	Rating  int    `json:"rating"`
	Comment string `json:"comment"`
}

// RecordCSAT (PATCH /v1/helpdesk/tickets/{id}/csat) — customer satisfaction
// capture; only resolved|closed tickets can be rated.
func (h *Handlers) RecordCSAT(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id")
	if !ok {
		return
	}
	var req csatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Rating < 1 || req.Rating > 5 {
		writeError(w, http.StatusBadRequest, "rating must be 1-5")
		return
	}
	if len(req.Comment) > maxCSATLen {
		writeError(w, http.StatusBadRequest, "comment exceeds maximum length")
		return
	}
	t, err := h.Store.RecordCSAT(r.Context(), tenant.ID, id, req.Rating, req.Comment)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticket": t})
}

// ---------------------------------------------------------------------------
// SLA policies
// ---------------------------------------------------------------------------

// ListPolicies (GET /v1/helpdesk/sla-policies).
func (h *Handlers) ListPolicies(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	policies, err := h.Store.ListPolicies(r.Context(), tenant.ID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": policies})
}

// createPolicyRequest is the POST /v1/helpdesk/sla-policies body.
type createPolicyRequest struct {
	Name                string `json:"name"`
	Priority            string `json:"priority"`
	FirstResponseMinute int    `json:"first_response_minutes"`
	ResolveMinutes      int    `json:"resolve_minutes"`
	Active              *bool  `json:"active,omitempty"`
}

// CreatePolicy (POST /v1/helpdesk/sla-policies) — 201 with the policy.
func (h *Handlers) CreatePolicy(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	var req createPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	p := SLAPolicy{
		TenantID:            tenant.ID,
		Name:                req.Name,
		Priority:            req.Priority,
		FirstResponseMinute: req.FirstResponseMinute,
		ResolveMinutes:      req.ResolveMinutes,
		Active:              true,
	}
	if req.Active != nil {
		p.Active = *req.Active
	}
	if err := ValidatePolicy(&p); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.CreatePolicy(r.Context(), &p); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"policy": p})
}

// updatePolicyRequest is the PATCH /v1/helpdesk/sla-policies/{id} body
// (pointer fields: only present fields are applied).
type updatePolicyRequest struct {
	Name                *string `json:"name"`
	Priority            *string `json:"priority"`
	FirstResponseMinute *int    `json:"first_response_minutes"`
	ResolveMinutes      *int    `json:"resolve_minutes"`
	Active              *bool   `json:"active"`
}

// UpdatePolicy (PATCH /v1/helpdesk/sla-policies/{id}).
func (h *Handlers) UpdatePolicy(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id")
	if !ok {
		return
	}
	var req updatePolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" || len(trimmed) > maxNameLen {
			writeError(w, http.StatusBadRequest, "name is required and must be <= 200 bytes")
			return
		}
		req.Name = &trimmed
	}
	if req.Priority != nil {
		if err := ValidatePriority(*req.Priority); err != nil {
			h.mapErr(w, err)
			return
		}
	}
	if req.FirstResponseMinute != nil && *req.FirstResponseMinute <= 0 {
		writeError(w, http.StatusBadRequest, "first_response_minutes must be > 0")
		return
	}
	if req.ResolveMinutes != nil && *req.ResolveMinutes <= 0 {
		writeError(w, http.StatusBadRequest, "resolve_minutes must be > 0")
		return
	}
	p, err := h.Store.UpdatePolicy(r.Context(), tenant.ID, id,
		req.Name, req.Priority, req.FirstResponseMinute, req.ResolveMinutes, req.Active)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": p})
}

// ---------------------------------------------------------------------------
// Stats, breaches, team members
// ---------------------------------------------------------------------------

// Stats (GET /v1/helpdesk/stats).
func (h *Handlers) Stats(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	st, err := h.Store.Stats(r.Context(), tenant.ID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": st})
}

// ListBreaches (GET /v1/helpdesk/breaches).
func (h *Handlers) ListBreaches(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	tickets, err := h.Store.ListBreaches(r.Context(), tenant.ID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tickets": tickets})
}

// ListTeamMembers (GET /v1/helpdesk/team-members) — active team members for
// the assignee picker (read-only projection of the core team_members table).
func (h *Handlers) ListTeamMembers(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
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
