package campaignstudio

// Campaign Studio HTTP API (SPEC-W19 Agent D). Routes are registered by
// RegisterRoutes (routes.go); the tenant context is injected by the
// integrator via Deps.TenantFromContext, so this package stays free of
// httpapi internals (same pattern as devices.Handlers).
//
//   - GET    /v1/studio/segments                     list           (view_analytics)
//   - POST   /v1/studio/segments                     create         (manage_bookings)
//   - PATCH  /v1/studio/segments/{id}                update         (manage_bookings)
//   - POST   /v1/studio/segments/{id}/count          evaluate       (view_analytics)
//   - GET    /v1/studio/journeys?status=             list           (view_analytics)
//   - POST   /v1/studio/journeys                     create (draft) (manage_bookings)
//   - GET    /v1/studio/journeys/{id}                detail         (view_analytics)
//   - PATCH  /v1/studio/journeys/{id}                edit/status    (manage_bookings)
//   - POST   /v1/studio/journeys/{id}/enroll         enroll         (manage_bookings)
//   - POST   /v1/studio/journeys/{id}/step           advance due    (manage_bookings)
//   - GET    /v1/studio/journeys/{id}/stats          stats          (view_analytics)

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/geo"
	"go.uber.org/zap"
)

// maxEnrollBatch caps one enroll call.
const maxEnrollBatch = 5000

// DefaultStepBatch bounds enrollments advanced per step call when neither
// the request nor Deps.StepBatchSize overrides it.
const DefaultStepBatch = 200

// timeNow is the clock (var for tests driving wait-step due logic through
// the HTTP surface).
var timeNow = time.Now

// Handlers bundles the API dependencies (populated by RegisterRoutes).
type Handlers struct {
	Store       *Store
	Log         *zap.Logger
	Starter     SendStarter // nil → send steps defer (SendsDeferred) instead of erroring
	UsageTopic  string      // opendesk.usage.events; empty disables metering
	EventsTopic string      // opendesk.studio.events.v1; empty disables lifecycle events
	StepBatch   int         // per-step advancement cap (<=0 → DefaultStepBatch)
}

func (h *Handlers) log() *zap.Logger {
	if h.Log != nil {
		return h.Log
	}
	return zap.NewNop()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handlers) mapErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.log().Error("campaign studio handler error", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func urlUUID(r *http.Request, param string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		return uuid.Nil, errors.New("invalid " + param)
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// Segments
// ---------------------------------------------------------------------------

// ListSegments (GET /v1/studio/segments).
func (h *Handlers) ListSegments(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	segs, err := h.Store.ListSegments(r.Context(), tenant.ID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"segments": segs})
}

type segmentRequest struct {
	Name       string             `json:"name"`
	Definition *SegmentDefinition `json:"definition"`
}

// CreateSegment (POST /v1/studio/segments).
func (h *Handlers) CreateSegment(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	var req segmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := ValidateSegmentDefinition(req.Definition); err != nil {
		h.mapErr(w, err)
		return
	}
	seg := Segment{TenantID: tenant.ID, Name: req.Name, Definition: *req.Definition}
	if err := h.Store.CreateSegment(r.Context(), &seg); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"segment": seg})
}

// PatchSegment (PATCH /v1/studio/segments/{id}): name and/or definition.
func (h *Handlers) PatchSegment(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	id, err := urlUUID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req segmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var name *string
	if trimmed := strings.TrimSpace(req.Name); trimmed != "" {
		name = &trimmed
	}
	if req.Definition != nil {
		if err := ValidateSegmentDefinition(req.Definition); err != nil {
			h.mapErr(w, err)
			return
		}
	}
	if name == nil && req.Definition == nil {
		writeError(w, http.StatusBadRequest, "nothing to update (name or definition required)")
		return
	}
	seg, err := h.Store.UpdateSegment(r.Context(), tenant.ID, id, name, req.Definition)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"segment": seg})
}

// CountSegment (POST /v1/studio/segments/{id}/count) evaluates the
// definition against contacts/leads (read-only evaluation; approx_count
// is stamped as a cache). 100k-row scan ceiling — truncated=true when hit.
func (h *Handlers) CountSegment(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	id, err := urlUUID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	count, truncated, err := h.Store.CountSegment(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"segment_id": id,
		"count":      count,
		"truncated":  truncated,
		"ceiling":    segmentCountRowCeiling,
	})
}

// ---------------------------------------------------------------------------
// Journeys
// ---------------------------------------------------------------------------

// ListJourneys (GET /v1/studio/journeys?status=).
func (h *Handlers) ListJourneys(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	status := r.URL.Query().Get("status")
	if status != "" {
		switch status {
		case StatusDraft, StatusActive, StatusPaused, StatusArchived:
		default:
			writeError(w, http.StatusBadRequest, "invalid status filter")
			return
		}
	}
	journeys, err := h.Store.ListJourneys(r.Context(), tenant.ID, status)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"journeys": journeys})
}

