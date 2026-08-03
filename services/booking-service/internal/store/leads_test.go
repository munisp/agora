package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// SPEC-W13 Agent A store tests run against embedded Postgres (same harness
// as the waitlist/CRM/incidents tests; STORE_TEST=0 / -short skips).

func strPtr(s string) *string { return &s }

func mkLead(tenantID uuid.UUID, phone, channel, dedupe string) Lead {
	return Lead{
		ID:                  uuid.New(),
		TenantID:            tenantID,
		PhoneE164:           phone,
		ChannelOfFirstTouch: channel,
		DedupeKey:           dedupe,
	}
}

// Dedupe idempotency (contract §1): same (tenant, dedupe_key) twice yields
// one row; the second insert returns the EXISTING lead with first-touch
// attribution intact (§3).
func TestInsertLeadDedupeFirstTouch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	in := mkLead(tenantID, "+2348011111111", "web", "dk-1")
	in.UTM = map[string]any{"utm_source": "google"}
	created, err := st.InsertLead(ctx, &in)
	if err != nil || !created {
		t.Fatalf("first insert: created=%v err=%v", created, err)
	}
	if in.Status != "new" {
		t.Fatalf("status = %q, want new", in.Status)
	}

	// Replay with DIFFERENT attribution: must not overwrite (first-touch).
	dup := mkLead(tenantID, "+2348011111111", "promo", "dk-1")
	dup.PromoCode = strPtr("LATER10")
	dup.UTM = map[string]any{"utm_source": "bing"}
	created, err = st.InsertLead(ctx, &dup)
	if err != nil {
		t.Fatalf("dup insert: %v", err)
	}
	if created {
		t.Fatal("dedupe hit must report created=false")
	}
	if dup.ID != in.ID || dup.ChannelOfFirstTouch != "web" || dup.PromoCode != nil {
		t.Fatalf("first-touch attribution overwritten: %+v", dup)
	}
	if src, _ := dup.UTM["utm_source"].(string); src != "google" {
		t.Fatalf("utm overwritten: %+v", dup.UTM)
	}

	// Cross-tenant isolation (app-level + RLS belt-and-braces).
	if _, err := st.GetLead(ctx, uuid.New(), in.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant get = %v, want ErrNotFound", err)
	}
}

// Status update + list filters.
func TestLeadStatusAndFilters(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	campaignID := uuid.New()

	a := mkLead(tenantID, "+234801", "whatsapp", "dk-a")
	a.CampaignID = &campaignID
	if _, err := st.InsertLead(ctx, &a); err != nil {
		t.Fatalf("insert a: %v", err)
	}
	b := mkLead(tenantID, "+234802", "qr", "dk-b")
	if _, err := st.InsertLead(ctx, &b); err != nil {
		t.Fatalf("insert b: %v", err)
	}

	updated, err := st.UpdateLeadStatus(ctx, tenantID, a.ID, "contacted")
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if updated.Status != "contacted" || updated.ChannelOfFirstTouch != "whatsapp" ||
		updated.CampaignID == nil || *updated.CampaignID != campaignID {
		t.Fatalf("status update touched attribution: %+v", updated)
	}
	if !updated.UpdatedAt.After(updated.CreatedAt) && !updated.UpdatedAt.Equal(updated.CreatedAt) {
		t.Fatalf("updated_at not stamped: %+v", updated)
	}

	contacted, err := st.ListLeads(ctx, tenantID, "contacted", "", nil, nil, nil)
	if err != nil || len(contacted) != 1 || contacted[0].ID != a.ID {
		t.Fatalf("status filter: %+v, %v", contacted, err)
	}
	qr, err := st.ListLeads(ctx, tenantID, "", "qr", nil, nil, nil)
	if err != nil || len(qr) != 1 || qr[0].ID != b.ID {
		t.Fatalf("channel filter: %+v, %v", qr, err)
	}
	byCampaign, err := st.ListLeads(ctx, tenantID, "", "", &campaignID, nil, nil)
	if err != nil || len(byCampaign) != 1 || byCampaign[0].ID != a.ID {
		t.Fatalf("campaign filter: %+v, %v", byCampaign, err)
	}
	future := time.Now().Add(24 * time.Hour)
	none, err := st.ListLeads(ctx, tenantID, "", "", nil, &future, nil)
	if err != nil || len(none) != 0 {
		t.Fatalf("from filter: %d, %v", len(none), err)
	}
	if _, err := st.UpdateLeadStatus(ctx, tenantID, uuid.New(), "contacted"); err != ErrNotFound {
		t.Fatalf("update missing = %v, want ErrNotFound", err)
	}
}

