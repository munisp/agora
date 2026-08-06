package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/civic"
	"github.com/opendesk/booking-service/internal/incidents"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// Civic reporting API (SPEC-W32 WS-A): the public citizen endpoints (no
// tenant middleware — slug-resolved, throttled, honeypot-guarded), the
// operator console (tenant auth + manage_bookings, role-masked reporter
// PII) and the internal sla-breach callback (X-Tenant-Slug only).

func (s *server) civicSvc(w http.ResponseWriter) *civic.Service {
	if s.d.Civic == nil {
		writeError(w, http.StatusServiceUnavailable, "civic unavailable")
		return nil
	}
	return s.d.Civic
}

// mapCivicError converts civic/store sentinel errors to HTTP statuses.
func (s *server) mapCivicError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict")
	case errors.Is(err, civic.ErrThrottled):
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, civic.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.internal(w, err)
	}
}

// ---------------------------------------------------------------------------
// Public endpoints (no tenant middleware)
// ---------------------------------------------------------------------------

// publicReportRequest is the POST /v1/civic/public/tenants/{slug}/reports
// body: the report fields plus the honeypot `website` (must be empty).
type publicReportRequest struct {
	civic.ReportInput
	Website string `json:"website,omitempty"` // honeypot — bots fill it, humans never see it
}

// publicTenant resolves the tenant of a public civic route by slug.
func (s *server) publicTenant(w http.ResponseWriter, r *http.Request) (uuid.UUID, string, bool) {
	slug := chi.URLParam(r, "slug")
	if slug == "" || s.d.TenantBySlug == nil {
		writeError(w, http.StatusNotFound, "tenant not found")
		return uuid.Nil, "", false
	}
	info, err := s.d.TenantBySlug(r.Context(), slug)
	if err != nil {
		s.d.Logger.Warn("civic public tenant resolution failed", zap.String("slug", slug), zap.Error(err))
		writeError(w, http.StatusNotFound, "tenant not found")
		return uuid.Nil, "", false
	}
	return info.ID, info.Slug, true
}

// clientIP resolves the caller IP for throttling: the first
// X-Forwarded-For hop (trimmed) when present — behind APISIX every citizen
// would otherwise share one RemoteAddr bucket — falling back to RemoteAddr.
// (Spoofing note: this matches the gateway deployment, where APISIX
// terminates the edge and appends the real client IP.)
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		if ip := strings.TrimSpace(xff); ip != "" {
			return ip
		}
	}
	return r.RemoteAddr
}

// publicCivicReport handles POST /v1/civic/public/tenants/{slug}/reports:
// honeypot → throttle (per-IP + per-phone) → validate → persist →
// {ref, ack_due_at}. Emits com.opendesk.civic.ReportReceived.
func (s *server) publicCivicReport(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenantID, slug, ok := s.publicTenant(w, r)
	if !ok {
		return
	}
	var req publicReportRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Honeypot: a filled `website` is a bot — reject as a validation
	// failure (SPEC-W32 WS-A: the field must be empty).
	if req.Website != "" {
		writeError(w, http.StatusBadRequest, "spam detected")
		return
	}
	if err := svc.CheckThrottle(clientIP(r), req.ReporterPhoneE164); err != nil {
		s.mapCivicError(w, err)
		return
	}
	c, err := svc.Submit(r.Context(), tenantID, slug, civic.ChannelWeb, req.ReportInput)
	if err != nil {
		s.mapCivicError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"ref":        c.Ref,
		"ack_due_at": c.AckDueAt,
	})
}

