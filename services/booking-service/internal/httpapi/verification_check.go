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

	"github.com/opendesk/booking-service/internal/store"
)

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
