package httpapi

// SPEC-W28 WS-C: graph audience intake REST.
//
// POST /v1/audiences takes a saved segment + campaign id, pulls the
// materialized consent-passing audience from graph-service and enqueues one
// PacedSendWorkflow per recipient (the EXISTING pacer path: DND 2442
// suppression, quiet-hours deferral, CPS pacing, sender rotation — all
// unchanged). Tenant scope comes from the JWT seam: the APISIX jwt plugin
// maps the workforce JWT sub to X-Tenant-Id (X-Tenant-Slug accepted as the
// slug twin, exactly like /v1/webhooks); the tenant is never read from the
// body, and the same header is forwarded to graph-service, which injects the
// tenant filter on all graph paths (SPEC-W28 §5 gate 1).
//
// Idempotency (SPEC-W24): callers SHOULD send Idempotency-Key; the dedupe
// key is campaign_id, so a replayed POST answers 200 with duplicate=true and
// zero side effects. A failed intake releases the claim, so retries are safe.
//
// Integrator wiring (3 lines — nothing else needs to change):
//
//	// cmd/worker/main.go, next to the other httpapi.Server deps:
//	audienceIntake := activities.NewAudienceIntake(daprClient, cfg.BookingAppID, tc, cfg.TemporalTaskQueue, strings.Split(cfg.KafkaBrokers, ","), logger)
//	// internal/httpapi/server.go NewRouter, just before `return r`:
//	RegisterAudienceRoutes(r, NewAudienceHandler(audienceIntake, s.Log))

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/opendesk/notification-worker/internal/activities"
	"go.uber.org/zap"
)

// AudienceIntaker is the intake slice used by the handler
// (*activities.AudienceIntake satisfies it; tests use a fake).
type AudienceIntaker interface {
	Intake(ctx context.Context, req activities.AudienceIntakeRequest) (activities.AudienceIntakeResult, error)
}

// AudienceHandler serves POST /v1/audiences.
type AudienceHandler struct {
	Intake AudienceIntaker
	Log    *zap.Logger
}

// NewAudienceHandler builds the handler. A nil intaker degrades the route to
// 503 (same posture as /v1/webhooks without DATABASE_URL).
func NewAudienceHandler(intake AudienceIntaker, log *zap.Logger) *AudienceHandler {
	if log == nil {
		log = zap.NewNop()
	}
	return &AudienceHandler{Intake: intake, Log: log}
}

// RegisterAudienceRoutes mounts POST /v1/audiences on the router.
func RegisterAudienceRoutes(r chi.Router, h *AudienceHandler) {
	r.Post("/v1/audiences", h.createAudience)
}

// createAudienceRequest is the body of POST /v1/audiences. Tenant fields are
// absent BY DESIGN — they come from the JWT seam headers.
type createAudienceRequest struct {
	SegmentID  string `json:"segment_id"`
	CampaignID string `json:"campaign_id"`
	Message    string `json:"message"`
	Channel    string `json:"channel,omitempty"` // sms (default) | whatsapp | telegram
}

// createAudience serves POST /v1/audiences.
func (h *AudienceHandler) createAudience(w http.ResponseWriter, r *http.Request) {
	if h.Intake == nil {
		http.Error(w, `{"error":"audience intake not configured"}`, http.StatusServiceUnavailable)
		return
	}
	// JWT seam: APISIX maps the workforce JWT sub → X-Tenant-Id; the slug
	// twin rides X-Tenant-Slug (same contract as /v1/webhooks).
	tenantID := strings.TrimSpace(r.Header.Get("X-Tenant-Id"))
	tenantSlug := strings.TrimSpace(r.Header.Get("X-Tenant-Slug"))
	if tenantID == "" {
		// Deployments fronted only by the slug header (current APISIX
		// jwt+rewrite pattern) use the slug as the scope token; graph-service
		// still enforces the tenant filter server-side.
		tenantID = tenantSlug
	}
	if tenantID == "" {
		http.Error(w, `{"error":"X-Tenant-Id header is required"}`, http.StatusBadRequest)
		return
	}
	var body createAudienceRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	res, err := h.Intake.Intake(r.Context(), activities.AudienceIntakeRequest{
		TenantID:       tenantID,
		TenantSlug:     tenantSlug,
		SegmentID:      strings.TrimSpace(body.SegmentID),
		CampaignID:     strings.TrimSpace(body.CampaignID),
		Message:        body.Message,
		Channel:        body.Channel,
		IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")),
	})
	if err != nil {
		var down *activities.AudienceGraphDownError
		if errors.As(err, &down) {
			h.Log.Warn("audience intake degraded: graph-service unavailable",
				zap.String("tenant_id", tenantID), zap.Error(err))
			http.Error(w, `{"error":"graph-service unavailable; nothing enqueued — retry is safe"}`, http.StatusBadGateway)
			return
		}
		h.Log.Warn("audience intake rejected/failed", zap.String("tenant_id", tenantID), zap.Error(err))
		http.Error(w, `{"error":`+strconv.Quote(err.Error())+`}`, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
