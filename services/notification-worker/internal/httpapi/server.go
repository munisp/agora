// Package httpapi exposes /healthz and small dev endpoints for the worker.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// Server is the small HTTP sidecar of the worker process.
type Server struct {
	Temporal  client.Client
	TaskQueue string
	Log       *zap.Logger

	// Outbound webhook platform (Wave 5 #10); nil Webhooks disables the
	// /v1/webhooks routes (503).
	Webhooks               WebhookStore
	ResolveTenant          func(ctx context.Context, slug string) (TenantRef, error)
	WebhookSigningRequired bool

	// DND is the SPEC-W12 DND registry (NCC 2442 + tenant opt-outs); nil
	// disables the /v1/dnd routes (503).
	DND DNDStore
}

// NewRouter builds the chi router.
func NewRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// POST /dev/trigger-reminder starts a ReminderWorkflow with overridden
	// (short) delays for manual testing.
	r.Post("/dev/trigger-reminder", s.triggerReminder)
	// POST /dev/trigger-onboarding starts a TenantOnboardingWorkflow.
	r.Post("/dev/trigger-onboarding", s.triggerOnboarding)
	// POST /dev/trigger-twin-cleanup starts a TwinCleanupWorkflow (24h →
	// delete the twin tenant). Invoked by identity-service's twin endpoint
	// via Dapr service invocation (SPEC-W3 §3 innovation 12).
	r.Post("/dev/trigger-twin-cleanup", s.triggerTwinCleanup)
	// POST /v1/signals delivers a signal to a running workflow (staff UI:
	// IntakeCompleted / Responded on the pack workflows, SPEC-CRM §C2).
	r.Post("/v1/signals", s.sendSignal)

	// Outbound webhook platform (Wave 5 #10): tenant-scoped via
	// X-Tenant-Slug; reached from the UI through APISIX /api/notifications/*.
	r.Route("/v1/webhooks", func(r chi.Router) {
		r.Use(s.tenantMiddleware)
		r.Post("/", s.createWebhook)
		r.Get("/", s.listWebhooks)
		r.Delete("/{id}", s.deleteWebhook)
		r.Get("/{id}/deliveries", s.listWebhookDeliveries)
	})

	// DND registry (SPEC-W12 Agent B): NCC 2442 global import (admin via
	// APISIX), opt-out honor, suppression check.
	r.Route("/v1/dnd", func(r chi.Router) {
		r.Post("/import", s.importDND)
		r.Delete("/{phone}", s.deleteDND)
		r.Get("/check", s.checkDND)
	})
	return r
}

