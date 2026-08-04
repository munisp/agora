package surveys

// Surveys/VoC HTTP API (SPEC-W20 Agent B). Routes (mounted by
// RegisterRoutes on the ROOT chi router):
//
//	POST  /v1/surveys/respond                    PUBLIC — no tenant header,
//	                                             no JWT; the invite token
//	                                             resolves the tenant (see
//	                                             the SECURITY note in store.go)
//	GET   /v1/surveys/surveys?status=&kind=
//	POST  /v1/surveys/surveys
//	GET   /v1/surveys/surveys/{id}               (+ invite/response stats)
//	PATCH /v1/surveys/surveys/{id}
//	POST  /v1/surveys/surveys/{id}/send          {contact_ids[]}
//	GET   /v1/surveys/surveys/{id}/results
//	GET   /v1/surveys/voc/themes?survey_id=
//
// AuthZ (SPEC-W20 contract §3): reads are view_analytics, writes are
// manage_bookings. RegisterRoutes applies the variadic mw GROUP-WIDE to
// the tenant-scoped group; the INTEGRATOR wires the permission middleware
// — recommended shape (composed from httpapi's existing require()):
//
//	method-aware: GET/HEAD → require("view_analytics"), else require("manage_bookings")
//
// plus the appgate entitlement gate for app_id "surveys-voc".
//
// !!! INTEGRATOR: /v1/surveys/respond is registered OUTSIDE the gated
// group ON PURPOSE — it must stay reachable without tenant slug, JWT or
// appgate entitlement (it is the customer's public submit path). Do NOT
// wrap it with the group middleware. Rate-limiting it is an OPS concern
// (enforce at the APISIX edge — see docs/apps/surveys-voc.md).
//
// Tenant context: this package resolves the tenant itself from the
// X-Tenant-Slug header via Deps.Resolver (httpapi's tenantMiddleware ctx
// key is unexported) and stores it under its own key; handlers read it via
// TenantFromContext — the contract §3 helper idiom (mirrors workorders).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"go.uber.org/zap"
)

// TenantResolver resolves a tenant by slug (bookingops.TenantResolver
// satisfies it).
type TenantResolver interface {
	BySlug(ctx context.Context, slug string) (bookingops.TenantInfo, error)
}

// Deps are the integration seams the integrator wires (SPEC-W20
// anti-collision contract). All topics are OPTIONAL: empty disables the
// corresponding emission (graceful no-op).
type Deps struct {
	Store    *Store
	Resolver TenantResolver
	Logger   *zap.Logger
	// NotificationsTopic is the notifications command topic
	// (opendesk.notifications.outbox). Empty disables invite sends —
	// invites stay queued.
	NotificationsTopic string
	// UsageTopic is the metering topic (opendesk.usage.events). Empty
	// disables the survey_response_received meter.
	UsageTopic string
	// EventsTopic is the lifecycle-events topic
	// (opendesk.surveys.events.v1). Empty disables sent/answered events.
	EventsTopic string
	// PublicBaseURL is the public base embedded in invite links
	// (<base>?t=<token>). Empty → DefaultPublicBaseURL.
	PublicBaseURL string
}

// Handlers serves the surveys endpoints. Constructed by RegisterRoutes;
// exported fields allow direct wiring in tests (mirrors devices.Handlers).
type Handlers struct {
	Store              *Store
	Log                *zap.Logger
	NotificationsTopic string
	UsageTopic         string
	EventsTopic        string
	PublicBaseURL      string
}

func (h *Handlers) log() *zap.Logger {
	if h.Log != nil {
		return h.Log
	}
	return zap.NewNop()
}

// ctxKey is the package-private context key type (httpapi's key is
// unexported — see the file comment).
type ctxKey string

const ctxTenant ctxKey = "surveys-tenant"

// TenantFromContext returns the tenant resolved by the package tenant
// middleware (contract §3 helper). The zero value (ID == uuid.Nil) means
// no tenant context — handlers treat it as 400.
func TenantFromContext(ctx context.Context) bookingops.TenantInfo {
	t, _ := ctx.Value(ctxTenant).(bookingops.TenantInfo)
	return t
}

