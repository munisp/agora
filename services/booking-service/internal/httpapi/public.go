package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/opendesk/booking-service/internal/bookingops"
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
//
// SPEC-W47 (W47-A item 3, A-UIMOB UIMOB-01): two request shapes are
// accepted —
//   - offering_id + team_member_id + from/to (RFC3339): the ORIGINAL
//     per-member range computation (unchanged behavior, cache-backed);
//   - offering_id + date (YYYY-MM-DD, tenant-local): the aggregate public
//     variant the booking page actually sends — slots are computed across
//     ALL active team members and merged by start time, each slot carrying
//     the team_member_id of a member free at that time so the subsequent
//     POST /bookings satisfies the required-field validation.
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
	if r.URL.Query().Get("team_member_id") == "" && r.URL.Query().Get("date") != "" {
		s.computePublicDayAvailability(w, r, site.TenantID, tenant.Location())
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

// ---------------------------------------------------------------------------
// Public pay leg (SPEC-W48 A3)
// ---------------------------------------------------------------------------

const (
	publicPayRateLimit  = 10
	publicPayRateWindow = time.Minute
)

// publicPayRequest is the POST /public/sites/{slug}/bookings/{id}/pay body.
type publicPayRequest struct {
	Email       string `json:"email"`
	Phone       string `json:"phone,omitempty"` // accepted for future rails; Flutterwave needs email only
	RedirectURL string `json:"redirect_url,omitempty"`
}

// publicPayBooking serves POST /public/sites/{slug}/bookings/{id}/pay —
// slug-resolved public, no session: the site resolves the tenant
// server-side, so cross-tenant access is impossible by construction (a
// booking of another tenant is a 404). The booking must be payable (status
// pending|confirmed, offering price > 0); the Flutterwave checkout is
// initialized via payments-service (idempotency key
// "agora-<booking_id>-<unix>") and payment_ref/payment_status='initialized'
// are persisted on the booking. 409 when the booking is already paid.
//
// Rate limiting reuses the existing public-endpoint limiter instance
// (portalLimiter) under a pay-specific key prefix — no new middleware.
func (s *server) publicPayBooking(w http.ResponseWriter, r *http.Request) {
	site, ok := s.resolveSite(w, r)
	if !ok {
		return
	}
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if !s.portalLimiter.Allow("pay-ip:"+r.RemoteAddr, publicPayRateLimit*6, publicPayRateWindow) ||
		!s.portalLimiter.Allow("pay-booking:"+id.String(), publicPayRateLimit, publicPayRateWindow) {
		writeError(w, http.StatusTooManyRequests, "too many payment attempts — try again later")
		return
	}
	var req publicPayRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" {
		writeError(w, http.StatusBadRequest, "email is required (flutterwave customer)")
		return
	}
	// SPEC-W49 B2: redirect_url is open-redirect-grade caller input — it is
	// allow-listed (PUBLIC_PAY_REDIRECT_ALLOW_HOSTS, fail-closed when unset).
	req.RedirectURL = strings.TrimSpace(req.RedirectURL)
	if err := bookingops.ValidatePayRedirectURL(req.RedirectURL, s.d.PayRedirectAllowHosts); err != nil {
		writeError(w, http.StatusBadRequest, bookingops.ErrRedirectNotAllowed.Error())
		return
	}
	booking, err := s.d.Store.GetBooking(r.Context(), site.TenantID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if booking.Status != store.StatusPending && booking.Status != store.StatusConfirmed {
		writeError(w, http.StatusConflict, "booking is not payable (status "+booking.Status+")")
		return
	}
	// Honest pre-flight 409; InitializeBookingPaymentTx re-checks under a
	// row lock after the rail answers (the authoritative double-pay guard).
	if status, _, err := s.d.Store.GetBookingPayment(r.Context(), site.TenantID, id); err == nil &&
		status == store.PaymentStatusPaid {
		writeError(w, http.StatusConflict, "booking already paid")
		return
	}
	offering, err := s.d.Store.GetOffering(r.Context(), site.TenantID, booking.OfferingID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if offering.PriceCents <= 0 {
		writeError(w, http.StatusConflict, "booking has no payable amount")
		return
	}
	tenant, err := s.d.Resolver.BySlug(r.Context(), site.TenantSlug)
	if err != nil {
		s.internal(w, err)
		return
	}
	if s.d.Ops == nil || s.d.Ops.Payments == nil {
		s.mapOpError(w, bookingops.ErrPaymentsNotConfigured)
		return
	}
	key := "agora-" + booking.ID.String() + "-" + strconv.FormatInt(time.Now().Unix(), 10)
	res, err := s.d.Ops.Payments.InitializeFlutterwave(r.Context(), site.TenantID.String(), bookingops.FlutterwaveInitializeRequest{
		AmountCents:    offering.PriceCents,
		Currency:       tenant.Currency,
		Email:          req.Email,
		BookingID:      booking.ID.String(),
		IdempotencyKey: key,
		RedirectURL:    req.RedirectURL,
	}, bookingops.CallerIdentity{}) // public leg: no gateway identity — the K2 internal token authenticates the service
	if err != nil {
		s.mapOpError(w, err)
		return
	}
	txRef := res.TxRef
	if txRef == "" {
		txRef = key // honest fallback: the idempotency key identifies the hold
	}
	if err := s.d.Store.InitializeBookingPaymentTx(r.Context(), site.TenantID, booking.ID, txRef, res.Link); err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"link": res.Link, "tx_ref": txRef})
}

