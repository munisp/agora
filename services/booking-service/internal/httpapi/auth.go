package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// jwtClaims are the claims booking-service reads from the Bearer token.
// Signature verification happens at the APISIX gateway (jwt-auth /
// openid-connect plugins, SPEC §8/§12); inside the cluster network we only
// parse the payload. This trust boundary is documented in the README.
type jwtClaims struct {
	Sub         string   `json:"sub"`
	TenantSlugs []string `json:"tenant_slugs"`
	// Email carries the caller's email claim when the IdP includes one —
	// used by GET /v1/bookings?mine=true to resolve the team member.
	Email string `json:"email"`
	// RealmAccess carries the Keycloak realm roles (docs/security/roles.md)
	// — SPEC-W32 WS-A uses them for role-based reporter masking on civic
	// cases (owner/admin see reporter PII on detail views).
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// roles returns the caller's Keycloak realm roles (empty when absent).
func (c jwtClaims) roles() []string { return c.RealmAccess.Roles }

// parseBearerClaims decodes the payload segment of a JWT without verifying
// the signature (verified upstream at the gateway).
func parseBearerClaims(authHeader string) jwtClaims {
	var claims jwtClaims
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return claims
	}
	token := strings.TrimPrefix(authHeader, prefix)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims
	}
	_ = json.Unmarshal(payload, &claims)
	return claims
}

func (c jwtClaims) hasTenant(slug string) bool {
	for _, s := range c.TenantSlugs {
		if s == slug {
			return true
		}
	}
	return false
}

// firstTenant returns the first tenant slug claim, used when the
// X-Tenant-Slug header is absent.
func (c jwtClaims) firstTenant() string {
	if len(c.TenantSlugs) > 0 {
		return c.TenantSlugs[0]
	}
	return ""
}
