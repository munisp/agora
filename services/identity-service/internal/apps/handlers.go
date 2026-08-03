package apps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/identity-service/internal/store"
	"go.uber.org/zap"
)

// TenantResolver resolves a tenant slug to its uuid (store.Store satisfies
// it; tests substitute a fake). Same contract as the consent handler's.
type TenantResolver interface {
	GetTenantBySlug(ctx context.Context, slug string) (store.Tenant, error)
}

// Authorizer abstracts permission checks (permify.HTTPClient satisfies it;
// tests substitute a fake). Same interface as permify.Authorizer, redeclared
// here so the apps package does not depend on the permify client package.
type Authorizer interface {
	Check(ctx context.Context, tenantID, subject, permission, resource string) (bool, error)
}

// manageAppsPermission is the Permify permission guarding app mutations.
// schema.perm defines manage_catalog = owner or admin; app provisioning is
// catalog management of the tenant's apps, so the existing permission is
// reused (no schema change this wave — same guard twin.go uses for admin
// deletion).
const manageAppsPermission = "manage_catalog"

// Handler bundles the app registry HTTP handlers (SPEC-W18 §1 / Agent A).
type Handler struct {
	Repo      Repository
	Tenants   TenantResolver
	Authz     Authorizer // nil rejects mutations with 503 (misconfiguration)
	Publisher *Publisher // nil-safe: lifecycle events become logged no-ops
	Logger    *zap.Logger
}

// RegisterRoutes adds the app registry routes to the router (called
// additively from httpapi.NewRouter):
//
//	GET    /v1/apps                                  global catalog
//	GET    /v1/tenants/{slug}/apps                   catalog LEFT JOIN tenant_apps
//	POST   /v1/tenants/{slug}/apps/{app_id}          provision+enable (idempotent upsert)
//	PATCH  /v1/tenants/{slug}/apps/{app_id}          partial {status?, config?}
//	DELETE /v1/tenants/{slug}/apps/{app_id}          soft delete -> disabled (row kept)
//	GET    /internal/entitlements/check?app_id=      service-to-service gate
//
// Auth model (existing identity-service idiom, see twin.go): GETs are open
// like the rest of the service's read endpoints (the gateway terminates
// OIDC); mutations require an authenticated subject (JWT sub or X-User-Id
// header) holding owner/admin (Permify manage_catalog) on the organization.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Get("/v1/apps", h.listCatalog)
	r.Route("/v1/tenants/{slug}/apps", func(r chi.Router) {
		r.Get("/", h.listTenantApps)
		r.Post("/{app_id}", h.provision)
		r.Patch("/{app_id}", h.patch)
		r.Delete("/{app_id}", h.disable)
	})
	r.Get("/internal/entitlements/check", h.entitlementCheck)
}

// listCatalog (GET /v1/apps) returns the global platform app catalog.
func (h *Handler) listCatalog(w http.ResponseWriter, r *http.Request) {
	apps, err := h.Repo.ListCatalog(r.Context())
	if err != nil {
		h.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps})
}

// tenant resolves the {slug} path param to a store.Tenant (404/500 written).
func (h *Handler) tenant(w http.ResponseWriter, r *http.Request) (store.Tenant, bool) {
	t, err := h.Tenants.GetTenantBySlug(r.Context(), chi.URLParam(r, "slug"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "tenant not found")
		return t, false
	}
	if err != nil {
		h.internal(w, err)
		return t, false
	}
	return t, true
}

