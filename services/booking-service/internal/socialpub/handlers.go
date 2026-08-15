package socialpub

// Social-publisher HTTP API (SPEC-W21 Agent B). The package is
// self-contained per the anti-collision contract: RegisterRoutes mounts
// everything under /v1/social and the INTEGRATOR supplies the middleware
// chain (tenant resolution → JWT auth → appgate app_id "social-publisher"
// → perms) via mw, plus the TenantFromContext accessor that reads
// httpapi's request-scoped tenant value.
//
//	GET    /v1/social/accounts                 list (provider/status filters)
//	POST   /v1/social/accounts                 connect (record only — NO OAuth; docs runbook)
//	PATCH  /v1/social/accounts/{id}            status / display_name / account_ref / political flag
//	GET    /v1/social/creatives                list (kind filter)
//	POST   /v1/social/creatives                create
//	PATCH  /v1/social/creatives/{id}           partial update
//	GET    /v1/social/posts                    list (status/account_id filters)
//	POST   /v1/social/posts                    create draft|queued
//	GET    /v1/social/posts/{id}               one post
//	POST   /v1/social/posts/{id}/publish       provider publish (mock is opt-in; gates account status)
//	GET    /v1/social/ads                      list (status/account_id filters)
//	POST   /v1/social/ads                      create draft (budget/age gates at input)
//	PATCH  /v1/social/ads/{id}                 edit (draft|review|rejected) / status machine
//	POST   /v1/social/ads/{id}/launch          provider launch (political + account gates)
//	GET    /v1/social/ads/{id}/stats           provider stats (mock is opt-in)

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
	"github.com/opendesk/booking-service/internal/socialpub/provider"
	"go.uber.org/zap"
)

// Deps is the integrator-facing wiring bundle (SPEC-W21 anti-collision
// contract). The integrator builds it in httpapi/server.go:
//
//	socialpub.RegisterRoutes(r, &socialpub.Deps{
//	    Store:             socialStore,            // NewStore/DialStore
//	    Log:               logger,
//	    TenantFromContext: httpapi tenant accessor, // reads ctxTenant
//	    EventsTopic:       cfg.SocialEventsTopic,   // default opendesk.social.events.v1
//	    UsageTopic:        cfg.UsageEventsTopic,    // default opendesk.usage.events
//	    Publishers:        provider mocks (or real wiring when it lands),
//	}, tenantMw, appgateMw("social-publisher"), require("manage_bookings"|"view_analytics"))
type Deps struct {
	Store *Store
	Log   *zap.Logger
	// TenantFromContext extracts the resolved tenant injected by the
	// integrator's tenant middleware (same accessor shape as helpdesk).
	TenantFromContext func(ctx context.Context) (bookingops.TenantInfo, bool)
	// EventsTopic is the CloudEvents topic (SOCIAL_EVENTS_TOPIC, default
	// opendesk.social.events.v1). Empty disables emission.
	EventsTopic string
	// UsageTopic is the usage-metering topic (USAGE_EVENTS_TOPIC, default
	// opendesk.usage.events). Empty disables social_ad_launched metering.
	UsageTopic string
	// Publishers maps provider id (meta|tiktok|x) → Publisher. When nil or
	// missing an entry, the package lazily resolves the provider from the
	// environment (SOCIAL_MOCK / <PROVIDER>_MOCK): the deterministic mock
	// ONLY under an explicit truthy opt-in, otherwise the honest real-API
	// stub that fails closed with "not configured" (W39 SIM-005).
	Publishers map[string]provider.Publisher
}

// Handlers serves the /v1/social route group.
type Handlers struct {
	Store             *Store
	Log               *zap.Logger
	TenantFromContext func(ctx context.Context) (bookingops.TenantInfo, bool)
	EventsTopic       string
	UsageTopic        string
	publishers        map[string]provider.Publisher
}

func (h *Handlers) log() *zap.Logger {
	if h.Log != nil {
		return h.Log
	}
	return zap.NewNop()
}