// RegisterRoutes mounts the surveys API at /v1/surveys on the given router
// (call it on the ROOT router — the /v1 prefix is included). The PUBLIC
// respond route is registered FIRST, outside the tenant-scoped group (see
// the file comment). mw are applied group-wide AFTER the package tenant
// middleware.
func RegisterRoutes(r chi.Router, d *Deps, mw ...func(http.Handler) http.Handler) {
	h := &Handlers{
		Store:              d.Store,
		Log:                d.Logger,
		NotificationsTopic: d.NotificationsTopic,
		UsageTopic:         d.UsageTopic,
		EventsTopic:        d.EventsTopic,
		PublicBaseURL:      d.PublicBaseURL,
	}
	// PUBLIC: token-resolved tenant, NO tenant middleware, NO group mw.
	r.Post("/v1/surveys/respond", h.Respond)
	r.Route("/v1/surveys", func(r chi.Router) {
		r.Use(tenantMiddleware(d.Resolver, d.Logger))
		r.Use(mw...)
		r.Get("/surveys", h.List)
		r.Post("/surveys", h.Create)
		r.Get("/surveys/{id}", h.Get)
		r.Patch("/surveys/{id}", h.Patch)
		r.Post("/surveys/{id}/send", h.Send)
		r.Get("/surveys/{id}/results", h.Results)
		r.Get("/voc/themes", h.Themes)
	})
}

// tenantMiddleware resolves X-Tenant-Slug via the resolver and stores the
// TenantInfo under the package key. 503 when no resolver is wired (partial
// deployment), 400 without a slug, 404 on resolution failure — mirroring
// httpapi.tenantMiddleware's status codes.
func tenantMiddleware(resolver TenantResolver, log *zap.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = zap.NewNop()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if resolver == nil {
				writeError(w, http.StatusServiceUnavailable, "surveys unavailable (tenant resolver not wired)")
				return
			}
			slug := strings.TrimSpace(r.Header.Get("X-Tenant-Slug"))
			if slug == "" {
				writeError(w, http.StatusBadRequest, "X-Tenant-Slug header is required")
				return
			}
			tenant, err := resolver.BySlug(r.Context(), slug)
			if err != nil || tenant.ID == uuid.Nil {
				log.Warn("tenant resolution failed", zap.String("slug", slug), zap.Error(err))
				writeError(w, http.StatusNotFound, "tenant not found")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxTenant, tenant)))
		})
	}
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
	case errors.Is(err, ErrAlreadyAnswered):
		writeError(w, http.StatusConflict, ErrAlreadyAnswered.Error())
	case errors.Is(err, ErrInviteExpired):
		writeError(w, http.StatusGone, "invite expired")
	case errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrSurveyNotActive):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.log().Error("surveys handler error", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// tenantOr400 extracts the tenant context or writes 400.
func (h *Handlers) tenantOr400(w http.ResponseWriter, r *http.Request) (bookingops.TenantInfo, bool) {
	t := TenantFromContext(r.Context())
	if t.ID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "tenant context required")
		return t, false
	}
	return t, true
}

// ---------------------------------------------------------------------------
// Survey CRUD
// ---------------------------------------------------------------------------

// createSurveyRequest is the POST /v1/surveys/surveys body. A survey
// always starts in "draft" — PATCH it to active. Defaults: kind nps,
// trigger_kind manual, channel sms.
type createSurveyRequest struct {
	Name        string     `json:"name"`
	Kind        string     `json:"kind,omitempty"`
	Questions   []Question `json:"questions"`
	TriggerKind string     `json:"trigger_kind,omitempty"`
	Channel     string     `json:"channel,omitempty"`
}

// Create (POST /v1/surveys/surveys) creates a draft survey. 201.
func (h *Handlers) Create(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	var req createSurveyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := ValidateQuestions(req.Questions); err != nil {
		h.mapErr(w, err)
		return
	}
	sv := Survey{
		TenantID:    tenant.ID,
		Name:        req.Name,
		Status:      StatusDraft,
		Kind:        orDefault(req.Kind, KindNPS),
		Questions:   req.Questions,
		TriggerKind: orDefault(req.TriggerKind, TriggerManual),
		Channel:     orDefault(req.Channel, ChannelSMS),
	}
	if err := sv.Validate(); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.CreateSurvey(r.Context(), &sv); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"survey": sv})
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return strings.TrimSpace(v)
}

// List (GET /v1/surveys/surveys?status=&kind=).
func (h *Handlers) List(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	status := strings.TrimSpace(q.Get("status"))
	if status != "" && !validSurveyStatus(status) {
		writeError(w, http.StatusBadRequest, "invalid status filter")
		return
	}
	kind := strings.TrimSpace(q.Get("kind"))
	if kind != "" && !validKind(kind) {
		writeError(w, http.StatusBadRequest, "invalid kind filter")
		return
	}
	surveys, err := h.Store.ListSurveys(r.Context(), tenant.ID, status, kind)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"surveys": surveys})
}

