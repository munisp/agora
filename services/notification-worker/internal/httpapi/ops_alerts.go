package httpapi

// SPEC-W44 K3 / F15-04: ops-alerts read-back. The opsalerts consumer
// persists opendesk.ops.alerts CloudEvents; GET /v1/ops-alerts exposes them
// to operators. The route is mounted behind requireDNDAdmin (server.go) —
// ops alerts can cross tenants, so the gate is the platform admin role set
// (DND_ADMIN_ROLES, default "platform-admin"). An optional ?tenant=<slug>
// filter is BOUND to the X-Tenant-Slugs membership list (K1 binding); the
// result limit is capped at 500.

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/opendesk/notification-worker/internal/store"
	"go.uber.org/zap"
)

// OpsAlertStore is the persistence slice of the ops-alerts handler
// (*store.Store satisfies it; tests use a fake).
type OpsAlertStore interface {
	ListOpsAlerts(ctx context.Context, tenantID string, limit int) ([]store.OpsAlert, error)
}

// listOpsAlerts serves GET /v1/ops-alerts[?tenant=<slug>][&limit=<n>].
func (s *Server) listOpsAlerts(w http.ResponseWriter, r *http.Request) {
	if s.OpsAlerts == nil {
		http.Error(w, `{"error":"ops alerts not configured"}`, http.StatusServiceUnavailable)
		return
	}
	tenant := strings.TrimSpace(r.URL.Query().Get("tenant"))
	if tenant != "" && !s.bindTenantSlug(r, tenant) {
		s.Log.Warn("ops-alerts tenant binding rejected", zap.String("tenant", tenant))
		http.Error(w, `{"error":"tenant is not bound to the caller"}`, http.StatusForbidden)
		return
	}
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			http.Error(w, `{"error":"limit must be a positive integer"}`, http.StatusBadRequest)
			return
		}
		if n > 500 {
			n = 500
		}
		limit = n
	}
	alerts, err := s.OpsAlerts.ListOpsAlerts(r.Context(), tenant, limit)
	if err != nil {
		s.Log.Error("list ops alerts", zap.Error(err))
		http.Error(w, `{"error":"failed to list ops alerts"}`, http.StatusInternalServerError)
		return
	}
	if alerts == nil {
		alerts = []store.OpsAlert{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": alerts})
}
