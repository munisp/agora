package httpapi

// SPEC-W87: verification-grant check. The voice runtime calls
// POST /internal/sites/{slug}/verification/check before running its OTP
// step-up on a PSTN call: when the contact already holds an active
// (unexpired) verification grant — written by a genuine portal OTP success
// — the runtime honors it under its strict anchor rule instead of
// re-prompting. The endpoint is internAuth-guarded (never routed publicly)
// and answers {"verified": false} uniformly for unknown contacts and
// missing/expired grants (no enumeration oracle behind the token gate).

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/store"
)

// effectiveGrantPolicy (SPEC-W94) resolves the pack-aware verification
// grant policy. Baseline is the global env behavior (enabled iff envTTL
// > 0); a pack block narrows it: enabled=false kills grant writing AND
// honoring for the pack's tenants, ttlHours>0 overrides the TTL (0 = keep
// env). The env kill switch (envTTL == 0) stays AUTHORITATIVE — a pack
// can never revive grants the operator disabled globally (W87: the kill
// switch is global, both sides). A nil pack (or unresolved tenant)
// preserves the pre-W94 global behavior exactly.
func effectiveGrantPolicy(pack *bookingops.PackSummary, envTTL time.Duration) (bool, time.Duration) {
	enabled, ttl := envTTL > 0, envTTL
	if pack == nil || pack.VerificationGrants == nil {
		return enabled, ttl
	}
	pol := pack.VerificationGrants
	if !pol.Enabled {
		return false, ttl
	}
	if pol.TTLHours > 0 {
		ttl = time.Duration(pol.TTLHours) * time.Hour
	}
	return enabled, ttl
}

// grantPolicyFor resolves the effective grant policy for a site's tenant.
// Tenant-resolution failure (or a nil hook) fails OPEN to the env
// baseline: the pre-W94 behavior had no tenant lookup at all, so a
// control-plane outage must not change grant honoring.
func (s *server) grantPolicyFor(r *http.Request, tenantSlug string) (bool, time.Duration) {
	if s.d.TenantBySlug == nil {
		return effectiveGrantPolicy(nil, s.d.VerificationGrantTTL)
	}
	tenant, err := s.d.TenantBySlug(r.Context(), tenantSlug)
	if err != nil {
		return effectiveGrantPolicy(nil, s.d.VerificationGrantTTL)
	}
	return effectiveGrantPolicy(tenant.Pack, s.d.VerificationGrantTTL)
}

type verificationCheckRequest struct {
	Phone   string `json:"phone"`
	Email   string `json:"email"`
	Channel string `json:"channel"` // optional explicit channel (portalChannelFor rules)
}

// verificationCheckInternal serves POST /internal/sites/{slug}/verification/check.
func (s *server) verificationCheckInternal(w http.ResponseWriter, r *http.Request) {
	site, ok := s.resolveSite(w, r)
	if !ok {
		return
	}
	var req verificationCheckRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Phone = strings.TrimSpace(req.Phone)
	req.Email = strings.TrimSpace(req.Email)
	req.Channel = strings.ToLower(strings.TrimSpace(req.Channel))
	channel, dest, cerr := portalChannelFor(req.Phone, req.Email, req.Channel)
	if cerr != "" {
		writeError(w, http.StatusBadRequest, cerr)
		return
	}
	contact, err := s.d.Store.GetContactByChannel(r.Context(), site.TenantID, channel, dest)
	if errors.Is(err, store.ErrNotFound) {
		// Unknown contact: same body as a missing grant (anti-enumeration).
		writeJSON(w, http.StatusOK, map[string]any{"verified": false})
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	// SPEC-W94: the pack policy is authoritative at READ time — a pack
	// with verificationGrants.enabled=false stops honoring even grants
	// written before the policy landed.
	if enabled, _ := s.grantPolicyFor(r, site.TenantSlug); !enabled {
		writeJSON(w, http.StatusOK, map[string]any{"verified": false})
		return
	}
	grant, err := s.d.Store.GetActiveVerificationGrant(r.Context(), site.TenantID, contact.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"verified": false})
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"verified":   true,
		"method":     grant.Method,
		"expires_at": grant.ExpiresAt.UTC().Format(time.RFC3339),
	})
}
