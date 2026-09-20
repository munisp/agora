package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/store"
)

// Waitlist backfill (SPEC-W3 §3 innovation 7). The claim endpoint is NOT
// behind manage_bookings on purpose: the claim_token delivered to the
// contact by the WaitlistBackfillWorkflow notification is the capability
// that authorizes the claim (a random UUID, unguessable, single-use via the
// transactional status flip).

type createWaitlistRequest struct {
	OfferingID   string    `json:"offering_id"`
	ContactName  string    `json:"contact_name"`
	ContactPhone string    `json:"contact_phone"`
	WindowStart  time.Time `json:"window_start"`
	WindowEnd    time.Time `json:"window_end"`
}

// createWaitlistEntry handles POST /v1/waitlist (manage_bookings).
func (s *server) createWaitlistEntry(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	var req createWaitlistRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	offeringID, err := uuid.Parse(req.OfferingID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "offering_id is required (uuid)")
		return
	}
	if req.ContactPhone == "" {
		writeError(w, http.StatusUnprocessableEntity, "contact_phone is required (phone-confirmation policy)")
		return
	}
	if req.WindowStart.IsZero() || req.WindowEnd.IsZero() || !req.WindowEnd.After(req.WindowStart) {
		writeError(w, http.StatusBadRequest, "window_start and window_end are required (window_end after window_start)")
		return
	}
	if _, err := s.d.Store.GetOffering(r.Context(), tenant.ID, offeringID); err != nil {
		s.mapOpError(w, err)
		return
	}
	entry := store.WaitlistEntry{
		TenantID:     tenant.ID,
		OfferingID:   offeringID,
		ContactName:  req.ContactName,
		ContactPhone: req.ContactPhone,
		WindowStart:  req.WindowStart,
		WindowEnd:    req.WindowEnd,
	}
	if err := s.d.Store.CreateWaitlistEntry(r.Context(), &entry); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

// listWaitlist handles GET /v1/waitlist?offering_id&status= — also invoked
// by the notification-worker WaitlistBackfillWorkflow via Dapr service
// invocation (X-Tenant-Slug header, no manage_bookings required for reads).
func (s *server) listWaitlist(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	q := r.URL.Query()
	var f store.WaitlistFilter
	f.Status = q.Get("status")
	if v := q.Get("offering_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid offering_id")
			return
		}
		f.OfferingID = &id
	}
	items, err := s.d.Store.ListWaitlist(r.Context(), tenant.ID, f)
	if err != nil {
		s.internal(w, err)
		return
	}
	if items == nil {
		items = []store.WaitlistEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": items})
}

type claimWaitlistRequest struct {
	Token        string    `json:"token"`
	TeamMemberID string    `json:"team_member_id"`
	StartsAt     time.Time `json:"starts_at"`
}

// claimWaitlist handles POST /v1/waitlist/{id}/claim. Transactional in
// bookingops/store: token + window + slot re-check + booking insert +
// status flip happen atomically; any failure → 409 and nothing is written.
func (s *server) claimWaitlist(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req claimWaitlistRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	token, err := uuid.Parse(req.Token)
	if err != nil {
		writeError(w, http.StatusBadRequest, "token is required (uuid)")
		return
	}
	teamMemberID, err := uuid.Parse(req.TeamMemberID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "team_member_id is required (uuid)")
		return
	}
	in := bookingops.ClaimInput{
		TenantID:     tenant.ID,
		TenantSlug:   tenant.Slug,
		Timezone:     tenant.Timezone,
		EntryID:      id,
		Token:        token,
		TeamMemberID: teamMemberID,
		StartsAt:     req.StartsAt,
		Industry:     tenant.Industry,
	}
	if tenant.Pack != nil {
		policy := tenant.Pack.BookingPolicy
		in.BookingPolicy = &policy
	}
	booking, entry, err := s.d.Ops.ClaimWaitlist(r.Context(), in)
	if err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"booking": booking,
		"entry":   entry,
	})
}

