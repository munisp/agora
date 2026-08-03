// Package httpapi wires the chi router and REST handlers for identity-service.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/opendesk/identity-service/internal/consent"
	"github.com/opendesk/identity-service/internal/daprc"
	"github.com/opendesk/identity-service/internal/events"
	"github.com/opendesk/identity-service/internal/keycloak"
	"github.com/opendesk/identity-service/internal/packs"
	"github.com/opendesk/identity-service/internal/permify"
	"github.com/opendesk/identity-service/internal/store"
	"go.uber.org/zap"
)

// Deps bundles server dependencies.
type Deps struct {
	Store             *store.Store
	Keycloak          *keycloak.Client
	Permify           *permify.HTTPClient
	Dapr              *daprc.Client
	PubSub            string
	Topic             string // identity events topic
	NotificationAppID string // Dapr app-id of notification-worker
	Packs             *packs.Registry
	Logger            *zap.Logger
	// Consents is the NDPA consent registry handler (SPEC-W12 §4); nil
	// disables the consent routes (tests).
	Consents *consent.Handler
}

// NewRouter builds the chi router with all routes.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	s := &server{d: d}

	r.Get("/healthz", s.healthz)
	r.Route("/v1", func(r chi.Router) {
		r.Post("/tenants", s.createTenant)
		r.Get("/tenants/{slug}", s.getTenant)
		// SPEC-W3 §3 innovation 12: tenant deletion (twin cleanup + guarded
		// admin deletion — see twin.go for the guard rules).
		r.Delete("/tenants/{slug}", s.deleteTenant)
		r.Get("/tenants/{slug}/members", s.listMembers)
		r.Post("/tenants/{slug}/members", s.inviteMember)
	})
	// Idempotent internal endpoints used by the TenantOnboardingWorkflow
	// (SPEC §6) via Dapr service invocation.
	r.Route("/internal/tenants/{slug}", func(r chi.Router) {
		r.Post("/ensure-group", s.ensureGroup)
		r.Post("/ensure-permify", s.ensurePermify)
		r.Post("/terminology", s.mergeTerminology)
		// SPEC-W3 §3 innovation 12: digital-twin provisioning.
		r.Post("/twin", s.createTwin)
	})
	// SPEC-W12 §4: NDPA consent registry (/v1/consents*, /internal/consents/check).
	if d.Consents != nil {
		d.Consents.RegisterRoutes(r)
	}
	return r
}

