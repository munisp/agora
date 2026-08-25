package httpapi

// SPEC-W44 tests: K2 internal-token gate, K1 role/tenant bindings, S1-F7-04
// signals gate, N-01 dev-endpoints gate, F15-05/N-07 dependency-aware
// healthz.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// fakeTemporal embeds client.Client (nil) and overrides only what the
// handlers call.
type fakeTemporal struct {
	client.Client
	signalledWf     string
	signalledSignal string
	signalErr       error
	executed        []string
}

func (f *fakeTemporal) SignalWorkflow(_ context.Context, workflowID, _ string, signalName string, _ interface{}) error {
	f.signalledWf, f.signalledSignal = workflowID, signalName
	return f.signalErr
}

type fakeWorkflowRun struct{ client.WorkflowRun }

func (fakeWorkflowRun) GetID() string    { return "wf-1" }
func (fakeWorkflowRun) GetRunID() string { return "run-1" }

func (f *fakeTemporal) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, _ interface{}, _ ...interface{}) (client.WorkflowRun, error) {
	f.executed = append(f.executed, opts.ID)
	return fakeWorkflowRun{}, nil
}

func newAuthedServer() (*Server, *fakeTemporal) {
	ft := &fakeTemporal{}
	return &Server{Log: zap.NewNop(), Temporal: ft, TaskQueue: "q", InternalToken: "tok-1"}, ft
}

func signalReq(t *testing.T, s *Server, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/signals", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	NewRouter(s).ServeHTTP(rec, req)
	return rec
}

