package store

// SPEC-W47 (W47-A) tests: extended TenantDeleted PII cascade (A-DB DB-02),
// full per-subject erasure (A-DB DB-03) and the campaigns lifecycle
// (A-DB DB-07).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// execTenant runs raw SQL inside the tenant's RLS tx (test seeding helper).
func execTenant(t *testing.T, st *Store, tenantID uuid.UUID, sql string, args ...any) {
	t.Helper()
	full := append([]any{tenantID}, args...)
	err := st.withTenant(context.Background(), tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), sql, full...)
		return err
	})
	if err != nil {
		t.Fatalf("seed %s: %v", sql[:40], err)
	}
}

// queryTenant reads one string value inside the tenant's RLS tx.
func queryTenant(t *testing.T, st *Store, tenantID uuid.UUID, sql string, args ...any) string {
	t.Helper()
	full := append([]any{tenantID}, args...)
	var out string
	err := st.withTenant(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), sql, full...).Scan(&out)
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return out
}

func w47SeedPII(t *testing.T, st *Store, tenantID uuid.UUID, phone string) {
	t.Helper()
	sum := sha256.Sum256([]byte(tenantID.String() + "|" + phone + "|web|" + time.Now().Format("2006-01-02")))
	dedupe := hex.EncodeToString(sum[:])
	leadID := uuid.New()
	execTenant(t, st, tenantID,
		`INSERT INTO leads (id, tenant_id, phone_e164, channel_of_first_touch, dedupe_key) VALUES ($2,$1,$3,'web',$4)`,
		leadID, phone, dedupe)
	execTenant(t, st, tenantID,
		`INSERT INTO waitlist (id, tenant_id, offering_id, contact_name, contact_phone, window_start, window_end, claim_token)
		 VALUES ($2,$1,$3,'Ada Lovelace',$4, now(), now() + interval '1 hour', $5)`,
		uuid.New(), uuid.New(), phone, uuid.NewString())
	execTenant(t, st, tenantID,
		`INSERT INTO referral_agents (id, tenant_id, name, phone, status) VALUES ($2,$1,'Agent Ada',$3,'approved')`,
		uuid.New(), phone)
	execTenant(t, st, tenantID,
		`INSERT INTO referrals (id, tenant_id, referrer_type, referrer_id, referee_phone, status)
		 VALUES ($2,$1,'agent',$3,$4,'paid')`, uuid.New(), uuid.NewString(), phone)
	code := "W47-" + uuid.NewString()[:8]
	execTenant(t, st, tenantID,
		`INSERT INTO promo_codes (tenant_id, code) VALUES ($1,$2)`, code)
	execTenant(t, st, tenantID,
		`INSERT INTO promo_redemptions (tenant_id, code, phone_e164, lead_id) VALUES ($1,$2,$3,$4)`,
		code, phone, leadID)
	execTenant(t, st, tenantID,
		`INSERT INTO civic_cases (tenant_id, ref, description, category_id, reporter_phone_e164, reporter_name)
		 VALUES ($1,$2,'Pothole on main street',$3,$4,'Reporter Ada')`,
		"CSTM-"+uuid.NewString()[:8], uuid.New(), phone)
	execTenant(t, st, tenantID,
		`INSERT INTO qr_scans (tenant_id, site_slug, user_agent) VALUES ($1,'acme','Mozilla/5.0 test-agent')`)
}

// w47AssertPIIGone verifies all seeded PII rows are tombstoned; civicWant
// is the expected reporter_phone_e164|reporter_name pair ("NULL|NULL" for
// the tenant cascade, tombstone|NULL for subject erasure).
func w47AssertPIIGone(t *testing.T, st *Store, tenantID uuid.UUID, phone, civicWant string) {
	t.Helper()
	tomb := hashPii(phone)
	if got := queryTenant(t, st, tenantID, `SELECT phone_e164 FROM leads WHERE tenant_id=$1`); got != tomb {
		t.Fatalf("leads phone not tombstoned: %q", got)
	}
	if got := queryTenant(t, st, tenantID, `SELECT contact_name || '|' || contact_phone FROM waitlist WHERE tenant_id=$1`); got != "erased|"+tomb {
		t.Fatalf("waitlist not tombstoned: %q", got)
	}
	if got := queryTenant(t, st, tenantID, `SELECT name || '|' || phone FROM referral_agents WHERE tenant_id=$1`); got != "erased|"+tomb {
		t.Fatalf("referral_agents not tombstoned: %q", got)
	}
	if got := queryTenant(t, st, tenantID, `SELECT referee_phone FROM referrals WHERE tenant_id=$1`); got != tomb {
		t.Fatalf("referrals phone not tombstoned: %q", got)
	}
	if got := queryTenant(t, st, tenantID, `SELECT phone_e164 FROM promo_redemptions WHERE tenant_id=$1`); got != tomb {
		t.Fatalf("promo_redemptions phone not tombstoned: %q", got)
	}
	if got := queryTenant(t, st, tenantID,
		`SELECT COALESCE(reporter_phone_e164,'NULL') || '|' || COALESCE(reporter_name,'NULL') FROM civic_cases WHERE tenant_id=$1`); got != civicWant {
		t.Fatalf("civic_cases reporter not erased: %q want %q", got, civicWant)
	}
}

