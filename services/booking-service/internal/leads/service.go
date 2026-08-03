package leads

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// Service bundles the CAC lead orchestration: create (deduped, first-touch
// attribution), the status machine and promo redemption — every lifecycle
// step emits the contract §2 FunnelEvent onto cac.events via the
// transactional outbox (same pattern as the incidents usage metering).
type Service struct {
	Store *store.Store
	// CACEventsTopic is the funnel topic (CAC_EVENTS_TOPIC, default
	// cac.events). Empty disables emission.
	CACEventsTopic string
	// FirstTouchOnly mirrors LEAD_ATTRIBUTION_FIRST_TOUCH_ONLY (default
	// true): attribution is written at creation and never overwritten —
	// enforced structurally by the dedupe upsert (contract §3); the flag is
	// the operator-visible kill switch for any future re-attribution flow.
	FirstTouchOnly bool
	Log            *zap.Logger
}

func (s *Service) log() *zap.Logger {
	if s.Log != nil {
		return s.Log
	}
	return zap.NewNop()
}

// CreateInput is one lead-creation request (POST /v1/leads).
type CreateInput struct {
	TenantID   uuid.UUID
	PhoneE164  string
	Channel    string // channel_of_first_touch as observed
	PromoCode  string
	UTM        map[string]any
	RefQR      string
	CampaignID *uuid.UUID
	LgaID      *int
	ConsentID  *uuid.UUID
}

// Create persists a lead with first-touch attribution (§3) and the 24h
// dedupe key (§1). A dedupe hit returns the EXISTING lead unchanged
// (created=false) and emits nothing — the funnel stays exactly-once per
// lead. created=true emits the lead_created FunnelEvent.
func (s *Service) Create(ctx context.Context, in CreateInput, tenantSlug string) (store.Lead, bool, error) {
	channel, promo, utm, err := ResolveAttribution(AttributionInput{
		Channel:   in.Channel,
		PromoCode: in.PromoCode,
		UTM:       in.UTM,
		RefQR:     in.RefQR,
	})
	if err != nil {
		return store.Lead{}, false, err
	}
	lead := store.Lead{
		ID:                  uuid.New(),
		TenantID:            in.TenantID,
		PhoneE164:           strings.TrimSpace(in.PhoneE164),
		ChannelOfFirstTouch: channel,
		CampaignID:          in.CampaignID,
		PromoCode:           promo,
		UTM:                 utm,
		LgaID:               in.LgaID,
		ConsentID:           in.ConsentID,
	}
	lead.DedupeKey = DedupeKey(lead.TenantID, lead.PhoneE164, lead.ChannelOfFirstTouch, time.Now())
	if err := Validate(&lead); err != nil {
		return store.Lead{}, false, err
	}
	created, err := s.Store.InsertLead(ctx, &lead)
	if err != nil {
		return lead, false, err
	}
	if !created {
		return lead, false, nil
	}
	s.emit(ctx, &lead, EventLeadCreated, tenantSlug)
	return lead, true, nil
}

// Transition moves a lead along new→contacted→qualified→converted|lost and
// emits the matching FunnelEvent (contacted / qualified / converted / lost).
// Only the status changes — attribution is never touched (§3).
func (s *Service) Transition(ctx context.Context, tenantID, leadID uuid.UUID, to, tenantSlug string) (store.Lead, error) {
	to = strings.ToLower(strings.TrimSpace(to))
	current, err := s.Store.GetLead(ctx, tenantID, leadID)
	if err != nil {
		return store.Lead{}, err
	}
	if !CanTransition(current.Status, to) {
		return store.Lead{}, fmt.Errorf("%w: %s→%s", ErrInvalidTransition, current.Status, to)
	}
	updated, err := s.Store.UpdateLeadStatus(ctx, tenantID, leadID, to)
	if err != nil {
		return store.Lead{}, err
	}
	if name := EventNameFor(to); name != "" {
		s.emit(ctx, &updated, name, tenantSlug)
	}
	return updated, nil
}

