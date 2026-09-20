package httpapi

// SPEC-W45 K18 plan enforcement v1: member-count limits per plan, enforced
// at invite time, plus the owner/platform-admin plan-change endpoint.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/opendesk/identity-service/internal/events"
	"github.com/opendesk/identity-service/internal/store"
	"go.uber.org/zap"
)

// planMemberLimits is the v1 member cap per plan. Plans absent from the map
// (scale, enterprise, twin) are unlimited. The map is deliberately in code
// (not config) so the gate cannot be weakened by an env typo; plan_defaults
// changes ship with a deploy.
var planMemberLimits = map[string]int{
	"free": 3,
	"pro":  20,
	// scale / enterprise / twin: unlimited (not listed).
}

// memberLimitExceeded reports whether inviting ONE more member would exceed
// the tenant's plan cap, and the cap for the error message (0 = unlimited).
// An empty/unknown plan fails SAFE to the free cap (the DB CHECK keeps this
// theoretical; data drift must not silently unlock unlimited seats).
func memberLimitExceeded(plan string, current int) (bool, int) {
	if plan == "scale" || plan == "enterprise" || plan == "twin" {
		return false, 0
	}
	limit, capped := planMemberLimits[plan]
	if !capped {
		limit = planMemberLimits["free"]
	}
	return current >= limit, limit
}

// enforceMemberLimit applies the K18 plan gate in inviteMember. Writes the
// 403 upgrade response and returns a non-nil error when the cap is reached.
func (s *server) enforceMemberLimit(w http.ResponseWriter, r *http.Request, t store.Tenant) error {
	count, err := s.d.Store.CountMembers(r.Context(), t.ID)
	if err != nil {
		s.internal(w, err)
		return err
	}
	if exceeded, limit := memberLimitExceeded(t.Plan, count); exceeded {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "member limit reached for plan \"" + t.Plan + "\" (" + strconv.Itoa(limit) + " members) — upgrade the plan to invite more members",
			"plan":    t.Plan,
			"limit":   limit,
			"upgrade": true,
		})
		return errors.New("plan member limit reached")
	}
	return nil
}

type updatePlanRequest struct {
	Plan string `json:"plan"`
}

// updatePlan handles PATCH /v1/tenants/{slug}/plan (K18): the caller must be
// a platform-admin OR hold the owner relation on the organization (a plain
// admin must not change the commercial plan). The change is audit-logged and
// publishes TenantPlanChanged on opendesk.identity.events (billing-engine /
// notification consumers).
func (s *server) updatePlan(w http.ResponseWriter, r *http.Request) {
	t, err := s.tenant(w, r)
	if err != nil {
		return
	}
	c, err := resolveCaller(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "malformed bearer token")
		return
	}
	if c.Subject == "" {
		writeError(w, http.StatusUnauthorized, "authenticated subject required (JWT sub or X-User-Id)")
		return
	}
	var req updatePlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// The public plan set (validPlans) — 'twin' stays internal-only and the
	// DB CHECK is the last line of defence either way.
	if !validPlans[req.Plan] {
		writeError(w, http.StatusBadRequest, "plan must be free|pro|enterprise")
		return
	}
	if !s.isPlatformAdmin(c) {
		owner, err := s.d.Permify.Check(r.Context(), t.ID.String(),
			"user:"+c.Subject, "owner", "organization:"+t.ID.String())
		if err != nil {
			s.d.Logger.Error("permify owner check failed", zap.Error(err))
			writeError(w, http.StatusBadGateway, "authorization service error")
			return
		}
		if !owner {
			writeError(w, http.StatusForbidden, "only a tenant owner or platform-admin can change the plan")
			return
		}
	}
	if req.Plan == t.Plan {
		writeJSON(w, http.StatusOK, map[string]any{"slug": t.Slug, "plan": t.Plan, "changed": false})
		return
	}
	if err := s.d.Store.SetTenantPlan(r.Context(), t.Slug, req.Plan); err != nil {
		s.internal(w, err)
		return
	}
	// Audit trail (K18): plan changes are commercial events — always logged
	// at Info with actor + old/new, independent of event-bus delivery.
	s.d.Logger.Info("AUDIT tenant plan changed",
		zap.String("tenant_slug", t.Slug),
		zap.String("tenant_id", t.ID.String()),
		zap.String("old_plan", t.Plan),
		zap.String("new_plan", req.Plan),
		zap.String("actor", c.Subject))

	evt := events.New("identity-service", "com.opendesk.identity.TenantPlanChanged", t.Slug, t.ID.String(), map[string]any{
		"tenant_slug": t.Slug,
		"tenant_id":   t.ID.String(),
		"old_plan":    t.Plan,
		"new_plan":    req.Plan,
		"actor":       c.Subject,
		"changed_at":  time.Now().UTC().Format(time.RFC3339),
	})
	if err := s.d.Dapr.PublishEvent(r.Context(), s.d.PubSub, s.d.Topic, evt); err != nil {
		s.d.Logger.Error("failed to publish TenantPlanChanged", zap.Error(err))
	}

	// SPEC-W45 K contract note: best-effort plan push to billing-engine
	// (PUT {BILLING_URL}/v1/tenants/{uuid}/plan, X-Internal-Token). On
	// failure: log ERROR + surface a response warning — the identity change
	// is NEVER rolled back (the event above remains the recovery path).
	var warnings []string
	if s.d.Billing != nil {
		if err := s.d.Billing.PushPlan(r.Context(), t.ID.String(), req.Plan); err != nil {
			s.d.Logger.Error("billing plan push failed (identity plan change NOT rolled back)",
				zap.String("tenant_slug", t.Slug),
				zap.String("tenant_id", t.ID.String()),
				zap.String("new_plan", req.Plan),
				zap.Error(err))
			warnings = append(warnings, "billing plan push failed: "+err.Error())
		}
	}
	resp := map[string]any{
		"slug": t.Slug, "plan": req.Plan, "old_plan": t.Plan, "changed": true,
	}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	writeJSON(w, http.StatusOK, resp)
}
