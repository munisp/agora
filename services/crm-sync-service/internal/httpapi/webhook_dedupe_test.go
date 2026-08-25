package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opendesk/crm-sync-service/internal/daprc"
	"github.com/opendesk/crm-sync-service/internal/metrics"
	"go.uber.org/zap"
)

// SPEC-W43 K-13: webhook replay freshness at the HTTP boundary — a
// duplicate event id is acknowledged (200) but NOT re-published; a dedupe
// store outage fails loud (502) so Twenty retries.

// fakeDedupe is an in-memory WebhookDedupe.
type fakeDedupe struct {
	mu   sync.Mutex
	seen map[string]bool
	err  error
}

func (f *fakeDedupe) MarkWebhookSeen(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[id] {
		return false, nil
	}
	f.seen[id] = true
	return true, nil
}

// daprSpy stands up an httptest server answering Dapr publish calls and
// counting them.
type daprSpy struct {
	mu        sync.Mutex
	published int
	client    *daprc.Client
}

func newDaprSpy(t *testing.T) *daprSpy {
	t.Helper()
	spy := &daprSpy{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1.0/publish/") {
			spy.mu.Lock()
			spy.published++
			spy.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	spy.client = daprc.New(u.Hostname(), port)
	return spy
}

func (s *daprSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.published
}

func newWebhookServer(dap *daprc.Client, dedupe WebhookDedupe) *Server {
	return &Server{
		Dapr:           dap,
		PubSubName:     "pubsub-kafka",
		CRMEventsTopic: "opendesk.crm.events",
		WebhookSecret:  "s3cret",
		Dedupe:         dedupe,
		Metrics:        metrics.New(),
		Log:            zap.NewNop(),
	}
}

func postWebhook(t *testing.T, h http.Handler, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/twenty", strings.NewReader(body))
	req.Header.Set("X-Twenty-Webhook-Signature", signHex("s3cret", []byte(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestTwentyWebhookDuplicateIgnored(t *testing.T) {
	spy := newDaprSpy(t)
	dedupe := &fakeDedupe{}
	h := newWebhookServer(spy.client, dedupe).Router()

	body := `{"event":"person.created","id":"evt-42"}`
	code, _ := postWebhook(t, h, body)
	if code != http.StatusAccepted {
		t.Fatalf("first delivery code = %d, want 202", code)
	}
	if spy.count() != 1 {
		t.Fatalf("published = %d, want 1", spy.count())
	}

	code, out := postWebhook(t, h, body)
	if code != http.StatusOK {
		t.Fatalf("duplicate code = %d, want 200", code)
	}
	if out["duplicate"] != true || out["status"] != "ignored" {
		t.Fatalf("duplicate response = %v", out)
	}
	if spy.count() != 1 {
		t.Fatalf("published = %d after duplicate, want still 1 (no re-publish)", spy.count())
	}

	// A different event id is processed.
	code, _ = postWebhook(t, h, `{"event":"person.created","id":"evt-43"}`)
	if code != http.StatusAccepted {
		t.Fatalf("new id code = %d, want 202", code)
	}
	if spy.count() != 2 {
		t.Fatalf("published = %d, want 2", spy.count())
	}

	// No id field: exact body replay dedupes via the body digest.
	noID := `{"event":"task.updated"}`
	code, _ = postWebhook(t, h, noID)
	if code != http.StatusAccepted {
		t.Fatalf("id-less first delivery code = %d, want 202", code)
	}
	before := spy.count()
	code, out = postWebhook(t, h, noID)
	if code != http.StatusOK || out["duplicate"] != true {
		t.Fatalf("id-less replay code=%d out=%v, want 200 duplicate", code, out)
	}
	if spy.count() != before {
		t.Fatal("id-less replay must not re-publish")
	}
}

func TestTwentyWebhookDedupeOutageFailsLoud(t *testing.T) {
	spy := newDaprSpy(t)
	dedupe := &fakeDedupe{err: fmt.Errorf("db down")}
	h := newWebhookServer(spy.client, dedupe).Router()

	code, _ := postWebhook(t, h, `{"event":"person.created","id":"evt-1"}`)
	if code != http.StatusBadGateway {
		t.Fatalf("dedupe outage code = %d, want 502 (fail loud so Twenty retries)", code)
	}
	if spy.count() != 0 {
		t.Fatalf("published = %d, want 0 (nothing published without dedupe record)", spy.count())
	}
}
