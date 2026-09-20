package consent

// SPEC-W45 STK O13: booking-portal JWT acceptance for the consent
// data-access/erasure surfaces so DATA SUBJECTS can self-serve (NDPA right
// of access/erasure without operator involvement).
//
// The booking portal (booking-service/internal/httpapi/portal.go) issues a
// 15-minute HS256 JWT after a phone/e-mail OTP challenge — possessing the
// contact's phone/e-mail IS the credential. Claims: sub=contact_id,
// tid=tenant_id, tsl=tenant_slug, scope=portal. This file verifies the
// signature with the SHARED PORTAL_SECRET (unset = portal path unavailable,
// fail-closed) and binds BOTH claims to the request: the tenant slug must
// match the request tenant and the contact id must match the data subject
// being read/erased — a portal token can never touch another subject's or
// another tenant's records.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// portalClaims mirrors booking-service's portalClaims (keep in sync).
type portalClaims struct {
	Sub        string `json:"sub"`   // contact id
	TenantID   string `json:"tid"`   // tenant uuid
	TenantSlug string `json:"tsl"`   // tenant slug
	Scope      string `json:"scope"` // must be "portal"
	ExpiresAt  int64  `json:"exp"`
}

// parsePortalJWT verifies the Authorization header as a booking portal JWT:
// HS256 signature under secret, alg pinned (no alg confusion), scope=portal,
// not expired. Returns ok=false for ANY deviation (callers fall through to
// the other credential paths or deny).
func parsePortalJWT(secret, authHeader string) (portalClaims, bool) {
	var claims portalClaims
	if secret == "" {
		return claims, false // fail-closed: portal path not configured
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return claims, false
	}
	parts := strings.Split(strings.TrimPrefix(authHeader, prefix), ".")
	if len(parts) != 3 {
		return claims, false
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || !strings.Contains(string(header), `"HS256"`) {
		return claims, false
	}
	body := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || subtle.ConstantTimeCompare(sig, mac.Sum(nil)) != 1 {
		return claims, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &claims) != nil {
		return portalClaims{}, false
	}
	if claims.Scope != "portal" || claims.Sub == "" || claims.TenantSlug == "" {
		return portalClaims{}, false
	}
	if claims.ExpiresAt <= time.Now().Unix() {
		return portalClaims{}, false
	}
	return claims, true
}

// portalSelfServe reports whether the request carries a valid portal JWT
// whose contact (sub) and tenant (tsl) claims are BOTH bound to the data
// subject and tenant of the request (STK O13 self-serve rule).
func (h *Handler) portalSelfServe(r *http.Request, tenantSlug, subject string) bool {
	claims, ok := parsePortalJWT(h.PortalSecret, r.Header.Get("Authorization"))
	if !ok {
		return false
	}
	return claims.TenantSlug == tenantSlug && claims.Sub == subject
}