// Promo redemption (contract §6): idempotent per code+phone, count bumps
// only on first redemption, max_redemptions enforced, lead gets promo
// attribution.
func TestRedeemPromoTx(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	campaignID := uuid.New()

	promo := PromoCode{TenantID: tenantID, Code: "WELCOME50", CampaignID: &campaignID, MaxRedemptions: 2}
	if err := st.UpsertPromoCode(ctx, &promo); err != nil {
		t.Fatalf("upsert promo: %v", err)
	}

	newLead := func(phone string) *Lead {
		return &Lead{
			ID:                  uuid.New(),
			TenantID:            tenantID,
			PhoneE164:           phone,
			ChannelOfFirstTouch: "promo",
			CampaignID:          promo.CampaignID,
			PromoCode:           &promo.Code,
			DedupeKey:           "dk-promo-" + phone,
		}
	}

	lead, leadCreated, already, err := st.RedeemPromoTx(ctx, tenantID, "WELCOME50", "+2349001", newLead("+2349001"))
	if err != nil || !leadCreated || already {
		t.Fatalf("first redeem: created=%v already=%v err=%v", leadCreated, already, err)
	}
	if lead.ChannelOfFirstTouch != "promo" || lead.PromoCode == nil || *lead.PromoCode != "WELCOME50" ||
		lead.CampaignID == nil || *lead.CampaignID != campaignID {
		t.Fatalf("promo attribution: %+v", lead)
	}

	// Replay same code+phone: alreadyRedeemed, same lead, no count bump.
	replay, created2, already2, err := st.RedeemPromoTx(ctx, tenantID, "WELCOME50", "+2349001", newLead("+2349001"))
	if err != nil || created2 || !already2 {
		t.Fatalf("replay: created=%v already=%v err=%v", created2, already2, err)
	}
	if replay.ID != lead.ID {
		t.Fatalf("replay returned different lead: %s vs %s", replay.ID, lead.ID)
	}
	got, _ := st.GetPromoCode(ctx, tenantID, "WELCOME50")
	if got.RedeemedCount != 1 {
		t.Fatalf("redeemed_count after replay = %d, want 1", got.RedeemedCount)
	}

	// Second phone: allowed (max 2), count bumps.
	if _, created3, _, err := st.RedeemPromoTx(ctx, tenantID, "WELCOME50", "+2349002", newLead("+2349002")); err != nil || !created3 {
		t.Fatalf("second phone: created=%v err=%v", created3, err)
	}
	// Third phone: exhausted.
	if _, _, _, err := st.RedeemPromoTx(ctx, tenantID, "WELCOME50", "+2349003", newLead("+2349003")); err != ErrPromoExhausted {
		t.Fatalf("exhausted = %v, want ErrPromoExhausted", err)
	}
	got, _ = st.GetPromoCode(ctx, tenantID, "WELCOME50")
	if got.RedeemedCount != 2 {
		t.Fatalf("redeemed_count = %d, want 2", got.RedeemedCount)
	}

	// Unknown code + cross-tenant lookup guard.
	if _, _, _, err := st.RedeemPromoTx(ctx, tenantID, "NOPE", "+2349004", newLead("+2349004")); err != ErrNotFound {
		t.Fatalf("unknown code = %v, want ErrNotFound", err)
	}

	// Public lookup resolves the owning tenant.
	found, err := st.LookupPromoCode(ctx, "WELCOME50")
	if err != nil || found.TenantID != tenantID {
		t.Fatalf("lookup: %+v, %v", found, err)
	}
	if _, err := st.LookupPromoCode(ctx, "NOPE"); err != ErrNotFound {
		t.Fatalf("lookup unknown = %v, want ErrNotFound", err)
	}

	// List for the dashboard read.
	codes, err := st.ListPromoCodes(ctx, tenantID)
	if err != nil || len(codes) != 1 || codes[0].Code != "WELCOME50" || codes[0].RedeemedCount != 2 {
		t.Fatalf("list promo codes: %+v, %v", codes, err)
	}
}