// RegisterRoutes mounts the full /v1/social route group (SPEC-W21
// anti-collision contract). mw is applied to the whole group in order —
// the integrator passes tenant resolution, appgate and perms middleware.
func RegisterRoutes(r chi.Router, d *Deps, mw ...func(http.Handler) http.Handler) {
	h := &Handlers{
		Store:             d.Store,
		Log:               d.Log,
		TenantFromContext: d.TenantFromContext,
		EventsTopic:       d.EventsTopic,
		UsageTopic:        d.UsageTopic,
		publishers:        map[string]provider.Publisher{},
	}
	for k, v := range d.Publishers {
		h.publishers[k] = v
	}
	r.Route("/v1/social", func(r chi.Router) {
		r.Use(mw...)
		r.Get("/accounts", h.ListAccounts)
		r.Post("/accounts", h.CreateAccount)
		r.Patch("/accounts/{id}", h.PatchAccount)
		r.Get("/creatives", h.ListCreatives)
		r.Post("/creatives", h.CreateCreative)
		r.Patch("/creatives/{id}", h.PatchCreative)
		r.Get("/posts", h.ListPosts)
		r.Post("/posts", h.CreatePost)
		r.Get("/posts/{id}", h.GetPost)
		r.Post("/posts/{id}/publish", h.PublishPost)
		r.Get("/ads", h.ListAds)
		r.Post("/ads", h.CreateAd)
		r.Patch("/ads/{id}", h.PatchAd)
		r.Post("/ads/{id}/launch", h.LaunchAd)
		r.Get("/ads/{id}/stats", h.AdStats)
	})
}

// publisher resolves the Publisher for a provider id, lazily resolving
// the env posture when the integrator wired none (W39 SIM-005): the
// deterministic mock ONLY under an explicit SOCIAL_MOCK/<PROVIDER>_MOCK
// truthy opt-in; otherwise the honest real-API stub, which fails closed
// with "not configured" on every call.
func (h *Handlers) publisher(providerID string) (provider.Publisher, bool) {
	if p, ok := h.publishers[providerID]; ok {
		return p, true
	}
	p, ok := provider.New(providerID, provider.MockEnabledFromEnv(providerID))
	if !ok {
		return nil, false
	}
	h.publishers[providerID] = p
	return p, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// mapErr maps package errors to HTTP statuses (SPEC-W21 gates: political
// gate → 422; expired|revoked account + state-machine violations → 409;
// validation → 400).
func (h *Handlers) mapErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrAccountInactive):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrPoliticalGate):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		h.log().Error("social handler error", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// tenant resolves the request tenant via the integrator-supplied accessor.
func (h *Handlers) tenant(w http.ResponseWriter, r *http.Request) (bookingops.TenantInfo, bool) {
	if h.TenantFromContext == nil {
		writeError(w, http.StatusInternalServerError, "tenant accessor not wired")
		return bookingops.TenantInfo{}, false
	}
	t, ok := h.TenantFromContext(r.Context())
	if !ok || t.ID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "tenant context required (X-Tenant-Slug middleware)")
		return bookingops.TenantInfo{}, false
	}
	return t, true
}

func parseIDParam(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid "+name)
		return uuid.Nil, false
	}
	return id, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Accounts
// ---------------------------------------------------------------------------

// ListAccounts (GET /v1/social/accounts?provider=&status=).
func (h *Handlers) ListAccounts(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	providerF, statusF := q.Get("provider"), q.Get("status")
	if providerF != "" && !member(Providers, providerF) {
		writeError(w, http.StatusBadRequest, "invalid provider filter")
		return
	}
	if statusF != "" && !member(AccountStatuses, statusF) {
		writeError(w, http.StatusBadRequest, "invalid status filter")
		return
	}
	accounts, err := h.Store.ListAccounts(r.Context(), tenant.ID, providerF, statusF)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

type createAccountRequest struct {
	Provider      string `json:"provider"`
	AccountRef    string `json:"account_ref"`
	DisplayName   string `json:"display_name"`
	Status        string `json:"status"`
	PoliticalAuth *bool  `json:"political_ads_authorized"`
}

// CreateAccount (POST /v1/social/accounts) — "connect" is a RECORD ONLY:
// no OAuth flow exists (docs/apps/social-publisher.md runbook covers the
// out-of-band token provisioning). Default status connected.
func (h *Handlers) CreateAccount(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	var req createAccountRequest
	if !decodeBody(w, r, &req) {
		return
	}
	a := Account{
		TenantID:    tenant.ID,
		Provider:    strings.ToLower(strings.TrimSpace(req.Provider)),
		AccountRef:  req.AccountRef,
		DisplayName: req.DisplayName,
		Status:      AccountConnected,
	}
	if req.Status != "" {
		a.Status = strings.ToLower(strings.TrimSpace(req.Status))
	}
	if req.PoliticalAuth != nil {
		a.PoliticalAuth = *req.PoliticalAuth
	}
	if err := h.Store.CreateAccount(r.Context(), &a); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"account": a})
}