// RedeemPromo redeems a promo code for a phone (contract §6): idempotent
// per code+phone, creates the lead with promo attribution (channel "promo",
// campaign from the code) when this is the phone's first touch today.
// The lead_created FunnelEvent fires only when a NEW lead was actually
// created (leadCreated), never on dedupe hits or redemption replays.
func (s *Service) RedeemPromo(ctx context.Context, code, phone, tenantSlug string) (store.Lead, bool, error) {
	code = strings.TrimSpace(code)
	phone = strings.TrimSpace(phone)
	if code == "" || phone == "" {
		return store.Lead{}, false, fmt.Errorf("%w: code and phone are required", ErrInvalidInput)
	}
	promo, err := s.Store.LookupPromoCode(ctx, code)
	if err != nil {
		return store.Lead{}, false, err
	}
	lead := &store.Lead{
		ID:                  uuid.New(),
		TenantID:            promo.TenantID,
		PhoneE164:           phone,
		ChannelOfFirstTouch: ChannelPromo,
		CampaignID:          promo.CampaignID,
		PromoCode:           &promo.Code,
	}
	lead.DedupeKey = DedupeKey(promo.TenantID, phone, ChannelPromo, time.Now())
	out, leadCreated, _, err := s.Store.RedeemPromoTx(ctx, promo.TenantID, code, phone, lead)
	if err != nil {
		return store.Lead{}, false, err
	}
	if leadCreated {
		s.emit(ctx, &out, EventLeadCreated, tenantSlug)
	}
	return out, leadCreated, nil
}

// CreateCampaign validates + persists a marketing campaign (the entity the
// §4 spend endpoint attaches to).
func (s *Service) CreateCampaign(ctx context.Context, c *store.Campaign) error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("%w: campaign name is required", ErrInvalidInput)
	}
	return s.Store.CreateCampaign(ctx, c)
}

// RecordSpend records one (campaign, day, channel) spend entry (§4).
// SET semantics at the store layer make reposts idempotent.
func (s *Service) RecordSpend(ctx context.Context, tenantID, campaignID uuid.UUID, channel string, amountNGN float64, day time.Time) (store.CampaignSpend, error) {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		return store.CampaignSpend{}, fmt.Errorf("%w: channel is required", ErrInvalidInput)
	}
	if amountNGN < 0 {
		return store.CampaignSpend{}, fmt.Errorf("%w: amount_ngn must be >= 0", ErrInvalidInput)
	}
	if day.IsZero() {
		return store.CampaignSpend{}, fmt.Errorf("%w: day is required (YYYY-MM-DD)", ErrInvalidInput)
	}
	sp := store.CampaignSpend{
		TenantID:   tenantID,
		CampaignID: campaignID,
		Day:        day.UTC(),
		Channel:    channel,
		AmountNGN:  amountNGN,
	}
	if err := s.Store.UpsertCampaignSpend(ctx, &sp); err != nil {
		return store.CampaignSpend{}, err
	}
	return sp, nil
}

// emit publishes one FunnelEvent CloudEvent to cac.events via the outbox.
// Best-effort (same posture as the incidents usage metering): the lead row
// is durable; an enqueue failure is logged loudly for reconciliation.
func (s *Service) emit(ctx context.Context, l *store.Lead, eventName, tenantSlug string) {
	if s.CACEventsTopic == "" {
		return
	}
	evt := NewFunnelEvent(l, eventName, time.Now())
	payload, err := json.Marshal(events.New("booking-service", EventTypeFunnel, tenantSlug,
		l.TenantID.String(), evt.Map()))
	if err != nil {
		s.log().Warn("funnel event marshal failed; skipping emission", zap.Error(err))
		return
	}
	if err := s.Store.EnqueueOutbox(ctx, l.ID, s.CACEventsTopic, payload); err != nil {
		s.log().Error("funnel event enqueue failed; lead durable but cac.events row lost — reconcile",
			zap.String("lead_id", l.ID.String()), zap.String("event_name", eventName), zap.Error(err))
	}
}
