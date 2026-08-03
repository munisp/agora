package leads

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/opendesk/booking-service/internal/events"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// newServiceTestStore boots embedded Postgres like the incidents service
// tests (dedicated port 5546 to avoid the postmaster.pid race under
// `go test ./...`; -short skips). The outbox table is infra-managed, so it
// is created here for the funnel-emission asserts.
func newServiceTestStore(t *testing.T) *store.Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres leads service test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_leads_test").
		Port(5546).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })
	dsn := "postgres://postgres:postgres@localhost:5546/booking_leads_test?sslmode=disable"
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	defer conn.Close(ctx) //nolint:errcheck
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS outbox (
	    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	    aggregate_id UUID NOT NULL,
	    topic TEXT NOT NULL,
	    payload JSONB NOT NULL,
	    sent_at TIMESTAMPTZ
	)`); err != nil {
		t.Fatalf("outbox ddl: %v", err)
	}
	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// funnelEvents drains the outbox and decodes every cac.events CloudEvent
// payload into its FunnelEvent data.
func funnelEvents(t *testing.T, st *store.Store, topic string) []FunnelEvent {
	t.Helper()
	rows, err := st.FetchUnsentOutbox(context.Background(), 100)
	if err != nil {
		t.Fatalf("fetch outbox: %v", err)
	}
	var out []FunnelEvent
	for _, r := range rows {
		if r.Topic != topic {
			t.Fatalf("unexpected outbox topic %q", r.Topic)
		}
		var ce events.CloudEvent
		if err := json.Unmarshal(r.Payload, &ce); err != nil {
			t.Fatalf("payload not a CloudEvent: %v", err)
		}
		if ce.Type != EventTypeFunnel {
			t.Fatalf("cloudevent type = %q, want %q", ce.Type, EventTypeFunnel)
		}
		raw, _ := json.Marshal(ce.Data)
		var fe FunnelEvent
		if err := json.Unmarshal(raw, &fe); err != nil {
			t.Fatalf("data not a FunnelEvent: %v", err)
		}
		out = append(out, fe)
	}
	return out
}

// Create + full transition chain (SPEC-W13 Agent A): each lifecycle step
// emits exactly one FunnelEvent; the dedupe replay emits nothing.
func TestCreateAndTransitionEmitsFunnel(t *testing.T) {
	st := newServiceTestStore(t)
	svc := &Service{Store: st, CACEventsTopic: "cac.events", FirstTouchOnly: true, Log: zap.NewNop()}
	ctx := context.Background()
	tenantID := uuid.New()

	lead, created, err := svc.Create(ctx, CreateInput{
		TenantID:  tenantID,
		PhoneE164: "+2348012345678",
		Channel:   "whatsapp",
		UTM:       map[string]any{"utm_source": "fb", "utm_medium": "paid"},
	}, "acme-ng")
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	if lead.ChannelOfFirstTouch != "whatsapp" || lead.DedupeKey == "" {
		t.Fatalf("lead: %+v", lead)
	}

	// Same phone+channel same day: dedupe hit, existing returned, no event.
	// Different UTM on the replay must NOT overwrite the first touch (§3).
	dup, created2, err := svc.Create(ctx, CreateInput{
		TenantID: tenantID, PhoneE164: "+2348012345678", Channel: "whatsapp",
		UTM: map[string]any{"utm_source": "other"},
	}, "acme-ng")
	if err != nil || created2 {
		t.Fatalf("dedupe create: created=%v err=%v", created2, err)
	}
	if dup.ID != lead.ID || dup.PromoCode != nil {
		t.Fatalf("first-touch violated: %+v", dup)
	}
	if src, _ := dup.UTM["utm_source"].(string); src != "fb" {
		t.Fatalf("first-touch utm overwritten: %+v", dup.UTM)
	}

	// Transition chain: new→contacted→qualified→converted.
	for _, to := range []string{StatusContacted, StatusQualified, StatusConverted} {
		lead, err = svc.Transition(ctx, tenantID, lead.ID, to, "acme-ng")
		if err != nil || lead.Status != to {
			t.Fatalf("transition to %s: %+v err=%v", to, lead, err)
		}
	}
	// Terminal: converted→lost is illegal.
	if _, err := svc.Transition(ctx, tenantID, lead.ID, StatusLost, "acme-ng"); err == nil {
		t.Fatal("converted→lost must fail")
	}

	evts := funnelEvents(t, st, "cac.events")
	if len(evts) != 4 {
		t.Fatalf("funnel events = %d, want 4: %+v", len(evts), evts)
	}
	// Outbox id is gen_random_uuid() — order is not insertion order.
	byName := map[string]FunnelEvent{}
	for _, e := range evts {
		byName[e.EventName] = e
		if e.EntityID != lead.ID.String() || e.EntityType != "lead" || e.TenantID != tenantID.String() {
			t.Fatalf("event entity/tenant: %+v", e)
		}
		if e.IdempotencyKey != "lead:"+lead.ID.String()+":"+e.EventName {
			t.Fatalf("idempotency_key: %q", e.IdempotencyKey)
		}
	}
	for _, name := range []string{EventLeadCreated, EventContacted, EventQualified, EventConverted} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("missing funnel event %q in %+v", name, evts)
		}
	}
	if byName[EventLeadCreated].Channel != "whatsapp" {
		t.Fatalf("channel on lead_created: %q", byName[EventLeadCreated].Channel)
	}
}

// Promo redemption (contract §6): creates the promo-attributed lead +
// lead_created event exactly once; replay creates nothing new.
func TestRedeemPromoService(t *testing.T) {
	st := newServiceTestStore(t)
	svc := &Service{Store: st, CACEventsTopic: "cac.events", FirstTouchOnly: true, Log: zap.NewNop()}
	ctx := context.Background()
	tenantID := uuid.New()
	campaignID := uuid.New()

	promo := store.PromoCode{TenantID: tenantID, Code: "WELCOME50", CampaignID: &campaignID}
	if err := st.UpsertPromoCode(ctx, &promo); err != nil {
		t.Fatalf("upsert promo: %v", err)
	}

	lead, created, err := svc.RedeemPromo(ctx, "WELCOME50", "+2349001", "acme-ng")
	if err != nil || !created {
		t.Fatalf("redeem: created=%v err=%v", created, err)
	}
	if lead.ChannelOfFirstTouch != ChannelPromo || lead.PromoCode == nil || *lead.PromoCode != "WELCOME50" ||
		lead.CampaignID == nil || *lead.CampaignID != campaignID {
		t.Fatalf("promo attribution: %+v", lead)
	}

	// Replay: same lead, no new event.
	lead2, created2, err := svc.RedeemPromo(ctx, "WELCOME50", "+2349001", "acme-ng")
	if err != nil || created2 || lead2.ID != lead.ID {
		t.Fatalf("replay: created=%v lead=%s err=%v", created2, lead2.ID, err)
	}

	evts := funnelEvents(t, st, "cac.events")
	if len(evts) != 1 || evts[0].EventName != EventLeadCreated || evts[0].Channel != ChannelPromo {
		t.Fatalf("funnel events: %+v", evts)
	}
	if evts[0].CampaignID == nil || *evts[0].CampaignID != campaignID {
		t.Fatalf("campaign on event: %+v", evts[0])
	}

	// Unknown code → store.ErrNotFound (handler maps to 404).
	if _, _, err := svc.RedeemPromo(ctx, "NOPE", "+2349002", "acme-ng"); err != store.ErrNotFound {
		t.Fatalf("unknown code = %v, want ErrNotFound", err)
	}
}

// RecordSpend validation + persistence round-trip.
func TestRecordSpendService(t *testing.T) {
	st := newServiceTestStore(t)
	svc := &Service{Store: st, CACEventsTopic: "cac.events", Log: zap.NewNop()}
	ctx := context.Background()
	tenantID := uuid.New()

	c := store.Campaign{TenantID: tenantID, Name: "Billboards Lekki", Channel: "field"}
	if err := svc.CreateCampaign(ctx, &c); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	day := time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC)
	if _, err := svc.RecordSpend(ctx, tenantID, c.ID, "", 100, day); err == nil {
		t.Fatal("empty channel must fail")
	}
	if _, err := svc.RecordSpend(ctx, tenantID, c.ID, "field", -5, day); err == nil {
		t.Fatal("negative amount must fail")
	}
	sp, err := svc.RecordSpend(ctx, tenantID, c.ID, "Field", 12500.50, day)
	if err != nil {
		t.Fatalf("record spend: %v", err)
	}
	if sp.Channel != "field" || sp.AmountNGN != 12500.50 {
		t.Fatalf("spend: %+v", sp)
	}
	total, byChannel, err := st.CampaignSpendSum(ctx, tenantID, c.ID, nil, nil)
	if err != nil || total != 12500.50 || len(byChannel) != 1 {
		t.Fatalf("sum: %v %+v %v", total, byChannel, err)
	}
}
