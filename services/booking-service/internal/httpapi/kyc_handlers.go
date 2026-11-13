package httpapi

// SPEC-W58 Scope C: the booking-service KYC document surface (P1 Option A,
// staff-facing leg of booking -> kyc-service -> ocr-service).
//
// Routes (wired in server.go inside the /v1 tenant group):
//
//	POST /v1/kyc/documents                 multipart upload passthrough
//	GET  /v1/kyc/documents/{id}            review read (fields via kyc)
//	POST /v1/kyc/documents/{id}/review     {decision, reason}
//
// Access scoping (P5, GATE R4): ALL three routes are owner/admin ONLY.
// manage_bookings = owner|admin|member (docs/security/roles.md:62-64 —
// field agents hold member), so the Permify perm is necessary but NOT
// sufficient; the realm-role gate (requireOwnerAdmin, mirroring the
// referrals admin-only precedent — referrals.go hasAdminRole +
// civic.CanViewReporterRole) rejects members with 403. No new authz
// machinery is invented: the gate reuses the existing rolesFrom context
// accessor (tenant middleware) and the existing civic.CanViewReporterRole
// owner/admin role predicate.
//
// View audit (P5/R8): every successful review GET emits a structured
// view-audit log line (actor, document_id, tenant_id) — the kyc_audit row
// is kyc-service's half; this line is the booking-side NDPA accountability
// minimum.
//
// Fail-closed posture (receipt precedent): a nil/unconfigured client
// answers 503 (never a silent open call); kyc-service transport failures
// answer 502 {code:"KYC_UNAVAILABLE"}; downstream domain statuses (400
// validation, 403 consent/K22 denial, 404, ...) pass through verbatim.

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/opendesk/booking-service/internal/civic"
	"github.com/opendesk/booking-service/internal/kycclient"
	"go.uber.org/zap"
)

// requireOwnerAdmin returns middleware enforcing the owner/admin realm-role
// gate (P5) on top of whatever Permify guard the route already carries.
// The roles are resolved by the tenant middleware (JWT realm_access.roles,
// X-User-Roles fallback); NO roles = DENIED (fail closed — the lending
// kycOverrideAllowed posture). Not AuthzDisabled-sensitive on purpose: the
// dev escape hatch relaxes Permify, never the KYC PII access scope.
func (s *server) requireOwnerAdmin() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !civic.CanViewReporterRole(rolesFrom(r.Context())) {
				s.d.Logger.Warn("kyc route rejected: caller lacks the owner/admin role (P5)",
					zap.String("caller", userFrom(r.Context())),
					zap.String("path", r.URL.Path))
				writeError(w, http.StatusForbidden, "kyc document access requires the owner or admin role")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// kycClientOr503 returns the configured client or writes the fail-closed
// answer (nil -> 503, unconfigured -> 503; receipt-handler precedent).
func (s *server) kycClientOr503(w http.ResponseWriter, r *http.Request) *kycclient.Client {
	c := s.d.KYC
	if c == nil || !c.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "kyc documents not configured (BOOKING_KYC_SERVICE_URL unset)")
		return nil
	}
	if !c.Configured() {
		s.d.Logger.Error("KYC_INTERNAL_TOKEN unset — refusing kyc document call (fail-closed, kyc internauth mirror)",
			zap.String("path", r.URL.Path))
		writeError(w, http.StatusServiceUnavailable, "kyc documents not configured (KYC_INTERNAL_TOKEN unset)")
		return nil
	}
	return c
}

// kycUploadDocument serves POST /v1/kyc/documents — multipart passthrough
// (file + subject_phone + doc_type + optional consent_id) to kyc-service
// with the 25 MB cap + mime pre-check applied BEFORE any byte is forwarded
// (receipt precedent: validation is never spent downstream on bytes
// booking would itself reject).
func (s *server) kycUploadDocument(w http.ResponseWriter, r *http.Request) {
	client := s.kycClientOr503(w, r)
	if client == nil {
		return
	}
	// Hard byte cap (SPEC-W58: 25 MB) — MaxBytesReader trips at cap+1.
	r.Body = http.MaxBytesReader(w, r.Body, kycclient.MaxDocumentBytes)
	if err := r.ParseMultipartForm(kycclient.MaxDocumentBytes); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "document exceeds the 25 MB cap")
			return
		}
		writeError(w, http.StatusBadRequest, "multipart/form-data body required (file + subject_phone + doc_type)")
		return
	}
	subjectPhone := strings.TrimSpace(r.FormValue("subject_phone"))
	if subjectPhone == "" || len(subjectPhone) > 20 {
		writeError(w, http.StatusBadRequest, "subject_phone (E.164) is required")
		return
	}
	docType := strings.TrimSpace(r.FormValue("doc_type"))
	if !kycclient.IsDocType(docType) {
		writeError(w, http.StatusBadRequest, "doc_type must be one of kyc_nin_slip|kyc_drivers_license|kyc_passport|kyc_voters_card|kyc_cac_document")
		return
	}
	consentID := strings.TrimSpace(r.FormValue("consent_id")) // optional; recorded in kyc_audit by kyc-service

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "multipart file field is required")
		return
	}
	defer file.Close() //nolint:errcheck
	data, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "document exceeds the 25 MB cap")
		return
	}
	mime := header.Header.Get("Content-Type")
	if err := kycclient.ValidateMime(mime, data); err != nil {
		writeError(w, http.StatusUnsupportedMediaType, err.Error())
		return
	}

	res, err := client.UploadDocument(r.Context(), tenantFrom(r.Context()).ID, mime, data, subjectPhone, docType, consentID)
	if err != nil {
		s.kycClientError(w, r, err)
		return
	}
	writePassthrough(w, res)
}

