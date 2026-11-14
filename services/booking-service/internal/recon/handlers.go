package recon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"go.uber.org/zap"
)

// Receipt-recon HTTP API (SPEC-W56 Scope A item 6). Routes are wired
// centrally in httpapi/server.go (W53 lesson: no self-registration):
//
//	GET  /v1/recon/receipts?status=open|resolved|all&limit=  — manage_bookings
//	POST /v1/recon/receipts/{id}/resolve                     — manage_bookings (SPEC-W64)
//	POST /v1/internal/recon/receipts                         — X-Internal-Token
//
// The tenant context is injected by httpapi's tenant middleware and passed
// explicitly (same pattern as fieldcapture.Handlers).
type Handlers struct {
	Svc *Service
	Log *zap.Logger
	// Timeout (optional, tests) overrides triggerTimeout for the manual
	// trigger; zero keeps the pinned 90s. A NEGATIVE value yields an
	// already-expired sweep context — the deterministic timeout test hook
	// (SPEC-W66 WS1-B).
	Timeout time.Duration
}

func (h *Handlers) timeout() time.Duration {
	if h.Timeout != 0 {
		return h.Timeout
	}
	return triggerTimeout
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

// Resolve serves POST /v1/recon/receipts/{id}/resolve (SPEC-W64): marks one
// finding resolved and emits com.opendesk.booking.ReceiptReconResolved in
// the same transaction. resolved_by is the caller identity claim resolved
// by httpapi's tenant middleware (payment_receipt P3 server-resolved
// precedent) — NEVER read from the request body. Idempotent: an unknown or
// already-resolved id answers 404 (not_found_or_resolved — the two cases
// are deliberately indistinguishable) with no event emitted.
func (h *Handlers) Resolve(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo, actor string) {
	if h.Svc == nil || h.Svc.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "receipt recon unavailable")
		return
	}
	findingID, err := uuid.Parse(strings.TrimSpace(chi.URLParam(r, "id")))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid finding id")
		return
	}
	f, err := h.Svc.ResolveFinding(r.Context(), tenant.ID, findingID, actor)
	if errors.Is(err, ErrFindingNotFoundOrResolved) {
		writeError(w, http.StatusNotFound, "finding not found or already resolved")
		return
	}
	if err != nil {
		h.log().Error("receipt recon resolve failed",
			zap.String("tenant_id", tenant.ID.String()),
			zap.String("finding_id", findingID.String()), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"finding": f})
}

// Trigger serves POST /v1/internal/recon/receipts (pinned decision 6: the
// manual half of the trigger). The route is internauth-guarded in
// httpapi/server.go (X-Internal-Token vs BOOKING_INTERNAL_TOKEN,
// constant-time, 503 fail-closed when unset, 401 on mismatch — K2 idiom);
// this handler runs one sweep synchronously and returns its summary.
//
// SPEC-W66 WS1-B: the body MAY be a JSON object with optional
// {since?: RFC3339, lookback_days?: int, tenant_id?: uuid}. An EMPTY body
// (or {}) is the pre-W66 behavior byte-identically: the configured trailing
// window over every receipt-bearing tenant, 200 + bare Summary.
//
// Pinned rules:
//   - since must be RFC3339; lookback_days an integer > 0; tenant_id a UUID;
//     unknown keys are rejected. Any violation answers 400 with a clear
//     error and NOTHING is swept.
//   - When BOTH since and lookback_days are present, SINCE WINS (the
//     explicit timestamp is the more precise operator intent).
//   - tenant_id scopes the sweep to that tenant; the pinned-4 gate still
//     applies (a tenant with no receipt captures in the window sweeps
//     nothing — tenants_scanned=0).
//   - If the sweep exceeds triggerTimeout the response is 202 Accepted with
//     the PARTIAL summary (partial:true added; counts cover fully-swept
//     tenants plus whatever the interrupted tenant committed) — never a
//     silent 504. Re-calling the endpoint is the resumability story: the
//     dedupe index makes the follow-up run converge with zero duplicates.
func (h *Handlers) Trigger(w http.ResponseWriter, r *http.Request) {
	if h.Svc == nil || h.Svc.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "receipt recon unavailable")
		return
	}
	req, err := parseTriggerRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout())
	defer cancel()
	sum, err := h.Svc.SweepOnceSince(ctx, req.since, req.tenantID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			h.log().Warn("manual receipt recon sweep hit trigger timeout — partial progress",
				zap.String("run_id", sum.RunID.String()),
				zap.Int("tenants_scanned", sum.TenantsScanned),
				zap.Int("findings_new", sum.FindingsNew))
			writeJSON(w, http.StatusAccepted, triggerPartialResponse{Summary: sum, Partial: true})
			return
		}
		h.log().Error("manual receipt recon sweep failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "sweep failed")
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// triggerPartialResponse is the 202 body when the sweep exceeds
// triggerTimeout: the normal Summary inlined plus partial:true. A completed
// sweep answers 200 with the bare Summary (NO partial key) so the
// empty-body response shape stays byte-identical to pre-W66.
type triggerPartialResponse struct {
	Summary
	Partial bool `json:"partial"`
}

// triggerRequest is the parsed + validated POST body. A zero since / nil
// tenantID means "use the configured default" (the pre-W66 empty-body
// behavior — see SweepOnceSince).
type triggerRequest struct {
	since    time.Time
	tenantID *uuid.UUID
}

// parseTriggerRequest decodes and validates the optional trigger body.
// io.EOF (empty body) is valid and yields the zero request; anything else
// malformed is a 400 with a precise reason.
func parseTriggerRequest(r *http.Request) (triggerRequest, error) {
	var out triggerRequest
	if r.Body == nil {
		return out, nil
	}
	var raw struct {
		Since        *string `json:"since"`
		LookbackDays *int    `json:"lookback_days"`
		TenantID     *string `json:"tenant_id"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return out, nil // empty body = configured-window behavior
		}
		return out, fmt.Errorf("body must be a JSON object with optional since, lookback_days, tenant_id: %v", err)
	}
	// Pinned rule: when BOTH since and lookback_days are present, since wins.
	if raw.Since != nil && strings.TrimSpace(*raw.Since) != "" {
		since, err := time.Parse(time.RFC3339, strings.TrimSpace(*raw.Since))
		if err != nil {
			return out, fmt.Errorf("since must be an RFC3339 timestamp (got %q)", *raw.Since)
		}
		out.since = since.UTC()
	} else if raw.LookbackDays != nil {
		if *raw.LookbackDays <= 0 {
			return out, fmt.Errorf("lookback_days must be a positive integer (got %d)", *raw.LookbackDays)
		}
		out.since = time.Now().UTC().Add(-time.Duration(*raw.LookbackDays) * 24 * time.Hour)
	}
	if raw.TenantID != nil && strings.TrimSpace(*raw.TenantID) != "" {
		id, err := uuid.Parse(strings.TrimSpace(*raw.TenantID))
		if err != nil {
			return out, fmt.Errorf("tenant_id must be a UUID (got %q)", *raw.TenantID)
		}
		out.tenantID = &id
	}
	return out, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
