package referrals

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/opendesk/booking-service/internal/events"
	"github.com/opendesk/booking-service/internal/leads"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// SPEC-W14 Agent A service tests run against embedded Postgres (same harness
// as the leads service tests; dedicated port 5548 to avoid the
// postmaster.pid race under `go test ./...`; -short skips). The outbox table
// is infra-managed, so it is created here for the funnel-emission asserts.
func newServiceTestStore(t *testing.T) *store.Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres referrals service test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_referrals_test").
		Port(5548).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })
	dsn := "postgres://postgres:postgres@localhost:5548/booking_referrals_test?sslmode=disable"
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

func newService(st *store.Store) *Service {
	return &Service{
		Store:          st,
		Ledger:         NewPostgresLedger(st),
		Leads:          &leads.Service{Store: st, CACEventsTopic: "cac.events", Log: zap.NewNop()},
		CACEventsTopic: "cac.events",
		Log:            zap.NewNop(),
	}
}

// funnelEvents drains the outbox and decodes every cac.events CloudEvent
// payload into its FunnelEvent data (same helper shape as the leads tests).
func funnelEvents(t *testing.T, st *store.Store, topic string) []leads.FunnelEvent {
	t.Helper()
	rows, err := st.FetchUnsentOutbox(context.Background(), 100)
	if err != nil {
		t.Fatalf("fetch outbox: %v", err)
	}
	var out []leads.FunnelEvent
	for _, r := range rows {
		if r.Topic != topic {
			t.Fatalf("unexpected outbox topic %q", r.Topic)
		}
		var ce events.CloudEvent
		if err := json.Unmarshal(r.Payload, &ce); err != nil {
			t.Fatalf("payload not a CloudEvent: %v", err)
		}
		if ce.Type != leads.EventTypeFunnel {
			t.Fatalf("cloudevent type = %q, want %q", ce.Type, leads.EventTypeFunnel)
		}
		raw, _ := json.Marshal(ce.Data)
		var fe leads.FunnelEvent
		if err := json.Unmarshal(raw, &fe); err != nil {
			t.Fatalf("data not a FunnelEvent: %v", err)
		}
		out = append(out, fe)
	}
	return out
}

func mkSvcRule(t *testing.T, svc *Service, tenantID uuid.UUID, name, trigger, beneficiary, amountType string, amountNGN int64, bps int, cap *int64, active bool, priority int) CommissionRule {
	t.Helper()
	r := CommissionRule{
		TenantID: tenantID, Name: name, Trigger: trigger, Beneficiary: beneficiary,
		AmountType: amountType, AmountNGN: amountNGN, Bps: bps, CapNGN: cap,
		Active: active, Priority: priority,
	}
	if err := svc.CreateRule(context.Background(), &r); err != nil {
		t.Fatalf("create rule %s: %v", name, err)
	}
	return r
}

