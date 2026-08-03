// Package leads implements SPEC-W13 Agent A: the CAC Lead domain model
// (contract §1), 24h dedupe key, first-touch attribution precedence (§3),
// the status machine (new→contacted→qualified→converted|lost) and the
// FunnelEvent CloudEvent builder for Kafka topic cac.events (§2).
package leads

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
)

// EventTypeFunnel is the CloudEvents type on topic cac.events (contract §2).
const EventTypeFunnel = "com.opendesk.cac.FunnelEvent"

// Channel values of the Lead (contract §1 enum).
const (
	ChannelVoice    = "voice"
	ChannelWhatsApp = "whatsapp"
	ChannelTelegram = "telegram"
	ChannelWeb      = "web"
	ChannelSMS      = "sms"
	ChannelWebhook  = "webhook"
	ChannelUSSD     = "ussd"
	ChannelQR       = "qr"
	ChannelPromo    = "promo"
	ChannelField    = "field"
)

var validChannels = map[string]bool{
	ChannelVoice: true, ChannelWhatsApp: true, ChannelTelegram: true,
	ChannelWeb: true, ChannelSMS: true, ChannelWebhook: true,
	ChannelUSSD: true, ChannelQR: true, ChannelPromo: true, ChannelField: true,
}

// Lead row statuses (contract §1 enum).
const (
	StatusNew       = "new"
	StatusContacted = "contacted"
	StatusQualified = "qualified"
	StatusConverted = "converted"
	StatusLost      = "lost"
)

// Funnel event names (contract §2 enum; the leads service emits the lead
// subset: lead_created / contacted / qualified / converted / lost).
const (
	EventLeadCreated = "lead_created"
	EventContacted   = "contacted"
	EventOptedIn     = "opted_in"
	EventQualified   = "qualified"
	EventConverted   = "converted"
	EventFirstTxn    = "first_txn"
	EventLost        = "lost"
)

// EntityTypeLead is the funnel entity_type for lead events (contract §2).
const EntityTypeLead = "lead"

// ErrInvalidInput marks deterministic validation failures.
var ErrInvalidInput = errors.New("invalid lead input")

// ErrInvalidTransition marks an illegal status transition.
var ErrInvalidTransition = errors.New("invalid lead status transition")

// transitions is the lead status machine (SPEC-W13 Agent A):
// new→contacted→qualified→converted|lost. converted/lost are terminal.
var transitions = map[string][]string{
	StatusNew:       {StatusContacted},
	StatusContacted: {StatusQualified},
	StatusQualified: {StatusConverted, StatusLost},
}

// funnelEventName maps a transition target status to its funnel event name.
var funnelEventName = map[string]string{
	StatusContacted: EventContacted,
	StatusQualified: EventQualified,
	StatusConverted: EventConverted,
	StatusLost:      EventLost,
}

// CanTransition reports whether from→to is a legal status transition.
func CanTransition(from, to string) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// EventNameFor returns the funnel event name emitted when a lead enters
// status `to` ("" when the target has no funnel event).
func EventNameFor(to string) string { return funnelEventName[to] }

// DedupeKey computes the 24h dedup key (contract §1 / CAC FR-009):
// sha256(tenant_id|lower(phone)|channel|YYYY-MM-DD) — the date is the
// lead's creation day in UTC.
func DedupeKey(tenantID uuid.UUID, phone, channel string, day time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s",
		tenantID.String(), strings.ToLower(strings.TrimSpace(phone)), channel,
		day.UTC().Format("2006-01-02"))))
	return hex.EncodeToString(sum[:])
}

// AttributionInput carries the raw attribution hints of a create/redeem
// call. Precedence (contract §3): explicit promo_code > UTM
// (utm_source/medium/campaign) > QR slug > channel_of_first_touch.
type AttributionInput struct {
	Channel    string         // channel_of_first_touch as observed
	PromoCode  string         // explicit promo code (highest precedence)
	UTM        map[string]any // utm_source / utm_medium / utm_campaign ...
	RefQR      string         // QR slug (?ref= / landing slug)
	CampaignID *uuid.UUID     // explicit campaign link (kept as given)
}

