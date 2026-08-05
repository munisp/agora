package lending

// Lending HTTP API (SPEC-W20 Agent C). Routes (mounted by RegisterRoutes
// on the ROOT chi router):
//
//	GET    /v1/lending/products?all=
//	POST   /v1/lending/products
//	PATCH  /v1/lending/products/{id}
//	GET    /v1/lending/applications?status=&contact_id=
//	POST   /v1/lending/applications
//	PATCH  /v1/lending/applications/{id}
//	POST   /v1/lending/applications/{id}/disburse
//	GET    /v1/lending/loans?status=&application_id=&contact_id=
//	POST   /v1/lending/loans/{id}/repay
//	GET    /v1/lending/loans/{id}
//	GET    /v1/lending/portfolio
//
// AuthZ (SPEC-W20 contract §3): reads are view_analytics, writes are
// manage_bookings. RegisterRoutes applies the variadic mw GROUP-WIDE; the
// INTEGRATOR wires the permission middleware — recommended shape (composed
// from httpapi's existing require()):
//
//	method-aware: GET/HEAD → require("view_analytics"), else require("manage_bookings")
//
// plus the appgate entitlement gate for app_id "lending".
//
// Tenant context: this package resolves the tenant itself from the
// X-Tenant-Slug header via Deps.Resolver (httpapi's tenantMiddleware ctx
// key is unexported, so the self-contained package cannot reuse it) and
// stores it under its own key; handlers read it via TenantFromContext —
// the small helper contract §3 asks for (mirrors internal/workorders).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// anti-collision contract). All topics are OPTIONAL: empty disables the
// corresponding emission (graceful no-op).
type Deps struct {
	Store    *Store
	Resolver TenantResolver
	Logger   *zap.Logger
	// EventsTopic is the lifecycle + disbursement-intent topic
	// (LENDING_EVENTS_TOPIC, default opendesk.lending.events.v1). Empty
	// disables decided/disbursed/repaid/intent events.
	EventsTopic string
	// UsageTopic is the metering topic (USAGE_EVENTS_TOPIC, default
	// opendesk.usage.events). Empty disables loan_disbursed metering.
	UsageTopic string
	// KYCURL is the kyc-service base URL (LENDING_KYC_URL). Empty = the
	// KYC service is not wired: approvals then REQUIRE an explicit
	// {kyc_override: true, reason}, recorded in the decision event payload.
	KYCURL string
	// KYCHTTP is the HTTP client for the KYC call (nil → a 5s-timeout
	// default). Tests inject a client against httptest servers.
	KYCHTTP *http.Client
	// UserFromContext extracts the caller subject (JWT sub) for the
	// applications.decided_by column; may be nil (X-User-Id header
	// fallback, then the body-supplied decided_by). Mirrors
	// workforce.Deps.UserFromContext.
	UserFromContext func(ctx context.Context) string
}