// Get (GET /v1/surveys/surveys/{id}) returns the survey plus its
// invite/response rollup.
func (h *Handlers) Get(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid survey id")
		return
	}
	sv, err := h.Store.GetSurvey(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	stats, err := h.Store.Stats(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"survey": sv, "stats": stats})
}

// patchSurveyRequest is the PATCH /v1/surveys/surveys/{id} body. Every
// field is optional; only present fields change. Status changes run the
// status machine. Archived surveys are terminal: any PATCH → 409.
type patchSurveyRequest struct {
	Name        *string    `json:"name,omitempty"`
	Status      *string    `json:"status,omitempty"`
	Kind        *string    `json:"kind,omitempty"`
	Questions   []Question `json:"questions,omitempty"`
	TriggerKind *string    `json:"trigger_kind,omitempty"`
	Channel     *string    `json:"channel,omitempty"`
}

// Patch (PATCH /v1/surveys/surveys/{id}) applies a partial update with
// status-machine enforcement.
func (h *Handlers) Patch(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid survey id")
		return
	}
	var req patchSurveyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	sv, err := h.Store.GetSurvey(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if sv.Status == StatusArchived {
		writeError(w, http.StatusConflict, "survey is archived (terminal)")
		return
	}
	if req.Name != nil {
		sv.Name = *req.Name
	}
	if req.Kind != nil {
		sv.Kind = strings.TrimSpace(*req.Kind)
	}
	if req.TriggerKind != nil {
		sv.TriggerKind = strings.TrimSpace(*req.TriggerKind)
	}
	if req.Channel != nil {
		sv.Channel = strings.TrimSpace(*req.Channel)
	}
	if req.Questions != nil {
		if err := ValidateQuestions(req.Questions); err != nil {
			h.mapErr(w, err)
			return
		}
		sv.Questions = req.Questions
	}
	if req.Status != nil && *req.Status != sv.Status {
		if err := ValidateTransition(sv.Status, *req.Status); err != nil {
			h.mapErr(w, err)
			return
		}
		sv.Status = *req.Status
	}
	if err := sv.Validate(); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.UpdateSurvey(r.Context(), &sv); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"survey": sv})
}

// ---------------------------------------------------------------------------
// Send flow
// ---------------------------------------------------------------------------

// sendRequest is the POST /v1/surveys/surveys/{id}/send body.
type sendRequest struct {
	ContactIDs []uuid.UUID `json:"contact_ids"`
}

// inviteView is one invite as returned by the send endpoint — the token
// and rendered link are operator-facing (the operator is authorised to
// share them; the token IS the public respond capability).
type inviteView struct {
	ID        uuid.UUID `json:"id"`
	ContactID uuid.UUID `json:"contact_id"`
	Token     string    `json:"token"`
	Link      string    `json:"link"`
	Status    string    `json:"status"`
}

// Send (POST /v1/surveys/surveys/{id}/send) creates one invite per
// requested contact (queued, 128-bit token) and enqueues one PacedSend
// CloudEvent per invite to the notifications outbox (the W16/W19
// notification-worker contract — see events.go). The survey must be
// active (409 otherwise). Unresolvable contacts are skipped, not fatal.
// When the notifications topic is disabled, invites stay queued and the
// response reports sends_deferred. 200.
func (h *Handlers) Send(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid survey id")
		return
	}
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.ContactIDs) == 0 {
		writeError(w, http.StatusBadRequest, "contact_ids is required (1-500)")
		return
	}
	if len(req.ContactIDs) > maxSendContacts {
		writeError(w, http.StatusBadRequest, "contact_ids exceeds 500")
		return
	}
	// Dedupe, preserving request order.
	seen := map[uuid.UUID]bool{}
	ids := make([]uuid.UUID, 0, len(req.ContactIDs))
	for _, cid := range req.ContactIDs {
		if cid == uuid.Nil || seen[cid] {
			continue
		}
		seen[cid] = true
		ids = append(ids, cid)
	}

	sv, err := h.Store.GetSurvey(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if sv.Status != StatusActive {
		h.mapErr(w, fmt.Errorf("%w: status %s (activate it first)", ErrSurveyNotActive, sv.Status))
		return
	}

	res, err := h.Store.CreateInvites(r.Context(), tenant.ID, sv.ID, sv.Channel, ids)
	if err != nil {
		h.mapErr(w, err)
		return
	}

	views := make([]inviteView, 0, len(res.Invites))
	sent := 0
	for _, inv := range res.Invites {
		view := inviteView{ID: inv.ID, ContactID: inv.ContactID, Token: inv.Token, Link: h.inviteLink(inv.Token), Status: inv.Status}
		if h.NotificationsTopic != "" {
			payload, err := h.MarshalInvitePacedSend(tenant.Slug, sv, inv, res.Contacts[inv.ContactID])
			if err != nil {
				h.log().Warn("invite paced send marshal failed; invite stays queued",
					zap.String("invite_id", inv.ID.String()), zap.Error(err))
			} else if err := h.Store.EnqueueOutbox(r.Context(), inv.ID, h.NotificationsTopic, payload); err != nil {
				h.log().Warn("invite paced send enqueue failed; invite stays queued",
					zap.String("invite_id", inv.ID.String()), zap.Error(err))
			} else {
				if err := h.Store.MarkInviteSent(r.Context(), tenant.ID, inv.ID); err != nil {
					h.log().Warn("invite sent flip failed", zap.String("invite_id", inv.ID.String()), zap.Error(err))
				}
				inv.Status = InviteSent
				view.Status = InviteSent
				sent++
				h.publishInviteSent(r.Context(), tenant.Slug, inv, sv.Channel)
			}
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"invites":         views,
		"invites_created": len(views),
		"sent":            sent,
		"queued":          len(views) - sent,
		"skipped":         res.Skipped,
		"sends_deferred":  h.NotificationsTopic == "",
	})
}

