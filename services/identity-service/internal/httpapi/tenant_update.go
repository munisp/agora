package httpapi

// SPEC-W47 UIWEB-01: PATCH /v1/tenants/{slug} — partial tenant update.
//
// The admin-web settings screens issue exactly three PATCH shapes against
// this endpoint (all previously 405 — the route did not exist):
//
//	settings/settings-client.tsx:65    {name, timezone, currency, locale}
//	settings/packs/packs-client.tsx:187 {industry}
//	voice-agent/voice-agent-client.tsx:79 {terminology}
//
// Semantics: absent fields are untouched; "terminology" REPLACES the whole
// document (the voice-agent client always sends the full edited map, so a
// removed row must disappear). Plan/slug/is_twin are NOT patchable here —
// plan changes stay on PATCH /v1/tenants/{slug}/plan (owner/platform-admin).
//
// Authorization: manage_catalog (owner|admin) on the organization — the same
// gate as the other tenant mutations (invite/remove member). The change is
// audit-logged (Info, actor + fields) and published as TenantUpdated on
// opendesk.identity.events.

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/opendesk/identity-service/internal/events"
	"github.com/opendesk/identity-service/internal/store"
	"go.uber.org/zap"
)

type updateTenantRequest struct {
	Name        *string          `json:"name"`
	Timezone    *string          `json:"timezone"`
	Currency    *string          `json:"currency"`
	Locale      *string          `json:"locale"`
	Terminology *json.RawMessage `json:"terminology"`
	Industry    *string          `json:"industry"`
}

var (
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	localeRe   = regexp.MustCompile(`^[a-zA-Z]{2,3}(-[a-zA-Z0-9]{2,8})*$`)
	timezoneRe = regexp.MustCompile(`^[A-Za-z0-9_+\-/]{1,64}$`)
)

// updateTenant handles PATCH /v1/tenants/{slug} (UIWEB-01).
func (s *server) updateTenant(w http.ResponseWriter, r *http.Request) {
	t, err := s.tenant(w, r)
	if err != nil {
		return
	}
	c, ok := s.requireOrgAccess(w, r, t, "manage_catalog")
	if !ok {
		return
	}
	var req updateTenantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	var patch store.TenantPatch
	var changed []string
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len(name) > 120 {
			writeError(w, http.StatusBadRequest, "name must be 1-120 characters")
			return
		}
		patch.Name = &name
		changed = append(changed, "name")
	}
	if req.Timezone != nil {
		tz := strings.TrimSpace(*req.Timezone)
		if !timezoneRe.MatchString(tz) {
			writeError(w, http.StatusBadRequest, "timezone must be an IANA-style id (e.g. Africa/Lagos, UTC)")
			return
		}
		patch.Timezone = &tz
		changed = append(changed, "timezone")
	}
	if req.Currency != nil {
		cur := strings.ToUpper(strings.TrimSpace(*req.Currency))
		if !currencyRe.MatchString(cur) {
			writeError(w, http.StatusBadRequest, "currency must be a 3-letter ISO code (e.g. USD, NGN)")
			return
		}
		patch.Currency = &cur
		changed = append(changed, "currency")
	}
	if req.Locale != nil {
		loc := strings.TrimSpace(*req.Locale)
		if !localeRe.MatchString(loc) {
			writeError(w, http.StatusBadRequest, "locale must look like en-US")
			return
		}
		patch.Locale = &loc
		changed = append(changed, "locale")
	}
	if req.Industry != nil {
		ind := strings.TrimSpace(*req.Industry)
		// SPEC-CRM §C1: industry must reference a loaded pack id (same gate
		// as createTenant; an empty registry accepts everything so the
		// service keeps working outside compose).
		if s.d.Packs != nil && !s.d.Packs.Has(ind) {
			writeError(w, http.StatusBadRequest,
				"unknown industry "+strconv.Quote(ind)+" (loaded packs: "+strings.Join(s.d.Packs.IDs(), ", ")+")")
			return
		}
		patch.Industry = &ind
		changed = append(changed, "industry")
	}
	if req.Terminology != nil {
		// Replace semantics: the client sends the full map. Values must all
		// be strings (terminologyOverrides reads only strings).
		var m map[string]string
		if err := json.Unmarshal(*req.Terminology, &m); err != nil || m == nil {
			writeError(w, http.StatusBadRequest, "terminology must be a JSON object of string values")
			return
		}
		raw, err := json.Marshal(m)
		if err != nil {
			s.internal(w, err)
			return
		}
		patch.Terminology = raw
		changed = append(changed, "terminology")
	}
	if len(changed) == 0 {
		writeError(w, http.StatusBadRequest,
			"no updatable fields in patch (allowed: name, timezone, currency, locale, terminology, industry)")
		return
	}

	updated, err := s.d.Store.UpdateTenant(r.Context(), t.Slug, patch)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}

	// Audit trail: settings/pack changes are tenant-level config events —
	// always logged with actor + changed fields, independent of event-bus
	// delivery (same posture as the K18 plan change).
	s.d.Logger.Info("AUDIT tenant updated",
		zap.String("tenant_slug", t.Slug),
		zap.String("tenant_id", t.ID.String()),
		zap.String("actor", c.Subject),
		zap.Strings("fields", changed))

	data := map[string]any{
		"tenant_slug": t.Slug,
		"tenant_id":   t.ID.String(),
		"actor":       c.Subject,
		"fields":      changed,
		"changed_at":  time.Now().UTC().Format(time.RFC3339),
	}
	if patch.Industry != nil {
		data["old_industry"] = t.Industry
		data["new_industry"] = *patch.Industry
	}
	evt := events.New("identity-service", "com.opendesk.identity.TenantUpdated", t.Slug, t.ID.String(), data)
	if err := s.d.Dapr.PublishEvent(r.Context(), s.d.PubSub, s.d.Topic, evt); err != nil {
		s.d.Logger.Error("failed to publish TenantUpdated", zap.Error(err))
	}

	writeJSON(w, http.StatusOK, s.tenantResponse(updated))
}