type server struct{ d Deps }

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.d.Store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "db unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// getTenant returns the public tenant context used by the agent session
// injection path: name, timezone, currency, locale, terminology, plan.
func (s *server) getTenant(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	t, err := s.d.Store.GetTenantBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	resp := map[string]any{
		"id":          t.ID,
		"slug":        t.Slug,
		"name":        t.Name,
		"timezone":    t.Timezone,
		"currency":    t.Currency,
		"locale":      t.Locale,
		"terminology": t.Terminology,
		"plan":        t.Plan,
		"industry":    t.Industry,
	}
	// SPEC-CRM §C1: resolved pack summary with the tenant's terminology
	// overrides merged over the pack defaults.
	if s.d.Packs != nil {
		if p, ok := s.d.Packs.Get(t.Industry); ok {
			resp["pack"] = p.Summary(terminologyOverrides(t.Terminology))
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// terminologyOverrides flattens the tenant's terminology JSON document into a
// string map (non-string values are ignored).
func terminologyOverrides(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return out
	}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

type createTenantRequest struct {
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Timezone    string          `json:"timezone"`
	Currency    string          `json:"currency"`
	Locale      string          `json:"locale"`
	Terminology json.RawMessage `json:"terminology"`
	Plan        string          `json:"plan"`
	Industry    string          `json:"industry"`
	OwnerUserID string          `json:"owner_user_id"`
}

// createTenant provisions a tenant: DB row, Keycloak group /tenants/{slug},
// Permify tenant + owner relationship, and publishes TenantProvisioned.
func (s *server) createTenant(w http.ResponseWriter, r *http.Request) {
	var req createTenantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !slugRe.MatchString(req.Slug) {
		writeError(w, http.StatusBadRequest, "slug must be 2-63 chars of [a-z0-9-]")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Timezone == "" {
		req.Timezone = "UTC"
	}
	if req.Currency == "" {
		req.Currency = "USD"
	}
	if req.Locale == "" {
		req.Locale = "en-US"
	}
	if req.Plan == "" {
		req.Plan = "free"
	}
	if len(req.Terminology) == 0 {
		req.Terminology = json.RawMessage(`{}`)
	}
	if req.Industry == "" {
		req.Industry = packs.DefaultIndustry
	}
	// SPEC-CRM §C1: industry must reference a loaded pack id.
	if s.d.Packs != nil && !s.d.Packs.Has(req.Industry) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("unknown industry %q (loaded packs: %s)", req.Industry, strings.Join(s.d.Packs.IDs(), ", ")))
		return
	}

	t := store.Tenant{
		Slug:        req.Slug,
		Name:        req.Name,
		Timezone:    req.Timezone,
		Currency:    req.Currency,
		Locale:      req.Locale,
		Terminology: req.Terminology,
		Plan:        req.Plan,
		Industry:    req.Industry,
	}
	if err := s.d.Store.CreateTenant(r.Context(), &t); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "slug already taken")
			return
		}
		s.internal(w, err)
		return
	}

	// Keycloak group /tenants/{slug} (best-effort: logged, not fatal — the
	// TenantOnboardingWorkflow retries this step durably, SPEC §6).
	if _, err := s.d.Keycloak.CreateTenantGroup(r.Context(), t.Slug); err != nil {
		s.d.Logger.Warn("keycloak group creation deferred to onboarding workflow",
			zap.String("slug", t.Slug), zap.Error(err))
	}

	// Permify tenant + owner relationship.
	if err := s.d.Permify.CreateTenant(r.Context(), t.ID.String(), t.Slug); err != nil {
		s.d.Logger.Warn("permify tenant creation deferred", zap.String("slug", t.Slug), zap.Error(err))
	}
	if req.OwnerUserID != "" {
		if err := s.d.Permify.WriteRelationship(r.Context(), t.ID.String(),
			"organization:"+t.ID.String(), "owner", "user:"+req.OwnerUserID); err != nil {
			s.d.Logger.Warn("permify owner relationship deferred", zap.Error(err))
		}
	}

	// CloudEvent: TenantProvisioned on opendesk.identity.events.
	evt := events.New("identity-service", "com.opendesk.identity.TenantProvisioned", t.Slug, t.ID.String(), map[string]any{
		"tenant_id": t.ID.String(),
		"slug":      t.Slug,
		"name":      t.Name,
		"plan":      t.Plan,
		"industry":  t.Industry,
	})
	if err := s.d.Dapr.PublishEvent(r.Context(), s.d.PubSub, s.d.Topic, evt); err != nil {
		// provisioning succeeded; event delivery is retried by onboarding flow
		s.d.Logger.Error("failed to publish TenantProvisioned", zap.Error(err))
	}

	// Fire-and-forget: kick off the durable TenantOnboardingWorkflow in
	// notification-worker (Keycloak group, Permify tenant, site seed, search
	// alias — SPEC §6). Failures are logged, never fail provisioning.
	go s.triggerOnboarding(t)

	writeJSON(w, http.StatusCreated, t)
}

// triggerOnboarding invokes notification-worker's POST /dev/trigger-onboarding
// via Dapr service invocation with a fresh context (the request context dies
// with the response).
func (s *server) triggerOnboarding(t store.Tenant) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := s.d.Dapr.InvokeService(ctx, s.d.NotificationAppID, "dev/trigger-onboarding", map[string]any{
		"tenant_id":   t.ID.String(),
		"tenant_slug": t.Slug,
		"slug":        t.Slug,
		"name":        t.Name,
		"plan":        t.Plan,
		"industry":    t.Industry,
	}, nil)
	if err != nil {
		s.d.Logger.Error("failed to trigger TenantOnboardingWorkflow",
			zap.String("slug", t.Slug), zap.Error(err))
		return
	}
	s.d.Logger.Info("TenantOnboardingWorkflow triggered", zap.String("slug", t.Slug))
}

func (s *server) listMembers(w http.ResponseWriter, r *http.Request) {
	t, err := s.tenant(w, r)
	if err != nil {
		return
	}
	members, err := s.d.Store.ListMembers(r.Context(), t.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members})
}

type inviteMemberRequest struct {
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Role      string `json:"role"`
}

var memberRoles = map[string]bool{"owner": true, "admin": true, "staff": true, "viewer": true}

