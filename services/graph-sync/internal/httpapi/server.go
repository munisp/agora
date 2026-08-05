// Package httpapi exposes graph-sync's HTTP sidecar (workforce
// conventions):
//   - GET /healthz  liveness + FalkorDB ping
//   - GET /metrics  Prometheus text format
package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/opendesk/graph-sync/internal/metrics"
)

// Pinger abstracts the graph-store liveness check.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Server bundles the sidecar dependencies.
type Server struct {
	Graph   Pinger
	Metrics *metrics.Registry
	// EmbedDegraded reports Ollama reachability (nil → embeddings disabled;
	// surfaced on /healthz as informational only — the service is healthy
	// in degraded mode, SPEC-W28 §4 graceful degradation).
	EmbedDegraded func() bool
}

// Router builds the chi router.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", s.healthz)
	r.Get("/metrics", s.metricsHandler)
	return r
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{"status": "ok"}
	if s.EmbedDegraded != nil && s.EmbedDegraded() {
		status["embeddings"] = "degraded (ollama unreachable; merge proposals skipped)"
	}
	if s.Graph != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.Graph.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, s.Metrics.Render())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
