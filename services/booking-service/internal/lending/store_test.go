package lending

import (
	"context"
	"errors"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
)

// SPEC-W20 Agent C store tests run against embedded Postgres (same harness
// as the devices/leads/referrals/W19 tests; dedicated port 5565 avoids the
// postmaster.pid race with sibling packages under `go test ./...`; -short
// skips).
//
// The harness also bootstraps the minimal contacts + bookings + outbox
// tables the scoring reads and the outbox enqueues against (owned by the
// shared store package in production — mirrored here exactly like
// store/waitlist_test.go and workorders/store_test.go do). The contacts
// mirror deliberately OMITS created_at (the canonical schema has none) so
// the defensive tenure fallback (first booking) is what runs.

const testSupportDDL = `
CREATE TABLE IF NOT EXISTS contacts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    phone TEXT,
    email TEXT,
    notes TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS bookings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    contact_id UUID,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at TIMESTAMPTZ
);`

func newTestStore(t *testing.T) *Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres lending store test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_lending_test").
		Port(5565).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })
	st, err := DialStore(context.Background(),
		"postgres://postgres:postgres@localhost:5565/booking_lending_test?sslmode=disable")
	if err != nil {
		t.Fatalf("DialStore: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.pool.Exec(context.Background(), testSupportDDL); err != nil {
		t.Fatalf("support DDL: %v", err)
	}
	return st
}

func mkProduct(tenantID uuid.UUID) Product {
	return Product{
		TenantID:         tenantID,
		Name:             "Trader Cash",
		Active:           true,
		PrincipalMinKobo: 100000,  // ₦1,000
		PrincipalMaxKobo: 5000000, // ₦50,000
		TermDays:         30,
		InterestBps:      1500, // 15% flat
		FeeFlatKobo:      50000,
	}
}

func addContact(t *testing.T, st *Store, tenantID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := st.pool.Exec(context.Background(),
		`INSERT INTO contacts (id, tenant_id, name) VALUES ($1,$2,$3)`, id, tenantID, name)
	if err != nil {
		t.Fatalf("insert contact: %v", err)
	}
	return id
}

