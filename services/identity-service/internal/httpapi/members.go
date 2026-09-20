package httpapi

// SPEC-W45 K16 member lifecycle: removal and role change of tenant members.
//
// Both endpoints are Permify admin-gated (manage_catalog — owner|admin) and
// OWNER-ONLY for the owner role: removing an owner, or changing the role of
// an owner (or TO owner), requires the caller to hold the owner relation —
// an admin must not demote/remove owners or mint new ones.
//
// Removal is a DISABLE, not an account deletion: the Keycloak user is
// disabled and its sessions revoked (audit history survives), the Permify
// relationship is unlinked and the membership row removed. A
// MemberRemoved / MemberRoleChanged CloudEvent is published on
// opendesk.identity.events in both paths.

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/opendesk/identity-service/internal/events"
	"github.com/opendesk/identity-service/internal/store"
	"go.uber.org/zap"
)

// permifyRelation maps a membership role to the Permify organization
// relation (staff -> member; the rest are same-named).
func permifyRelation(role string) string {
	if role == "staff" {
		return "member"
	}
	return role
}

// requireOwnerForOwnerRole enforces the owner-only guard: when the TARGET
// membership is owner (removal/demotion) or the NEW role is owner
// (promotion), the caller must hold the owner relation on the organization.
// Writes the error response and returns false on denial/error.
func (s *server) requireOwnerForOwnerRole(w http.ResponseWriter, r *http.Request, t store.Tenant, caller caller) bool {
	owner, err := s.d.Permify.Check(r.Context(), t.ID.String(),
		"user:"+caller.Subject, "owner", "organization:"+t.ID.String())
	if err != nil {
		s.d.Logger.Error("permify owner check failed", zap.Error(err))
		writeError(w, http.StatusBadGateway, "authorization service error")
		return false
	}
	if !owner {
		writeError(w, http.StatusForbidden, "only an owner can remove or change the role of an owner")
		return false
	}
	return true
}

// removeMember handles DELETE /v1/tenants/{slug}/members/{user_id} (K16).
func (s *server) removeMember(w http.ResponseWriter, r *http.Request) {
	t, err := s.tenant(w, r)
	if err != nil {
		return
	}
	c, ok := s.requireOrgAccess(w, r, t, "manage_catalog")
	if !ok {
		return
	}
	userID := chi.URLParam(r, "user_id")
	m, err := s.d.Store.GetMember(r.Context(), t.ID, userID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "member not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if m.Role == "owner" && !s.requireOwnerForOwnerRole(w, r, t, c) {
		return
	}

	// IdP first (best-effort, surfaced): disable + session revocation. The
	// membership/Permify cleanup below still runs — a disabled Keycloak
	// account without a membership cannot act anyway, and the platform must
	// not keep an active IdP session when the DB removal fails later.
	var warnings []string
	if err := s.d.Keycloak.DisableUser(r.Context(), userID); err != nil {
		s.d.Logger.Error("keycloak disable user failed (member removal continues)",
			zap.String("user_id", userID), zap.Error(err))
		warnings = append(warnings, "keycloak disable failed: "+err.Error())
	}
	if err := s.d.Keycloak.LogoutUserSessions(r.Context(), userID); err != nil {
		s.d.Logger.Error("keycloak session revocation failed (member removal continues)",
			zap.String("user_id", userID), zap.Error(err))
		warnings = append(warnings, "keycloak logout failed: "+err.Error())
	}
	if err := s.d.Permify.DeleteRelationship(r.Context(), t.ID.String(),
		"organization:"+t.ID.String(), permifyRelation(m.Role), "user:"+userID); err != nil {
		s.d.Logger.Error("permify relationship unlink failed (member removal continues)",
			zap.String("user_id", userID), zap.Error(err))
		warnings = append(warnings, "permify unlink failed: "+err.Error())
	}
	if err := s.d.Store.RemoveMember(r.Context(), t.ID, userID); err != nil {
		s.internal(w, err)
		return
	}

	evt := events.New("identity-service", "com.opendesk.identity.MemberRemoved", t.Slug, t.ID.String(), map[string]any{
		"tenant_slug": t.Slug,
		"tenant_id":   t.ID.String(),
		"user_id":     userID,
		"role":        m.Role,
		"removed_by":  c.Subject,
		"removed_at":  time.Now().UTC().Format(time.RFC3339),
	})
	if err := s.d.Dapr.PublishEvent(r.Context(), s.d.PubSub, s.d.Topic, evt); err != nil {
		s.d.Logger.Error("failed to publish MemberRemoved", zap.Error(err))
		warnings = append(warnings, "MemberRemoved publish failed: "+err.Error())
	}

	resp := map[string]any{"removed": userID, "tenant_slug": t.Slug}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	writeJSON(w, http.StatusOK, resp)
}

