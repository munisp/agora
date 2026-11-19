package httpapi

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/leads"
	"github.com/opendesk/booking-service/internal/store"
)

// Leads/CAC API (SPEC-W13 Agent A): lead CRUD + status transitions
// (manage_bookings mutations, view_analytics reads), promo-code CRUD, the
// public rate-limited promo redeem, campaign CRUD + spend entry (§4/§6)
// and the internal spend-sum endpoint consumed by analytics-service via
// Dapr (§5 coordination with Agent B).

// promo redeem rate limit (public endpoint): per code+phone and per IP.
const (
	promoRedeemLimit  = 10
	promoRedeemWindow = time.Minute
)

func (s *server) leadsSvc(w http.ResponseWriter) *leads.Service {
	if s.d.Leads == nil {
		writeError(w, http.StatusServiceUnavailable, "leads unavailable")
		return nil
	}
	return s.d.Leads
}

// mapLeadError converts leads/store sentinel errors to HTTP statuses.
func (s *server) mapLeadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, leads.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, leads.ErrInvalidTransition):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrPromoExhausted):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrCampaignCancelled), errors.Is(err, store.ErrInvalidCampaignStatus):
		writeError(w, http.StatusConflict, err.Error())
	default:
		s.internal(w, err)
	}
}

// ---------------------------------------------------------------------------
// Leads
// ---------------------------------------------------------------------------

// createLeadRequest is the POST /v1/leads body.
type createLeadRequest struct {
	PhoneE164  string         `json:"phone_e164"`
	Channel    string         `json:"channel"` // channel_of_first_touch
	PromoCode  string         `json:"promo_code,omitempty"`
	UTM        map[string]any `json:"utm,omitempty"`
	RefQR      string         `json:"ref,omitempty"` // QR slug
	CampaignID *uuid.UUID     `json:"campaign_id,omitempty"`
	LgaID      *int           `json:"lga_id,omitempty"`
	ConsentID  *uuid.UUID     `json:"consent_id,omitempty"`
}

