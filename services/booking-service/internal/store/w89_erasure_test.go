package store

// SPEC-W89 tests: erasure canonical coverage — the per-subject tombstone
// matches the W61 G3 digits form BOTH SIDES, so format-variant rows
// ('+234…' vs '234…') of the SAME subject can no longer survive erasure
// (the W88-registered NDPA residual). Guards: digit-less needle matches
// NOTHING; multi-format duplicates get row-unique discriminants so the
// uniqueness invariants (promo PK, referrals open-phone index) hold.
// REDIRECTION PROOF: fails on the pristine tree (raw-equality steps
// leave the alternate-format rows in cleartext).

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestW89ErasureCoversFormatVariants(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	phonePlus := "+234 801 777 6666"
	phoneDigits := "2348017776666"
	tomb := hashPii(phonePlus)

	// Contacts in BOTH forms (raw insert — the app dedupe is post-W89 but
	// the erasure must cope with pre-dedupe databases too).
	execTenant(t, st, tenantID,
		`INSERT INTO contacts (id, tenant_id, name, phone) VALUES ($2,$1,'Plus',$3)`, uuid.New(), phonePlus)
	execTenant(t, st, tenantID,
		`INSERT INTO contacts (id, tenant_id, name, phone) VALUES ($2,$1,'Digits',$3)`, uuid.New(), phoneDigits)

	// Subject-keyed tables, each holding the ALTERNATE format.
	execTenant(t, st, tenantID,
		`INSERT INTO leads (id, tenant_id, phone_e164, channel_of_first_touch, dedupe_key) VALUES ($2,$1,$3,'web',$4)`,
		uuid.New(), phoneDigits, uuid.NewString())
	execTenant(t, st, tenantID,
		`INSERT INTO waitlist (id, tenant_id, offering_id, contact_name, contact_phone, window_start, window_end, claim_token)
		 VALUES ($2,$1,$3,'Ada',$4, now(), now() + interval '1 hour', $5)`,
		uuid.New(), uuid.New(), phoneDigits, uuid.NewString())
	execTenant(t, st, tenantID,
		`INSERT INTO referral_agents (id, tenant_id, name, phone, status) VALUES ($2,$1,'Agent',$3,'approved')`,
		uuid.New(), phoneDigits)
	execTenant(t, st, tenantID,
		`INSERT INTO referrals (id, tenant_id, referrer_type, referrer_id, referee_phone, status)
		 VALUES ($2,$1,'agent',$3,$4,'paid')`, uuid.New(), uuid.NewString(), phoneDigits)
	execTenant(t, st, tenantID,
		`INSERT INTO promo_codes (tenant_id, code) VALUES ($1,$2)`, "W89-"+uuid.NewString()[:8])
	code := queryTenant(t, st, tenantID, `SELECT code FROM promo_codes WHERE tenant_id=$1`)
	execTenant(t, st, tenantID,
		`INSERT INTO promo_redemptions (tenant_id, code, phone_e164, lead_id) VALUES ($1,$2,$3,$4)`,
		code, phoneDigits, uuid.New())
	execTenant(t, st, tenantID,
		`INSERT INTO civic_cases (tenant_id, ref, description, category_id, reporter_phone_e164, reporter_name)
		 VALUES ($1,$2,'x',$3,$4,'Ada')`, "CSTM-"+uuid.NewString()[:8], uuid.New(), phoneDigits)

	sum, err := st.AnonymizeSubject(ctx, tenantID, phonePlus, "")
	if err != nil {
		t.Fatalf("AnonymizeSubject: %v", err)
	}
	if sum.ContactsAnonymized != 2 {
		t.Fatalf("contacts anonymized = %d, want 2 (both format rows)", sum.ContactsAnonymized)
	}

	// Every format-variant row is tombstoned; single-row tables keep the
	// EXACT hashPii form (no discriminator needed).
	assertGone := func(table, col string) {
		t.Helper()
		got := queryTenant(t, st, tenantID,
			`SELECT count(*)::text FROM `+table+` WHERE tenant_id=$1 AND `+col+` NOT LIKE 'sha256:%' AND `+col+` <> ''`)
		if got != "0" {
			t.Fatalf("%s.%s still has cleartext: %s rows", table, col, got)
		}
	}
	assertGone("contacts", "phone")
	assertGone("leads", "phone_e164")
	assertGone("waitlist", "contact_phone")
	assertGone("referral_agents", "phone")
	assertGone("referrals", "referee_phone")
	assertGone("promo_redemptions", "phone_e164")
	assertGone("civic_cases", "reporter_phone_e164")
	if got := queryTenant(t, st, tenantID,
		`SELECT phone_e164 FROM leads WHERE tenant_id=$1`); got != tomb {
		t.Fatalf("single-match tombstone changed form: %q want %q", got, tomb)
	}
	// Contacts: both rows tombstoned, DISTINCT tombstones (digits unique
	// index intact), name erased.
	if got := queryTenant(t, st, tenantID,
		`SELECT count(DISTINCT phone)::text FROM contacts WHERE tenant_id=$1`); got != "2" {
		t.Fatalf("contact tombstones not row-unique: %s", got)
	}
	if got := queryTenant(t, st, tenantID,
		`SELECT count(*)::text FROM contacts WHERE tenant_id=$1 AND name='erased'`); got != "2" {
		t.Fatalf("contacts not erased: %s", got)
	}
	// civic reporter name NULLed.
	if got := queryTenant(t, st, tenantID,
		`SELECT COALESCE(reporter_name,'NULL') FROM civic_cases WHERE tenant_id=$1`); got != "NULL" {
		t.Fatalf("civic reporter name = %s", got)
	}
	// Idempotent: second erase matches nothing.
	sum2, err := st.AnonymizeSubject(ctx, tenantID, phonePlus, "")
	if err != nil {
		t.Fatal(err)
	}
	if sum2.ContactsAnonymized != 0 {
		t.Fatalf("second erase matched %d contacts", sum2.ContactsAnonymized)
	}
	// Digit-less needle tombstones NOTHING (guard).
	execTenant(t, st, tenantID,
		`INSERT INTO waitlist (id, tenant_id, offering_id, contact_name, contact_phone, window_start, window_end, claim_token)
		 VALUES ($2,$1,$3,'Bee',$4, now(), now() + interval '1 hour', $5)`,
		uuid.New(), uuid.New(), "(no digits here)", uuid.NewString())
	sum3, err := st.AnonymizeSubject(ctx, tenantID, "(no digits here)", "")
	if err != nil {
		t.Fatal(err)
	}
	if sum3.ContactsAnonymized != 0 || sum3.Details["waitlist"] != 0 {
		t.Fatalf("digit-less needle matched: %+v", sum3)
	}
	if got := queryTenant(t, st, tenantID,
		`SELECT contact_phone FROM waitlist WHERE tenant_id=$1 AND contact_name='Bee'`); got != "(no digits here)" {
		t.Fatalf("digit-less row was tombstoned: %q", got)
	}
}

