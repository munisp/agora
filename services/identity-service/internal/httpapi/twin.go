package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/opendesk/identity-service/internal/events"
	"github.com/opendesk/identity-service/internal/store"
	"go.uber.org/zap"
)

// Digital twins (SPEC-W3 §3 innovation 12): a twin is an ephemeral copy of a
// tenant used for safe what-if experiments and demos. Twins are created via
// POST /internal/tenants/{slug}/twin (internauth-gated, K2), onboarded
// exactly like a real tenant (same TenantOnboardingWorkflow → site seed,
// search alias, industry pack), carry plan='twin', is_twin=true and metadata
// {"twin_of": "<source slug>"}, and are deleted after 24h by
// notification-worker's TwinCleanupWorkflow via DELETE
// /internal/tenants/{slug} (SPEC-W44 W-I-2).
//
// Deletion guard (DELETE /v1/tenants/{slug}): the authoritative source is
// tenants.is_twin (SPEC-W44 W-I-3 / S1-F7-06 — the old
// strings.Contains(slug, "-twin-") heuristic let anyone delete any tenant
// whose name happened to contain the marker). Twin tenants may always be
// deleted (the cleanup workflow authenticates via the internal token);
// any other tenant requires the caller to hold the manage_catalog
// permission on the organization (Permify check on the JWT sub / X-User-Id).

// twinSlugMarker identifies digital-twin tenants.
const twinSlugMarker = "-twin-"

// twinRandAlphabet for the 6-char random suffix (DNS-safe, unambiguous).
const twinRandAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// createTwin handles POST /internal/tenants/{slug}/twin.
func (s *server) createTwin(w http.ResponseWriter, r *http.Request) {
	src, err := s.tenant(w, r)
	if err != nil {
		return
	}
	slug, err := newTwinSlug(src.Slug)
	if err != nil {
		s.internal(w, err)
		return
	}
	metadata, _ := json.Marshal(map[string]string{"twin_of": src.Slug})
	t := store.Tenant{
		Slug:        slug,
		Name:        src.Name + " (twin)",
		Timezone:    src.Timezone,
		Currency:    src.Currency,
		Locale:      src.Locale,
		Terminology: src.Terminology,
		Plan:        "twin",
		Industry:    src.Industry, // industry copied from the source tenant
		Metadata:    metadata,
		IsTwin:      true, // SPEC-W44 W-I-3: exact deletion-guard flag
	}
	if err := s.d.Store.CreateTenant(r.Context(), &t); err != nil {
		s.internal(w, err)
		return
	}

	// Onboard exactly like createTenant (durable workflow seeds site, search
	// alias, pack) and arm the 24h cleanup — both fire-and-forget.
	go s.triggerOnboarding(t)
	go s.triggerTwinCleanup(t, src.Slug)

	s.d.Logger.Info("digital twin created",
		zap.String("slug", t.Slug), zap.String("twin_of", src.Slug))
	writeJSON(w, http.StatusCreated, t)
}

// newTwinSlug builds "{slug}-twin-{6rand}", truncating the base so the
// result always satisfies slugRe (≤63 chars).
func newTwinSlug(base string) (string, error) {
	const suffix = 12 // len("-twin-") + 6 random chars
	if len(base)+suffix > 63 {
		base = strings.TrimRight(base[:63-suffix], "-")
	}
	rnd := make([]byte, 6)
	if _, err := rand.Read(rnd); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString(twinSlugMarker)
	for _, c := range rnd {
		b.WriteByte(twinRandAlphabet[int(c)%len(twinRandAlphabet)])
	}
	return b.String(), nil
}

// triggerTwinCleanup asks notification-worker to start TwinCleanupWorkflow
// (24h timer → DELETE /internal/tenants/{slug} with the internal token,
// SPEC-W44 W-I-2/W-N-4) via Dapr service invocation.
func (s *server) triggerTwinCleanup(t store.Tenant, twinOf string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// SPEC-W44 K2: notification-worker gates /dev/* behind X-Internal-Token
	// == NOTIFICATION_INTERNAL_TOKEN (see notificationInternalHeaders).
	err := s.d.Dapr.InvokeServiceWithHeaders(ctx, s.d.NotificationAppID, "dev/trigger-twin-cleanup", map[string]any{
		"tenant_id": t.ID.String(),
		"slug":      t.Slug,
		"twin_of":   twinOf,
	}, notificationInternalHeaders(), nil)
	if err != nil {
		s.d.Logger.Error("failed to trigger TwinCleanupWorkflow",
			zap.String("slug", t.Slug), zap.Error(err))
		return
	}
	s.d.Logger.Info("TwinCleanupWorkflow triggered", zap.String("slug", t.Slug))
}