type patchAccountRequest struct {
	AccountRef    *string `json:"account_ref"`
	DisplayName   *string `json:"display_name"`
	Status        *string `json:"status"`
	PoliticalAuth *bool   `json:"political_ads_authorized"`
}

// PatchAccount (PATCH /v1/social/accounts/{id}).
func (h *Handlers) PatchAccount(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id")
	if !ok {
		return
	}
	var req patchAccountRequest
	if !decodeBody(w, r, &req) {
		return
	}
	a, err := h.Store.GetAccount(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if req.AccountRef != nil {
		a.AccountRef = *req.AccountRef
	}
	if req.DisplayName != nil {
		a.DisplayName = *req.DisplayName
	}
	if req.Status != nil {
		a.Status = strings.ToLower(strings.TrimSpace(*req.Status))
	}
	if req.PoliticalAuth != nil {
		a.PoliticalAuth = *req.PoliticalAuth
	}
	if err := h.Store.UpdateAccount(r.Context(), &a); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": a})
}

// ---------------------------------------------------------------------------
// Creatives
// ---------------------------------------------------------------------------

// ListCreatives (GET /v1/social/creatives?kind=).
func (h *Handlers) ListCreatives(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "" && !member(CreativeKinds, kind) {
		writeError(w, http.StatusBadRequest, "invalid kind filter")
		return
	}
	creatives, err := h.Store.ListCreatives(r.Context(), tenant.ID, kind)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"creatives": creatives})
}

type creativeRequest struct {
	Name           string  `json:"name"`
	Kind           string  `json:"kind"`
	Body           string  `json:"body"`
	MediaURL       *string `json:"media_url"`
	DisclaimerText *string `json:"disclaimer_text"`
}

// CreateCreative (POST /v1/social/creatives).
func (h *Handlers) CreateCreative(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	var req creativeRequest
	if !decodeBody(w, r, &req) {
		return
	}
	c := Creative{
		TenantID:       tenant.ID,
		Name:           req.Name,
		Kind:           strings.ToLower(strings.TrimSpace(req.Kind)),
		Body:           req.Body,
		MediaURL:       req.MediaURL,
		DisclaimerText: req.DisclaimerText,
	}
	if err := h.Store.CreateCreative(r.Context(), &c); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"creative": c})
}

// PatchCreative (PATCH /v1/social/creatives/{id}).
func (h *Handlers) PatchCreative(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id")
	if !ok {
		return
	}
	var req creativeRequest
	if !decodeBody(w, r, &req) {
		return
	}
	c, err := h.Store.GetCreative(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if strings.TrimSpace(req.Name) != "" {
		c.Name = req.Name
	}
	if strings.TrimSpace(req.Kind) != "" {
		c.Kind = strings.ToLower(strings.TrimSpace(req.Kind))
	}
	if strings.TrimSpace(req.Body) != "" {
		c.Body = req.Body
	}
	if req.MediaURL != nil {
		c.MediaURL = req.MediaURL
	}
	if req.DisclaimerText != nil {
		c.DisclaimerText = req.DisclaimerText
	}
	if err := h.Store.UpdateCreative(r.Context(), &c); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"creative": c})
}

// ---------------------------------------------------------------------------
// Posts
// ---------------------------------------------------------------------------

// ListPosts (GET /v1/social/posts?status=&account_id=).
func (h *Handlers) ListPosts(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	statusF := q.Get("status")
	if statusF != "" {
		if err := ValidatePostStatus(statusF); err != nil {
			writeError(w, http.StatusBadRequest, "invalid status filter")
			return
		}
	}
	var accountID uuid.UUID
	if raw := q.Get("account_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid account_id filter")
			return
		}
		accountID = id
	}
	posts, err := h.Store.ListPosts(r.Context(), tenant.ID, statusF, accountID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"posts": posts})
}

type createPostRequest struct {
	AccountID  string `json:"account_id"`
	CreativeID string `json:"creative_id"`
	Status     string `json:"status"` // draft|queued (default queued)
}