func TestW89ErasureMultiFormatUniqueSafe(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	// Two OPEN referrals for the same subject in two formats: both must
	// tombstone WITHOUT tripping the open-phone unique index.
	execTenant(t, st, tenantID,
		`INSERT INTO referrals (id, tenant_id, referrer_type, referrer_id, referee_phone, status)
		 VALUES ($2,$1,'agent','r1','+2348010001111','pending')`, uuid.New())
	execTenant(t, st, tenantID,
		`INSERT INTO referrals (id, tenant_id, referrer_type, referrer_id, referee_phone, status)
		 VALUES ($2,$1,'agent','r2','2348010001111','pending')`, uuid.New())
	if _, err := st.AnonymizeSubject(ctx, tenantID, "+234 801 000 1111", ""); err != nil {
		t.Fatalf("multi-format erase failed (unique index): %v", err)
	}
	if got := queryTenant(t, st, tenantID,
		`SELECT count(*)::text FROM referrals WHERE tenant_id=$1 AND referee_phone LIKE 'sha256:%'`); got != "2" {
		t.Fatalf("open referrals not both tombstoned: %s", got)
	}
	if got := queryTenant(t, st, tenantID,
		`SELECT count(DISTINCT referee_phone)::text FROM referrals WHERE tenant_id=$1`); got != "2" {
		t.Fatalf("referral tombstones not row-unique: %s", got)
	}
	// The discriminator is prefixed by the shared salted hash.
	if got := queryTenant(t, st, tenantID,
		`SELECT count(*)::text FROM referrals WHERE tenant_id=$1 AND referee_phone LIKE '`+hashPii("+234 801 000 1111")+`:%'`); got != "2" {
		t.Fatalf("tombstones lack the shared-hash prefix: %s", got)
	}
	_ = strings.TrimSpace // (kept imports stable if assertions shift)
}
