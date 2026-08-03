package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opendesk/messaging-gateway/internal/metrics"
	"go.uber.org/zap"
)

// --- eBulksSMS provider (SPEC-W12 Agent A; shape is an ASSUMPTION) ---

func TestEBulkSMS(t *testing.T) {
	var last *fakeProvider
	runCase(t, func(baseURL string, f *fakeProvider) error {
		last = f
		p := &EBulkSMS{Client: testClient("ebulksms"), BaseURL: baseURL,
			APIKey: "eb-key", Username: "eb-user", Sender: "OpenDesk"}
		_, _, err := p.SendSMS(context.Background(), "+2348012345678", "hello", "")
		return err
	})

	f := last
	if f.path != "/sendsms" {
		t.Fatalf("unexpected path %q", f.path)
	}
	if ct := f.contentType; !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("expected JSON body, got %q", ct)
	}
	var payload map[string]any
	if err := json.Unmarshal(f.body, &payload); err != nil {
		t.Fatalf("decode ebulksms payload: %v", err)
	}
	wantStr := map[string]string{
		"username": "eb-user", "apikey": "eb-key", "sender": "OpenDesk",
		"messagetext": "hello", "recipients": "+2348012345678",
	}
	for k, v := range wantStr {
		if payload[k] != v {
			t.Fatalf("payload[%s] = %v, want %q (full: %s)", k, payload[k], v, f.body)
		}
	}
	if payload["flash"] != float64(0) {
		t.Fatalf("flash must be 0, got %v", payload["flash"])
	}
}

func TestEBulkSMSSenderOverride(t *testing.T) {
	f := &fakeProvider{t: t, statuses: []int{200}}
	srv := f.server()
	defer srv.Close()
	p := &EBulkSMS{Client: testClient("ebulksms"), BaseURL: srv.URL, APIKey: "k", Username: "u", Sender: "OpenDesk"}
	if _, _, err := p.SendSMS(context.Background(), "+2348000000000", "hi", "AcmeNG"); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	json.Unmarshal(f.body, &payload) //nolint:errcheck
	if payload["sender"] != "AcmeNG" {
		t.Fatalf("sender override not applied: %s", f.body)
	}
}

func TestEBulkSMSConfigured(t *testing.T) {
	p := &EBulkSMS{}
	if p.Configured() {
		t.Fatal("unconfigured provider must report Configured()=false")
	}
	p.APIKey = "k"
	if p.Configured() {
		t.Fatal("username missing: Configured() must be false")
	}
	p.Username = "u"
	if !p.Configured() {
		t.Fatal("apikey+username set: Configured() must be true")
	}
}

// --- Failover chain (SPEC-W12 Agent A) ---

// chainServer is one fake aggregator endpoint for the chain tests.
type chainServer struct {
	calls  atomic.Int32
	status int
	srv    *httptest.Server
}

func newChainServer(status int) *chainServer {
	c := &chainServer{status: status}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.calls.Add(1)
		body, _ := io.ReadAll(r.Body) // drain
		_ = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(c.status)
		w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
	}))
	return c
}

func (c *chainServer) Close() { c.srv.Close() }

// realChain builds a Failover over REAL provider clients (AT → Termii →
// eBulks) pointing at the given fake servers, so the chain tests exercise
// the providers' retry/error mapping and the chain together.
func realChain(t *testing.T, at, termii, ebulk *chainServer) *Failover {
	t.Helper()
	mk := func(name string, srv *chainServer) SMSProvider {
		switch name {
		case "africastalking":
			return &AfricasTalking{Client: testClient("africastalking"), BaseURL: srv.srv.URL, APIKey: "k", Username: "u"}
		case "termii":
			return &Termii{Client: testClient("termii"), BaseURL: srv.srv.URL, APIKey: "k"}
		default:
			return &EBulkSMS{Client: testClient("ebulksms"), BaseURL: srv.srv.URL, APIKey: "k", Username: "u"}
		}
	}
	providers := map[string]SMSProvider{}
	for name, srv := range map[string]*chainServer{"africastalking": at, "termii": termii, "ebulksms": ebulk} {
		if srv != nil {
			providers[name] = mk(name, srv)
		}
	}
	return NewFailover(providers, "", zap.NewNop())
}

