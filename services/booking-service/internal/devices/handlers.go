package devices

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"go.uber.org/zap"
)

// Devices HTTP API (SPEC-W16 Agent B, contract §1). Routes are wired in
// httpapi/server.go; the tenant context is injected by httpapi's tenant
// middleware and passed explicitly, so this package stays free of httpapi
// internals (same pattern as geo.Handlers).
//
//   - POST   /v1/devices                       register/upsert (manage_bookings)
//   - DELETE /v1/devices/{token}               unregister      (manage_bookings)
//   - GET    /v1/devices?platform=&app=        list            (view_analytics)
//   - GET    /internal/devices?contact_id=     service-to-service (X-Tenant-Slug
//     middleware only, like /internal/contacts — invoked by notification-worker
//     via Dapr; response shape frozen for Agent A)
type Handlers struct {
	Store *Store
	Log   *zap.Logger
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
	default:
		h.log().Error("devices handler error", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// registerRequest is the POST /v1/devices body.
type registerRequest struct {
	Token     string     `json:"token"`
	Platform  string     `json:"platform"`
	App       string     `json:"app"`
	ContactID *uuid.UUID `json:"contact_id,omitempty"`
}

// Register (POST /v1/devices) upserts the caller's push token. Called by
// the mobile app / PWA right after the push-permission grant and on every
// token refresh (contract §1). 201 on first registration, 200 on refresh.
func (h *Handlers) Register(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	d := DeviceToken{
		TenantID:  tenant.ID,
		ContactID: req.ContactID,
		Token:     req.Token,
		Platform:  req.Platform,
		App:       req.App,
	}
	if err := Validate(&d); err != nil {
		h.mapErr(w, err)
		return
	}
	created, err := h.Store.Upsert(r.Context(), &d)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"device": d, "created": created})
}

// Unregister (DELETE /v1/devices/{token}) removes a token (logout / push
// permission revoked). Web-push tokens are endpoint URLs and may contain
// slashes: clients MUST URL-encode the path segment; a ?token= query
// fallback is accepted for tokens that cannot survive path encoding.
func (h *Handlers) Unregister(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	token := chi.URLParam(r, "token")
	if q := r.URL.Query().Get("token"); q != "" {
		token = q
	}
	if token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	if err := h.Store.Delete(r.Context(), tenant.ID, token); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// List (GET /v1/devices?platform=&app=) enumerates the tenant's registered
// devices (view_analytics — device inventory on the admin dashboard).
func (h *Handlers) List(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	q := r.URL.Query()
	platform := q.Get("platform")
	if platform != "" {
		if err := ValidatePlatform(platform); err != nil {
			writeError(w, http.StatusBadRequest, "invalid platform filter")
			return
		}
	}
	app := q.Get("app")
	if app != "" {
		if err := ValidateApp(app); err != nil {
			writeError(w, http.StatusBadRequest, "invalid app filter")
			return
		}
	}
	devs, err := h.Store.List(r.Context(), tenant.ID, platform, app)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devs})
}

// ListInternal (GET /internal/devices?contact_id=) is the service-to-service
// contract §1 lookup consumed by the notification-worker's
// SendPushNotification activity (Agent A codes TO this shape — do NOT
// change it): a bare JSON array of the contact's device tokens, empty
// array (never null, never 404) when the contact has none.
func (h *Handlers) ListInternal(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	raw := r.URL.Query().Get("contact_id")
	if raw == "" {
		writeError(w, http.StatusBadRequest, "contact_id query param is required")
		return
	}
	contactID, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid contact_id")
		return
	}
	devs, err := h.Store.ListByContact(r.Context(), tenant.ID, contactID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, devs)
}
