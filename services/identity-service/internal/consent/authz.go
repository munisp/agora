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
// anyone. Both surfaces now require ONE OF
//
//	(a) a service caller presenting X-Internal-Token == IDENTITY_INTERNAL_TOKEN
//	    (K2, constant-time; unset token = path unavailable, fail-closed), OR
//	(b) a DATA SUBJECT self-serving with a booking portal JWT (SPEC-W45 STK
//	    O13): HS256 under the shared PORTAL_SECRET, scope=portal, unexpired,
//	    with the contact claim (sub) equal to the REQUESTED data subject and
//	    the tenant claim (tsl) equal to the request tenant — the token can
//	    only ever touch its owner's records in its own tenant, OR
//	(c) an authenticated subject (gateway-verified JWT sub / K1 X-User-Id)
//	    whose tenant membership binds to the REQUEST tenant via
//	    X-Tenant-Slugs / JWT tenant_slugs (K1 pattern). The explicit dev
//	    escape OPENDESK_TRUST_DIRECT_TENANT=1 (logged on every use) trusts an
//	    authenticated subject with NO slug claims for gateway-less local runs.
//
// Statuses: 401 without credentials (or a malformed presented Bearer token),
// 403 when authenticated but not bound to the request tenant.
func (h *Handler) authorizeDataAccess(w http.ResponseWriter, r *http.Request, tenantSlug, subject string) bool {
	// (a) K2 service caller.
	if authn.ValidInternalToken(h.InternalToken, r) {
		return true
	}
	// (b) STK O13 data-subject self-service via the booking portal session
	// (OTP-verified contact credential; claims bound to subject+tenant).
	if h.portalSelfServe(r, tenantSlug, subject) {
		return true
	}
	// (c) K1 authenticated subject bound to the request tenant.
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