func TestFailoverFirstFailsSecondSucceeds(t *testing.T) {
	at := newChainServer(http.StatusBadGateway) // persistent 5xx
	defer at.Close()
	termii := newChainServer(http.StatusOK)
	defer termii.Close()
	ebulk := newChainServer(http.StatusOK)
	defer ebulk.Close()

	chain := realChain(t, at, termii, ebulk)
	name, status, _, err := chain.SendSMS(context.Background(), "+2348012345678", "hello", "")
	if err != nil {
		t.Fatalf("expected termii to serve the send, got %v", err)
	}
	if name != "termii" {
		t.Fatalf("expected winning provider termii, got %q", name)
	}
	if status != http.StatusOK {
		t.Fatalf("unexpected status %d", status)
	}
	if got := at.calls.Load(); got != maxAttempts {
		t.Fatalf("AT must be exhausted (%d attempts incl. retries) before failover, got %d", maxAttempts, got)
	}
	if got := termii.calls.Load(); got != 1 {
		t.Fatalf("termii must be called once, got %d", got)
	}
	if got := ebulk.calls.Load(); got != 0 {
		t.Fatalf("ebulksms must not be called after termii succeeded, got %d", got)
	}
}

func TestFailoverAllFail(t *testing.T) {
	at := newChainServer(http.StatusInternalServerError)
	defer at.Close()
	termii := newChainServer(http.StatusBadGateway)
	defer termii.Close()
	ebulk := newChainServer(http.StatusServiceUnavailable)
	defer ebulk.Close()

	chain := realChain(t, at, termii, ebulk)
	_, _, _, err := chain.SendSMS(context.Background(), "+2348012345678", "hello", "")
	if err == nil {
		t.Fatal("expected error when every provider fails")
	}
	if !strings.Contains(err.Error(), "all sms providers failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	// Every provider was tried (with the shared client's retries).
	for name, srv := range map[string]*chainServer{"at": at, "termii": termii, "ebulk": ebulk} {
		if got := srv.calls.Load(); got != maxAttempts {
			t.Fatalf("%s must be exhausted (%d attempts), got %d", name, maxAttempts, got)
		}
	}
}

func TestFailover4xxDoesNotFailOver(t *testing.T) {
	at := newChainServer(http.StatusBadRequest) // caller fault
	defer at.Close()
	termii := newChainServer(http.StatusOK)
	defer termii.Close()

	chain := realChain(t, at, termii, nil)
	name, _, _, err := chain.SendSMS(context.Background(), "+2348012345678", "hello", "")
	if err == nil || !ClientError(err) {
		t.Fatalf("expected provider 4xx surfaced, got %v", err)
	}
	if name != "africastalking" {
		t.Fatalf("expected failing provider name africastalking, got %q", name)
	}
	if got := termii.calls.Load(); got != 0 {
		t.Fatalf("4xx must not fail over to termii, got %d calls", got)
	}
}

func TestFailoverCircuitBreaker(t *testing.T) {
	at := newChainServer(http.StatusInternalServerError)
	defer at.Close()
	termii := newChainServer(http.StatusOK)
	defer termii.Close()

	chain := realChain(t, at, termii, nil)
	// Tighten the AT breaker for the test: open after 1 failure.
	var atBreaker *CircuitBreaker
	for _, e := range chain.Entries() {
		if e.Name == "africastalking" {
			e.Breaker.threshold = 1
			atBreaker = e.Breaker
		}
	}
	if atBreaker == nil {
		t.Fatal("chain missing africastalking entry")
	}

	// First send: AT fails once → breaker opens → termii serves.
	if _, _, _, err := chain.SendSMS(context.Background(), "+2348012345678", "a", ""); err != nil {
		t.Fatal(err)
	}
	if !atBreaker.Open() {
		t.Fatal("breaker must be open after the threshold failure")
	}
	callsAfterOpen := at.calls.Load()

	// Second send: AT circuit is open — skipped entirely, termii serves.
	if _, _, _, err := chain.SendSMS(context.Background(), "+2348012345678", "b", ""); err != nil {
		t.Fatal(err)
	}
	if got := at.calls.Load(); got != callsAfterOpen {
		t.Fatalf("open circuit must skip the provider, got %d extra calls", got-callsAfterOpen)
	}

	// After the cooldown the breaker allows one probe.
	now := time.Now()
	atBreaker.now = func() time.Time { return now.Add(61 * time.Second) }
	if !atBreaker.Allow() {
		t.Fatal("breaker must allow a probe after the cooldown")
	}
}

func TestFailoverSkipsUnconfigured(t *testing.T) {
	termii := newChainServer(http.StatusOK)
	defer termii.Close()
	// AT has no credentials (unconfigured) — must be skipped silently.
	at := &AfricasTalking{Client: testClient("africastalking")}
	chain := NewFailover(map[string]SMSProvider{
		"africastalking": at,
		"termii":         &Termii{Client: testClient("termii"), BaseURL: termii.srv.URL, APIKey: "k"},
	}, "", zap.NewNop())
	name, _, _, err := chain.SendSMS(context.Background(), "+2348012345678", "hi", "")
	if err != nil || name != "termii" {
		t.Fatalf("expected termii to serve, got name=%q err=%v", name, err)
	}
}

func TestFailoverChainOrder(t *testing.T) {
	// Custom SMS_PROVIDER_CHAIN order is honored.
	ebulk := newChainServer(http.StatusOK)
	defer ebulk.Close()
	termii := newChainServer(http.StatusOK)
	defer termii.Close()
	chain := NewFailover(map[string]SMSProvider{
		"termii":   &Termii{Client: testClient("termii"), BaseURL: termii.srv.URL, APIKey: "k"},
		"ebulksms": &EBulkSMS{Client: testClient("ebulksms"), BaseURL: ebulk.srv.URL, APIKey: "k", Username: "u"},
	}, "ebulksms, termii", zap.NewNop())
	name, _, _, err := chain.SendSMS(context.Background(), "+2348012345678", "hi", "")
	if err != nil || name != "ebulksms" {
		t.Fatalf("expected ebulksms first per SMS_PROVIDER_CHAIN, got name=%q err=%v", name, err)
	}
	if got := termii.calls.Load(); got != 0 {
		t.Fatalf("termii must not be called, got %d", got)
	}
}

func TestFailoverEmptyChain(t *testing.T) {
	chain := NewFailover(map[string]SMSProvider{}, "", zap.NewNop())
	if _, _, _, err := chain.SendSMS(context.Background(), "+2348", "hi", ""); err == nil {
		t.Fatal("empty chain must error")
	}
}

func TestPriceTierAnnotations(t *testing.T) {
	// SPEC-W12: at=1.0, termii=1.0, ebulks=0.85 (relative, reporting only).
	if PriceTier("africastalking") != 1.0 || PriceTier("termii") != 1.0 || PriceTier("ebulksms") != 0.85 {
		t.Fatalf("wrong price tiers: at=%v termii=%v ebulksms=%v",
			PriceTier("africastalking"), PriceTier("termii"), PriceTier("ebulksms"))
	}
	if PriceTier("unknown") != 1.0 {
		t.Fatalf("unknown provider must default to 1.0, got %v", PriceTier("unknown"))
	}
	chain := NewFailover(map[string]SMSProvider{
		"termii":   &Termii{Client: testClient("termii")},
		"ebulksms": &EBulkSMS{Client: testClient("ebulksms")},
	}, "termii,ebulksms", zap.NewNop())
	entries := chain.Entries()
	if len(entries) != 2 || entries[0].Name != "termii" || entries[1].Name != "ebulksms" {
		t.Fatalf("unexpected chain entries: %+v", entries)
	}
	if entries[1].PriceTier != 0.85 {
		t.Fatalf("ebulksms tier must be 0.85, got %v", entries[1].PriceTier)
	}
}

func TestFailoverMetersPerProvider(t *testing.T) {
	// Metering: each provider's Client records
	// messaging_gateway_sends_total{provider,result} — the sms_send event
	// with the provider label (existing metering, unchanged).
	reg := metrics.New()
	mkClient := func(name string) *Client {
		c := NewClient(name, reg, zap.NewNop())
		c.sleep = func(context.Context, int) {}
		return c
	}
	at := newChainServer(http.StatusInternalServerError)
	defer at.Close()
	termii := newChainServer(http.StatusOK)
	defer termii.Close()
	chain := NewFailover(map[string]SMSProvider{
		"africastalking": &AfricasTalking{Client: mkClient("africastalking"), BaseURL: at.srv.URL, APIKey: "k", Username: "u"},
		"termii":         &Termii{Client: mkClient("termii"), BaseURL: termii.srv.URL, APIKey: "k"},
	}, "", zap.NewNop())
	if _, _, _, err := chain.SendSMS(context.Background(), "+2348012345678", "hi", ""); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	reg.Render(&sb)
	for _, want := range []string{
		`messaging_gateway_sends_total{provider="africastalking",result="provider_error"} 1`,
		`messaging_gateway_sends_total{provider="termii",result="success"} 1`,
	} {
		if !strings.Contains(sb.String(), want) {
			t.Fatalf("missing counter %q:\n%s", want, sb.String())
		}
	}
}