// Handlers serves the lending endpoints. Constructed by RegisterRoutes;
// exported fields allow direct wiring in tests (mirrors devices.Handlers).
type Handlers struct {
	Store       *Store
	Log         *zap.Logger
	EventsTopic string
	UsageTopic  string
	KYCURL      string
	KYCHTTP     *http.Client
	// UserFromContext extracts the caller subject (JWT sub) for
	// decided_by; may be nil (see Deps).
	UserFromContext func(ctx context.Context) string
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

const ctxTenant ctxKey = "lending-tenant"

// TenantFromContext returns the tenant resolved by the package tenant
// middleware (contract §3 helper). The zero value (ID == uuid.Nil) means
// no tenant context — handlers treat it as 400.
func TenantFromContext(ctx context.Context) bookingops.TenantInfo {
	t, _ := ctx.Value(ctxTenant).(bookingops.TenantInfo)
	return t
}

// RegisterRoutes mounts the lending API at /v1/lending on the given router
// (call it on the ROOT router — the /v1 prefix is included). mw are
// applied group-wide AFTER the package tenant middleware (see the file
// comment for the integrator's recommended authZ shape).
func RegisterRoutes(r chi.Router, d *Deps, mw ...func(http.Handler) http.Handler) {
	h := &Handlers{
		Store:           d.Store,
		Log:             d.Logger,
		EventsTopic:     d.EventsTopic,
		UsageTopic:      d.UsageTopic,
		KYCURL:          d.KYCURL,
		KYCHTTP:         d.KYCHTTP,
		UserFromContext: d.UserFromContext,
	}
	r.Route("/v1/lending", func(r chi.Router) {
		r.Use(tenantMiddleware(d.Resolver, d.Logger))
		r.Use(mw...)
		r.Get("/products", h.ListProducts)
		r.Post("/products", h.CreateProduct)
		r.Patch("/products/{id}", h.UpdateProduct)
		r.Get("/applications", h.ListApplications)
		r.Post("/applications", h.CreateApplication)
		r.Patch("/applications/{id}", h.PatchApplication)
		r.Post("/applications/{id}/disburse", h.Disburse)
		r.Get("/loans", h.ListLoans)
		r.Post("/loans/{id}/repay", h.Repay)
		r.Get("/loans/{id}", h.GetLoan)
		r.Get("/portfolio", h.Portfolio)
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
				writeError(w, http.StatusServiceUnavailable, "lending unavailable (tenant resolver not wired)")
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
	case errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrKYCRequired):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.log().Error("lending handler error", zap.Error(err))
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

// callerSub resolves the caller subject for decided_by: JWT sub via the
// integrator-wired UserFromContext, X-User-Id fallback, else "" (mirrors
// workforce.Handlers.callerSub).
func (h *Handlers) callerSub(r *http.Request) string {
	if h.UserFromContext != nil {
		if sub := strings.TrimSpace(h.UserFromContext(r.Context())); sub != "" {
			return sub
		}
	}
	return strings.TrimSpace(r.Header.Get("X-User-Id"))
}

// decidedBy resolves the operator identity stamped on approve/decline
// (SPEC-W24 WS-A2). Precedence follows the workforce leave-decision
// convention (leave_requests.decided_by is ALWAYS the caller identity,
// never client input): the authenticated caller (JWT sub via
// UserFromContext, X-User-Id fallback) WINS over any body-supplied
// decided_by. The body value remains only as a fallback for callers
// without an authenticated identity (unwired middleware in dev/tests) so
// the pre-W24 request shape keeps working.
func (h *Handlers) decidedBy(r *http.Request, bodyValue *string) *string {
	if sub := h.callerSub(r); sub != "" {
		return &sub
	}
	return bodyValue
}

// ---------------------------------------------------------------------------
// Products
// ---------------------------------------------------------------------------

// ListProducts (GET /v1/lending/products?all=true) — active products by
// default; all=true includes inactive.
func (h *Handlers) ListProducts(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	includeInactive := r.URL.Query().Get("all") == "true"
	products, err := h.Store.ListProducts(r.Context(), tenant.ID, includeInactive)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": products})
}

// productRequest is the POST /v1/lending/products body.
type productRequest struct {
	Name             string `json:"name"`
	Active           *bool  `json:"active,omitempty"`
	PrincipalMinKobo int64  `json:"principal_min_kobo"`
	PrincipalMaxKobo int64  `json:"principal_max_kobo"`
	TermDays         int    `json:"term_days"`
	InterestBps      int    `json:"interest_bps"`
	FeeFlatKobo      int64  `json:"fee_flat_kobo,omitempty"`
}

// CreateProduct (POST /v1/lending/products) → 201.
func (h *Handlers) CreateProduct(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	var req productRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	p := Product{
		TenantID:         tenant.ID,
		Name:             req.Name,
		Active:           active,
		PrincipalMinKobo: req.PrincipalMinKobo,
		PrincipalMaxKobo: req.PrincipalMaxKobo,
		TermDays:         req.TermDays,
		InterestBps:      req.InterestBps,
		FeeFlatKobo:      req.FeeFlatKobo,
	}
	if err := p.Validate(); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.CreateProduct(r.Context(), &p); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"product": p})
}

// UpdateProduct (PATCH /v1/lending/products/{id}) — partial update
// validated against the merged row.
func (h *Handlers) UpdateProduct(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid product id")
		return
	}
	var patch ProductPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	p, err := h.Store.UpdateProduct(r.Context(), tenant.ID, id, patch)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"product": p})
}

// ---------------------------------------------------------------------------
// Applications
// ---------------------------------------------------------------------------

