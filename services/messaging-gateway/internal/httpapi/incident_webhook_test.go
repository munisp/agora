package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/opendesk/messaging-gateway/internal/metrics"
	"go.uber.org/zap"
)

// fakeIngest captures forwarded ingest envelopes.
type fakeIngest struct {
	mu    sync.Mutex
	calls [][]byte
	err   error
}

func (f *fakeIngest) Ingest(_ context.Context, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, body)
	return f.err
}

func (f *fakeIngest) captured() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.calls))
	copy(out, f.calls)
	return out
}

func newIncidentServer(fi IncidentIngester) *Server {
	return &Server{
		IncidentSecrets: map[string]string{"acme-ng": "s3cret"},
		IncidentIngest:  fi,
		Metrics:         metrics.New(),
		Log:             zap.NewNop(),
	}
}

const validIncidentBody = `{
  "tenant_slug": "acme-ng",
  "secret": "s3cret",
  "incident": {
    "incident_type": "fire",
    "severity": "critical",
    "callback_number": "+2348012345678",
    "location": {"lat": 6.45, "lng": 3.40, "accuracy_m": 12, "source": "gps", "address_text": "12 Marina Rd"}
  }
}`

// Valid post: 200 and the incident forwarded to booking-service's ingest
// (tenant addressing + incident payload preserved).
func TestIncidentWebhookValid(t *testing.T) {
	fi := &fakeIngest{}
	s := newIncidentServer(fi)
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/incidents", validIncidentBody, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	calls := fi.captured()
	if len(calls) != 1 {
		t.Fatalf("forwards = %d, want 1", len(calls))
	}
	var fwd map[string]json.RawMessage
	if err := json.Unmarshal(calls[0], &fwd); err != nil {
		t.Fatalf("forward not JSON: %v", err)
	}
	var slug string
	if err := json.Unmarshal(fwd["tenant_slug"], &slug); err != nil || slug != "acme-ng" {
		t.Fatalf("tenant_slug = %q (%v)", slug, err)
	}
	var inc map[string]any
	if err := json.Unmarshal(fwd["incident"], &inc); err != nil {
		t.Fatalf("incident not an object: %v", err)
	}
	if inc["incident_type"] != "fire" || inc["severity"] != "critical" {
		t.Fatalf("incident payload mangled: %v", inc)
	}
}

// Bad secret → 403 (authentication failure), nothing forwarded.
func TestIncidentWebhookBadSecret(t *testing.T) {
	fi := &fakeIngest{}
	s := newIncidentServer(fi)
	body := `{"tenant_slug":"acme-ng","secret":"wrong","incident":{"incident_type":"fire"}}`
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/incidents", body, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if len(fi.captured()) != 0 {
		t.Fatal("bad secret must not be forwarded")
	}
}

// Unknown tenant → 403.
func TestIncidentWebhookUnknownTenant(t *testing.T) {
	fi := &fakeIngest{}
	s := newIncidentServer(fi)
	body := `{"tenant_slug":"ghost","secret":"s3cret","incident":{"incident_type":"fire"}}`
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/incidents", body, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

// Missing tenant addressing → 403.
func TestIncidentWebhookNoTenant(t *testing.T) {
	fi := &fakeIngest{}
	s := newIncidentServer(fi)
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/incidents",
		`{"secret":"s3cret","incident":{"incident_type":"fire"}}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

// Garbage JSON → 400 (the only client-error case).
func TestIncidentWebhookGarbageJSON(t *testing.T) {
	fi := &fakeIngest{}
	s := newIncidentServer(fi)
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/incidents", `{not json`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if len(fi.captured()) != 0 {
		t.Fatal("garbage must not be forwarded")
	}
}

// Valid JSON but no incident signal → 200 ignored, nothing forwarded.
func TestIncidentWebhookNoSignalIgnored(t *testing.T) {
	fi := &fakeIngest{}
	s := newIncidentServer(fi)
	body := `{"tenant_slug":"acme-ng","secret":"s3cret","incident":{"severity":"low"}}`
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/incidents", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp["status"] != "ignored" {
		t.Fatalf("expected ignored, got %s", rec.Body.String())
	}
	if len(fi.captured()) != 0 {
		t.Fatal("signal-less incident must not be forwarded")
	}
}

// Missing incident block → 200 ignored.
func TestIncidentWebhookMissingIncidentIgnored(t *testing.T) {
	fi := &fakeIngest{}
	s := newIncidentServer(fi)
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/incidents",
		`{"tenant_slug":"acme-ng","secret":"s3cret"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if len(fi.captured()) != 0 {
		t.Fatal("missing incident must not be forwarded")
	}
}

// tenant_id addressing works against the same secret map.
func TestIncidentWebhookTenantIDAddressing(t *testing.T) {
	fi := &fakeIngest{}
	s := newIncidentServer(fi)
	s.IncidentSecrets["9f1c2d3e-0000-4000-8000-abcdefabcdef"] = "other-secret"
	body := `{"tenant_id":"9f1c2d3e-0000-4000-8000-abcdefabcdef","secret":"other-secret","incident":{"narrative_summary":"smoke alarm zone 4"}}`
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/incidents", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(fi.captured()) != 1 {
		t.Fatalf("forwards = %d, want 1", len(fi.captured()))
	}
}

// INCIDENT_WEBHOOK_SECRETS parsing.
func TestParseIncidentSecrets(t *testing.T) {
	m, err := ParseIncidentSecrets(`{"acme-ng":"s3","b":"x"}`)
	if err != nil || len(m) != 2 || m["acme-ng"] != "s3" {
		t.Fatalf("parse = %v, %v", m, err)
	}
	if m, err := ParseIncidentSecrets(""); err != nil || len(m) != 0 {
		t.Fatalf("empty = %v, %v", m, err)
	}
	if _, err := ParseIncidentSecrets(`{bad`); err == nil {
		t.Fatal("invalid JSON must error")
	}
}

// Dapr invoke base resolution: override wins, else the sidecar URL.
func TestResolveIncidentBase(t *testing.T) {
	if got := ResolveIncidentBase("http://booking:7002", 3500); got != "http://booking:7002" {
		t.Fatalf("override = %q", got)
	}
	if got := ResolveIncidentBase("", 3501); got != "http://127.0.0.1:3501/v1.0/invoke/booking/method" {
		t.Fatalf("dapr base = %q", got)
	}
}