// listTenantApps (GET /v1/tenants/{slug}/apps) returns every catalog app with
// the tenant's provisioning status (not_provisioned + {} config when the app
// was never provisioned).
func (h *Handler) listTenantApps(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenant(w, r)
	if !ok {
		return
	}
	views, err := h.Repo.ListTenantApps(r.Context(), t.ID)
	if err != nil {
		h.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": views})
}

// authorizeMutation guards POST/PATCH/DELETE: caller must be an
// authenticated subject (JWT sub via Authorization bearer, or X-User-Id —
// the twin.go trust model) with owner/admin (Permify manage_catalog) on the
// organization. Returns the actor id for audit/event payloads.
func (h *Handler) authorizeMutation(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) (string, bool) {
	if h.Authz == nil {
		h.Logger.Error("apps authorizer not configured")
		writeError(w, http.StatusServiceUnavailable, "authorization not configured")
		return "", false
	}
	userID := bearerSubject(r.Header.Get("Authorization"))
	if userID == "" {
		userID = r.Header.Get("X-User-Id")
	}
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "authenticated subject required (JWT sub or X-User-Id)")
		return "", false
	}
	org := "organization:" + tenantID.String()
	allowed, err := h.Authz.Check(r.Context(), tenantID.String(), "user:"+userID, manageAppsPermission, org)
	if err != nil {
		h.Logger.Error("permify check failed", zap.Error(err))
		writeError(w, http.StatusBadGateway, "authorization service error")
		return "", false
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "missing permission "+manageAppsPermission+" (owner/admin required)")
		return "", false
	}
	return userID, true
}

// knownApp verifies app_id against the catalog (404 {error} on unknown).
func (h *Handler) knownApp(w http.ResponseWriter, r *http.Request, appID string) bool {
	_, err := h.Repo.GetApp(r.Context(), appID)
	if errors.Is(err, ErrUnknownApp) {
		writeError(w, http.StatusNotFound, "unknown app: "+appID)
		return false
	}
	if err != nil {
		h.internal(w, err)
		return false
	}
	return true
}

// provision (POST /v1/tenants/{slug}/apps/{app_id}) provisions and enables an
// app for the tenant. Idempotent upsert: a replay keeps the original
// provisioned_at/provisioned_by and simply ensures status=enabled.
func (h *Handler) provision(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenant(w, r)
	if !ok {
		return
	}
	appID := chi.URLParam(r, "app_id")
	if !h.knownApp(w, r, appID) {
		return
	}
	actor, ok := h.authorizeMutation(w, r, t.ID)
	if !ok {
		return
	}
	row, prev, created, err := h.Repo.Provision(r.Context(), t.ID, appID, actor)
	if errors.Is(err, ErrUnknownApp) {
		writeError(w, http.StatusNotFound, "unknown app: "+appID)
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	// Lifecycle event: first provision -> AppProvisioned; re-enable from a
	// non-enabled state -> AppStatusChanged; enabled->enabled replay -> none.
	switch {
	case created:
		h.Publisher.Lifecycle(r.Context(), ProvisionedEventType, t.ID, appID, row.Status, actor)
	case prev != StatusEnabled:
		h.Publisher.Lifecycle(r.Context(), StatusChangedEventType, t.ID, appID, row.Status, actor)
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, row)
}

type patchRequest struct {
	Status *AppStatus       `json:"status"`
	Config *json.RawMessage `json:"config"`
}

// patch (PATCH /v1/tenants/{slug}/apps/{app_id}) applies a partial update:
// absent fields keep their stored values; config, when present, replaces the
// whole config document (must be a JSON object).
func (h *Handler) patch(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenant(w, r)
	if !ok {
		return
	}
	appID := chi.URLParam(r, "app_id")
	if !h.knownApp(w, r, appID) {
		return
	}
	var req patchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Status == nil && req.Config == nil {
		writeError(w, http.StatusBadRequest, "patch must include status and/or config")
		return
	}
	if req.Status != nil && !req.Status.Valid() {
		writeError(w, http.StatusBadRequest, "status must be enabled|disabled|suspended")
		return
	}
	var config []byte
	if req.Config != nil {
		var obj map[string]any
		if err := json.Unmarshal(*req.Config, &obj); err != nil {
			writeError(w, http.StatusBadRequest, "config must be a JSON object")
			return
		}
		config = *req.Config
	}
	actor, ok := h.authorizeMutation(w, r, t.ID)
	if !ok {
		return
	}
	before, err := h.Repo.GetTenantApp(r.Context(), t.ID, appID)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "app not provisioned for tenant")
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	row, err := h.Repo.Patch(r.Context(), t.ID, appID, req.Status, config)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "app not provisioned for tenant")
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	if req.Status != nil && *req.Status != before.Status {
		h.Publisher.Lifecycle(r.Context(), StatusChangedEventType, t.ID, appID, row.Status, actor)
	}
	writeJSON(w, http.StatusOK, row)
}

