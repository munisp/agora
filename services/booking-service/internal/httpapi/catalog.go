package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
)

// ---------------------------------------------------------------------------
// Offerings (catalog)
// ---------------------------------------------------------------------------

func (s *server) listOfferings(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	items, err := s.d.Store.ListOfferings(r.Context(), tenant.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"offerings": items})
}

type offeringRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	DurationMin int    `json:"duration_min"`
	BufferMin   int    `json:"buffer_min"`
	PriceCents  int64  `json:"price_cents"`
	Currency    string `json:"currency"`
	Capacity    int    `json:"capacity"`
	Bookable    *bool  `json:"bookable"`
}

func (req offeringRequest) validate() string {
	if req.Name == "" {
		return "name is required"
	}
	if req.DurationMin <= 0 {
		return "duration_min must be > 0"
	}
	if req.BufferMin < 0 {
		return "buffer_min must be >= 0"
	}
	if req.Capacity < 0 {
		return "capacity must be >= 0"
	}
	return ""
}

func (s *server) createOffering(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	var req offeringRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if msg := req.validate(); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	o := store.Offering{
		TenantID:    tenant.ID,
		Name:        req.Name,
		Description: req.Description,
		DurationMin: req.DurationMin,
		BufferMin:   req.BufferMin,
		PriceCents:  req.PriceCents,
		Currency:    defaultStr(req.Currency, tenant.Currency),
		Capacity:    defaultInt(req.Capacity, 1),
		Bookable:    req.Bookable == nil || *req.Bookable,
	}
	if err := s.d.Store.CreateOffering(r.Context(), &o); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, o)
}

func (s *server) getOffering(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	o, err := s.d.Store.GetOffering(r.Context(), tenant.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "offering not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *server) updateOffering(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req offeringRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if msg := req.validate(); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	o := store.Offering{
		ID:          id,
		TenantID:    tenant.ID,
		Name:        req.Name,
		Description: req.Description,
		DurationMin: req.DurationMin,
		BufferMin:   req.BufferMin,
		PriceCents:  req.PriceCents,
		Currency:    defaultStr(req.Currency, tenant.Currency),
		Capacity:    defaultInt(req.Capacity, 1),
		Bookable:    req.Bookable == nil || *req.Bookable,
	}
	if err := s.d.Store.UpdateOffering(r.Context(), &o); err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *server) deleteOffering(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if err := s.d.Store.DeleteOffering(r.Context(), tenant.ID, id); err != nil {
		s.mapOpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Team members + availability rules
// ---------------------------------------------------------------------------

func (s *server) listTeamMembers(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	items, err := s.d.Store.ListTeamMembers(r.Context(), tenant.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"team_members": items})
}

type teamMemberRequest struct {
	Name   string `json:"name"`
	Email  string `json:"email"`
	Role   string `json:"role"`
	Active *bool  `json:"active"`
	// UserID optionally links the member to an identity-service user
	// (SPEC-W45 CODER-M, STK O14 completion). Format-validated as UUID but
	// NOT existence-checked — identity-service owns users; this is a soft
	// reference for the team UI.
	UserID string `json:"user_id"`
}

func (s *server) createTeamMember(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	var req teamMemberRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	m := store.TeamMember{
		TenantID: tenant.ID,
		Name:     req.Name,
		Email:    req.Email,
		Role:     defaultStr(req.Role, "staff"),
		Active:   req.Active == nil || *req.Active,
	}
	if req.UserID != "" {
		uid, err := uuid.Parse(req.UserID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "user_id must be a uuid")
			return
		}
		m.UserID = &uid
	}
	if err := s.d.Store.CreateTeamMember(r.Context(), &m); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *server) getTeamMember(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	m, err := s.d.Store.GetTeamMember(r.Context(), tenant.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "team member not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *server) updateTeamMember(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req teamMemberRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	m := store.TeamMember{
		ID:       id,
		TenantID: tenant.ID,
		Name:     req.Name,
		Email:    req.Email,
		Role:     defaultStr(req.Role, "staff"),
		Active:   req.Active == nil || *req.Active,
	}
	if err := s.d.Store.UpdateTeamMember(r.Context(), &m); err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *server) deleteTeamMember(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if err := s.d.Store.DeleteTeamMember(r.Context(), tenant.ID, id); err != nil {
		s.mapOpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// availabilityRulesRequest replaces a team member's weekly rules.
type availabilityRulesRequest struct {
	Rules []ruleInput `json:"rules"`
}

type ruleInput struct {
	Weekday       int    `json:"weekday"` // 0=Sunday .. 6=Saturday
	StartMin      int    `json:"start_min"`
	EndMin        int    `json:"end_min"`
	EffectiveFrom string `json:"effective_from,omitempty"` // RFC3339 or YYYY-MM-DD
	EffectiveTo   string `json:"effective_to,omitempty"`
}

func (s *server) putAvailability(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if _, err := s.d.Store.GetTeamMember(r.Context(), tenant.ID, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "team member not found")
		return
	}
	var req availabilityRulesRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	rules := make([]store.AvailabilityRule, 0, len(req.Rules))
	for _, ri := range req.Rules {
		if ri.Weekday < 0 || ri.Weekday > 6 {
			writeError(w, http.StatusBadRequest, "weekday must be 0..6")
			return
		}
		if ri.StartMin < 0 || ri.EndMin > 24*60 || ri.EndMin <= ri.StartMin {
			writeError(w, http.StatusBadRequest, "invalid start_min/end_min")
			return
		}
		rule := store.AvailabilityRule{
			TeamMemberID: id,
			Weekday:      ri.Weekday,
			StartMin:     ri.StartMin,
			EndMin:       ri.EndMin,
		}
		if t, err := parseOptionalDate(ri.EffectiveFrom); err != nil {
			writeError(w, http.StatusBadRequest, "invalid effective_from")
			return
		} else {
			rule.EffectiveFrom = t
		}
		if t, err := parseOptionalDate(ri.EffectiveTo); err != nil {
			writeError(w, http.StatusBadRequest, "invalid effective_to")
			return
		} else {
			rule.EffectiveTo = t
		}
		rules = append(rules, rule)
	}
	if err := s.d.Store.SetAvailability(r.Context(), tenant.ID, id, rules); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

func (s *server) getAvailabilityRules(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	rules, err := s.d.Store.ListAvailabilityRules(r.Context(), tenant.ID, id)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
