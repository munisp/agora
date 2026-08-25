// Package httpapi exposes /healthz, /metrics and small internal/tenant
// endpoints for the worker.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// Server is the small HTTP sidecar of the worker process.
type Server struct {
	Temporal  client.Client
	TaskQueue string
	Log       *zap.Logger

	// InternalToken is NOTIFICATION_INTERNAL_TOKEN (K2): required on the
	// machine-only /v1/signals route (constant-time compare; unset fails
	// closed with 503).
	InternalToken string
	// DevEndpoints (OPENDESK_DEV_ENDPOINTS=1) compiles the /dev/* triggers
	// into the router (N-01); default OFF — the routes answer 404.
	DevEndpoints bool
	// TrustDirectTenancy (OPENDESK_TRUST_DIRECT_TENANT=1) is the explicit
	// gateway-less dev escape for the K1 role/tenant bindings (auth.go);
	// every use is logged. Never set in deployed environments.
	TrustDirectTenancy bool
	// DNDAdminRoles (DND_ADMIN_ROLES csv, default "platform-admin") gates
	// the DND import / global delete and the ops-alerts read (S1-F7-03, K3).
	DNDAdminRoles []string
	// SignalWorkflowPrefixes (SIGNAL_WORKFLOW_PREFIXES csv) is the /v1/signals
	// workflow-id prefix allowlist (S1-F7-04); "{tenant}" expands to the
	// bound tenant slug. Empty → defaultSignalPrefixes.
	SignalWorkflowPrefixes []string
	// WebhookURLValidator validates outbound webhook subscription URLs
	// (S1-F6-03/N-02 SSRF guard); nil → strict default (https-only).
	WebhookURLValidator *URLValidator

	// Outbound webhook platform (Wave 5 #10); nil Webhooks disables the
	// /v1/webhooks routes (503).
	Webhooks               WebhookStore
	ResolveTenant          func(ctx context.Context, slug string) (TenantRef, error)
	WebhookSigningRequired bool

	// DND is the SPEC-W12 DND registry (NCC 2442 + tenant opt-outs); nil
	// disables the /v1/dnd routes (503).
	DND DNDStore

	// OpsAlerts is the K3 ops-alert store; nil disables GET /v1/ops-alerts
	// (503).
	OpsAlerts OpsAlertStore

	// AudienceIntake is the SPEC-W28 graph audience intake; nil degrades
	// POST /v1/audiences to 503.
	AudienceIntake AudienceIntaker

	// HealthPostgres pings the notifications DB for /healthz (F15-05/N-07);
	// nil skips the check (DATABASE_URL unset).
	HealthPostgres func(ctx context.Context) error
	// HealthTemporal checks Temporal frontend health for /healthz; nil
	// skips the check.
	HealthTemporal func(ctx context.Context) error
}

// NewRouter builds the chi router.
func NewRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", s.healthz)
	// Prometheus scrape endpoint (F15-02; client_golang promhttp).
	r.Handle("/metrics", promhttp.Handler())

	// /dev/* triggers (manual testing) are compiled OUT unless
	// OPENDESK_DEV_ENDPOINTS=1 (N-01): without the flag the routes do not
	// exist and answer 404. APISIX additionally denies /api/notifications/dev/*
	// (C7). Even when compiled in, the group sits behind the K2
	// internal-token middleware (V2-D2: identity-service reaches
	// trigger-twin-cleanup with X-Internal-Token = NOTIFICATION_INTERNAL_TOKEN;
	// compose defaults the flag OFF, and the token gate is the real control).
	if s.DevEndpoints {
		r.Group(func(r chi.Router) {
			r.Use(s.requireInternalToken)
			// POST /dev/trigger-reminder starts a ReminderWorkflow with
			// overridden (short) delays for manual testing.
			r.Post("/dev/trigger-reminder", s.triggerReminder)
			// POST /dev/trigger-onboarding starts a TenantOnboardingWorkflow.
			r.Post("/dev/trigger-onboarding", s.triggerOnboarding)
			// POST /dev/trigger-twin-cleanup starts a TwinCleanupWorkflow (24h
			// → delete the twin tenant). Invoked by identity-service's twin
			// endpoint via Dapr service invocation (SPEC-W3 §3 innovation 12).
			r.Post("/dev/trigger-twin-cleanup", s.triggerTwinCleanup)
		})
	}
	// POST /v1/signals delivers a signal to a running workflow (staff UI:
	// IntakeCompleted / Responded on the pack workflows, SPEC-CRM §C2).
	// Machine-only route (S1-F7-04): K2 internal token + required tenant_slug
	// bound to X-Tenant-Slugs + workflow-id prefix allowlist.
	r.With(s.requireInternalToken).Post("/v1/signals", s.sendSignal)

	// Outbound webhook platform (Wave 5 #10): tenant-scoped via
	// X-Tenant-Slug; reached from the UI through APISIX /api/notifications/*.
	r.Route("/v1/webhooks", func(r chi.Router) {
		r.Use(s.tenantMiddleware)
		r.Post("/", s.createWebhook)
		r.Get("/", s.listWebhooks)
		r.Delete("/{id}", s.deleteWebhook)
		r.Get("/{id}/deliveries", s.listWebhookDeliveries)
	})

	// DND registry (SPEC-W12 Agent B + S1-F7-03): the NCC 2442 global import
	// and the global delete are admin-mutation routes gated on
	// X-User-Roles ∩ DND_ADMIN_ROLES (K1 header contract — APISIX injects the
	// roles from the verified JWT); the tenant-scoped delete binds its slug
	// to X-Tenant-Slugs membership. /v1/dnd/check stays an ungated read (the
	// send-guard lookup path).
	r.Route("/v1/dnd", func(r chi.Router) {
		r.With(s.requireDNDAdmin).Post("/import", s.importDND)
		r.Delete("/{phone}", s.deleteDND)
		r.Get("/check", s.checkDND)
	})

	// K3: ops-alerts read-back (the opendesk.ops.alerts consumer persists;
	// this endpoint is DND-admin-role gated, limit ≤ 500).
	r.With(s.requireDNDAdmin).Get("/v1/ops-alerts", s.listOpsAlerts)

	// SPEC-W28: consent-gated graph audiences → existing pacer path.
	RegisterAudienceRoutes(r, NewAudienceHandler(s.AudienceIntake, s.Log))
	return r
}