// ListApplications (GET /v1/lending/applications?status=&contact_id=).
func (h *Handlers) ListApplications(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var f ApplicationFilters
	if s := q.Get("status"); s != "" {
		if err := ValidateStatus(s); err != nil {
			writeError(w, http.StatusBadRequest, "invalid status filter")
			return
		}
		f.Status = s
	}
	if c := q.Get("contact_id"); c != "" {
		id, err := uuid.Parse(c)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid contact_id filter")
			return
		}
		f.ContactID = &id
	}
	apps, err := h.Store.ListApplications(r.Context(), tenant.ID, f)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applications": apps})
}

// applicationRequest is the POST /v1/lending/applications body. status may
// be "draft" (default) or "submitted" — submitting at create computes the
// naive score immediately (SPEC-W20: score computed on submit).
type applicationRequest struct {
	ContactID     uuid.UUID `json:"contact_id"`
	ProductID     uuid.UUID `json:"product_id"`
	PrincipalKobo int64     `json:"principal_kobo"`
	Status        string    `json:"status,omitempty"`
}

// CreateApplication (POST /v1/lending/applications) → 201. The principal
// is validated against the product band; the product must be active.
func (h *Handlers) CreateApplication(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	var req applicationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	status := StatusDraft
	if req.Status != "" {
		status = req.Status
		if status != StatusDraft && status != StatusSubmitted {
			writeError(w, http.StatusBadRequest, "status must be draft|submitted at create")
			return
		}
	}
	prod, err := h.Store.GetProduct(r.Context(), tenant.ID, req.ProductID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if !prod.Active {
		writeError(w, http.StatusBadRequest, "product is inactive")
		return
	}
	if err := ValidatePrincipalAgainst(prod, req.PrincipalKobo); err != nil {
		h.mapErr(w, err)
		return
	}
	a := Application{
		TenantID:      tenant.ID,
		ContactID:     req.ContactID,
		ProductID:     req.ProductID,
		PrincipalKobo: req.PrincipalKobo,
		Status:        status,
	}
	if status == StatusSubmitted {
		score, _ := h.Store.ComputeScore(r.Context(), tenant.ID, a.ContactID)
		a.Score = &score
	}
	if err := a.Validate(); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.CreateApplication(r.Context(), &a); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"application": a})
}

// patchApplicationRequest is the PATCH /v1/lending/applications/{id} body.
// The only mutable field is the operator decision:
//
//	status:         submitted|under_review|approved|declined|defaulted
//	decline_reason: required for →declined
//	decided_by:     fallback operator handle for approve/decline — the
//	                authenticated identity (JWT sub) wins when resolvable
//	kyc_override + kyc_reason: approve gate when LENDING_KYC_URL is unset
//	kyc:            {subject_phone, id_type, id_value} for the kyc-service
//	                call when LENDING_KYC_URL IS set
type patchApplicationRequest struct {
	Status        *string   `json:"status,omitempty"`
	DeclineReason *string   `json:"decline_reason,omitempty"`
	DecidedBy     *string   `json:"decided_by,omitempty"`
	KYCOverride   bool      `json:"kyc_override,omitempty"`
	KYCReason     string    `json:"kyc_reason,omitempty"`
	KYC           *kycInput `json:"kyc,omitempty"`
}

// kycInput carries the subject identifiers for the kyc-service resolve
// call (SPEC-W12 contract: BVN/NIN, consent-gated server-side).
type kycInput struct {
	SubjectPhone string `json:"subject_phone"`
	IDType       string `json:"id_type"` // bvn|nin
	IDValue      string `json:"id_value"`
}

