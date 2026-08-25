package httpapi

// S1-F6-03/N-02 table tests for the webhook SSRF guard.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestURLValidatorSchemes(t *testing.T) {
	v := NewURLValidator(false)
	v.LookupIP = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	for _, bad := range []string{
		"",
		"not-a-url",
		"ftp://example.com/hook",
		"http://example.com/hook",      // https-only outside dev
		"https://user:pw@example.com/", // userinfo
		"https://",                     // no host
	} {
		if err := v.ValidateWebhookURL(context.Background(), bad); err == nil {
			t.Fatalf("expected rejection for %q", bad)
		}
	}
	if err := v.ValidateWebhookURL(context.Background(), "https://example.com/hook"); err != nil {
		t.Fatalf("public https URL rejected: %v", err)
	}

	// Dev escape: http accepted when AllowHTTP.
	dev := NewURLValidator(true)
	dev.LookupIP = v.LookupIP
	if err := dev.ValidateWebhookURL(context.Background(), "http://example.com/hook"); err != nil {
		t.Fatalf("dev http URL rejected: %v", err)
	}
}

func TestURLValidatorBlockedIPs(t *testing.T) {
	cases := []struct {
		name string
		url  string
		ips  []string
	}{
		{"loopback literal", "https://127.0.0.1/hook", nil},
		{"private literal", "https://10.1.2.3/hook", nil},
		{"rfc1918 192.168", "https://192.168.1.1/", nil},
		{"link-local metadata", "https://169.254.169.254/latest/meta-data", nil},
		{"ipv6 loopback", "https://[::1]/hook", nil},
		{"ipv4-mapped loopback", "https://[::ffff:127.0.0.1]/hook", nil},
		{"multicast", "https://224.0.0.1/hook", nil},
		{"unspecified", "https://0.0.0.0/hook", nil},
		{"cgnat", "https://100.64.0.1/hook", nil},
		{"dns loopback", "https://internal.example/", []string{"127.0.0.1"}},
		{"dns private", "https://svc.cluster.local/", []string{"172.16.0.5"}},
		{"dns mixed public+private", "https://split.example/", []string{"93.184.216.34", "192.168.0.1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vv := NewURLValidator(false)
			vv.LookupIP = func(context.Context, string) ([]net.IP, error) {
				var ips []net.IP
				for _, s := range tc.ips {
					ips = append(ips, net.ParseIP(s))
				}
				return ips, nil
			}
			if err := vv.ValidateWebhookURL(context.Background(), tc.url); err == nil {
				t.Fatalf("expected %q to be rejected", tc.url)
			}
		})
	}
}

func TestURLValidatorDNSFailClosed(t *testing.T) {
	v := NewURLValidator(false)
	v.LookupIP = func(context.Context, string) ([]net.IP, error) {
		return nil, errors.New("no such host")
	}
	if err := v.ValidateWebhookURL(context.Background(), "https://gone.example/"); err == nil {
		t.Fatal("DNS failure must reject (fail-closed)")
	}
	v.LookupIP = func(context.Context, string) ([]net.IP, error) { return nil, nil }
	if err := v.ValidateWebhookURL(context.Background(), "https://gone.example/"); err == nil {
		t.Fatal("empty DNS answer must reject (fail-closed)")
	}
}

func TestURLValidatorAllowPrivateDev(t *testing.T) {
	v := NewURLValidator(true)
	v.AllowPrivate = true
	if err := v.ValidateWebhookURL(context.Background(), "http://127.0.0.1:9999/hook"); err != nil {
		t.Fatalf("dev private receiver rejected: %v", err)
	}
	// Multicast stays blocked even in dev.
	if err := v.ValidateWebhookURL(context.Background(), "http://224.0.0.1/"); err == nil {
		t.Fatal("multicast must stay blocked in dev mode")
	}
}

func TestWebhookHTTPClientNoRedirects(t *testing.T) {
	c := NewWebhookHTTPClient(NewURLValidator(false))
	req, err := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckRedirect(req, nil); err == nil {
		t.Fatal("redirects must be refused (N-02)")
	}
	if c.Timeout != 10*time.Second {
		t.Fatalf("timeout = %v, want 10s", c.Timeout)
	}
}
