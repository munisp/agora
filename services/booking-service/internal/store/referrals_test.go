package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// SPEC-W14 Agent A store tests run against embedded Postgres (same harness
// as the waitlist/CRM/incidents/leads tests; STORE_TEST=0 / -short skips).

func mkReferral(tenantID uuid.UUID, phone string) Referral {
	return Referral{
		ID:           uuid.New(),
		TenantID:     tenantID,
		ReferrerType: "contact",
		ReferrerID:   "contact-1",
		RefereePhone: phone,
	}
}

// Contract §1 dedupe: one OPEN referral per (tenant, referee_phone).
func TestInsertReferralOpenDedupe(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	in := mkReferral(tenantID, "+2348022222222")
	created, err := st.InsertReferral(ctx, &in)
	if err != nil || !created {
		t.Fatalf("first insert: created=%v err=%v", created, err)
	}
	if in.Status != "pending" {
		t.Fatalf("status = %q, want pending", in.Status)
	}

	// Replay: same tenant+phone → existing open referral returned unchanged.
	dup := mkReferral(tenantID, "+2348022222222")
	dup.ReferrerID = "other"
	created, err = st.InsertReferral(ctx, &dup)
	if err != nil {
		t.Fatalf("dup insert: %v", err)
	}
	if created {
		t.Fatal("open dedupe hit must report created=false")
	}
	if dup.ID != in.ID || dup.ReferrerID != "contact-1" {
		t.Fatalf("existing row not returned: %+v", dup)
	}

	// Another tenant may refer the same phone (RLS-scoped dedupe).
	other := mkReferral(uuid.New(), "+2348022222222")
	created, err = st.InsertReferral(ctx, &other)
	if err != nil || !created {
		t.Fatalf("cross-tenant insert: created=%v err=%v", created, err)
	}
}

// VerifyReferralTx (contract §1+§3): status flip + balanced entries commit
// atomically; a replay short-circuits with already=true and posts nothing.
func TestVerifyReferralTxIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	ref := mkReferral(tenantID, "+2348033333333")
	if _, err := st.InsertReferral(ctx, &ref); err != nil {
		t.Fatalf("insert: %v", err)
	}
	ruleID := uuid.New()
	entries := []LedgerEntry{
		{TenantID: tenantID, JournalID: uuid.New(), AccountCode: 301, DebitNGN: 40000,
			RefType: "commission_accrual", RefID: ref.ID.String() + ":" + ruleID.String()},
		{TenantID: tenantID, JournalID: uuid.Nil, AccountCode: 300, CreditNGN: 40000,
			BeneficiaryID: "contact-1", RefType: "commission_accrual", RefID: ref.ID.String() + ":" + ruleID.String()},
	}
	journal := uuid.New()
	entries[0].JournalID = journal
	entries[1].JournalID = journal

	updated, already, err := st.VerifyReferralTx(ctx, tenantID, ref.ID, "verified", &ruleID, entries)
	if err != nil || already {
		t.Fatalf("verify tx: already=%v err=%v", already, err)
	}
	if updated.Status != "verified" || updated.VerifiedAt == nil ||
		updated.BountyRuleID == nil || *updated.BountyRuleID != ruleID {
		t.Fatalf("updated referral: %+v", updated)
	}

	updated2, already2, err := st.VerifyReferralTx(ctx, tenantID, ref.ID, "converted", nil, entries)
	if err != nil || !already2 {
		t.Fatalf("replay: already=%v err=%v", already2, err)
	}
	if updated2.Status != "verified" {
		t.Fatalf("replay changed status: %+v", updated2)
	}
	rows, err := st.ListLedgerEntries(ctx, tenantID, nil, nil)
	if err != nil || len(rows) != 2 {
		t.Fatalf("entries = %d, want 2", len(rows))
	}
}

// Reject: pending|verified → rejected; converted/paid/rejected are terminal.
func TestRejectReferralTransitions(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	ref := mkReferral(tenantID, "+2348044444444")
	if _, err := st.InsertReferral(ctx, &ref); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rej, err := st.RejectReferral(ctx, tenantID, ref.ID)
	if err != nil || rej.Status != "rejected" {
		t.Fatalf("reject: %+v err=%v", rej, err)
	}
	if _, err := st.RejectReferral(ctx, tenantID, ref.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("re-reject: err=%v, want ErrConflict", err)
	}
	if _, err := st.RejectReferral(ctx, tenantID, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reject missing: err=%v, want ErrNotFound", err)
	}
}