// PatchApplication (PATCH /v1/lending/applications/{id}) runs the
// operator decision machine: draft→submitted recomputes the score;
// submitted→under_review; under_review→approved (KYC-gated) | declined
// (reason required); approved|disbursed|submitted|under_review→defaulted
// (operator-driven; flips the active loan too). decided_at/decided_by are
// stamped on approve/decline; decided_by is the authenticated operator
// identity (see decidedBy).
func (h *Handlers) PatchApplication(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}
	var req patchApplicationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Status == nil {
		writeError(w, http.StatusBadRequest, "status is required")
		return
	}
	a, err := h.Store.GetApplication(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	target := *req.Status
	if err := ValidateTransition(a.Status, target); err != nil {
		h.mapErr(w, err)
		return
	}

	var kycDecision *KYCDecision
	switch target {
	case StatusSubmitted:
		score, _ := h.Store.ComputeScore(r.Context(), tenant.ID, a.ContactID)
		a.Score = &score
	case StatusDeclined:
		reason := strings.TrimSpace(deref(req.DeclineReason))
		if reason == "" {
			writeError(w, http.StatusBadRequest, "decline_reason is required")
			return
		}
		a.DeclineReason = &reason
		a.DecidedBy = h.decidedBy(r, req.DecidedBy)
		now := time.Now().UTC()
		a.DecidedAt = &now
	case StatusApproved:
		kycDecision, err = h.checkKYC(r, tenant, req)
		if err != nil {
			h.mapErr(w, err)
			return
		}
		a.DeclineReason = nil
		a.DecidedBy = h.decidedBy(r, req.DecidedBy)
		now := time.Now().UTC()
		a.DecidedAt = &now
	}
	a.Status = target
	if err := a.Validate(); err != nil {
		h.mapErr(w, err)
		return
	}
	defaultedLoanID, err := h.Store.UpdateApplication(r.Context(), &a)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if target == StatusApproved || target == StatusDeclined {
		h.publishDecided(r.Context(), tenant.Slug, a, target, kycDecision)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"application":       a,
		"defaulted_loan_id": defaultedLoanID,
	})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---------------------------------------------------------------------------
// KYC gate (approve)
// ---------------------------------------------------------------------------

// kycResolveResponse mirrors kyc-service's POST /v1/kyc/resolve response
// (SPEC-W12; field tags kept by contract — duplicated, not shared).
type kycResolveResponse struct {
	Status    string `json:"status"` // verified|mismatch|pending
	Reference string `json:"reference"`
}

// checkKYC enforces the approve gate (SPEC-W20):
//   - KYCURL set → POST {KYCURL}/v1/kyc/resolve with the operator-supplied
//     subject identifiers; only status "verified" passes. The kyc-service
//     endpoint is consent-gated (403 when the subject has no kyc consent).
//   - KYCURL empty → explicit {kyc_override: true, kyc_reason} required;
//     the override is recorded in the decision event payload.
func (h *Handlers) checkKYC(r *http.Request, tenant bookingops.TenantInfo, req patchApplicationRequest) (*KYCDecision, error) {
	if h.KYCURL == "" {
		if !req.KYCOverride || strings.TrimSpace(req.KYCReason) == "" {
			return nil, fmt.Errorf("%w: kyc service not configured — pass kyc_override:true with kyc_reason", ErrKYCRequired)
		}
		return &KYCDecision{Mode: "override", Reason: strings.TrimSpace(req.KYCReason)}, nil
	}
	if req.KYC == nil || strings.TrimSpace(req.KYC.SubjectPhone) == "" ||
		strings.TrimSpace(req.KYC.IDType) == "" || strings.TrimSpace(req.KYC.IDValue) == "" {
		return nil, fmt.Errorf("%w: pass kyc:{subject_phone,id_type,id_value} for the kyc-service check", ErrKYCRequired)
	}
	body, err := json.Marshal(map[string]string{
		"tenant_id":     tenant.Slug,
		"subject_phone": strings.TrimSpace(req.KYC.SubjectPhone),
		"id_type":       strings.ToLower(strings.TrimSpace(req.KYC.IDType)),
		"id_value":      strings.TrimSpace(req.KYC.IDValue),
	})
	if err != nil {
		return nil, err
	}
	client := h.KYCHTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	url := strings.TrimRight(h.KYCURL, "/") + "/v1/kyc/resolve"
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		h.log().Warn("kyc-service call failed", zap.Error(err))
		return nil, fmt.Errorf("%w: kyc-service unreachable", ErrKYCRequired)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: kyc-service response unreadable", ErrKYCRequired)
	}
	if resp.StatusCode != http.StatusOK {
		h.log().Warn("kyc-service rejected the resolve", zap.Int("status", resp.StatusCode))
		return nil, fmt.Errorf("%w: kyc-service answered %d", ErrKYCRequired, resp.StatusCode)
	}
	var resolved kycResolveResponse
	if err := json.Unmarshal(raw, &resolved); err != nil {
		return nil, fmt.Errorf("%w: kyc-service response undecodable", ErrKYCRequired)
	}
	if resolved.Status != "verified" {
		return nil, fmt.Errorf("%w: kyc status %q (want verified)", ErrKYCRequired, resolved.Status)
	}
	return &KYCDecision{Mode: "service", Reference: resolved.Reference, Status: resolved.Status}, nil
}

