package httpapi

import (
	"errors"
	"net/http"

	"github.com/opendesk/booking-service/internal/store"
)

func (s *server) listContacts(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	items, err := s.d.Store.ListContacts(r.Context(), tenant.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"contacts": items})
}

type contactRequest struct {
	Name  string `json:"name"`
	Phone string `json:"phone"`
	Email string `json:"email"`
	Notes string `json:"notes"`
}

func (s *server) createContact(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	var req contactRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	c := store.Contact{
		TenantID: tenant.ID,
		Name:     req.Name,
		Phone:    req.Phone,
		Email:    req.Email,
		Notes:    req.Notes,
	}
	if err := s.d.Store.CreateContact(r.Context(), &c); err != nil {
		// SPEC-W89: ErrConflict (cross-format duplicate phone) -> 409 via
		// mapOpError; everything else keeps the 500 path.
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *server) getContact(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	c, err := s.d.Store.GetContact(r.Context(), tenant.ID, id)
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

func (s *server) updateContact(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req contactRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	c := store.Contact{
		ID:       id,
		TenantID: tenant.ID,
		Name:     req.Name,
		Phone:    req.Phone,
		Email:    req.Email,
		Notes:    req.Notes,
	}
	if err := s.d.Store.UpdateContact(r.Context(), &c); err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *server) deleteContact(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if err := s.d.Store.DeleteContact(r.Context(), tenant.ID, id); err != nil {
		s.mapOpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
