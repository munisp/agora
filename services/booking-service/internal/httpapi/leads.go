package httpapi

import (
	"errors"
	"net/http"
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

// listLeads handles GET /v1/leads?status=&channel=&campaign_id=&from=&to=.
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
	rows, err := svc.Store.ListLeads(r.Context(), tenant.ID, status, channel, campaignID, from, to)
	if err != nil {
		s.internal(w, err)
		return
	}
	if rows == nil {
		rows = []store.Lead{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"leads": rows})
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
	lead, created, err := svc.RedeemPromo(r.Context(), req.Code, req.Phone, "")
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
