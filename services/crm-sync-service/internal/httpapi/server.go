// Package httpapi exposes crm-sync-service's HTTP surface (SPEC-CRM §B):
//   - GET  /healthz          liveness + DB ping
//   - GET  /metrics          Prometheus text format
//   - POST /webhooks/twenty  reverse intake, HMAC-verified, CloudEvent ->
//     opendesk.crm.events via Dapr pubsub `pubsub-kafka`
//   - POST /v1/tasks         helper for Temporal activities: create a Twenty
//     Task; accepts {personId|phone|email, title, body, dueAt} and the
//     industry-activity shape {tenant_slug, tenant_id, kind, title, body,
//     booking_id, due_at}
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/crm-sync-service/internal/daprc"
	"github.com/opendesk/crm-sync-service/internal/events"
	"github.com/opendesk/crm-sync-service/internal/metrics"
	"github.com/opendesk/crm-sync-service/internal/syncmap"
	"github.com/opendesk/crm-sync-service/internal/twentyc"
	"go.uber.org/zap"
)

// Pinger abstracts the DB liveness check.
type Pinger interface {
	Ping(ctx context.Context) error
}

// MapReader is the read side of the sync_map store (booking -> person lookup).
type MapReader interface {
	Get(ctx context.Context, kind, opendeskID string, tenantID *uuid.UUID) (syncmap.Mapping, error)
}

// WebhookDedupe records seen webhook event ids (SPEC-W43 K-13); it reports
// true for a NEW event and false for a replay inside the dedupe window.
type WebhookDedupe interface {
	MarkWebhookSeen(ctx context.Context, eventID string) (bool, error)
}

// Server bundles the HTTP dependencies.
type Server struct {
	Twenty         *twentyc.Client
	Dapr           *daprc.Client
	PubSubName     string
	CRMEventsTopic string
	WebhookSecret  string
	DB             Pinger
	Map            MapReader
	// Dedupe is the webhook replay guard (webhook_events_seen, 24h window).
	// Nil → dedupe disabled (tests / no-DB deployments).
	Dedupe  WebhookDedupe
	Metrics *metrics.Registry
	Log     *zap.Logger
}

// Router builds the chi router.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", s.healthz)
	r.Get("/metrics", s.metricsHandler)
	r.Post("/webhooks/twenty", s.twentyWebhook)
	r.Post("/v1/tasks", s.createTask)
	r.Get("/v1/people/lookup", s.lookupPerson)
	return r
}