// ---------------------------------------------------------------------------
// Results + VoC themes
// ---------------------------------------------------------------------------

// Results (GET /v1/surveys/surveys/{id}/results) returns the aggregated
// results block (response count, score distribution, NPS for kind=nps,
// mean for csat/ces, per-question single/multi breakdowns).
func (h *Handlers) Results(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid survey id")
		return
	}
	sv, err := h.Store.GetSurvey(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	responses, total, truncated, err := h.Store.ListResponses(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	results := BuildResults(sv, responses)
	results.ResponseCount = total // exact COUNT(*), not the scan cap
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "truncated": truncated})
}

// Themes (GET /v1/surveys/voc/themes?survey_id=) returns the naive keyword
// frequency over text answers (lowercase, stopwords stripped, top 20) —
// documented as naive, NOT NLP. survey_id is optional; without it the
// tenant's surveys aggregate together.
func (h *Handlers) Themes(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	var surveyID *uuid.UUID
	if v := strings.TrimSpace(r.URL.Query().Get("survey_id")); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid survey_id")
			return
		}
		surveyID = &id
	}
	texts, scanned, err := h.Store.ThemeTexts(r.Context(), tenant.ID, surveyID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"themes":            BuildThemes(texts),
		"responses_scanned": scanned,
		"naive":             true,
		"note":              "naive keyword frequency (lowercase, stopwords stripped, top 20) — not NLP",
	})
}

// ---------------------------------------------------------------------------
// PUBLIC respond
// ---------------------------------------------------------------------------

// respondRequest is the POST /v1/surveys/respond body: {token, answers}.
// The token is the ONLY credential — 128-bit random hex, delivered to the
// customer inside the invite message.
type respondRequest struct {
	Token   string         `json:"token"`
	Answers map[string]any `json:"answers"`
}

// Respond (POST /v1/surveys/respond) is the PUBLIC submit path (no tenant
// header, no JWT — registered outside the gated group). Unknown token →
// 404; already answered → 409 already_answered; expired → 410; invalid
// answers → 400. On success the answered lifecycle event + the metered
// survey_response_received usage record are enqueued (best-effort).
func (h *Handlers) Respond(w http.ResponseWriter, r *http.Request) {
	var req respondRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" || len(req.Token) > 128 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if req.Answers == nil {
		req.Answers = map[string]any{}
	}
	res, err := h.Store.SubmitResponse(r.Context(), req.Token, req.Answers)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	// The invite carries the tenant slug only implicitly; the lifecycle
	// event subject uses the tenant id (the respond path deliberately
	// performs no slug lookup — one less cross-tenant read on a public
	// endpoint).
	h.publishAnswered(r.Context(), res.Invite.TenantID.String(), res)
	writeJSON(w, http.StatusCreated, map[string]any{
		"response": map[string]any{
			"id":           res.Response.ID,
			"survey_id":    res.Response.SurveyID,
			"score":        res.Response.Score,
			"submitted_at": res.Response.SubmittedAt,
		},
		"survey": map[string]any{
			"id":   res.Survey.ID,
			"name": res.Survey.Name,
			"kind": res.Survey.Kind,
		},
	})
}
