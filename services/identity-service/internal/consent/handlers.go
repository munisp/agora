package consent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/identity-service/internal/store"
	"go.uber.org/zap"
)

// TenantResolver resolves a tenant reference (slug OR uuid) to the tenant
// (store.Store satisfies it; tests substitute a fake). The slug is needed by
// the V2-D3 gate: caller tenant binding is expressed in slugs
// (X-Tenant-Slugs), so a uuid tenant reference must be reverse-resolved.
type TenantResolver interface {
	GetTenantBySlug(ctx context.Context, slug string) (store.Tenant, error)
	GetTenantByID(ctx context.Context, id uuid.UUID) (store.Tenant, error)
}

// EventPublisher publishes a CloudEvent via Dapr pubsub (daprc.Client
// satisfies it; tests substitute a fake).
type EventPublisher interface {
	PublishEvent(ctx context.Context, pubsub, topic string, data any) error
}

// Handler bundles the consent registry HTTP handlers (SPEC-W12 Agent C).
type Handler struct {
	Repo    Repository
	Tenants TenantResolver
	// Relay publishes the durable erasure outbox rows (SPEC-W43 I-04). The
	// erasure handler triggers a synchronous sweep after tombstoning (same
	// latency profile as the old direct publish); a background loop in
	// cmd/server retries failures. Nil disables erasure publishing (tests).
	Relay *Relay
	// Events/PubSub/ErasureTopic are retained for construction compatibility
	// but publishing now flows through Relay (K4 adds the privacy topic,
	// which does not fit these fields).
	Events       EventPublisher
	PubSub       string
	ErasureTopic string
	// InternalToken (IDENTITY_INTERNAL_TOKEN) authorizes K2 service callers
	// of the gated consent surfaces; TrustDirectTenancy
	// (OPENDESK_TRUST_DIRECT_TENANT=1) is the logged dev escape for
	// gateway-less runs. See authorizeDataAccess (SPEC-W44 F4 / V2-D3).
	InternalToken      string
	TrustDirectTenancy bool
	Logger             *zap.Logger
}

// RegisterRoutes adds the consent routes to the router (called additively
// from httpapi.NewRouter):
//
//	POST /v1/consents            capture (idempotent on tenant+subject+purpose)
//	GET  /v1/consents?subject=   list one subject's records (V2-D3 gated)
//	POST /v1/consents/erasure    tombstone + ErasureRequested CloudEvent (V2-D3 gated)
//	GET  /internal/consents/check?subject=&purpose=  service-to-service gate
//
// The two gated surfaces require an X-Internal-Token service caller or a
// tenant-bound authenticated subject — see authorizeDataAccess.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Route("/v1/consents", func(r chi.Router) {
		r.Post("/", h.capture)
		r.Get("/", h.list)
		r.Post("/erasure", h.erasure)
	})
	r.Get("/internal/consents/check", h.check)
}

// resolveTenant accepts a tenant reference as uuid OR slug (SPEC-W12 §5
// callers variously carry one or the other) and returns the tenant row (the
// slug is required by the V2-D3 tenant-binding check).
func (h *Handler) resolveTenant(ctx context.Context, ref string) (store.Tenant, error) {
	if id, err := uuid.Parse(strings.TrimSpace(ref)); err == nil {
		return h.Tenants.GetTenantByID(ctx, id)
	}
	return h.Tenants.GetTenantBySlug(ctx, ref)
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
	tenant, err := h.resolveTenant(r.Context(), ref)
	if err != nil {
		h.tenantError(w, err)
		return
	}
	rec := Record{
		TenantID:        tenant.ID,
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
// subject, including tombstoned ones (erasure is audit-visible). Gated
// (V2-D3): without the gate this leaked any tenant's consent records to any
// caller.
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
	tenant, err := h.resolveTenant(r.Context(), ref)
	if err != nil {
		h.tenantError(w, err)
		return
	}
	if !h.authorizeDataAccess(w, r, tenant.Slug) {
		return
	}
	recs, err := h.Repo.List(r.Context(), tenant.ID, subject)
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
// X-Tenant-Slug header. The /internal prefix is internauth-gated (K2,
// X-Internal-Token == IDENTITY_INTERNAL_TOKEN) by the httpapi router.
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
	tenant, err := h.resolveTenant(r.Context(), ref)
	if err != nil {
		h.tenantError(w, err)
		return
	}
	rec, err := h.Repo.Active(r.Context(), tenant.ID, subject, purpose)
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
		"tenant_id":  tenant.ID.String(),
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
//
// GATED (SPEC-W44 F4 / V2-D3): K4's PrivacyEraseRequested fanout to
// opendesk.privacy.events makes this endpoint DESTRUCTIVE cross-service, so
// it requires an X-Internal-Token service caller or a tenant-bound
// authenticated subject (authorizeDataAccess). Public data-principal
// self-service erasure is out of scope pending a verification flow.
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
	tenant, err := h.resolveTenant(r.Context(), ref)
	if err != nil {
		h.tenantError(w, err)
		return
	}
	// V2-D3 gate BEFORE any tombstone/outbox work: unauthorized callers must
	// not move a single record (or fan out the privacy event).
	if !h.authorizeDataAccess(w, r, tenant.Slug) {
		return
	}
	// Erasure eligibility (SPEC-W17 §8.8 / Agent D — additive): a data subject
	// whose id is seed-tagged (is_synthetic record) is immediate-eligible and
	// skips any waiting period. No waiting period exists today (erasure is
	// tombstone-only immediate for everyone), so this is behaviour-preserving
	// for real subjects; EvaluateErasureEligibility is the seam the NDPA
	// waiting period plugs into.
	eligibility := EvaluateErasureEligibility(req.Subject)
	if !eligibility.Immediate {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status":          "erasure_pending",
			"data_subject_id": req.Subject,
			"synthetic":       false,
			"eligibility":     eligibility.Reason,
			"retry_after":     eligibility.WaitingPeriod.String(),
		})
		return
	}
	if eligibility.Synthetic {
		h.Logger.Info("erasure fast-path: synthetic seed data subject",
			zap.String("subject", req.Subject), zap.String("tenant_id", tenant.ID.String()))
	}

	n, err := h.Repo.Erase(r.Context(), tenant.ID, req.Subject, req.Purpose)
	if err != nil {
		h.internal(w, err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "no active consent records for subject")
		return
	}

	// SPEC-W43 I-04 / SPEC-W44 K4: Erase wrote a durable outbox row in the
	// tombstone transaction; the relay publishes ErasureRequested (consent
	// topic) + PrivacyEraseRequested (opendesk.privacy.events — the topic
	// booking/conversation actually consume, F15-06). A synchronous sweep
	// keeps the request-path latency/profile of the old direct publish;
	// failures stay queued for the background relay loop.
	if h.Relay != nil {
		if _, err := h.Relay.Sweep(r.Context()); err != nil {
			h.Logger.Error("erasure outbox sweep failed (background relay retries)",
				zap.String("subject", req.Subject), zap.Error(err))
		}
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":          "erasure_recorded",
		"data_subject_id": req.Subject,
		"erased_records":  n,
		"eligibility":     "immediate",
		"synthetic":       eligibility.Synthetic,
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