func (s *server) createLead(w http.ResponseWriter, r *http.Request) {
	svc := s.leadsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	var req createLeadRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	lead, created, err := svc.Create(r.Context(), leads.CreateInput{
		TenantID:   tenant.ID,
		PhoneE164:  req.PhoneE164,
		Channel:    req.Channel,
		PromoCode:  req.PromoCode,
		UTM:        req.UTM,
		RefQR:      req.RefQR,
		CampaignID: req.CampaignID,
		LgaID:      req.LgaID,
		ConsentID:  req.ConsentID,
	}, tenant.Slug)
	if err != nil {
		s.mapLeadError(w, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK // dedupe hit: existing first-touch lead returned
	}
	writeJSON(w, status, map[string]any{"lead": lead, "created": created})
}

// leadQueryRe bounds the SPEC-W83 phone-prefix search filter (leads carry
// no name/email — the phone is the searchable field): digits with at most
// one leading '+', 3-32 chars total ("++234"/"+"/"12" are all rejected).
var leadQueryRe = regexp.MustCompile(`^(\+[0-9]{2,31}|[0-9]{3,32})$`)

// listLeads handles GET /v1/leads?status=&channel=&campaign_id=&from=&to=&q=.
func (s *server) listLeads(w http.ResponseWriter, r *http.Request) {
	svc := s.leadsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	q := r.URL.Query()
	status := q.Get("status")
	if status != "" {
		switch status {
		case leads.StatusNew, leads.StatusContacted, leads.StatusQualified, leads.StatusConverted, leads.StatusLost:
		default:
			writeError(w, http.StatusBadRequest, "invalid status filter")
			return
		}
	}
	channel := q.Get("channel")
	if channel != "" {
		switch channel {
		case leads.ChannelVoice, leads.ChannelWhatsApp, leads.ChannelTelegram, leads.ChannelWeb,
			leads.ChannelSMS, leads.ChannelWebhook, leads.ChannelUSSD, leads.ChannelQR,
			leads.ChannelPromo, leads.ChannelField:
		default:
			writeError(w, http.StatusBadRequest, "invalid channel filter")
			return
		}
	}
	var campaignID *uuid.UUID
	if raw := q.Get("campaign_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid campaign_id filter")
			return
		}
		campaignID = &id
	}
	from, ok := parseTimeBound(w, q.Get("from"))
	if !ok {
		return
	}
	to, ok := parseTimeBound(w, q.Get("to"))
	if !ok {
		return
	}
	// SPEC-W83: optional phone_e164 prefix search; invalid shapes are a
	// deterministic 400 (operator typos surface, like the enum filters).
	leadQ := strings.TrimSpace(q.Get("q"))
	if leadQ != "" && !leadQueryRe.MatchString(leadQ) {
		writeError(w, http.StatusBadRequest, "invalid q filter")
		return
	}
	// SPEC-W90: keyset pagination (see Store.ListLeadsPage). limit follows
	// the crm360 limitParam semantics: non-integer -> 400, <=0 -> default,
	// >500 -> clamped. The next_cursor key is emitted only when another
	// page exists — additive, so pre-W90 clients see no change.
	limit := 0
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	cursor := strings.TrimSpace(q.Get("cursor"))
	rows, next, err := svc.Store.ListLeadsPage(r.Context(), tenant.ID, status, channel, campaignID, from, to, leadQ, cursor, limit)
	if err != nil {
		if errors.Is(err, store.ErrInvalidCursor) {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		s.internal(w, err)
		return
	}
	if rows == nil {
		rows = []store.Lead{}
	}
	resp := map[string]any{"leads": rows}
	if next != "" {
		resp["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) getLead(w http.ResponseWriter, r *http.Request) {
	svc := s.leadsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	lead, err := svc.Store.GetLead(r.Context(), tenant.ID, id)
	if err != nil {
		s.mapLeadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lead": lead})
}

// transitionLeadRequest is the POST /v1/leads/{id}/status body.
type transitionLeadRequest struct {
	Status string `json:"status"` // contacted | qualified | converted | lost
}

func (s *server) transitionLead(w http.ResponseWriter, r *http.Request) {
	svc := s.leadsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req transitionLeadRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	lead, err := svc.Transition(r.Context(), tenant.ID, id, req.Status, tenant.Slug)
	if err != nil {
		s.mapLeadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lead": lead})
}

// ---------------------------------------------------------------------------
// Promo codes
// ---------------------------------------------------------------------------

// createPromoRequest is the POST /v1/promo body (upsert by code).
type createPromoRequest struct {
	Code           string     `json:"code"`
	CampaignID     *uuid.UUID `json:"campaign_id,omitempty"`
	DiscountNGN    *float64   `json:"discount_ngn,omitempty"`
	MaxRedemptions int        `json:"max_redemptions"` // 0 = unlimited
}

func (s *server) createPromoCode(w http.ResponseWriter, r *http.Request) {
	svc := s.leadsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	var req createPromoRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Code = strings.TrimSpace(req.Code)
	if req.Code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}
	if req.MaxRedemptions < 0 {
		writeError(w, http.StatusBadRequest, "max_redemptions must be >= 0")
		return
	}
	p := store.PromoCode{
		TenantID:       tenant.ID,
		Code:           req.Code,
		CampaignID:     req.CampaignID,
		DiscountNGN:    req.DiscountNGN,
		MaxRedemptions: req.MaxRedemptions,
	}
	if err := svc.Store.UpsertPromoCode(r.Context(), &p); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"promo_code": p})
}

// listPromoCodes handles GET /v1/promo (view_analytics) — dashboard read
// for the CAC admin pages.
func (s *server) listPromoCodes(w http.ResponseWriter, r *http.Request) {
	svc := s.leadsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	rows, err := svc.Store.ListPromoCodes(r.Context(), tenant.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if rows == nil {
		rows = []store.PromoCode{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"promo_codes": rows})
}

// redeemPromoRequest is the POST /v1/promo/redeem body (public).
type redeemPromoRequest struct {
	Code  string `json:"code"`
	Phone string `json:"phone"`
}

// redeemPromo is the PUBLIC, rate-limited, code+phone-idempotent redemption
// endpoint (contract §6). No tenant middleware: the promo code itself
// resolves the owning tenant server-side (unguessable codes), mirroring the
// public site-slug resolution pattern.
func (s *server) redeemPromo(w http.ResponseWriter, r *http.Request) {
	svc := s.leadsSvc(w)
	if svc == nil {
		return
	}
	var req redeemPromoRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Code = strings.TrimSpace(req.Code)
	req.Phone = strings.TrimSpace(req.Phone)
	if req.Code == "" || req.Phone == "" {
		writeError(w, http.StatusBadRequest, "code and phone are required")
		return
	}
	// Rate limit per code+phone and per source IP (abuse guard for the
	// unauthenticated path).
	if !s.promoLimiter.Allow("redeem:"+req.Code+":"+req.Phone, promoRedeemLimit, promoRedeemWindow) ||
		!s.promoLimiter.Allow("redeem-ip:"+r.RemoteAddr, promoRedeemLimit*6, promoRedeemWindow) {
		writeError(w, http.StatusTooManyRequests, "too many redemption attempts — try again later")
		return
	}
	// SPEC-W80 B6: resolve the REAL tenant slug for the funnel events
	// RedeemPromo emits — the public path has no tenant header, so map the
	// code's owning tenant through its site row (resolveClaimTenant idiom).
	// Best-effort: a resolution failure degrades to "" exactly like before,
	// never to a failed redemption.
	tenantSlug := ""
	if promo, err := s.d.Store.LookupPromoCode(r.Context(), req.Code); err == nil {
		if tenant, terr := s.resolveClaimTenant(r.Context(), promo.TenantID); terr == nil {
			tenantSlug = tenant.Slug
		}
	}
	lead, created, err := svc.RedeemPromo(r.Context(), req.Code, req.Phone, tenantSlug)
	if errors.Is(err, store.ErrNotFound) {
		// Unknown code: 404 without revealing which tenant (codes unguessable).
		writeError(w, http.StatusNotFound, "unknown promo code")
		return
	}
	if err != nil {
		s.mapLeadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lead": lead, "lead_created": created})
}

// ---------------------------------------------------------------------------
// Campaigns + spend
// ---------------------------------------------------------------------------

// createCampaignRequest is the POST /v1/campaigns body.
type createCampaignRequest struct {
	Name    string     `json:"name"`
	Channel string     `json:"channel"`
	StartTs *time.Time `json:"start_ts,omitempty"`
	EndTs   *time.Time `json:"end_ts,omitempty"`
}

func (s *server) createCampaign(w http.ResponseWriter, r *http.Request) {
	svc := s.leadsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	var req createCampaignRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	c := store.Campaign{
		TenantID: tenant.ID,
		Name:     strings.TrimSpace(req.Name),
		Channel:  strings.ToLower(strings.TrimSpace(req.Channel)),
		StartsAt: req.StartTs,
		EndsAt:   req.EndTs,
	}
	if err := svc.CreateCampaign(r.Context(), &c); err != nil {
		s.mapLeadError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"campaign": c})
}

// listCampaigns handles GET /v1/campaigns (view_analytics) — campaigns with
// lifetime spend sums for the CAC dashboard.
func (s *server) listCampaigns(w http.ResponseWriter, r *http.Request) {
	svc := s.leadsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	rows, err := svc.Store.ListCampaignsWithSpend(r.Context(), tenant.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if rows == nil {
		rows = []store.CampaignView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"campaigns": rows})
}

// updateCampaignRequest is the PATCH /v1/campaigns/{id} body (SPEC-W47
// W47-A item 7, A-DB DB-07). Pointer fields: only present fields change.
// status accepts active|paused|cancelled (cancelled is terminal; use the
// POST /{id}/cancel endpoint for the dedicated soft-delete path).
type updateCampaignRequest struct {
	Name    *string    `json:"name,omitempty"`
	Channel *string    `json:"channel,omitempty"`
	StartTs *time.Time `json:"start_ts,omitempty"`
	EndTs   *time.Time `json:"end_ts,omitempty"`
	Status  *string    `json:"status,omitempty"`
}

// updateCampaign handles PATCH /v1/campaigns/{id} (manage_bookings).
func (s *server) updateCampaign(w http.ResponseWriter, r *http.Request) {
	svc := s.leadsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req updateCampaignRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	u := store.CampaignUpdate{StartsAt: req.StartTs, EndsAt: req.EndTs}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "name must not be empty")
			return
		}
		u.Name = &name
	}
	if req.Channel != nil {
		ch := strings.ToLower(strings.TrimSpace(*req.Channel))
		u.Channel = &ch
	}
	if req.Status != nil {
		u.Status = strings.TrimSpace(*req.Status)
	}
	c, err := svc.Store.UpdateCampaign(r.Context(), tenant.ID, id, u)
	if err != nil {
		s.mapLeadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"campaign": c})
}