// ---------------------------------------------------------------------------
// Disburse / repay / loan view / portfolio
// ---------------------------------------------------------------------------

// Disburse (POST /v1/lending/applications/{id}/disburse) — approved →
// disbursed: creates the loan account (interest = principal*bps/10000,
// fee, outstanding = principal+interest+fee, due_at = now+term_days),
// posts the ledger 500 journal, emits LoanDisbursed + the disbursement
// INTENT for the payments rail and meters loan_disbursed. Idempotent via
// the application status guard: a replay returns 200 with the existing
// loan account and {replayed: true} (no events, no metering, no intent —
// money movement is never re-intended).
func (h *Handlers) Disburse(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}
	res, err := h.Store.Disburse(r.Context(), tenant.ID, id, time.Now().UTC())
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if !res.Replayed {
		h.publishDisbursed(r.Context(), tenant.Slug, res)
		h.meterLoanDisbursed(r.Context(), tenant.Slug, res)
	}
	writeJSON(w, http.StatusOK, res)
}

// repayRequest is the POST /v1/lending/loans/{id}/repay body. ref_id is
// the caller idempotency key (REQUIRED): a replay answers 200 with the
// same stored body.
type repayRequest struct {
	AmountKobo int64  `json:"amount_kobo"`
	RefID      string `json:"ref_id"`
}

// Repay (POST /v1/lending/loans/{id}/repay) — amount clamped to
// outstanding (overpay noted via {clamped: true}, never recorded);
// outstanding == 0 flips loan + application to repaid and fires
// LoanRepaid; ledger 501 journal on the non-idempotent path.
func (h *Handlers) Repay(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid loan id")
		return
	}
	var req repayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := ValidateRepaymentInput(req.AmountKobo, req.RefID); err != nil {
		h.mapErr(w, err)
		return
	}
	res, err := h.Store.Repay(r.Context(), tenant.ID, id, req.AmountKobo, strings.TrimSpace(req.RefID))
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if !res.Replayed && res.LoanRepaid {
		h.publishRepaid(r.Context(), tenant.Slug, res)
	}
	writeJSON(w, http.StatusOK, res)
}

// ListLoans (GET /v1/lending/loans?status=&application_id=&contact_id=) —
// the book browser; application_id resolves the loan of one application
// (the UI's "view loan" link from a disbursed application row).
func (h *Handlers) ListLoans(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var f LoanFilters
	if s := q.Get("status"); s != "" {
		switch s {
		case LoanActive, LoanRepaid, LoanDefaulted:
			f.Status = s
		default:
			writeError(w, http.StatusBadRequest, "invalid status filter (want active|repaid|defaulted)")
			return
		}
	}
	if a := q.Get("application_id"); a != "" {
		id, err := uuid.Parse(a)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid application_id filter")
			return
		}
		f.ApplicationID = &id
	}
	if c := q.Get("contact_id"); c != "" {
		id, err := uuid.Parse(c)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid contact_id filter")
			return
		}
		f.ContactID = &id
	}
	loans, err := h.Store.ListLoans(r.Context(), tenant.ID, f)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"loans": loans})
}

// GetLoan (GET /v1/lending/loans/{id}) — the schedule view: loan account
// (principal/interest/fee/outstanding/due/status), its application summary
// and the repayment history.
func (h *Handlers) GetLoan(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid loan id")
		return
	}
	loan, err := h.Store.GetLoan(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	repayments, err := h.Store.ListRepayments(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	var app *Application
	if a, err := h.Store.GetApplication(r.Context(), tenant.ID, loan.ApplicationID); err == nil {
		app = &a
	}
	now := time.Now().UTC()
	writeJSON(w, http.StatusOK, map[string]any{
		"loan":          loan,
		"application":   app,
		"repayments":    repayments,
		"total_kobo":    loan.TotalKobo(),
		"days_past_due": loan.DaysPastDue(now),
	})
}

// Portfolio (GET /v1/lending/portfolio) — total outstanding, status counts
// and PAR30 (see the Portfolio doc).
func (h *Handlers) Portfolio(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	p, err := h.Store.Portfolio(r.Context(), tenant.ID, time.Now().UTC())
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"portfolio": p})
}
