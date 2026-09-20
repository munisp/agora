package httpapi

// SPEC-W45 K17 (booking half): the external read-only bookings surface
// behind the APISIX api-ext-booking route (/api/ext/booking/* →
// /v1/ext/bookings/*). AuthN/Z is the apikey middleware ONLY — these routes
// are registered OUTSIDE the JWT/tenant/Permify group; the tenant binding
// comes FROM the validated key (never from headers), and the store's
// withTenant sets the app.tenant_id GUC from it, so a key for tenant A
// cannot read tenant B by construction.

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/opendesk/booking-service/internal/apikey"
	"github.com/opendesk/booking-service/internal/store"
)

// extClaims extracts the key-stamped claims (the middleware guarantees
// their presence; a missing value is a wiring bug → 401, error-closed).
func extClaims(w http.ResponseWriter, r *http.Request) (apikey.Claims, bool) {
	c, ok := apikey.ClaimsFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "api key claims missing")
		return apikey.Claims{}, false
	}
	return c, true
}

// extListBookings handles GET /v1/ext/bookings — tenant-scoped (from the
// key), paginated (limit 1..500 default 100, offset >= 0), filterable by
// status + starts_at range (from/to, RFC3339).
func (s *server) extListBookings(w http.ResponseWriter, r *http.Request) {
	claims, ok := extClaims(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var f store.BookingFilter
	f.Status = q.Get("status")
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit (positive integer, clamped to 500)")
			return
		}
		f.Limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid offset (non-negative integer)")
			return
		}
		f.Offset = n
	}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid from (RFC3339)")
			return
		}
		f.From = &t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid to (RFC3339)")
			return
		}
		f.To = &t
	}
	items, err := s.d.Store.ListBookings(r.Context(), claims.TenantID, f)
	if err != nil {
		s.internal(w, err)
		return
	}
	if items == nil {
		items = []store.Booking{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bookings": items,
		"limit":    f.Limit,
		"offset":   f.Offset,
	})
}

// extGetBooking handles GET /v1/ext/bookings/{id} — 404 both when the
// booking does not exist AND when it belongs to another tenant (the store
// query is tenant-scoped, so cross-tenant reads are indistinguishable from
// missing rows — no existence oracle).
func (s *server) extGetBooking(w http.ResponseWriter, r *http.Request) {
	claims, ok := extClaims(w, r)
	if !ok {
		return
	}
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	b, err := s.d.Store.GetBooking(r.Context(), claims.TenantID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "booking not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}