// ---------------------------------------------------------------------------
// Public review leg (SPEC-W48 A4)
// ---------------------------------------------------------------------------

const (
	publicReviewRateLimit  = 10
	publicReviewRateWindow = time.Minute
	// publicReviewCommentMax bounds the stored free-text fields.
	publicReviewCommentMax = 4000
	publicReviewNameMax    = 200
)

// publicReviewRequest is the POST /public/sites/{slug}/bookings/{id}/review
// body.
type publicReviewRequest struct {
	Rating       int    `json:"rating"`
	Comment      string `json:"comment,omitempty"`
	ReviewerName string `json:"reviewer_name,omitempty"`
}

// publicCreateReview serves POST /public/sites/{slug}/bookings/{id}/review
// — slug-resolved public, no session (same tenant-isolation-by-construction
// posture as the pay leg: a booking of another tenant is a 404). The
// booking must be status completed; one review per booking (UNIQUE on
// booking_id → 409 on a duplicate).
//
// Rate limiting reuses the existing public-endpoint limiter instance
// (portalLimiter) under a review-specific key prefix — no new middleware.
func (s *server) publicCreateReview(w http.ResponseWriter, r *http.Request) {
	site, ok := s.resolveSite(w, r)
	if !ok {
		return
	}
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if !s.portalLimiter.Allow("review-ip:"+r.RemoteAddr, publicReviewRateLimit*6, publicReviewRateWindow) ||
		!s.portalLimiter.Allow("review-booking:"+id.String(), publicReviewRateLimit, publicReviewRateWindow) {
		writeError(w, http.StatusTooManyRequests, "too many review attempts — try again later")
		return
	}
	var req publicReviewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Rating < 1 || req.Rating > 5 {
		writeError(w, http.StatusBadRequest, "rating must be between 1 and 5")
		return
	}
	req.Comment = strings.TrimSpace(req.Comment)
	req.ReviewerName = strings.TrimSpace(req.ReviewerName)
	if len(req.Comment) > publicReviewCommentMax || len(req.ReviewerName) > publicReviewNameMax {
		writeError(w, http.StatusBadRequest, "comment/reviewer_name too long")
		return
	}
	booking, err := s.d.Store.GetBooking(r.Context(), site.TenantID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if booking.Status != store.StatusCompleted {
		writeError(w, http.StatusConflict, "only completed bookings can be reviewed")
		return
	}
	review := store.BookingReview{
		TenantID:     site.TenantID,
		BookingID:    booking.ID,
		Rating:       req.Rating,
		Comment:      req.Comment,
		ReviewerName: req.ReviewerName,
	}
	if err := s.d.Store.CreateBookingReview(r.Context(), &review); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, review)
}

// publicListReviews serves GET /public/sites/{slug}/reviews?limit=&offset=
// — the public aggregate list over the tenant's completed bookings:
// {reviews: [...], average, count}. limit is capped at 50 (store-enforced).
func (s *server) publicListReviews(w http.ResponseWriter, r *http.Request) {
	site, ok := s.resolveSite(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, err := strconv.Atoi(q.Get("limit"))
	if q.Get("limit") != "" && err != nil {
		writeError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	offset, err := strconv.Atoi(q.Get("offset"))
	if q.Get("offset") != "" && err != nil {
		writeError(w, http.StatusBadRequest, "invalid offset")
		return
	}
	reviews, average, count, err := s.d.Store.ListBookingReviews(r.Context(), site.TenantID, limit, offset)
	if err != nil {
		s.internal(w, err)
		return
	}
	if reviews == nil {
		reviews = []store.BookingReview{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reviews": reviews,
		"average": average,
		"count":   count,
	})
}
