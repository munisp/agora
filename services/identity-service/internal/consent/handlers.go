package consent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/identity-service/internal/events"
	"github.com/opendesk/identity-service/internal/store"
	"go.uber.org/zap"
)

// TenantResolver resolves a tenant slug to its uuid (store.Store satisfies
// it; tests substitute a fake). The /internal/consents/check endpoint is
// service-to-service: no auth middleware, tenant carried by header.
type TenantResolver interface {
	GetTenantBySlug(ctx context.Context, slug string) (store.Tenant, error)
}

// EventPublisher publishes a CloudEvent via Dapr pubsub (daprc.Client
// satisfies it; tests substitute a fake).
type EventPublisher interface {
	PublishEvent(ctx context.Context, pubsub, topic string, data any) error
}

// Handler bundles the consent registry HTTP handlers (SPEC-W12 Agent C).
type Handler struct {
	Repo         Repository
	Tenants      TenantResolver
	Events       EventPublisher
	PubSub       string
	ErasureTopic string
	Logger       *zap.Logger
}

// RegisterRoutes adds the consent routes to the router (called additively
// from httpapi.NewRouter):
//
//	POST /v1/consents            capture (idempotent on tenant+subject+purpose)
//	GET  /v1/consents?subject=   list one subject's records
//	POST /v1/consents/erasure    tombstone + ErasureRequested CloudEvent
//	GET  /internal/consents/check?subject=&purpose=  service-to-service gate
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Route("/v1/consents", func(r chi.Router) {
		r.Post("/", h.capture)
		r.Get("/", h.list)
		r.Post("/erasure", h.erasure)
	})
	r.Get("/internal/consents/check", h.check)
}

// resolveTenant accepts a tenant reference as uuid OR slug (SPEC-W12 §5
// callers variously carry one or the other).
func (h *Handler) resolveTenant(ctx context.Context, ref string) (uuid.UUID, error) {
	if id, err := uuid.Parse(strings.TrimSpace(ref)); err == nil {
		return id, nil
	}
	t, err := h.Tenants.GetTenantBySlug(ctx, ref)
	if err != nil {
		return uuid.Nil, err
	}
	return t.ID, nil
}

// tenantFromRequest extracts the tenant reference from headers
// (X-Tenant-ID uuid / X-Tenant-Slug slug — the repo header convention) or
// the explicit body/query fields.
func tenantFromRequest(r *http.Request, bodyID, bodySlug string) string {
	if v := r.Header.Get("X-Tenant-ID"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Tenant-Slug"); v != "" {
		return v
	}
	if bodyID != "" {
		return bodyID
	}
	return bodySlug
}

type captureRequest struct {
	TenantID        string `json:"tenant_id"` // uuid
	TenantSlug      string `json:"tenant"`    // slug alternative
	Subject         string `json:"subject"`
	Purpose         string `json:"purpose"`
	CapturedChannel string `json:"captured_channel"`
	CapturedLocale  string `json:"captured_locale"`
}