// End-to-end verify (contract §1–§3): rules fire in priority order, the
// balanced accrual pair lands (301 debit house / 300 credit beneficiary),
// the referral flips to verified, the §6 funnel hook is emitted — and a
// replay is a strict no-op (idempotent).
func TestVerifyFiresRulesPostsAndIsIdempotent(t *testing.T) {
	st := newServiceTestStore(t)
	svc := newService(st)
	ctx := context.Background()
	tenantID := uuid.New()

	flat := mkSvcRule(t, svc, tenantID, "signup-flat", TriggerSignupVerified, BeneficiaryReferrer, AmountFlat, 50000, 0, nil, true, 1)
	mkSvcRule(t, svc, tenantID, "inactive", TriggerSignupVerified, BeneficiaryReferrer, AmountFlat, 99000, 0, nil, false, 2)
	mkSvcRule(t, svc, tenantID, "agent-only", TriggerSignupVerified, BeneficiaryAgent, AmountFlat, 77000, 0, nil, true, 3)
	mkSvcRule(t, svc, tenantID, "other-trigger", TriggerSale, BeneficiaryReferrer, AmountFlat, 88000, 0, nil, true, 1)

	ref, created, err := svc.Create(ctx, CreateInput{
		TenantID: tenantID, ReferrerType: ReferrerContact, ReferrerID: "contact-1", RefereePhone: "+2348099990001",
	})
	if err != nil || !created {
		t.Fatalf("create referral: created=%v err=%v", created, err)
	}

	res, err := svc.Verify(ctx, tenantID, ref.ID, TriggerSignupVerified, 0, "acme-ng")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.AlreadyVerified {
		t.Fatal("first verify must not be AlreadyVerified")
	}
	if res.Referral.Status != StatusVerified || res.Referral.VerifiedAt == nil {
		t.Fatalf("referral after verify: %+v", res.Referral)
	}
	// Only the flat rule fires (inactive/agent/other-trigger skipped;
	// percent rules would compute 0 on base 0 anyway).
	if len(res.Awards) != 1 || res.Awards[0].RuleID != flat.ID || res.Awards[0].AmountKobo != 50000 {
		t.Fatalf("awards: %+v", res.Awards)
	}
	if res.Referral.BountyRuleID == nil || *res.Referral.BountyRuleID != flat.ID {
		t.Fatalf("bounty_rule_id: %+v", res.Referral.BountyRuleID)
	}

	// Balanced pair, one journal (contract §3).
	entries, err := svc.LedgerEntries(ctx, tenantID, nil, nil)
	if err != nil {
		t.Fatalf("ledger entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ledger entries = %d, want 2: %+v", len(entries), entries)
	}
	var debit, credit store.LedgerEntry
	for _, e := range entries {
		if e.JournalID != AccrualJournalID(ref.ID, flat.ID) {
			t.Fatalf("unexpected journal id %v", e.JournalID)
		}
		if e.RefType != RefTypeCommissionAccrual || e.RefID != AccrualRefID(ref.ID, flat.ID) {
			t.Fatalf("unexpected ref %s/%s", e.RefType, e.RefID)
		}
		switch e.AccountCode {
		case AccountCommissionExpense:
			debit = e
		case AccountCommissionPayable:
			credit = e
		default:
			t.Fatalf("unexpected account %d", e.AccountCode)
		}
	}
	if debit.DebitNGN != 50000 || debit.CreditNGN != 0 || debit.BeneficiaryID != "" {
		t.Fatalf("expense side: %+v", debit)
	}
	if credit.CreditNGN != 50000 || credit.DebitNGN != 0 || credit.BeneficiaryID != "contact-1" {
		t.Fatalf("payable side: %+v", credit)
	}

	// Balance known vector: contact-1 payable = 50,000 kobo.
	bal, err := svc.CommissionBalance(ctx, tenantID, "contact-1")
	if err != nil || bal != 50000 {
		t.Fatalf("balance = %d, %v; want 50000", bal, err)
	}

	// §6 funnel hook: converted, entity_type customer, deterministic key.
	evts := funnelEvents(t, st, "cac.events")
	if len(evts) != 1 {
		t.Fatalf("funnel events = %d, want 1: %+v", len(evts), evts)
	}
	if evts[0].EventName != leads.EventConverted || evts[0].EntityType != EntityTypeCustomer ||
		evts[0].EntityID != "+2348099990001" || evts[0].Channel != ChannelReferral ||
		evts[0].IdempotencyKey != "referral:"+ref.ID.String()+":converted" {
		t.Fatalf("funnel hook: %+v", evts[0])
	}

	// Idempotent replay: no new postings, no new events, same referral.
	res2, err := svc.Verify(ctx, tenantID, ref.ID, TriggerSale, 123456, "acme-ng")
	if err != nil {
		t.Fatalf("replay verify: %v", err)
	}
	if !res2.AlreadyVerified || len(res2.Awards) != 0 {
		t.Fatalf("replay: %+v", res2)
	}
	entries, _ = svc.LedgerEntries(ctx, tenantID, nil, nil)
	if len(entries) != 2 {
		t.Fatalf("entries after replay = %d, want 2", len(entries))
	}
	if evts := funnelEvents(t, st, "cac.events"); len(evts) != 1 {
		t.Fatalf("events after replay = %d, want 1", len(evts))
	}
}