// ResolveAttribution applies the §3 precedence and returns the effective
// channel + normalized promo/utm fields to persist. First-touch only: the
// result is written once at lead creation and never updated afterwards.
func ResolveAttribution(in AttributionInput) (channel string, promoCode *string, utm map[string]any, err error) {
	channel = strings.ToLower(strings.TrimSpace(in.Channel))
	if channel == "" {
		channel = ChannelWeb
	}
	utm = in.UTM
	// 1. Explicit promo code wins over everything.
	if p := strings.TrimSpace(in.PromoCode); p != "" {
		pc := p
		return ChannelPromo, &pc, utm, nil
	}
	// 2. UTM attribution: channel stays as observed (utm carried alongside).
	if hasUTM(utm) {
		if !validChannels[channel] {
			return "", nil, nil, fmt.Errorf("%w: channel %q", ErrInvalidInput, channel)
		}
		return channel, nil, utm, nil
	}
	// 3. QR slug: channel qr + synthesized utm triple (mirrors the Agent E
	//    QR landing redirect utm_source=qr&utm_medium=offline&utm_campaign=slug).
	if slug := strings.TrimSpace(in.RefQR); slug != "" {
		utm = map[string]any{
			"utm_source":   "qr",
			"utm_medium":   "offline",
			"utm_campaign": slug,
		}
		return ChannelQR, nil, utm, nil
	}
	// 4. Bare channel of first touch.
	if !validChannels[channel] {
		return "", nil, nil, fmt.Errorf("%w: channel %q", ErrInvalidInput, channel)
	}
	return channel, nil, utm, nil
}

// hasUTM reports whether the utm map carries at least one of the canonical
// utm_source / utm_medium / utm_campaign keys.
func hasUTM(utm map[string]any) bool {
	for _, k := range []string{"utm_source", "utm_medium", "utm_campaign"} {
		if v, ok := utm[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return true
			}
		}
	}
	return false
}

// Validate checks the minimal field set required for persistence.
func Validate(l *store.Lead) error {
	if l.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if strings.TrimSpace(l.PhoneE164) == "" {
		return fmt.Errorf("%w: phone_e164 is required", ErrInvalidInput)
	}
	if !validChannels[l.ChannelOfFirstTouch] {
		return fmt.Errorf("%w: channel_of_first_touch %q", ErrInvalidInput, l.ChannelOfFirstTouch)
	}
	if l.DedupeKey == "" {
		return fmt.Errorf("%w: dedupe_key is required", ErrInvalidInput)
	}
	return nil
}

// FunnelEvent is the contract §2 data payload on cac.events.
type FunnelEvent struct {
	EventID        string     `json:"event_id"`
	TenantID       string     `json:"tenant_id"`
	EntityType     string     `json:"entity_type"` // lead | customer | agent
	EntityID       string     `json:"entity_id"`
	EventName      string     `json:"event_name"`
	EventTS        time.Time  `json:"event_ts"`
	Channel        string     `json:"channel"`
	CampaignID     *uuid.UUID `json:"campaign_id"`
	LgaID          *int       `json:"lga_id"`
	AmountNGN      *float64   `json:"amount_ngn"` // null for leads (filled by txn events)
	IdempotencyKey string     `json:"idempotency_key"`
}

// NewFunnelEvent builds the §2 payload for one lead lifecycle event.
// The idempotency key is deterministic (lead × event) so the
// analytics-service consumer dedupes replays.
func NewFunnelEvent(l *store.Lead, eventName string, ts time.Time) FunnelEvent {
	return FunnelEvent{
		EventID:        uuid.NewString(),
		TenantID:       l.TenantID.String(),
		EntityType:     EntityTypeLead,
		EntityID:       l.ID.String(),
		EventName:      eventName,
		EventTS:        ts.UTC(),
		Channel:        l.ChannelOfFirstTouch,
		CampaignID:     l.CampaignID,
		LgaID:          l.LgaID,
		AmountNGN:      nil,
		IdempotencyKey: "lead:" + l.ID.String() + ":" + eventName,
	}
}

// Map converts the FunnelEvent to the CloudEvent data map (events.New).
func (e FunnelEvent) Map() map[string]any {
	var campaign any
	if e.CampaignID != nil {
		campaign = e.CampaignID.String()
	}
	var lga any
	if e.LgaID != nil {
		lga = *e.LgaID
	}
	var amount any
	if e.AmountNGN != nil {
		amount = *e.AmountNGN
	}
	return map[string]any{
		"event_id":        e.EventID,
		"tenant_id":       e.TenantID,
		"entity_type":     e.EntityType,
		"entity_id":       e.EntityID,
		"event_name":      e.EventName,
		"event_ts":        e.EventTS,
		"channel":         e.Channel,
		"campaign_id":     campaign,
		"lga_id":          lga,
		"amount_ngn":      amount,
		"idempotency_key": e.IdempotencyKey,
	}
}
