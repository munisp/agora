package bookingops

// SPEC-W49 B2 allow-list table tests: allowed/exact, allowed/subdomain
// wildcard, rejected/other-host, rejected/http-scheme, rejected when the
// env is unset (fail closed), empty-ok.

import (
	"errors"
	"testing"
)

func TestValidatePayRedirectURL(t *testing.T) {
	allow := []string{"pay.acme.ng", "*.opendesk.ng"}
	cases := []struct {
		name      string
		redirect  string
		allowList []string
		wantErr   bool
	}{
		{"empty-ok", "", allow, false},
		{"empty-ok-whitespace", "   ", allow, false},
		{"empty-ok-even-when-unset", "", nil, false},
		{"allowed-exact", "https://pay.acme.ng/done", allow, false},
		{"allowed-exact-case-insensitive", "https://Pay.Acme.NG/done", allow, false},
		{"allowed-exact-with-port-path-query", "https://pay.acme.ng:8443/done?ref=1", allow, false},
		{"allowed-subdomain-wildcard", "https://tenant.opendesk.ng/thanks", allow, false},
		{"allowed-wildcard-parent-itself", "https://opendesk.ng/thanks", allow, false},
		{"rejected-other-host", "https://evil.example.com/", allow, true},
		{"rejected-suffix-lookalike", "https://pay.acme.ng.evil.com/", allow, true},
		{"rejected-http-scheme", "http://pay.acme.ng/done", allow, true},
		{"rejected-scheme-relative", "//pay.acme.ng/done", allow, true},
		{"rejected-no-host", "https:///done", allow, true},
		{"rejected-relative", "/done", allow, true},
		{"rejected-not-a-url", "ht!tp://%%%", allow, true},
		{"rejected-when-unset", "https://pay.acme.ng/done", nil, true},
		{"rejected-when-empty-list", "https://pay.acme.ng/done", []string{}, true},
		{"rejected-when-only-blank-entries", "https://pay.acme.ng/done", []string{"", "  "}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePayRedirectURL(tc.redirect, tc.allowList)
			if tc.wantErr && !errors.Is(err, ErrRedirectNotAllowed) {
				t.Fatalf("err = %v, want ErrRedirectNotAllowed", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}
