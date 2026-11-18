package httpapi

// SPEC-W80 B4: GET /v1/bookings/{id}/crm-notes — read the CRM-originated
// notes appended to a booking (by the reverse-sync internal endpoint POST
// /internal/bookings/{id}/crm-note). The read side deliberately lives in
// THIS file: internal/store/bookings.go and internal/httpapi/bookings.go
// are BYTE-IDENTICAL invariants (W57), so the endpoint reuses the existing
// store seam ListBookingCRMNotes (store/crm.go) instead of extending
// bookingCols. Tenant-scoped under RLS via the store's withTenant; a
// missing or cross-tenant booking is an indistinguishable 404.

import (
	"errors"
	"net/http"

	"github.com/opendesk/booking-service/internal/store"
)

// listBookingCRMNotes handles GET /v1/bookings/{id}/crm-notes (same
// tenant-scoped read posture as getBooking).
func (s *server) listBookingCRMNotes(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	notes, err := s.d.Store.ListBookingCRMNotes(r.Context(), tenant.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "booking not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if notes == nil {
		notes = []store.CRMNote{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": notes})
}
