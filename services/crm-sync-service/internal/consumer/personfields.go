package consumer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/opendesk/crm-sync-service/internal/twentyc"
)

// teamNames caches each booking site's team list (advisor id -> name) for a few minutes.
type teamNames struct {
	mu    sync.Mutex
	sites map[string]teamEntry
	hc    *http.Client
}

type teamEntry struct {
	names map[string]string
	at    time.Time
}

func (t *teamNames) lookup(ctx context.Context, bookingURL, site, memberID string) string {
	if bookingURL == "" || site == "" || memberID == "" {
		return ""
	}
	t.mu.Lock()
	if t.sites == nil {
		t.sites = map[string]teamEntry{}
		t.hc = &http.Client{Timeout: 10 * time.Second}
	}
	e, ok := t.sites[site]
	t.mu.Unlock()
	if !ok || time.Since(e.at) > 5*time.Minute {
		var ctxPayload struct {
			TeamMembers []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"team_members"`
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			strings.TrimRight(bookingURL, "/")+"/public/sites/"+url.PathEscape(site)+"/context", nil)
		resp, err := t.hc.Do(req)
		if err != nil {
			return e.names[memberID]
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&ctxPayload) != nil {
			return e.names[memberID]
		}
		e = teamEntry{names: map[string]string{}, at: time.Now()}
		for _, m := range ctxPayload.TeamMembers {
			e.names[m.ID] = m.Name
		}
		t.mu.Lock()
		t.sites[site] = e
		t.mu.Unlock()
	}
	return e.names[memberID]
}

// fillPersonFields (2026-11-23) writes what Agora knows about a contact into the business's own CRM fields
// (TWENTY_ROUTES person_fields). Best effort: a failure is logged, never retried into the event.
func (s *Syncer) fillPersonFields(ctx context.Context, tenantUUID *uuid.UUID, personID string, facts twentyc.PersonFacts,
	advisorID string) {
	if personID == "" || s.Router == nil {
		return
	}
	f := s.Router.FieldsFor(tenantUUID)
	if f == nil {
		return
	}
	if facts.Advisor == "" && advisorID != "" {
		facts.Advisor = s.teams.lookup(ctx, s.BookingURL, f.Site, advisorID)
	}
	latest, ifEmpty := f.Patches(facts)
	if len(ifEmpty) > 0 {
		current, err := s.tw(tenantUUID).GetPersonFields(ctx, personID)
		if err != nil {
			s.Log.Warn("person fields not read", zap.String("person_id", personID), zap.Error(err))
			ifEmpty = nil
		}
		for k, v := range ifEmpty {
			if cur, ok := current[k]; !ok || cur == nil || cur == "" {
				latest[k] = v
			}
		}
	}
	if len(latest) == 0 {
		return
	}
	if err := s.tw(tenantUUID).PatchPerson(ctx, personID, latest); err != nil {
		s.Log.Warn("person fields not written", zap.String("person_id", personID), zap.Error(err))
		return
	}
	s.Log.Info("person fields filled", zap.String("person_id", personID), zap.Any("fields", latest))
}
