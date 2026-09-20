// SPEC-W45 tests: K14 USSD per-phone rate limit, ORPH O2 healthz upstream
// reachability reporting.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opendesk/messaging-gateway/internal/channel"
	"github.com/opendesk/messaging-gateway/internal/metrics"
	"go.uber.org/zap"
)

// K14: the USSD handler rate-limits per phoneNumber (sliding window);
// other phones are unaffected and the window slides open again.
func TestUSSDPerPhoneRateLimit(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	limiter := NewPhoneRateLimiter(3, time.Minute)
	limiter.SetClock(clock)

	conv := &fakeConversation{resp: channel.USSDTurnResponse{Reply: "ok", Continue: true}}
	s := newUSSDServer(channel.NewMemoryUSSDStore(), &fakeMenus{menu: nil}, conv)
	s.USSD.RateLimiter = limiter

	phoneA := "+2348011111111"
	phoneB := "+2348022222222"
	// First 3 callbacks from phone A pass.
	for i := 0; i < 3; i++ {
		rec := ussdPost(t, s.Router(), "sess-rl", "*384*123#", phoneA, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("callback %d within limit must pass, got %d (%s)", i+1, rec.Code, rec.Body.String())
		}
	}
	// 4th within the same window → 429.
	rec := ussdPost(t, s.Router(), "sess-rl", "*384*123#", phoneA, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("callback beyond limit must be 429, got %d (%s)", rec.Code, rec.Body.String())
	}
	// A different phone has its own window.
	rec = ussdPost(t, s.Router(), "sess-rl2", "*384*123#", phoneB, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("per-phone limit must be independent, got %d (%s)", rec.Code, rec.Body.String())
	}
	// The window slides: 61s later phone A is admitted again.
	now = now.Add(61 * time.Second)
	rec = ussdPost(t, s.Router(), "sess-rl", "*384*123#", phoneA, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("callback after the window must pass, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// K14: the handler lazily builds the default limiter (30/min) when none is
// injected.
func TestUSSDDefaultRateLimiterBuilt(t *testing.T) {
	conv := &fakeConversation{resp: channel.USSDTurnResponse{Reply: "ok", Continue: true}}
	s := newUSSDServer(channel.NewMemoryUSSDStore(), &fakeMenus{menu: nil}, conv)
	if s.USSD.RateLimiter != nil {
		t.Fatal("limiter must be lazy")
	}
	for i := 0; i < ussdDefaultRatePerMinute; i++ {
		rec := ussdPost(t, s.Router(), "sess-def", "*384*123#", "+2348033333333", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("callback %d within default limit must pass, got %d", i+1, rec.Code)
		}
	}
	rec := ussdPost(t, s.Router(), "sess-def", "*384*123#", "+2348033333333", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("callback beyond the default 30/min must be 429, got %d", rec.Code)
	}
	if s.USSD.RateLimiter == nil {
		t.Fatal("default limiter must have been built")
	}
}

// PhoneRateLimiter unit behaviour: exact window boundary, expiry sweep.
func TestPhoneRateLimiterUnit(t *testing.T) {
	now := time.Now()
	l := NewPhoneRateLimiter(2, time.Minute)
	l.SetClock(func() time.Time { return now })

	if !l.Allow("k") || !l.Allow("k") {
		t.Fatal("first two events must pass")
	}
	if l.Allow("k") {
		t.Fatal("third event in window must be rejected")
	}
	// Half-window slide: still 2 live hits → rejected.
	now = now.Add(30 * time.Second)
	if l.Allow("k") {
		t.Fatal("events inside the sliding window must still count")
	}
	// Full window slide: admitted again.
	now = now.Add(31 * time.Second)
	if !l.Allow("k") {
		t.Fatal("expired hits must free the window")
	}
}

// ORPH O2: /healthz reports upstream base reachability (warn-level) without
// changing the 200 liveness answer.
func TestHealthzReportsUpstreams(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok")) //nolint:errcheck
	}))
	defer up.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	downURL := down.URL
	down.Close() // nothing listening now

	s := &Server{
		Metrics: metrics.New(),
		Log:     zap.NewNop(),
		Upstreams: []UpstreamCheck{
			{Name: "conversation", Base: up.URL},
			{Name: "voice", Base: downURL},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz must stay 200 (warn-level), got %d", rec.Code)
	}
	var body struct {
		Status    string            `json:"status"`
		Upstreams map[string]string `json:"upstreams"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz body: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("status = %q", body.Status)
	}
	if body.Upstreams["conversation"] != "ok" {
		t.Fatalf("reachable upstream = %q", body.Upstreams["conversation"])
	}
	if body.Upstreams["voice"] != "unreachable" {
		t.Fatalf("unreachable upstream = %q", body.Upstreams["voice"])
	}
}

// ORPH O2: ProbeBase treats ANY HTTP response as reachable and only
// transport failure as unreachable.
func TestProbeBase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // still "reachable"
	}))
	defer srv.Close()
	if err := ProbeBase(context.Background(), srv.URL); err != nil {
		t.Fatalf("http 500 base must count as reachable: %v", err)
	}
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	if err := ProbeBase(context.Background(), deadURL); err == nil {
		t.Fatal("closed base must be unreachable")
	} else if !strings.Contains(err.Error(), deadURL) {
		t.Fatalf("error must name the base: %v", err)
	}
}
