package httpapi

// Internal endpoints for the reverse CRM sync (Twenty -> OpenDesk,
// SPEC-CRM §B). They are invoked ONLY by crm-sync-service via Dapr service
// invocation, hence no Permify guard — tenant resolution is the usual
// X-Tenant-Slug middleware.
//
// LOOP PREVENTION: none of these handlers writes an outbox event for CRM-originated
// (external_source) writes. The one exception is a receptionist capture (capture_channel set, no
// external_source), which publishes ContactCaptured; crm-sync never sends capture_channel, so a
// reverse-synced change still cannot re-enter the forward flow. Contacts
// have no outbox at all (only booking lifecycle mutations do), and the
// crm-note append deliberately bypasses the outbox, so a reverse-synced
// change can never re-enter the forward OpenDesk -> Twenty event flow.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/opendesk/booking-service/internal/events"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// internalContactUpsertRequest is the payload of POST /internal/contacts/upsert.
type internalContactUpsertRequest struct {
	Name           string `json:"name"`
	Phone          string `json:"phone"`
	Email          string `json:"email"`
	Notes          string `json:"notes"`
	ExternalSource string `json:"external_source"` // e.g. "twenty"
	ExternalID     string `json:"external_id"`     // e.g. the Twenty person id
	// CaptureChannel (2026-11-21) marks details a caller gave the AI receptionist at the start of a
	// call/chat ("voice", "web", "whatsapp", ...). Such details are self-reported, so on an existing
	// contact they only FILL blank fields, and a ContactCaptured event is published so crm-sync adds
	// the person to the CRM without waiting for a booking. Ignored when ExternalSource is set.
	CaptureChannel string `json:"capture_channel"`
}

// EventTypeReceptionistContactCaptured is published on the booking events topic for details captured
// by the AI receptionist (crm-sync turns it into a CRM person).
const EventTypeReceptionistContactCaptured = "com.opendesk.booking.ContactCaptured"

// upsertContactInternal (POST /internal/contacts/upsert) creates or updates a
// contact keyed by phone OR e-mail within the tenant. Contacts arriving with
// external_source set are stamped with source/external_id so later reverse
// events for the same CRM person re-match.
func (s *server) upsertContactInternal(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	var req internalContactUpsertRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Phone == "" && req.Email == "" {
		writeError(w, http.StatusBadRequest, "phone or email is required")
		return
	}
	captured := strings.TrimSpace(req.CaptureChannel) != "" && req.ExternalSource == ""
	if captured {
		if existing, err := s.d.Store.FindContactByPhoneOrEmail(r.Context(), tenant.ID, req.Phone, req.Email); err == nil {
			// self-reported: never overwrite what the business already holds, only fill the gaps
			if existing.Name != "" {
				req.Name = existing.Name
			}
			if existing.Phone != "" {
				req.Phone = existing.Phone
			}
			if existing.Email != "" {
				req.Email = existing.Email
			}
			req.Notes = ""
		} else if !errors.Is(err, store.ErrNotFound) {
			s.internal(w, err)
			return
		}
	}
	c := store.Contact{
		TenantID:   tenant.ID,
		Name:       req.Name,
		Phone:      req.Phone,
		Email:      req.Email,
		Notes:      req.Notes,
		Source:     req.ExternalSource,
		ExternalID: req.ExternalID,
	}
	created, err := s.d.Store.UpsertExternalContact(r.Context(), tenant.ID, &c)
	if err != nil {
		s.internal(w, err)
		return
	}
	if captured {
		s.emitReceptionistContactCaptured(r, tenant.Slug, tenant.ID.String(), c, strings.TrimSpace(req.CaptureChannel), created)
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"contact": c, "created": created})
}

// emitReceptionistContactCaptured queues ContactCaptured on the booking events topic (outbox, same
// sensitivity class as BookingCreated, which also carries the contact's phone/e-mail). Best effort: the
// contact row is already durable; a failed enqueue only delays the CRM until the caller books.
func (s *server) emitReceptionistContactCaptured(r *http.Request, slug, tenantID string, c store.Contact, channel string, created bool) {
	if s.d.Ops == nil || s.d.Ops.EventsTopic == "" {
		return
	}
	payload, err := json.Marshal(events.New("booking-service", EventTypeReceptionistContactCaptured, slug, tenantID, map[string]any{
		"contact_id":   c.ID.String(),
		"contact_name": c.Name,
		"phone":        c.Phone,
		"email":        c.Email,
		"channel":      channel,
		"source":       "ai_receptionist",
		"created":      created,
		"captured_at":  time.Now().UTC().Format(time.RFC3339Nano),
	}))
	if err != nil {
		return
	}
	if err := s.d.Store.EnqueueOutbox(r.Context(), c.ID, s.d.Ops.EventsTopic, payload); err != nil {
		if s.d.Logger != nil {
			s.d.Logger.Warn("receptionist contact-captured enqueue failed", zap.String("contact_id", c.ID.String()), zap.Error(err))
		}
	}
}

// lookupContactInternal (GET /internal/contacts?phone=|email=) is the lookup
// helper for the reverse sync worker: find a contact by phone OR e-mail.
func (s *server) lookupContactInternal(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	q := r.URL.Query()
	phone, email := q.Get("phone"), q.Get("email")
	if phone == "" && email == "" {
		writeError(w, http.StatusBadRequest, "phone or email query param is required")
		return
	}
	c, err := s.d.Store.FindContactByPhoneOrEmail(r.Context(), tenant.ID, phone, email)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "contact not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// addBookingCRMNoteInternal (POST /internal/bookings/{id}/crm-note) appends a
// CRM-originated note (e.g. "Twenty task ... marked DONE") to the booking's
// crm_notes JSONB array. Intentionally emits no outbox event.
func (s *server) addBookingCRMNoteInternal(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Text   string `json:"text"`
		Source string `json:"source"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	if req.Source == "" {
		req.Source = "twenty"
	}
	note := store.CRMNote{At: time.Now().UTC(), Source: req.Source, Text: req.Text}
	if err := s.d.Store.AppendBookingCRMNote(r.Context(), tenant.ID, id, note); err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note})
}