// Campaign spend (§4): SET semantics make reposts idempotent; spend-sum
// honors day bounds and per-channel breakdown.
func TestCampaignSpendAndSum(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	c := Campaign{TenantID: tenantID, Name: "Lagos Radio Q1", Channel: "field"}
	if err := st.CreateCampaign(ctx, &c); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	day1 := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 1, 11, 0, 0, 0, 0, time.UTC)

	mk := func(day time.Time, channel string, amount float64) CampaignSpend {
		return CampaignSpend{TenantID: tenantID, CampaignID: c.ID, Day: day, Channel: channel, AmountNGN: amount}
	}
	if err := st.UpsertCampaignSpend(ctx, &CampaignSpend{TenantID: tenantID, CampaignID: c.ID, Day: day1, Channel: "field", AmountNGN: 5000}); err != nil {
		t.Fatalf("spend 1: %v", err)
	}
	sp2 := mk(day2, "field", 7000)
	if err := st.UpsertCampaignSpend(ctx, &sp2); err != nil {
		t.Fatalf("spend 2: %v", err)
	}
	sp3 := mk(day2, "qr", 3000)
	if err := st.UpsertCampaignSpend(ctx, &sp3); err != nil {
		t.Fatalf("spend 3: %v", err)
	}
	// Repost same (day, channel) with corrected amount: replace, not add.
	sp2b := mk(day2, "field", 7500)
	if err := st.UpsertCampaignSpend(ctx, &sp2b); err != nil {
		t.Fatalf("spend repost: %v", err)
	}

	total, byChannel, err := st.CampaignSpendSum(ctx, tenantID, c.ID, nil, nil)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if total != 5000+7500+3000 {
		t.Fatalf("total = %v, want 15500", total)
	}
	if len(byChannel) != 2 {
		t.Fatalf("by_channel = %+v", byChannel)
	}

	// Day-bounded sum.
	from, to := day2, day2
	total2, _, err := st.CampaignSpendSum(ctx, tenantID, c.ID, &from, &to)
	if err != nil || total2 != 7500+3000 {
		t.Fatalf("bounded total = %v, %v; want 10500", total2, err)
	}

	// Dashboard list: spend sum joined.
	views, err := st.ListCampaignsWithSpend(ctx, tenantID)
	if err != nil || len(views) != 1 {
		t.Fatalf("list campaigns: %+v, %v", views, err)
	}
	if views[0].SpendNGN != 15500 || views[0].Name != "Lagos Radio Q1" || views[0].Channel != "field" {
		t.Fatalf("campaign view: %+v", views[0])
	}

	// Spend against a missing campaign → ErrNotFound; cross-tenant → ErrNotFound.
	bad := mk(day1, "field", 100)
	bad.CampaignID = uuid.New()
	if err := st.UpsertCampaignSpend(ctx, &bad); err != ErrNotFound {
		t.Fatalf("missing campaign = %v, want ErrNotFound", err)
	}
	cross := mk(day1, "field", 100)
	cross.TenantID = uuid.New()
	if err := st.UpsertCampaignSpend(ctx, &cross); err != ErrNotFound {
		t.Fatalf("cross-tenant spend = %v, want ErrNotFound", err)
	}
}
