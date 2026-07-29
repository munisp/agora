package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/incidents"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// Incidents API (SPEC-W11 Part B §3/§4/§6): admin list/detail/dispatch,
// dispatch-endpoint CRUD (manage_bookings), the internal IoT/webhook ingest
// and the delivery-ledger update endpoint invoked by notification-worker's
// Wave-5 WebhookDeliveryWorkflow.

// incidentView is the GET detail response: row + delivery ledger.
type incidentView struct {
	store.Incident
	Deliveries []store.IncidentDelivery `json:"deliveries"`
}

func (s *server) incidentsSvc(w http.ResponseWriter) *incidents.Service {
	if s.d.Incidents == nil {
		writeError(w, http.StatusServiceUnavailable, "incidents unavailable")
		return nil
	}
	return s.d.Incidents
}

// listIncidents handles GET /v1/incidents?status=&from=&to= (RFC3339 or
// YYYY-MM-DD bounds).
func (s *server) listIncidents(w http.ResponseWriter, r *http.Request) {
	svc := s.incidentsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	q := r.URL.Query()
	status := q.Get("status")
	if status != "" {
		switch status {
		case incidents.StatusNew, incidents.StatusDispatched, incidents.StatusAcknowledged, incidents.StatusClosed:
		default:
			writeError(w, http.StatusBadRequest, "invalid status filter")
			return
		}
	}
	from, ok := parseTimeBound(w, q.Get("from"))
	if !ok {
		return
	}
	to, ok := parseTimeBound(w, q.Get("to"))
	if !ok {
		return
	}
	rows, err := svc.Store.ListIncidents(r.Context(), tenant.ID, status, from, to)
	if err != nil {
		s.internal(w, err)
		return
	}
	if rows == nil {
		rows = []store.Incident{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": rows})
}

// parseTimeBound parses an optional RFC3339 / YYYY-MM-DD query bound.
func parseTimeBound(w http.ResponseWriter, raw string) (*time.Time, bool) {
	if raw == "" {
		return nil, true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return &t, true
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return &t, true
	}
	writeError(w, http.StatusBadRequest, "invalid time bound (want RFC3339 or YYYY-MM-DD)")
	return nil, false
}

// getIncident handles GET /v1/incidents/{id} — row + delivery ledger.
func (s *server) getIncident(w http.ResponseWriter, r *http.Request) {
	svc := s.incidentsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	inc, err := svc.Store.GetIncident(r.Context(), tenant.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	deliveries, err := svc.Store.ListIncidentDeliveries(r.Context(), tenant.ID, id)
	if err != nil {
		s.internal(w, err)
		return
	}
	if deliveries == nil {
		deliveries = []store.IncidentDelivery{}
	}
	writeJSON(w, http.StatusOK, incidentView{Incident: inc, Deliveries: deliveries})
}

// dispatchIncident handles POST /v1/incidents/{id}/dispatch — delivers the
// IDP to every active tenant endpoint (signed, retried by the Wave-5
// delivery workflow) and returns the delivery ledger rows created.
func (s *server) dispatchIncident(w http.ResponseWriter, r *http.Request) {
	svc := s.incidentsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	deliveries, err := svc.Dispatch(r.Context(), tenant.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if deliveries == nil {
		deliveries = []store.IncidentDelivery{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": deliveries})
}

// ---------------------------------------------------------------------------
// Dispatch endpoints CRUD (manage_bookings)
// ---------------------------------------------------------------------------

type dispatchEndpointRequest struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
	Active *bool  `json:"active"`
}

func (s *server) createDispatchEndpoint(w http.ResponseWriter, r *http.Request) {
	svc := s.incidentsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	var req dispatchEndpointRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" || !(strings.HasPrefix(req.URL, "https://") || strings.HasPrefix(req.URL, "http://")) {
		writeError(w, http.StatusBadRequest, "url must be an absolute http(s) URL")
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	ep := store.DispatchEndpoint{TenantID: tenant.ID, URL: req.URL, Secret: req.Secret, Active: active}
	if err := svc.Store.UpsertDispatchEndpoint(r.Context(), &ep); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ep)
}

func (s *server) listDispatchEndpoints(w http.ResponseWriter, r *http.Request) {
	svc := s.incidentsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	eps, err := svc.Store.ListDispatchEndpoints(r.Context(), tenant.ID, false)
	if err != nil {
		s.internal(w, err)
		return
	}
	// Never leak signing secrets to list readers.
	for i := range eps {
		eps[i].Secret = ""
	}
	if eps == nil {
		eps = []store.DispatchEndpoint{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": eps})
}

func (s *server) deleteDispatchEndpoint(w http.ResponseWriter, r *http.Request) {
	svc := s.incidentsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	url := r.URL.Query().Get("url")
	if url == "" {
		writeError(w, http.StatusBadRequest, "url query parameter is required")
		return
	}
	err := svc.Store.DeleteDispatchEndpoint(r.Context(), tenant.ID, url)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Internal: IoT/webhook ingest + delivery-ledger updates (Dapr-invoked)
// ---------------------------------------------------------------------------

// ingestRequest is the POST /v1/incidents/ingest body (messaging-gateway →
// Dapr invoke, SPEC-W11 Part B §6): a partial IDP plus tenant addressing.
type ingestRequest struct {
	TenantSlug string        `json:"tenant_slug,omitempty"`
	TenantID   string        `json:"tenant_id,omitempty"`
	Incident   incidents.IDP `json:"incident"`
}

// ingestIncident completes + persists a (possibly partial) IDP with
// channel=webhook, then triggers auto-dispatch + outreach. Idempotent on
// incident_id. This route is registered WITHOUT the tenant middleware — it
// is invoked service-to-service via Dapr by the messaging-gateway, which
// already authenticated the caller (per-tenant shared secret).
func (s *server) ingestIncident(w http.ResponseWriter, r *http.Request) {
	svc := s.incidentsSvc(w)
	if svc == nil {
		return
	}
	var req ingestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	idp := req.Incident
	slug := req.TenantSlug
	if idp.TenantID == uuid.Nil && req.TenantID != "" {
		tid, err := uuid.Parse(req.TenantID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid tenant_id")
			return
		}
		idp.TenantID = tid
	}
	if idp.TenantID == uuid.Nil && slug != "" && s.d.TenantBySlug != nil {
		info, err := s.d.TenantBySlug(r.Context(), slug)
		if err != nil {
			s.d.Logger.Warn("ingest tenant resolution failed", zap.String("slug", slug), zap.Error(err))
			writeError(w, http.StatusNotFound, "tenant not found")
			return
		}
		idp.TenantID = info.ID
	}
	if idp.TenantID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "tenant_id or tenant_slug is required")
		return
	}
	row, created, err := svc.Ingest(r.Context(), idp, slug)
	if errors.Is(err, incidents.ErrInvalidInput) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"incident_id":      row.ID,
		"reference_number": row.ReferenceNumber,
		"created":          created,
	})
}

// deliveryUpdateRequest is the POST /internal/incidents/deliveries/{id}
// body written by notification-worker's UpdateWebhookDelivery activity for
// payload type "incident".
type deliveryUpdateRequest struct {
	Status     string `json:"status"` // retrying | delivered | dlq
	Attempts   int    `json:"attempts"`
	StatusCode int    `json:"status_code"` // 0 = transport error
	LastError  string `json:"last_error,omitempty"`
}

// updateIncidentDelivery records one attempt outcome in the ledger.
func (s *server) updateIncidentDelivery(w http.ResponseWriter, r *http.Request) {
	svc := s.incidentsSvc(w)
	if svc == nil {
		return
	}
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req deliveryUpdateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	switch req.Status {
	case incidents.DeliveryRetrying, incidents.DeliveryDelivered, incidents.DeliveryDLQ:
	default:
		writeError(w, http.StatusBadRequest, "invalid status")
		return
	}
	var code *int
	if req.StatusCode > 0 {
		c := req.StatusCode
		code = &c
	}
	err := svc.Store.UpdateIncidentDelivery(r.Context(), id, req.Status, req.Attempts, code, req.LastError)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
