package leads

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
)

// Dedupe key (contract §1): sha256(tenant|lower(phone)|channel|YYYY-MM-DD) —
// stable within a UTC day, rolls over daily, phone case-insensitive.
func TestDedupeKey(t *testing.T) {
	tenantID := uuid.New()
	day := time.Date(2026, 2, 3, 23, 30, 0, 0, time.UTC)

	k1 := DedupeKey(tenantID, "+2348012345678", "web", day)
	k2 := DedupeKey(tenantID, "+2348012345678", "web", day.Add(-time.Hour)) // same UTC day
	if k1 != k2 {
		t.Fatal("same-day keys must match (24h dedup)")
	}
	if len(k1) != 64 {
		t.Fatalf("key = %q, want sha256 hex (64 chars)", k1)
	}
	if k3 := DedupeKey(tenantID, "+2348012345678", "web", day.Add(2*time.Hour)); k3 == k1 {
		t.Fatal("next-day key must differ")
	}
	if k4 := DedupeKey(tenantID, "+2348012345678 ", "web", day); k4 != k1 {
		t.Fatal("phone with trailing space must normalize to the same key")
	}
	if k5 := DedupeKey(tenantID, "+2348012345678", "qr", day); k5 == k1 {
		t.Fatal("different channel must differ")
	}
	if k6 := DedupeKey(uuid.New(), "+2348012345678", "web", day); k6 == k1 {
		t.Fatal("different tenant must differ")
	}
}

// Status machine: new→contacted→qualified→converted|lost only.
func TestCanTransition(t *testing.T) {
	legal := [][2]string{
		{StatusNew, StatusContacted},
		{StatusContacted, StatusQualified},
		{StatusQualified, StatusConverted},
		{StatusQualified, StatusLost},
	}
	for _, tr := range legal {
		if !CanTransition(tr[0], tr[1]) {
			t.Fatalf("%s→%s must be legal", tr[0], tr[1])
		}
		if name := EventNameFor(tr[1]); name == "" {
			t.Fatalf("%s must map to a funnel event", tr[1])
		}
	}
	illegal := [][2]string{
		{StatusNew, StatusQualified},
		{StatusNew, StatusConverted},
		{StatusNew, StatusLost},
		{StatusContacted, StatusConverted},
		{StatusConverted, StatusLost},
		{StatusLost, StatusContacted},
		{StatusNew, StatusNew},
	}
	for _, tr := range illegal {
		if CanTransition(tr[0], tr[1]) {
			t.Fatalf("%s→%s must be illegal", tr[0], tr[1])
		}
	}
}

// Attribution precedence (contract §3): promo > utm > qr slug > channel.
func TestResolveAttributionPrecedence(t *testing.T) {
	utm := map[string]any{"utm_source": "google", "utm_medium": "cpc"}

	// promo beats utm + qr + channel.
	ch, promo, _, err := ResolveAttribution(AttributionInput{
		Channel: "web", PromoCode: "WELCOME50", UTM: utm, RefQR: "lagos-stand",
	})
	if err != nil || ch != ChannelPromo || promo == nil || *promo != "WELCOME50" {
		t.Fatalf("promo precedence: ch=%q promo=%v err=%v", ch, promo, err)
	}

	// utm beats qr + channel (channel preserved as observed).
	ch, promo, gotUTM, err := ResolveAttribution(AttributionInput{
		Channel: "whatsapp", UTM: utm, RefQR: "lagos-stand",
	})
	if err != nil || ch != "whatsapp" || promo != nil || gotUTM["utm_source"] != "google" {
		t.Fatalf("utm precedence: ch=%q promo=%v utm=%v err=%v", ch, promo, gotUTM, err)
	}

	// qr slug beats bare channel, synthesizes the utm triple.
	ch, promo, gotUTM, err = ResolveAttribution(AttributionInput{Channel: "web", RefQR: "lagos-stand"})
	if err != nil || ch != ChannelQR || promo != nil {
		t.Fatalf("qr precedence: ch=%q err=%v", ch, err)
	}
	if gotUTM["utm_source"] != "qr" || gotUTM["utm_medium"] != "offline" || gotUTM["utm_campaign"] != "lagos-stand" {
		t.Fatalf("qr utm synthesis: %+v", gotUTM)
	}

	// bare channel; empty channel defaults to web.
	ch, _, _, err = ResolveAttribution(AttributionInput{Channel: "VOICE"})
	if err != nil || ch != ChannelVoice {
		t.Fatalf("channel fallback: ch=%q err=%v", ch, err)
	}
	ch, _, _, err = ResolveAttribution(AttributionInput{})
	if err != nil || ch != ChannelWeb {
		t.Fatalf("empty channel default: ch=%q err=%v", ch, err)
	}

	// invalid channel rejected.
	if _, _, _, err = ResolveAttribution(AttributionInput{Channel: "smoke-signals"}); err == nil ||
		!strings.Contains(err.Error(), "smoke-signals") {
		t.Fatalf("invalid channel must error, got %v", err)
	}
}

// FunnelEvent shape (contract §2): entity lead, deterministic idempotency
// key, null amount_ngn, campaign/lga passthrough.
func TestNewFunnelEvent(t *testing.T) {
	tenantID := uuid.New()
	campaignID := uuid.New()
	lga := 42
	l := &store.Lead{
		ID:                  uuid.New(),
		TenantID:            tenantID,
		ChannelOfFirstTouch: ChannelQR,
		CampaignID:          &campaignID,
		LgaID:               &lga,
	}
	ts := time.Date(2026, 2, 3, 10, 0, 0, 0, time.UTC)
	evt := NewFunnelEvent(l, EventLeadCreated, ts)
	if evt.EventID == "" || evt.TenantID != tenantID.String() || evt.EntityType != EntityTypeLead ||
		evt.EntityID != l.ID.String() || evt.EventName != EventLeadCreated || !evt.EventTS.Equal(ts) ||
		evt.Channel != ChannelQR || evt.CampaignID == nil || *evt.CampaignID != campaignID ||
		evt.LgaID == nil || *evt.LgaID != 42 || evt.AmountNGN != nil {
		t.Fatalf("event: %+v", evt)
	}
	wantKey := "lead:" + l.ID.String() + ":" + EventLeadCreated
	if evt.IdempotencyKey != wantKey {
		t.Fatalf("idempotency_key = %q, want %q", evt.IdempotencyKey, wantKey)
	}
	m := evt.Map()
	if m["amount_ngn"] != nil || m["campaign_id"] != campaignID.String() || m["lga_id"] != 42 ||
		m["entity_type"] != "lead" || m["event_name"] != "lead_created" {
		t.Fatalf("map: %+v", m)
	}
	// Null campaign/lga stay null.
	evt2 := NewFunnelEvent(&store.Lead{ID: uuid.New(), TenantID: tenantID, ChannelOfFirstTouch: ChannelWeb}, EventContacted, ts)
	m2 := evt2.Map()
	if m2["campaign_id"] != nil || m2["lga_id"] != nil {
		t.Fatalf("null fields must stay null: %+v", m2)
	}
}