func TestSignalsInternalTokenGate(t *testing.T) {
	// K2 fail-closed: token env unset → 503 even with the right header.
	s, _ := newAuthedServer()
	s.InternalToken = ""
	rec := signalReq(t, s, `{"workflow_id":"pack-b1","signal":"IntakeCompleted","tenant_slug":"acme"}`,
		map[string]string{"X-Internal-Token": "tok-1"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	// Missing / wrong token → 401.
	s, _ = newAuthedServer()
	rec = signalReq(t, s, `{}`, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	rec = signalReq(t, s, `{}`, map[string]string{"X-Internal-Token": "wrong"})
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestSignalsTenantAndPrefixGates(t *testing.T) {
	s, ft := newAuthedServer()
	auth := map[string]string{"X-Internal-Token": "tok-1"}

	// tenant_slug required (400).
	rec := signalReq(t, s, `{"workflow_id":"pack-b1","signal":"IntakeCompleted"}`, auth)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// tenant_slug not in X-Tenant-Slugs → 403 (K1 binding).
	rec = signalReq(t, s, `{"workflow_id":"pack-b1","signal":"IntakeCompleted","tenant_slug":"acme"}`,
		withHeaders(auth, "X-Tenant-Slugs", "other"))
	require.Equal(t, http.StatusForbidden, rec.Code)

	// No gateway claim at all → fail-closed 403.
	rec = signalReq(t, s, `{"workflow_id":"pack-b1","signal":"IntakeCompleted","tenant_slug":"acme"}`, auth)
	require.Equal(t, http.StatusForbidden, rec.Code)

	// Workflow id outside the prefix allowlist → 403.
	rec = signalReq(t, s, `{"workflow_id":"twin-cleanup-acme","signal":"IntakeCompleted","tenant_slug":"acme"}`,
		withHeaders(auth, "X-Tenant-Slugs", "acme"))
	require.Equal(t, http.StatusForbidden, rec.Code)

	// Bound tenant + allowed prefix → 202 and the signal is forwarded.
	rec = signalReq(t, s, `{"workflow_id":"pack-b1","signal":"IntakeCompleted","tenant_slug":"acme"}`,
		withHeaders(auth, "X-Tenant-Slugs", "acme,beta"))
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, "pack-b1", ft.signalledWf)
	require.Equal(t, "IntakeCompleted", ft.signalledSignal)

	// The {tenant} prefix expansion allows slug-prefixed workflow ids.
	rec = signalReq(t, s, `{"workflow_id":"acme-custom-1","signal":"Responded","tenant_slug":"acme"}`,
		withHeaders(auth, "X-Tenant-Slugs", "acme"))
	require.Equal(t, http.StatusAccepted, rec.Code)
}

func TestSignalsDevEscape(t *testing.T) {
	s, _ := newAuthedServer()
	s.TrustDirectTenancy = true // OPENDESK_TRUST_DIRECT_TENANT=1
	rec := signalReq(t, s, `{"workflow_id":"pack-b1","signal":"IntakeCompleted","tenant_slug":"acme"}`,
		map[string]string{"X-Internal-Token": "tok-1"})
	require.Equal(t, http.StatusAccepted, rec.Code)
}

func withHeaders(h map[string]string, k, v string) map[string]string {
	out := map[string]string{}
	for kk, vv := range h {
		out[kk] = vv
	}
	out[k] = v
	return out
}

func devReq(s *Server, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	NewRouter(s).ServeHTTP(rec, req)
	return rec
}

func TestDevEndpointsCompiledOut(t *testing.T) {
	// N-01: without OPENDESK_DEV_ENDPOINTS the /dev/* routes do not exist
	// (404 — not even the token gate is reached).
	s, _ := newAuthedServer()
	rec := devReq(s, "/dev/trigger-onboarding", `{"slug":"x"}`, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// With the flag + the K2 internal token the route exists and drives the
	// workflow start.
	s, ft := newAuthedServer()
	s.DevEndpoints = true
	rec = devReq(s, "/dev/trigger-onboarding", `{"slug":"acme"}`,
		map[string]string{"X-Internal-Token": "tok-1"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, []string{"onboarding-acme"}, ft.executed)
}

func TestDevEndpointsInternalTokenGate(t *testing.T) {
	// V2-D2: when OPENDESK_DEV_ENDPOINTS=1 the /dev/* group is additionally
	// gated on X-Internal-Token = NOTIFICATION_INTERNAL_TOKEN (constant-time,
	// fail-closed) — identity-service's twin caller already sends it.

	// Token env unset → 503 (misconfiguration is never an open door), even
	// with a header supplied.
	s, _ := newAuthedServer()
	s.DevEndpoints = true
	s.InternalToken = ""
	rec := devReq(s, "/dev/trigger-twin-cleanup", `{"slug":"acme-twin-1"}`,
		map[string]string{"X-Internal-Token": "anything"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	// Missing / wrong token → 401, and NO workflow is started.
	s, ft := newAuthedServer()
	s.DevEndpoints = true
	rec = devReq(s, "/dev/trigger-twin-cleanup", `{"slug":"acme-twin-1"}`, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	rec = devReq(s, "/dev/trigger-reminder", `{"booking_id":"b1"}`,
		map[string]string{"X-Internal-Token": "wrong"})
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Empty(t, ft.executed)

	// Correct token → 200-class and the workflow executes.
	rec = devReq(s, "/dev/trigger-twin-cleanup", `{"slug":"acme-twin-1"}`,
		map[string]string{"X-Internal-Token": "tok-1"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, []string{"twin-cleanup-acme-twin-1"}, ft.executed)
}

func TestHealthzDependencyAware(t *testing.T) {
	// No deps configured → plain ok (dependency checks skipped).
	s := &Server{Log: zap.NewNop()}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	NewRouter(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"status":"ok"`)

	// All deps healthy → 200 with per-check detail.
	s = &Server{
		Log:            zap.NewNop(),
		HealthPostgres: func(context.Context) error { return nil },
		HealthTemporal: func(context.Context) error { return nil },
	}
	rec = httptest.NewRecorder()
	NewRouter(s).ServeHTTP(httptest.NewRecorder(), req) // second router share ok
	rec = httptest.NewRecorder()
	NewRouter(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"postgres":{"status":"ok"}`)

	// A failed dependency → 503 + detail (F15-05/N-07).
	s = &Server{
		Log:            zap.NewNop(),
		HealthPostgres: func(context.Context) error { return errors.New("connection refused") },
		HealthTemporal: func(context.Context) error { return nil },
	}
	rec = httptest.NewRecorder()
	NewRouter(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), `"status":"degraded"`)
	require.Contains(t, rec.Body.String(), "connection refused")
}

func TestMetricsRouteMounted(t *testing.T) {
	s := &Server{Log: zap.NewNop()}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	NewRouter(s).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "go_goroutines") // promhttp default registry
}
