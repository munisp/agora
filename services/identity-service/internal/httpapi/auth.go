// Authn/authz helpers for the tenant/member endpoints (SPEC-W43 I-01,
// SPEC-W44 W-I-1, contracts K1/K2).
//
// Trust model (K1): the APISIX gateway terminates OIDC, strips all
// caller-sent x-* identity headers, and re-injects X-User-Id / X-User-Roles /
// X-Tenant-Slugs from the VERIFIED JWT only. Inside the cluster we parse the
// (gateway-verified) Bearer payload for sub/roles, falling back to the
// injected headers — the same trust boundary as booking-service auth.go.
// A PRESENTED but undecodable token is rejected error-closed (401).
//
// The generic machinery lives in internal/authn (factored out in SPEC-W44 F4
// / V2-D3 so the consent sub-package gates on the same semantics); the
// aliases below keep this package's call sites unchanged.
package httpapi

import (
	"net/http"
	"strings"

	"github.com/opendesk/identity-service/internal/authn"
	"go.uber.org/zap"
)

// errMalformedToken marks a presented Bearer token that cannot be decoded
// (booking-service K-07 idiom: error-closed — never act on partial claims).
var errMalformedToken = authn.ErrMalformedToken

// caller is the resolved identity of the requesting principal.
type caller = authn.Caller

// resolveCaller builds the caller identity from the (gateway-verified) Bearer
// token and the K1-injected headers. Error-closed: a malformed presented
// Bearer token yields errMalformedToken and the request must be rejected.
func resolveCaller(r *http.Request) (caller, error) {
	return authn.Resolve(r)
}

// internauth gates every /internal/* surface (K2): X-Internal-Token must
// match IDENTITY_INTERNAL_TOKEN via constant-time compare. Fail-closed: 503
// when the env token is unset, 401 on missing/wrong. Non-internal paths pass
// through untouched.
func (s *server) internauth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/internal/") {
			next.ServeHTTP(w, r)
			return
		}
		if s.d.InternalToken == "" {
			s.d.Logger.Error("IDENTITY_INTERNAL_TOKEN unset — refusing internal request (fail-closed, K2)",
				zap.String("path", r.URL.Path))
			writeError(w, http.StatusServiceUnavailable, "internal token not configured")
			return
		}
		if !s.validInternalToken(r) {
			writeError(w, http.StatusUnauthorized, "invalid internal token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// validInternalToken reports whether the request carries the configured
// internal token (constant-time). Returns false when no token is configured.
func (s *server) validInternalToken(r *http.Request) bool {
	return authn.ValidInternalToken(s.d.InternalToken, r)
}

// isPlatformAdmin reports whether the caller may perform platform-level
// actions (e.g. set a non-free plan): the "platform-admin" realm role
// (K1 X-User-Roles / JWT realm_access.roles) or membership in the
// OPENDESK_PLATFORM_ADMINS subject allowlist (SPEC-W43 I-01).
func (s *server) isPlatformAdmin(c caller) bool {
	if c.HasRole("platform-admin") {
		return true
	}
	for _, sub := range s.d.PlatformAdmins {
		if sub != "" && sub == c.Subject {
			return true
		}
	}
	return false
}
