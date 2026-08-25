package httpapi

// Outbound webhook platform REST (Wave 5 #10): per-tenant subscription CRUD
// + delivery history. Tenant scope comes from the X-Tenant-Slug header,
// resolved to a tenant id via identity-service (same contract as
// booking-service's tenantMiddleware); APISIX fronts these routes at
// /api/notifications/* with the standard jwt+rewrite pattern.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/store"
	"go.uber.org/zap"
)

// TenantRef is the resolved tenant context of one request.
type TenantRef struct {
	ID   uuid.UUID
	Slug string
}

// WebhookStore is the persistence slice of the webhook REST handlers
// (*store.Store satisfies it; tests use an in-memory fake).
type WebhookStore interface {
	CreateSubscription(ctx context.Context, sub *store.WebhookSubscription) error
	ListSubscriptions(ctx context.Context, tenantID uuid.UUID) ([]store.WebhookSubscription, error)
	GetSubscription(ctx context.Context, tenantID, id uuid.UUID) (store.WebhookSubscription, error)
	DeleteSubscription(ctx context.Context, tenantID, id uuid.UUID) error
	ListDeliveries(ctx context.Context, tenantID, subID uuid.UUID, limit int) ([]store.WebhookDelivery, error)
}

type ctxKey string

const ctxTenant ctxKey = "tenant"

// tenantMiddleware resolves the requested tenant (X-Tenant-Slug header or
// ?tenant= query — both caller-supplied) BOUND to the gateway-injected
// X-Tenant-Slugs membership list (C1/K1 extended binding, N-02): the slug
// is honored only when the verified JWT's tenant_slugs claim contains it
// (403 otherwise). The OPENDESK_TRUST_DIRECT_TENANT=1 dev escape is the
// only gateway-less path (logged). The bound slug is then resolved to a
// tenant id via the configured resolver.
func (s *Server) tenantMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Webhooks == nil || s.ResolveTenant == nil {
			http.Error(w, `{"error":"webhook platform not configured"}`, http.StatusServiceUnavailable)
			return
		}
		slug := strings.TrimSpace(r.Header.Get("X-Tenant-Slug"))
		if slug == "" {
			slug = strings.TrimSpace(r.URL.Query().Get("tenant"))
		}
		if slug == "" {
			http.Error(w, `{"error":"tenant slug is required (X-Tenant-Slug header or tenant query)"}`, http.StatusBadRequest)
			return
		}
		if !s.bindTenantSlug(r, slug) {
			s.Log.Warn("webhook tenant binding rejected", zap.String("slug", slug))
			http.Error(w, `{"error":"tenant is not bound to the caller"}`, http.StatusForbidden)
			return
		}
		ref, err := s.ResolveTenant(r.Context(), slug)
		if err != nil {
			s.Log.Warn("webhook tenant resolution failed", zap.String("slug", slug), zap.Error(err))
			http.Error(w, `{"error":"tenant not found"}`, http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxTenant, ref)))
	})
}

func tenantFrom(ctx context.Context) TenantRef {
	t, _ := ctx.Value(ctxTenant).(TenantRef)
	return t
}

type createWebhookRequest struct {
	URL    string   `json:"url"`
	Events []string `json:"events"`
	Secret string   `json:"secret,omitempty"`
}