// lookupPerson handles GET /v1/people/lookup?email=|phone= (GDPR export
// collector + tooling, SPEC-W3 §2): finds a Twenty person by e-mail first,
// then phone. Always 200 — {"person": null} when there is no match.
func (s *Server) lookupPerson(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	email, phone := q.Get("email"), q.Get("phone")
	if email == "" && phone == "" {
		writeError(w, http.StatusBadRequest, "email or phone query param is required")
		return
	}
	rec, err := s.Twenty.FindPerson(r.Context(), email, phone)
	if errors.Is(err, twentyc.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"person": nil})
		return
	}
	if err != nil {
		s.Log.Error("twenty person lookup failed", zap.Error(err))
		writeError(w, http.StatusBadGateway, "twenty lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"person": map[string]string{"id": rec.ID}})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if s.DB != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.DB.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, s.Metrics.Render())
}

// ---------------- reverse webhook intake ----------------

// twentyWebhook verifies the HMAC signature and re-emits the payload as a
// CloudEvent on opendesk.crm.events (SPEC-CRM §B).
func (s *Server) twentyWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}
	sig := r.Header.Get("X-Twenty-Webhook-Signature")
	if !VerifySignature(s.WebhookSecret, body, sig) {
		s.Metrics.Inc("webhook_rejected")
		writeError(w, http.StatusUnauthorized, "invalid webhook signature")
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// SPEC-W43 K-13: replay freshness — dedupe on the provider event id
	// (payload "id", falling back to a body digest so unsigned-id payloads
	// still dedupe exact replays) BEFORE re-emitting. A duplicate is a 200
	// "ignored" (acknowledged, not retried, not re-published). The dedupe
	// check runs only after HMAC verification, so an unauthenticated caller
	// cannot poison the seen-table. A dedupe-store failure fails LOUD (502)
	// so Twenty retries instead of the event being silently droppable.
	if s.Dedupe != nil {
		eventID, _ := payload["id"].(string)
		if eventID == "" {
			sum := sha256.Sum256(body)
			eventID = "sha256:" + hex.EncodeToString(sum[:])
		}
		fresh, err := s.Dedupe.MarkWebhookSeen(r.Context(), eventID)
		if err != nil {
			s.Log.Error("webhook dedupe store failed", zap.Error(err))
			writeError(w, http.StatusBadGateway, "webhook dedupe unavailable")
			return
		}
		if !fresh {
			s.Metrics.Inc("webhook_duplicate")
			s.Log.Info("duplicate twenty webhook ignored", zap.String("event_id", eventID))
			writeJSON(w, http.StatusOK, map[string]any{"status": "ignored", "duplicate": true})
			return
		}
	}
	// Twenty webhook payloads carry an "event" field like "person.created";
	// default to a generic type when absent.
	eventName, _ := payload["event"].(string)
	ceType := "com.opendesk.crm.TwentyWebhook"
	if eventName != "" {
		ceType = "com.opendesk.crm.twenty." + eventName
	}
	ce := events.New(uuid.NewString(), "crm-sync-service", ceType, "twenty", "", payload)
	if err := s.Dapr.PublishEvent(r.Context(), s.PubSubName, s.CRMEventsTopic, ce); err != nil {
		s.Log.Error("failed to publish CRM event", zap.Error(err))
		writeError(w, http.StatusBadGateway, "failed to publish event")
		return
	}
	s.Metrics.Inc("webhook_accepted")
	s.Log.Info("twenty webhook accepted", zap.String("type", ceType))
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// ---------------- /v1/tasks helper (Temporal activities) ----------------

// createTaskRequest accepts both payload shapes:
//   - canonical: {personId|phone|email, title, body, dueAt}
//   - notification-worker industry activities (internal/activities/industry.go):
//     {tenant_slug, tenant_id, kind ("staff_alert"|"follow_up"), title, body,
//     booking_id, due_at}
type createTaskRequest struct {
	PersonID string `json:"personId"`
	Phone    string `json:"phone"`
	Email    string `json:"email"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	DueAt    string `json:"dueAt"`
	// snake_case shape (Temporal pack activities)
	TenantSlug string `json:"tenant_slug"`
	TenantID   string `json:"tenant_id"`
	Kind       string `json:"kind"`
	BookingID  string `json:"booking_id"`
	DueAtSnake string `json:"due_at"`
}

// createTask resolves the person (personId -> booking_id via sync_map ->
// phone/email lookup -> none) and creates a Twenty Task. Tasks of kind
// "staff_alert" may legitimately have no linked person; "follow_up" tries to
// link but degrades to unlinked with a warning — never a 4xx for missing
// linkage (title is the only hard requirement).
func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	dueAt, err := resolveDueAt(req.DueAt, req.DueAtSnake)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	personID := s.resolvePerson(r, req)

	task := twentyc.TaskCreate{Title: req.Title, Body: req.Body, DueAt: dueAt, Status: "TODO"}
	taskID, err := s.Twenty.CreateTask(r.Context(), task, personID)
	if err != nil {
		if taskID != "" {
			// Task exists but the person link (taskTarget) failed: accept the
			// unlinked task rather than failing the caller (SPEC-CRM §C2
			// follow_up degradation rule).
			s.Log.Warn("task created without person link",
				zap.String("task_id", taskID), zap.String("kind", req.Kind), zap.Error(err))
			s.Metrics.Inc("tasks_created_api_unlinked")
			writeJSON(w, http.StatusCreated, map[string]any{"taskId": taskID, "personId": personID, "linked": false})
			return
		}
		s.Log.Error("task create failed", zap.Error(err))
		writeError(w, http.StatusBadGateway, "twenty task create failed")
		return
	}
	if personID == "" {
		if req.Kind == "follow_up" {
			s.Log.Warn("follow_up task created unlinked: no person resolved",
				zap.String("booking_id", req.BookingID), zap.String("tenant_slug", req.TenantSlug))
		}
		s.Metrics.Inc("tasks_created_api_unlinked")
	} else {
		s.Metrics.Inc("tasks_created_api")
	}
	writeJSON(w, http.StatusCreated, map[string]any{"taskId": taskID, "personId": personID, "linked": personID != ""})
}

// resolveDueAt merges the camelCase and snake_case due-date fields: dueAt
// wins when both are present; either must be RFC3339.
func resolveDueAt(dueAt, dueAtSnake string) (string, error) {
	v := dueAt
	if v == "" {
		v = dueAtSnake
	}
	if v == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return "", fmt.Errorf("due date %q must be RFC3339", v)
	}
	return twentyc.FormatTime(t), nil
}

// resolvePerson implements the resolution order: personId -> booking_id via
// sync_map (kind=booking_contact) -> phone/email Twenty lookup -> "" (unlinked).
func (s *Server) resolvePerson(r *http.Request, req createTaskRequest) string {
	if req.PersonID != "" {
		return req.PersonID
	}
	if req.BookingID != "" && s.Map != nil {
		var tid *uuid.UUID
		if id, err := uuid.Parse(req.TenantID); err == nil {
			tid = &id
		}
		m, err := s.Map.Get(r.Context(), syncmap.KindBookingContact, req.BookingID, tid)
		if err == nil && m.TwentyID != "" {
			return m.TwentyID
		}
		if err != nil && !errors.Is(err, syncmap.ErrNotFound) {
			s.Log.Warn("sync_map booking_contact lookup failed; continuing",
				zap.String("booking_id", req.BookingID), zap.Error(err))
		}
	}
	if req.Phone != "" || req.Email != "" {
		rec, err := s.Twenty.FindPerson(r.Context(), req.Email, req.Phone)
		if err == nil {
			return rec.ID
		}
		if !errors.Is(err, twentyc.ErrNotFound) {
			s.Log.Warn("twenty person lookup failed; creating task unlinked", zap.Error(err))
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
