package httpapi

// SPEC-W94: verification-grant revocation. Active revocation complements
// the TTL (W87's "TTL is the revocation"): a pack policy change, a fraud
// signal, or an internal reset deletes the contact's grant immediately
// instead of waiting for expiry. internAuth-guarded (never routed
// publicly) and mirrors the check endpoint's anti-enumeration discipline:
// an unknown contact and a contact without a grant both answer
// {"revoked": false} — the response only confirms that THIS caller's
// delete found a row.

import (
	"errors"
	"net/http"
	"strings"

	"github.com/opendesk/booking-service/internal/store"
)

// verificationRevokeInternal serves POST /internal/sites/{slug}/verification/revoke.
func (s *server) verificationRevokeInternal(w http.ResponseWriter, r *http.Request) {
	site, ok := s.resolveSite(w, r)
	if !ok {
		return
	}
	var req verificationCheckRequest // same shape as the check endpoint
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
		writeJSON(w, http.StatusOK, map[string]any{"revoked": false})
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	revoked, err := s.d.Store.RevokeVerificationGrant(r.Context(), site.TenantID, contact.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": revoked})
}
