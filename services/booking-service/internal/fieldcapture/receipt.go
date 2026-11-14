package fieldcapture

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/opendesk/booking-service/internal/bookingops"
	"go.uber.org/zap"
)

// Receipt OCR upload (SPEC-W53 Scope B):
// POST /v1/field/captures/{id}/receipt — {id} is the capture anchor's
// client_id (the field_captures PK half the field PWA already knows). The
// route itself (manage_bookings, same perm as POST /v1/field/capture) is
// wired in httpapi/server.go — the W53 wiring note documents the exact
// diff; the handler follows the devices.Handlers pattern (tenant injected
// explicitly, path param read via chi.URLParam).
//
// Request: raw receipt bytes with a Content-Type of image/jpeg, image/png,
// image/webp or application/pdf; hard cap 25 MB; magic bytes must match the
// declared mime (client-side validation mirrors the ocr-service's).
//
// Outcomes:
//   - 200: the ocr-service extract response verbatim ({document_id, status,
//     extracted_fields, page_count}); the capture row now carries
//     receipt_document_id + receipt_fields.
//   - 404: no capture anchor for (tenant, {id}) — checked BEFORE the OCR
//     call so an unknown id never spends an extraction.
//   - 413: body over the 25 MB cap.
//   - 415: mime not allow-listed / magic-byte mismatch.
//   - 502 {"code":"OCR_UNAVAILABLE"}: ocr-service down/error/non-2xx —
//     fail-open for the capture (the row is NEVER modified or lost), the
//     receipt attach itself fails closed (nothing half-stored).
//   - 503: OCR_SERVICE_URL / OCR_INTERNAL_TOKEN unset (fail-closed mirror
//     of the kyc-service internauth posture).

// Receipt serves POST /v1/field/captures/{id}/receipt.
func (h *Handlers) Receipt(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	clientID := strings.TrimSpace(chi.URLParam(r, "id"))
	if clientID == "" || len(clientID) > maxClientIDLen {
		writeError(w, http.StatusBadRequest, "invalid capture id")
		return
	}
	if h.OCR == nil || !h.OCR.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "receipt OCR not configured (OCR_SERVICE_URL unset)")
		return
	}
	if !h.OCR.Configured() {
		h.log().Error("OCR_INTERNAL_TOKEN unset — refusing receipt upload (fail-closed, kyc internauth mirror)",
			zap.String("path", r.URL.Path))
		writeError(w, http.StatusServiceUnavailable, "receipt OCR not configured (OCR_INTERNAL_TOKEN unset)")
		return
	}
	if h.Svc == nil || h.Svc.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "field capture unavailable")
		return
	}

	// The capture anchor must exist before any byte is forwarded — an OCR
	// extraction is CPU-expensive PII processing, never spent on a 404.
	exists, err := h.Svc.Store.CaptureExists(r.Context(), tenant.ID, clientID)
	if err != nil {
		h.log().Error("receipt capture lookup failed",
			zap.String("tenant_id", tenant.ID.String()), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "capture not found")
		return
	}

	// Hard byte cap (SPEC-W53: 25 MB) — MaxBytesReader trips at cap+1.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxReceiptBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "receipt exceeds the 25 MB cap")
		return
	}
	mime := r.Header.Get("Content-Type")
	if err := ValidateReceiptMime(mime, body); err != nil {
		writeError(w, http.StatusUnsupportedMediaType, err.Error())
		return
	}

	res, err := h.OCR.ExtractReceipt(r.Context(), tenant.ID, mime, body)
	if err != nil {
		switch {
		case errors.Is(err, errOCRNotConfigured):
			writeError(w, http.StatusServiceUnavailable, "receipt OCR not configured")
		case errors.Is(err, ErrReceiptMedia):
			writeError(w, http.StatusUnsupportedMediaType, err.Error())
		default:
			// ErrOCRUnavailable: fail-open for the capture row (untouched),
			// fail-closed for the attach (nothing half-stored).
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error": "ocr extraction failed", "code": "OCR_UNAVAILABLE",
			})
		}
		return
	}

	// SPEC-W64 Scope 3: the attach also writes the ReceiptIngested outbox
	// row in the same tx when Svc.EventsTopic is set (empty disables).
	if err := h.Svc.Store.AttachReceipt(r.Context(), tenant.ID, clientID, res.DocumentID, res.ExtractedFields, h.Svc.EventsTopic); err != nil {
		if errors.Is(err, ErrNotFound) {
			// Race: the anchor vanished between the existence check and the
			// attach. The OCR document is orphaned at A (documented cost of
			// the no-transactional-boundary split) — reported honestly.
			writeError(w, http.StatusNotFound, "capture not found")
			return
		}
		h.log().Error("receipt attach persist failed; ocr document orphaned at ocr-service",
			zap.String("tenant_id", tenant.ID.String()),
			zap.String("document_id", res.DocumentID.String()), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, res)
}