// signalRequest is the body of POST /v1/signals. Payload is optional (the
// IntakeCompleted / Responded / NoShow signals carry no payload); when given
// it must be a JSON value, e.g. {"type":"cancelled"} for "booking-event".
type signalRequest struct {
	WorkflowID string          `json:"workflow_id"`
	Signal     string          `json:"signal"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// sendSignal forwards the signal via the Temporal client. A workflow that is
// not running maps to 404; payload-less signals send no argument.
func (s *Server) sendSignal(w http.ResponseWriter, r *http.Request) {
	var req signalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	if req.WorkflowID == "" || req.Signal == "" {
		http.Error(w, `{"error":"workflow_id and signal are required"}`, http.StatusBadRequest)
		return
	}
	var payload any
	if len(req.Payload) > 0 && string(req.Payload) != "null" {
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			http.Error(w, `{"error":"payload must be valid JSON"}`, http.StatusBadRequest)
			return
		}
	}
	if err := s.Temporal.SignalWorkflow(r.Context(), req.WorkflowID, "", req.Signal, payload); err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			http.Error(w, `{"error":"workflow not found or already completed"}`, http.StatusNotFound)
			return
		}
		s.Log.Error("signal workflow", zap.String("workflow_id", req.WorkflowID), zap.Error(err))
		http.Error(w, `{"error":"failed to signal workflow"}`, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"signalled": req.WorkflowID, "signal": req.Signal})
}

type triggerReminderRequest struct {
	BookingID    string    `json:"booking_id"`
	TenantID     string    `json:"tenant_id"`
	TenantSlug   string    `json:"tenant_slug"`
	ContactName  string    `json:"contact_name"`
	ContactEmail string    `json:"contact_email"`
	ContactPhone string    `json:"contact_phone"`
	StartsAt     time.Time `json:"starts_at"`
	// DelaysSeconds replaces T-24h/T-1h for testing (e.g. [5, 10]).
	DelaysSeconds []int `json:"delays_seconds"`
}

func (s *Server) triggerReminder(w http.ResponseWriter, r *http.Request) {
	var req triggerReminderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	if req.BookingID == "" {
		req.BookingID = uuid.NewString()
	}
	if req.StartsAt.IsZero() {
		req.StartsAt = time.Now().Add(time.Hour)
	}
	delays := make([]time.Duration, 0, len(req.DelaysSeconds))
	for _, d := range req.DelaysSeconds {
		delays = append(delays, time.Duration(d)*time.Second)
	}
	if len(delays) == 0 {
		delays = []time.Duration{5 * time.Second, 10 * time.Second}
	}
	run, err := s.Temporal.ExecuteWorkflow(r.Context(), client.StartWorkflowOptions{
		ID:        "reminder-dev-" + req.BookingID + "-" + uuid.NewString()[:8],
		TaskQueue: s.TaskQueue,
	}, "ReminderWorkflow", workflows.ReminderInput{
		BookingID:         req.BookingID,
		TenantID:          req.TenantID,
		TenantSlug:        req.TenantSlug,
		ContactName:       req.ContactName,
		ContactEmail:      req.ContactEmail,
		ContactPhone:      req.ContactPhone,
		StartsAt:          req.StartsAt,
		DevOverrideDelays: delays,
	})
	if err != nil {
		s.Log.Error("start dev reminder", zap.Error(err))
		http.Error(w, `{"error":"failed to start workflow"}`, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"workflow_id": run.GetID(), "run_id": run.GetRunID()})
}

type triggerOnboardingRequest struct {
	TenantID string `json:"tenant_id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Plan     string `json:"plan"`
	Industry string `json:"industry"`
}

func (s *Server) triggerOnboarding(w http.ResponseWriter, r *http.Request) {
	var req triggerOnboardingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Slug == "" {
		http.Error(w, `{"error":"invalid JSON body (slug required)"}`, http.StatusBadRequest)
		return
	}
	run, err := s.Temporal.ExecuteWorkflow(r.Context(), client.StartWorkflowOptions{
		ID:        "onboarding-" + req.Slug,
		TaskQueue: s.TaskQueue,
	}, "TenantOnboardingWorkflow", workflows.OnboardingInput{
		TenantID: req.TenantID, Slug: req.Slug, Name: req.Name, Plan: req.Plan, Industry: req.Industry,
	})
	if err != nil {
		s.Log.Error("start onboarding", zap.Error(err))
		http.Error(w, `{"error":"failed to start workflow"}`, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"workflow_id": run.GetID(), "run_id": run.GetRunID()})
}

type triggerTwinCleanupRequest struct {
	TenantID   string  `json:"tenant_id"`
	Slug       string  `json:"slug"`
	TwinOf     string  `json:"twin_of"`
	DelayHours float64 `json:"delay_hours,omitempty"`
}

func (s *Server) triggerTwinCleanup(w http.ResponseWriter, r *http.Request) {
	var req triggerTwinCleanupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Slug == "" {
		http.Error(w, `{"error":"invalid JSON body (slug required)"}`, http.StatusBadRequest)
		return
	}
	run, err := s.Temporal.ExecuteWorkflow(r.Context(), client.StartWorkflowOptions{
		ID:        "twin-cleanup-" + req.Slug,
		TaskQueue: s.TaskQueue,
	}, "TwinCleanupWorkflow", workflows.TwinCleanupInput{
		TenantID: req.TenantID, Slug: req.Slug, TwinOf: req.TwinOf, DelayHours: req.DelayHours,
	})
	if err != nil {
		s.Log.Error("start twin cleanup", zap.Error(err))
		http.Error(w, `{"error":"failed to start workflow"}`, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"workflow_id": run.GetID(), "run_id": run.GetRunID()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
