package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/store"
)

type createBookingRequest struct {
	OfferingID     string                   `json:"offering_id"`
	TeamMemberID   string                   `json:"team_member_id"`
	ContactID      string                   `json:"contact_id,omitempty"`
	Contact        *bookingops.ContactInput `json:"contact,omitempty"`
	StartsAt       time.Time                `json:"starts_at"`
	IdempotencyKey string                   `json:"idempotency_key,omitempty"`
	Source         string                   `json:"source,omitempty"`
}

// parseCreateBooking validates and normalizes the create-booking payload
// shared by the tenant API and the public site endpoint.
func parseCreateBooking(req createBookingRequest) (bookingops.CreateInput, error) {
	var in bookingops.CreateInput
	var err error
	if in.OfferingID, err = uuid.Parse(req.OfferingID); err != nil {
		return in, errBadUUID("offering_id")
	}
	if in.TeamMemberID, err = uuid.Parse(req.TeamMemberID); err != nil {
		return in, errBadUUID("team_member_id")
	}
	if req.ContactID != "" {
		id, err := uuid.Parse(req.ContactID)
		if err != nil {
			return in, errBadUUID("contact_id")
		}
		in.ContactID = &id
	}
	in.Contact = req.Contact
	in.StartsAt = req.StartsAt
	in.Source = req.Source
	in.IdempotencyKey = req.IdempotencyKey
	return in, nil
}

type badUUIDError string

func (e badUUIDError) Error() string { return "invalid uuid field: " + string(e) }

func errBadUUID(field string) error { return badUUIDError(field) }

// createBooking handles POST /v1/bookings (manage_bookings).
func (s *server) createBooking(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	var req createBookingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	in, err := parseCreateBooking(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	in.TenantID = tenant.ID
	in.TenantSlug = tenant.Slug
	in.Timezone = tenant.Timezone
	// SPEC-CRM §C3: industry + pack booking policy from the resolved tenant.
	in.Industry = tenant.Industry
	if tenant.Pack != nil {
		policy := tenant.Pack.BookingPolicy
		in.BookingPolicy = &policy
	}
	if in.Source == "" {
		in.Source = "api"
	}
	booking, err := s.d.Ops.Create(r.Context(), in)
	if err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, booking)
}

func (s *server) listBookings(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	q := r.URL.Query()
	var f store.BookingFilter
	f.Status = q.Get("status")
	f.Contact = q.Get("contact")
	// mine=true: restrict to the caller's own team member, resolved by
	// matching the JWT email claim (X-User-Email header fallback, then an
	// email-shaped sub) against team_members.email. No matching member is a
	// 403 — the caller is authenticated but not staff of this tenant.
	if q.Get("mine") == "true" {
		// The tenant middleware already rejected malformed tokens (K-07);
		// on any residual decode error the email stays empty (error-closed).
		claims, _ := parseBearerClaims(r.Header.Get("Authorization"))
		email := claims.Email
		if email == "" {
			email = r.Header.Get("X-User-Email")
		}
		if email == "" {
			if u := userFrom(r.Context()); strings.Contains(u, "@") {
				email = u
			}
		}
		if email == "" {
			writeError(w, http.StatusForbidden, "mine=true requires an email identity (JWT email claim or X-User-Email header)")
			return
		}
		member, err := s.d.Store.GetTeamMemberByEmail(r.Context(), tenant.ID, email)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusForbidden, "no team member in this tenant matches "+email)
			return
		}
		if err != nil {
			s.internal(w, err)
			return
		}
		f.TeamMemberID = &member.ID
	}
	if v := q.Get("team_member_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid team_member_id")
			return
		}
		f.TeamMemberID = &id
	}
	// SPEC-W44 W-B/F15-10: ?limit= (1..500; the store clamps over-large
	// values to 500 rather than resetting to the default).
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit (positive integer, clamped to 500)")
			return
		}
		f.Limit = n
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
	items, err := s.d.Store.ListBookings(r.Context(), tenant.ID, f)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bookings": items})
}

func (s *server) getBooking(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	b, err := s.d.Store.GetBooking(r.Context(), tenant.ID, id)
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

type rescheduleRequest struct {
	StartsAt time.Time `json:"starts_at"`
}

// rescheduleBooking handles POST /v1/bookings/{id}/reschedule.
func (s *server) rescheduleBooking(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req rescheduleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	booking, err := s.d.Ops.Reschedule(r.Context(), tenant.ID, tenant.Slug, tenant.Timezone, id, req.StartsAt)
	if err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, booking)
}

type cancelRequest struct {
	Reason string `json:"reason,omitempty"`
}

// cancelBooking handles POST /v1/bookings/{id}/cancel.
func (s *server) cancelBooking(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req cancelRequest
	// empty body is acceptable
	_ = decodeOptionalJSON(r, &req)
	booking, err := s.d.Ops.Cancel(r.Context(), tenant.ID, tenant.Slug, id, defaultStr(req.Reason, "user_request"))
	if err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, booking)
}