// capture (POST /v1/consents) records consent. Idempotent on
// (tenant, subject, purpose): a replay returns the existing record with its
// original captured_ts; a replay after erasure re-consents (clears the
// tombstone).
func (h *Handler) capture(w http.ResponseWriter, r *http.Request) {
	var req captureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Subject = strings.TrimSpace(req.Subject)
	req.Purpose = strings.TrimSpace(req.Purpose)
	if req.Subject == "" || req.Purpose == "" {
		writeError(w, http.StatusBadRequest, "subject and purpose are required")
		return
	}
	ref := tenantFromRequest(r, req.TenantID, req.TenantSlug)
	if ref == "" {
		writeError(w, http.StatusBadRequest, "tenant reference required (X-Tenant-ID/X-Tenant-Slug header or tenant_id/tenant field)")
		return
	}
	tenantID, err := h.resolveTenant(r.Context(), ref)
	if err != nil {
		h.tenantError(w, err)
		return
	}
	rec := Record{
		TenantID:        tenantID,
		DataSubjectID:   req.Subject,
		Purpose:         req.Purpose,
		CapturedChannel: req.CapturedChannel,
		CapturedLocale:  req.CapturedLocale,
	}
	if err := h.Repo.Capture(r.Context(), &rec); err != nil {
		h.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

// list (GET /v1/consents?subject=) returns every consent record of a data
// subject, including tombstoned ones (erasure is audit-visible).
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	subject := strings.TrimSpace(r.URL.Query().Get("subject"))
	if subject == "" {
		writeError(w, http.StatusBadRequest, "subject query parameter is required")
		return
	}
	ref := tenantFromRequest(r, r.URL.Query().Get("tenant_id"), r.URL.Query().Get("tenant"))
	if ref == "" {
		writeError(w, http.StatusBadRequest, "tenant reference required (X-Tenant-ID/X-Tenant-Slug header or tenant_id/tenant parameter)")
		return
	}
	tenantID, err := h.resolveTenant(r.Context(), ref)
	if err != nil {
		h.tenantError(w, err)
		return
	}
	recs, err := h.Repo.List(r.Context(), tenantID, subject)
	if err != nil {
		h.internal(w, err)
		return
	}
	if recs == nil {
		recs = []Record{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"consents": recs})
}

// check (GET /internal/consents/check?subject=&purpose=) is the
// service-to-service consent gate consumed by kyc-service (SPEC-W12 §5):
// 200 {"allowed": true, ...} when an active (non-erased) consent exists,
// 403 {"allowed": false, ...} otherwise. Tenant comes from the X-Tenant-ID /
// X-Tenant-Slug header; there is deliberately no auth middleware (mesh-internal
// endpoint, same trust level as /internal/tenants/*).
func (h *Handler) check(w http.ResponseWriter, r *http.Request) {
	subject := strings.TrimSpace(r.URL.Query().Get("subject"))
	purpose := strings.TrimSpace(r.URL.Query().Get("purpose"))
	if subject == "" || purpose == "" {
		writeError(w, http.StatusBadRequest, "subject and purpose query parameters are required")
		return
	}
	ref := tenantFromRequest(r, "", "")
	if ref == "" {
		writeError(w, http.StatusBadRequest, "X-Tenant-ID or X-Tenant-Slug header is required")
		return
	}
	tenantID, err := h.resolveTenant(r.Context(), ref)
	if err != nil {
		h.tenantError(w, err)
		return
	}
	rec, err := h.Repo.Active(r.Context(), tenantID, subject, purpose)
	if errors.Is(err, ErrNotFound) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"allowed": false,
			"error":   "no active consent for subject+purpose",
		})
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"allowed":    true,
		"tenant_id":  tenantID.String(),
		"consent_id": rec.ConsentID.String(),
		"purpose":    rec.Purpose,
	})
}

type erasureRequest struct {
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant"`
	Subject    string `json:"subject"`
	Purpose    string `json:"purpose"` // empty = all purposes of the subject
}

// erasure (POST /v1/consents/erasure) tombstones the subject's consent
// records and publishes com.opendesk.consent.ErasureRequested on
// opendesk.consent.erasure.v1 (SPEC-W12 §4). Erasure is TOMBSTONE-ONLY:
// this service keeps the audit row; downstream consumers (booking contacts,
// conversation transcripts, ...) anonymize their own copies of the data
// subject's records on receipt of the event (see docs/compliance-ndpa.md).
func (h *Handler) erasure(w http.ResponseWriter, r *http.Request) {
	var req erasureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Subject = strings.TrimSpace(req.Subject)
	req.Purpose = strings.TrimSpace(req.Purpose)
	if req.Subject == "" {
		writeError(w, http.StatusBadRequest, "subject is required")
		return
	}
	ref := tenantFromRequest(r, req.TenantID, req.TenantSlug)
	if ref == "" {
		writeError(w, http.StatusBadRequest, "tenant reference required (X-Tenant-ID/X-Tenant-Slug header or tenant_id/tenant field)")
		return
	}
	tenantID, err := h.resolveTenant(r.Context(), ref)
	if err != nil {
		h.tenantError(w, err)
		return
	}
	n, err := h.Repo.Erase(r.Context(), tenantID, req.Subject, req.Purpose)
	if err != nil {
		h.internal(w, err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "no active consent records for subject")
		return
	}

	// CloudEvent via the service's existing best-effort Dapr publish pattern
	// (same as TenantProvisioned/MemberInvited): the tombstone is the durable
	// record; a failed publish is logged and can be reconciled from consents.
	evt := events.New("identity-service", ErasureEventType, req.Subject, tenantID.String(), map[string]any{
		"tenant_id":       tenantID.String(),
		"data_subject_id": req.Subject,
		"purpose":         req.Purpose,
		"erased_records":  n,
		"erasure_ts":      time.Now().UTC(),
	})
	if err := h.Events.PublishEvent(r.Context(), h.PubSub, h.ErasureTopic, evt); err != nil {
		h.Logger.Error("failed to publish ErasureRequested",
			zap.String("subject", req.Subject), zap.Error(err))
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":          "erasure_recorded",
		"data_subject_id": req.Subject,
		"erased_records":  n,
	})
}

func (h *Handler) tenantError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	h.internal(w, err)
}

func (h *Handler) internal(w http.ResponseWriter, err error) {
	h.Logger.Error("consent internal error", zap.Error(err))
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