// inviteMember creates the Keycloak user, a membership row and the Permify
// relationship, then publishes MemberInvited.
func (s *server) inviteMember(w http.ResponseWriter, r *http.Request) {
	t, err := s.tenant(w, r)
	if err != nil {
		return
	}
	var req inviteMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}
	if req.Role == "" {
		req.Role = "staff"
	}
	if !memberRoles[req.Role] {
		writeError(w, http.StatusBadRequest, "role must be owner|admin|staff|viewer")
		return
	}

	userID, err := s.d.Keycloak.CreateUser(r.Context(), t.Slug, keycloak.CreateUserInput{
		Email:     req.Email,
		FirstName: req.FirstName,
		LastName:  req.LastName,
	})
	if err != nil {
		s.d.Logger.Error("keycloak create user", zap.Error(err))
		writeError(w, http.StatusBadGateway, "identity provider error")
		return
	}
	if err := s.d.Store.AddMember(r.Context(), store.Membership{
		TenantID: t.ID, UserID: userID, Role: req.Role,
	}); err != nil {
		s.internal(w, err)
		return
	}
	// map realm roles to Permify relations: staff -> member
	relation := req.Role
	if relation == "staff" {
		relation = "member"
	}
	if err := s.d.Permify.WriteRelationship(r.Context(), t.ID.String(),
		"organization:"+t.ID.String(), relation, "user:"+userID); err != nil {
		s.d.Logger.Warn("permify member relationship deferred", zap.Error(err))
	}

	evt := events.New("identity-service", "com.opendesk.identity.MemberInvited", t.Slug, t.ID.String(), map[string]any{
		"tenant_id": t.ID.String(),
		"user_id":   userID,
		"email":     req.Email,
		"role":      req.Role,
	})
	if err := s.d.Dapr.PublishEvent(r.Context(), s.d.PubSub, s.d.Topic, evt); err != nil {
		s.d.Logger.Error("failed to publish MemberInvited", zap.Error(err))
	}

	writeJSON(w, http.StatusCreated, map[string]string{"user_id": userID, "role": req.Role})
}

// ensureGroup (POST /internal/tenants/{slug}/ensure-group) idempotently
// creates the Keycloak group /tenants/{slug}.
func (s *server) ensureGroup(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if _, err := s.d.Store.GetTenantBySlug(r.Context(), slug); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "tenant not found")
			return
		}
		s.internal(w, err)
		return
	}
	groupID, err := s.d.Keycloak.CreateTenantGroup(r.Context(), slug)
	if err != nil {
		s.d.Logger.Error("ensure keycloak group", zap.String("slug", slug), zap.Error(err))
		writeError(w, http.StatusBadGateway, "keycloak error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"group_id": groupID})
}

// ensurePermify (POST /internal/tenants/{slug}/ensure-permify) idempotently
// creates the Permify tenant for relationship storage.
func (s *server) ensurePermify(w http.ResponseWriter, r *http.Request) {
	t, err := s.tenant(w, r)
	if err != nil {
		return
	}
	if err := s.d.Permify.CreateTenant(r.Context(), t.ID.String(), t.Slug); err != nil {
		s.d.Logger.Error("ensure permify tenant", zap.String("slug", t.Slug), zap.Error(err))
		writeError(w, http.StatusBadGateway, "permify error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"permify_tenant": t.ID.String()})
}

// mergeTerminology (POST /internal/tenants/{slug}/terminology) merge-patches
// the tenant's terminology overrides. Body is a flat JSON object of
// terminology keys (e.g. from an industry pack); patch keys win over stored
// ones. Used by the onboarding ApplyIndustryPack activity (SPEC-CRM §C2).
func (s *server) mergeTerminology(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	var patch map[string]string
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body (object of string values expected)")
		return
	}
	if len(patch) == 0 {
		writeError(w, http.StatusBadRequest, "empty terminology patch")
		return
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		s.internal(w, err)
		return
	}
	merged, err := s.d.Store.MergeTerminology(r.Context(), slug, raw)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"slug": slug, "terminology": merged})
}

func (s *server) tenant(w http.ResponseWriter, r *http.Request) (store.Tenant, error) {
	t, err := s.d.Store.GetTenantBySlug(r.Context(), chi.URLParam(r, "slug"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "tenant not found")
		return t, err
	}
	if err != nil {
		s.internal(w, err)
	}
	return t, err
}

func (s *server) internal(w http.ResponseWriter, err error) {
	s.d.Logger.Error("internal error", zap.Error(err))
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
