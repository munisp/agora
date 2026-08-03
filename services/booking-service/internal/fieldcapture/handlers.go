package fieldcapture

import (
	"encoding/json"
	"net/http"

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
	Log        *zap.Logger
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
	results := h.Svc.Capture(r.Context(), tenant, req.Items)
	writeJSON(w, http.StatusOK, captureResponse{Results: results})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
