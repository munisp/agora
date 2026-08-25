package httpapi

// S1-F6-03 / N-02: SSRF guard for outbound webhook subscription URLs.
//
// A tenant-supplied webhook URL is a server-side request forgery primitive:
// without validation a tenant could point deliveries at cloud metadata
// (169.254.169.254), cluster-internal services or loopback daemons. The
// validator therefore enforces, at subscription-create time:
//
//   - https only outside dev (http accepted only when AllowHTTP, i.e.
//     OPENDESK_DEV_ENDPOINTS=1 local runs);
//   - no userinfo (user:pass@host);
//   - DNS resolution of the host (FAIL-CLOSED: a resolution error rejects
//     the URL) with every resolved IP outside the private / loopback /
//     link-local / multicast / CGNAT / unspecified ranges (IP literals are
//     checked directly);
//   - the delivery HTTP client (NewWebhookHTTPClient) additionally refuses
//     redirects (a 30x could bounce to an internal target) and bounds each
//     attempt at 10s, and re-checks the dialed IP so a DNS rebinding between
//     validation and delivery cannot smuggle a private address through.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// URLValidator validates outbound webhook target URLs.
type URLValidator struct {
	// AllowHTTP accepts plain http:// URLs (dev only; default false →
	// https-only per N-02).
	AllowHTTP bool
	// AllowPrivate skips the private/loopback/link-local CIDR block (dev
	// only — local webhook receivers; default false). Multicast/unspecified
	// stay blocked.
	AllowPrivate bool
	// LookupIP resolves a host to IPs; injectable for tests. Nil →
	// net.DefaultResolver.LookupIP.
	LookupIP func(ctx context.Context, host string) ([]net.IP, error)
}

// NewURLValidator returns the production validator (https-only, system
// resolver). allowHTTP is the dev escape.
func NewURLValidator(allowHTTP bool) *URLValidator {
	return &URLValidator{AllowHTTP: allowHTTP}
}

// blocked CIDRs beyond netip's built-in classifications: CGNAT (100.64/10)
// is not "private" per RFC 1918 but is never a legitimate webhook target.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT (RFC 6598)
	netip.MustParsePrefix("0.0.0.0/8"),       // "this host" (RFC 1122)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmark testing (RFC 2544)
	netip.MustParsePrefix("::/128"),          // IPv6 unspecified
	netip.MustParsePrefix("fc00::/7"),        // IPv6 ULA (not covered by IsPrivate)
	netip.MustParsePrefix("fec0::/10"),       // IPv6 site-local (deprecated)
}

// ValidateWebhookURL parses and validates one subscription URL.
func (v *URLValidator) ValidateWebhookURL(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && !(scheme == "http" && v.AllowHTTP) {
		if scheme == "http" {
			return errors.New("url must use https outside dev mode")
		}
		return fmt.Errorf("url scheme %q is not allowed (https only)", u.Scheme)
	}
	if u.User != nil {
		return errors.New("url must not contain userinfo")
	}
	if u.Host == "" || u.Hostname() == "" {
		return errors.New("url must have a host")
	}
	return v.checkHost(ctx, u.Hostname())
}

// checkHost resolves host (IP literal or DNS name) and rejects any address
// in a blocked range. DNS resolution failure rejects the URL (fail-closed).
func (v *URLValidator) checkHost(ctx context.Context, host string) error {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		return v.checkIP(ip.Unmap())
	}
	lookup := v.LookupIP
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return fmt.Errorf("url host %q does not resolve: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("url host %q does not resolve", host)
	}
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			return fmt.Errorf("url host %q resolved to an unparseable address", host)
		}
		if err := v.checkIP(addr.Unmap()); err != nil {
			return fmt.Errorf("url host %q resolves to a blocked address: %w", host, err)
		}
	}
	return nil
}

// checkIP rejects loopback / private / link-local / multicast / unspecified
// and the explicitly blocked CIDRs (AllowPrivate lifts only the
// loopback/private/link-local-unicast classes — the dev escape).
func (v *URLValidator) checkIP(addr netip.Addr) error {
	if addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return fmt.Errorf("%s is a non-public address", addr)
	}
	if !v.AllowPrivate && (addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast()) {
		return fmt.Errorf("%s is a non-public address", addr)
	}
	if !v.AllowPrivate {
		for _, p := range blockedPrefixes {
			if p.Contains(addr) {
				return fmt.Errorf("%s is in blocked range %s", addr, p)
			}
		}
	}
	return nil
}

// NewWebhookHTTPClient builds the outbound delivery client (N-02): 10s
// total timeout, redirects REFUSED (ErrUseLastResponse — a redirect target
// is never re-validated, so following one is an SSRF bypass), and a dialing
// re-check so DNS rebinding between validation and delivery cannot reach a
// blocked address.
func NewWebhookHTTPClient(v *URLValidator) *http.Client {
	if v == nil {
		v = NewURLValidator(false)
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err == nil && host != "" {
					if cerr := v.checkHost(ctx, host); cerr != nil {
						return nil, fmt.Errorf("webhook dial blocked: %w", cerr)
					}
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
	}
}
