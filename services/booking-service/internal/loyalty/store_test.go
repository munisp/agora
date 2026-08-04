package loyalty

import (
	"context"
	"errors"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
)

// SPEC-W19 Agent C store tests run against embedded Postgres (same harness
// as the devices/leads/referrals tests; dedicated port 5562 avoids the
// postmaster.pid race with sibling packages under `go test ./...`; -short
// skips). The outbox table is bootstrapped here (the shared store owns it
// in production) so the lifecycle/metering emission path runs for real.

func newTestStore(t *testing.T) *Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres loyalty store test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_loyalty_test").
		Port(5562).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })
	st, err := DialStore(context.Background(),
		"postgres://postgres:postgres@localhost:5562/booking_loyalty_test?sslmode=disable")
	if err != nil {
		t.Fatalf("DialStore: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.pool.Exec(context.Background(), `
CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    sent_at TIMESTAMPTZ
);`); err != nil {
		t.Fatalf("bootstrap outbox: %v", err)
	}
	return st
}

func mkProgram(tenantID uuid.UUID) Program {
	return Program{
		TenantID: tenantID,
		Name:     "Club",
		Active:   true,
		EarnRules: []EarnRule{
			{Event: EventBookingCompleted, Points: 50},
			{Event: EventFirstTxn, Points: 100},
		},
		Tiers: []Tier{
			{Name: "silver", MinPoints: 100, Benefits: "priority support"},
			{Name: "gold", MinPoints: 200, Benefits: "free upgrades"},
		},
	}
}

// Programs CRUD: create → list → patch (merged validation) → active
// resolution; cross-tenant patch → ErrNotFound.
func TestProgramCRUD(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	p := mkProgram(tenantID)
	if err := st.CreateProgram(ctx, &p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.ID == uuid.Nil || p.CreatedAt.IsZero() {
		t.Fatalf("id/timestamps not stamped: %+v", p)
	}

	list, err := st.ListPrograms(ctx, tenantID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v, %v", list, err)
	}
	if cross, err := st.ListPrograms(ctx, uuid.New()); err != nil || len(cross) != 0 {
		t.Fatalf("cross-tenant list: %+v, %v", cross, err)
	}

	// Patch: change earn rules + deactivate. Validation runs on the merged
	// row (a dup-event patch must be rejected).
	newRules := []EarnRule{{Event: EventReferralConverted, Points: 25}}
	active := false
	patched, err := st.UpdateProgram(ctx, tenantID, p.ID, ProgramPatch{EarnRules: &newRules, Active: &active})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if patched.Active || patched.PointsForEvent(EventReferralConverted) != 25 || patched.Name != "Club" {
		t.Fatalf("patch did not merge: %+v", patched)
	}
	dup := []EarnRule{{Event: EventFirstTxn, Points: 1}, {Event: EventFirstTxn, Points: 2}}
	if _, err := st.UpdateProgram(ctx, tenantID, p.ID, ProgramPatch{EarnRules: &dup}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("dup-event patch = %v, want ErrInvalidInput", err)
	}
	if _, err := st.UpdateProgram(ctx, uuid.New(), p.ID, ProgramPatch{Name: strPtr("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant patch = %v, want ErrNotFound", err)
	}

	// Inactive now → ActiveProgram misses; reactivate → resolves again.
	if _, err := st.ActiveProgram(ctx, tenantID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("inactive program resolved: %v", err)
	}
	on := true
	if _, err := st.UpdateProgram(ctx, tenantID, p.ID, ProgramPatch{Active: &on}); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	act, err := st.ActiveProgram(ctx, tenantID)
	if err != nil || act.ID != p.ID {
		t.Fatalf("active resolution: %+v, %v", act, err)
	}
}

func strPtr(s string) *string { return &s }

// Accrue: applies earn rules, idempotent on ref_id+event (ledger UNIQUE),
// tier recomputed from lifetime_earned, wallet matches the ledger balance.
func TestAccrueIdempotentAndTier(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := uuid.New()
	prog := mkProgram(tenantID)

	svc := &Service{Store: st, Ledger: NewPostgresLedger(st)}
	if err := st.CreateProgram(ctx, &prog); err != nil {
		t.Fatalf("create program: %v", err)
	}

	// booking_completed → 50 points, tier still "" (below silver@100).
	res, err := svc.Accrue(ctx, tenantID, contactID, EventBookingCompleted, "bk-1")
	if err != nil {
		t.Fatalf("accrue: %v", err)
	}
	if !res.Applied || res.Awarded != 50 || res.Wallet.Balance != 50 || res.Wallet.Tier != "" {
		t.Fatalf("first accrual: %+v", res)
	}

	// Replay (same ref_id+event) → no-op.
	re, err := svc.Accrue(ctx, tenantID, contactID, EventBookingCompleted, "bk-1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if re.Applied || re.Awarded != 0 || re.Wallet.Balance != 50 || re.Wallet.LifetimeEarned != 50 {
		t.Fatalf("replay must be a no-op: %+v", re)
	}

	// Same ref_id, DIFFERENT event is a distinct idempotency key.
	res2, err := svc.Accrue(ctx, tenantID, contactID, EventFirstTxn, "bk-1")
	if err != nil {
		t.Fatalf("second event: %v", err)
	}
	if !res2.Applied || res2.Awarded != 100 || res2.Wallet.Balance != 150 {
		t.Fatalf("distinct event accrual: %+v", res2)
	}
	// lifetime 150 → silver (>=100) but not gold (>=200).
	if res2.Wallet.Tier != "silver" || res2.Wallet.LifetimeEarned != 150 {
		t.Fatalf("tier recompute: %+v", res2.Wallet)
	}

	// Push past gold@200 → tier flips.
	res3, err := svc.Accrue(ctx, tenantID, contactID, EventBookingCompleted, "bk-2")
	if err != nil {
		t.Fatalf("third accrual: %v", err)
	}
	if res3.Wallet.Tier != "gold" || res3.Wallet.Balance != 200 || res3.Wallet.LifetimeEarned != 200 {
		t.Fatalf("gold transition: %+v", res3.Wallet)
	}

	// Wallet cache == ledger-derived balance (account 400, beneficiary).
	bal, err := svc.Ledger.Balance(ctx, tenantID, AccountPointsIssued, contactID.String())
	if err != nil || bal != res3.Wallet.Balance {
		t.Fatalf("ledger balance %d != wallet %d (err %v)", bal, res3.Wallet.Balance, err)
	}

	// Unknown event / unawarded event / no program.
	if _, err := svc.Accrue(ctx, tenantID, contactID, "nonsense", "x"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown event = %v, want ErrInvalidInput", err)
	}
	if _, err := svc.Accrue(ctx, tenantID, contactID, EventReferralConverted, "x"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unawarded event = %v, want ErrInvalidInput", err)
	}
	if _, err := svc.Accrue(ctx, uuid.New(), contactID, EventFirstTxn, "x"); !errors.Is(err, ErrNoActiveProgram) {
		t.Fatalf("no program = %v, want ErrNoActiveProgram", err)
	}
}

// cap_per_day: over-cap accruals are clamped to the remaining allowance;
// an exhausted cap awards 0 without posting a journal; replays stay
// consistent.
func TestAccrueCapPerDay(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := uuid.New()

	prog := mkProgram(tenantID)
	prog.CapPerDay = 120
	if err := st.CreateProgram(ctx, &prog); err != nil {
		t.Fatalf("create: %v", err)
	}
	svc := &Service{Store: st, Ledger: NewPostgresLedger(st)}

	// first_txn = 100 → full award (100 <= 120).
	r1, err := svc.Accrue(ctx, tenantID, contactID, EventFirstTxn, "c1")
	if err != nil || r1.Awarded != 100 || r1.Capped {
		t.Fatalf("first: %+v, %v", r1, err)
	}
	// booking_completed = 50 → clamped to 20 (cap 120 − 100 earned).
	r2, err := svc.Accrue(ctx, tenantID, contactID, EventBookingCompleted, "c2")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !r2.Applied || !r2.Capped || r2.Awarded != 20 || r2.Wallet.Balance != 120 {
		t.Fatalf("clamped accrual: %+v", r2)
	}
	// Cap exhausted → awarded 0, capped, no journal posted.
	r3, err := svc.Accrue(ctx, tenantID, contactID, EventBookingCompleted, "c3")
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if r3.Applied || !r3.Capped || r3.Awarded != 0 || r3.Wallet.Balance != 120 {
		t.Fatalf("exhausted cap: %+v", r3)
	}
	// A zero-award accrual posts nothing → a later replay still computes 0.
	r3b, err := svc.Accrue(ctx, tenantID, contactID, EventBookingCompleted, "c3")
	if err != nil || r3b.Awarded != 0 || r3b.Wallet.Balance != 120 {
		t.Fatalf("zero-award replay: %+v, %v", r3b, err)
	}

	// Ledger holds exactly the 100 + 20 credited, not the clamped-away 30.
	bal, err := svc.Ledger.Balance(ctx, tenantID, AccountPointsIssued, contactID.String())
	if err != nil || bal != 120 {
		t.Fatalf("ledger balance = %d, want 120 (err %v)", bal, err)
	}

	// The cap is per-contact: a different contact keeps earning.
	other := uuid.New()
	r4, err := svc.Accrue(ctx, tenantID, other, EventBookingCompleted, "c4")
	if err != nil || r4.Awarded != 50 {
		t.Fatalf("other contact unaffected by cap: %+v, %v", r4, err)
	}
}

// Redeem: insufficient balance → *InsufficientError (409 at the API);
// successful redeem posts a balanced journal; ref_id makes retries
// idempotent.
func TestRedeem(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := uuid.New()
	prog := mkProgram(tenantID)
	if err := st.CreateProgram(ctx, &prog); err != nil {
		t.Fatalf("create: %v", err)
	}
	svc := &Service{Store: st, Ledger: NewPostgresLedger(st)}

	// Redeem with NO wallet at all → insufficient with balance 0.
	_, err := svc.Redeem(ctx, tenantID, contactID, 10, "early", "r0")
	var insuff *InsufficientError
	if !errors.As(err, &insuff) || insuff.Balance != 0 {
		t.Fatalf("empty-wallet redeem = %v, want insufficient(0)", err)
	}

	if _, err := svc.Accrue(ctx, tenantID, contactID, EventFirstTxn, "bk"); err != nil {
		t.Fatalf("accrue: %v", err)
	}

	// Over-balance redemption → 409 carrier with the current balance.
	if _, err = svc.Redeem(ctx, tenantID, contactID, 150, "too much", "r1"); !errors.As(err, &insuff) || insuff.Balance != 100 {
		t.Fatalf("over-balance redeem = %v, want insufficient(100)", err)
	}

	// Valid redemption → balanced journal + wallet update.
	res, err := svc.Redeem(ctx, tenantID, contactID, 40, "voucher", "r2")
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if !res.Applied || res.Redeemed != 40 || res.Wallet.Balance != 60 || res.Wallet.LifetimeRedeemed != 40 {
		t.Fatalf("redeem result: %+v", res)
	}
	// Tier is driven by lifetime_earned (100 → silver) — redemption does
	// NOT demote.
	if res.Wallet.Tier != "silver" {
		t.Fatalf("redeem must not demote tier: %+v", res.Wallet)
	}

	// Idempotent retry on ref_id → no-op.
	re, err := svc.Redeem(ctx, tenantID, contactID, 40, "voucher", "r2")
	if err != nil {
		t.Fatalf("redeem replay: %v", err)
	}
	if re.Applied || re.Redeemed != 0 || re.Wallet.Balance != 60 || re.Wallet.LifetimeRedeemed != 40 {
		t.Fatalf("redeem replay must be a no-op: %+v", re)
	}

	// Ledger agrees: 100 issued − 40 redeemed = 60 outstanding.
	bal, err := svc.Ledger.Balance(ctx, tenantID, AccountPointsIssued, contactID.String())
	if err != nil || bal != 60 {
		t.Fatalf("ledger balance = %d, want 60 (err %v)", bal, err)
	}
	entries, err := svc.Ledger.Entries(ctx, tenantID, nil, nil)
	if err != nil || len(entries) != 4 { // accrual pair + redeem pair
		t.Fatalf("ledger entries: %+v, %v", entries, err)
	}
	var debits, credits int64
	for _, e := range entries {
		debits += e.DebitPoints
		credits += e.CreditPoints
	}
	if debits != credits {
		t.Fatalf("journal hygiene: debits %d != credits %d", debits, credits)
	}

	// Validation: non-positive points.
	if _, err := svc.Redeem(ctx, tenantID, contactID, 0, "x", "r3"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero redeem = %v, want ErrInvalidInput", err)
	}
}

// RLS isolation: every table is FORCE-RLS'd; a second tenant sees nothing
// and cannot touch the first tenant's rows even knowing the ids.
func TestRLSIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	contactA := uuid.New()

	prog := mkProgram(tenantA)
	if err := st.CreateProgram(ctx, &prog); err != nil {
		t.Fatalf("create: %v", err)
	}
	svc := &Service{Store: st, Ledger: NewPostgresLedger(st)}
	if _, err := svc.Accrue(ctx, tenantA, contactA, EventFirstTxn, "bk-rls"); err != nil {
		t.Fatalf("accrue: %v", err)
	}

	// Programs / wallets / leaderboard / ledger all invisible cross-tenant.
	if list, err := st.ListPrograms(ctx, tenantB); err != nil || len(list) != 0 {
		t.Fatalf("B sees A's programs: %+v, %v", list, err)
	}
	if _, err := st.GetWallet(ctx, tenantB, contactA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B reads A's wallet: %v", err)
	}
	if lb, err := st.Leaderboard(ctx, tenantB, "", 10); err != nil || len(lb) != 0 {
		t.Fatalf("B sees A's leaderboard: %+v, %v", lb, err)
	}
	if entries, err := svc.Ledger.Entries(ctx, tenantB, nil, nil); err != nil || len(entries) != 0 {
		t.Fatalf("B sees A's ledger: %+v, %v", entries, err)
	}
	// A's wallet untouched by B's activity.
	if _, err := svc.Accrue(ctx, tenantB, contactA, EventFirstTxn, "bk-rls"); !errors.Is(err, ErrNoActiveProgram) {
		t.Fatalf("B accrual against A's ids = %v, want ErrNoActiveProgram", err)
	}
	w, err := st.GetWallet(ctx, tenantA, contactA)
	if err != nil || w.Balance != 100 {
		t.Fatalf("A's wallet after B's probing: %+v, %v", w, err)
	}
}

// Wallet view: GetWallet + per-contact ledger entries (handler data path).
func TestWalletView(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactA, contactB := uuid.New(), uuid.New()
	prog := mkProgram(tenantID)
	if err := st.CreateProgram(ctx, &prog); err != nil {
		t.Fatalf("create: %v", err)
	}
	svc := &Service{Store: st, Ledger: NewPostgresLedger(st)}
	for _, c := range []uuid.UUID{contactA, contactB} {
		if _, err := svc.Accrue(ctx, tenantID, c, EventFirstTxn, "tx-"+c.String()[:8]); err != nil {
			t.Fatalf("accrue %s: %v", c, err)
		}
	}
	entries, err := st.ListLedgerEntries(ctx, tenantID, nil, nil, contactA.String())
	if err != nil || len(entries) != 1 {
		t.Fatalf("contact A entries: %+v, %v", entries, err)
	}
	if entries[0].BeneficiaryID != contactA.String() || entries[0].CreditPoints != 100 {
		t.Fatalf("wrong entry leaked: %+v", entries[0])
	}
	if _, err := st.GetWallet(ctx, tenantID, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown contact wallet = %v, want ErrNotFound", err)
	}

	// Leaderboard ranks by lifetime_earned desc.
	if _, err := svc.Accrue(ctx, tenantID, contactB, EventBookingCompleted, "txb-2"); err != nil {
		t.Fatalf("accrue B: %v", err)
	}
	lb, err := st.Leaderboard(ctx, tenantID, "", 10)
	if err != nil || len(lb) != 2 {
		t.Fatalf("leaderboard: %+v, %v", lb, err)
	}
	if lb[0].ContactID != contactB || lb[0].LifetimeEarned != 150 || lb[0].Rank != 1 || lb[1].Rank != 2 {
		t.Fatalf("leaderboard order: %+v", lb)
	}
	if _, err := st.Leaderboard(ctx, tenantID, "bogus", 10); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bogus metric = %v, want ErrInvalidInput", err)
	}
}