// CreatePost (POST /v1/social/posts) — creates a draft|queued post
// against a connected account + existing creative.
func (h *Handlers) CreatePost(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	var req createPostRequest
	if !decodeBody(w, r, &req) {
		return
	}
	accountID, err := uuid.Parse(strings.TrimSpace(req.AccountID))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid account_id")
		return
	}
	creativeID, err := uuid.Parse(strings.TrimSpace(req.CreativeID))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid creative_id")
		return
	}
	// Referenced rows must exist (and belong to this tenant — RLS).
	if _, err := h.Store.GetAccount(r.Context(), tenant.ID, accountID); err != nil {
		h.mapErr(w, err)
		return
	}
	if _, err := h.Store.GetCreative(r.Context(), tenant.ID, creativeID); err != nil {
		h.mapErr(w, err)
		return
	}
	p := Post{
		TenantID:   tenant.ID,
		AccountID:  accountID,
		CreativeID: creativeID,
		Status:     PostQueued,
	}
	if req.Status != "" {
		p.Status = strings.ToLower(strings.TrimSpace(req.Status))
	}
	if err := h.Store.CreatePost(r.Context(), &p); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"post": p})
}

// GetPost (GET /v1/social/posts/{id}).
func (h *Handlers) GetPost(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id")
	if !ok {
		return
	}
	p, err := h.Store.GetPost(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"post": p})
}

// PublishPost (POST /v1/social/posts/{id}/publish) — synchronous publish
// through the provider seam (mock only when opted in). Gates: the account must be
// connected (expired|revoked → 409). Success stamps provider_post_id +
// published_at and emits com.opendesk.social.PostPublished; provider
// failure lands the post in failed with the error recorded (502 to the
// caller, so the UI can surface it honestly).
func (h *Handlers) PublishPost(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()
	p, err := h.Store.GetPost(ctx, tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	switch p.Status {
	case PostDraft, PostQueued, PostFailed:
		// publishable (failed = retry)
	case PostPublished:
		writeError(w, http.StatusConflict, "post is already published")
		return
	default: // publishing
		h.mapErr(w, fmt.Errorf("%w: post is already publishing", ErrInvalidTransition))
		return
	}
	account, err := h.Store.GetAccount(ctx, tenant.ID, p.AccountID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if !account.Connected() {
		h.mapErr(w, errAccountInactive(account))
		return
	}
	creative, err := h.Store.GetCreative(ctx, tenant.ID, p.CreativeID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	pub, ok := h.publisher(account.Provider)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider "+account.Provider)
		return
	}
	mediaURL := ""
	if creative.MediaURL != nil {
		mediaURL = *creative.MediaURL
	}
	providerPostID, err := pub.PublishPost(ctx, provider.PostRequest{
		TenantID:   tenant.ID.String(),
		AccountID:  account.ID.String(),
		AccountRef: account.AccountRef,
		CreativeID: creative.ID.String(),
		Body:       creative.Body,
		MediaURL:   mediaURL,
		Disclaimer: EffectiveDisclaimer(nil, &creative),
	})
	if err != nil {
		if uerr := h.Store.CompletePublish(ctx, tenant.ID, p.ID, "", err.Error()); uerr != nil {
			h.log().Error("record publish failure failed", zap.Error(uerr))
		}
		writeError(w, http.StatusBadGateway, "provider publish failed: "+err.Error())
		return
	}
	if err := h.Store.CompletePublish(ctx, tenant.ID, p.ID, providerPostID, ""); err != nil {
		h.mapErr(w, err)
		return
	}
	p, err = h.Store.GetPost(ctx, tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	h.emit(ctx, p.ID, EventTypePostPublished, tenant.Slug, tenant.ID.String(), postPublishedData(p, account.Provider))
	writeJSON(w, http.StatusOK, map[string]any{"post": p})
}

// ---------------------------------------------------------------------------
// Ads
// ---------------------------------------------------------------------------

// ListAds (GET /v1/social/ads?status=&account_id=).
func (h *Handlers) ListAds(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	statusF := q.Get("status")
	if statusF != "" {
		if err := ValidateAdStatus(statusF); err != nil {
			writeError(w, http.StatusBadRequest, "invalid status filter")
			return
		}
	}
	var accountID uuid.UUID
	if raw := q.Get("account_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid account_id filter")
			return
		}
		accountID = id
	}
	ads, err := h.Store.ListAds(r.Context(), tenant.ID, statusF, accountID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ads": ads})
}

type adRequest struct {
	AccountID       string     `json:"account_id"`
	CreativeID      string     `json:"creative_id"`
	Name            string     `json:"name"`
	Objective       string     `json:"objective"`
	BudgetKobo      int64      `json:"budget_kobo"`
	DailyBudgetKobo int64      `json:"daily_budget_kobo"`
	Targeting       *Targeting `json:"targeting"`
	Political       bool       `json:"political"`
	DisclaimerText  *string    `json:"disclaimer_text"`
}

// CreateAd (POST /v1/social/ads) — validates the budget/age gates at
// input (400); the political + account gates fire at LAUNCH (422/409).
func (h *Handlers) CreateAd(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	var req adRequest
	if !decodeBody(w, r, &req) {
		return
	}
	accountID, err := uuid.Parse(strings.TrimSpace(req.AccountID))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid account_id")
		return
	}
	creativeID, err := uuid.Parse(strings.TrimSpace(req.CreativeID))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid creative_id")
		return
	}
	if _, err := h.Store.GetAccount(r.Context(), tenant.ID, accountID); err != nil {
		h.mapErr(w, err)
		return
	}
	if _, err := h.Store.GetCreative(r.Context(), tenant.ID, creativeID); err != nil {
		h.mapErr(w, err)
		return
	}
	a := Ad{
		TenantID:        tenant.ID,
		AccountID:       accountID,
		CreativeID:      creativeID,
		Name:            req.Name,
		Objective:       strings.ToLower(strings.TrimSpace(req.Objective)),
		BudgetKobo:      req.BudgetKobo,
		DailyBudgetKobo: req.DailyBudgetKobo,
		Political:       req.Political,
		DisclaimerText:  req.DisclaimerText,
	}
	if req.Targeting != nil {
		a.Targeting = *req.Targeting
	}
	if err := h.Store.CreateAd(r.Context(), &a); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ad": a})
}