// Rules CRUD + evaluation ordering (contract §2): priority asc.
func TestRuleCRUDAndOrdering(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	for _, p := range []int{30, 10, 20} {
		r := CommissionRule{
			TenantID: tenantID, Name: "r", Trigger: "first_txn", Beneficiary: "referrer",
			AmountType: "flat", AmountNGN: 100, Active: true, Priority: p,
		}
		if err := st.InsertRule(ctx, &r); err != nil {
			t.Fatalf("insert rule p=%d: %v", p, err)
		}
	}
	rules, err := st.ListRules(ctx, tenantID)
	if err != nil || len(rules) != 3 {
		t.Fatalf("list rules: %d err=%v", len(rules), err)
	}
	if rules[0].Priority != 10 || rules[1].Priority != 20 || rules[2].Priority != 30 {
		t.Fatalf("order = %d,%d,%d", rules[0].Priority, rules[1].Priority, rules[2].Priority)
	}

	rules[0].Name = "renamed"
	rules[0].Active = false
	if err := st.UpdateRule(ctx, &rules[0]); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := st.GetRule(ctx, tenantID, rules[0].ID)
	if err != nil || got.Name != "renamed" || got.Active {
		t.Fatalf("get after update: %+v err=%v", got, err)
	}
	if err := st.DeleteRule(ctx, tenantID, rules[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetRule(ctx, tenantID, rules[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted: err=%v", err)
	}
}

// Ledger store (contract §3): idempotent insert on
// (ref_type, ref_id, account_code), from/to listing and the balance
// known-vector math (credits − debits).
func TestLedgerStoreKnownVectors(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	j1 := uuid.New()
	journal1 := []LedgerEntry{
		{TenantID: tenantID, JournalID: j1, AccountCode: 301, DebitNGN: 70000, RefType: "commission_accrual", RefID: "r1"},
		{TenantID: tenantID, JournalID: j1, AccountCode: 300, CreditNGN: 70000, BeneficiaryID: "b1", RefType: "commission_accrual", RefID: "r1"},
	}
	if err := st.PostLedgerJournal(ctx, tenantID, journal1); err != nil {
		t.Fatalf("post j1: %v", err)
	}
	if err := st.PostLedgerJournal(ctx, tenantID, journal1); err != nil {
		t.Fatalf("replay j1: %v", err)
	}
	j2 := uuid.New()
	journal2 := []LedgerEntry{
		{TenantID: tenantID, JournalID: j2, AccountCode: 300, DebitNGN: 25000, BeneficiaryID: "b1", RefType: "commission_payout", RefID: "p1"},
		{TenantID: tenantID, JournalID: j2, AccountCode: 302, CreditNGN: 25000, BeneficiaryID: "b1", RefType: "commission_payout", RefID: "p1"},
	}
	if err := st.PostLedgerJournal(ctx, tenantID, journal2); err != nil {
		t.Fatalf("post j2: %v", err)
	}

	bal, err := st.LedgerBalance(ctx, tenantID, 300, "b1")
	if err != nil || bal != 45000 {
		t.Fatalf("balance = %d, %v; want 45000 (70000 credit − 25000 debit)", bal, err)
	}
	floatBal, err := st.LedgerBalance(ctx, tenantID, 302, "b1")
	if err != nil || floatBal != 25000 {
		t.Fatalf("float balance = %d, want 25000", floatBal)
	}
	expense, err := st.LedgerBalance(ctx, tenantID, 301, "")
	if err != nil || expense != -70000 {
		t.Fatalf("expense balance = %d, want −70000 (debit side)", expense)
	}

	all, err := st.ListLedgerEntries(ctx, tenantID, nil, nil)
	if err != nil || len(all) != 4 {
		t.Fatalf("entries = %d, want 4 (replay deduped)", len(all))
	}
	future := time.Now().Add(24 * time.Hour)
	none, err := st.ListLedgerEntries(ctx, tenantID, &future, nil)
	if err != nil || len(none) != 0 {
		t.Fatalf("from-filter entries = %d, want 0", len(none))
	}
}

// The referral → lead seam: open leads match by phone, terminal leads don't.
func TestFindOpenLeadByPhone(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	lead := Lead{
		ID: uuid.New(), TenantID: tenantID, PhoneE164: "+2348055555555",
		ChannelOfFirstTouch: "web", DedupeKey: "dk-ref-1",
	}
	if _, err := st.InsertLead(ctx, &lead); err != nil {
		t.Fatalf("insert lead: %v", err)
	}
	got, err := st.FindOpenLeadByPhone(ctx, tenantID, lead.PhoneE164)
	if err != nil || got.ID != lead.ID {
		t.Fatalf("find open: %+v err=%v", got, err)
	}
	if _, err := st.UpdateLeadStatus(ctx, tenantID, lead.ID, "lost"); err != nil {
		t.Fatalf("lose lead: %v", err)
	}
	if _, err := st.FindOpenLeadByPhone(ctx, tenantID, lead.PhoneE164); !errors.Is(err, ErrNotFound) {
		t.Fatalf("find terminal: err=%v, want ErrNotFound", err)
	}
	if _, err := st.FindOpenLeadByPhone(ctx, tenantID, "+2348000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("find unknown: err=%v, want ErrNotFound", err)
	}
}