// TestDeleteTenantData_ExtendedCascade (A-DB DB-02): the TenantDeleted
// cascade anonymizes/deletes every PII table, keeps financial rows
// (referral ledger entry stays, hashed), is idempotent, and skips tables
// absent from a partial deployment (helpdesk/surveys/etc. not bootstrapped
// here).
func TestDeleteTenantData_ExtendedCascade(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	phone := "+2348012345678"

	c := Contact{TenantID: tenantID, Name: "Grace Hopper", Phone: phone}
	if err := st.CreateContact(ctx, &c); err != nil {
		t.Fatal(err)
	}
	b := Booking{TenantID: tenantID, ContactID: c.ID, OfferingID: uuid.New(),
		TeamMemberID: uuid.New(), StartsAt: time.Now().Add(time.Hour), EndsAt: time.Now().Add(2 * time.Hour),
		Status: "confirmed", Source: "api"}
	if err := st.CreateBookingTx(ctx, &b, SlotGuard{}, "test.events", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	w47SeedPII(t, st, tenantID, phone)

	sum, err := st.DeleteTenantData(ctx, tenantID)
	if err != nil {
		t.Fatalf("DeleteTenantData: %v", err)
	}
	if sum.ContactsAnonymized != 1 || sum.BookingsCancelled != 1 {
		t.Fatalf("summary contacts/bookings: %+v", sum)
	}
	for _, tbl := range []string{"leads", "waitlist", "referral_agents", "referrals", "promo_redemptions", "civic_cases", "qr_scans"} {
		if sum.Details[tbl] != 1 {
			t.Fatalf("cascade step %s affected %d rows, want 1 (details=%+v)", tbl, sum.Details[tbl], sum.Details)
		}
	}
	w47AssertPIIGone(t, st, tenantID, phone, "NULL|NULL")
	if got := queryTenant(t, st, tenantID, `SELECT count(*)::text FROM qr_scans WHERE tenant_id=$1`); got != "0" {
		t.Fatalf("qr_scans not deleted: %q", got)
	}
	// Financial/audit rows stay: the referral ledger row exists (hashed).
	if got := queryTenant(t, st, tenantID, `SELECT status FROM referrals WHERE tenant_id=$1`); got != "paid" {
		t.Fatalf("referral audit row lost: %q", got)
	}
	// Idempotent redelivery: everything converges to zero.
	sum2, err := st.DeleteTenantData(ctx, tenantID)
	if err != nil {
		t.Fatalf("second DeleteTenantData: %v", err)
	}
	if sum2.ContactsAnonymized != 0 || sum2.BookingsCancelled != 0 {
		t.Fatalf("second run not idempotent: %+v", sum2)
	}
	for tbl, n := range sum2.Details {
		if n != 0 {
			t.Fatalf("second run step %s affected %d rows, want 0", tbl, n)
		}
	}
	// Partial deployment: helpdesk/surveys/device/portal/field tables are
	// absent here — their steps must be skipped silently (no error above).
	if _, ok := sum.Details["tickets"]; ok {
		t.Fatalf("tickets step should have been skipped (table absent): %+v", sum.Details)
	}
}

// TestAnonymizeSubject_Extended (A-DB DB-03): PrivacyEraseRequested now
// tombstones the subject across every subject-keyed table in one tx.
func TestAnonymizeSubject_Extended(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID, otherTenant := uuid.New(), uuid.New()
	phone := "+15551234567"

	c1 := Contact{TenantID: tenantID, Name: "Subject", Phone: phone}
	if err := st.CreateContact(ctx, &c1); err != nil {
		t.Fatal(err)
	}
	c2 := Contact{TenantID: otherTenant, Name: "Other", Phone: phone}
	if err := st.CreateContact(ctx, &c2); err != nil {
		t.Fatal(err)
	}
	w47SeedPII(t, st, tenantID, phone)

	sum, err := st.AnonymizeSubject(ctx, tenantID, phone, "")
	if err != nil {
		t.Fatalf("AnonymizeSubject: %v", err)
	}
	if sum.ContactsAnonymized != 1 {
		t.Fatalf("contacts affected: %d", sum.ContactsAnonymized)
	}
	for _, tbl := range []string{"leads", "waitlist", "referral_agents", "referrals", "promo_redemptions", "civic_cases"} {
		if sum.Details[tbl] != 1 {
			t.Fatalf("subject erase step %s affected %d, want 1", tbl, sum.Details[tbl])
		}
	}
	w47AssertPIIGone(t, st, tenantID, phone, hashPii(phone)+"|NULL")

	// Idempotent: second erase matches nothing (tombstones ≠ cleartext).
	sum2, err := st.AnonymizeSubject(ctx, tenantID, phone, "")
	if err != nil {
		t.Fatal(err)
	}
	if sum2.ContactsAnonymized != 0 {
		t.Fatalf("second erase not idempotent: %+v", sum2)
	}
	for tbl, n := range sum2.Details {
		if n != 0 {
			t.Fatalf("second erase step %s affected %d", tbl, n)
		}
	}

	// Cross-tenant isolation: the other tenant's contact keeps its PII.
	if got := queryTenant(t, st, otherTenant, `SELECT name || '|' || phone FROM contacts WHERE tenant_id=$1`); got != "Other|"+phone {
		t.Fatalf("other tenant contact touched: %q", got)
	}
}

// TestCampaignLifecycle (A-DB DB-07): campaigns are manageable end-to-end —
// create (active) → update → pause/resume → cancel (terminal soft-delete),
// with spend rejected after cancel.
func TestCampaignLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	c := Campaign{TenantID: tenantID, Name: "Launch", Channel: "instagram"}
	if err := st.CreateCampaign(ctx, &c); err != nil {
		t.Fatal(err)
	}
	if c.Status != CampaignStatusActive {
		t.Fatalf("new campaign status %q, want active", c.Status)
	}

	newName := "Launch v2"
	c, err := st.UpdateCampaign(ctx, tenantID, c.ID, CampaignUpdate{Name: &newName, Status: CampaignStatusPaused})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if c.Name != "Launch v2" || c.Status != CampaignStatusPaused {
		t.Fatalf("updated campaign: %+v", c)
	}
	// paused → active resumes.
	if c, err = st.UpdateCampaign(ctx, tenantID, c.ID, CampaignUpdate{Status: CampaignStatusActive}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// Illegal value rejected.
	if _, err = st.UpdateCampaign(ctx, tenantID, c.ID, CampaignUpdate{Status: "bogus"}); !errors.Is(err, ErrInvalidCampaignStatus) {
		t.Fatalf("bogus status: %v", err)
	}
	// Spend while active: fine.
	if err := st.UpsertCampaignSpend(ctx, &CampaignSpend{TenantID: tenantID, CampaignID: c.ID,
		Day: time.Now(), Channel: "instagram", AmountNGN: 100}); err != nil {
		t.Fatalf("spend: %v", err)
	}
	// Cancel (terminal).
	if c, err = st.CancelCampaign(ctx, tenantID, c.ID); err != nil || c.Status != CampaignStatusCancelled {
		t.Fatalf("cancel: %v %+v", err, c)
	}
	// Idempotent cancel.
	if c2, err := st.CancelCampaign(ctx, tenantID, c.ID); err != nil || c2.Status != CampaignStatusCancelled {
		t.Fatalf("re-cancel: %v %+v", err, c2)
	}
	// Cancelled is terminal: updates and new spend are rejected.
	if _, err = st.UpdateCampaign(ctx, tenantID, c.ID, CampaignUpdate{Name: &newName}); !errors.Is(err, ErrCampaignCancelled) {
		t.Fatalf("update after cancel: %v", err)
	}
	if err = st.UpsertCampaignSpend(ctx, &CampaignSpend{TenantID: tenantID, CampaignID: c.ID,
		Day: time.Now(), Channel: "instagram", AmountNGN: 50}); !errors.Is(err, ErrCampaignCancelled) {
		t.Fatalf("spend after cancel: %v", err)
	}
	// The audit trail survives: list still shows the cancelled campaign
	// with its spend.
	views, err := st.ListCampaignsWithSpend(ctx, tenantID)
	if err != nil || len(views) != 1 {
		t.Fatalf("list: %v %d", err, len(views))
	}
	if views[0].Status != CampaignStatusCancelled || views[0].SpendNGN != 100 {
		t.Fatalf("campaign view: %+v", views[0])
	}
	// Cross-tenant isolation.
	if _, err := st.GetCampaign(ctx, uuid.New(), c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant get: %v", err)
	}
	if _, err := st.CancelCampaign(ctx, uuid.New(), c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant cancel: %v", err)
	}
}