type updateMemberRoleRequest struct {
	Role string `json:"role"`
}

// updateMemberRole handles PATCH /v1/tenants/{slug}/members/{user_id} (K16).
// Body: {"role": "admin|staff|viewer|owner"}.
func (s *server) updateMemberRole(w http.ResponseWriter, r *http.Request) {
	t, err := s.tenant(w, r)
	if err != nil {
		return
	}
	c, ok := s.requireOrgAccess(w, r, t, "manage_catalog")
	if !ok {
		return
	}
	userID := chi.URLParam(r, "user_id")
	var req updateMemberRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !memberRoles[req.Role] {
		writeError(w, http.StatusBadRequest, "role must be owner|admin|staff|viewer")
		return
	}
	m, err := s.d.Store.GetMember(r.Context(), t.ID, userID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "member not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if req.Role == m.Role {
		writeJSON(w, http.StatusOK, map[string]any{"user_id": userID, "role": m.Role, "changed": false})
		return
	}
	// Owner-only guard both ways: demoting/removing the owner role and
	// promoting TO owner.
	if (m.Role == "owner" || req.Role == "owner") && !s.requireOwnerForOwnerRole(w, r, t, c) {
		return
	}

	if err := s.d.Store.AddMember(r.Context(), store.Membership{
		TenantID: t.ID, UserID: userID, Role: req.Role,
	}); err != nil {
		s.internal(w, err)
		return
	}

	// Permify: unlink the old relation, write the new one (best-effort,
	// surfaced — authorization must not silently drift from the DB role).
	var warnings []string
	if err := s.d.Permify.DeleteRelationship(r.Context(), t.ID.String(),
		"organization:"+t.ID.String(), permifyRelation(m.Role), "user:"+userID); err != nil {
		s.d.Logger.Error("permify old-relation unlink failed",
			zap.String("user_id", userID), zap.Error(err))
		warnings = append(warnings, "permify unlink failed: "+err.Error())
	}
	if err := s.d.Permify.WriteRelationship(r.Context(), t.ID.String(),
		"organization:"+t.ID.String(), permifyRelation(req.Role), "user:"+userID); err != nil {
		s.d.Logger.Error("permify new-relation write failed",
			zap.String("user_id", userID), zap.Error(err))
		warnings = append(warnings, "permify write failed: "+err.Error())
	}
	// Keycloak role-mappings mirror (STK O4): revoke the old realm role,
	// grant the new one (both fail-soft — Permify is the authz truth).
	for _, old := range realmRoleNames(m.Role) {
		if err := s.d.Keycloak.RemoveRealmRole(r.Context(), userID, old); err != nil {
			s.d.Logger.Warn("old realm role removal deferred",
				zap.String("user_id", userID), zap.String("role", old), zap.Error(err))
		}
	}
	for _, newRole := range realmRoleNames(req.Role) {
		if err := s.d.Keycloak.AssignRealmRole(r.Context(), userID, newRole); err != nil {
			s.d.Logger.Warn("new realm role assignment deferred",
				zap.String("user_id", userID), zap.String("role", newRole), zap.Error(err))
		}
	}

	evt := events.New("identity-service", "com.opendesk.identity.MemberRoleChanged", t.Slug, t.ID.String(), map[string]any{
		"tenant_slug": t.Slug,
		"tenant_id":   t.ID.String(),
		"user_id":     userID,
		"old_role":    m.Role,
		"new_role":    req.Role,
		"changed_by":  c.Subject,
		"changed_at":  time.Now().UTC().Format(time.RFC3339),
	})
	if err := s.d.Dapr.PublishEvent(r.Context(), s.d.PubSub, s.d.Topic, evt); err != nil {
		s.d.Logger.Error("failed to publish MemberRoleChanged", zap.Error(err))
		warnings = append(warnings, "MemberRoleChanged publish failed: "+err.Error())
	}

	resp := map[string]any{"user_id": userID, "role": req.Role, "old_role": m.Role, "changed": true}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	writeJSON(w, http.StatusOK, resp)
}