type journeyRequest struct {
	Name        string     `json:"name"`
	TriggerKind string     `json:"trigger_kind"`
	SegmentID   *uuid.UUID `json:"segment_id"`
	Steps       Steps      `json:"steps"`
}

// CreateJourney (POST /v1/studio/journeys) — starts in draft.
func (h *Handlers) CreateJourney(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	var req journeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.TriggerKind == "" {
		req.TriggerKind = TriggerManual
	}
	if !validTriggerKind(req.TriggerKind) {
		writeError(w, http.StatusBadRequest, "trigger_kind must be segment|manual|event")
		return
	}
	if req.TriggerKind == TriggerSegment && req.SegmentID == nil {
		writeError(w, http.StatusBadRequest, "segment_id is required for trigger_kind segment")
		return
	}
	if err := ValidateSteps(req.Steps); err != nil {
		h.mapErr(w, err)
		return
	}
	j := Journey{
		TenantID:    tenant.ID,
		Name:        req.Name,
		TriggerKind: req.TriggerKind,
		SegmentID:   req.SegmentID,
		Steps:       req.Steps,
	}
	if err := h.Store.CreateJourney(r.Context(), &j); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"journey": j})
}

// GetJourney (GET /v1/studio/journeys/{id}).
func (h *Handlers) GetJourney(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	id, err := urlUUID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	j, err := h.Store.GetJourney(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"journey": j})
}

type patchJourneyRequest struct {
	Name        *string    `json:"name"`
	Status      *string    `json:"status"`
	TriggerKind *string    `json:"trigger_kind"`
	SegmentID   *uuid.UUID `json:"segment_id"`
	Steps       *Steps     `json:"steps"`
}

// PatchJourney (PATCH /v1/studio/journeys/{id}): structural edits (name /
// trigger_kind / segment_id / steps) are accepted only while the journey
// is draft or paused (409 otherwise); status moves go through the status
// machine (draft→active→paused↔active→archived).
func (h *Handlers) PatchJourney(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	id, err := urlUUID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req patchJourneyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	structural := req.Name != nil || req.TriggerKind != nil || req.SegmentID != nil || req.Steps != nil
	var j Journey
	if structural {
		cur, err := h.Store.GetJourney(r.Context(), tenant.ID, id)
		if err != nil {
			h.mapErr(w, err)
			return
		}
		if cur.Status != StatusDraft && cur.Status != StatusPaused {
			writeError(w, http.StatusConflict, "journey fields are editable only while draft or paused")
			return
		}
		if req.TriggerKind != nil && !validTriggerKind(*req.TriggerKind) {
			writeError(w, http.StatusBadRequest, "trigger_kind must be segment|manual|event")
			return
		}
		if req.Steps != nil {
			if err := ValidateSteps(*req.Steps); err != nil {
				h.mapErr(w, err)
				return
			}
		}
		var segmentID **uuid.UUID
		if req.SegmentID != nil {
			segmentID = &req.SegmentID
		}
		j, err = h.Store.UpdateJourney(r.Context(), tenant.ID, id, req.Name, req.TriggerKind, segmentID, req.Steps)
		if err != nil {
			h.mapErr(w, err)
			return
		}
	}
	if req.Status != nil {
		switch *req.Status {
		case StatusDraft, StatusActive, StatusPaused, StatusArchived:
		default:
			writeError(w, http.StatusBadRequest, "status must be draft|active|paused|archived")
			return
		}
		j, err = h.Store.TransitionJourney(r.Context(), tenant.ID, id, *req.Status)
		if err != nil {
			h.mapErr(w, err)
			return
		}
	}
	if !structural && req.Status == nil {
		writeError(w, http.StatusBadRequest, "nothing to update")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"journey": j})
}

// ---------------------------------------------------------------------------
// Enrollments
// ---------------------------------------------------------------------------

type enrollRequest struct {
	ContactIDs []uuid.UUID `json:"contact_ids"`
}

