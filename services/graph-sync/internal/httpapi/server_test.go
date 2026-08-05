package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opendesk/graph-sync/internal/metrics"
	"github.com/stretchr/testify/require"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestHealthz_OK(t *testing.T) {
	s := &Server{Graph: fakePinger{}, Metrics: metrics.New()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "ok", body["status"])
}

func TestHealthz_GraphDown_503(t *testing.T) {
	s := &Server{Graph: fakePinger{err: errors.New("connection refused")}, Metrics: metrics.New()}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestHealthz_EmbeddingDegraded_StillOK(t *testing.T) {
	// Ollama degradation is informational only (SPEC §4 graceful degrade).
	s := &Server{Graph: fakePinger{}, Metrics: metrics.New(), EmbedDegraded: func() bool { return true }}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "degraded")
}

func TestMetrics_RendersPrometheusText(t *testing.T) {
	reg := metrics.New()
	reg.Inc("events_processed.com.opendesk.booking.BookingCreated")
	s := &Server{Graph: fakePinger{}, Metrics: reg}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
	require.Contains(t, rec.Body.String(), `graph_sync_counter{name="events_processed.com.opendesk.booking.BookingCreated"} 1`)
}
