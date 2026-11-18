package callsummary

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSummarizeReadsTranscriptAndAsksModel(t *testing.T) {
	var gotTenant, gotPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/conversations/"):
			gotTenant = r.Header.Get("X-Tenant-Slugs")
			_ = json.NewEncoder(w).Encode(map[string]any{"turns": []map[string]string{
				{"role": "user", "text": "I want to open an RSA account"},
				{"role": "tool", "text": "{internal}"},
				{"role": "agent", "text": "Booked for Tuesday at 9."},
			}})
		case r.URL.Path == "/chat/completions":
			var body struct {
				Messages []struct{ Content string } `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotPrompt = body.Messages[1].Content
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{
				{"message": map[string]string{"content": "**The caller** wanted an RSA account.\n\nIt was booked for Tuesday at 9."}}}})
		}
	}))
	defer srv.Close()
	s := New(srv.URL, srv.URL, "m", 5*time.Second)
	s.Settle = time.Nanosecond
	got := s.Summarize(context.Background(), "c1", "t1")
	if got != "The caller wanted an RSA account. It was booked for Tuesday at 9." {
		t.Fatalf("summary = %q", got)
	}
	if gotTenant != "t1" {
		t.Errorf("tenant header = %q", gotTenant)
	}
	if gotPrompt != "Caller: I want to open an RSA account\nReceptionist: Booked for Tuesday at 9.\n" {
		t.Errorf("transcript sent = %q", gotPrompt)
	}
}

func TestSummarizeFailsQuietly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	if got := New(srv.URL, srv.URL, "m", time.Second).Summarize(context.Background(), "c", "t"); got != "" {
		t.Errorf("want empty on failure, got %q", got)
	}
	if got := New("", "", "", time.Second).Summarize(context.Background(), "c", "t"); got != "" {
		t.Errorf("unconfigured must return empty, got %q", got)
	}
}

func TestCleanRemovesIdentityNumbersButKeepsPhones(t *testing.T) {
	got := Clean("The caller offered BVN 22334455667; we confirmed 08065553108.")
	if strings.Contains(got, "22334455667") || !strings.Contains(got, "08065553108") {
		t.Errorf("Clean = %q", got)
	}
}