// Enroll (POST /v1/studio/journeys/{id}/enroll) — idempotent per
// journey+contact; the journey must be active (409 otherwise). New
// enrollments are metered (journey_enrolled) and emitted
// (com.opendesk.studio.JourneyEnrolled) post-commit, best-effort.
func (h *Handlers) Enroll(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	id, err := urlUUID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req enrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.ContactIDs) == 0 {
		writeError(w, http.StatusBadRequest, "contact_ids must be a non-empty array")
		return
	}
	if len(req.ContactIDs) > maxEnrollBatch {
		writeError(w, http.StatusBadRequest, "at most 5000 contact_ids per call")
		return
	}
	j, err := h.Store.GetJourney(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if j.Status != StatusActive {
		writeError(w, http.StatusConflict, "enrollments require an active journey (status "+j.Status+")")
		return
	}
	created, existing, err := h.Store.Enroll(r.Context(), tenant.ID, id, req.ContactIDs)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	// Post-commit, best-effort: meter + lifecycle events for the NEW rows
	// only (idempotent replays can never double-meter).
	h.meterJourneyEnrolled(r.Context(), tenant.Slug, tenant.ID, created)
	h.publishJourneyEvents(r.Context(), EventTypeJourneyEnrolled, tenant.Slug, tenant.ID, created)
	writeJSON(w, http.StatusOK, map[string]any{
		"journey_id": id,
		"enrolled":   len(created),
		"existing":   existing,
	})
}

// ---------------------------------------------------------------------------
// Step advancement (operator/CRON-triggered)
// ---------------------------------------------------------------------------

type stepRequest struct {
	Limit int `json:"limit"` // <=0 → Deps.StepBatch / DefaultStepBatch
}

// Step (POST /v1/studio/journeys/{id}/step) advances due enrollments one
// step (see Store.AdvanceDue). Collected sends dispatch through one
// StudioSendWorkflow started post-commit; when no Temporal starter is
// configured the due sends stay in place (sends_deferred=true) for the
// next dispatched call.
func (h *Handlers) Step(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	id, err := urlUUID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req stepRequest
	if r.Body != nil && r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	j, err := h.Store.GetJourney(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if j.Status != StatusActive {
		writeError(w, http.StatusConflict, "step requires an active journey (status "+j.Status+")")
		return
	}
	limit := req.Limit
	if limit <= 0 {
		limit = h.StepBatch
	}
	if limit <= 0 {
		limit = DefaultStepBatch
	}
	dispatch := h.Starter != nil
	res, err := h.Store.AdvanceDue(r.Context(), tenant.ID, j, timeNow(), limit, dispatch)
	if err != nil {
		h.mapErr(w, err)
		return
	}

	// Post-commit, best-effort: journey_completed lifecycle events.
	h.publishJourneyEvents(r.Context(), EventTypeJourneyCompleted, tenant.Slug, tenant.ID, res.CompletedEnrollments)

	resp := map[string]any{
		"journey_id":     id,
		"scanned":        res.Scanned,
		"advanced":       res.Advanced,
		"completed":      res.Completed,
		"exited":         res.Exited,
		"skipped":        res.Skipped,
		"wait_not_due":   res.WaitNotDue,
		"sends_queued":   len(res.Sends),
		"sends_deferred": res.SendsDeferred,
		"dispatch":       "none",
	}
	if len(res.Sends) > 0 {
		batch := StudioSendBatchInput{
			BatchID:     uuid.NewString(),
			TenantID:    tenant.ID.String(),
			TenantSlug:  tenant.Slug,
			JourneyID:   j.ID.String(),
			JourneyName: j.Name,
			Sends:       res.Sends,
		}
		// Quiet-hours config captured at step time (SPEC-W12 §8) so the
		// workflow's deferral replay stays deterministic if the env
		// changes mid-batch (same idiom as the geo campaign handler).
		overrides, oerr := geo.ParseQuietHoursOverrides(os.Getenv("QUIET_HOURS_OVERRIDES"))
		if oerr != nil {
			h.log().Warn("ignoring malformed QUIET_HOURS_OVERRIDES; default quiet-hours window applies",
				zap.Error(oerr))
			overrides = nil
		}
		batch.QuietHoursWindow = os.Getenv("QUIET_HOURS_DEFAULT")
		batch.QuietHoursOverrides = overrides
		workflowID, serr := h.Starter.StartStudioSendBatch(r.Context(), batch)
		if serr != nil {
			// The enrollments already advanced; the failure is surfaced
			// honestly (503-shaped flag in the 200 body would be worse:
			// make it a real error so CRON alerts).
			h.log().Error("studio send batch dispatch failed",
				zap.String("journey_id", j.ID.String()), zap.Error(serr))
			writeError(w, http.StatusBadGateway, "enrollments advanced but send dispatch failed: "+serr.Error())
			return
		}
		resp["dispatch"] = "started"
		resp["workflow_id"] = workflowID
	}
	writeJSON(w, http.StatusOK, resp)
}

// Stats (GET /v1/studio/journeys/{id}/stats): enrolled/active/completed/
// exited totals + per-step counts.
func (h *Handlers) Stats(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	id, err := urlUUID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	j, err := h.Store.GetJourney(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	stats, err := h.Store.Stats(r.Context(), tenant.ID, j)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"journey_id": id,
		"stats":      stats,
	})
}
