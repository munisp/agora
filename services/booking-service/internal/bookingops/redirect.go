package bookingops

// SPEC-W49 B2: redirect_url allow-list for the public pay leg. Before W49
// the caller-supplied redirect_url flowed unvalidated from
// POST /public/sites/{slug}/bookings/{id}/pay through payments-service into
// the Flutterwave checkout — open-redirect-grade trust of anonymous input.
// The allow-list (PUBLIC_PAY_REDIRECT_ALLOW_HOSTS) is FAIL-CLOSED: unset or
// empty rejects EVERY non-empty redirect_url (the empty value stays allowed
// and falls through to the provider default, FLUTTERWAVE_REDIRECT_URL).

import (
	"errors"
	"net/url"
	"strings"
)

// ErrRedirectNotAllowed marks a redirect_url that fails the allow-list.
// httpapi maps it to 400 {"error":"redirect_url_not_allowed"}.
var ErrRedirectNotAllowed = errors.New("redirect_url_not_allowed")

// ValidatePayRedirectURL checks redirectURL against the allow-list:
//   - empty (after trim) → allowed (provider default redirect applies);
//   - allowHosts empty → EVERY non-empty value rejected (fail-closed);
//   - otherwise the URL must parse, use the https scheme, and its host must
//     match an entry — either exactly ("pay.acme.ng") or via a leading-`*`
//     parent-domain entry ("*.acme.ng" matches "acme.ng" and every
//     subdomain like "pay.acme.ng"). Host comparison is case-insensitive.
func ValidatePayRedirectURL(redirectURL string, allowHosts []string) error {
	raw := strings.TrimSpace(redirectURL)
	if raw == "" {
		return nil
	}
	if len(allowHosts) == 0 {
		return ErrRedirectNotAllowed
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ErrRedirectNotAllowed
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return ErrRedirectNotAllowed
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ErrRedirectNotAllowed
	}
	for _, entry := range allowHosts {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if strings.HasPrefix(entry, "*.") {
			base := entry[2:]
			if base != "" && (host == base || strings.HasSuffix(host, "."+base)) {
				return nil
			}
			continue
		}
		if host == entry {
			return nil
		}
	}
	return ErrRedirectNotAllowed
}
