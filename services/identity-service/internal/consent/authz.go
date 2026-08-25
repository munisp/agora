package consent

import (
	"net/http"

	"github.com/opendesk/identity-service/internal/authn"
	"go.uber.org/zap"
)

// authorizeDataAccess gates the destructive erasure POST and the
// consent-record GET (SPEC-W44 F4 / V2-D3). Background: K4 (W-I) made the
// erasure path publish PrivacyEraseRequested to opendesk.privacy.events, so
// an UNAUTHENTICATED caller could trigger cross-tenant PII destruction
// fanout, and GET /v1/consents?subject= leaked any tenant's records to
// anyone. Both surfaces now require EITHER
//
//	(a) a service caller presenting X-Internal-Token == IDENTITY_INTERNAL_TOKEN
//	    (K2, constant-time; unset token = path unavailable, fail-closed), OR
//	(b) an authenticated subject (gateway-verified JWT sub / K1 X-User-Id)
//	    whose tenant membership binds to the REQUEST tenant via
//	    X-Tenant-Slugs / JWT tenant_slugs (K1 pattern). The explicit dev
//	    escape OPENDESK_TRUST_DIRECT_TENANT=1 (logged on every use) trusts an
//	    authenticated subject with NO slug claims for gateway-less local runs.
//
// Statuses: 401 without credentials (or a malformed presented Bearer token),
// 403 when authenticated but not bound to the request tenant.
//
// NOTE: public data-principal self-service erasure (the data subject
// tombstoning their OWN records without a tenant membership) is OUT OF
// SCOPE here pending a verification flow (signed subject token / OTP
// challenge) — today erasure is operator/service initiated only.
func (h *Handler) authorizeDataAccess(w http.ResponseWriter, r *http.Request, tenantSlug string) bool {
	// (a) K2 service caller.
	if authn.ValidInternalToken(h.InternalToken, r) {
		return true
	}
	// (b) K1 authenticated subject bound to the request tenant.
	c, err := authn.Resolve(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "malformed bearer token")
		return false
	}
	if c.Subject == "" {
		writeError(w, http.StatusUnauthorized, "authentication required (X-Internal-Token service credential or authenticated subject)")
		return false
	}
	if c.HasSlug(tenantSlug) {
		return true
	}
	if len(c.Slugs) == 0 && h.TrustDirectTenancy {
		h.Logger.Warn("DEV ESCAPE: tenant binding bypassed (OPENDESK_TRUST_DIRECT_TENANT=1, no tenant_slugs claims)",
			zap.String("path", r.URL.Path), zap.String("subject", c.Subject),
			zap.String("tenant", tenantSlug))
		return true
	}
	writeError(w, http.StatusForbidden, "subject is not bound to this tenant (X-Tenant-Slugs membership required)")
	return false
}
