package httpapi

// SPEC-W45 K17 tenant API keys: programmatic credentials a tenant owner/admin
// mints for external integrations. The full key ("<prefix>.<secret>") is
// returned ONCE at creation; only its SHA-256 hash is stored. Services
// validate presented keys via POST /internal/api-keys/validate (K2
// internauth) which returns the tenant binding + scopes. booking-service
// consumes this for the read-only /v1/ext/bookings group (X-Api-Key).

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/identity-service/internal/store"
	"go.uber.org/zap"
)

// apiKeyAlphabet is unambiguous and URL-safe (no 0/O/1/l).
const apiKeyAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// allowedAPIScopes is the v1 scope set (K17: read-only bookings is the first
// REAL scope; further scopes are additive — extend here + the validator's
// consumers together).
var allowedAPIScopes = map[string]bool{"bookings:read": true}

// randChars returns n crypto-rand characters from apiKeyAlphabet.
func randChars(n int) (string, error) {
	buf := make([]byte, n)
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	for i, b := range raw {
		buf[i] = apiKeyAlphabet[int(b)%len(apiKeyAlphabet)]
	}
	return string(buf), nil
}

// generateAPIKey builds (prefix, fullKey, keyHash). The secret segment never
// touches the store; the hash is the only persisted credential material.
func generateAPIKey() (prefix, fullKey, keyHash string, err error) {
	p, err := randChars(8)
	if err != nil {
		return "", "", "", err
	}
	secret, err := randChars(32)
	if err != nil {
		return "", "", "", err
	}
	prefix = "odk_" + p
	fullKey = prefix + "." + secret
	sum := sha256.Sum256([]byte(fullKey))
	return prefix, fullKey, hex.EncodeToString(sum[:]), nil
}

// HashAPIKey maps a presented key to its stored SHA-256 hex (validator path).
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

type createAPIKeyRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

// createAPIKey handles POST /v1/tenants/{slug}/api-keys (owner/admin).
func (s *server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	t, err := s.tenant(w, r)
	if err != nil {
		return
	}
	c, ok := s.requireOrgAccess(w, r, t, "manage_catalog")
	if !ok {
		return
	}
	var req createAPIKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(req.Scopes) == 0 {
		// v1 default: the one REAL end-to-end scope (read-only bookings).
		req.Scopes = []string{"bookings:read"}
	}
	for _, sc := range req.Scopes {
		if !allowedAPIScopes[sc] {
			writeError(w, http.StatusBadRequest, "unknown scope "+sc+" (v1 scopes: bookings:read)")
			return
		}
	}
	prefix, fullKey, keyHash, err := generateAPIKey()
	if err != nil {
		s.internal(w, err)
		return
	}
	k := store.APIKey{
		TenantID:  t.ID,
		Name:      req.Name,
		Prefix:    prefix,
		KeyHash:   keyHash,
		Scopes:    req.Scopes,
		CreatedBy: "user:" + c.Subject,
	}
	if err := s.d.Store.CreateAPIKey(r.Context(), &k); err != nil {
		s.internal(w, err)
		return
	}
	s.d.Logger.Info("tenant api key created",
		zap.String("tenant_slug", t.Slug), zap.String("prefix", prefix),
		zap.String("actor", c.Subject))
	// The plaintext key is returned exactly once — it is not recoverable.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         k.ID,
		"name":       k.Name,
		"prefix":     k.Prefix,
		"key":        fullKey,
		"scopes":     k.Scopes,
		"created_at": k.CreatedAt,
	})
}

// listAPIKeys handles GET /v1/tenants/{slug}/api-keys (owner/admin). Hashes
// never leave the service (store.APIKey.KeyHash is json:"-").
func (s *server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	t, err := s.tenant(w, r)
	if err != nil {
		return
	}
	if _, ok := s.requireOrgAccess(w, r, t, "manage_catalog"); !ok {
		return
	}
	keys, err := s.d.Store.ListAPIKeys(r.Context(), t.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if keys == nil {
		keys = []store.APIKey{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_keys": keys})
}

// revokeAPIKey handles DELETE /v1/tenants/{slug}/api-keys/{key_id}
// (owner/admin). Soft delete: the row stays for audit.
func (s *server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	t, err := s.tenant(w, r)
	if err != nil {
		return
	}
	c, ok := s.requireOrgAccess(w, r, t, "manage_catalog")
	if !ok {
		return
	}
	keyID, err := uuid.Parse(chi.URLParam(r, "key_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "key_id must be a uuid")
		return
	}
	if err := s.d.Store.RevokeAPIKey(r.Context(), t.ID, keyID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "api key not found (or already revoked)")
			return
		}
		s.internal(w, err)
		return
	}
	s.d.Logger.Info("tenant api key revoked",
		zap.String("tenant_slug", t.Slug), zap.String("key_id", keyID.String()),
		zap.String("actor", c.Subject))
	writeJSON(w, http.StatusOK, map[string]string{"revoked": keyID.String()})
}

type validateAPIKeyRequest struct {
	Key string `json:"key"`
}

// validateAPIKey handles POST /internal/api-keys/validate (K2 internauth via
// the /internal prefix). 200 {tenant_slug, tenant_id, scopes, key_id,
// prefix} for an active key; 401 otherwise. Deliberately no tenant scoping
// on the lookup: the caller (booking-service X-Api-Key middleware) learns
// the tenant binding FROM the key.
func (s *server) validateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req validateAPIKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}
	k, slug, err := s.d.Store.GetAPIKeyByHash(r.Context(), HashAPIKey(req.Key))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "invalid api key")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_slug": slug,
		"tenant_id":   k.TenantID.String(),
		"scopes":      k.Scopes,
		"key_id":      k.ID.String(),
		"prefix":      k.Prefix,
	})
}