func addBooking(t *testing.T, st *Store, tenantID, contactID uuid.UUID, status string, createdAt time.Time) {
	t.Helper()
	_, err := st.pool.Exec(context.Background(),
		`INSERT INTO bookings (tenant_id, contact_id, starts_at, ends_at, status, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		tenantID, contactID, createdAt, createdAt.Add(time.Hour), status, createdAt)
	if err != nil {
		t.Fatalf("insert booking: %v", err)
	}
}

// Product CRUD: create → list → patch (merged validation) → filters;
// cross-tenant isolation.
func TestProductCRUD(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	p := mkProduct(tenantID)
	if err := st.CreateProduct(ctx, &p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.ID == uuid.Nil || p.CreatedAt.IsZero() {
		t.Fatalf("id/timestamps not stamped: %+v", p)
	}

	list, err := st.ListProducts(ctx, tenantID, true)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v, %v", list, err)
	}
	if cross, err := st.ListProducts(ctx, uuid.New(), true); err != nil || len(cross) != 0 {
		t.Fatalf("cross-tenant list: %+v, %v", cross, err)
	}

	fee := int64(75000)
	bps := 2000
	patched, err := st.UpdateProduct(ctx, tenantID, p.ID, ProductPatch{FeeFlatKobo: &fee, InterestBps: &bps})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if patched.FeeFlatKobo != 75000 || patched.InterestBps != 2000 || patched.Name != "Trader Cash" {
		t.Fatalf("patch did not merge: %+v", patched)
	}
	// Merged validation: min above the persisted max must be rejected.
	min := int64(99999999)
	if _, err := st.UpdateProduct(ctx, tenantID, p.ID, ProductPatch{PrincipalMinKobo: &min}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad merged patch = %v, want ErrInvalidInput", err)
	}
	if _, err := st.UpdateProduct(ctx, uuid.New(), p.ID, ProductPatch{FeeFlatKobo: &fee}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant patch = %v, want ErrNotFound", err)
	}

	// Active filter.
	active := false
	if _, err := st.UpdateProduct(ctx, tenantID, p.ID, ProductPatch{Active: &active}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	list, err = st.ListProducts(ctx, tenantID, false)
	if err != nil || len(list) != 0 {
		t.Fatalf("active-only list: %+v, %v", list, err)
	}
}

// Scoring: naive 0..100 from tenure (first booking) + completed bookings +
// prior repaid loans; defensive when the contact has no history at all.
func TestComputeScore(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contact := addContact(t, st, tenantID, "Ada")

	// No history → 0 (defensive: contacts has no created_at, no bookings).
	score, sig := st.ComputeScore(ctx, tenantID, contact)
	if score != 0 || sig.TenureDays != 0 || sig.CompletedBookings != 0 {
		t.Fatalf("empty history score = %d (%+v)", score, sig)
	}

	// 2 completed + 1 cancelled booking, first one 120 days old.
	addBooking(t, st, tenantID, contact, "completed", time.Now().AddDate(0, 0, -120))
	addBooking(t, st, tenantID, contact, "completed", time.Now().AddDate(0, 0, -10))
	addBooking(t, st, tenantID, contact, "cancelled", time.Now().AddDate(0, 0, -5))
	score, sig = st.ComputeScore(ctx, tenantID, contact)
	// tenure: 120 days → 4 months × 3 = 12; bookings: 2 × 4 = 8; loans: 0.
	if score != 20 || sig.TenureDays < 120 || sig.CompletedBookings != 2 {
		t.Fatalf("score = %d (%+v), want 20", score, sig)
	}

	// A repaid loan application adds 10.
	if err := st.CreateApplication(ctx, &Application{
		TenantID: tenantID, ContactID: contact, ProductID: uuid.New(),
		PrincipalKobo: 1000, Status: StatusRepaid,
	}); err != nil {
		t.Fatalf("seed repaid application: %v", err)
	}
	score, sig = st.ComputeScore(ctx, tenantID, contact)
	if score != 30 || sig.RepaidLoans != 1 {
		t.Fatalf("score with repaid loan = %d (%+v), want 30", score, sig)
	}

	// Unknown contact (no rows anywhere) → 0, no error.
	if score, _ := st.ComputeScore(ctx, tenantID, uuid.New()); score != 0 {
		t.Fatalf("unknown contact score = %d", score)
	}
}

// seedApproved walks one application to approved and returns both rows.
func seedApproved(t *testing.T, st *Store, tenantID, contactID uuid.UUID, principal int64) (Product, Application) {
	t.Helper()
	ctx := context.Background()
	prod := mkProduct(tenantID)
	if err := st.CreateProduct(ctx, &prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	app := Application{
		TenantID: tenantID, ContactID: contactID, ProductID: prod.ID,
		PrincipalKobo: principal, Status: StatusUnderReview,
	}
	if err := st.CreateApplication(ctx, &app); err != nil {
		t.Fatalf("create application: %v", err)
	}
	now := time.Now().UTC()
	app.Status = StatusApproved
	app.DecidedAt = &now
	if _, err := st.UpdateApplication(ctx, &app); err != nil {
		t.Fatalf("approve: %v", err)
	}
	return prod, app
}

// Disburse: interest/fee/outstanding/due math, ledger 500 journal,
// idempotent replay via the application status guard.
func TestDisburseIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contact := addContact(t, st, tenantID, "Ada")
	prod, app := seedApproved(t, st, tenantID, contact, 2000000) // ₦20,000

	now := time.Now().UTC()
	res, err := st.Disburse(ctx, tenantID, app.ID, now)
	if err != nil {
		t.Fatalf("disburse: %v", err)
	}
	if res.Replayed {
		t.Fatal("first disburse must not be a replay")
	}
	// interest = 2,000,000 × 1500/10000 = 300,000; outstanding =
	// 2,000,000 + 300,000 + 50,000 = 2,350,000.
	if res.Loan.InterestKobo != 300000 || res.Loan.FeeKobo != 50000 || res.Loan.OutstandingKobo != 2350000 {
		t.Fatalf("loan math: %+v", res.Loan)
	}
	wantDue := now.AddDate(0, 0, prod.TermDays)
	if res.Loan.DueAt.Sub(wantDue) > time.Second || wantDue.Sub(res.Loan.DueAt) > time.Second {
		t.Fatalf("due_at = %v, want ≈%v", res.Loan.DueAt, wantDue)
	}
	if res.Application.Status != StatusDisbursed || res.Loan.Status != LoanActive {
		t.Fatalf("statuses: app=%s loan=%s", res.Application.Status, res.Loan.Status)
	}

	// Ledger: principal credited to the borrower on 500.
	ledger := NewPostgresLedger(st)
	bal, err := ledger.Balance(ctx, tenantID, AccountPrincipalDisbursed, contact.String())
	if err != nil || bal != 2000000 {
		t.Fatalf("ledger 500 balance = %d (err %v), want 2000000", bal, err)
	}
	house, err := ledger.Balance(ctx, tenantID, AccountRepaymentReceived, "")
	if err != nil || house != -2000000 {
		t.Fatalf("ledger 501 balance = %d (err %v), want -2000000", house, err)
	}

	// Replay: same application → same loan, replayed, NO new ledger rows.
	re, err := st.Disburse(ctx, tenantID, app.ID, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("disburse replay: %v", err)
	}
	if !re.Replayed || re.Loan.ID != res.Loan.ID || re.Loan.OutstandingKobo != 2350000 {
		t.Fatalf("replay: %+v", re)
	}
	entries, err := ledger.Entries(ctx, tenantID, nil, nil)
	if err != nil || len(entries) != 2 {
		t.Fatalf("ledger entries after replay = %d, want 2 (err %v)", len(entries), err)
	}

	// A non-approved application cannot disburse.
	_, app2 := seedApproved(t, st, tenantID, contact, 1500000)
	app2.Status = StatusSubmitted
	if _, err := st.UpdateApplication(ctx, &app2); err != nil {
		t.Fatalf("reset app2: %v", err)
	}
	if _, err := st.Disburse(ctx, tenantID, app2.ID, now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("disburse submitted = %v, want ErrInvalidTransition", err)
	}
	if _, err := st.Disburse(ctx, tenantID, uuid.New(), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disburse unknown = %v, want ErrNotFound", err)
	}
}

// Repay: idempotent on ref_id (replay → same body), overpay clamped to
// outstanding, outstanding==0 flips loan + application to repaid, ledger
// 501 journals.
func TestRepayIdempotentAndClamp(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contact := addContact(t, st, tenantID, "Ada")
	_, app := seedApproved(t, st, tenantID, contact, 2000000)
	dis, err := st.Disburse(ctx, tenantID, app.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("disburse: %v", err)
	}
	loanID := dis.Loan.ID

	// Partial repayment: 1,000,000 of 2,350,000.
	r1, err := st.Repay(ctx, tenantID, loanID, 1000000, "pay-1")
	if err != nil {
		t.Fatalf("repay 1: %v", err)
	}
	if r1.Replayed || r1.Clamped || r1.LoanRepaid || r1.Repayment.AmountKobo != 1000000 || r1.Loan.OutstandingKobo != 1350000 {
		t.Fatalf("repay 1: %+v", r1)
	}

	// Replay with the same ref_id → identical body, nothing written.
	re, err := st.Repay(ctx, tenantID, loanID, 1000000, "pay-1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !re.Replayed || re.Repayment.ID != r1.Repayment.ID || re.Loan.OutstandingKobo != 1350000 || re.LoanRepaid {
		t.Fatalf("replay must return the stored body: %+v", re)
	}
	reps, err := st.ListRepayments(ctx, tenantID, loanID)
	if err != nil || len(reps) != 1 {
		t.Fatalf("repayments after replay = %d, want 1 (err %v)", len(reps), err)
	}

	// Overpay: 2,000,000 against 1,350,000 outstanding → clamped, noted,
	// loan + application flip to repaid.
	r2, err := st.Repay(ctx, tenantID, loanID, 2000000, "pay-2")
	if err != nil {
		t.Fatalf("repay 2: %v", err)
	}
	if !r2.Clamped || r2.RequestedKobo != 2000000 || r2.Repayment.AmountKobo != 1350000 ||
		!r2.LoanRepaid || r2.Loan.OutstandingKobo != 0 || r2.Loan.Status != LoanRepaid {
		t.Fatalf("clamped repay: %+v", r2)
	}
	appAfter, err := st.GetApplication(ctx, tenantID, app.ID)
	if err != nil || appAfter.Status != StatusRepaid {
		t.Fatalf("application after full repay: %+v, %v", appAfter, err)
	}

	// Overpay replay returns the stored (clamped) body.
	re2, err := st.Repay(ctx, tenantID, loanID, 2000000, "pay-2")
	if err != nil || !re2.Replayed || re2.Repayment.AmountKobo != 1350000 {
		t.Fatalf("overpay replay: %+v, %v", re2, err)
	}

	// A NEW ref_id on a repaid loan → 409 transition error.
	if _, err := st.Repay(ctx, tenantID, loanID, 1000, "pay-3"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("repay on repaid loan = %v, want ErrInvalidTransition", err)
	}

	// Collection lookup: by application id and by status.
	loans, err := st.ListLoans(ctx, tenantID, LoanFilters{ApplicationID: &app.ID})
	if err != nil || len(loans) != 1 || loans[0].ID != loanID {
		t.Fatalf("loans by application: %+v, %v", loans, err)
	}
	loans, err = st.ListLoans(ctx, tenantID, LoanFilters{Status: LoanRepaid})
	if err != nil || len(loans) != 1 {
		t.Fatalf("loans by status: %+v, %v", loans, err)
	}

	// Ledger: repayments credited on 501; borrower 500 balance nets to
	// principal − repayments (cash view) = 2,000,000 − 2,350,000.
	ledger := NewPostgresLedger(st)
	house, err := ledger.Balance(ctx, tenantID, AccountRepaymentReceived, "")
	if err != nil || house != 350000 { // −2,000,000 disb + 2,350,000 repaid
		t.Fatalf("ledger 501 balance = %d (err %v), want 350000", house, err)
	}
	entries, err := ledger.Entries(ctx, tenantID, nil, nil)
	if err != nil || len(entries) != 6 { // disburse pair + 2 repay pairs
		t.Fatalf("ledger entries = %d, want 6 (err %v)", len(entries), err)
	}
	var debits, credits int64
	for _, e := range entries {
		debits += e.DebitKobo
		credits += e.CreditKobo
	}
	if debits != credits {
		t.Fatalf("journal hygiene: debits %d != credits %d", debits, credits)
	}
}

// Operator-driven default marking: PATCH → defaulted flips the active loan
// in the same tx.
func TestDefaultMarking(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contact := addContact(t, st, tenantID, "Ada")
	_, app := seedApproved(t, st, tenantID, contact, 1000000)
	dis, err := st.Disburse(ctx, tenantID, app.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("disburse: %v", err)
	}

	// Repay is blocked once defaulted... first flip via UpdateApplication.
	app.Status = StatusDefaulted
	loanID, err := st.UpdateApplication(ctx, &app)
	if err != nil {
		t.Fatalf("mark defaulted: %v", err)
	}
	if loanID == nil || *loanID != dis.Loan.ID {
		t.Fatalf("defaulted loan id = %v", loanID)
	}
	loan, err := st.GetLoan(ctx, tenantID, dis.Loan.ID)
	if err != nil || loan.Status != LoanDefaulted {
		t.Fatalf("loan after default: %+v, %v", loan, err)
	}
	if _, err := st.Repay(ctx, tenantID, dis.Loan.ID, 1000, "p-x"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("repay defaulted = %v, want ErrInvalidTransition", err)
	}
}

// Portfolio: totals, counts and PAR30 (loans >30d past due).
func TestPortfolio(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contact := addContact(t, st, tenantID, "Ada")

	// Empty book → zeroes + null PAR30 (honest empty state).
	p, err := st.Portfolio(ctx, tenantID, time.Now().UTC())
	if err != nil || p.TotalOutstandingKobo != 0 || p.PAR30 != nil || p.ActiveCount != 0 {
		t.Fatalf("empty portfolio: %+v, %v", p, err)
	}

	// Loan 1: disbursed 60 days ago, 30-day term → 30 days past due... use
	// 80 days to be safely >30 past due. outstanding 1,175,000.
	_, app1 := seedApproved(t, st, tenantID, contact, 1000000)
	d1, err := st.Disburse(ctx, tenantID, app1.ID, time.Now().AddDate(0, 0, -80))
	if err != nil {
		t.Fatalf("disburse 1: %v", err)
	}
	// Loan 2: fresh, not past due. outstanding 2,350,000.
	_, app2 := seedApproved(t, st, tenantID, contact, 2000000)
	if _, err := st.Disburse(ctx, tenantID, app2.ID, time.Now().UTC()); err != nil {
		t.Fatalf("disburse 2: %v", err)
	}
	// Loan 3: repaid in full.
	_, app3 := seedApproved(t, st, tenantID, contact, 1000000)
	d3, err := st.Disburse(ctx, tenantID, app3.ID, time.Now().AddDate(0, 0, -10))
	if err != nil {
		t.Fatalf("disburse 3: %v", err)
	}
	if _, err := st.Repay(ctx, tenantID, d3.Loan.ID, 1200000, "full"); err != nil {
		t.Fatalf("repay 3: %v", err)
	}

	p, err = st.Portfolio(ctx, tenantID, time.Now().UTC())
	if err != nil {
		t.Fatalf("portfolio: %v", err)
	}
	if p.ActiveCount != 2 || p.RepaidCount != 1 || p.DefaultedCount != 0 {
		t.Fatalf("counts: %+v", p)
	}
	if p.TotalOutstandingKobo != 1200000+2350000 {
		t.Fatalf("total outstanding = %d", p.TotalOutstandingKobo)
	}
	// Only loan 1 is >30d past due → PAR30 = 1,200,000 / 3,550,000 ≈ 0.338.
	if p.PAR30 == nil || p.PAR30OutstandingKobo != d1.Loan.OutstandingKobo {
		t.Fatalf("PAR30: %+v", p)
	}
	if *p.PAR30 < 0.33 || *p.PAR30 > 0.34 {
		t.Fatalf("PAR30 ratio = %v, want ≈0.333", *p.PAR30)
	}
}

// RLS isolation: every table is FORCE-RLS'd; a second tenant sees nothing
// and cannot touch the first tenant's rows even knowing the ids.
func TestRLSIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	contactA := addContact(t, st, tenantA, "Ada")

	_, app := seedApproved(t, st, tenantA, contactA, 1000000)
	dis, err := st.Disburse(ctx, tenantA, app.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("disburse: %v", err)
	}
	if _, err := st.Repay(ctx, tenantA, dis.Loan.ID, 100000, "rls-1"); err != nil {
		t.Fatalf("repay: %v", err)
	}

	// Products / applications / loans / repayments / ledger / portfolio all
	// invisible cross-tenant.
	if list, err := st.ListProducts(ctx, tenantB, true); err != nil || len(list) != 0 {
		t.Fatalf("B sees A's products: %+v, %v", list, err)
	}
	if list, err := st.ListApplications(ctx, tenantB, ApplicationFilters{}); err != nil || len(list) != 0 {
		t.Fatalf("B sees A's applications: %+v, %v", list, err)
	}
	if _, err := st.GetApplication(ctx, tenantB, app.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B reads A's application: %v", err)
	}
	if _, err := st.GetLoan(ctx, tenantB, dis.Loan.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B reads A's loan: %v", err)
	}
	if reps, err := st.ListRepayments(ctx, tenantB, dis.Loan.ID); err != nil || len(reps) != 0 {
		t.Fatalf("B sees A's repayments: %+v, %v", reps, err)
	}
	if loans, err := st.ListLoans(ctx, tenantB, LoanFilters{}); err != nil || len(loans) != 0 {
		t.Fatalf("B sees A's loans: %+v, %v", loans, err)
	}
	if entries, err := NewPostgresLedger(st).Entries(ctx, tenantB, nil, nil); err != nil || len(entries) != 0 {
		t.Fatalf("B sees A's ledger: %+v, %v", entries, err)
	}
	p, err := st.Portfolio(ctx, tenantB, time.Now().UTC())
	if err != nil || p.TotalOutstandingKobo != 0 || p.ActiveCount != 0 {
		t.Fatalf("B sees A's portfolio: %+v, %v", p, err)
	}
	// Writes with A's ids under B's tenant context fail closed.
	if _, err := st.Disburse(ctx, tenantB, app.ID, time.Now().UTC()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B disburses A's application: %v", err)
	}
	if _, err := st.Repay(ctx, tenantB, dis.Loan.ID, 1000, "rls-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B repays A's loan: %v", err)
	}
	// A's rows untouched by B's probing.
	loan, err := st.GetLoan(ctx, tenantA, dis.Loan.ID)
	if err != nil || loan.OutstandingKobo != 1100000 {
		t.Fatalf("A's loan after B's probing: %+v, %v", loan, err)
	}
}
