package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/opendesk/booking-service/internal/store"
)

// resolveSite loads the published site and its tenant context. All public
// handlers are scoped through site.TenantID — tenant-safe by construction.
func (s *server) resolveSite(w http.ResponseWriter, r *http.Request) (store.Site, bool) {
	slug := chi.URLParam(r, "slug")
	site, err := s.d.Store.GetSiteBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "site not found")
		return site, false
	}
	if err != nil {
		s.internal(w, err)
		return site, false
	}
	return site, true
}

// publicTenantContext resolves the tenant display context for the public
// site endpoints. SPEC-W46 (W46-A item 5, P-DATA P-01): this used to be an
// UNCACHED daprd service-invoke per page view (booking→daprd→identity→PG);
// it now rides the TenantResolver — TTL-cached (TENANT_CACHE_TTL_SECONDS,
// default 300s, stale-on-outage) and direct-HTTP when IDENTITY_BASE_URL is
// set (the W45 pattern). The map mirrors identity's response shape
// (id/slug/name/timezone/currency/locale/terminology/plan/industry/pack) so
// the API contract is unchanged; on a hard resolution failure the same
// fail-open fallback as before applies ({"slug": …}).
func (s *server) publicTenantContext(r *http.Request, slug string) map[string]any {
	info, err := s.d.Resolver.BySlug(r.Context(), slug)
	if err != nil {
		s.d.Logger.Warn("tenant context lookup failed for public site")
		return map[string]any{"slug": slug}
	}
	m := map[string]any{
		"id":          info.ID,
		"slug":        info.Slug,
		"name":        info.Name,
		"timezone":    info.Timezone,
		"currency":    info.Currency,
		"locale":      info.Locale,
		"terminology": info.Terminology,
		"plan":        info.Plan,
		"industry":    info.Industry,
	}
	if info.Pack != nil {
		m["pack"] = info.Pack
	}
	return m
}

// publicContext serves GET /public/sites/{slug}/context — everything the
// public booking page needs: site info, tenant display context, bookable
// offerings and active team members.
func (s *server) publicContext(w http.ResponseWriter, r *http.Request) {
	site, ok := s.resolveSite(w, r)
	if !ok {
		return
	}
	// tenant display context (name/timezone/currency/locale/terminology)
	tenantCtx := s.publicTenantContext(r, site.TenantSlug)
	offerings, err := s.d.Store.ListOfferings(r.Context(), site.TenantID)
	if err != nil {
		s.internal(w, err)
		return
	}
	bookable := make([]store.Offering, 0, len(offerings))
	for _, o := range offerings {
		if o.Bookable {
			bookable = append(bookable, o)
		}
	}
	members, err := s.d.Store.ListTeamMembers(r.Context(), site.TenantID)
	if err != nil {
		s.internal(w, err)
		return
	}
	active := make([]store.TeamMember, 0, len(members))
	for _, m := range members {
		if m.Active {
			active = append(active, m)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"site":         site,
		"tenant":       tenantCtx,
		"offerings":    bookable,
		"team_members": active,
	})
}

// publicSite serves GET /public/sites/{slug} — the site metadata + tenant
// display context the booking page / embed widget shell needs. Unpublished
// or unknown slugs 404 exactly like /context (resolveSite → GetSiteBySlug
// only matches published sites).
func (s *server) publicSite(w http.ResponseWriter, r *http.Request) {
	site, ok := s.resolveSite(w, r)
	if !ok {
		return
	}
	tenantCtx := s.publicTenantContext(r, site.TenantSlug)
	writeJSON(w, http.StatusOK, map[string]any{
		"site_slug":     site.Slug,
		"business_name": site.DisplayName,
		"published":     site.Published,
		"theme":         site.Theme,
		"tenant": map[string]any{
			"name":        tenantCtx["name"],
			"timezone":    tenantCtx["timezone"],
			"currency":    tenantCtx["currency"],
			"locale":      tenantCtx["locale"],
			"terminology": tenantCtx["terminology"],
		},
	})
}

// publicOfferings serves GET /public/sites/{slug}/offerings — the bookable
// offerings array for the booking page / embed widget.
func (s *server) publicOfferings(w http.ResponseWriter, r *http.Request) {
	site, ok := s.resolveSite(w, r)
	if !ok {
		return
	}
	offerings, err := s.d.Store.ListOfferings(r.Context(), site.TenantID)
	if err != nil {
		s.internal(w, err)
		return
	}
	bookable := make([]store.Offering, 0, len(offerings))
	for _, o := range offerings {
		if o.Bookable {
			bookable = append(bookable, o)
		}
	}
	writeJSON(w, http.StatusOK, bookable)
}

// publicAvailability serves GET /public/sites/{slug}/availability.
func (s *server) publicAvailability(w http.ResponseWriter, r *http.Request) {
	site, ok := s.resolveSite(w, r)
	if !ok {
		return
	}
	tenant, err := s.d.Resolver.BySlug(r.Context(), site.TenantSlug)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.computeAvailability(w, r, site.TenantID, tenant.Location())
}

// publicCreateBooking serves POST /public/sites/{slug}/bookings. The
// phone-confirmation policy applies here exactly as for agent-driven
// bookings (bookingops enforces it).
func (s *server) publicCreateBooking(w http.ResponseWriter, r *http.Request) {
	site, ok := s.resolveSite(w, r)
	if !ok {
		return
	}
	tenant, err := s.d.Resolver.BySlug(r.Context(), site.TenantSlug)
	if err != nil {
		s.internal(w, err)
		return
	}
	var req createBookingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	in, err := parseCreateBooking(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	in.TenantID = site.TenantID
	in.TenantSlug = site.TenantSlug
	in.Timezone = tenant.Timezone
	// SPEC-CRM §C3: industry + pack booking policy from the resolved tenant.
	in.Industry = tenant.Industry
	if tenant.Pack != nil {
		policy := tenant.Pack.BookingPolicy
		in.BookingPolicy = &policy
	}
	in.Source = "web"
	booking, err := s.d.Ops.Create(r.Context(), in)
	if err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, booking)
}
