package fieldcapture

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/opendesk/booking-service/internal/bookingops"
	"go.uber.org/zap"
)

// Field capture HTTP API (SPEC-W16 Agent B, contract §4). The route
// (POST /v1/field/capture, manage_bookings) is wired in httpapi/server.go;
// the tenant context is injected by httpapi's tenant middleware and passed
// explicitly (same pattern as geo.Handlers / devices.Handlers).
type Handlers struct {
	Svc *Service
	// BatchLimit caps items per request (FIELD_CAPTURE_BATCH_LIMIT, default
	// 100 — one offline flush stays well inside the 60s server timeout).
	BatchLimit int
	// OCR serves POST /v1/field/captures/{id}/receipt (SPEC-W53 Scope B:
	// receipt/invoice OCR via the ocr-service). Nil/disabled → the receipt
	// endpoint fails closed with 503 (never silently open).
	OCR *OCRClient
	Log *zap.Logger
}

func (h *Handlers) log() *zap.Logger {
	if h.Log != nil {
		return h.Log
	}
	return zap.NewNop()
}

// captureRequest is the POST /v1/field/capture body: one offline-queue
// flush. Items are applied in array order.
type captureRequest struct {
	Items []CaptureItem `json:"items"`
}

// captureResponse is the per-item outcome array; HTTP stays 200 as long as
// the batch itself was processable — per-item status carries the detail.
type captureResponse struct {
	Results []ItemResult `json:"results"`
}

// Capture (POST /v1/field/capture) applies one offline-queue flush.
// Idempotent per item on field_capture:{client_id} (contract §4).
func (h *Handlers) Capture(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	var req captureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "items is required (non-empty array)")
		return
	}
	limit := h.BatchLimit
	if limit <= 0 {
		limit = 100
	}
	if len(req.Items) > limit {
		writeError(w, http.StatusBadRequest, "too many items (max per batch)")
		return
	}
	results := h.Svc.CaptureWithCaller(r.Context(), tenant, req.Items, callerEmail(r))
	writeJSON(w, http.StatusOK, captureResponse{Results: results})
}

// callerEmail resolves the caller's email identity at the handler boundary
// (SPEC-W57 Scope A, P3 — the same mechanism as GET /v1/bookings?mine=true
// at httpapi/bookings.go): the JWT email claim wins, X-User-Email is the
// trusted-internal/test fallback. The token was verified upstream (APISIX /
// tenant middleware); inside the cluster we parse the payload only — the
// same trust posture as httpapi.parseBearerClaims. Any decode failure is
// error-closed (""), which simply skips team-member resolution. NOTE: the
// ctxUser context key is unexported in httpapi and unreadable here; the
// JWT-claim + header paths cover the mine=true identity sources.
func callerEmail(r *http.Request) string {
	const prefix = "Bearer "
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, prefix) {
		parts := strings.Split(strings.TrimPrefix(h, prefix), ".")
		if len(parts) == 3 {
			if payload, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
				var claims struct {
					Email string `json:"email"`
				}
				if json.Unmarshal(payload, &claims) == nil && claims.Email != "" {
					return claims.Email
				}
			}
		}
	}
	return strings.TrimSpace(r.Header.Get("X-User-Email"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
