package recon

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opendesk/booking-service/internal/bookingops"
	"go.uber.org/zap"
)

// Receipt-recon HTTP API (SPEC-W56 Scope A item 6). Routes are wired
// centrally in httpapi/server.go (W53 lesson: no self-registration):
//
//	GET  /v1/recon/receipts?status=open|resolved|all&limit=  — manage_bookings
//	POST /v1/internal/recon/receipts                         — X-Internal-Token
//
// The tenant context is injected by httpapi's tenant middleware and passed
// explicitly (same pattern as fieldcapture.Handlers).
type Handlers struct {
	Svc *Service
	Log *zap.Logger
}

func (h *Handlers) log() *zap.Logger {
	if h.Log != nil {
		return h.Log
	}
	return zap.NewNop()
}

// triggerTimeout bounds the manual sweep trigger: the POST runs the sweep
// synchronously (ops/test runs want the summary in the response) but must
// not hang a service-to-service call forever.
const triggerTimeout = 90 * time.Second

// maxListLimit caps GET /v1/recon/receipts page size (default 100).
const maxListLimit = 500

// ListFindings serves GET /v1/recon/receipts: findings for the tenant,
// newest first, each carrying the linked capture's receipt_fields (the
// first read path for receipt data — R4). status defaults to "open"
// (unresolved); limit defaults to 100, capped at maxListLimit.
func (h *Handlers) ListFindings(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	if h.Svc == nil || h.Svc.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "receipt recon unavailable")
		return
	}
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	switch status {
	case "", "open":
		status = "open"
	case "resolved", "all":
	default:
		writeError(w, http.StatusBadRequest, "status must be open|resolved|all")
		return
	}
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	findings, err := h.Svc.Store.ListFindings(r.Context(), tenant.ID, status, limit)
	if err != nil {
		h.log().Error("receipt recon list failed",
			zap.String("tenant_id", tenant.ID.String()), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"findings": findings,
		"count":    len(findings),
		"status":   status,
	})
}

// Trigger serves POST /v1/internal/recon/receipts (pinned decision 6: the
// manual half of the trigger). The route is internauth-guarded in
// httpapi/server.go (X-Internal-Token vs BOOKING_INTERNAL_TOKEN,
// constant-time, 503 fail-closed when unset, 401 on mismatch — K2 idiom);
// this handler runs one sweep synchronously and returns its summary.
func (h *Handlers) Trigger(w http.ResponseWriter, r *http.Request) {
	if h.Svc == nil || h.Svc.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "receipt recon unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), triggerTimeout)
	defer cancel()
	sum, err := h.Svc.SweepOnce(ctx)
	if err != nil {
		h.log().Error("manual receipt recon sweep failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "sweep failed")
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