// createWebhook serves POST /v1/webhooks. When no secret is supplied one is
// generated; either way the plaintext secret is returned ONLY in this
// response ("shown once") — list/get responses mask it.
func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	var req createWebhookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" || !(strings.HasPrefix(req.URL, "https://") || strings.HasPrefix(req.URL, "http://")) {
		http.Error(w, `{"error":"url must be an http(s) URL"}`, http.StatusBadRequest)
		return
	}
	// S1-F6-03/N-02 SSRF guard: https-only outside dev, no userinfo, DNS
	// fail-closed, private/loopback/link-local/multicast CIDRs blocked.
	validator := s.WebhookURLValidator
	if validator == nil {
		validator = NewURLValidator(false)
	}
	if err := validator.ValidateWebhookURL(r.Context(), req.URL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url rejected: " + err.Error()})
		return
	}
	if len(req.Events) == 0 {
		http.Error(w, `{"error":"at least one event type (or *) is required"}`, http.StatusBadRequest)
		return
	}
	events := make([]string, 0, len(req.Events))
	for _, e := range req.Events {
		if e = strings.TrimSpace(e); e != "" {
			events = append(events, e)
		}
	}
	if len(events) == 0 {
		http.Error(w, `{"error":"at least one event type (or *) is required"}`, http.StatusBadRequest)
		return
	}
	secret := strings.TrimSpace(req.Secret)
	if secret == "" {
		if s.WebhookSigningRequired {
			http.Error(w, `{"error":"secret is required (WEBHOOK_SIGNING_REQUIRED=true)"}`, http.StatusBadRequest)
			return
		}
		secret = generateSecret()
	}
	sub := store.WebhookSubscription{
		TenantID:   tenant.ID,
		TenantSlug: tenant.Slug,
		URL:        req.URL,
		Secret:     secret,
		Events:     events,
	}
	if err := s.Webhooks.CreateSubscription(r.Context(), &sub); err != nil {
		s.Log.Error("create webhook subscription", zap.Error(err))
		http.Error(w, `{"error":"failed to create subscription"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         sub.ID,
		"tenant_id":  sub.TenantID,
		"url":        sub.URL,
		"events":     sub.Events,
		"active":     sub.Active,
		"created_at": sub.CreatedAt,
		"secret":     secret, // shown once — never returned by list/get
	})
}

// listWebhooks serves GET /v1/webhooks (secrets masked).
func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	subs, err := s.Webhooks.ListSubscriptions(r.Context(), tenant.ID)
	if err != nil {
		s.Log.Error("list webhook subscriptions", zap.Error(err))
		http.Error(w, `{"error":"failed to list subscriptions"}`, http.StatusInternalServerError)
		return
	}
	type view struct {
		ID        uuid.UUID `json:"id"`
		URL       string    `json:"url"`
		Events    []string  `json:"events"`
		Active    bool      `json:"active"`
		SecretSet bool      `json:"secret_set"`
		CreatedAt any       `json:"created_at"`
	}
	out := make([]view, 0, len(subs))
	for _, sub := range subs {
		out = append(out, view{
			ID: sub.ID, URL: sub.URL, Events: sub.Events, Active: sub.Active,
			SecretSet: sub.Secret != "", CreatedAt: sub.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out})
}

// deleteWebhook serves DELETE /v1/webhooks/{id}.
func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}
	if err := s.Webhooks.DeleteSubscription(r.Context(), tenant.ID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, `{"error":"subscription not found"}`, http.StatusNotFound)
			return
		}
		s.Log.Error("delete webhook subscription", zap.Error(err))
		http.Error(w, `{"error":"failed to delete subscription"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id.String()})
}

// listWebhookDeliveries serves GET /v1/webhooks/{id}/deliveries.
func (s *Server) listWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}
	// Existence + tenant scoping check (deliveries of another tenant's
	// subscription are a 404, not a leak).
	if _, err := s.Webhooks.GetSubscription(r.Context(), tenant.ID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, `{"error":"subscription not found"}`, http.StatusNotFound)
			return
		}
		s.Log.Error("get webhook subscription", zap.Error(err))
		http.Error(w, `{"error":"failed to load subscription"}`, http.StatusInternalServerError)
		return
	}
	deliveries, err := s.Webhooks.ListDeliveries(r.Context(), tenant.ID, id, 100)
	if err != nil {
		s.Log.Error("list webhook deliveries", zap.Error(err))
		http.Error(w, `{"error":"failed to list deliveries"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": deliveries})
}

// generateSecret returns a random 48-hex-char signing secret.
func generateSecret() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b[:])
}