type patchAdRequest struct {
	Name            *string    `json:"name"`
	Objective       *string    `json:"objective"`
	BudgetKobo      *int64     `json:"budget_kobo"`
	DailyBudgetKobo *int64     `json:"daily_budget_kobo"`
	Targeting       *Targeting `json:"targeting"`
	Political       *bool      `json:"political"`
	DisclaimerText  *string    `json:"disclaimer_text"`
	Status          *string    `json:"status"` // operator state machine (active⇄paused, reject, …)
}

// PatchAd (PATCH /v1/social/ads/{id}) — field edits (draft|review|
// rejected only, enforced by the store) and/or an operator status
// transition (the adTransitions machine; 409 on illegal edges).
func (h *Handlers) PatchAd(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id")
	if !ok {
		return
	}
	var req patchAdRequest
	if !decodeBody(w, r, &req) {
		return
	}
	ctx := r.Context()
	a, err := h.Store.GetAd(ctx, tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	changed := false
	if req.Name != nil {
		a.Name = *req.Name
		changed = true
	}
	if req.Objective != nil {
		a.Objective = strings.ToLower(strings.TrimSpace(*req.Objective))
		changed = true
	}
	if req.BudgetKobo != nil {
		a.BudgetKobo = *req.BudgetKobo
		changed = true
	}
	if req.DailyBudgetKobo != nil {
		a.DailyBudgetKobo = *req.DailyBudgetKobo
		changed = true
	}
	if req.Targeting != nil {
		a.Targeting = *req.Targeting
		changed = true
	}
	if req.Political != nil {
		a.Political = *req.Political
		changed = true
	}
	if req.DisclaimerText != nil {
		a.DisclaimerText = req.DisclaimerText
		changed = true
	}
	if changed {
		if err := h.Store.UpdateAd(ctx, &a); err != nil {
			h.mapErr(w, err)
			return
		}
	}
	if req.Status != nil {
		to := strings.ToLower(strings.TrimSpace(*req.Status))
		a, err = h.Store.SetAdStatus(ctx, tenant.ID, id, to, "", "")
		if err != nil {
			h.mapErr(w, err)
			return
		}
	} else if changed {
		a, err = h.Store.GetAd(ctx, tenant.ID, id)
		if err != nil {
			h.mapErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ad": a})
}

// LaunchAd (POST /v1/social/ads/{id}/launch) — the gated launch:
//
//  1. ad must be draft (else 409 — the launch edge is draft→review);
//  2. account must be connected (expired|revoked → 409);
//  3. political=true requires account.political_ads_authorized AND a
//     non-empty effective disclaimer (else 422);
//  4. provider launch (mock only when opted in): rejection → status rejected +
//     AdRejected event; success → status review + provider_ad_id +
//     AdLaunched event + social_ad_launched metering.
func (h *Handlers) LaunchAd(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()
	a, err := h.Store.GetAd(ctx, tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if a.Status != AdDraft {
		h.mapErr(w, fmt.Errorf("%w: launch requires a draft ad (status is %s)", ErrInvalidTransition, a.Status))
		return
	}
	account, err := h.Store.GetAccount(ctx, tenant.ID, a.AccountID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	creative, err := h.Store.GetCreative(ctx, tenant.ID, a.CreativeID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if err := checkLaunchGate(&a, &account, &creative); err != nil {
		h.mapErr(w, err)
		return
	}
	pub, ok := h.publisher(account.Provider)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider "+account.Provider)
		return
	}
	providerAdID, rejected, reason, err := pub.LaunchAd(ctx, provider.AdRequest{
		TenantID:        tenant.ID.String(),
		AccountID:       account.ID.String(),
		AccountRef:      account.AccountRef,
		AdID:            a.ID.String(),
		CreativeID:      a.CreativeID.String(),
		Name:            a.Name,
		Objective:       a.Objective,
		BudgetKobo:      a.BudgetKobo,
		DailyBudgetKobo: a.DailyBudgetKobo,
		Political:       a.Political,
		Disclaimer:      EffectiveDisclaimer(&a, &creative),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider launch failed: "+err.Error())
		return
	}
	if rejected {
		a, err = h.Store.SetAdStatus(ctx, tenant.ID, id, AdRejected, "", reason)
		if err != nil {
			h.mapErr(w, err)
			return
		}
		h.emit(ctx, a.ID, EventTypeAdRejected, tenant.Slug, tenant.ID.String(), adRejectedData(a, account.Provider, reason))
		writeJSON(w, http.StatusOK, map[string]any{"ad": a, "rejected": true, "reason": truncate(reason, maxErrorLen)})
		return
	}
	a, err = h.Store.SetAdStatus(ctx, tenant.ID, id, AdReview, providerAdID, "")
	if err != nil {
		h.mapErr(w, err)
		return
	}
	h.emit(ctx, a.ID, EventTypeAdLaunched, tenant.Slug, tenant.ID.String(), adLaunchedData(a, account.Provider))
	// W39 SIM-006: simulated (mock) launches are NOT billable usage —
	// metering counts only launches through a real provider rail.
	if !provider.IsMock(pub) {
		h.meterLaunched(ctx, a, account.Provider, tenant.Slug)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ad": a, "rejected": false})
}

// AdStats (GET /v1/social/ads/{id}/stats) — provider stats for a launched
// ad (with the mock opted in: deterministic plausible numbers keyed on the provider
// ad id). 409 when the ad has never reached a provider (no provider_ad_id).
func (h *Handlers) AdStats(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()
	a, err := h.Store.GetAd(ctx, tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if a.ProviderAdID == nil || *a.ProviderAdID == "" {
		writeError(w, http.StatusConflict, "ad has not been launched yet — no provider stats")
		return
	}
	account, err := h.Store.GetAccount(ctx, tenant.ID, a.AccountID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	pub, ok := h.publisher(account.Provider)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider "+account.Provider)
		return
	}
	stats, err := pub.AdStats(ctx, *a.ProviderAdID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider stats failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ad_id":          a.ID.String(),
		"provider_ad_id": *a.ProviderAdID,
		"provider":       account.Provider,
		"mock":           provider.IsMock(pub), // honest disclosure of the rail posture
		"stats":          stats,
	})
}

// errAccountInactive builds the 409 error for publish/launch on a
// non-connected account.
func errAccountInactive(a Account) error {
	return fmt.Errorf("%w: account %s is %s — reconnect before publishing",
		ErrAccountInactive, a.ID, a.Status)
}
