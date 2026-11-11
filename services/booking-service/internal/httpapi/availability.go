package httpapi

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/availability"
	"github.com/opendesk/booking-service/internal/cache"
	"github.com/opendesk/booking-service/internal/optimize"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// Sentinel errors distinguishing which lookup failed in loadSlots.
var (
	errOfferingNotFound = errors.New("offering not found")
	errMemberNotFound   = errors.New("team member not found")
)

// getAvailability computes open slots:
// GET /v1/availability?offering_id&team_member_id&from&to[&optimize=true]
func (s *server) getAvailability(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	s.computeAvailability(w, r, tenant.ID, tenant.Location())
}

// computeAvailability is shared by the tenant-scoped and public endpoints.
func (s *server) computeAvailability(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, loc *time.Location) {
	q := r.URL.Query()
	offeringID, err := uuid.Parse(q.Get("offering_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "offering_id is required (uuid)")
		return
	}
	teamMemberID, err := uuid.Parse(q.Get("team_member_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "team_member_id is required (uuid)")
		return
	}
	from, err := parseQueryTime(q.Get("from"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "from must be RFC3339")
		return
	}
	to, err := parseQueryTime(q.Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "to must be RFC3339")
		return
	}
	if !to.After(from) {
		writeError(w, http.StatusBadRequest, "to must be after from")
		return
	}
	if to.Sub(from) > 62*24*time.Hour {
		writeError(w, http.StatusBadRequest, "range too large (max 62 days)")
		return
	}
	wantOptimize := q.Get("optimize") == "true"

	now := time.Now()
	if from.Before(now) {
		from = now
	}

	// Read-through availability cache (SPEC-W3 §3): one key per UTC day
	// bucket. A full hit answers without touching Postgres; any miss falls
	// through to the engine and re-populates every bucket of the range.
	// optimize=true needs the day's bookings for scoring, so it always goes
	// through the engine (still refreshing the cache).
	if s.d.Cache.Enabled() && !wantOptimize {
		if slots, ok := s.cachedSlots(r, tenantID, offeringID, teamMemberID, from, to); ok {
			writeJSON(w, http.StatusOK, map[string]any{
				"offering_id":    offeringID,
				"team_member_id": teamMemberID,
				"slots":          slots,
			})
			return
		}
	}

	data, err := s.loadSlots(r, tenantID, loc, offeringID, teamMemberID, from, to)
	if err != nil {
		// Degraded mode (Wave 5 #5): the store (or an unexpected failure)
		// must not take availability down — serve the last-known-good slot
		// list for up to the stale TTL and flag it to the caller.
		if s.d.Cache.Enabled() {
			if slots, ok := s.staleSlots(r, tenantID, offeringID, teamMemberID, from, to); ok {
				s.d.Logger.Warn("availability compute failed; serving stale cache",
					zap.String("tenant_id", tenantID.String()), zap.Error(err))
				w.Header().Set("X-Cache", "stale")
				writeJSON(w, http.StatusOK, map[string]any{
					"offering_id":    offeringID,
					"team_member_id": teamMemberID,
					"slots":          slots,
				})
				return
			}
		}
		s.mapLoadError(w, err)
		return
	}

	if s.d.Cache.Enabled() {
		s.populateCache(r, tenantID, offeringID, teamMemberID, from, to, data.slots)
	}

	resp := map[string]any{
		"offering_id":    offeringID,
		"team_member_id": teamMemberID,
	}
	if wantOptimize {
		// Wave 5 #4: annotate every slot with its fragmentation score. The
		// list stays chronological; use GET /v1/availability/optimize for a
		// ranked shortlist.
		scored := optimize.Score(data.bookings, data.slots, optimizeParams(data.offering))
		sort.Slice(scored, func(i, j int) bool { return scored[i].StartsAt.Before(scored[j].StartsAt) })
		if scored == nil {
			scored = []optimize.ScoredSlot{}
		}
		resp["slots"] = scored
		resp["optimized"] = true
	} else {
		resp["slots"] = data.slots
	}
	writeJSON(w, http.StatusOK, resp)
}

// getOptimizedAvailability ranks a single day's slots by calendar
// fragmentation (Wave 5 #4):
// GET /v1/availability/optimize?offering_id&team_member_id&date=YYYY-MM-DD
// → top 3 suggestions {starts_at, ends_at, score, reason}.
func (s *server) getOptimizedAvailability(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	q := r.URL.Query()
	offeringID, err := uuid.Parse(q.Get("offering_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "offering_id is required (uuid)")
		return
	}
	teamMemberID, err := uuid.Parse(q.Get("team_member_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "team_member_id is required (uuid)")
		return
	}
	if q.Get("date") == "" {
		writeError(w, http.StatusBadRequest, "date is required (YYYY-MM-DD)")
		return
	}
	loc := tenant.Location()
	day, err := time.ParseInLocation("2006-01-02", q.Get("date"), loc)
	if err != nil {
		writeError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return
	}
	from, to := day, day.AddDate(0, 0, 1)
	if now := time.Now(); from.Before(now) {
		from = now
	}

	data, err := s.loadSlots(r, tenant.ID, loc, offeringID, teamMemberID, from, to)
	if err != nil {
		if s.d.Cache.Enabled() {
			if slots, ok := s.staleSlots(r, tenant.ID, offeringID, teamMemberID, from, to); ok {
				s.d.Logger.Warn("availability compute failed; scoring stale cache",
					zap.String("tenant_id", tenant.ID.String()), zap.Error(err))
				w.Header().Set("X-Cache", "stale")
				// The offering row is unavailable in the degraded path;
				// derive the appointment duration from the cached slots
				// themselves (buffer unknown → 0).
				p := optimize.Params{}
				if len(slots) > 0 {
					p.Duration = slots[0].EndsAt.Sub(slots[0].StartsAt)
				}
				suggestions := optimize.Top(nil, slots, p, 3)
				if suggestions == nil {
					suggestions = []optimize.ScoredSlot{}
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"offering_id":    offeringID,
					"team_member_id": teamMemberID,
					"date":           q.Get("date"),
					"suggestions":    suggestions,
				})
				return
			}
		}
		s.mapLoadError(w, err)
		return
	}

	suggestions := optimize.Top(data.bookings, data.slots, optimizeParams(data.offering), 3)
	if suggestions == nil {
		suggestions = []optimize.ScoredSlot{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"offering_id":    offeringID,
		"team_member_id": teamMemberID,
		"date":           q.Get("date"),
		"suggestions":    suggestions,
	})
}

// optimizeParams derives scoring parameters from the offering.
func optimizeParams(o store.Offering) optimize.Params {
	return optimize.Params{
		Duration: time.Duration(o.DurationMin) * time.Minute,
		Buffer:   time.Duration(o.BufferMin) * time.Minute,
	}
}

// slotData is the result of one availability computation plus the scoring
// inputs (the day's bookings).
type slotData struct {
	offering store.Offering
	slots    []availability.Slot
	bookings []availability.Booking
}

// loadSlots fetches offering/member/rules/bookings and runs the pure
// availability engine. The booking window is widened by duration+buffer on
// both sides so the optimizer sees neighbours just outside the requested
// range. An inactive member yields empty slots without error.
func (s *server) loadSlots(r *http.Request, tenantID uuid.UUID, loc *time.Location, offeringID, memberID uuid.UUID, from, to time.Time) (slotData, error) {
	offering, err := s.d.Store.GetOffering(r.Context(), tenantID, offeringID)
	if errors.Is(err, store.ErrNotFound) {
		return slotData{}, errOfferingNotFound
	}
	if err != nil {
		return slotData{}, err
	}
	member, err := s.d.Store.GetTeamMember(r.Context(), tenantID, memberID)
	if errors.Is(err, store.ErrNotFound) {
		return slotData{}, errMemberNotFound
	}
	if err != nil {
		return slotData{}, err
	}
	data := slotData{offering: offering, slots: []availability.Slot{}, bookings: []availability.Booking{}}
	if !member.Active {
		return data, nil
	}

	rules, err := s.d.Store.ListAvailabilityRules(r.Context(), tenantID, memberID)
	if err != nil {
		return slotData{}, err
	}
	pad := time.Duration(offering.BufferMin+offering.DurationMin) * time.Minute
	bookings, err := s.d.Store.ListBookingsForRange(r.Context(), tenantID, memberID, from.Add(-pad), to.Add(pad))
	if err != nil {
		return slotData{}, err
	}

	engineRules := make([]availability.Rule, 0, len(rules))
	for _, rl := range rules {
		engineRules = append(engineRules, availability.Rule{
			Weekday:       time.Weekday(rl.Weekday),
			StartMin:      rl.StartMin,
			EndMin:        rl.EndMin,
			EffectiveFrom: rl.EffectiveFrom,
			EffectiveTo:   rl.EffectiveTo,
		})
	}
	data.bookings = make([]availability.Booking, 0, len(bookings))
	for _, b := range bookings {
		data.bookings = append(data.bookings, availability.Booking{StartsAt: b.StartsAt, EndsAt: b.EndsAt})
	}

	slots := availability.Slots(availability.Params{
		From:     from,
		To:       to,
		Duration: time.Duration(offering.DurationMin) * time.Minute,
		Buffer:   time.Duration(offering.BufferMin) * time.Minute,
		Capacity: offering.Capacity,
		Rules:    engineRules,
		Bookings: data.bookings,
		Location: loc,
	})
	if slots != nil {
		data.slots = slots
	}
	return data, nil
}

// publicDaySlot is one aggregate availability slot of the public
// {offering_id, date} variant (SPEC-W47 W47-A item 3, A-UIMOB UIMOB-01):
// the time window plus the team_member_id of one member free then (the
// booking create path requires it) and how many members can still take the
// slot.
type publicDaySlot struct {
	StartsAt          time.Time `json:"starts_at"`
	EndsAt            time.Time `json:"ends_at"`
	TeamMemberID      uuid.UUID `json:"team_member_id"`
	RemainingCapacity int       `json:"remaining_capacity"`
}

// publicAvailabilityMaxMembers bounds the fan-out across team members so a
// pathological tenant cannot turn one anonymous request into unlimited work.
const publicAvailabilityMaxMembers = 100

// computePublicDayAvailability aggregates one tenant-local day's slots
// across ALL active team members for the offering:
// GET /public/sites/{slug}/availability?offering_id=<uuid>&date=YYYY-MM-DD
// → {"offering_id", "date", "slots": [{starts_at, ends_at, team_member_id,
// remaining_capacity}]}. Slots are merged by (starts_at, ends_at); the slot
// carries the first free member's id (deterministic: members sorted by
// name) and the aggregate remaining capacity. The "items" key mirrors
// "slots" for the pre-W47 public client (which read data.items); W47-F
// consumes "slots".
func (s *server) computePublicDayAvailability(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, loc *time.Location) {
	q := r.URL.Query()
	offeringID, err := uuid.Parse(q.Get("offering_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "offering_id is required (uuid)")
		return
	}
	day, err := time.ParseInLocation("2006-01-02", q.Get("date"), loc)
	if err != nil {
		writeError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return
	}
	from, to := day, day.AddDate(0, 0, 1)
	if day.After(time.Now().Add(62 * 24 * time.Hour)) {
		writeError(w, http.StatusBadRequest, "date too far in the future (max 62 days)")
		return
	}
	if now := time.Now(); from.Before(now) {
		from = now
	}

	offering, err := s.d.Store.GetOffering(r.Context(), tenantID, offeringID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "offering not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	// A non-bookable offering is invisible on the public site (the public
	// offerings list filters it) — availability answers 404 alike.
	if !offering.Bookable {
		writeError(w, http.StatusNotFound, "offering not found")
		return
	}

	slots := []publicDaySlot{}
	if to.After(from) {
		members, err := s.d.Store.ListTeamMembers(r.Context(), tenantID)
		if err != nil {
			s.internal(w, err)
			return
		}
		type slotKey struct{ start, end time.Time }
		merged := map[slotKey]*publicDaySlot{}
		var order []slotKey
		counted := 0
		for _, m := range members {
			if !m.Active {
				continue
			}
			counted++
			if counted > publicAvailabilityMaxMembers {
				s.d.Logger.Warn("public availability: member fan-out capped",
					zap.String("tenant_id", tenantID.String()), zap.Int("cap", publicAvailabilityMaxMembers))
				break
			}
			data, err := s.loadSlots(r, tenantID, loc, offeringID, m.ID, from, to)
			if err != nil {
				s.mapLoadError(w, err)
				return
			}
			capPer := offering.Capacity
			if capPer <= 0 {
				capPer = 1
			}
			for _, sl := range data.slots {
				k := slotKey{sl.StartsAt, sl.EndsAt}
				if e, ok := merged[k]; ok {
					e.RemainingCapacity += capPer
					continue
				}
				merged[k] = &publicDaySlot{
					StartsAt:          sl.StartsAt,
					EndsAt:            sl.EndsAt,
					TeamMemberID:      m.ID,
					RemainingCapacity: capPer,
				}
				order = append(order, k)
			}
		}
		sort.Slice(order, func(i, j int) bool { return order[i].start.Before(order[j].start) })
		for _, k := range order {
			slots = append(slots, *merged[k])
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"offering_id": offeringID,
		"date":        q.Get("date"),
		"slots":       slots,
		// Backward-compat mirror for the pre-W47 public client, which reads
		// data.items (public-booking-client.tsx:64-71); W47-F reads "slots".
		"items": slots,
	})
}

// mapLoadError translates loadSlots errors into HTTP responses.
func (s *server) mapLoadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errOfferingNotFound):
		writeError(w, http.StatusNotFound, errOfferingNotFound.Error())
	case errors.Is(err, errMemberNotFound):
		writeError(w, http.StatusNotFound, errMemberNotFound.Error())
	default:
		s.internal(w, err)
	}
}

// cachedSlots assembles the slot list from per-day cache entries. The
// second return value is false unless EVERY day bucket of [from, to] hits;
// partial ranges always fall back to the engine so no slice is ever served
// half-stale. Slots that have slid into the past since caching are dropped
// (the entry itself lives at most one TTL).
func (s *server) cachedSlots(r *http.Request, tenantID, offeringID, teamMemberID uuid.UUID, from, to time.Time) ([]availability.Slot, bool) {
	buckets := cache.DayBuckets(from, to)
	if len(buckets) == 0 {
		return nil, false
	}
	out := make([]availability.Slot, 0)
	for _, day := range buckets {
		slots, hit := s.d.Cache.GetSlots(r.Context(), cache.Key(tenantID, offeringID, teamMemberID, day))
		if !hit {
			return nil, false
		}
		for _, sl := range slots {
			// Clamp bucket edges back to the requested window.
			if sl.StartsAt.Before(from) || sl.EndsAt.After(to) {
				continue
			}
			out = append(out, sl)
		}
	}
	return out, true
}

// staleSlots assembles the last-known-good slot list from per-day stale
// copies (Wave 5 #5). Like cachedSlots, every day bucket must hit; the
// caller flags the response with X-Cache: stale.
func (s *server) staleSlots(r *http.Request, tenantID, offeringID, teamMemberID uuid.UUID, from, to time.Time) ([]availability.Slot, bool) {
	buckets := cache.DayBuckets(from, to)
	if len(buckets) == 0 {
		return nil, false
	}
	out := make([]availability.Slot, 0)
	for _, day := range buckets {
		slots, hit := s.d.Cache.GetSlotsStale(r.Context(), cache.Key(tenantID, offeringID, teamMemberID, day))
		if !hit {
			return nil, false
		}
		for _, sl := range slots {
			if sl.StartsAt.Before(from) || sl.EndsAt.After(to) {
				continue
			}
			out = append(out, sl)
		}
	}
	return out, true
}

// populateCache fans a computed slot list out into per-day bucket entries.
// Bucketing is by the slot's UTC start day, matching cache.DayBuckets.
func (s *server) populateCache(r *http.Request, tenantID, offeringID, teamMemberID uuid.UUID, from, to time.Time, slots []availability.Slot) {
	buckets := cache.DayBuckets(from, to)
	byDay := make(map[string][]availability.Slot, len(buckets))
	for _, sl := range slots {
		day := sl.StartsAt.UTC().Format("2006-01-02")
		byDay[day] = append(byDay[day], sl)
	}
	for _, day := range buckets {
		daySlots := byDay[day.Format("2006-01-02")]
		if daySlots == nil {
			daySlots = []availability.Slot{}
		}
		s.d.Cache.SetSlots(r.Context(), cache.Key(tenantID, offeringID, teamMemberID, day), daySlots)
	}
	s.d.Logger.Debug("availability cache populated",
		zap.String("tenant_id", tenantID.String()), zap.Int("buckets", len(buckets)))
}

func parseQueryTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, errEmpty
	}
	return time.Parse(time.RFC3339, v)
}

var errEmpty = errors.New("empty time value")

// parseOptionalDate accepts RFC3339 or YYYY-MM-DD, returning nil for "".
func parseOptionalDate(v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return &t, nil
	}
	return nil, errEmpty
}