// deleteTenant handles DELETE /v1/tenants/{slug} with the is_twin/permify
// guard documented above.
func (s *server) deleteTenant(w http.ResponseWriter, r *http.Request) {
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
	actor := "platform-internal"
	if !t.IsTwin {
		// Non-twin deletion requires manage_catalog on the organization.
		c, err := resolveCaller(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "malformed bearer token")
			return
		}
		if c.Subject == "" {
			writeError(w, http.StatusUnauthorized, "authenticated subject required (JWT sub or X-User-Id)")
			return
		}
		allowed, err := s.d.Permify.Check(r.Context(), t.ID.String(),
			"user:"+c.Subject, "manage_catalog", "organization:"+t.ID.String())
		if err != nil {
			s.d.Logger.Error("permify check failed", zap.Error(err))
			writeError(w, http.StatusBadGateway, "authorization service error")
			return
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "missing permission manage_catalog (only twin tenants delete freely)")
			return
		}
		actor = c.Subject
	}
	s.deleteTenantStore(w, r, t, actor)
}

// deleteTenantInternal handles DELETE /internal/tenants/{slug} (SPEC-W44
// W-I-2) — the TwinCleanupWorkflow contract consumed by
// notification-worker (W-N-4). Guarded exclusively by internauth (K2,
// X-Internal-Token == IDENTITY_INTERNAL_TOKEN); NO Permify/owner check —
// the caller is a platform-internal actor. Responses:
//
//	200 {"deleted": "<slug>"}          deleted (warnings[] when a cascade
//	                                   step failed — best-effort, surfaced)
//	401 {"error": ...}                 missing/wrong X-Internal-Token
//	404 {"error": "tenant not found"}  unknown slug
//	503 {"error": ...}                 IDENTITY_INTERNAL_TOKEN unset (fail-closed)
func (s *server) deleteTenantInternal(w http.ResponseWriter, r *http.Request) {
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
	// The cleanup workflow is a platform actor; a presented subject (admin
	// tooling calling the internal surface) is credited when available.
	actor := "platform-internal"
	if c, err := resolveCaller(r); err == nil && c.Subject != "" {
		actor = c.Subject
	}
	s.deleteTenantStore(w, r, t, actor)
}

// deleteTenantStore performs the actual deletion shared by the /v1 (guarded)
// and /internal (internauth) delete paths, plus the SPEC-W45 K9 cascade:
//
//  1. identity rows (tenant + memberships; tenant_api_keys via FK cascade),
//  2. TenantDeleted CloudEvent on opendesk.identity.events {tenant_slug,
//     tenant_id, deleted_at, actor} — booking/conversation/graph-sync/crm-sync
//     purge their tenant copies from this event,
//  3. Keycloak group /tenants/{slug} deletion,
//  4. Permify tenant deletion (all relationship tuples with it).
//
// Steps 2–4 are BEST-EFFORT: failures are logged at Error and surfaced in
// the response's warnings array (never silently swallowed, K9); the identity
// deletion itself is the authoritative commit.
func (s *server) deleteTenantStore(w http.ResponseWriter, r *http.Request, t store.Tenant, actor string) {
	slug := t.Slug
	if err := s.d.Store.DeleteTenant(r.Context(), slug); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "tenant not found")
			return
		}
		s.internal(w, err)
		return
	}
	s.d.Logger.Info("tenant deleted", zap.String("slug", slug), zap.String("actor", actor))

	var warnings []string
	// K9 cascade step: TenantDeleted event (consumers purge cross-service
	// tenant data). Published FIRST so downstream purges start even if the
	// IdP/ReBAC cleanup below hiccups.
	evt := events.New("identity-service", "com.opendesk.identity.TenantDeleted", slug, t.ID.String(), map[string]any{
		"tenant_slug": slug,
		"tenant_id":   t.ID.String(),
		"deleted_at":  time.Now().UTC().Format(time.RFC3339),
		"actor":       actor,
	})
	if err := s.d.Dapr.PublishEvent(r.Context(), s.d.PubSub, s.d.Topic, evt); err != nil {
		s.d.Logger.Error("failed to publish TenantDeleted", zap.String("slug", slug), zap.Error(err))
		warnings = append(warnings, "TenantDeleted publish failed: "+err.Error())
	}
	if err := s.d.Keycloak.DeleteTenantGroup(r.Context(), slug); err != nil {
		s.d.Logger.Error("keycloak tenant group deletion failed", zap.String("slug", slug), zap.Error(err))
		warnings = append(warnings, "keycloak group delete failed: "+err.Error())
	}
	if err := s.d.Permify.DeleteTenant(r.Context(), t.ID.String()); err != nil {
		s.d.Logger.Error("permify tenant deletion failed", zap.String("slug", slug), zap.Error(err))
		warnings = append(warnings, "permify tenant delete failed: "+err.Error())
	}

	resp := map[string]any{"deleted": slug}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	writeJSON(w, http.StatusOK, resp)
}
