package crm360

// CRM-360 HTTP API (SPEC-W20 Agent A). Routes (mounted by RegisterRoutes
// on the ROOT chi router — the /v1 prefix is included):
//
//	GET    /v1/crm/contacts/search?q=&tag=&limit=
//	GET    /v1/crm/contacts/{id}/360
//	GET    /v1/crm/contacts/{id}/timeline?limit=
//	GET    /v1/crm/contacts/{id}/tags
//	POST   /v1/crm/contacts/{id}/tags          {tag}
//	DELETE /v1/crm/contacts/{id}/tags/{tag}
//	GET    /v1/crm/contacts/{id}/notes
//	POST   /v1/crm/contacts/{id}/notes         {body, pinned?, author?}
//	PATCH  /v1/crm/notes/{id}                  {body?, pinned?}
//
// AuthZ (SPEC-W20 contract §3): reads are view_analytics, writes are
// manage_bookings. RegisterRoutes applies the variadic mw GROUP-WIDE; the
// INTEGRATOR wires the permission middleware — recommended shape (the
// W19 requireReadWrite chain):
//
//	method-aware: GET/HEAD → require("view_analytics"), else require("manage_bookings")
//
// plus the appgate entitlement gate for app_id "crm-360".
//
// Tenant context: this package resolves the tenant itself from the
// X-Tenant-Slug header via Deps.Resolver (httpapi's tenantMiddleware ctx
// key is unexported, so the self-contained package cannot reuse it) and
// stores it under its own key; handlers read it via TenantFromContext —
// the contract §3 helper idiom, mirroring internal/workorders.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
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

// ConsentResolver is the OPTIONAL consent lookup hook (SPEC-W20: "consent
// status if resolvable"). Consent records live in identity-service's
// consents table — a different database — so this package cannot join
// them; the integrator may wire an HTTP/GRPC-backed resolver. Nil (the
// zero-config default) → the 360 profile answers consent=null. A resolver
// error degrades to null (logged), never to a failed profile.
type ConsentResolver interface {
	ConsentStatus(ctx context.Context, tenantID, contactID uuid.UUID) (string, error)
}

// Deps are the integration seams the integrator wires (SPEC-W20
// anti-collision contract). CRMEventsTopic is OPTIONAL: empty disables
// event emission (graceful no-op).
type Deps struct {
	Store    *Store
	Resolver TenantResolver
	Logger   *zap.Logger
	// CRMEventsTopic is the lifecycle-events topic
	// (opendesk.crm.events.v1). Empty disables note/pin/tag events.
	CRMEventsTopic string
	// UserFromContext resolves the staff identity (JWT sub) for the note
	// author. Optional — nil falls back to the request body's author
	// field, then "".
	UserFromContext func(ctx context.Context) string
	// ConsentResolver optionally resolves the contact's consent status
	// (see the ConsentResolver doc). Nil → consent=null on profiles.
	ConsentResolver ConsentResolver
}

