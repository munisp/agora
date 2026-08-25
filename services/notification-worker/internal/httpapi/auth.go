package httpapi

// SPEC-W44 (contracts K1/K2): authentication/authorization helpers for the
// notification-worker HTTP sidecar.
//
// K2 INTERNAL TOKENS: every /internal/*-style machine surface (here:
// /v1/signals) requires the X-Internal-Token header, compared in constant
// time against NOTIFICATION_INTERNAL_TOKEN. Fail-closed: the token env unset
// answers 503 (misconfiguration is never an open door), a missing/wrong
// header answers 401.
//
// K1 HEADER CONTRACT: APISIX strips caller-sent x-user-roles / x-tenant-*
// headers on OIDC routes and re-injects X-User-Roles + X-Tenant-Slugs from
// the VERIFIED JWT only. Role and tenant checks therefore read ONLY those
// two headers, fail closed when they are absent, and offer a single
// explicit dev escape (OPENDESK_TRUST_DIRECT_TENANT=1, logged) for
// gateway-less local runs.

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"go.uber.org/zap"
)

// Gateway-injected header names (K1/K2).
const (
	HeaderInternalToken = "X-Internal-Token"
	HeaderUserRoles     = "X-User-Roles"
	HeaderTenantSlugs   = "X-Tenant-Slugs"
)

// DefaultDNDAdminRoles gates the DND registry mutations (and the ops-alerts
// read) when DND_ADMIN_ROLES is unset.
var DefaultDNDAdminRoles = []string{"platform-admin"}

// requireInternalToken is the K2 gate: X-Internal-Token must match the
// configured token (constant-time). 503 when unconfigured, 401 otherwise.
func (s *Server) requireInternalToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.InternalToken == "" {
			s.Log.Error("internal-token route hit but NOTIFICATION_INTERNAL_TOKEN is unset (fail-closed 503)",
				zap.String("path", r.URL.Path))
			http.Error(w, `{"error":"internal authentication not configured"}`, http.StatusServiceUnavailable)
			return
		}
		got := r.Header.Get(HeaderInternalToken)
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.InternalToken)) != 1 {
			http.Error(w, `{"error":"invalid internal token"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// dndAdminRoles resolves the configured admin role set (DND_ADMIN_ROLES),
// falling back to DefaultDNDAdminRoles.
func (s *Server) dndAdminRoles() []string {
	if len(s.DNDAdminRoles) > 0 {
		return s.DNDAdminRoles
	}
	return DefaultDNDAdminRoles
}

// parseCSV splits a comma-separated header value, trimming blanks.
func parseCSV(v string) []string {
	if v == "" {
		return nil
	}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// hasAnyRole reports whether the gateway-injected X-User-Roles header
// intersects allowed. Fail-closed: an absent/empty header means NO roles
// (K1/K6), except under the explicit dev escape (OPENDESK_TRUST_DIRECT_TENANT=1)
// which is logged on every use.
func (s *Server) hasAnyRole(r *http.Request, allowed []string) bool {
	roles := parseCSV(r.Header.Get(HeaderUserRoles))
	if len(roles) == 0 {
		if s.TrustDirectTenancy {
			s.Log.Warn("DEV ESCAPE: role check bypassed (OPENDESK_TRUST_DIRECT_TENANT=1, no X-User-Roles)",
				zap.String("path", r.URL.Path))
			return true
		}
		return false
	}
	set := make(map[string]bool, len(roles))
	for _, role := range roles {
		set[role] = true
	}
	for _, want := range allowed {
		if set[want] {
			return true
		}
	}
	return false
}

// requireDNDAdmin gates a handler on X-User-Roles ∩ DND_ADMIN_ROLES (403
// otherwise). Used by the DND import + global delete (S1-F7-03) and the
// ops-alerts read (K3).
func (s *Server) requireDNDAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hasAnyRole(r, s.dndAdminRoles()) {
			http.Error(w, `{"error":"admin role required"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bindTenantSlug validates a caller-supplied tenant slug (query/body/path
// parameter) against the gateway-injected X-Tenant-Slugs membership list
// (C1/K1 extended binding pattern): the slug is honored only when it
// appears in the verified JWT's tenant_slugs claim. Fail-closed: an absent
// X-Tenant-Slugs header binds nothing, except under the explicit dev escape
// OPENDESK_TRUST_DIRECT_TENANT=1 (gateway-less local dev, logged).
func (s *Server) bindTenantSlug(r *http.Request, slug string) bool {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return false
	}
	slugs := parseCSV(r.Header.Get(HeaderTenantSlugs))
	if len(slugs) == 0 {
		if s.TrustDirectTenancy {
			s.Log.Warn("DEV ESCAPE: tenant binding bypassed (OPENDESK_TRUST_DIRECT_TENANT=1, no X-Tenant-Slugs)",
				zap.String("slug", slug), zap.String("path", r.URL.Path))
			return true
		}
		return false
	}
	for _, allowed := range slugs {
		if allowed == slug {
			return true
		}
	}
	return false
}