// cancelCampaign handles POST /v1/campaigns/{id}/cancel (manage_bookings):
// terminal soft-delete (SPEC-W47 item 7). Idempotent — an already-cancelled
// campaign answers 200 with the unchanged row.
func (s *server) cancelCampaign(w http.ResponseWriter, r *http.Request) {
	svc := s.leadsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	c, err := svc.Store.CancelCampaign(r.Context(), tenant.ID, id)
	if err != nil {
		s.mapLeadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"campaign": c})
}

// recordSpendRequest is the POST /v1/campaigns/{id}/spend body (§4).
type recordSpendRequest struct {
	AmountNGN float64 `json:"amount_ngn"`
	Channel   string  `json:"channel"`
	Day       string  `json:"day"` // YYYY-MM-DD (UTC)
}

func (s *server) recordCampaignSpend(w http.ResponseWriter, r *http.Request) {
	svc := s.leadsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req recordSpendRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	day, err := time.Parse("2006-01-02", req.Day)
	if err != nil {
		writeError(w, http.StatusBadRequest, "day must be YYYY-MM-DD")
		return
	}
	sp, err := svc.RecordSpend(r.Context(), tenant.ID, id, req.Channel, req.AmountNGN, day)
	if err != nil {
		s.mapLeadError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"spend": sp})
}

// campaignSpendSum handles GET /internal/campaigns/{id}/spend-sum?from&to —
// the Dapr-invoked internal read analytics-service (Agent B) joins spend
// with (tenant via the usual X-Tenant-Slug middleware; from/to are
// RFC3339 or YYYY-MM-DD day bounds, inclusive).
func (s *server) campaignSpendSum(w http.ResponseWriter, r *http.Request) {
	svc := s.leadsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	from, ok := parseTimeBound(w, r.URL.Query().Get("from"))
	if !ok {
		return
	}
	to, ok := parseTimeBound(w, r.URL.Query().Get("to"))
	if !ok {
		return
	}
	total, byChannel, err := svc.Store.CampaignSpendSum(r.Context(), tenant.ID, id, from, to)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"campaign_id": id,
		"from":        from,
		"to":          to,
		"spend_ngn":   total,
		"by_channel":  byChannel,
	})
}