// Handlers serves the crm-360 endpoints. Constructed by RegisterRoutes;
// exported fields allow direct wiring in tests (mirrors
// workorders.Handlers).
type Handlers struct {
	Store           *Store
	Log             *zap.Logger
	EventsTopic     string
	UserFromContext func(ctx context.Context) string
	ConsentResolver ConsentResolver
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

const ctxTenant ctxKey = "crm360-tenant"

// TenantFromContext returns the tenant resolved by the package tenant
// middleware (contract §3 helper). The zero value (ID == uuid.Nil) means
// no tenant context — handlers treat it as 400.
func TenantFromContext(ctx context.Context) bookingops.TenantInfo {
	t, _ := ctx.Value(ctxTenant).(bookingops.TenantInfo)
	return t
}

// RegisterRoutes mounts the crm-360 API at /v1/crm on the given router
// (call it on the ROOT router — the /v1 prefix is included). mw are
// applied group-wide AFTER the package tenant middleware (see the file
// comment for the integrator's recommended authZ shape).
func RegisterRoutes(r chi.Router, d *Deps, mw ...func(http.Handler) http.Handler) {
	h := &Handlers{
		Store:           d.Store,
		Log:             d.Logger,
		EventsTopic:     d.CRMEventsTopic,
		UserFromContext: d.UserFromContext,
		ConsentResolver: d.ConsentResolver,
	}
	r.Route("/v1/crm", func(r chi.Router) {
		r.Use(tenantMiddleware(d.Resolver, d.Logger))
		r.Use(mw...)
		r.Get("/contacts/search", h.Search)
		r.Get("/contacts/{id}/360", h.Profile)
		r.Get("/contacts/{id}/timeline", h.Timeline)
		r.Get("/contacts/{id}/tags", h.ListTags)
		r.Post("/contacts/{id}/tags", h.AddTag)
		r.Delete("/contacts/{id}/tags/{tag}", h.RemoveTag)
		r.Get("/contacts/{id}/notes", h.ListNotes)
		r.Post("/contacts/{id}/notes", h.CreateNote)
		r.Patch("/notes/{id}", h.PatchNote)
	})
}

// tenantMiddleware resolves X-Tenant-Slug via the resolver and stores the
// TenantInfo under the package key. 503 when no resolver is wired
// (partial deployment), 400 without a slug, 404 on resolution failure —
// mirroring httpapi.tenantMiddleware's status codes (and the workorders
// package-local middleware).
func tenantMiddleware(resolver TenantResolver, log *zap.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = zap.NewNop()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if resolver == nil {
				writeError(w, http.StatusServiceUnavailable, "crm-360 unavailable (tenant resolver not wired)")
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
	default:
		h.log().Error("crm360 handler error", zap.Error(err))
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

// contactParam parses the {id} contact path parameter or writes 400.
func contactParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid contact id")
		return uuid.Nil, false
	}
	return id, true
}

// limitParam parses ?limit= (0/invalid → def, capped at max).
func limitParam(r *http.Request, def, max int) (int, error) {
	v := strings.TrimSpace(r.URL.Query().Get("limit"))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, errors.New("invalid limit (want an integer)")
	}
	if n <= 0 {
		return def, nil
	}
	if n > max {
		n = max
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// Search (GET /v1/crm/contacts/search?q=&tag=&limit=) — name/phone/email
// prefix search plus optional tag filter. The tag filter is normalized +
// validated like a write (an invalid tag shape is a 400, not an empty
// result, so operator typos surface).
func (h *Handlers) Search(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	tag := NormalizeTag(q.Get("tag"))
	if tag != "" {
		if err := ValidateTag(tag); err != nil {
			h.mapErr(w, err)
			return
		}
	}
	limit, err := limitParam(r, defaultSearchLimit, maxSearchLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	results, err := h.Store.SearchContacts(r.Context(), tenant.ID, q.Get("q"), tag, limit)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"contacts": results})
}

// ---------------------------------------------------------------------------
// 360 profile + timeline
// ---------------------------------------------------------------------------

// Profile (GET /v1/crm/contacts/{id}/360) — the unified customer profile.
// Optional sections degrade to empty arrays when their source table is
// absent; consent is null unless a ConsentResolver is wired.
func (h *Handlers) Profile(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	contactID, ok := contactParam(w, r)
	if !ok {
		return
	}
	p, err := h.Store.Profile360(r.Context(), tenant.ID, contactID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if h.ConsentResolver != nil {
		status, err := h.ConsentResolver.ConsentStatus(r.Context(), tenant.ID, contactID)
		if err != nil {
			h.log().Warn("consent resolution failed; answering consent=null",
				zap.String("contact_id", contactID.String()), zap.Error(err))
		} else if status != "" {
			p.Consent = &status
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profile": p})
}

// Timeline (GET /v1/crm/contacts/{id}/timeline?limit=) — the merged
// chronological feed ({ts, kind, summary, ref_id}).
func (h *Handlers) Timeline(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	contactID, ok := contactParam(w, r)
	if !ok {
		return
	}
	limit, err := limitParam(r, defaultTimelineLimit, maxTimelineLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	items, err := h.Store.Timeline(r.Context(), tenant.ID, contactID, limit)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"timeline": items})
}

// ---------------------------------------------------------------------------
// Tags
// ---------------------------------------------------------------------------

// ListTags (GET /v1/crm/contacts/{id}/tags).
func (h *Handlers) ListTags(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	contactID, ok := contactParam(w, r)
	if !ok {
		return
	}
	tags, err := h.Store.ListTags(r.Context(), tenant.ID, contactID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
}

// addTagRequest is the POST /v1/crm/contacts/{id}/tags body.
type addTagRequest struct {
	Tag string `json:"tag"`
}

// AddTag (POST /v1/crm/contacts/{id}/tags) attaches a tag (normalized to
// lowercase, validated against [a-z0-9-_]{1,40}). Idempotent — re-adding
// returns 200 with the full tag set. Emits com.opendesk.crm.TagAdded only
// when the tag was newly attached (no event on idempotent replay).
func (h *Handlers) AddTag(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	contactID, ok := contactParam(w, r)
	if !ok {
		return
	}
	var req addTagRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	tag := NormalizeTag(req.Tag)
	if err := ValidateTag(tag); err != nil {
		h.mapErr(w, err)
		return
	}
	// Distinguish a fresh attach (event-worthy) from an idempotent replay.
	existing, err := h.Store.ListTags(r.Context(), tenant.ID, contactID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	fresh := true
	for _, t := range existing {
		if t == tag {
			fresh = false
			break
		}
	}
	if err := h.Store.AddTag(r.Context(), tenant.ID, contactID, tag); err != nil {
		h.mapErr(w, err)
		return
	}
	if fresh {
		h.publishTagEvent(r.Context(), tenant.Slug, EventTypeTagAdded, tenant.ID, contactID, tag)
	}
	tags, err := h.Store.ListTags(r.Context(), tenant.ID, contactID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
}

// RemoveTag (DELETE /v1/crm/contacts/{id}/tags/{tag}) detaches a tag
// (404 when not attached). Emits com.opendesk.crm.TagRemoved.
func (h *Handlers) RemoveTag(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	contactID, ok := contactParam(w, r)
	if !ok {
		return
	}
	tag := NormalizeTag(chi.URLParam(r, "tag"))
	if err := ValidateTag(tag); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.RemoveTag(r.Context(), tenant.ID, contactID, tag); err != nil {
		h.mapErr(w, err)
		return
	}
	h.publishTagEvent(r.Context(), tenant.Slug, EventTypeTagRemoved, tenant.ID, contactID, tag)
	tags, err := h.Store.ListTags(r.Context(), tenant.ID, contactID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
}

// ---------------------------------------------------------------------------
// Notes
// ---------------------------------------------------------------------------

// ListNotes (GET /v1/crm/contacts/{id}/notes) — pinned first, then newest.
func (h *Handlers) ListNotes(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	contactID, ok := contactParam(w, r)
	if !ok {
		return
	}
	notes, err := h.Store.ListNotes(r.Context(), tenant.ID, contactID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": notes})
}

// createNoteRequest is the POST /v1/crm/contacts/{id}/notes body. Author
// defaults to the staff identity (Deps.UserFromContext, wired from the
// JWT sub); the explicit author field is a fallback for unwired dev
// deployments.
type createNoteRequest struct {
	Body   string `json:"body"`
	Pinned bool   `json:"pinned,omitempty"`
	Author string `json:"author,omitempty"`
}

// CreateNote (POST /v1/crm/contacts/{id}/notes) adds a note. 201. Emits
// com.opendesk.crm.NoteCreated.
func (h *Handlers) CreateNote(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	contactID, ok := contactParam(w, r)
	if !ok {
		return
	}
	var req createNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	author := strings.TrimSpace(req.Author)
	if h.UserFromContext != nil {
		if sub := strings.TrimSpace(h.UserFromContext(r.Context())); sub != "" {
			author = sub
		}
	}
	n := Note{
		TenantID:  tenant.ID,
		ContactID: contactID,
		Author:    author,
		Body:      req.Body,
		Pinned:    req.Pinned,
	}
	if err := n.Validate(); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.CreateNote(r.Context(), &n); err != nil {
		h.mapErr(w, err)
		return
	}
	h.publishNoteEvent(r.Context(), tenant.Slug, EventTypeNoteCreated, n)
	writeJSON(w, http.StatusCreated, map[string]any{"note": n})
}

// patchNoteRequest is the PATCH /v1/crm/notes/{id} body: every field
// optional; body edits the text, pinned toggles the pin.
type patchNoteRequest struct {
	Body   *string `json:"body,omitempty"`
	Pinned *bool   `json:"pinned,omitempty"`
}

// PatchNote (PATCH /v1/crm/notes/{id}) edits the body and/or toggles the
// pin. Emits com.opendesk.crm.NoteUpdated.
func (h *Handlers) PatchNote(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid note id")
		return
	}
	var req patchNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Body == nil && req.Pinned == nil {
		writeError(w, http.StatusBadRequest, "nothing to update (want body and/or pinned)")
		return
	}
	n, err := h.Store.GetNote(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if req.Body != nil {
		n.Body = *req.Body
	}
	if req.Pinned != nil {
		n.Pinned = *req.Pinned
	}
	if err := n.Validate(); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Store.UpdateNote(r.Context(), &n); err != nil {
		h.mapErr(w, err)
		return
	}
	h.publishNoteEvent(r.Context(), tenant.Slug, EventTypeNoteUpdated, n)
	writeJSON(w, http.StatusOK, map[string]any{"note": n})
}