// healthz is dependency-aware (F15-05/N-07): every configured dependency
// (Postgres ping, Temporal CheckHealth) is probed with a 2s budget; any
// failure answers 503 with per-check detail instead of a blind ok.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]func(context.Context) error{}
	if s.HealthPostgres != nil {
		checks["postgres"] = s.HealthPostgres
	}
	if s.HealthTemporal != nil {
		checks["temporal"] = s.HealthTemporal
	}
	type result struct {
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	out := map[string]result{}
	healthy := true
	for name, check := range checks {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		err := check(ctx)
		cancel()
		if err != nil {
			healthy = false
			out[name] = result{Status: "error", Error: err.Error()}
			s.Log.Warn("healthz dependency check failed", zap.String("check", name), zap.Error(err))
		} else {
			out[name] = result{Status: "ok"}
		}
	}
	status := http.StatusOK
	top := "ok"
	if !healthy {
		status = http.StatusServiceUnavailable
		top = "degraded"
	}
	writeJSON(w, status, map[string]any{"status": top, "checks": out})
}

// defaultSignalPrefixes is the /v1/signals workflow-id prefix allowlist
// (S1-F7-04): only the staff-facing pack/reminder/noshow workflow families
// are signalable — never twin-cleanup / onboarding / delivery machinery.
// "{tenant}" expands to the bound tenant slug for slug-prefixed ids.
var defaultSignalPrefixes = []string{"reminder-", "noshow-", "pack-", "{tenant}-"}

// signalAllowed reports whether workflowID matches the prefix allowlist.
func (s *Server) signalAllowed(tenantSlug, workflowID string) bool {
	prefixes := s.SignalWorkflowPrefixes
	if len(prefixes) == 0 {
		prefixes = defaultSignalPrefixes
	}
	for _, p := range prefixes {
		p = strings.ReplaceAll(p, "{tenant}", tenantSlug)
		if p != "" && strings.HasPrefix(workflowID, p) {
			return true
		}
	}
	return false
}

// signalRequest is the body of POST /v1/signals. tenant_slug is required
// (S1-F7-04) and bound to the X-Tenant-Slugs membership list. Payload is
// optional (the IntakeCompleted / Responded / NoShow signals carry no
// payload); when given
// it must be a JSON value, e.g. {"type":"cancelled"} for "booking-event".
type signalRequest struct {
	WorkflowID string          `json:"workflow_id"`
	TenantSlug string          `json:"tenant_slug"`
	Signal     string          `json:"signal"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// sendSignal forwards the signal via the Temporal client (behind the K2
// internal-token gate mounted on the route, the required tenant_slug binding
// and the workflow-id prefix allowlist — S1-F7-04). A workflow that is
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
	req.TenantSlug = strings.TrimSpace(req.TenantSlug)
	if req.TenantSlug == "" {
		http.Error(w, `{"error":"tenant_slug is required"}`, http.StatusBadRequest)
		return
	}
	if !s.bindTenantSlug(r, req.TenantSlug) {
		http.Error(w, `{"error":"tenant_slug is not bound to the caller"}`, http.StatusForbidden)
		return
	}
	if !s.signalAllowed(req.TenantSlug, req.WorkflowID) {
		http.Error(w, `{"error":"workflow_id is not in the signal allowlist"}`, http.StatusForbidden)
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