// civicTrackView is the public tracking payload (SPEC-W32 WS-A): NO
// operator notes, NO other cases, NO reporter PII beyond what the citizen
// already knows.
type civicTrackView struct {
	Ref        string     `json:"ref"`
	Category   string     `json:"category"`
	Status     string     `json:"status"`
	Ward       string     `json:"ward"`
	MDAQueue   string     `json:"mda_queue"`
	CreatedAt  time.Time  `json:"created_at"`
	AckedAt    *time.Time `json:"acked_at,omitempty"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	MergedInto string     `json:"merged_into,omitempty"`
}

// publicCivicTrack handles GET /v1/civic/public/tenants/{slug}/reports/{ref}?phone=
// — ref+phone must match; wrong phone / unknown ref both 404 (no oracle).
func (s *server) publicCivicTrack(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenantID, _, ok := s.publicTenant(w, r)
	if !ok {
		return
	}
	c, err := svc.Track(r.Context(), tenantID, chi.URLParam(r, "ref"), r.URL.Query().Get("phone"))
	if err != nil {
		s.mapCivicError(w, err)
		return
	}
	view := civicTrackView{
		Ref:        c.Ref,
		Status:     c.Status,
		Ward:       c.Ward,
		MDAQueue:   c.MDAQueue,
		CreatedAt:  c.CreatedAt,
		AckedAt:    c.AckedAt,
		ResolvedAt: c.ResolvedAt,
	}
	if cat, err := svc.Store.GetCivicCategory(r.Context(), tenantID, c.CategoryID); err == nil {
		view.Category = cat.Slug
	}
	if c.MergedInto != nil {
		// Merged cases stay readable and point at the canonical (§4.3).
		if canon, err := svc.Store.GetCivicCase(r.Context(), tenantID, *c.MergedInto); err == nil {
			view.MergedInto = canon.Ref
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// publicCivicCategories handles GET .../categories — active categories for
// the intake form.
func (s *server) publicCivicCategories(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenantID, _, ok := s.publicTenant(w, r)
	if !ok {
		return
	}
	cats, err := svc.Store.ListCivicCategories(r.Context(), tenantID, true)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": cats})
}

// publicCivicStats handles GET .../stats — aggregate-only counts by
// category and ward (no person data, SPEC-W32 §4.1).
func (s *server) publicCivicStats(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenantID, _, ok := s.publicTenant(w, r)
	if !ok {
		return
	}
	stats, err := svc.PublicStats(r.Context(), tenantID)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// ---------------------------------------------------------------------------
// Operator console (tenant auth + manage_bookings)
// ---------------------------------------------------------------------------

// civicCaseView is the operator case representation: the stored case plus
// SLA countdown fields (seconds to due; negative = overdue) and the
// category slug for display. Reporter PII is masked by role at write time.
type civicCaseView struct {
	store.CivicCase
	CategorySlug        string `json:"category_slug"`
	AckDueInSeconds     *int64 `json:"ack_due_in_seconds,omitempty"`
	ResolveDueInSeconds *int64 `json:"resolve_due_in_seconds,omitempty"`
}

func civicCountdown(due *time.Time, now time.Time) *int64 {
	if due == nil {
		return nil
	}
	s := int64(due.Sub(now).Seconds())
	return &s
}

// civicView builds the operator view with role-based reporter masking
// (SPEC-W32 §4.4: anonymous reporters masked unless owner/admin).
func (s *server) civicView(r *http.Request, svc *civic.Service, tenantID uuid.UUID, c store.CivicCase) civicCaseView {
	masked := c
	civic.MaskReporter(&masked, civic.CanViewReporterRole(rolesFrom(r.Context())))
	v := civicCaseView{CivicCase: masked}
	if cat, err := svc.Store.GetCivicCategory(r.Context(), tenantID, c.CategoryID); err == nil {
		v.CategorySlug = cat.Slug
	}
	now := time.Now().UTC()
	v.AckDueInSeconds = civicCountdown(c.AckDueAt, now)
	v.ResolveDueInSeconds = civicCountdown(c.ResolveDueAt, now)
	return v
}

// listCivicCases handles GET /v1/civic/cases?status=&category=&ward=&sla_breach=&q=
func (s *server) listCivicCases(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	q := r.URL.Query()
	f := store.CivicCaseFilter{
		Status:    q.Get("status"),
		Ward:      q.Get("ward"),
		SLABreach: q.Get("sla_breach"),
		Query:     q.Get("q"),
	}
	if f.Status != "" {
		switch f.Status {
		case store.CivicStatusNew, store.CivicStatusTriaged, store.CivicStatusAssigned,
			store.CivicStatusInProgress, store.CivicStatusResolved, store.CivicStatusClosed:
		default:
			writeError(w, http.StatusBadRequest, "invalid status filter")
			return
		}
	}
	if catParam := q.Get("category"); catParam != "" {
		cat, err := svc.Store.GetCivicCategoryBySlug(r.Context(), tenant.ID, catParam)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusBadRequest, "unknown category filter")
				return
			}
			s.internal(w, err)
			return
		}
		f.CategoryID = &cat.ID
	}
	cases, err := svc.Store.ListCivicCases(r.Context(), tenant.ID, f)
	if err != nil {
		s.internal(w, err)
		return
	}
	views := []civicCaseView{}
	for _, c := range cases {
		views = append(views, s.civicView(r, svc, tenant.ID, c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"cases": views})
}

// getCivicCase handles GET /v1/civic/cases/{id} — detail incl. reporter
// (masked unless role owner/admin).
func (s *server) getCivicCase(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	c, err := svc.Store.GetCivicCase(r.Context(), tenant.ID, id)
	if err != nil {
		s.mapCivicError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.civicView(r, svc, tenant.ID, c))
}

// triageCivicCase handles POST /v1/civic/cases/{id}/triage.
func (s *server) triageCivicCase(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req civic.TriageInput
	if !decodeJSON(w, r, &req) {
		return
	}
	c, err := svc.Triage(r.Context(), tenant.ID, tenant.Slug, id, req)
	if err != nil {
		s.mapCivicError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.civicView(r, svc, tenant.ID, c))
}

// assignCivicCase handles POST /v1/civic/cases/{id}/assign {assignee}.
func (s *server) assignCivicCase(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Assignee string `json:"assignee"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	c, err := svc.Assign(r.Context(), tenant.ID, tenant.Slug, id, req.Assignee)
	if err != nil {
		s.mapCivicError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.civicView(r, svc, tenant.ID, c))
}