// ---------------------------------------------------------------------------
// Public token claim endpoints (SPEC-W45 CODER-M, K13 completion)
// ---------------------------------------------------------------------------
//
// CODER-I's public claim page (/p/{slug}/claim?token=...) talks to TWO
// token-only endpoints registered OUTSIDE the tenant middleware:
//
//	GET  /v1/waitlist/claim-info?token=... — claim preview
//	POST /v1/waitlist/claim {token}        — perform the claim
//
// The unguessable claim_token (random UUID, emailed/SMSed by the
// WaitlistBackfillWorkflow) is the capability: it resolves the entry AND
// the owning tenant server-side, so no X-Tenant-Slug header or JWT is
// required. Both endpoints are rate-limited per token AND per source IP
// like the other public routes (portal request, promo redeem).

const (
	waitlistClaimRateLimit  = 10
	waitlistClaimRateWindow = time.Minute
)

// waitlistClaimEntry is the public projection of a waitlist entry: the
// contact name + requested time window (the model carries no separate
// party-size field — the window IS the party's request). The claim_token
// and the contact's phone are deliberately NOT exposed (PII minimization:
// the token itself is the bearer capability).
type waitlistClaimEntry struct {
	ID          uuid.UUID `json:"id"`
	OfferingID  uuid.UUID `json:"offering_id"`
	ContactName string    `json:"contact_name"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

// waitlistClaimOffering is the public projection of the offering.
type waitlistClaimOffering struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	DurationMin int       `json:"duration_min"`
	PriceCents  int64     `json:"price_cents"`
	Currency    string    `json:"currency"`
}

// waitlistClaimTenant is the public tenant projection (slug + name only).
type waitlistClaimTenant struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

func publicClaimEntry(e store.WaitlistEntry) waitlistClaimEntry {
	return waitlistClaimEntry{
		ID:          e.ID,
		OfferingID:  e.OfferingID,
		ContactName: e.ContactName,
		WindowStart: e.WindowStart,
		WindowEnd:   e.WindowEnd,
		Status:      e.Status,
		CreatedAt:   e.CreatedAt,
	}
}

func publicClaimOffering(o store.Offering) waitlistClaimOffering {
	return waitlistClaimOffering{
		ID:          o.ID,
		Name:        o.Name,
		Description: o.Description,
		DurationMin: o.DurationMin,
		PriceCents:  o.PriceCents,
		Currency:    o.Currency,
	}
}

// resolveClaimTenant maps the entry's tenant_id to the full tenant context
// (slug/name/timezone/pack) WITHOUT a tenant header: the tenant's public
// site row (seeded by the TenantOnboardingWorkflow for every tenant)
// carries tenant_id → tenant_slug, and the slug then resolves through the
// usual identity-service path (cached).
func (s *server) resolveClaimTenant(ctx context.Context, tenantID uuid.UUID) (bookingops.TenantInfo, error) {
	site, err := s.d.Store.GetSiteByTenant(ctx, tenantID)
	if err != nil {
		return bookingops.TenantInfo{}, err
	}
	if s.d.TenantBySlug == nil {
		return bookingops.TenantInfo{}, fmt.Errorf("tenant resolver not configured")
	}
	return s.d.TenantBySlug(ctx, site.TenantSlug)
}

// allowClaimRateLimit applies the public-endpoint abuse guard: per token
// and per source IP, mirroring the promo redeem posture.
func (s *server) allowClaimRateLimit(w http.ResponseWriter, r *http.Request, token string) bool {
	if !s.waitlistLimiter.Allow("wl-claim:"+token, waitlistClaimRateLimit, waitlistClaimRateWindow) ||
		!s.waitlistLimiter.Allow("wl-claim-ip:"+r.RemoteAddr, waitlistClaimRateLimit*6, waitlistClaimRateWindow) {
		writeError(w, http.StatusTooManyRequests, "too many claim attempts — try again later")
		return false
	}
	return true
}

// waitlistClaimInfo handles GET /v1/waitlist/claim-info?token=... (public).
//
//	200 {entry, offering, tenant, claimable} — claimable=false when no open
//	      slot remains inside the entry's window (slot gone);
//	400 token missing/malformed;
//	404 unknown token;
//	410 expired (window passed) or already consumed (entry claimed).
func (s *server) waitlistClaimInfo(w http.ResponseWriter, r *http.Request) {
	tokenStr := r.URL.Query().Get("token")
	token, err := uuid.Parse(tokenStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "token query parameter is required (uuid)")
		return
	}
	if !s.allowClaimRateLimit(w, r, token.String()) {
		return
	}
	entry, err := s.d.Store.GetWaitlistEntryByToken(r.Context(), token)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown claim token")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if entry.Status != store.WaitlistWaiting || time.Now().After(entry.WindowEnd) {
		writeError(w, http.StatusGone, "waitlist claim token expired or consumed")
		return
	}
	tenant, err := s.resolveClaimTenant(r.Context(), entry.TenantID)
	if err != nil {
		s.internal(w, err)
		return
	}
	offering, err := s.d.Store.GetOffering(r.Context(), entry.TenantID, entry.OfferingID)
	if err != nil {
		s.internal(w, err)
		return
	}
	_, _, claimable, err := s.d.Ops.EarliestClaimSlot(r.Context(), entry.TenantID, tenant.Location(), offering, entry)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entry":     publicClaimEntry(entry),
		"offering":  publicClaimOffering(offering),
		"tenant":    waitlistClaimTenant{Slug: tenant.Slug, Name: tenant.Name},
		"claimable": claimable,
	})
}

// claimWaitlistByTokenRequest is the POST /v1/waitlist/claim body.
type claimWaitlistByTokenRequest struct {
	Token string `json:"token"`
}

// claimWaitlistByToken handles POST /v1/waitlist/claim {token} (public).
// The server auto-picks the earliest open slot inside the entry's window
// and runs the regular transactional claim (bookingops.ClaimWaitlistByToken
// → ClaimWaitlist → store.ClaimWaitlistTx). Idempotent on token: a replay
// returns the ORIGINAL booking with replayed=true.
//
//	200 {booking, entry, replayed};
//	400 token missing/malformed;
//	404 unknown token;
//	409 already-claimed replay mismatch or slot lost;
//	410 expired (window passed / entry no longer waiting).
func (s *server) claimWaitlistByToken(w http.ResponseWriter, r *http.Request) {
	var req claimWaitlistByTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	token, err := uuid.Parse(req.Token)
	if err != nil {
		writeError(w, http.StatusBadRequest, "token is required (uuid)")
		return
	}
	if !s.allowClaimRateLimit(w, r, token.String()) {
		return
	}
	// Resolve the tenant context for the claim: the token binds exactly one
	// entry, and the entry's tenant_id drives site → slug → identity
	// resolution (see resolveClaimTenant).
	entry, err := s.d.Store.GetWaitlistEntryByToken(r.Context(), token)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown claim token")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	tenant, err := s.resolveClaimTenant(r.Context(), entry.TenantID)
	if err != nil {
		s.internal(w, err)
		return
	}
	in := bookingops.ClaimByTokenInput{
		TenantID:   entry.TenantID,
		TenantSlug: tenant.Slug,
		Timezone:   tenant.Timezone,
		Token:      token,
		Industry:   tenant.Industry,
	}
	if tenant.Pack != nil {
		policy := tenant.Pack.BookingPolicy
		in.BookingPolicy = &policy
	}
	res, err := s.d.Ops.ClaimWaitlistByToken(r.Context(), in)
	if err != nil {
		s.mapOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"booking":  res.Booking,
		"entry":    res.Entry,
		"replayed": res.Replayed,
	})
}
