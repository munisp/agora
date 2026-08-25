package httpapi

// SPEC-W12 Agent B: DND registry REST (NCC 2442 global list + per-tenant
// opt-outs), backing the marketing-send suppression guard.
//
// Routes (registered in server.go):
//
//	POST   /v1/dnd/import       bulk-load the global NCC 2442 list. ADMIN
//	                            MUTATION (S1-F7-03): gated on
//	                            X-User-Roles ∩ DND_ADMIN_ROLES (default
//	                            "platform-admin") — the roles come from the
//	                            gateway-injected header ONLY (K1: APISIX
//	                            strips caller-sent x-* and re-injects from
//	                            the verified JWT; fail-closed when absent).
//	DELETE /v1/dnd/{phone}      opt-out honor: remove a number from the DND
//	                            registry. Without a tenant slug this is a
//	                            global delete (global + all tenant lists) and
//	                            carries the same admin-role gate as import;
//	                            with ?tenant= / X-Tenant-Slug only that
//	                            tenant's row is removed and the slug is BOUND
//	                            to the X-Tenant-Slugs membership list (C1/K1
//	                            binding, 403 on mismatch).
//	GET    /v1/dnd/check?phone= suppression lookup (+ optional tenant= slug)
//
// The handlers are thin; all matching/normalization semantics live in
// internal/store (dnd.go) and internal/pacer (guards.go).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// DNDStore is the persistence slice of the DND REST handlers (*store.Store
// satisfies it; tests use an in-memory fake).
type DNDStore interface {
	ImportGlobalDND(ctx context.Context, phones []string, source string) (int, error)
	RemoveDND(ctx context.Context, phone, tenantSlug string) (int64, error)
	IsSuppressed(ctx context.Context, tenantSlug, phone string) (bool, string, error)
}

type dndImportRequest struct {
	Phones []string `json:"phones"`
	// Source labels the imported rows (default "ncc2442"). Kept free-form so
	// future registry feeds (state lists, aggregator lists) stay
	// distinguishable.
	Source string `json:"source,omitempty"`
}

// importDND serves POST /v1/dnd/import. Idempotent: numbers already on the
// global list are skipped, so re-importing an updated registry snapshot is
// safe.
func (s *Server) importDND(w http.ResponseWriter, r *http.Request) {
	if s.DND == nil {
		http.Error(w, `{"error":"DND registry not configured"}`, http.StatusServiceUnavailable)
		return
	}
	var req dndImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	if len(req.Phones) == 0 {
		http.Error(w, `{"error":"phones must be a non-empty array"}`, http.StatusBadRequest)
		return
	}
	inserted, err := s.DND.ImportGlobalDND(r.Context(), req.Phones, strings.TrimSpace(req.Source))
	if err != nil {
		s.Log.Error("dnd import", zap.Error(err))
		http.Error(w, `{"error":"failed to import DND numbers"}`, http.StatusInternalServerError)
		return
	}
	s.Log.Info("dnd global list import", zap.Int("received", len(req.Phones)), zap.Int("inserted", inserted))
	writeJSON(w, http.StatusOK, map[string]any{"received": len(req.Phones), "imported": inserted})
}

// deleteDND serves DELETE /v1/dnd/{phone} (opt-out honor, S1-F7-03). With a
// tenant slug (X-Tenant-Slug header or ?tenant= query) only that tenant's
// opt-out row is removed and the slug is bound to X-Tenant-Slugs membership
// (403 on mismatch). WITHOUT a tenant slug the number is removed from the
// global list AND every tenant list (full re-consent) — a global mutation
// gated on X-User-Roles ∩ DND_ADMIN_ROLES (403 otherwise).
func (s *Server) deleteDND(w http.ResponseWriter, r *http.Request) {
	if s.DND == nil {
		http.Error(w, `{"error":"DND registry not configured"}`, http.StatusServiceUnavailable)
		return
	}
	phone := strings.TrimSpace(chi.URLParam(r, "phone"))
	if phone == "" {
		http.Error(w, `{"error":"phone is required"}`, http.StatusBadRequest)
		return
	}
	tenant := strings.TrimSpace(r.Header.Get("X-Tenant-Slug"))
	if tenant == "" {
		tenant = strings.TrimSpace(r.URL.Query().Get("tenant"))
	}
	if tenant == "" {
		// Global delete: admin-role gate (same set as /v1/dnd/import).
		if !s.hasAnyRole(r, s.dndAdminRoles()) {
			http.Error(w, `{"error":"admin role required for a global DND delete"}`, http.StatusForbidden)
			return
		}
	} else if !s.bindTenantSlug(r, tenant) {
		s.Log.Warn("dnd tenant binding rejected", zap.String("tenant", tenant))
		http.Error(w, `{"error":"tenant is not bound to the caller"}`, http.StatusForbidden)
		return
	}
	removed, err := s.DND.RemoveDND(r.Context(), phone, tenant)
	if err != nil {
		s.Log.Error("dnd remove", zap.String("phone", phone), zap.Error(err))
		http.Error(w, `{"error":"failed to remove DND number"}`, http.StatusInternalServerError)
		return
	}
	if removed == 0 {
		http.Error(w, `{"error":"number not on the DND registry"}`, http.StatusNotFound)
		return
	}
	s.Log.Info("dnd number removed (opt-out honored)",
		zap.String("phone", phone), zap.String("tenant", tenant), zap.Int64("removed", removed))
	writeJSON(w, http.StatusOK, map[string]any{"phone": phone, "removed": removed})
}

// checkDND serves GET /v1/dnd/check?phone=[&tenant=slug]: the same lookup
// order the send guard uses (per-tenant opt-out first, then the global NCC
// 2442 list).
func (s *Server) checkDND(w http.ResponseWriter, r *http.Request) {
	if s.DND == nil {
		http.Error(w, `{"error":"DND registry not configured"}`, http.StatusServiceUnavailable)
		return
	}
	phone := strings.TrimSpace(r.URL.Query().Get("phone"))
	if phone == "" {
		http.Error(w, `{"error":"phone query parameter is required"}`, http.StatusBadRequest)
		return
	}
	tenant := strings.TrimSpace(r.URL.Query().Get("tenant"))
	suppressed, reason, err := s.DND.IsSuppressed(r.Context(), tenant, phone)
	if err != nil {
		s.Log.Error("dnd check", zap.String("phone", phone), zap.Error(err))
		http.Error(w, `{"error":"failed to check DND registry"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"phone":      phone,
		"tenant":     tenant,
		"suppressed": suppressed,
		"reason":     reason,
	})
}