// statusCivicCase handles POST /v1/civic/cases/{id}/status
// {status: in_progress|resolved|closed, note?}.
func (s *server) statusCivicCase(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Status string `json:"status"`
		Note   string `json:"note,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	c, err := svc.SetStatus(r.Context(), tenant.ID, tenant.Slug, id, req.Status, req.Note)
	if err != nil {
		s.mapCivicError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.civicView(r, svc, tenant.ID, c))
}

// mergeCivicCase handles POST /v1/civic/cases/{id}/merge {canonical_id}.
func (s *server) mergeCivicCase(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		CanonicalID uuid.UUID `json:"canonical_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CanonicalID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "canonical_id is required")
		return
	}
	c, err := svc.Merge(r.Context(), tenant.ID, tenant.Slug, id, req.CanonicalID)
	if err != nil {
		s.mapCivicError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.civicView(r, svc, tenant.ID, c))
}

// civicCaseDuplicates handles GET /v1/civic/cases/{id}/duplicates — geo
// ≤500m + same category + ±72h candidates.
func (s *server) civicCaseDuplicates(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	cands, err := svc.Duplicates(r.Context(), tenant.ID, id)
	if err != nil {
		s.mapCivicError(w, err)
		return
	}
	views := []civicCaseView{}
	for _, c := range cands {
		views = append(views, s.civicView(r, svc, tenant.ID, c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": views})
}

// ---------------------------------------------------------------------------
// Category + routing CRUD
// ---------------------------------------------------------------------------

func (s *server) listCivicCategories(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	cats, err := svc.Store.ListCivicCategories(r.Context(), tenant.ID, false)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": cats})
}

func (s *server) createCivicCategory(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	var c store.CivicCategory
	if !decodeJSON(w, r, &c) {
		return
	}
	c.ID = uuid.Nil
	c.TenantID = tenant.ID
	if err := svc.CreateCategory(r.Context(), &c); err != nil {
		s.mapCivicError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// patchCivicCategory applies a partial category update (name, mda_queue,
// SLA hours, active).
func (s *server) patchCivicCategory(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Name            *string `json:"name,omitempty"`
		MDAQueue        *string `json:"mda_queue,omitempty"`
		AckSLAHours     *int    `json:"ack_sla_hours,omitempty"`
		ResolveSLAHours *int    `json:"resolve_sla_hours,omitempty"`
		Active          *bool   `json:"active,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	c, err := svc.Store.GetCivicCategory(r.Context(), tenant.ID, id)
	if err != nil {
		s.mapCivicError(w, err)
		return
	}
	if req.Name != nil {
		c.Name = *req.Name
	}
	if req.MDAQueue != nil {
		c.MDAQueue = *req.MDAQueue
	}
	if req.AckSLAHours != nil {
		c.AckSLAHours = *req.AckSLAHours
	}
	if req.ResolveSLAHours != nil {
		c.ResolveSLAHours = *req.ResolveSLAHours
	}
	if req.Active != nil {
		c.Active = *req.Active
	}
	if err := svc.UpdateCategory(r.Context(), &c); err != nil {
		s.mapCivicError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *server) listCivicRoutingRules(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	rules, err := svc.Store.ListCivicRoutingRules(r.Context(), tenant.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"routing_rules": rules})
}

func (s *server) createCivicRoutingRule(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	var rule store.CivicRoutingRule
	if !decodeJSON(w, r, &rule) {
		return
	}
	rule.ID = uuid.Nil
	rule.TenantID = tenant.ID
	if err := svc.CreateRoutingRule(r.Context(), &rule); err != nil {
		s.mapCivicError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (s *server) patchCivicRoutingRule(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Ward       *string    `json:"ward,omitempty"`
		CategoryID *uuid.UUID `json:"category_id,omitempty"`
		MDAQueue   *string    `json:"mda_queue,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	rules, err := svc.Store.ListCivicRoutingRules(r.Context(), tenant.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	var rule *store.CivicRoutingRule
	for i := range rules {
		if rules[i].ID == id {
			rule = &rules[i]
			break
		}
	}
	if rule == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if req.Ward != nil {
		rule.Ward = *req.Ward
	}
	if req.CategoryID != nil {
		rule.CategoryID = *req.CategoryID
	}
	if req.MDAQueue != nil {
		rule.MDAQueue = *req.MDAQueue
	}
	if err := svc.Store.UpdateCivicRoutingRule(r.Context(), rule); err != nil {
		s.mapCivicError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (s *server) deleteCivicRoutingRule(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if err := svc.Store.DeleteCivicRoutingRule(r.Context(), tenant.ID, id); err != nil {
		s.mapCivicError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Internal: SLA-breach callback (X-Tenant-Slug only, no Permify guard)
// ---------------------------------------------------------------------------

// civicSLABreachRequest is the POST /v1/civic/internal/cases/{ref}/sla-breach
// body written by notification-worker's ReportCivicSLABreach activity
// (SPEC-W32 §3 WS-B contract): the breach kind plus the MDA notification
// request. notify_mda asks booking-service to push the breach to the case's
// mda_queue dispatch endpoint via the W11 incident delivery path (the
// endpoint URL/secret live in this store, so the dispatch happens here).
type civicSLABreachRequest struct {
	Kind      string `json:"kind"`
	NotifyMDA bool   `json:"notify_mda,omitempty"`
	MDAQueue  string `json:"mda_queue,omitempty"`
}

// civicSLABreach handles POST /v1/civic/internal/cases/{ref}/sla-breach
// {kind: ack|resolve, notify_mda?, mda_queue?} — invoked by the
// notification-worker's CivicSLAWorkflow via Dapr when an SLA deadline
// passes unmet (SPEC-W32 WS-B contract).
func (s *server) civicSLABreach(w http.ResponseWriter, r *http.Request) {
	svc := s.civicSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	var req civicSLABreachRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	c, err := svc.MarkSLABreach(r.Context(), tenant.ID, chi.URLParam(r, "ref"), req.Kind)
	if err != nil {
		s.mapCivicError(w, err)
		return
	}
	resp := map[string]any{
		"ref":                c.Ref,
		"sla_breach_ack":     c.SLABreachAck,
		"sla_breach_resolve": c.SLABreachResolve,
		"mda_notified":       false,
	}
	if req.NotifyMDA {
		deliveries, notified := s.notifyCivicSLABreachMDA(r, tenant, c, req)
		resp["mda_notified"] = notified
		if notified {
			resp["deliveries"] = deliveries
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// notifyCivicSLABreachMDA pushes the breach to the case's MDA queue through
// the W11 incident delivery path (signed webhook + incident_deliveries
// ledger, SPEC-W32 §3 WS-B contract). The synthesized incident id is
// deterministic per (tenant, ref, kind) so Temporal/consumer replays are
// idempotent (Ingest reports created=false on replay); Dispatch is
// idempotent on (incident×endpoint) and is invoked unconditionally rather
// than relying on the AutoDispatch wiring. Dispatch failures are logged and
// never fail the breach response (Ingest's side-effect policy).
func (s *server) notifyCivicSLABreachMDA(r *http.Request, tenant bookingops.TenantInfo, c store.CivicCase, req civicSLABreachRequest) (deliveries int, notified bool) {
	if s.d.Incidents == nil {
		s.d.Logger.Warn("civic sla-breach mda notification skipped: incidents service not wired",
			zap.String("ref", c.Ref))
		return 0, false
	}
	queue := strings.TrimSpace(req.MDAQueue)
	if queue == "" {
		queue = c.MDAQueue
	}
	idp := incidents.IDP{
		IncidentID:      uuid.NewSHA1(uuid.NameSpaceURL, []byte(tenant.ID.String()+"|civic-sla-breach|"+c.Ref+"|"+req.Kind)),
		TenantID:        tenant.ID,
		Channel:         "system",
		IncidentType:    "civic_sla_breach",
		Severity:        incidents.SeverityHigh,
		ReferenceNumber: c.Ref,
		NarrativeSummary: fmt.Sprintf("Civic case %s SLA breach (%s); notify mda_queue %s; ward %s",
			c.Ref, req.Kind, queue, c.Ward),
	}
	if _, _, err := s.d.Incidents.Ingest(r.Context(), idp, tenant.Slug); err != nil {
		s.d.Logger.Error("civic sla-breach incident ingest failed",
			zap.String("ref", c.Ref), zap.String("kind", req.Kind), zap.Error(err))
		return 0, false
	}
	d, err := s.d.Incidents.Dispatch(r.Context(), tenant.ID, idp.IncidentID)
	if err != nil {
		s.d.Logger.Error("civic sla-breach mda dispatch failed",
			zap.String("ref", c.Ref), zap.String("mda_queue", queue), zap.Error(err))
		return 0, false
	}
	return len(d), true
}