// disable (DELETE /v1/tenants/{slug}/apps/{app_id}) soft-deletes: status ->
// disabled, the row is retained for audit (existing app data is preserved —
// re-provisioning re-enables against the same row).
func (h *Handler) disable(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenant(w, r)
	if !ok {
		return
	}
	appID := chi.URLParam(r, "app_id")
	if !h.knownApp(w, r, appID) {
		return
	}
	actor, ok := h.authorizeMutation(w, r, t.ID)
	if !ok {
		return
	}
	row, err := h.Repo.Disable(r.Context(), t.ID, appID)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "app not provisioned for tenant")
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	h.Publisher.Lifecycle(r.Context(), StatusChangedEventType, t.ID, appID, row.Status, actor)
	writeJSON(w, http.StatusOK, row)
}

// entitlementCheck (GET /internal/entitlements/check?app_id=) is the
// service-to-service entitlement gate consumed by app backends (SPEC-W18 §1,
// §4): 200 {app_id, allowed, reason} where reason mirrors the tenant's
// effective status (enabled|disabled|suspended|not_provisioned); unknown
// app_id -> 404 {error} (callers treat as denied). Tenant comes from the
// X-Tenant-ID / X-Tenant-Slug header; deliberately no auth middleware
// (mesh-internal endpoint, same trust level as /internal/consents/check).
func (h *Handler) entitlementCheck(w http.ResponseWriter, r *http.Request) {
	appID := strings.TrimSpace(r.URL.Query().Get("app_id"))
	if appID == "" {
		writeError(w, http.StatusBadRequest, "app_id query parameter is required")
		return
	}
	ref := r.Header.Get("X-Tenant-ID")
	if ref == "" {
		ref = r.Header.Get("X-Tenant-Slug")
	}
	if ref == "" {
		writeError(w, http.StatusBadRequest, "X-Tenant-ID or X-Tenant-Slug header is required")
		return
	}
	// Unknown app -> 404 {error} (checked before tenant resolution so the
	// caller gets the definitive answer for a bad app_id).
	if _, err := h.Repo.GetApp(r.Context(), appID); errors.Is(err, ErrUnknownApp) {
		writeError(w, http.StatusNotFound, "unknown app: "+appID)
		return
	} else if err != nil {
		h.internal(w, err)
		return
	}
	tenantID, err := h.resolveTenantRef(r.Context(), ref)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "tenant not found")
			return
		}
		h.internal(w, err)
		return
	}
	row, err := h.Repo.GetTenantApp(r.Context(), tenantID, appID)
	if errors.Is(err, ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{
			"app_id":  appID,
			"allowed": false,
			"reason":  ReasonNotProvisioned,
		})
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"app_id":  appID,
		"allowed": row.Status == StatusEnabled,
		"reason":  string(row.Status),
	})
}

// resolveTenantRef accepts a tenant reference as uuid OR slug (X-Tenant-ID
// carries uuids, X-Tenant-Slug slugs — consent handler idiom).
func (h *Handler) resolveTenantRef(ctx context.Context, ref string) (uuid.UUID, error) {
	if id, err := uuid.Parse(strings.TrimSpace(ref)); err == nil {
		return id, nil
	}
	t, err := h.Tenants.GetTenantBySlug(ctx, ref)
	if err != nil {
		return uuid.Nil, err
	}
	return t.ID, nil
}

// bearerSubject extracts the JWT sub claim without verifying the signature —
// the same trust model as httpapi/twin.go's bearerSubject (the gateway
// terminates OIDC; internal callers are trusted by network policy).
func bearerSubject(header string) string {
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(header, "Bearer "), ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Sub
}

func (h *Handler) internal(w http.ResponseWriter, err error) {
	h.Logger.Error("apps internal error", zap.Error(err))
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
