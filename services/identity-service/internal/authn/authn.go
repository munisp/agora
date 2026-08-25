// Package authn holds the shared K1/K2 caller-resolution machinery
// (SPEC-W43 I-01, SPEC-W44 W-I-1).
//
// Trust model (K1): the APISIX gateway terminates OIDC, strips all
// caller-sent x-* identity headers, and re-injects X-User-Id / X-User-Roles /
// X-Tenant-Slugs from the VERIFIED JWT only. Inside the cluster we parse the
// (gateway-verified) Bearer payload for sub/roles/tenant_slugs, falling back
// to the injected headers — the same trust boundary as booking-service
// auth.go. A PRESENTED but undecodable token is rejected error-closed (401).
//
// K2: service-to-service callers present X-Internal-Token matched
// constant-time against the service's configured internal token.
//
// This machinery lived in internal/httpapi/auth.go and was factored out so
// the consent sub-package can gate its destructive/read surfaces on the exact
// same semantics (SPEC-W44 F4 / V2-D3).
package authn

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Claims are the JWT claims identity-service reads from the Bearer token.
type Claims struct {
	Sub         string   `json:"sub"`
	TenantSlugs []string `json:"tenant_slugs"`
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// ErrMalformedToken marks a presented Bearer token that cannot be decoded
// (booking-service K-07 idiom: error-closed — never act on partial claims).
var ErrMalformedToken = errors.New("malformed bearer token")

// ParseBearerClaims decodes the payload segment of a JWT without verifying
// the signature (verified upstream at the gateway). A missing/non-Bearer
// header yields zero claims and nil error (anonymous request — downstream
// guards decide); a presented but undecodable token yields ErrMalformedToken.
func ParseBearerClaims(authHeader string) (Claims, error) {
	var claims Claims
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return claims, nil
	}
	token := strings.TrimPrefix(authHeader, prefix)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, ErrMalformedToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, ErrMalformedToken
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Claims{}, ErrMalformedToken
	}
	return claims, nil
}

// Caller is the resolved identity of the requesting principal.
type Caller struct {
	Subject string   // JWT sub or X-User-Id (gateway-injected)
	Roles   []string // realm roles (JWT realm_access.roles and/or X-User-Roles)
	Slugs   []string // tenant slugs (JWT tenant_slugs and/or X-Tenant-Slugs)
}

// HasRole reports whether the caller holds the realm role.
func (c Caller) HasRole(role string) bool {
	for _, r := range c.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// HasSlug reports whether the caller is bound to the tenant slug (K1
// X-Tenant-Slugs / JWT tenant_slugs membership).
func (c Caller) HasSlug(slug string) bool {
	for _, s := range c.Slugs {
		if s == slug {
			return true
		}
	}
	return false
}

// SplitCSV parses a comma-separated header value into trimmed non-empty
// parts.
func SplitCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Resolve builds the caller identity from the (gateway-verified) Bearer
// token and the K1-injected headers. Error-closed: a malformed presented
// Bearer token yields ErrMalformedToken and the request must be rejected.
func Resolve(r *http.Request) (Caller, error) {
	claims, err := ParseBearerClaims(r.Header.Get("Authorization"))
	if err != nil {
		return Caller{}, err
	}
	c := Caller{
		Subject: claims.Sub,
		Roles:   append([]string{}, claims.RealmAccess.Roles...),
		Slugs:   append([]string{}, claims.TenantSlugs...),
	}
	if c.Subject == "" {
		c.Subject = strings.TrimSpace(r.Header.Get("X-User-Id"))
	}
	c.Roles = append(c.Roles, SplitCSV(r.Header.Get("X-User-Roles"))...)
	c.Slugs = append(c.Slugs, SplitCSV(r.Header.Get("X-Tenant-Slugs"))...)
	return c, nil
}

// ValidInternalToken reports whether the request carries the configured
// internal token (K2, constant-time). Fail-closed: returns false when no
// token is configured.
func ValidInternalToken(configured string, r *http.Request) bool {
	if configured == "" {
		return false
	}
	got := r.Header.Get("X-Internal-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(configured)) == 1
}
