package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/store"
)

// activityRequest is the common payload of saga activity callbacks.
type activityRequest struct {
	BookingID  string `json:"booking_id"`
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Reason     string `json:"reason,omitempty"`
}

func (s *server) parseActivity(w http.ResponseWriter, r *http.Request) (activityRequest, uuid.UUID, uuid.UUID, bool) {
	var req activityRequest
	if !decodeJSON(w, r, &req) {
		return req, uuid.Nil, uuid.Nil, false
	}
	bookingID, err := uuid.Parse(req.BookingID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid booking_id")
		return req, uuid.Nil, uuid.Nil, false
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid tenant_id")
		return req, uuid.Nil, uuid.Nil, false
	}
	return req, bookingID, tenantID, true
}

// activityReserveSlot (POST /activities/reserve-slot) re-validates that the
// slot is still open for the pending booking. Returning an error makes the
// saga fail and compensate; 200 means the slot is held.
func (s *server) activityReserveSlot(w http.ResponseWriter, r *http.Request) {
	_, bookingID, tenantID, ok := s.parseActivity(w, r)
	if !ok {
		return
	}
	booking, err := s.d.Store.GetBooking(r.Context(), tenantID, bookingID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "booking not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if booking.Status == store.StatusCancelled {
		// SPEC-W43 K-06: a cancelled booking holds NO slot — the saga must
		// see reserved:false (and compensate) instead of an idempotent
		// "reserved" success that would confirm a dead booking.
		writeJSON(w, http.StatusOK, map[string]any{"reserved": false, "status": booking.Status})
		return
	}
	if booking.Status != store.StatusPending {
		// already processed — idempotent success
		writeJSON(w, http.StatusOK, map[string]any{"reserved": true, "status": booking.Status})
		return
	}
	bookings, err := s.d.Store.ListBookingsForRange(r.Context(), tenantID, booking.TeamMemberID,
		booking.StartsAt.Add(-time.Hour), booking.EndsAt.Add(time.Hour))
	if err != nil {
		s.internal(w, err)
		return
	}
	for _, other := range bookings {
		if other.ID == booking.ID {
			continue
		}
		if other.StartsAt.Before(booking.EndsAt) && booking.StartsAt.Before(other.EndsAt) {
			writeError(w, http.StatusConflict, "slot already taken")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"reserved": true})
}

// activityConfirmBooking (POST /activities/confirm-booking) marks the
// booking confirmed and emits BookingConfirmed through the outbox.
func (s *server) activityConfirmBooking(w http.ResponseWriter, r *http.Request) {
	req, bookingID, tenantID, ok := s.parseActivity(w, r)
	if !ok {
		return
	}
	booking, err := s.d.Store.GetBooking(r.Context(), tenantID, bookingID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "booking not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if booking.Status == store.StatusConfirmed {
		writeJSON(w, http.StatusOK, map[string]any{"confirmed": true, "status": booking.Status})
		return
	}
	offering, _ := s.d.Store.GetOffering(r.Context(), tenantID, booking.OfferingID)
	contact, _ := s.d.Store.GetContact(r.Context(), tenantID, booking.ContactID)
	payload, err := bookingops.MarshalBookingEvent("com.opendesk.booking.BookingConfirmed", req.TenantSlug, booking, offering, contact)
	if err != nil {
		s.internal(w, err)
		return
	}
	if err := s.d.Store.SetBookingStatus(r.Context(), tenantID, bookingID, store.StatusConfirmed, s.d.Ops.EventsTopic, payload,
		s.d.Ops.UsageExtra(req.TenantSlug, tenantID, bookingID, offering)...); err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"confirmed": true})
}

// activityMarkNoShow (POST /activities/mark-no-show) flips a booking to
// no_show and emits BookingNoShow (used by NoShowFollowupWorkflow).
func (s *server) activityMarkNoShow(w http.ResponseWriter, r *http.Request) {
	req, bookingID, tenantID, ok := s.parseActivity(w, r)
	if !ok {
		return
	}
	booking, err := s.d.Store.GetBooking(r.Context(), tenantID, bookingID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "booking not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	// SPEC-W43 K-06: terminal states are rejected, never flipped to no_show
	// (a cancelled/completed booking cannot become a no-show; an existing
	// no_show is already terminal). 409 lets the saga fail loudly instead of
	// emitting a false BookingNoShow.
	switch booking.Status {
	case store.StatusCancelled, store.StatusCompleted, store.StatusNoShow:
		writeError(w, http.StatusConflict, "cannot mark no_show from terminal status "+booking.Status)
		return
	}
	offering, _ := s.d.Store.GetOffering(r.Context(), tenantID, booking.OfferingID)
	contact, _ := s.d.Store.GetContact(r.Context(), tenantID, booking.ContactID)
	payload, err := bookingops.MarshalBookingEvent("com.opendesk.booking.BookingNoShow", req.TenantSlug, booking, offering, contact)
	if err != nil {
		s.internal(w, err)
		return
	}
	if err := s.d.Store.SetBookingStatus(r.Context(), tenantID, bookingID, store.StatusNoShow, s.d.Ops.EventsTopic, payload); err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"no_show": true})
}

// activityReleaseSlot (POST /activities/release-slot) is the saga
// compensation: it cancels the pending booking and emits BookingCancelled.
func (s *server) activityReleaseSlot(w http.ResponseWriter, r *http.Request) {
	req, bookingID, tenantID, ok := s.parseActivity(w, r)
	if !ok {
		return
	}
	reason := req.Reason
	if reason == "" {
		reason = "saga_compensation"
	}
	booking, err := s.d.Ops.Cancel(r.Context(), tenantID, req.TenantSlug, bookingID, reason)
	if err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"released": true, "status": booking.Status})
}
