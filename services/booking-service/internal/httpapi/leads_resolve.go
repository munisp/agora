package httpapi

// SPEC-W28 (verification-gate fix): internal bulk lead→phone resolution for
// the notification-worker's graph audience intake.
//
// The tenant knowledge graph (SPEC-W28 §3) stores phones HASHED only — raw
// PII stays in Postgres. When the audience intake materializes a segment,
// graph-service returns members as {person_id, phone_hash, lead_id}; this
// endpoint resolves lead_ids back to their E.164 phones so the existing
// paced send path can address the messages.
//
// POST /v1/leads/resolve
//   body:     {"lead_ids": ["<uuid>", ...]}   (capped at 500)
//   response: {"phones": {"<lead_id>": "<e164 phone>"}}
//
// Invoked ONLY by notification-worker via Dapr service invocation, hence no
// Permify guard — tenant resolution is the usual X-Tenant-Slug middleware
// (same posture as /internal/contacts, /internal/devices, ...). The response
// contains ONLY leads that exist AND belong to the resolved tenant: unknown
// ids, malformed ids and OTHER TENANTS' ids are silently omitted (the
// tenant filter is applied per lookup by the store, so a cross-tenant id is
// indistinguishable from a missing one — RLS belt and braces).
//
// Integrator wiring (2 lines — nothing else needs to change):
//
//	// internal/httpapi/server.go NewRouter, next to the other internal blocks:
//	// Internal lead phone resolution for the notification-worker audience intake (SPEC-W28).
//	r.With(s.tenantMiddleware).Post("/v1/leads/resolve", s.resolveLeadPhones)

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
)

// leadsResolveMaxIDs caps one resolution batch (SPEC-W28 fix contract).
const leadsResolveMaxIDs = 500

// leadPhoneGetter is the store slice the resolver needs (*store.Store
// satisfies it; tests use a fake). GetLead applies the tenant filter
// internally (withTenant + RLS).
type leadPhoneGetter interface {
	GetLead(ctx context.Context, tenantID, id uuid.UUID) (store.Lead, error)
}

// leadsResolveRequest is the POST /v1/leads/resolve body.
type resolveLeadPhonesRequest struct {
	LeadIDs []string `json:"lead_ids"`
}

// resolveLeadPhones serves POST /v1/leads/resolve. Contract violations
// (bad JSON, over-cap batch) are 400s regardless of store availability; a
// nil store degrades a WELL-FORMED request to 503 (the /v1/leads posture).
func (s *server) resolveLeadPhones(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	var req resolveLeadPhonesRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.LeadIDs) > leadsResolveMaxIDs {
		writeError(w, http.StatusBadRequest, "lead_ids is capped at 500 per request")
		return
	}
	if s.d.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "leads unavailable")
		return
	}
	phones, err := resolveLeadPhoneMap(r.Context(), s.d.Store, tenant.ID, req.LeadIDs)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"phones": phones})
}

// resolveLeadPhoneMap looks up each lead id scoped to the tenant and
// returns the {lead_id: phone_e164} map of the FOUND leads. Unknown,
// malformed and cross-tenant ids are omitted (never an error); a store
// error other than not-found aborts the whole batch.
func resolveLeadPhoneMap(ctx context.Context, g leadPhoneGetter, tenantID uuid.UUID, ids []string) (map[string]string, error) {
	phones := make(map[string]string, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, raw := range ids {
		if raw == "" || seen[raw] {
			continue
		}
		seen[raw] = true
		id, err := uuid.Parse(raw)
		if err != nil {
			continue // malformed id: treated as unknown, omitted
		}
		lead, err := g.GetLead(ctx, tenantID, id)
		if errors.Is(err, store.ErrNotFound) {
			continue // unknown or cross-tenant id: omitted by design
		}
		if err != nil {
			return nil, err
		}
		if lead.PhoneE164 != "" {
			phones[id.String()] = lead.PhoneE164
		}
	}
	return phones, nil
}