// kycGetDocument serves GET /v1/kyc/documents/{id} — the review surface
// (hash-only record + TTL-bound fields; fields_available:false honestly
// after TTL expiry). Every served view emits the P5 view-audit log line.
func (s *server) kycGetDocument(w http.ResponseWriter, r *http.Request) {
	client := s.kycClientOr503(w, r)
	if client == nil {
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" || len(id) > 64 {
		writeError(w, http.StatusBadRequest, "invalid document id")
		return
	}
	res, err := client.GetDocument(r.Context(), tenantFrom(r.Context()).ID, id)
	if err != nil {
		s.kycClientError(w, r, err)
		return
	}
	if res.Status >= 200 && res.Status < 300 {
		// P5/R8 view audit: actor + document + tenant on every served
		// review view (NDPA accountability for PII reads).
		s.d.Logger.Info("kyc document view",
			zap.String("audit", "kyc_document_view"),
			zap.String("actor", userFrom(r.Context())),
			zap.String("document_id", id),
			zap.String("tenant_id", tenantFrom(r.Context()).ID.String()))
	}
	writePassthrough(w, res)
}

// kycReviewRequest is the POST /v1/kyc/documents/{id}/review body
// (cross-scope contract: decision approved|rejected + reason string).
type kycReviewRequest struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// kycReviewDocument serves POST /v1/kyc/documents/{id}/review — the
// human-review terminal action (P7: OCR is pre-fill only; the reviewer
// decision is the verdict). The decision enum is validated here (fail
// fast) and authoritatively by kyc-service (B2), which owns the audit row.
func (s *server) kycReviewDocument(w http.ResponseWriter, r *http.Request) {
	client := s.kycClientOr503(w, r)
	if client == nil {
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" || len(id) > 64 {
		writeError(w, http.StatusBadRequest, "invalid document id")
		return
	}
	var req kycReviewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Decision = strings.ToLower(strings.TrimSpace(req.Decision))
	if req.Decision != "approved" && req.Decision != "rejected" {
		writeError(w, http.StatusBadRequest, "decision must be approved|rejected")
		return
	}
	res, err := client.ReviewDocument(r.Context(), tenantFrom(r.Context()).ID, id, req.Decision, req.Reason)
	if err != nil {
		s.kycClientError(w, r, err)
		return
	}
	if res.Status >= 200 && res.Status < 300 {
		s.d.Logger.Info("kyc document review action",
			zap.String("audit", "kyc_document_review"),
			zap.String("actor", userFrom(r.Context())),
			zap.String("document_id", id),
			zap.String("tenant_id", tenantFrom(r.Context()).ID.String()),
			zap.String("decision", req.Decision))
	}
	writePassthrough(w, res)
}

// kycClientError maps client errors: ErrNotConfigured -> 503, ErrMedia ->
// 415, ErrDocType -> 400, everything else (ErrUnavailable) -> 502
// {code:"KYC_UNAVAILABLE"} (receipt OCR_UNAVAILABLE precedent).
func (s *server) kycClientError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, kycclient.ErrNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "kyc documents not configured")
	case errors.Is(err, kycclient.ErrMedia):
		writeError(w, http.StatusUnsupportedMediaType, err.Error())
	case errors.Is(err, kycclient.ErrDocType):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.d.Logger.Warn("kyc-service call failed (fail-closed 502)",
			zap.String("path", r.URL.Path), zap.Error(err))
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "kyc service call failed", "code": "KYC_UNAVAILABLE",
		})
	}
}

// writePassthrough forwards the downstream status + body verbatim. The
// kyc-service owns the envelope shape and domain statuses; booking is a
// faithful proxy (Content-Type defaults to JSON — every kyc-service
// response body is JSON by contract).
func writePassthrough(w http.ResponseWriter, res *kycclient.Response) {
	ct := "application/json"
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(res.Status)
	if len(res.Body) > 0 {
		_, _ = w.Write(res.Body)
	}
}