// Revenue triggers convert the referral and compute percent commissions
// against the verify base (integer kobo, capped); first_txn emits the
// first_txn hook with amount_ngn.
func TestVerifyRevenueTriggerConvertedFirstTxn(t *testing.T) {
	st := newServiceTestStore(t)
	svc := newService(st)
	ctx := context.Background()
	tenantID := uuid.New()

	mkSvcRule(t, svc, tenantID, "txn5pct-cap60k", TriggerFirstTxn, BeneficiaryReferrer, AmountPercent, 0, 500, i64(60000), true, 1)

	ref, _, err := svc.Create(ctx, CreateInput{
		TenantID: tenantID, ReferrerType: ReferrerAgent, ReferrerID: "agent-7", RefereePhone: "+2348099990002",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := svc.Verify(ctx, tenantID, ref.ID, TriggerFirstTxn, 2_000_000, "acme-ng")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Referral.Status != StatusConverted {
		t.Fatalf("status = %q, want converted", res.Referral.Status)
	}
	// 5% of 2,000,000 = 100,000 → capped to 60,000 kobo.
	if len(res.Awards) != 1 || res.Awards[0].AmountKobo != 60000 || res.Awards[0].BeneficiaryID != "agent-7" {
		t.Fatalf("awards: %+v", res.Awards)
	}
	bal, _ := svc.CommissionBalance(ctx, tenantID, "agent-7")
	if bal != 60000 {
		t.Fatalf("balance = %d, want 60000", bal)
	}
	evts := funnelEvents(t, st, "cac.events")
	if len(evts) != 1 || evts[0].EventName != leads.EventFirstTxn {
		t.Fatalf("events: %+v", evts)
	}
	if evts[0].AmountNGN == nil || *evts[0].AmountNGN != 20000 {
		t.Fatalf("amount_ngn = %v, want 20000 (kobo→NGN)", evts[0].AmountNGN)
	}
}

// §6 coordination with W13: verify walks the referee's open lead to
// converted through the leads SERVICE (contacted/qualified/converted lead
// funnel events fire in order), then the referral hook lands.
func TestVerifyConvertsRefereeLead(t *testing.T) {
	st := newServiceTestStore(t)
	svc := newService(st)
	ctx := context.Background()
	tenantID := uuid.New()
	phone := "+2348099990003"

	leadSvc := &leads.Service{Store: st, CACEventsTopic: "cac.events", Log: zap.NewNop()}
	lead, created, err := leadSvc.Create(ctx, leads.CreateInput{
		TenantID: tenantID, PhoneE164: phone, Channel: leads.ChannelWeb,
	}, "acme-ng")
	if err != nil || !created {
		t.Fatalf("create lead: created=%v err=%v", created, err)
	}

	ref, _, err := svc.Create(ctx, CreateInput{
		TenantID: tenantID, ReferrerType: ReferrerContact, ReferrerID: "contact-9", RefereePhone: phone,
	})
	if err != nil {
		t.Fatalf("create referral: %v", err)
	}
	// No rules at all → referral still verifies, no postings.
	if _, err := svc.Verify(ctx, tenantID, ref.ID, TriggerSignupVerified, 0, "acme-ng"); err != nil {
		t.Fatalf("verify: %v", err)
	}

	got, err := st.GetLead(ctx, tenantID, lead.ID)
	if err != nil {
		t.Fatalf("get lead: %v", err)
	}
	if got.Status != leads.StatusConverted {
		t.Fatalf("lead status = %q, want converted", got.Status)
	}
	entries, _ := svc.LedgerEntries(ctx, tenantID, nil, nil)
	if len(entries) != 0 {
		t.Fatalf("entries = %d, want 0 (no rules)", len(entries))
	}

	// lead_created (create) + contacted/qualified/converted (walk) +
	// converted (referral hook) — the outbox fetch has no ORDER BY, so the
	// assertion is a set comparison on idempotency keys.
	evts := funnelEvents(t, st, "cac.events")
	want := map[string]bool{
		"lead:" + lead.ID.String() + ":lead_created": false,
		"lead:" + lead.ID.String() + ":contacted":    false,
		"lead:" + lead.ID.String() + ":qualified":    false,
		"lead:" + lead.ID.String() + ":converted":    false,
		"referral:" + ref.ID.String() + ":converted": false,
	}
	if len(evts) != len(want) {
		t.Fatalf("events = %d, want %d: %+v", len(evts), len(want), evts)
	}
	for _, e := range evts {
		if _, ok := want[e.IdempotencyKey]; !ok {
			t.Fatalf("unexpected event key %q (all: %+v)", e.IdempotencyKey, evts)
		}
		want[e.IdempotencyKey] = true
	}
	for key, seen := range want {
		if !seen {
			t.Fatalf("missing event %q (all: %+v)", key, evts)
		}
	}
}

// Reject is the audit-preserving delete; terminal states are protected.
func TestRejectFlow(t *testing.T) {
	st := newServiceTestStore(t)
	svc := newService(st)
	ctx := context.Background()
	tenantID := uuid.New()

	ref, _, err := svc.Create(ctx, CreateInput{
		TenantID: tenantID, ReferrerType: ReferrerStaff, ReferrerID: "staff-1", RefereePhone: "+2348099990004",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rej, err := svc.Reject(ctx, tenantID, ref.ID)
	if err != nil || rej.Status != StatusRejected {
		t.Fatalf("reject: %+v err=%v", rej, err)
	}
	if _, err := svc.Verify(ctx, tenantID, ref.ID, TriggerSignupVerified, 0, "acme-ng"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("verify rejected: err=%v, want ErrInvalidTransition", err)
	}
	if _, err := svc.Reject(ctx, tenantID, ref.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("re-reject: err=%v, want ErrInvalidTransition", err)
	}
	if _, err := svc.Reject(ctx, tenantID, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reject missing: err=%v, want ErrNotFound", err)
	}
}

// Create dedupe (contract §1): one OPEN referral per (tenant, referee_phone);
// after rejection a NEW referral for the same phone may be opened.
func TestCreateDedupeOpenOnly(t *testing.T) {
	st := newServiceTestStore(t)
	svc := newService(st)
	ctx := context.Background()
	tenantID := uuid.New()

	in := CreateInput{TenantID: tenantID, ReferrerType: ReferrerContact, ReferrerID: "c-1", RefereePhone: "+2348099990005"}
	r1, created1, err := svc.Create(ctx, in)
	if err != nil || !created1 {
		t.Fatalf("first create: created=%v err=%v", created1, err)
	}
	r2, created2, err := svc.Create(ctx, CreateInput{
		TenantID: tenantID, ReferrerType: ReferrerAgent, ReferrerID: "a-2", RefereePhone: in.RefereePhone,
	})
	if err != nil || created2 {
		t.Fatalf("dedupe create: created=%v err=%v", created2, err)
	}
	if r2.ID != r1.ID || r2.ReferrerID != "c-1" {
		t.Fatalf("dedupe returned wrong row: %+v", r2)
	}
	if _, err := svc.Reject(ctx, tenantID, r1.ID); err != nil {
		t.Fatalf("reject: %v", err)
	}
	r3, created3, err := svc.Create(ctx, in)
	if err != nil || !created3 || r3.ID == r1.ID {
		t.Fatalf("create after reject: created=%v id=%v err=%v", created3, r3.ID, err)
	}
}

// Ledger known vectors (contract §3): balanced-pair validation, idempotent
// replay on (ref_type, ref_id, account_code), and balance math
// (credits − debits) across accrual + payout postings.
func TestLedgerKnownVectors(t *testing.T) {
	st := newServiceTestStore(t)
	ledger := NewPostgresLedger(st)
	ctx := context.Background()
	tenantID := uuid.New()

	// Unbalanced / malformed journals are rejected before any row lands.
	if err := ledger.Post(ctx, tenantID, uuid.New(), []LedgerEntry{
		{TenantID: tenantID, AccountCode: 301, DebitNGN: 100, RefType: "t", RefID: "x"},
		{TenantID: tenantID, AccountCode: 300, CreditNGN: 50, RefType: "t", RefID: "x"},
	}); !errors.Is(err, ErrUnbalancedJournal) {
		t.Fatalf("unbalanced journal: err=%v", err)
	}
	if err := ledger.Post(ctx, tenantID, uuid.New(), []LedgerEntry{
		{TenantID: tenantID, AccountCode: 301, DebitNGN: 100, RefType: "t", RefID: "x"},
	}); !errors.Is(err, ErrUnbalancedJournal) {
		t.Fatalf("single-entry journal: err=%v", err)
	}
	if err := ledger.PostBalanced(ctx, BalancedPosting{
		TenantID: tenantID, DebitAccount: 305, CreditAccount: 300, AmountNGN: 100, RefType: "t", RefID: "y",
	}); !errors.Is(err, ErrUnbalancedJournal) {
		t.Fatalf("bad account code: err=%v", err)
	}

	// Accrual: +50,000 payable for agent-1 (known vector 1).
	accrual := NewAccrualPair(tenantID, Referral{ID: uuid.New(), TenantID: tenantID},
		Award{RuleID: uuid.New(), BeneficiaryID: "agent-1", AmountKobo: 50000})
	if err := ledger.Post(ctx, tenantID, accrual[0].JournalID, accrual); err != nil {
		t.Fatalf("accrual post: %v", err)
	}
	// Replay the exact journal: strict no-op (idempotency).
	if err := ledger.Post(ctx, tenantID, accrual[0].JournalID, accrual); err != nil {
		t.Fatalf("accrual replay: %v", err)
	}

	// Payout: debit 300 / credit 302 for 20,000 (known vector 2), twice.
	payout := BalancedPosting{
		TenantID: tenantID, DebitAccount: AccountCommissionPayable, CreditAccount: AccountAgentFloat,
		AmountNGN: 20000, RefType: RefTypePayout, RefID: uuid.NewString(), BeneficiaryID: "agent-1",
	}
	if err := ledger.PostBalanced(ctx, payout); err != nil {
		t.Fatalf("payout post: %v", err)
	}
	if err := ledger.PostBalanced(ctx, payout); err != nil {
		t.Fatalf("payout replay: %v", err)
	}

	bal, err := ledger.Balance(ctx, tenantID, AccountCommissionPayable, "agent-1")
	if err != nil || bal != 30000 {
		t.Fatalf("payable balance = %d, %v; want 30000 (50000 accrued − 20000 paid)", bal, err)
	}
	floatBal, err := ledger.Balance(ctx, tenantID, AccountAgentFloat, "agent-1")
	if err != nil || floatBal != 20000 {
		t.Fatalf("float balance = %d, %v; want 20000", floatBal, err)
	}
	entries, err := ledger.Entries(ctx, tenantID, nil, nil)
	if err != nil || len(entries) != 4 {
		t.Fatalf("entries = %d, want 4 (2 journals × 2, replays deduped)", len(entries))
	}
}
